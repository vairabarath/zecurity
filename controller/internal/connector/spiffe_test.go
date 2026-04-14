package connector

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"math/big"
	"net/url"
	"testing"

	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// ── parseSPIFFEID tests ─────────────────────────────────────────────────────

func TestParseSPIFFEID_Valid(t *testing.T) {
	cert := certWithURIs(t, "spiffe://ws-acme.zecurity.in/connector/abc-123")

	td, role, id, err := parseSPIFFEID(cert)
	if err != nil {
		t.Fatalf("parseSPIFFEID: %v", err)
	}
	if td != "ws-acme.zecurity.in" {
		t.Fatalf("expected trust domain ws-acme.zecurity.in, got %s", td)
	}
	if role != "connector" {
		t.Fatalf("expected role connector, got %s", role)
	}
	if id != "abc-123" {
		t.Fatalf("expected id abc-123, got %s", id)
	}
}

func TestParseSPIFFEID_NoURIs(t *testing.T) {
	cert := &x509.Certificate{}
	_, _, _, err := parseSPIFFEID(cert)
	if err == nil {
		t.Fatal("expected error for 0 URI SANs")
	}
}

func TestParseSPIFFEID_MultipleURIs(t *testing.T) {
	u1, _ := url.Parse("spiffe://a.example/connector/1")
	u2, _ := url.Parse("spiffe://b.example/connector/2")
	cert := &x509.Certificate{URIs: []*url.URL{u1, u2}}

	_, _, _, err := parseSPIFFEID(cert)
	if err == nil {
		t.Fatal("expected error for 2 URI SANs")
	}
}

func TestParseSPIFFEID_WrongScheme(t *testing.T) {
	cert := certWithURIs(t, "https://ws-acme.zecurity.in/connector/abc")

	_, _, _, err := parseSPIFFEID(cert)
	if err == nil {
		t.Fatal("expected error for non-spiffe scheme")
	}
}

func TestParseSPIFFEID_TooFewSegments(t *testing.T) {
	cert := certWithURIs(t, "spiffe://ws-acme.zecurity.in/connector")

	_, _, _, err := parseSPIFFEID(cert)
	if err == nil {
		t.Fatal("expected error for single path segment")
	}
}

func TestParseSPIFFEID_TooManySegments(t *testing.T) {
	cert := certWithURIs(t, "spiffe://ws-acme.zecurity.in/connector/abc/extra")

	_, _, _, err := parseSPIFFEID(cert)
	if err == nil {
		t.Fatal("expected error for 3 path segments")
	}
}

func TestParseSPIFFEID_EmptyTrustDomain(t *testing.T) {
	// spiffe:///connector/abc — empty host
	u, _ := url.Parse("spiffe:///connector/abc")
	cert := &x509.Certificate{URIs: []*url.URL{u}}

	_, _, _, err := parseSPIFFEID(cert)
	if err == nil {
		t.Fatal("expected error for empty trust domain")
	}
}

// ── Context accessor tests ──────────────────────────────────────────────────

func TestContextAccessors_RoundTrip(t *testing.T) {
	ctx := context.Background()
	ctx = context.WithValue(ctx, spiffeIDKey{}, "spiffe://ws-acme.zecurity.in/connector/abc")
	ctx = context.WithValue(ctx, spiffeRoleKey{}, "connector")
	ctx = context.WithValue(ctx, spiffeEntityIDKey{}, "abc")
	ctx = context.WithValue(ctx, trustDomainKey{}, "ws-acme.zecurity.in")

	if v := SPIFFEIDFromContext(ctx); v != "spiffe://ws-acme.zecurity.in/connector/abc" {
		t.Fatalf("SPIFFEIDFromContext = %q", v)
	}
	if v := SPIFFERoleFromContext(ctx); v != "connector" {
		t.Fatalf("SPIFFERoleFromContext = %q", v)
	}
	if v := SPIFFEEntityIDFromContext(ctx); v != "abc" {
		t.Fatalf("SPIFFEEntityIDFromContext = %q", v)
	}
	if v := TrustDomainFromContext(ctx); v != "ws-acme.zecurity.in" {
		t.Fatalf("TrustDomainFromContext = %q", v)
	}
}

