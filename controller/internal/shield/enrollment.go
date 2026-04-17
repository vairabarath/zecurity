package shield

import (
	"context"
	"crypto/x509"
	"errors"

	pgx "github.com/jackc/pgx/v5"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *service) Enroll(ctx context.Context, req *shieldpb.EnrollRequest) (*shieldpb.EnrollResponse, error) {
	claims, err := VerifyShieldToken(s.cfg, req.EnrollmentToken)
	if err != nil {
		return nil, status.Errorf(codes.Unauthenticated, "invalid shield enrollment token: %v", err)
	}

	jti := claims.ID
	shieldID := claims.ShieldID
	workspaceID := claims.WorkspaceID
	trustDomain := claims.TrustDomain
	connectorID := claims.ConnectorID
	connectorAddr := claims.ConnectorAddr
	interfaceAddr := claims.InterfaceAddr
	remoteNetworkID := claims.RemoteNetworkID

	if jti == "" || shieldID == "" || workspaceID == "" || trustDomain == "" || connectorID == "" || connectorAddr == "" || interfaceAddr == "" {
		return nil, status.Error(codes.InvalidArgument, "shield enrollment token missing required claims")
	}

	burnedShieldID, found, err := BurnShieldJTI(ctx, s.redis, jti)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "burn shield jti: %v", err)
	}
	if !found {
		return nil, status.Error(codes.PermissionDenied, "token expired or already used")
	}
	if burnedShieldID != shieldID {
		return nil, status.Error(codes.PermissionDenied, "token shield mismatch")
	}

	var shieldStatus, shieldTenantID, shieldConnectorID, shieldRemoteNetworkID string
	var shieldInterfaceAddr *string
	err = s.db.QueryRow(ctx,
		`SELECT status, tenant_id, connector_id, remote_network_id, interface_addr
		   FROM shields
		  WHERE id = $1`,
		shieldID,
	).Scan(&shieldStatus, &shieldTenantID, &shieldConnectorID, &shieldRemoteNetworkID, &shieldInterfaceAddr)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.PermissionDenied, "shield not found")
		}
		return nil, status.Errorf(codes.Internal, "load shield: %v", err)
	}
	if shieldStatus != "pending" {
		return nil, status.Errorf(codes.PermissionDenied, "shield status is %q, expected pending", shieldStatus)
	}
	if shieldTenantID != workspaceID {
		return nil, status.Error(codes.PermissionDenied, "shield tenant mismatch")
	}
	if shieldConnectorID != connectorID {
		return nil, status.Error(codes.PermissionDenied, "shield connector mismatch")
	}
	if shieldRemoteNetworkID != remoteNetworkID {
		return nil, status.Error(codes.PermissionDenied, "shield remote network mismatch")
	}
	if shieldInterfaceAddr != nil && *shieldInterfaceAddr != "" && *shieldInterfaceAddr != interfaceAddr {
		return nil, status.Error(codes.PermissionDenied, "shield interface address mismatch")
	}

	var workspaceStatus string
	err = s.db.QueryRow(ctx,
		`SELECT status FROM workspaces WHERE id = $1`,
		workspaceID,
	).Scan(&workspaceStatus)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load workspace: %v", err)
	}
	if workspaceStatus != "active" {
		return nil, status.Errorf(codes.FailedPrecondition, "workspace status is %q, expected active", workspaceStatus)
	}

	var connectorStatus string
	err = s.db.QueryRow(ctx,
		`SELECT status
		   FROM connectors
		  WHERE id = $1
		    AND tenant_id = $2
		    AND remote_network_id = $3`,
		connectorID,
		workspaceID,
		remoteNetworkID,
	).Scan(&connectorStatus)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.PermissionDenied, "connector not found for shield token")
		}
		return nil, status.Errorf(codes.Internal, "load connector: %v", err)
	}
	if connectorStatus != "active" {
		return nil, status.Errorf(codes.FailedPrecondition, "connector status is %q, expected active", connectorStatus)
	}

	csr, err := x509.ParseCertificateRequest(req.CsrDer)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse CSR: %v", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "CSR signature invalid: %v", err)
	}

	expectedSPIFFE := appmeta.ShieldSPIFFEID(trustDomain, shieldID)
	if !csrHasSPIFFEURI(csr, expectedSPIFFE) {
		return nil, status.Errorf(codes.PermissionDenied, "SPIFFE ID in CSR does not match token: expected %s", expectedSPIFFE)
	}

	certResult, err := s.pki.SignShieldCert(ctx, workspaceID, shieldID, trustDomain, csr, s.cfg.CertTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign shield cert: %v", err)
	}

	_, err = s.db.Exec(ctx,
		`UPDATE shields
		    SET status = 'active',
		        trust_domain = $1,
		        interface_addr = $2,
		        cert_serial = $3,
		        cert_not_after = $4,
		        hostname = $5,
		        version = $6,
		        last_heartbeat_at = NOW(),
		        enrollment_token_jti = NULL,
		        updated_at = NOW()
		  WHERE id = $7`,
		trustDomain,
		interfaceAddr,
		certResult.Serial,
		certResult.NotAfter,
		req.Hostname,
		req.Version,
		shieldID,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update shield: %v", err)
	}

	return &shieldpb.EnrollResponse{
		CertificatePem:    []byte(certResult.CertificatePEM),
		WorkspaceCaPem:    []byte(certResult.WorkspaceCAPEM),
		IntermediateCaPem: []byte(certResult.IntermediateCAPEM),
		ShieldId:          shieldID,
		InterfaceAddr:     interfaceAddr,
		ConnectorAddr:     connectorAddr,
		ConnectorId:       connectorID,
	}, nil
}

func csrHasSPIFFEURI(csr *x509.CertificateRequest, expectedURI string) bool {
	for _, uri := range csr.URIs {
		if uri.String() == expectedURI {
			return true
		}
	}
	return false
}
