package pki

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"math/big"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func testCertPEM(t *testing.T) (string, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "fingerprint test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), der
}

type fakeRow struct {
	val string
	err error
}

func (r fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	*dest[0].(*string) = r.val
	return nil
}

type fakeQuerier struct {
	row fakeRow
	sql string
}

func (q *fakeQuerier) QueryRow(_ context.Context, sql string, _ ...any) pgx.Row {
	q.sql = sql
	return q.row
}

func TestFingerprintPEM_IsSHA256OfDER(t *testing.T) {
	certPEM, der := testCertPEM(t)
	sum := sha256.Sum256(der)
	got, err := FingerprintPEM(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint %s, want %s", got, hex.EncodeToString(sum[:]))
	}
}

func TestFingerprintPEM_RejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "not pem", "-----BEGIN CERTIFICATE-----\n-----END CERTIFICATE-----\n"} {
		if fp, err := FingerprintPEM(in); err == nil {
			t.Errorf("FingerprintPEM(%q) = %q, want error", in, fp)
		}
	}
}

func TestIntermediateFingerprint_ReadsTheServedCertificate(t *testing.T) {
	certPEM, der := testCertPEM(t)
	sum := sha256.Sum256(der)
	q := &fakeQuerier{row: fakeRow{val: certPEM}}
	got, err := IntermediateFingerprint(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	if got != hex.EncodeToString(sum[:]) {
		t.Fatalf("fingerprint %s, want %s", got, hex.EncodeToString(sum[:]))
	}
	// Same row /ca.crt serves (connector.CAEndpointHandler).
	if q.sql != `SELECT certificate_pem FROM ca_intermediate LIMIT 1` {
		t.Fatalf("query %q", q.sql)
	}
}

func TestIntermediateFingerprint_PropagatesErrors(t *testing.T) {
	q := &fakeQuerier{row: fakeRow{err: errors.New("boom")}}
	if _, err := IntermediateFingerprint(context.Background(), q); err == nil {
		t.Fatal("want error when the CA row can't be read")
	}
}
