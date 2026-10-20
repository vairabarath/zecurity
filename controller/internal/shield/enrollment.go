package shield

import (
	"context"
	"crypto/x509"
	"errors"
	"log"

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

	// The token must still be stored in Valkey (not expired, not consumed),
	// for this shield. Checked, not consumed: the entry is deleted only after
	// the enrollment commits, so a failed enrollment leaves the token usable
	// for a retry. The shield row is the authority (below); this is the
	// second layer (certificate audit bug 8).
	storedShieldID, found, err := PeekShieldJTI(ctx, s.redis, jti)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read shield jti: %v", err)
	}
	if !found {
		return nil, status.Error(codes.PermissionDenied, "token expired or already used")
	}
	if storedShieldID != shieldID {
		return nil, status.Error(codes.PermissionDenied, "token shield mismatch")
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

	// Everything from here runs in one transaction holding the shield row
	// lock, so only one enrollment can win and a concurrent re-issue waits.
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "begin enrollment: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // no-op after commit

	// Claim the row: still pending, and this token is its current one. A
	// superseded or already used token is refused here, before anything is
	// signed, whatever Valkey still holds.
	var shieldTenantID, shieldConnectorID, shieldRemoteNetworkID string
	var shieldInterfaceAddr *string
	err = tx.QueryRow(ctx,
		`SELECT tenant_id, connector_id, remote_network_id, interface_addr
		   FROM shields
		  WHERE id = $1 AND status = 'pending' AND enrollment_token_jti = $2
		    FOR UPDATE`,
		shieldID, jti,
	).Scan(&shieldTenantID, &shieldConnectorID, &shieldRemoteNetworkID, &shieldInterfaceAddr)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, shieldClaimError(ctx, tx, shieldID)
	}
	if err != nil {
		return nil, status.Errorf(codes.Internal, "claim shield: %v", err)
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
	err = tx.QueryRow(ctx,
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
	err = tx.QueryRow(ctx,
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

	// The signer only reads CA keys; the certificate it signs is returned
	// only once its serial is committed, so none leaves untracked.
	certResult, err := s.pki.SignShieldCert(ctx, workspaceID, shieldID, trustDomain, csr, s.cfg.CertTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign shield cert: %v", err)
	}

	// Activate the claimed row, still bound to this token.
	tag, err := tx.Exec(ctx,
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
		  WHERE id = $7 AND status = 'pending' AND enrollment_token_jti = $8`,
		trustDomain,
		interfaceAddr,
		certResult.Serial,
		certResult.NotAfter,
		req.Hostname,
		req.Version,
		shieldID,
		jti,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update shield: %v", err)
	}
	if tag.RowsAffected() != 1 {
		return nil, status.Error(codes.Internal, "update shield: claimed row no longer matches")
	}

	// Nothing is returned unless the serial is recorded.
	if err := tx.Commit(ctx); err != nil {
		return nil, status.Errorf(codes.Internal, "commit enrollment: %v", err)
	}

	// Consume the token's Valkey entry. Cleanup only: the row no longer names
	// this token, so a leftover entry cannot enroll again.
	if err := DeleteShieldJTI(ctx, s.redis, jti); err != nil {
		log.Printf("shield enrollment: %s: consumed token entry not deleted: %v", shieldID, err)
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

// shieldClaimError explains why the shield row could not be claimed: it does
// not exist, is no longer pending, or the token is not its current one
// (superseded by a re-issue, or already used).
func shieldClaimError(ctx context.Context, tx pgx.Tx, shieldID string) error {
	var shieldStatus string
	err := tx.QueryRow(ctx, `SELECT status FROM shields WHERE id = $1`, shieldID).Scan(&shieldStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.PermissionDenied, "shield not found")
	}
	if err != nil {
		return status.Errorf(codes.Internal, "load shield: %v", err)
	}
	if shieldStatus != "pending" {
		return status.Errorf(codes.PermissionDenied, "shield status is %q, expected pending", shieldStatus)
	}
	return status.Error(codes.PermissionDenied, "enrollment token is not the shield's current token")
}

func csrHasSPIFFEURI(csr *x509.CertificateRequest, expectedURI string) bool {
	for _, uri := range csr.URIs {
		if uri.String() == expectedURI {
			return true
		}
	}
	return false
}
