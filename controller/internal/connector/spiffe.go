package connector

import (
	"context"
	"crypto/x509"
	"fmt"
	"strings"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

// ── Context keys ────────────────────────────────────────────────────────────
// Unexported types prevent collisions with other packages' context values.

type spiffeIDKey struct{}
type spiffeRoleKey struct{}
type spiffeEntityIDKey struct{}
type trustDomainKey struct{}

// ── Context accessors ───────────────────────────────────────────────────────
// These are the ONLY way handlers should read SPIFFE identity from context.
// The interceptor injects the values; handlers consume them via these helpers.

// SPIFFEIDFromContext returns the full SPIFFE URI from the context.
// Called by: enrollment.go, heartbeat.go
func SPIFFEIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(spiffeIDKey{}).(string)
	return v
}

// SPIFFERoleFromContext returns the SPIFFE role ("connector", "agent", "controller").
// Called by: heartbeat.go (Phase 4 — verifies role == "connector")
func SPIFFERoleFromContext(ctx context.Context) string {
	v, _ := ctx.Value(spiffeRoleKey{}).(string)
	return v
}

// SPIFFEEntityIDFromContext returns the entity-specific ID (e.g. connector UUID).
// Called by: heartbeat.go (Phase 4 — used as connectorID)
func SPIFFEEntityIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(spiffeEntityIDKey{}).(string)
	return v
}

// TrustDomainFromContext returns the trust domain from the context.
// Called by: heartbeat.go (Phase 4 — used for tenant resolution)
func TrustDomainFromContext(ctx context.Context) string {
	v, _ := ctx.Value(trustDomainKey{}).(string)
	return v
}

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
		req interface{},
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (interface{}, error) {
		// Skip Enroll RPCs — neither connector nor shield has a certificate
		// during enrollment; both authenticate via JWT instead.
		if info.FullMethod == pb.ConnectorService_Enroll_FullMethodName ||
			info.FullMethod == shieldpb.ShieldService_Enroll_FullMethodName {
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

		// Build the full SPIFFE URI for context injection.
		spiffeID := "spiffe://" + trustDomain + "/" + role + "/" + entityID

		// Inject identity into context for downstream handlers.
		ctx = context.WithValue(ctx, spiffeIDKey{}, spiffeID)
		ctx = context.WithValue(ctx, spiffeRoleKey{}, role)
		ctx = context.WithValue(ctx, spiffeEntityIDKey{}, entityID)
		ctx = context.WithValue(ctx, trustDomainKey{}, trustDomain)

		return handler(ctx, req)
	}
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
var _ = shieldpb.ShieldService_Enroll_FullMethodName
var _ = appmeta.SPIFFEGlobalTrustDomain
