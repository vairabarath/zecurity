package pki

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"fmt"
	"math/big"
	"time"
)

// RevokedEntry is one revoked certificate for CRL generation: a canonical hex
// serial (as produced by SerialNumber.Text(16), matching relay_certificates.serial)
// and its revocation time.
type RevokedEntry struct {
	Serial    string
	RevokedAt time.Time
}

// relayCRLValidity is how long a generated relay CRL is valid (thisUpdate →
// nextUpdate). It must comfortably exceed the consumer refresh interval (60s) so a
// freshly-fetched CRL is never already past nextUpdate, while still bounding how
// long a stale cache may be trusted (consumers deny once past nextUpdate).
const relayCRLValidity = 10 * time.Minute

// GenerateRelayCRL builds a DER-encoded CRL signed by the platform Intermediate CA
// for the supplied revoked relay serials.
//
// Relay leaf certs are signed by the Intermediate CA (see SignRelayCert), NOT by a
// workspace CA — so relay revocation cannot reuse GenerateClientCRL (workspace-CA
// signed). This is the CRL that every off-controller verifier of a relay cert
// (connector, client) pulls to reject a revoked relay.
//
// It is a pure function of `revoked`: the caller (the /relay.crl handler) fetches
// the serials from the relay store and passes them in, so the PKI service needs no
// relay-store handle — which avoids the pki.Init-before-relayStore init-order
// problem and any pki→relay import cycle.
func (s *serviceImpl) GenerateRelayCRL(ctx context.Context, revoked []RevokedEntry) ([]byte, error) {
	if s.intermediateKey == nil {
		return nil, fmt.Errorf("intermediate CA not initialized")
	}

	entries := make([]x509.RevocationListEntry, 0, len(revoked))
	for _, r := range revoked {
		n, ok := new(big.Int).SetString(r.Serial, 16)
		if !ok {
			return nil, fmt.Errorf("invalid relay serial %q", r.Serial)
		}
		entries = append(entries, x509.RevocationListEntry{
			SerialNumber:   n,
			RevocationTime: r.RevokedAt.UTC(),
		})
	}

	crlSerial, err := newSerialNumber()
	if err != nil {
		return nil, fmt.Errorf("generate relay CRL serial: %w", err)
	}

	now := time.Now().UTC()
	template := &x509.RevocationList{
		Number:                    crlSerial,
		ThisUpdate:                now,
		NextUpdate:                now.Add(relayCRLValidity),
		RevokedCertificateEntries: entries,
	}

	der, err := x509.CreateRevocationList(rand.Reader, template, s.intermediateKey.cert, s.intermediateKey.privKey)
	if err != nil {
		return nil, fmt.Errorf("create relay CRL: %w", err)
	}
	return der, nil
}
