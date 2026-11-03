package pki

import (
	"context"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"testing"
	"time"
)

// newTestIntermediateService builds an in-memory, self-signed Intermediate CA
// (CertSign|CRLSign) and returns a serviceImpl wired with it plus the CA cert.
// GenerateRelayCRL is a pure function of s.intermediateKey, so no DB is needed.
func newTestIntermediateService(t *testing.T) (*serviceImpl, *x509.Certificate) {
	t.Helper()

	privKey, err := generateECKeyPair()
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	serial, err := newSerialNumber()
	if err != nil {
		t.Fatalf("serial: %v", err)
	}
	notBefore, notAfter := certValidity(5)
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "test-intermediate-ca"},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		MaxPathLen:            1,
	}
	// Self-signed: template is its own parent.
	certDER, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &privKey.PublicKey, privKey)
	if err != nil {
		t.Fatalf("create intermediate cert: %v", err)
	}
	caCert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parse intermediate cert: %v", err)
	}
	return &serviceImpl{intermediateKey: &intermediateCAState{cert: caCert, privKey: privKey}}, caCert
}

// TestGenerateRelayCRL_SignsVerifiesAndListsSerials is a DB-free end-to-end check
// of the relay CRL producer: it signs with the Intermediate CA, parses back,
// verifies the signature against that CA, lists exactly the supplied serials, and
// sets a nextUpdate comfortably beyond the 60s consumer refresh interval.
func TestGenerateRelayCRL_SignsVerifiesAndListsSerials(t *testing.T) {
	svc, caCert := newTestIntermediateService(t)
	ctx := context.Background()

	// Serials in canonical SerialNumber.Text(16) form, as relay_certificates.serial stores them.
	want := map[string]*big.Int{
		"1a2b3c":           big.NewInt(0x1a2b3c),
		"deadbeef":         big.NewInt(0xdeadbeef),
		"7fffffffffffffff": big.NewInt(0x7fffffffffffffff),
	}

	cases := []struct {
		name    string
		serials []string
	}{
		{"zero revoked", nil},
		{"one revoked", []string{"1a2b3c"}},
		{"many revoked", []string{"1a2b3c", "deadbeef", "7fffffffffffffff"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var revoked []RevokedEntry
			for _, s := range tc.serials {
				revoked = append(revoked, RevokedEntry{Serial: s, RevokedAt: time.Now().Add(-time.Minute)})
			}

			before := time.Now()
			der, err := svc.GenerateRelayCRL(ctx, revoked)
			if err != nil {
				t.Fatalf("GenerateRelayCRL: %v", err)
			}

			crl, err := x509.ParseRevocationList(der)
			if err != nil {
				t.Fatalf("ParseRevocationList: %v", err)
			}

			// Signed by the Intermediate CA.
			if err := crl.CheckSignatureFrom(caCert); err != nil {
				t.Fatalf("CRL not signed by Intermediate CA: %v", err)
			}

			// Application/pkix-crl content is parseable (above) and the entry set matches exactly.
			if got := len(crl.RevokedCertificateEntries); got != len(tc.serials) {
				t.Fatalf("entry count: got %d, want %d", got, len(tc.serials))
			}
			gotSet := map[string]bool{}
			for _, e := range crl.RevokedCertificateEntries {
				gotSet[e.SerialNumber.Text(16)] = true
			}
			for _, s := range tc.serials {
				if !gotSet[s] {
					t.Errorf("serial %q missing from CRL", s)
				}
				// And the parsed big.Int round-trips to the expected value.
				for _, e := range crl.RevokedCertificateEntries {
					if e.SerialNumber.Text(16) == s && e.SerialNumber.Cmp(want[s]) != 0 {
						t.Errorf("serial %q parsed as %s, want %s", s, e.SerialNumber, want[s])
					}
				}
			}

			// nextUpdate must beat the 60s consumer refresh so a fresh CRL is never already stale.
			if !crl.NextUpdate.After(before.Add(60 * time.Second)) {
				t.Errorf("NextUpdate %v is not > now+60s", crl.NextUpdate)
			}
			if !crl.NextUpdate.After(crl.ThisUpdate) {
				t.Errorf("NextUpdate %v not after ThisUpdate %v", crl.NextUpdate, crl.ThisUpdate)
			}
		})
	}
}

// TestGenerateRelayCRL_FailsClosedWithoutIntermediate asserts the guard: no
// in-memory Intermediate CA => error, never an unsigned/empty CRL.
func TestGenerateRelayCRL_FailsClosedWithoutIntermediate(t *testing.T) {
	svc := &serviceImpl{} // intermediateKey nil
	if _, err := svc.GenerateRelayCRL(context.Background(), nil); err == nil {
		t.Fatal("expected error when intermediate CA is not initialized, got nil")
	}
}

// TestGenerateRelayCRL_RejectsInvalidSerial asserts a non-hex serial is rejected
// rather than silently dropped from the CRL.
func TestGenerateRelayCRL_RejectsInvalidSerial(t *testing.T) {
	svc, _ := newTestIntermediateService(t)
	_, err := svc.GenerateRelayCRL(context.Background(), []RevokedEntry{
		{Serial: "not-hex-zz", RevokedAt: time.Now()},
	})
	if err == nil {
		t.Fatal("expected error for invalid serial, got nil")
	}
}