func TestContextAccessors_EmptyContext(t *testing.T) {
	ctx := context.Background()

	if v := SPIFFEIDFromContext(ctx); v != "" {
		t.Fatalf("expected empty, got %q", v)
	}
	if v := SPIFFERoleFromContext(ctx); v != "" {
		t.Fatalf("expected empty, got %q", v)
	}
	if v := SPIFFEEntityIDFromContext(ctx); v != "" {
		t.Fatalf("expected empty, got %q", v)
	}
	if v := TrustDomainFromContext(ctx); v != "" {
		t.Fatalf("expected empty, got %q", v)
	}
}

// ── TrustDomainValidator tests ──────────────────────────────────────────────

type mockWorkspaceStore struct {
	workspaces map[string]*WorkspaceLookup
}

func (m *mockWorkspaceStore) GetByTrustDomain(ctx context.Context, domain string) (*WorkspaceLookup, error) {
	ws, ok := m.workspaces[domain]
	if !ok {
		return nil, nil
	}
	return ws, nil
}

func (m *mockWorkspaceStore) GetWorkspaceCAByTrustDomain(ctx context.Context, domain string) (*x509.Certificate, error) {
	return nil, nil
}

func (m *mockWorkspaceStore) GetIntermediateCA(ctx context.Context) (*x509.Certificate, error) {
	return nil, nil
}

func TestNewTrustDomainValidator_GlobalDomain(t *testing.T) {
	store := &mockWorkspaceStore{workspaces: map[string]*WorkspaceLookup{}}
	v := NewTrustDomainValidator(appmeta.SPIFFEGlobalTrustDomain, store)

	if !v(context.Background(), appmeta.SPIFFEGlobalTrustDomain) {
		t.Fatal("expected global trust domain to be accepted")
	}
}

func TestNewTrustDomainValidator_ActiveWorkspace(t *testing.T) {
	store := &mockWorkspaceStore{
		workspaces: map[string]*WorkspaceLookup{
			"ws-acme.zecurity.in": {ID: "t1", Status: "active"},
		},
	}
	v := NewTrustDomainValidator(appmeta.SPIFFEGlobalTrustDomain, store)

	if !v(context.Background(), "ws-acme.zecurity.in") {
		t.Fatal("expected active workspace trust domain to be accepted")
	}
}

func TestNewTrustDomainValidator_SuspendedWorkspace(t *testing.T) {
	store := &mockWorkspaceStore{
		workspaces: map[string]*WorkspaceLookup{
			"ws-bad.zecurity.in": {ID: "t2", Status: "suspended"},
		},
	}
	v := NewTrustDomainValidator(appmeta.SPIFFEGlobalTrustDomain, store)

	if v(context.Background(), "ws-bad.zecurity.in") {
		t.Fatal("expected suspended workspace trust domain to be rejected")
	}
}

func TestNewTrustDomainValidator_UnknownDomain(t *testing.T) {
	store := &mockWorkspaceStore{workspaces: map[string]*WorkspaceLookup{}}
	v := NewTrustDomainValidator(appmeta.SPIFFEGlobalTrustDomain, store)

	if v(context.Background(), "ws-unknown.evil.com") {
		t.Fatal("expected unknown domain to be rejected")
	}
}

// ── test helpers ────────────────────────────────────────────────────────────

// certWithURIs creates a minimal x509.Certificate with a single URI SAN for testing.
func certWithURIs(t *testing.T, rawURI string) *x509.Certificate {
	t.Helper()
	u, err := url.Parse(rawURI)
	if err != nil {
		t.Fatalf("parse URI %q: %v", rawURI, err)
	}

	// Create a self-signed cert with the URI SAN so the URIs field is populated.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		URIs:         []*url.URL{u},
	}

	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}

	cert, err := x509.ParseCertificate(certDER)
	if err != nil {
		t.Fatalf("parse cert: %v", err)
	}

	return cert
}
