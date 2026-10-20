package shield

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"fmt"
	"net/url"
	"sync"
	"testing"
	"time"

	shieldpb "github.com/yourorg/ztna/controller/gen/go/proto/shield/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/pki"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Certificate audit bug 10 (shield): RenewCert, proxied by the shield's
// connector, signed whatever public key the renewal CSR carried. The
// controller never sees the shield's own certificate, so it now pins the
// shield's key at enrollment and renews only for that key. The shield reuses
// its key on renewal; a key change needs re-enrollment.

func newShieldKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// shieldCSR is an enrollment/renewal CSR for shieldID signed by key.
func shieldCSR(t *testing.T, trustDomain, shieldID string, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	uri, _ := url.Parse(appmeta.ShieldSPIFFEID(trustDomain, shieldID))
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: "shield-" + shieldID},
		URIs:    []*url.URL{uri},
	}, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return der
}

// keyFingerprint is SHA-256 over the key's PKIX DER, lowercase hex (as the
// client and relay pins are computed).
func keyFingerprint(t *testing.T, key *ecdsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:])
}

// enrollWithKey enrolls the fixture's shield with key.
func (f *shieldBindingFixture) enrollWithKey(key *ecdsa.PrivateKey) {
	f.t.Helper()
	req := f.request(f.mustReissue())
	req.CsrDer = shieldCSR(f.t, f.td, f.shieldID, key)
	resp, err := f.svc(f.pki, f.rdb).Enroll(f.ctx, req)
	if err != nil {
		f.t.Fatalf("enroll: %v", err)
	}
	f.enrolledAs(resp, nil)
}

// renewAs renews the shield through its connector (the identity the SPIFFE
// interceptor injects) with a CSR signed by csrKey.
func (f *shieldBindingFixture) renewAs(svc *service, csrKey *ecdsa.PrivateKey) (*shieldpb.RenewCertResponse, error) {
	connID := f.scalar(`SELECT connector_id::text FROM shields WHERE id = $1`, f.shieldID)
	ctx := spiffe.WithIdentity(f.ctx, fmt.Sprintf("spiffe://%s/connector/%s", f.td, connID),
		appmeta.SPIFFERoleConnector, connID, f.td)
	return svc.RenewCert(ctx, &shieldpb.RenewCertRequest{ShieldId: f.shieldID, PublicKeyDer: shieldCSR(f.t, f.td, f.shieldID, csrKey)})
}

func (f *shieldBindingFixture) fingerprint() string {
	f.t.Helper()
	return f.scalar(`SELECT COALESCE(public_key_fingerprint, '') FROM shields WHERE id = $1`, f.shieldID)
}

// refusedRenewal asserts no certificate and no change to the recorded serial.
func (f *shieldBindingFixture) refusedRenewal(resp *shieldpb.RenewCertResponse, err error, serialBefore string) {
	f.t.Helper()
	if err == nil || resp != nil {
		f.t.Fatalf("renewal was not refused (err %v)", err)
	}
	if got := f.row().serial; got != serialBefore {
		f.t.Fatalf("recorded serial changed by a refused renewal: %s → %s", serialBefore, got)
	}
}

func TestShieldRenewalKey_SameKeyRenews(t *testing.T) {
	f := newShieldBindingFixture(t)
	key := newShieldKey(t)
	f.enrollWithKey(key)
	resp, err := f.renewAs(f.svc(f.pki, f.rdb), key)
	if err != nil {
		t.Fatalf("renewal with the enrolled key refused: %v", err)
	}
	if got := shieldCertSerial(t, &shieldpb.EnrollResponse{CertificatePem: resp.CertificatePem}); got != f.row().serial {
		t.Fatalf("returned serial %s is not the recorded %s", got, f.row().serial)
	}
}

// A connector cannot re-key its shield by proxying a CSR for another key.
func TestShieldRenewalKey_DifferentKeyRefused(t *testing.T) {
	f := newShieldBindingFixture(t)
	f.enrollWithKey(newShieldKey(t))
	before := f.row().serial
	resp, err := f.renewAs(f.svc(f.pki, f.rdb), newShieldKey(t))
	f.refusedRenewal(resp, err, before)
	if got := status.Code(err); got != codes.PermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", got)
	}
}

func TestShieldRenewalKey_EnrollmentPinsTheCSRKey(t *testing.T) {
	f := newShieldBindingFixture(t)
	key := newShieldKey(t)
	f.enrollWithKey(key)
	if got := f.fingerprint(); got != keyFingerprint(t, key) {
		t.Fatalf("pinned fingerprint %q, want the enrolled key's %q", got, keyFingerprint(t, key))
	}
	// Ordinary renewal never moves the pin.
	if _, err := f.renewAs(f.svc(f.pki, f.rdb), key); err != nil {
		t.Fatalf("renew: %v", err)
	}
	if got := f.fingerprint(); got != keyFingerprint(t, key) {
		t.Fatalf("renewal changed the pinned fingerprint to %q", got)
	}
}

