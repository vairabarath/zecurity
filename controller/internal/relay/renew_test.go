package relay

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/spiffe"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

// Sprint 20 Phase F-1 — RelayService.RenewCert (controller half, D-19).

const presentedSerialHex = "2a" // big.NewInt(42) in the test leaf

// fakeRenewStore is a heartbeatStore that models relay_certificates for renewal.
type fakeRenewStore struct {
	fakeProvisionStore
	certs     map[string]bool // serial → revoked
	recordErr error
	recorded  []string // new serials passed to RecordRenewedCert
	presented []string // presented serials passed to RecordRenewedCert
}

func newFakeRenewStore(status string, dnsAllowlist, ipAllowlist []string) *fakeRenewStore {
	s := &fakeRenewStore{certs: map[string]bool{presentedSerialHex: false}}
	s.row = &RelayRow{ID: testRelayID, Status: status, DNSAllowlist: dnsAllowlist, IPAllowlist: ipAllowlist}
	return s
}

func (s *fakeRenewStore) RelayCertStatus(_ context.Context, _, serial string) (bool, bool, error) {
	revoked, known := s.certs[serial]
	return known, revoked, nil
}

func (s *fakeRenewStore) RecordRenewedCert(_ context.Context, _, presentedSerial, newSerial string, _ time.Time) (int, error) {
	if s.recordErr != nil {
		return 0, s.recordErr
	}
	s.presented = append(s.presented, presentedSerial)
	s.recorded = append(s.recorded, newSerial)
	s.certs[newSerial] = false
	return 0, nil
}

// renewContext is an authenticated relay mTLS context whose presented leaf
// carries pub (so same-key checks are meaningful) and the given expiry.
func renewContext(t *testing.T, relayID string, pub *ecdsa.PublicKey, notAfter time.Time) context.Context {
	t.Helper()
	spiffeURI, err := url.Parse(appmeta.RelaySPIFFEID(relayID))
	if err != nil {
		t.Fatalf("parse SPIFFE URI: %v", err)
	}
	leaf := &x509.Certificate{
		SerialNumber: big.NewInt(42),
		NotAfter:     notAfter,
		URIs:         []*url.URL{spiffeURI},
		PublicKey:    pub,
	}
	ctx := spiffe.WithIdentity(context.Background(), appmeta.RelaySPIFFEID(relayID),
		appmeta.SPIFFERoleRelay, relayID, appmeta.SPIFFEGlobalTrustDomain)
	return peer.NewContext(ctx, &peer.Peer{
		Addr:     &net.TCPAddr{IP: net.ParseIP("192.168.1.71"), Port: 54321},
		AuthInfo: credentials.TLSInfo{State: tls.ConnectionState{PeerCertificates: []*x509.Certificate{leaf}}},
	})
}

func newRelayKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	return key
}

// renewCSR builds a relay CSR signed by key.
func renewCSR(t *testing.T, key *ecdsa.PrivateKey, relayID string, dnsNames, ips []string) []byte {
	t.Helper()
	spiffeURI, err := url.Parse(appmeta.RelaySPIFFEID(relayID))
	if err != nil {
		t.Fatalf("parse SPIFFE URI: %v", err)
	}
	template := &x509.CertificateRequest{URIs: []*url.URL{spiffeURI}, DNSNames: dnsNames}
	for _, v := range ips {
		template.IPAddresses = append(template.IPAddresses, net.ParseIP(v))
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, template, key)
	if err != nil {
		t.Fatalf("create CSR: %v", err)
	}
	return der
}

type renewFixture struct {
	fake    *fakePKI
	store   *fakeRenewStore
	service *Service
	key     *ecdsa.PrivateKey
	ctx     context.Context
}

func newRenewFixture(t *testing.T, store *fakeRenewStore) *renewFixture {
	t.Helper()
	key := newRelayKey(t)
	fake := newFakePKI()
	fake.result.Serial = "b0b" // must differ from the presented leaf's serial (2a)
	service := NewService(fake, store, time.Hour).WithHeartbeatCache(newProvisionTestValkey(t), time.Minute)
	return &renewFixture{
		fake:    fake,
		store:   store,
		service: service,
		key:     key,
		ctx:     renewContext(t, testRelayID, &key.PublicKey, time.Now().Add(time.Hour)),
	}
}

func (f *renewFixture) renew(t *testing.T, dnsNames, ips []string) (*relaypb.RenewCertResponse, error) {
	t.Helper()
	return f.service.RenewCert(f.ctx, &relaypb.RenewCertRequest{CsrDer: renewCSR(t, f.key, testRelayID, dnsNames, ips)})
}

func (f *renewFixture) assertNotSigned(t *testing.T) {
	t.Helper()
	if f.fake.signCalls != 0 || len(f.store.recorded) != 0 {
		t.Fatalf("signCalls=%d recorded=%v, want nothing signed or recorded", f.fake.signCalls, f.store.recorded)
	}
}

