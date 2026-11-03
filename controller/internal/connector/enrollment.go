package connector

import (
	"context"
	"crypto"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/valkey-io/valkey-go/valkeycompat"
	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/policy"
	"github.com/yourorg/ztna/controller/internal/posture"
	"github.com/yourorg/ztna/controller/internal/shield"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// EnrollmentHandler holds the dependencies for the ConnectorService gRPC handlers.
// Created in main.go and registered with the gRPC server.
type EnrollmentHandler struct {
	pb.UnimplementedConnectorServiceServer
	Cfg        Config
	Pool       *pgxpool.Pool
	Redis      valkeycompat.Cmdable
	PKIService pki.Service
	ShieldSvc  shield.Service
	Registry   *ConnectorRegistry // tracks active Control streams
	PostureStore *posture.Store
	PolicyStore       *policy.Store
	PolicyCache       *policy.SnapshotCache
	PolicyNotifier    PolicyChangeNotifier
	RelayStore        RelayPlacementStore
	RelayListSrc      LabelledRelayListSource // ADR-016: source of LabelledRelayList for control-stream push
	TransportNotifier TransportChangeNotifier // ADR-017 Track B: topology-change notifier for relay-placement changes

	Recon reconcileState // ADR-004 Phase 3 reconciliation hysteresis (zero value ready)

	// lookupOverride replaces lookupConnector's query in unit tests that need
	// a lookup failure the real database can't produce on demand. nil in
	// production.
	lookupOverride func(ctx context.Context, connectorID, trustDomain string) (connectorAuthState, connectorRecord, error)
}

// Enroll implements the ConnectorService.Enroll gRPC handler.
// Called by: gRPC server (registered via proto-generated service definition)
//
// NOTE: The SPIFFE interceptor SKIPS this method — the connector has no certificate
// during enrollment. Authentication is via the enrollment JWT.
//
// Full sequence:
//
//  1. Verify JWT signature using cfg.JWTSecret, check exp, verify iss == appmeta.ControllerIssuer
//  2. Extract jti, connector_id, workspace_id, trust_domain from JWT claims
//  3. PeekEnrollmentJTI: the token must still be stored in Valkey for this
//     connector (checked, not consumed)
//     - Not found → codes.PermissionDenied ("token expired or already used")
//  4. Parse the CSR, verify its self-signature and that its SPIFFE SAN is
//     appmeta.ConnectorSPIFFEID(trust_domain, connector_id)
//  5. Begin a transaction
//  6. Claim the row with SELECT … FOR UPDATE: status='pending' and
//     enrollment_token_jti = the token's jti (the authority: a superseded or
//     used token is refused before signing); verify tenant_id == workspace_id
//  7. Verify workspace status='active' (codes.FailedPrecondition otherwise)
//  8. pki.SignConnectorCert
//  9. Load the CA certs for the response
//  10. UPDATE the row (still pending, still this jti): status='active',
//     cert_serial, …, enrollment_token_jti=NULL; exactly one row
//  11. Commit — nothing is returned unless the serial is recorded
//  12. Delete the token's Valkey entry (cleanup; the row already moved on)
//  13. Return EnrollResponse with signed cert PEM, workspace CA PEM,
//     intermediate CA PEM, and connector ID
//
// Certificate audit bug 8: the row lock serializes concurrent enrollments
// and re-issues, and the token binding makes every superseded token useless.
func (h *EnrollmentHandler) Enroll(ctx context.Context, req *pb.EnrollRequest) (*pb.EnrollResponse, error) {
	// Step 1 — Verify enrollment JWT.
	// Called: token.go → VerifyEnrollmentToken()
	claims, err := VerifyEnrollmentToken(h.Cfg, req.EnrollmentToken)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid enrollment token: %v", err)
	}

	// Step 2 — Extract identity from JWT claims.
	jti := claims.ID
	connectorID := claims.ConnectorID
	workspaceID := claims.WorkspaceID
	trustDomain := claims.TrustDomain

	if jti == "" || connectorID == "" || workspaceID == "" || trustDomain == "" {
		return nil, status.Error(codes.InvalidArgument, "enrollment token missing required claims")
	}

	// Step 3 — The token must still be stored in Valkey (not expired, not
	// consumed), for this connector. Checked, not consumed: the entry is
	// deleted only after the enrollment commits (step 12), so a failed
	// enrollment leaves the token usable for a retry. The connector row is
	// the authority (step 6); this is the second layer.
	// Called: token.go → PeekEnrollmentJTI()
	storedConnectorID, found, err := PeekEnrollmentJTI(ctx, h.Redis, jti)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read jti: %v", err)
	}
	if !found {
		return nil, status.Error(codes.PermissionDenied, "token expired or already used")
	}
	// Verify the JTI was stored for the same connector (defense in depth).
	if storedConnectorID != connectorID {
		return nil, status.Error(codes.PermissionDenied, "token connector mismatch")
	}

	// Step 4 — Parse the CSR and verify its self-signature (proves the
	// connector holds the private key) and its SPIFFE SAN. No state yet.
	csr, err := x509.ParseCertificateRequest(req.CsrDer)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "CSR signature invalid: %v", err)
	}
	// Called: appmeta/identity.go → ConnectorSPIFFEID()
	expectedSPIFFE := appmeta.ConnectorSPIFFEID(trustDomain, connectorID)
	if !csrHasSPIFFEURI(csr, expectedSPIFFE) {
		return nil, status.Errorf(codes.PermissionDenied,
			"SPIFFE ID in CSR does not match token: expected %s", expectedSPIFFE)
	}

	// Step 5 — Everything from here runs in one transaction holding the
	// connector row lock (certificate audit bug 8), so only one enrollment
	// can win, and a concurrent re-issue waits for it.
	tx, err := h.Pool.Begin(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "begin enrollment: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Step 6 — Claim the row: still pending, and this token is its current
	// one. A superseded or already used token is refused here, before
	// anything is signed, whatever Valkey still holds.
	var connTenantID string
	err = tx.QueryRow(ctx,
		`SELECT tenant_id FROM connectors
		  WHERE id = $1 AND status = 'pending' AND enrollment_token_jti = $2
		    FOR UPDATE`,
		connectorID, jti,
	).Scan(&connTenantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, enrollmentClaimError(ctx, tx, connectorID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "claim connector: %v", err)
	}
	if connTenantID != workspaceID {
		return nil, status.Error(codes.PermissionDenied, "connector tenant mismatch")
	}

	// Step 7 — Verify workspace is active.
	var wsStatus string
	err = tx.QueryRow(ctx,
		`SELECT status FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsStatus)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load workspace: %v", err)
	}
	if wsStatus != "active" {
		return nil, status.Errorf(codes.FailedPrecondition, "workspace status is %q, expected active", wsStatus)
	}

	// Step 8 — Sign connector certificate. The signer only reads CA keys;
	// a certificate it signs is returned (step 13) only once its serial is
	// committed (step 11), so none leaves the controller untracked.
	// Called: pki/workspace.go → SignConnectorCert()
	certResult, err := h.PKIService.SignConnectorCert(ctx, workspaceID, connectorID, trustDomain, csr, h.Cfg.CertTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign connector cert: %v", err)
	}

	// Step 9 — Load CA certs for the response before committing, so a
	// failure here leaves the enrollment undone.
	workspaceCAPEM, intermediateCAPEM, err := h.loadCACerts(ctx, workspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load CA certs: %v", err)
	}

	// Step 10 — Activate the claimed row, still bound to this token.
	tag, err := tx.Exec(ctx,
		`UPDATE connectors
		    SET status = 'active',
		        trust_domain = $1,
		        cert_serial = $2,
		        cert_not_after = $3,
		        hostname = $4,
		        version = $5,
		        last_heartbeat_at = NOW(),
		        enrollment_token_jti = NULL,
		        updated_at = NOW()
		  WHERE id = $6 AND status = 'pending' AND enrollment_token_jti = $7`,
		trustDomain,
		certResult.Serial,
		certResult.NotAfter,
		req.Hostname,
		req.Version,
		connectorID,
		jti,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update connector: %v", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, status.Error(codes.Internal, "update connector: claimed row no longer matches")
	}

	// Step 11 — Commit. Nothing is returned unless the serial is recorded.
	if err := tx.Commit(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "commit enrollment: %v", err)
	}

	// Step 12 — Consume the token's Valkey entry. Cleanup only: the row no
	// longer names this token, so a leftover entry cannot enroll again.
	if err := DeleteEnrollmentJTI(ctx, h.Redis, jti); err != nil {
		log.Printf("connector enrollment: %s: consumed token entry not deleted: %v", connectorID, err)
	}

	// Step 13 — Respond.
	return &pb.EnrollResponse{
		CertificatePem:    []byte(certResult.CertificatePEM),
		WorkspaceCaPem:    []byte(workspaceCAPEM),
		IntermediateCaPem: []byte(intermediateCAPEM),
		ConnectorId:       connectorID,
	}, nil
}

// requireSameKeyAsPresented refuses a renewal CSR unless its key is the key
// of the mTLS peer certificate, the leaf the SPIFFE interceptor verified for
// this connection (never a certificate from the request). Keys are compared
// as parsed keys, not as encodings. It fails closed: no peer certificate, an
// unreadable key or an invalid CSR are refused.
func requireSameKeyAsPresented(ctx context.Context, csrDER []byte) error {
	certs, err := peerFromContext(ctx)
	if err != nil {
		return status.Error(codes.Unauthenticated, "renewal requires the presented connector certificate")
	}
	presented, ok := certs[0].PublicKey.(interface{ Equal(crypto.PublicKey) bool })
	if !ok || certs[0].PublicKey == nil {
		return status.Error(codes.PermissionDenied, "presented certificate key is unreadable")
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "parse renewal CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return status.Errorf(codes.InvalidArgument, "renewal CSR signature invalid: %v", err)
	}
	if !presented.Equal(csr.PublicKey) {
		return status.Error(codes.PermissionDenied, "renewal key does not match the presented certificate")
	}
	return nil
}

// enrollmentClaimError explains why the connector row could not be claimed:
// it does not exist, is no longer pending, or the token is not its current
// one (superseded by a re-issue, or already used).
func enrollmentClaimError(ctx context.Context, tx pgx.Tx, connectorID string) error {
	var connStatus string
	err := tx.QueryRow(ctx, `SELECT status FROM connectors WHERE id = $1`, connectorID).Scan(&connStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.NotFound, "connector not found")
	}
	if err != nil {
		return status.Errorf(codes.Internal, "load connector: %v", err)
	}
	if connStatus != "pending" {
		return status.Errorf(codes.PermissionDenied, "connector status is %q, expected pending", connStatus)
	}
	return status.Error(codes.PermissionDenied, "enrollment token is not the connector's current token")
}

// loadCACerts fetches the workspace CA PEM and intermediate CA PEM for the EnrollResponse.
// Called by: Enroll() above (Step 11)
func (h *EnrollmentHandler) loadCACerts(ctx context.Context, workspaceID string) (workspaceCAPEM, intermediateCAPEM string, err error) {
	// Workspace CA cert.
	err = h.Pool.QueryRow(ctx,
		`SELECT certificate_pem FROM workspace_ca_keys WHERE tenant_id = $1`,
		workspaceID,
	).Scan(&workspaceCAPEM)
	if err != nil {
		return "", "", fmt.Errorf("load workspace CA cert: %w", err)
	}

	// Intermediate CA cert.
	err = h.Pool.QueryRow(ctx,
		`SELECT certificate_pem FROM ca_intermediate LIMIT 1`,
	).Scan(&intermediateCAPEM)
	if err != nil {
		return "", "", fmt.Errorf("load intermediate CA cert: %w", err)
	}

	return workspaceCAPEM, intermediateCAPEM, nil
}

// csrHasSPIFFEURI checks if the CSR contains the expected SPIFFE URI as a SAN.
// Called by: Enroll() above (Step 8)
func csrHasSPIFFEURI(csr *x509.CertificateRequest, expectedURI string) bool {
	for _, uri := range csr.URIs {
		if uri.String() == expectedURI {
			return true
		}
	}
	return false
}

// RenewCert handles the RenewCert RPC.
// Called by: gRPC server when connector's cert is expiring soon.
func (h *EnrollmentHandler) RenewCert(ctx context.Context, req *pb.RenewCertRequest) (*pb.RenewCertResponse, error) {
	trustDomain := TrustDomainFromContext(ctx)
	role := SPIFFERoleFromContext(ctx)
	connectorID := SPIFFEEntityIDFromContext(ctx)

	if role != appmeta.SPIFFERoleConnector {
		return nil, status.Errorf(codes.PermissionDenied, "expected role %q, got %q", appmeta.SPIFFERoleConnector, role)
	}

	var connStatus, tenantID string
	var certNotAfter *time.Time
	var revokedAt *time.Time
	err := h.Pool.QueryRow(ctx,
		`SELECT status, tenant_id, cert_not_after, revoked_at FROM connectors WHERE id = $1 AND trust_domain = $2`,
		connectorID, trustDomain,
	).Scan(&connStatus, &tenantID, &certNotAfter, &revokedAt)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "connector not found: %v", err)
	}

	if connStatus == "revoked" || revokedAt != nil {
		return nil, status.Error(codes.PermissionDenied, "connector is revoked")
	}

	// Certificate audit bug 10: renew only for the key of the certificate
	// this connection authenticated with. The connector reuses its key on
	// renewal; anything else would let whoever briefly held the key move the
	// identity onto a key of their own.
	if err := requireSameKeyAsPresented(ctx, req.PublicKeyDer); err != nil {
		return nil, err
	}

	certResult, err := h.PKIService.RenewConnectorCert(
		ctx,
		tenantID,
		connectorID,
		trustDomain,
		req.PublicKeyDer,
		h.Cfg.CertTTL,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "renew connector cert: %v", err)
	}

	cmdTag, err := h.Pool.Exec(ctx,
		`UPDATE connectors
		    SET cert_serial = $1,
		        cert_not_after = $2,
		        updated_at = NOW()
		  WHERE id = $3 AND tenant_id = $4
		    AND status <> 'revoked'
		    AND revoked_at IS NULL`,
		certResult.Serial,
		certResult.NotAfter,
		connectorID,
		tenantID,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update connector cert: %v", err)
	}
	if cmdTag.RowsAffected() == 0 {
		return nil, status.Error(codes.PermissionDenied, "connector is revoked")
	}

	workspaceCAPEM, intermediateCAPEM, err := h.loadCACerts(ctx, tenantID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load CA certs: %v", err)
	}

	fmt.Printf("connector %s: cert renewed, new expiry=%v\n", connectorID, certResult.NotAfter)

	return &pb.RenewCertResponse{
		CertificatePem:    []byte(certResult.CertificatePEM),
		WorkspaceCaPem:    []byte(workspaceCAPEM),
		IntermediateCaPem: []byte(intermediateCAPEM),
	}, nil
}

// Compile-time interface check — ensure EnrollmentHandler implements ConnectorServiceServer.
var _ pb.ConnectorServiceServer = (*EnrollmentHandler)(nil)

// Suppress unused import warnings for packages used only in type signatures.
var _ time.Duration
