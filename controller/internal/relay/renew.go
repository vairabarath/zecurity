package relay

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log"
	"time"

	"github.com/valkey-io/valkey-go/valkeycompat"
	relaypb "github.com/yourorg/ztna/controller/gen/go/proto/relay/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Relay renewal idempotency (D-19). A relay retrying RenewCert while still
// presenting the same certificate — e.g. because the previous response was
// lost — gets back the SAME renewed certificate instead of a new one.
const (
	relayRenewResultPrefix = "relay:renew:result:" // + relayID + ":" + presented serial
	relayRenewLockPrefix   = "relay:renew:lock:"   // + relayID + ":" + presented serial
	relayRenewResultTTL    = time.Hour
	relayRenewLockTTL      = 30 * time.Second
)

// cachedRenewal is the response issued for one (relay, presented certificate)
// pair. Certificates are public; no private key material is cached.
type cachedRenewal struct {
	Serial            string `json:"serial"`
	CertificatePEM    string `json:"certificate_pem"`
	IntermediateCAPEM string `json:"intermediate_ca_pem"`
	NotBeforeUnix     int64  `json:"not_before_unix"`
	NotAfterUnix      int64  `json:"not_after_unix"`
	PublicKeySHA256   string `json:"public_key_sha256"`
}

func (c *cachedRenewal) response() *relaypb.RenewCertResponse {
	return &relaypb.RenewCertResponse{
		CertificatePem:    []byte(c.CertificatePEM),
		IntermediateCaPem: []byte(c.IntermediateCAPEM),
		CertNotAfterUnix:  c.NotAfterUnix,
		CertNotBeforeUnix: c.NotBeforeUnix,
	}
}

// RenewCert renews the calling relay's own certificate (D-19).
//
// The SPIFFE interceptor has already verified the presented certificate chains
// to the Platform Intermediate and is not on the relay revocation list.
// Before signing, this handler requires:
//   - the presented certificate has not expired (an expired relay is replaced,
//     not renewed — D-20);
//   - the CSR is signed by the SAME key as the presented certificate;
//   - the relay row is active or inactive;
//   - the presented certificate is a known, unrevoked row in relay_certificates;
//   - every CSR check SignRelayCert makes passes, with DNS/IP SANs inside the
//     relay's stored allowlists (Phase C rules, reused).
//
// The new certificate is recorded under the same FOR UPDATE lock RevokeRelay
// takes (RecordRenewedCert). The previous certificate is NOT revoked; it
// expires naturally so in-flight sessions are unaffected.
func (s *Service) RenewCert(ctx context.Context, req *relaypb.RenewCertRequest) (*relaypb.RenewCertResponse, error) {
	if req == nil {
		return nil, status.Error(codes.InvalidArgument, "request is required")
	}
	relayID, leaf, err := authenticatedRelay(ctx)
	if err != nil {
		return nil, err
	}
	if s.store == nil || s.pki == nil {
		return nil, status.Error(codes.FailedPrecondition, "relay renewal is not configured")
	}
	if !time.Now().Before(leaf.NotAfter) {
		return nil, status.Error(codes.FailedPrecondition, "relay certificate has expired; the relay must be replaced")
	}

	csr, err := x509.ParseCertificateRequest(req.CsrDer)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "parse Relay CSR: %v", err)
	}
	// D-19: renew with the existing private key only.
	if !samePublicKey(csr.PublicKey, leaf.PublicKey) {
		return nil, status.Error(codes.PermissionDenied, "renewal CSR must be signed by the relay's existing key")
	}
	presentedSerial := leaf.SerialNumber.Text(16)

	row, err := s.store.LoadRelayByID(ctx, relayID)
	if errors.Is(err, ErrRelayNotFound) {
		return nil, status.Error(codes.FailedPrecondition, "relay not registered")
	}
	if err != nil {
		return nil, status.Error(codes.Internal, "load Relay registration")
	}
	if row.Status != "active" && row.Status != "inactive" {
		return nil, status.Error(codes.FailedPrecondition, "relay is not in a renewable state")
	}

	known, revoked, err := s.store.RelayCertStatus(ctx, relayID, presentedSerial)
	if err != nil {
		return nil, status.Error(codes.Internal, "load presented Relay certificate")
	}
	if !known || revoked {
		return nil, status.Error(codes.PermissionDenied, "presented relay certificate is unknown or revoked")
	}

	allow, err := newRelaySANAllowlist(row.DNSAllowlist, row.IPAllowlist)
	if err != nil {
		log.Printf("relay renew: relay=%s stored SAN allowlist invalid: %v", relayID, err)
		return nil, status.Error(codes.FailedPrecondition, "relay SAN allowlist is invalid")
	}
	if err := precheckRelayCSR(relayID, csr, allow); err != nil {
		log.Printf("relay renew: relay=%s CSR rejected: %v", relayID, err)
		return nil, err
	}

	keyFingerprint, err := publicKeySHA256(leaf.PublicKey)
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "presented relay certificate public key is invalid")
	}

	// Idempotent retry: the same (relay, presented certificate) was already renewed.
	if resp, ok := s.cachedRenewal(ctx, relayID, presentedSerial, keyFingerprint); ok {
		log.Printf("relay renew: relay=%s presented=%s returned cached renewal (idempotent retry)", relayID, presentedSerial)
		return resp, nil
	}
	release, acquired := s.acquireRenewLock(ctx, relayID, presentedSerial)
	if !acquired {
		return nil, status.Error(codes.Aborted, "relay renewal already in progress; retry")
	}
	defer release()
	// A concurrent attempt may have finished while this one waited for the lock.
	if resp, ok := s.cachedRenewal(ctx, relayID, presentedSerial, keyFingerprint); ok {
		return resp, nil
	}

	// The signer gets the STORED allowlists and re-validates the CSR independently.
	cert, err := s.pki.SignRelayCert(ctx, relayID, csr, allow.dnsNames, allow.ipAddresses, s.certTTL)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign Relay certificate: %v", err)
	}

	superseded, err := s.store.RecordRenewedCert(ctx, relayID, presentedSerial, cert.Serial, cert.NotAfter)
	switch {
	case errors.Is(err, ErrRelayNotFound), errors.Is(err, ErrRelayNotRenewable):
		// Revoked/deleted between the checks above and the locked write: the
		// signed certificate is discarded, never returned.
		return nil, status.Error(codes.FailedPrecondition, "relay is not in a renewable state")
	case errors.Is(err, ErrPresentedCertInvalid):
		return nil, status.Error(codes.PermissionDenied, "presented relay certificate is unknown or revoked")
	case err != nil:
		log.Printf("relay renew: relay=%s record renewed cert: %v", relayID, err)
		return nil, status.Error(codes.Internal, "record renewed Relay certificate")
	}

	result := &cachedRenewal{
		Serial:            cert.Serial,
		CertificatePEM:    cert.CertificatePEM,
		IntermediateCAPEM: cert.IntermediateCAPEM,
		NotBeforeUnix:     cert.NotBefore.Unix(),
		NotAfterUnix:      cert.NotAfter.Unix(),
		PublicKeySHA256:   keyFingerprint,
	}
	s.storeRenewal(ctx, relayID, presentedSerial, result)

	log.Printf("relay renew: relay=%s old_serial=%s new_serial=%s not_after=%s superseded=%d",
		relayID, presentedSerial, cert.Serial, cert.NotAfter.Format(time.RFC3339), superseded)
	return result.response(), nil
}

