package auth

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yourorg/ztna/controller/internal/auth/providers"
	"github.com/yourorg/ztna/controller/internal/idp"
)

func TestCallbackHandler_MissingParams(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	handler := svc.CallbackHandler()

	tests := []struct {
		name  string
		query string
	}{
		{"no params", ""},
		{"missing code", "?state=abc"},
		{"missing state", "?code=abc"},
		{"empty code", "?code=&state=abc"},
		{"empty state", "?code=abc&state="},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/auth/callback"+tt.query, nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, req)

			if w.Code != http.StatusFound {
				t.Fatalf("expected 302, got %d", w.Code)
			}
			loc := w.Header().Get("Location")
			if loc != svc.cfg.AllowedOrigin+"/login?error=missing_params" {
				t.Fatalf("expected redirect to %s/login?error=missing_params, got %s", svc.cfg.AllowedOrigin, loc)
			}
		})
	}
}

func TestCallbackHandler_InvalidState(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	handler := svc.CallbackHandler()

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state=forged.state", nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	loc := w.Header().Get("Location")
	if loc != svc.cfg.AllowedOrigin+"/login?error=invalid_state" {
		t.Fatalf("expected redirect to %s/login?error=invalid_state, got %s", svc.cfg.AllowedOrigin, loc)
	}
}

func TestCallbackHandler_StateExpired(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	handler := svc.CallbackHandler()

	// Generate a valid state but do NOT store a PKCE verifier in Redis.
	state, err := generateSignedState(svc.cfg.JWTSecret)
	if err != nil {
		t.Fatalf("generateSignedState: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=abc&state="+state, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	loc := w.Header().Get("Location")
	if loc != svc.cfg.AllowedOrigin+"/login?error=state_expired" {
		t.Fatalf("expected redirect to %s/login?error=state_expired, got %s", svc.cfg.AllowedOrigin, loc)
	}
}

// storeState stores a login scratchpad and returns the signed state value.
func storeState(t *testing.T, rc *valkeyClient, secret, connectionID string) string {
	t.Helper()
	state, _ := generateSignedState(secret)
	if err := rc.SetPKCEState(context.Background(), state, PKCEState{
		CodeVerifier: "test-verifier", ConnectionID: connectionID, Nonce: "test-nonce",
	}); err != nil {
		t.Fatalf("SetPKCEState: %v", err)
	}
	return state
}

func runCallback(handler http.Handler, state string) string {
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=fake-code&state="+state, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	return w.Header().Get("Location")
}

// withFakeProvider overrides the adapter-selection seam for one test.
func withFakeProvider(t *testing.T, p *fakeProvider) {
	t.Helper()
	orig := providerForFn
	t.Cleanup(func() { providerForFn = orig })
	providerForFn = func(*idp.Connection, GoogleCreds) (providers.IdentityProvider, error) { return p, nil }
}

func TestCallbackHandler_AuthenticateFails(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc, idpStore: googleConnStore()}
	withFakeProvider(t, &fakeProvider{err: fmt.Errorf("exchange rejected")})

	state := storeState(t, rc, svc.cfg.JWTSecret, "google-conn")
	loc := runCallback(svc.CallbackHandler(), state)

	if loc != svc.cfg.AllowedOrigin+"/login?error=authentication_failed" {
		t.Fatalf("expected authentication_failed redirect, got %s", loc)
	}
	// Scratchpad must be consumed (single-use) even though authentication failed.
	if _, found, _ := rc.GetAndDeletePKCEState(context.Background(), state); found {
		t.Fatal("PKCE state should have been consumed")
	}
}

// TestCallbackHandler_ConnectionDeleted covers the redirect window: the admin
// deletes the IdP connection after InitiateAuth but before the callback returns.
func TestCallbackHandler_ConnectionDeleted(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc, idpStore: &fakeConnStore{}} // empty → GetByID not found

	state := storeState(t, rc, svc.cfg.JWTSecret, "deleted-conn")
	loc := runCallback(svc.CallbackHandler(), state)

	if loc != svc.cfg.AllowedOrigin+"/login?error=authentication_failed" {
		t.Fatalf("deleted connection must fail closed, got %s", loc)
	}
	if _, found, _ := rc.GetAndDeletePKCEState(context.Background(), state); found {
		t.Fatal("PKCE state should have been consumed")
	}
}

// TestCallbackHandler_ConnectionDisabled covers the admin disabling the IdP
// connection during the redirect window.
func TestCallbackHandler_ConnectionDisabled(t *testing.T) {
	rc, _ := newTestValkey(t)
	disabled := &idp.Connection{ID: "c1", Provider: "okta", Status: "disabled", Issuer: "https://acme.okta.com"}
	store := &fakeConnStore{byID: map[string]*idp.Connection{"c1": disabled}}
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc, idpStore: store}

	state := storeState(t, rc, svc.cfg.JWTSecret, "c1")
	loc := runCallback(svc.CallbackHandler(), state)

	if loc != svc.cfg.AllowedOrigin+"/login?error=authentication_failed" {
		t.Fatalf("disabled connection must fail closed, got %s", loc)
	}
}