// TestRenewCert_SameKeyRenewsWithStoredAllowlists — happy path: a same-key CSR
// from an active relay is signed with the STORED allowlists and recorded
// against the presented serial.
func TestRenewCert_SameKeyRenewsWithStoredAllowlists(t *testing.T) {
	f := newRenewFixture(t, newFakeRenewStore("active", []string{"relay.example.com", "alt.example.com"}, []string{"203.0.113.10"}))

	resp, err := f.renew(t, []string{"relay.example.com"}, []string{"203.0.113.10"})
	if err != nil {
		t.Fatalf("RenewCert: %v", err)
	}
	if string(resp.CertificatePem) != "relay-cert" || string(resp.IntermediateCaPem) != "intermediate-cert" {
		t.Fatalf("unexpected response: %+v", resp)
	}
	if f.fake.signCalls != 1 || len(f.fake.dnsNames) != 2 || len(f.fake.ipAddrs) != 1 {
		t.Fatalf("signer calls=%d dns=%v ip=%v, want 1 call with the stored allowlists", f.fake.signCalls, f.fake.dnsNames, f.fake.ipAddrs)
	}
	if len(f.store.presented) != 1 || f.store.presented[0] != presentedSerialHex {
		t.Fatalf("recorded against presented %v, want [%s]", f.store.presented, presentedSerialHex)
	}
}

// TestRenewCert_InactiveRelayCanRenew — an evicted (inactive) relay is still
// provisioned and must be able to renew.
func TestRenewCert_InactiveRelayCanRenew(t *testing.T) {
	f := newRenewFixture(t, newFakeRenewStore("inactive", nil, nil))
	if _, err := f.renew(t, nil, nil); err != nil {
		t.Fatalf("RenewCert for inactive relay: %v", err)
	}
}

// TestRenewCert_RejectsDifferentKey — D-19: renewal must reuse the existing key.
func TestRenewCert_RejectsDifferentKey(t *testing.T) {
	f := newRenewFixture(t, newFakeRenewStore("active", nil, nil))
	other := newRelayKey(t)

	_, err := f.service.RenewCert(f.ctx, &relaypb.RenewCertRequest{CsrDer: renewCSR(t, other, testRelayID, nil, nil)})
	assertStatusCode(t, err, codes.PermissionDenied)
	f.assertNotSigned(t)
}

// TestRenewCert_RejectsExpiredCertificate — D-20: an expired relay is replaced,
// not renewed (the interceptor also rejects it; this is defense in depth).
func TestRenewCert_RejectsExpiredCertificate(t *testing.T) {
	f := newRenewFixture(t, newFakeRenewStore("active", nil, nil))
	f.ctx = renewContext(t, testRelayID, &f.key.PublicKey, time.Now().Add(-time.Minute))

	_, err := f.renew(t, nil, nil)
	assertStatusCode(t, err, codes.FailedPrecondition)
	f.assertNotSigned(t)
}

// TestRenewCert_RejectsNonRenewableStatus — pending, revoked and deleted
// relays cannot renew.
func TestRenewCert_RejectsNonRenewableStatus(t *testing.T) {
	for _, st := range []string{"pending", "revoked", "deleted"} {
		t.Run(st, func(t *testing.T) {
			f := newRenewFixture(t, newFakeRenewStore(st, nil, nil))
			_, err := f.renew(t, nil, nil)
			assertStatusCode(t, err, codes.FailedPrecondition)
			f.assertNotSigned(t)
		})
	}
}

// TestRenewCert_RejectsUnknownOrRevokedPresentedCert — the presented cert must
// be a known, unrevoked relay_certificates row (defense in depth over the
// interceptor's 60s revocation cache).
func TestRenewCert_RejectsUnknownOrRevokedPresentedCert(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		store := newFakeRenewStore("active", nil, nil)
		delete(store.certs, presentedSerialHex)
		f := newRenewFixture(t, store)
		_, err := f.renew(t, nil, nil)
		assertStatusCode(t, err, codes.PermissionDenied)
		f.assertNotSigned(t)
	})
	t.Run("revoked", func(t *testing.T) {
		store := newFakeRenewStore("active", nil, nil)
		store.certs[presentedSerialHex] = true
		f := newRenewFixture(t, store)
		_, err := f.renew(t, nil, nil)
		assertStatusCode(t, err, codes.PermissionDenied)
		f.assertNotSigned(t)
	})
}

// TestRenewCert_ReusesPhaseCAllowlistRules — renewed SANs stay within the
// stored allowlist, and CSR DNS SANs must be canonical (Phase C rules).
func TestRenewCert_ReusesPhaseCAllowlistRules(t *testing.T) {
	t.Run("SAN outside allowlist", func(t *testing.T) {
		f := newRenewFixture(t, newFakeRenewStore("active", []string{"relay.example.com"}, nil))
		_, err := f.renew(t, []string{"login.bank.example"}, nil)
		assertStatusCode(t, err, codes.PermissionDenied)
		f.assertNotSigned(t)
	})
	t.Run("non-canonical CSR DNS", func(t *testing.T) {
		f := newRenewFixture(t, newFakeRenewStore("active", []string{"relay.example.com"}, nil))
		_, err := f.renew(t, []string{"Relay.Example.com"}, nil)
		assertStatusCode(t, err, codes.InvalidArgument)
		f.assertNotSigned(t)
	})
	t.Run("IPv6 compared as parsed value", func(t *testing.T) {
		f := newRenewFixture(t, newFakeRenewStore("active", nil, []string{"2001:db8::1"}))
		if _, err := f.renew(t, nil, []string{"2001:DB8:0:0::1"}); err != nil {
			t.Fatalf("equivalent IPv6 SAN rejected: %v", err)
		}
	})
}

