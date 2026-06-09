package shield

import (
	"context"
	"errors"

	pgx "github.com/jackc/pgx/v5"
	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RenewCert handles the ShieldService.RenewCert RPC — re-issuing a shield's
// leaf certificate before its 7-day cert expires. Without this the shield's
// renewal fails and it mTLS-locks-out a week after enrollment.
//
// Trust model: the shield's renewal is PROXIED by its connector over the
// connector's own mTLS channel, so the verified SPIFFE identity in context is
// the CONNECTOR's, not the shield's. The chain is:
//   - the controller trusts the connector (its cert is verified by the SPIFFE
//     interceptor before this handler runs),
//   - the connector verified the shield's mTLS before proxying,
//   - here we confirm req.ShieldId is a shield managed by THAT connector.
// A connector already controls its shields' traffic, so letting it renew their
// certs stays within the existing trust boundary.
//
// req.PublicKeyDer carries a DER-encoded PKCS#10 CSR (the field name is
// historical) — RenewShieldCert verifies the CSR signature (proof of key
// possession) and signs a fresh leaf with the same SPIFFE SAN.
func (s *service) RenewCert(ctx context.Context, req *shieldpb.RenewCertRequest) (*shieldpb.RenewCertResponse, error) {
	if spiffe.Role(ctx) != appmeta.SPIFFERoleConnector {
		return nil, status.Error(codes.PermissionDenied, "shield cert renewal must be proxied by a connector")
	}
	connectorID := spiffe.EntityID(ctx)
	trustDomain := spiffe.TrustDomain(ctx)

	shieldID := req.ShieldId
	if shieldID == "" {
		return nil, status.Error(codes.InvalidArgument, "shield_id is required")
	}

	var shieldStatus, tenantID, shieldConnectorID, shieldTrustDomain string
	err := s.db.QueryRow(ctx,
		`SELECT status, tenant_id, connector_id, COALESCE(trust_domain, '')
		   FROM shields
		  WHERE id = $1`,
		shieldID,
	).Scan(&shieldStatus, &tenantID, &shieldConnectorID, &shieldTrustDomain)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "shield not found")
		}
		return nil, status.Errorf(codes.Internal, "load shield: %v", err)
	}
	if shieldStatus == "revoked" || shieldStatus == "deleted" {
		return nil, status.Errorf(codes.PermissionDenied, "shield status is %q", shieldStatus)
	}
	if shieldConnectorID != connectorID {
		return nil, status.Error(codes.PermissionDenied, "shield is not managed by the requesting connector")
	}
	if shieldTrustDomain != "" && shieldTrustDomain != trustDomain {
		return nil, status.Error(codes.PermissionDenied, "shield trust domain mismatch")
	}

	certResult, err := s.pki.RenewShieldCert(ctx, tenantID, shieldID, trustDomain, req.PublicKeyDer, s.cfg.CertTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "renew shield cert: %v", err)
	}

	_, err = s.db.Exec(ctx,
		`UPDATE shields
		    SET cert_serial = $1,
		        cert_not_after = $2,
		        updated_at = NOW()
		  WHERE id = $3 AND tenant_id = $4`,
		certResult.Serial,
		certResult.NotAfter,
		shieldID,
		tenantID,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update shield cert: %v", err)
	}

	return &shieldpb.RenewCertResponse{
		CertificatePem:    []byte(certResult.CertificatePEM),
		WorkspaceCaPem:    []byte(certResult.WorkspaceCAPEM),
		IntermediateCaPem: []byte(certResult.IntermediateCAPEM),
	}, nil
}
