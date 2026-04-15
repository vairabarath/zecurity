package connector

import (
	"context"
	"crypto/x509"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	pb "github.com/yourorg/ztna/controller/proto/connector"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// EnrollmentHandler holds the dependencies for the Enroll gRPC handler.
// Created in main.go and registered with the gRPC server.
// Called by: main.go (Member 2 wires this up)
type EnrollmentHandler struct {
	pb.UnimplementedConnectorServiceServer
	Cfg        Config
	Pool       *pgxpool.Pool
	Redis      *redis.Client
	PKIService pki.Service
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
//  3. Call BurnEnrollmentJTI(ctx, redis, jti) — atomic GET+DEL (single-use)
//     - Not found → codes.PermissionDenied ("token expired or already used")
//  4. Load connector row: verify status='pending', verify tenant_id == workspace_id
//     - Fail → codes.PermissionDenied
//  5. Verify workspace status='active'
//     - Fail → codes.FailedPrecondition
//  6. Parse CSR from request.csr_der
//  7. Verify CSR self-signature (proves connector holds the private key)
//  8. Verify CSR SPIFFE SAN matches expected:
//     expected := appmeta.ConnectorSPIFFEID(trust_domain, connector_id)
//     - Mismatch → codes.PermissionDenied
//  9. Call pki.SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, cfg.CertTTL)
//  10. UPDATE connector row: status='active', trust_domain, cert_serial, cert_not_after,
//     hostname, version, last_heartbeat_at=NOW(), enrollment_token_jti=NULL
//  11. Return EnrollResponse with signed cert PEM, workspace CA PEM, intermediate CA PEM,
//     and connector ID
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

	// Step 3 — Burn the JTI (atomic single-use).
	// Called: token.go → BurnEnrollmentJTI()
	burnedConnectorID, found, err := BurnEnrollmentJTI(ctx, h.Redis, jti)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "burn jti: %v", err)
	}
	if !found {
		return nil, status.Error(codes.PermissionDenied, "token expired or already used")
	}
	// Verify the JTI was stored for the same connector (defense in depth).
	if burnedConnectorID != connectorID {
		return nil, status.Error(codes.PermissionDenied, "token connector mismatch")
	}

	// Step 4 — Load connector row, verify status='pending' and tenant match.
	var connStatus, connTenantID string
	err = h.Pool.QueryRow(ctx,
		`SELECT status, tenant_id FROM connectors WHERE id = $1`,
		connectorID,
	).Scan(&connStatus, &connTenantID)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "connector not found: %v", err)
	}
	if connStatus != "pending" {
		return nil, status.Errorf(codes.PermissionDenied, "connector status is %q, expected pending", connStatus)
	}
	if connTenantID != workspaceID {
		return nil, status.Error(codes.PermissionDenied, "connector tenant mismatch")
	}

	// Step 5 — Verify workspace is active.
	var wsStatus string
	err = h.Pool.QueryRow(ctx,
		`SELECT status FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&wsStatus)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load workspace: %v", err)
	}
	if wsStatus != "active" {
		return nil, status.Errorf(codes.FailedPrecondition, "workspace status is %q, expected active", wsStatus)
	}

	// Step 6 — Parse CSR from DER bytes.
	csr, err := x509.ParseCertificateRequest(req.CsrDer)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse CSR: %v", err)
	}

	// Step 7 — Verify CSR self-signature (proves connector holds the private key).
	if err := csr.CheckSignature(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "CSR signature invalid: %v", err)
	}

	// Step 8 — Verify CSR SPIFFE SAN matches expected identity.
	// Called: appmeta/identity.go → ConnectorSPIFFEID()
	expectedSPIFFE := appmeta.ConnectorSPIFFEID(trustDomain, connectorID)
	if !csrHasSPIFFEURI(csr, expectedSPIFFE) {
		return nil, status.Errorf(codes.PermissionDenied,
			"SPIFFE ID in CSR does not match token: expected %s", expectedSPIFFE)
	}

	// Step 9 — Sign connector certificate.
	// Called: pki/workspace.go → SignConnectorCert()
	certResult, err := h.PKIService.SignConnectorCert(ctx, workspaceID, connectorID, trustDomain, csr, h.Cfg.CertTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign connector cert: %v", err)
	}

	// Step 10 — Update connector row to 'active'.
	_, err = h.Pool.Exec(ctx,
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
		  WHERE id = $6`,
		trustDomain,
		certResult.Serial,
		certResult.NotAfter,
		req.Hostname,
		req.Version,
		connectorID,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update connector: %v", err)
	}

	// Step 11 — Load CA certs for the response.
	workspaceCAPEM, intermediateCAPEM, err := h.loadCACerts(ctx, workspaceID)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load CA certs: %v", err)
	}

	return &pb.EnrollResponse{
		CertificatePem:    []byte(certResult.CertificatePEM),
		WorkspaceCaPem:    []byte(workspaceCAPEM),
		IntermediateCaPem: []byte(intermediateCAPEM),
		ConnectorId:       connectorID,
	}, nil
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
	err := h.Pool.QueryRow(ctx,
		`SELECT status, tenant_id, cert_not_after FROM connectors WHERE id = $1 AND trust_domain = $2`,
		connectorID, trustDomain,
	).Scan(&connStatus, &tenantID, &certNotAfter)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "connector not found: %v", err)
	}

	if connStatus == "revoked" {
		return nil, status.Error(codes.PermissionDenied, "connector is revoked")
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

	_, err = h.Pool.Exec(ctx,
		`UPDATE connectors
		    SET cert_serial = $1,
		        cert_not_after = $2,
		        updated_at = NOW()
		  WHERE id = $3 AND tenant_id = $4`,
		certResult.Serial,
		certResult.NotAfter,
		connectorID,
		tenantID,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update connector cert: %v", err)
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
