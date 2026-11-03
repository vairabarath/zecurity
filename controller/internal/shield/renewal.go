package shield

import (
	"context"
	"time"

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
//
// A connector already controls its shields' traffic, so letting it renew their
// certs stays within the existing trust boundary — but only while it is
// trusted (certificate audit bug 7). A renewed certificate is returned only if
// the connector is not revoked (status <> 'revoked' AND revoked_at IS NULL)
// in the caller's trust domain, and the shield is managed by it, is not
// revoked or deleted, and has no revocation tombstone. The interceptor already
// refuses revoked connectors; this handler checks again so it is safe on its
// own, and the final write re-checks everything atomically, so a revocation
// that lands while the certificate is being signed returns no certificate.
// Unreadable authorization state fails closed.
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

	// One read: the tombstone (which outlives the shields row), the shield and
	// its connector.
	var tombstoned bool
	var shieldStatus, tenantID, shieldConnectorID, shieldTrustDomain *string
	var connStatus, connTrustDomain, pinnedKey *string
	var connRevokedAt *time.Time
	err := s.db.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM shield_revocations WHERE shield_id = $1),
		        s.status, s.tenant_id::text, s.connector_id::text, COALESCE(s.trust_domain, ''),
		        c.status, c.revoked_at, COALESCE(c.trust_domain, ''),
		        s.public_key_fingerprint
		   FROM (SELECT 1) AS one
		   LEFT JOIN shields s ON s.id = $1
		   LEFT JOIN connectors c ON c.id = s.connector_id`,
		shieldID,
	).Scan(&tombstoned, &shieldStatus, &tenantID, &shieldConnectorID, &shieldTrustDomain,
		&connStatus, &connRevokedAt, &connTrustDomain, &pinnedKey)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "load shield: %v", err)
	}
	if tombstoned {
		return nil, status.Error(codes.PermissionDenied, "shield is revoked")
	}
	if shieldStatus == nil {
		return nil, status.Error(codes.NotFound, "shield not found")
	}
	if *shieldStatus == "revoked" || *shieldStatus == "deleted" {
		return nil, status.Errorf(codes.PermissionDenied, "shield status is %q", *shieldStatus)
	}
	if *shieldConnectorID != connectorID {
		return nil, status.Error(codes.PermissionDenied, "shield is not managed by the requesting connector")
	}
	if *shieldTrustDomain != "" && *shieldTrustDomain != trustDomain {
		return nil, status.Error(codes.PermissionDenied, "shield trust domain mismatch")
	}
	if connStatus == nil || *connTrustDomain != trustDomain {
		return nil, status.Error(codes.PermissionDenied, "connector not found in the caller's trust domain")
	}
	if *connStatus == "revoked" || connRevokedAt != nil {
		return nil, status.Error(codes.PermissionDenied, "connector is revoked")
	}

	// Certificate audit bug 10: renew only for the key the shield enrolled
	// with. A missing or malformed pin is refused: the shield must re-enroll
	// (no trust on first use). The pin is never changed by renewal.
	csrKeyFingerprint, err := renewalKeyFingerprint(req.PublicKeyDer)
	if err != nil {
		return nil, err
	}
	if pinnedKey == nil || !validKeyFingerprint(*pinnedKey) {
		return nil, status.Error(codes.PermissionDenied, "shield has no valid pinned key; re-enroll the shield")
	}
	if *pinnedKey != csrKeyFingerprint {
		return nil, status.Error(codes.PermissionDenied, "renewal key does not match the shield's enrolled key")
	}

	certResult, err := s.pki.RenewShieldCert(ctx, *tenantID, shieldID, trustDomain, req.PublicKeyDer, s.cfg.CertTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "renew shield cert: %v", err)
	}

	// Record the certificate only if everything still holds. FOR SHARE makes
	// a concurrent connector revocation either wait for this write or be seen
	// by it; a concurrent shield revocation updates the shields row, so the
	// same applies to it.
	tag, err := s.db.Exec(ctx,
		`WITH conn AS (
		     SELECT id FROM connectors
		      WHERE id = $5 AND trust_domain = $6
		        AND status <> 'revoked' AND revoked_at IS NULL
		        FOR SHARE
		 )
		 UPDATE shields s
		    SET cert_serial = $1,
		        cert_not_after = $2,
		        updated_at = NOW()
		   FROM conn
		  WHERE s.id = $3 AND s.tenant_id = $4
		    AND s.connector_id = conn.id
		    AND s.status NOT IN ('revoked', 'deleted')
		    AND COALESCE(s.trust_domain, '') IN ('', $6)
		    AND NOT EXISTS (SELECT 1 FROM shield_revocations r WHERE r.shield_id = s.id)
		    AND s.public_key_fingerprint = $7`,
		certResult.Serial,
		certResult.NotAfter,
		shieldID,
		*tenantID,
		connectorID,
		trustDomain,
		csrKeyFingerprint,
	)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "update shield cert: %v", err)
	}
	if tag.RowsAffected() == 0 {
		return nil, status.Error(codes.PermissionDenied, "shield or connector is no longer eligible for renewal")
	}

	return &shieldpb.RenewCertResponse{
		CertificatePem:    []byte(certResult.CertificatePEM),
		WorkspaceCaPem:    []byte(certResult.WorkspaceCAPEM),
		IntermediateCaPem: []byte(certResult.IntermediateCAPEM),
	}, nil
}