// TestRenewCert_RejectsWrongIdentity — shared relay identity checks (same as
// Heartbeat): wrong role or a leaf for another relay is refused.
func TestRenewCert_RejectsWrongIdentity(t *testing.T) {
	f := newRenewFixture(t, newFakeRenewStore("active", nil, nil))
	csr := renewCSR(t, f.key, testRelayID, nil, nil)

	ctx := relayHeartbeatContext(t, appmeta.SPIFFERoleConnector, testRelayID, testRelayID, time.Now().Add(time.Hour))
	_, err := f.service.RenewCert(ctx, &relaypb.RenewCertRequest{CsrDer: csr})
	assertStatusCode(t, err, codes.PermissionDenied)

	ctx = relayHeartbeatContext(t, appmeta.SPIFFERoleRelay, testRelayID, otherTestRelayID, time.Now().Add(time.Hour))
	_, err = f.service.RenewCert(ctx, &relaypb.RenewCertRequest{CsrDer: csr})
	assertStatusCode(t, err, codes.Unauthenticated)
	f.assertNotSigned(t)
}

// TestRenewCert_IdenticalRetryIsIdempotent — a retry while still presenting
// the same certificate (e.g. the response was lost) returns the SAME renewed
// certificate: nothing is signed or recorded a second time.
func TestRenewCert_IdenticalRetryIsIdempotent(t *testing.T) {
	f := newRenewFixture(t, newFakeRenewStore("active", nil, nil))

	first, err := f.renew(t, nil, nil)
	if err != nil {
		t.Fatalf("first RenewCert: %v", err)
	}
	second, err := f.renew(t, nil, nil) // fresh CSR bytes, same key, same presented cert
	if err != nil {
		t.Fatalf("retry RenewCert: %v", err)
	}
	if string(first.CertificatePem) != string(second.CertificatePem) || first.CertNotAfterUnix != second.CertNotAfterUnix {
		t.Fatal("retry returned a different certificate")
	}
	if f.fake.signCalls != 1 || len(f.store.recorded) != 1 {
		t.Fatalf("signCalls=%d recorded=%v, want exactly one signature and one record", f.fake.signCalls, f.store.recorded)
	}
}

// TestRenewCert_CachedRenewalIgnoredOnceRevoked — a cached renewal whose
// certificate has since been revoked/superseded is not replayed.
func TestRenewCert_CachedRenewalIgnoredOnceRevoked(t *testing.T) {
	f := newRenewFixture(t, newFakeRenewStore("active", nil, nil))
	if _, err := f.renew(t, nil, nil); err != nil {
		t.Fatalf("first RenewCert: %v", err)
	}
	f.store.certs[f.store.recorded[0]] = true // renewed cert revoked

	if _, err := f.renew(t, nil, nil); err != nil {
		t.Fatalf("retry RenewCert: %v", err)
	}
	if f.fake.signCalls != 2 {
		t.Fatalf("signCalls=%d, want a fresh signature once the cached cert is revoked", f.fake.signCalls)
	}
}

// TestRenewCert_ConcurrentAttemptAborted — while an identical attempt holds the
// per-(relay, presented cert) lock, a second one is told to retry instead of
// signing a second certificate.
func TestRenewCert_ConcurrentAttemptAborted(t *testing.T) {
	f := newRenewFixture(t, newFakeRenewStore("active", nil, nil))
	if err := f.service.redis.Set(context.Background(),
		renewalKey(relayRenewLockPrefix, testRelayID, presentedSerialHex), "other-attempt", time.Minute).Err(); err != nil {
		t.Fatalf("seed lock: %v", err)
	}

	_, err := f.renew(t, nil, nil)
	assertStatusCode(t, err, codes.Aborted)
	f.assertNotSigned(t)
}

// TestRenewCert_RevokedDuringRenewalReturnsNoCert — the relay is revoked after
// the pre-checks but before the locked write: the signed certificate is
// discarded, never returned, and nothing is cached.
func TestRenewCert_RevokedDuringRenewalReturnsNoCert(t *testing.T) {
	store := newFakeRenewStore("active", nil, nil)
	store.recordErr = ErrRelayNotRenewable
	f := newRenewFixture(t, store)

	resp, err := f.renew(t, nil, nil)
	assertStatusCode(t, err, codes.FailedPrecondition)
	if resp != nil {
		t.Fatal("a certificate was returned for a relay revoked during renewal")
	}
	n, err := f.service.redis.Exists(context.Background(), renewalKey(relayRenewResultPrefix, testRelayID, presentedSerialHex)).Result()
	if err != nil || n != 0 {
		t.Fatalf("renewal was cached despite failing (exists=%d err=%v)", n, err)
	}
}
