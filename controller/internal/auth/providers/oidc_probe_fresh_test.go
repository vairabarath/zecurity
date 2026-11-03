package providers

// Coverage for the create-time OIDC configuration check used by
// createIdpConnection (graph/resolvers/idp_helpers.go: validateOIDCDiscovery).
//
// The behavior under test is ProbeFresh: it must perform a real network fetch
// and re-run the full discovery validation every time, never answering from the
// process-wide discoveryCache. That distinction is the whole point — the cache
// is keyed on the ISSUER ALONE, so a cache-consulting check would report a
// misconfigured (or since-broken) connection as valid without a request.
//
// These are in-package so the cache can be cleared directly; no production seam
// is added for tests.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// resetDiscoveryCache clears the shared per-issuer discovery cache so a test
// starts from a known-cold state and leaves nothing behind for its neighbors.
func resetDiscoveryCache(t *testing.T) {
	t.Helper()
	discoveryCache.Lock()
	discoveryCache.m = map[string]discoveryEntry{}
	discoveryCache.Unlock()
	t.Cleanup(func() {
		discoveryCache.Lock()
		discoveryCache.m = map[string]discoveryEntry{}
		discoveryCache.Unlock()
	})
}

// discoveryFixture serves a controllable discovery document and counts the
// requests it receives, so a test can prove whether the network was touched.
type discoveryFixture struct {
	server *httptest.Server
	hits   int
	// doc is what the well-known endpoint returns; nil ⇒ a valid document
	// advertising this server as the issuer.
	doc map[string]string
	// status, when non-zero, is returned instead of a document.
	status int
	// raw, when non-empty, is written verbatim (for malformed-JSON cases).
	raw string
}