// No trust on first use: a shield without a valid pin must re-enroll.
func TestShieldRenewalKey_MissingOrMalformedPinRefused(t *testing.T) {
	for name, value := range map[string]any{
		"missing":   nil,
		"empty":     "",
		"malformed": "not-a-fingerprint",
		"uppercase": "", // set below to the right key's fingerprint in uppercase
	} {
		t.Run(name, func(t *testing.T) {
			f := newShieldBindingFixture(t)
			key := newShieldKey(t)
			f.enrollWithKey(key)
			if name == "uppercase" {
				value = fmt.Sprintf("%X", mustHex(t, keyFingerprint(t, key)))
			}
			f.exec(`UPDATE shields SET public_key_fingerprint = $2 WHERE id = $1`, f.shieldID, value)
			before := f.row().serial
			resp, err := f.renewAs(f.svc(f.pki, f.rdb), key)
			f.refusedRenewal(resp, err, before)
		})
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("decode hex: %v", err)
	}
	return b
}

// If the pin cannot be read, nothing is signed.
func TestShieldRenewalKey_UnreadablePinFailsClosed(t *testing.T) {
	f := newShieldBindingFixture(t)
	key := newShieldKey(t)
	f.enrollWithKey(key)
	before := f.row().serial
	f.exec(`ALTER TABLE shields RENAME COLUMN public_key_fingerprint TO public_key_fingerprint_hidden`)
	hook := &shieldRenewHookPKI{Service: f.pki}
	resp, err := f.renewAs(f.svc(hook, f.rdb), key)
	if err == nil || resp != nil {
		t.Fatalf("renewal was not refused (err %v)", err)
	}
	if hook.calls != 0 {
		t.Fatal("a certificate was signed although the pin was unreadable")
	}
	if got := f.row().serial; got != before {
		t.Fatalf("recorded serial changed: %s → %s", before, got)
	}
}

type shieldRenewHookPKI struct {
	pki.Service
	once   sync.Once
	during func()
	mu     sync.Mutex
	calls  int
}

func (p *shieldRenewHookPKI) RenewShieldCert(ctx context.Context, tenantID, shieldID, trustDomain string, csr []byte, ttl time.Duration) (*pki.ShieldCertResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	if p.during != nil {
		p.once.Do(p.during)
	}
	return p.Service.RenewShieldCert(ctx, tenantID, shieldID, trustDomain, csr, ttl)
}

// The pin changes while the certificate is being signed (e.g. the shield is
// re-enrolled with another key): the final write refuses, no certificate.
func TestShieldRenewalKey_PinChangedDuringRenewalReturnsNoCertificate(t *testing.T) {
	f := newShieldBindingFixture(t)
	key := newShieldKey(t)
	f.enrollWithKey(key)
	before := f.row().serial
	other := keyFingerprint(t, newShieldKey(t))
	hook := &shieldRenewHookPKI{Service: f.pki, during: func() {
		f.exec(`UPDATE shields SET public_key_fingerprint = $2 WHERE id = $1`, f.shieldID, other)
	}}
	resp, err := f.renewAs(f.svc(hook, f.rdb), key)
	f.refusedRenewal(resp, err, before)
	if hook.calls != 1 {
		t.Fatalf("signing calls = %d, want 1 (the change lands mid-renewal)", hook.calls)
	}
}

// A failed enrollment commits no pin.
func TestShieldRenewalKey_FailedEnrollmentPinsNothing(t *testing.T) {
	f := newShieldBindingFixture(t)
	token := f.mustReissue()
	f.exec(`CREATE FUNCTION fail_pin_commit() RETURNS trigger AS $$ BEGIN RAISE EXCEPTION 'commit refused'; END; $$ LANGUAGE plpgsql`)
	f.exec(`CREATE CONSTRAINT TRIGGER fail_pin_commit AFTER UPDATE ON shields
	        DEFERRABLE INITIALLY DEFERRED FOR EACH ROW
	        WHEN (NEW.status = 'active') EXECUTE FUNCTION fail_pin_commit()`)
	req := f.request(token)
	req.CsrDer = shieldCSR(t, f.td, f.shieldID, newShieldKey(t))
	if resp, err := f.svc(f.pki, f.rdb).Enroll(f.ctx, req); err == nil || resp != nil {
		t.Fatalf("enrollment was not refused (err %v)", err)
	}
	if got := f.fingerprint(); got != "" {
		t.Fatalf("a failed enrollment pinned %q", got)
	}
}
