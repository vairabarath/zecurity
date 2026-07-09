package connector

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"

	clientpb "github.com/yourorg/ztna/controller/gen/go/proto/client/v1"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ── Context accessors ───────────────────────────────────────────────────────
// The verified SPIFFE identity now lives in the neutral internal/spiffe package
// so handler packages outside connector (e.g. shield) can read it without an
// import cycle. These thin wrappers preserve the existing connector call sites.

// SPIFFEIDFromContext returns the full SPIFFE URI from the context.
func SPIFFEIDFromContext(ctx context.Context) string { return spiffe.ID(ctx) }

// SPIFFERoleFromContext returns the SPIFFE role ("connector", "shield", "controller").
func SPIFFERoleFromContext(ctx context.Context) string { return spiffe.Role(ctx) }

// SPIFFEEntityIDFromContext returns the entity-specific ID (e.g. connector UUID).
func SPIFFEEntityIDFromContext(ctx context.Context) string { return spiffe.EntityID(ctx) }

// TrustDomainFromContext returns the trust domain from the context.
func TrustDomainFromContext(ctx context.Context) string { return spiffe.TrustDomain(ctx) }

// ── WorkspaceStore ──────────────────────────────────────────────────────────

// WorkspaceStore defines the DB lookup needed by the trust domain validator.
// Decouples the validator from a concrete DB type so it can be tested with stubs.
// Called by: NewTrustDomainValidator() below
type WorkspaceStore interface {
	// GetByTrustDomain looks up a workspace by its SPIFFE trust domain.
	// Returns (nil, nil) if the workspace does not exist.
	// Returns (nil, err) on DB errors.
	GetByTrustDomain(ctx context.Context, domain string) (*WorkspaceLookup, error)

	// GetWorkspaceCAByTrustDomain loads the workspace CA certificate for the
	// active workspace owning the trust domain.
	GetWorkspaceCAByTrustDomain(ctx context.Context, domain string) (*x509.Certificate, error)

	// GetIntermediateCA loads the platform intermediate CA certificate used to
	// verify workspace CA chains and the controller server certificate.
	GetIntermediateCA(ctx context.Context) (*x509.Certificate, error)
}

// WorkspaceLookup is the minimal workspace info needed by the trust domain validator.
// We define our own type instead of importing models.Workspace to avoid coupling.
type WorkspaceLookup struct {
	ID     string
	Status string
}

// ── Trust domain validation ─────────────────────────────────────────────────

// TrustDomainValidator checks if a trust domain is valid (accepted by the system).
// Called by: UnarySPIFFEInterceptor() below
type TrustDomainValidator func(ctx context.Context, domain string) bool

// NewTrustDomainValidator returns a validator that:
//   - Accepts appmeta.SPIFFEGlobalTrustDomain (the controller's own domain)
//   - Accepts any workspace trust domain found via store.GetByTrustDomain(domain)
//     where the workspace status is "active"
//   - Rejects everything else
//
// Trust domain validation is LIVE, not cached. If a workspace is suspended,
// its trust domain becomes invalid immediately. Do NOT add a cache.
// Called by: main.go (Member 2 creates this and passes to UnarySPIFFEInterceptor)
func NewTrustDomainValidator(globalDomain string, store WorkspaceStore) TrustDomainValidator {
	return func(ctx context.Context, domain string) bool {
		// The controller's own trust domain is always valid.
		if domain == globalDomain {
			return true
		}

		// Check if this is an active workspace trust domain (live DB lookup).
		ws, err := store.GetByTrustDomain(ctx, domain)
		if err != nil || ws == nil {
			return false
		}

		return ws.Status == "active"
	}
}

// ── SPIFFE ID parser ────────────────────────────────────────────────────────

// parseSPIFFEID extracts the trust domain, role, and entity ID from a certificate's
// SPIFFE URI SAN.
// Called by: UnarySPIFFEInterceptor() below (on every gRPC call except Enroll)
//
// Requires:
//   - Exactly 1 URI SAN on the certificate
//   - URI scheme must be "spiffe"
//   - Path must have exactly 2 segments: /<role>/<id>
//
// Returns parsed components: trustDomain, role, id
func parseSPIFFEID(cert *x509.Certificate) (trustDomain, role, id string, err error) {
	if len(cert.URIs) != 1 {
		return "", "", "", fmt.Errorf("expected exactly 1 URI SAN, got %d", len(cert.URIs))
	}

	uri := cert.URIs[0]

	if uri.Scheme != "spiffe" {
		return "", "", "", fmt.Errorf("URI SAN scheme must be spiffe, got %q", uri.Scheme)
	}

	trustDomain = uri.Host
	if trustDomain == "" {
		return "", "", "", fmt.Errorf("SPIFFE URI has empty trust domain")
	}

	// Path must be "/<role>/<id>" → after trim: "role/id" → 2 segments.
	path := strings.TrimPrefix(uri.Path, "/")
	segments := strings.SplitN(path, "/", 3) // limit 3 to detect too many segments
	if len(segments) != 2 || segments[0] == "" || segments[1] == "" {
		return "", "", "", fmt.Errorf("SPIFFE path must be /<role>/<id>, got %q", uri.Path)
	}

	return trustDomain, segments[0], segments[1], nil
}

// ── gRPC interceptor ────────────────────────────────────────────────────────

