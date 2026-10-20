package connector

import (
	"context"
	"crypto/x509"
	"fmt"
	"sync"
	"testing"
	"time"

	pb "github.com/yourorg/ztna/controller/gen/go/proto/connector/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/codes"
)

// Certificate audit bug 9 (connector issuance paths): the workspace CRL read
// the connector's current cert_serial only, and every renewal overwrote it,
// so revoking a renewed connector left its earlier certificates off the CRL.
// These tests drive the real Enroll and RenewCert handlers with real signing.

func (f *bindingFixture) renew(h *EnrollmentHandler) (*pb.RenewCertResponse, error) {
	ctx := spiffe.WithIdentity(f.ctx, fmt.Sprintf("spiffe://%s/connector/%s", f.td, f.connID),
		appmeta.SPIFFERoleConnector, f.connID, f.td)
	return h.RenewCert(ctx, &pb.RenewCertRequest{PublicKeyDer: generateConnectorCSR(f.t, f.connID, f.td)})
}

func (f *bindingFixture) mustEnroll() string {
	f.t.Helper()
	resp, err := f.handler(f.pki).Enroll(f.ctx, f.request(f.mustReissue()))
	if err != nil {
		f.t.Fatalf("enroll: %v", err)
	}
	return certSerial(f.t, resp)
}

func (f *bindingFixture) mustRenew() string {
	f.t.Helper()
	resp, err := f.renew(f.handler(f.pki))
	if err != nil {
		f.t.Fatalf("renew: %v", err)
	}
	return pemSerial(f.t, resp.CertificatePem)
}

func pemSerial(t *testing.T, certPEM []byte) string {
	t.Helper()
	return certSerial(t, &pb.EnrollResponse{CertificatePem: certPEM})
}

// revoke revokes the connector exactly as the revokeConnector resolver does.
func (f *bindingFixture) revoke() {
	f.t.Helper()
	f.exec(`UPDATE connectors
	           SET status = 'revoked', revoked_at = NOW(), revocation_reason = 'revoked via admin API', updated_at = NOW()
	         WHERE id = $1 AND tenant_id = $2 AND status IN ('active', 'disconnected') AND revoked_at IS NULL`,
		f.connID, f.wsID)
}

func (f *bindingFixture) crlSerials() map[string]bool {
	f.t.Helper()
	der, err := f.pki.GenerateClientCRL(f.ctx, f.wsID)
	if err != nil {
		f.t.Fatalf("GenerateClientCRL: %v", err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		f.t.Fatalf("parse CRL: %v", err)
	}
	got := map[string]bool{}
	for _, e := range list.RevokedCertificateEntries {
		got[e.SerialNumber.Text(16)] = true
	}
	return got
}

// exactlyListed asserts the CRL lists exactly these serials.
func (f *bindingFixture) exactlyListed(serials ...string) {
	f.t.Helper()
	crl := f.crlSerials()
	for _, s := range serials {
		if !crl[s] {
			f.t.Errorf("issued serial %s is not on the CRL (CRL: %v)", s, crl)
		}
	}
	if len(crl) != len(serials) {
		f.t.Errorf("CRL lists %d serials, want exactly %d: %v", len(crl), len(serials), crl)
	}
}

func TestConnectorCertificateHistory_EnrollmentAndRenewalsAllRevocable(t *testing.T) {
	f := newBindingFixture(t)
	issued := []string{f.mustEnroll(), f.mustRenew(), f.mustRenew()}
	if len(f.crlSerials()) != 0 {
		t.Fatal("nothing may be listed before revocation")
	}
	f.revoke()
	f.exactlyListed(issued...)
}

// A failed enrollment (commit refused) and a refused renewal (revoked) issue
// nothing to the caller, so nothing of theirs may appear later.
func TestConnectorCertificateHistory_FailedIssuanceRecordsNothing(t *testing.T) {
	f := newBindingFixture(t)
	token := f.mustReissue()
	f.exec(`CREATE FUNCTION fail_commit() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'commit refused'; END; $$ LANGUAGE plpgsql`)
	f.exec(`CREATE CONSTRAINT TRIGGER fail_commit AFTER UPDATE ON connectors
	        DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
	        WHEN (NEW.status = 'active') EXECUTE FUNCTION fail_commit()`)
	resp, err := f.handler(f.pki).Enroll(f.ctx, f.request(token))
	refusedEnrollment(t, resp, err, codes.Internal)
	f.exec(`DROP TRIGGER fail_commit ON connectors`)

	resp, err = f.handler(f.pki).Enroll(f.ctx, f.request(token))
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	enrolled := certSerial(t, resp)
	f.revoke()
	if _, err := f.renew(f.handler(f.pki)); err == nil {
		t.Fatal("a revoked connector renewed")
	}
	f.exactlyListed(enrolled)
}

// renewHookPKI runs during once inside the first renewal signing.
type renewHookPKI struct {
	pki.Service
	once   sync.Once
	during func()
}

func (p *renewHookPKI) RenewConnectorCert(ctx context.Context, tenantID, connectorID, trustDomain string, csr []byte, ttl time.Duration) (*pki.ConnectorCertResult, error) {
	if p.during != nil {
		p.once.Do(p.during)
	}
	return p.Service.RenewConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, ttl)
}

// A revocation that lands while a renewal is being signed: the renewal is
// refused, and every certificate the connector held is listed.
func TestConnectorCertificateHistory_RenewalRacingRevocation(t *testing.T) {
	f := newBindingFixture(t)
	issued := []string{f.mustEnroll(), f.mustRenew()}
	hook := &renewHookPKI{Service: f.pki, during: f.revoke}
	if resp, err := f.renew(f.handler(hook)); err == nil {
		t.Fatalf("the racing renewal returned a certificate (%s)", pemSerial(t, resp.CertificatePem))
	}
	f.exactlyListed(issued...)
}