// cachedRenewal returns the renewal already issued for this presented
// certificate, provided it was issued for the same key and the renewed
// certificate is still live (not revoked or superseded).
func (s *Service) cachedRenewal(ctx context.Context, relayID, presentedSerial, keyFingerprint string) (*relaypb.RenewCertResponse, bool) {
	if s.redis == nil {
		return nil, false
	}
	raw, err := s.redis.Get(ctx, renewalKey(relayRenewResultPrefix, relayID, presentedSerial)).Result()
	if err != nil {
		if err != valkeycompat.Nil {
			log.Printf("relay renew: read cached renewal relay=%s: %v", relayID, err)
		}
		return nil, false
	}
	var cached cachedRenewal
	if err := json.Unmarshal([]byte(raw), &cached); err != nil || cached.PublicKeySHA256 != keyFingerprint {
		return nil, false
	}
	known, revoked, err := s.store.RelayCertStatus(ctx, relayID, cached.Serial)
	if err != nil || !known || revoked {
		return nil, false
	}
	return cached.response(), true
}

func (s *Service) storeRenewal(ctx context.Context, relayID, presentedSerial string, result *cachedRenewal) {
	if s.redis == nil {
		return
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return
	}
	if err := s.redis.Set(ctx, renewalKey(relayRenewResultPrefix, relayID, presentedSerial), raw, relayRenewResultTTL).Err(); err != nil {
		log.Printf("relay renew: cache renewal relay=%s: %v", relayID, err)
	}
}

// acquireRenewLock serializes concurrent renewal attempts for the same
// presented certificate so they cannot each sign a certificate. Without a
// cache the lock is skipped; RecordRenewedCert still bounds live successors.
// A Valkey error does not block renewal (fail open to the DB guarantee).
func (s *Service) acquireRenewLock(ctx context.Context, relayID, presentedSerial string) (func(), bool) {
	if s.redis == nil {
		return func() {}, true
	}
	key := renewalKey(relayRenewLockPrefix, relayID, presentedSerial)
	token := randomToken()
	ok, err := s.redis.SetNX(ctx, key, token, relayRenewLockTTL).Result()
	if err != nil {
		log.Printf("relay renew: acquire lock relay=%s: %v", relayID, err)
		return func() {}, true
	}
	if !ok {
		return nil, false
	}
	return func() {
		// Release only our own lock (it may have expired and been re-acquired).
		if current, err := s.redis.Get(context.Background(), key).Result(); err == nil && current == token {
			_ = s.redis.Del(context.Background(), key).Err()
		}
	}, true
}

func renewalKey(prefix, relayID, presentedSerial string) string {
	return prefix + relayID + ":" + presentedSerial
}

func samePublicKey(a, b crypto.PublicKey) bool {
	eq, ok := a.(interface{ Equal(crypto.PublicKey) bool })
	return ok && b != nil && eq.Equal(b)
}

func publicKeySHA256(pub crypto.PublicKey) (string, error) {
	der, err := x509.MarshalPKIXPublicKey(pub)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(der)
	return hex.EncodeToString(sum[:]), nil
}

func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