func newDiscoveryFixture(t *testing.T) *discoveryFixture {
	t.Helper()
	f := &discoveryFixture{}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		f.hits++
		if f.status != 0 {
			http.Error(w, "nope", f.status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if f.raw != "" {
			_, _ = w.Write([]byte(f.raw))
			return
		}
		doc := f.doc
		if doc == nil {
			doc = f.validDoc()
		}
		_ = json.NewEncoder(w).Encode(doc)
	})
	f.server = httptest.NewServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

func (f *discoveryFixture) validDoc() map[string]string {
	return map[string]string{
		"issuer":                 f.server.URL,
		"authorization_endpoint": f.server.URL + "/authorize",
		"token_endpoint":         f.server.URL + "/token",
		"jwks_uri":               f.server.URL + "/jwks",
	}
}

// providerFor builds a discovery-only provider exactly as the create path does:
// with EMPTY client credentials, because discovery needs none.
func (f *discoveryFixture) providerFor(issuer string) *OIDCProvider {
	return NewOIDCProvider("okta", issuer, "", "", "", "")
}

func TestProbeFresh_ValidDiscoveryDocumentSucceeds(t *testing.T) {
	resetDiscoveryCache(t)
	f := newDiscoveryFixture(t)

	issuer, err := f.providerFor(f.server.URL).ProbeFresh(context.Background())
	if err != nil {
		t.Fatalf("ProbeFresh on a valid document: unexpected error: %v", err)
	}
	if issuer != f.server.URL {
		t.Fatalf("expected the advertised issuer %q, got %q", f.server.URL, issuer)
	}
	if f.hits != 1 {
		t.Fatalf("expected exactly 1 discovery request, got %d", f.hits)
	}
}

func TestProbeFresh_UnreachableIssuerFails(t *testing.T) {
	resetDiscoveryCache(t)
	// Reserved-for-invalid TLD: never resolves, so this is a genuine
	// unreachable-host case and not a live network dependency.
	p := NewOIDCProvider("okta", "https://acme-not-a-real-org.invalid", "", "", "", "")
	if _, err := p.ProbeFresh(context.Background()); err == nil {
		t.Fatal("expected ProbeFresh to fail against an unreachable issuer, got nil error")
	}
}

func TestProbeFresh_Non200StatusFails(t *testing.T) {
	resetDiscoveryCache(t)
	f := newDiscoveryFixture(t)
	f.status = http.StatusNotFound

	_, err := f.providerFor(f.server.URL).ProbeFresh(context.Background())
	if err == nil {
		t.Fatal("expected ProbeFresh to fail on a non-200 discovery response")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected the status in the error, got %q", err.Error())
	}
}

func TestProbeFresh_MalformedDocumentFails(t *testing.T) {
	resetDiscoveryCache(t)
	f := newDiscoveryFixture(t)
	f.raw = "{ this is not json"

	if _, err := f.providerFor(f.server.URL).ProbeFresh(context.Background()); err == nil {
		t.Fatal("expected ProbeFresh to fail on a malformed discovery document")
	}
}

func TestProbeFresh_IssuerMismatchFails(t *testing.T) {
	resetDiscoveryCache(t)
	f := newDiscoveryFixture(t)
	// A document that advertises a DIFFERENT issuer than the one configured —
	// the OIDC-spec mismatch the discovery path must reject.
	f.doc = map[string]string{
		"issuer":                 "https://attacker.example.com",
		"authorization_endpoint": f.server.URL + "/authorize",
		"token_endpoint":         f.server.URL + "/token",
		"jwks_uri":               f.server.URL + "/jwks",
	}

	_, err := f.providerFor(f.server.URL).ProbeFresh(context.Background())
	if err == nil {
		t.Fatal("expected ProbeFresh to reject a discovery issuer mismatch")
	}
	if !strings.Contains(err.Error(), "issuer mismatch") {
		t.Fatalf("expected an issuer-mismatch error, got %q", err.Error())
	}
}

func TestProbeFresh_MissingRequiredEndpointsFails(t *testing.T) {
	resetDiscoveryCache(t)
	f := newDiscoveryFixture(t)
	// Well-formed JSON, matching issuer, but no token/JWKS endpoints.
	f.doc = map[string]string{
		"issuer":                 f.server.URL,
		"authorization_endpoint": f.server.URL + "/authorize",
	}

	_, err := f.providerFor(f.server.URL).ProbeFresh(context.Background())
	if err == nil {
		t.Fatal("expected ProbeFresh to reject a document missing required endpoints")
	}
	if !strings.Contains(err.Error(), "missing required endpoints") {
		t.Fatalf("expected a missing-endpoints error, got %q", err.Error())
	}
}

// The core cache test: a WARM cache entry for an issuer must not let a now-broken
// configuration pass. This is what makes the create-time check a real check
// rather than a lookup of someone else's earlier success.
func TestProbeFresh_IgnoresWarmCacheAndStillDetectsFailure(t *testing.T) {
	resetDiscoveryCache(t)
	f := newDiscoveryFixture(t)
	p := f.providerFor(f.server.URL)

	// 1. ProbeFresh is cache-NEUTRAL: a successful probe must not populate the
	//    cache the login path reads, so an admin action cannot seed or refresh
	//    it.
	if _, err := p.ProbeFresh(context.Background()); err != nil {
		t.Fatalf("priming probe: %v", err)
	}
	if _, ok := discoveryCache.get(f.server.URL); ok {
		t.Fatal("ProbeFresh must not write to the discovery cache")
	}

	// 2. Warm the cache through the cache-consulting path (what a login does).
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatalf("warming Probe: %v", err)
	}
	if _, ok := discoveryCache.get(f.server.URL); !ok {
		t.Fatal("expected the cache-consulting Probe to populate the discovery cache")
	}

	// 3. Confirm the cache really is warm: the cache-consulting path answers
	//    with NO further network request. This is the vacuous "success" the
	//    create path must not rely on.
	hitsBefore := f.hits
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatalf("cache-consulting Probe on a warm cache: %v", err)
	}
	if f.hits != hitsBefore {
		t.Fatalf("expected Probe to answer from cache without a request, got %d new hits", f.hits-hitsBefore)
	}

	// 4. Break the issuer, then prove ProbeFresh still fails despite the warm
	//    (successful) cache entry.
	f.status = http.StatusInternalServerError
	if _, err := p.ProbeFresh(context.Background()); err == nil {
		t.Fatal("STALE-SUCCESS REGRESSION: ProbeFresh returned success from a warm cache entry " +
			"while the issuer was failing; the create-time check would pass a broken configuration")
	}
	if f.hits != hitsBefore+1 {
		t.Fatalf("expected ProbeFresh to make exactly 1 new request, got %d", f.hits-hitsBefore)
	}

	// 5. And the cache-consulting path, by contrast, still reports the stale
	//    success — documenting exactly why ProbeFresh exists.
	if _, err := p.Probe(context.Background()); err != nil {
		t.Fatalf("expected the cached document to still satisfy Probe, got %v", err)
	}
}

