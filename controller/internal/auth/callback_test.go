package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
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

func TestCallbackHandler_TokenExchangeFails(t *testing.T) {
	rc, _ := newTestValkey(t)
	svc := &serviceImpl{cfg: testConfig(), redisClient: rc}
	handler := svc.CallbackHandler()

	// Generate valid state and store a verifier.
	state, _ := generateSignedState(svc.cfg.JWTSecret)
	if err := rc.SetPKCEState(context.Background(), state, "test-verifier", nil); err != nil {
		t.Fatalf("SetPKCEState: %v", err)
	}

	// The token exchange will fail because Google's endpoint rejects our fake code.
	req := httptest.NewRequest(http.MethodGet, "/auth/callback?code=fake-code&state="+state, nil)
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)

	loc := w.Header().Get("Location")
	if loc != svc.cfg.AllowedOrigin+"/login?error=token_exchange_failed" {
		t.Fatalf("expected redirect to %s/login?error=token_exchange_failed, got %s", svc.cfg.AllowedOrigin, loc)
	}

	// Verifier should have been consumed (single-use) even though exchange failed.
	_, _, found, _ := rc.GetAndDeletePKCEState(context.Background(), state)
	if found {
		t.Fatal("PKCE verifier should have been consumed before exchange")
	}
}