// UnarySPIFFEInterceptor returns a gRPC unary server interceptor that:
//   - Skips the Enroll method (connector has no cert during enrollment)
//   - For all other RPCs: extracts peer cert from mTLS, parses SPIFFE ID,
//     validates trust domain, injects identity into context
//
// Called by: main.go (Member 2 wires this into the gRPC server options)
//
// Context keys injected:
//   - spiffeIDKey{}       — full SPIFFE URI string
//   - spiffeRoleKey{}     — "connector", "agent", or "controller"
//   - spiffeEntityIDKey{} — the entity-specific ID (e.g. connector UUID)
//   - trustDomainKey{}    — the trust domain from the SPIFFE URI
func UnarySPIFFEInterceptor(validator TrustDomainValidator, store WorkspaceStore) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		// Skip bootstrap RPCs because the caller has no certificate yet.
		// Connector and Shield authenticate with JWTs. Relay Provision currently
		// uses server-authenticated TLS only; token authentication is deferred.
		if info.FullMethod == pb.ConnectorService_Enroll_FullMethodName ||
			info.FullMethod == shieldpb.ShieldService_Enroll_FullMethodName ||
			info.FullMethod == relaypb.RelayService_Provision_FullMethodName {
			return handler(ctx, req)
		}

		// Skip the entire ClientService — Sprint 7 end-user device flow.
		// The CLI has no certificate yet (GetAuthConfig/TokenExchange) or only
		// receives one as the response (EnrollDevice). Auth is JWT-based via a
		// field inside the request, validated by the ClientService handler.
		if strings.HasPrefix(info.FullMethod, "/client.v1.ClientService/") {
			return handler(ctx, req)
		}

		// Extract peer TLS info from the gRPC connection.
		p, ok := peer.FromContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "no peer info")
		}

		tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "no TLS credentials")
		}

		// We request client certs but verify them dynamically per workspace.
		// The TLS handshake gives us the presented leaf; the interceptor verifies
		// the chain against the workspace CA resolved from the SPIFFE trust domain.
		if len(tlsInfo.State.PeerCertificates) == 0 {
			return nil, status.Error(codes.Unauthenticated, "no client certificate")
		}

		leaf := tlsInfo.State.PeerCertificates[0]

		// Parse the SPIFFE ID from the certificate's URI SAN.
		trustDomain, role, entityID, err := parseSPIFFEID(leaf)
		if err != nil {
			return nil, status.Errorf(codes.Unauthenticated, "invalid SPIFFE ID: %v", err)
		}

		// Validate the trust domain (live DB lookup, no cache).
		if !validator(ctx, trustDomain) {
			return nil, status.Errorf(codes.PermissionDenied, "trust domain %q not accepted", trustDomain)
		}

		if role == appmeta.SPIFFERoleConnector {
			if err := verifyConnectorCertificate(ctx, store, trustDomain, leaf); err != nil {
				return nil, status.Errorf(codes.Unauthenticated, "connector certificate verification failed: %v", err)
			}
		}
		if role == appmeta.SPIFFERoleRelay {
			if err := verifyRelayCertificate(ctx, store, trustDomain, leaf); err != nil {
				return nil, status.Errorf(codes.Unauthenticated, "relay certificate verification failed: %v", err)
			}
		}

		// Build the full SPIFFE URI for context injection.
		spiffeID := "spiffe://" + trustDomain + "/" + role + "/" + entityID

		// Inject identity into context for downstream handlers (shared package).
		ctx = spiffe.WithIdentity(ctx, spiffeID, role, entityID, trustDomain)

		return handler(ctx, req)
	}
}

func verifyRelayCertificate(ctx context.Context, store WorkspaceStore, trustDomain string, leaf *x509.Certificate) error {
	if trustDomain != appmeta.SPIFFEGlobalTrustDomain {
		return fmt.Errorf("relay trust domain must be %q", appmeta.SPIFFEGlobalTrustDomain)
	}
	intermediateCA, err := store.GetIntermediateCA(ctx)
	if err != nil {
		return fmt.Errorf("load intermediate CA: %w", err)
	}
	if intermediateCA == nil {
		return fmt.Errorf("intermediate CA not found")
	}
	roots := x509.NewCertPool()
	roots.AddCert(intermediateCA)
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return fmt.Errorf("verify leaf against intermediate CA: %w", err)
	}
	return nil
}

func verifyConnectorCertificate(ctx context.Context, store WorkspaceStore, trustDomain string, leaf *x509.Certificate) error {
	workspaceCA, err := store.GetWorkspaceCAByTrustDomain(ctx, trustDomain)
	if err != nil {
		return fmt.Errorf("load workspace CA: %w", err)
	}
	if workspaceCA == nil {
		return fmt.Errorf("workspace CA not found for trust domain %q", trustDomain)
	}

	// Verify the leaf cert was signed by the Workspace CA.
	// We use the Workspace CA as the trusted root directly — it is the
	// immediate issuer of connector leaf certs. This avoids Go's x509.Verify
	// enforcing MaxPathLen across the full chain (Root → Intermediate →
	// Workspace → Leaf), which would require all CAs to have correct
	// path length values in already-generated certificates.
	roots := x509.NewCertPool()
	roots.AddCert(workspaceCA)

	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return fmt.Errorf("verify leaf against workspace CA: %w", err)
	}

	return nil
}

// Compile-time check: ensure the Enroll method name matches expectations.
// If the proto definition changes, this will fail at compile time.
var _ = pb.ConnectorService_Enroll_FullMethodName
var _ = clientpb.ClientService_GetAuthConfig_FullMethodName
var _ = shieldpb.ShieldService_Enroll_FullMethodName
var _ = appmeta.SPIFFEGlobalTrustDomain