// A warm cache entry for the issuer must not mask a bogus explicit discoveryURL
// override: the cache is keyed on the issuer alone, while the fetch prefers the
// override, so only a fresh fetch exercises what the operator actually typed.
func TestProbeFresh_FetchesExplicitDiscoveryURLOverrideDespiteWarmCache(t *testing.T) {
	resetDiscoveryCache(t)
	f := newDiscoveryFixture(t)

	// Warm the cache for this issuer via the cache-populating path.
	if _, err := f.providerFor(f.server.URL).Probe(context.Background()); err != nil {
		t.Fatalf("priming probe: %v", err)
	}
	if _, ok := discoveryCache.get(f.server.URL); !ok {
		t.Fatal("expected a warm cache entry for the issuer")
	}

	// Same issuer, but an unreachable discoveryURL override.
	withOverride := NewOIDCProvider("okta", f.server.URL, "", "",
		"https://acme-not-a-real-org.invalid/.well-known/openid-configuration", "")
	if _, err := withOverride.ProbeFresh(context.Background()); err == nil {
		t.Fatal("expected ProbeFresh to fetch (and fail on) the unreachable discoveryURL override " +
			"instead of answering from the issuer-keyed cache")
	}
}

// The credential-safety invariant, asserted structurally: the discovery request
// carries no client secret, no client id, and no Authorization header, because
// the create path constructs the provider with empty credentials and discovery
// is unauthenticated. This is what makes "the secret cannot leak here" true by
// construction rather than by a logging convention.
func TestProbeFresh_SendsNoCredentialMaterial(t *testing.T) {
	resetDiscoveryCache(t)

	const secret = "super-secret-value-must-never-appear"
	var gotAuth string
	var gotQuery string
	var gotBody []byte

	f := newDiscoveryFixture(t)
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotQuery = r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"issuer":                 f.server.URL,
			"authorization_endpoint": f.server.URL + "/authorize",
			"token_endpoint":         f.server.URL + "/token",
			"jwks_uri":               f.server.URL + "/jwks",
		})
	})
	f.server.Config.Handler = mux

	// Even if a caller wrongly passed real credentials, discovery must not send
	// them — so construct WITH a secret and assert none of it reaches the wire.
	p := NewOIDCProvider("okta", f.server.URL, "client-abc", secret, "", "")
	if _, err := p.ProbeFresh(context.Background()); err != nil {
		t.Fatalf("ProbeFresh: %v", err)
	}

	if gotAuth != "" {
		t.Fatalf("discovery request carried an Authorization header: %q", gotAuth)
	}
	if strings.Contains(gotQuery, secret) || strings.Contains(gotQuery, "client-abc") {
		t.Fatalf("discovery request query carried credential material: %q", gotQuery)
	}
	if strings.Contains(string(gotBody), secret) {
		t.Fatal("discovery request body carried the client secret")
	}
}
