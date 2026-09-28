package provider_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/middleware"
	"github.com/yourorg/ztna/controller/internal/provider"
)

// End-to-end provider identity flows over the real store (throwaway DB),
// middleware and handlers, wired like cmd/server/main.go.

// allowAllLimiter never blocks; rate limiting has its own tests.
type allowAllLimiter struct{}

func (allowAllLimiter) Blocked(context.Context, string, string) (bool, time.Duration, error) {
	return false, 0, nil
}
func (allowAllLimiter) RecordFailure(context.Context, string, string) ([]provider.LimitTrip, error) {
	return nil, nil
}
func (allowAllLimiter) Reset(context.Context, string) error { return nil }

const (
	e2eKey       = "e2e-provider-signing-key-0123456789abcdef"
	e2eEmail     = "boot@inkyank.com"
	e2eTempPass  = "temporary-pass-1"
	e2eFinalPass = "my-real-password-1"
)

func newE2EServer(t *testing.T) *httptest.Server {
	t.Helper()
	store, _ := provider.NewTestStore(t)
	ctx := context.Background()
	if res, _, err := provider.Bootstrap(ctx, store, e2eEmail, e2eTempPass); err != nil || res != provider.BootstrapCreated {
		t.Fatalf("bootstrap: res=%v err=%v", res, err)
	}
	ids, err := provider.NewIdentityService(e2eKey)
	if err != nil {
		t.Fatal(err)
	}
	auth := provider.NewAuthHandlers(ids, provider.NewLocalPasswordAuthenticator(store), store, allowAllLimiter{})
	handlers := provider.NewHandlers(store, provider.NewAuthz())
	require := middleware.RequireProvider(ids, store)

	mux := http.NewServeMux()
	mux.Handle("POST /provider/auth/login", http.HandlerFunc(auth.Login))
	mux.Handle("POST /provider/auth/password",
		middleware.RequireProvider(ids, store, middleware.AllowPasswordChangeToken())(http.HandlerFunc(auth.ChangePassword)))
	mux.Handle("POST /provider/auth/logout", require(http.HandlerFunc(auth.Logout)))
	mux.Handle("GET /provider/me", require(http.HandlerFunc(handlers.Me)))
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

type session struct {
	Token                  string `json:"token"`
	PasswordChangeRequired bool   `json:"password_change_required"`
}

func call(t *testing.T, srv *httptest.Server, method, path, token string, body any) (int, string) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, _ := http.NewRequest(method, srv.URL+path, rdr)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}

func login(t *testing.T, srv *httptest.Server, password string) session {
	t.Helper()
	code, body := call(t, srv, http.MethodPost, "/provider/auth/login", "", map[string]string{"email": e2eEmail, "password": password})
	if code != http.StatusOK {
		t.Fatalf("login: %d %s", code, body)
	}
	var s session
	_ = json.Unmarshal([]byte(body), &s)
	return s
}

func changePassword(t *testing.T, srv *httptest.Server, token, current, next string) session {
	t.Helper()
	code, body := call(t, srv, http.MethodPost, "/provider/auth/password", token,
		map[string]string{"current_password": current, "new_password": next})
	if code != http.StatusOK {
		t.Fatalf("change password: %d %s", code, body)
	}
	var s session
	_ = json.Unmarshal([]byte(body), &s)
	return s
}

// Bootstrap → forced change → full session → logout, end to end.
func TestE2E_BootstrapLoginChangeLogout(t *testing.T) {
	srv := newE2EServer(t)

	first := login(t, srv, e2eTempPass)
	if !first.PasswordChangeRequired {
		t.Fatal("bootstrap login must require a password change")
	}
	if code, body := call(t, srv, http.MethodGet, "/provider/me", first.Token, nil); code != http.StatusForbidden || !strings.Contains(body, "password_change_required") {
		t.Fatalf("pwc token on /provider/me: %d %s", code, body)
	}

	full := changePassword(t, srv, first.Token, e2eTempPass, e2eFinalPass)
	if full.PasswordChangeRequired || full.Token == "" {
		t.Fatalf("after change: %+v", full)
	}
	if code, _ := call(t, srv, http.MethodPost, "/provider/auth/password", first.Token,
		map[string]string{"current_password": e2eFinalPass, "new_password": "yet-another-pass-1"}); code != http.StatusUnauthorized {
		t.Fatalf("pwc token after the change must be revoked: %d", code)
	}
	if code, body := call(t, srv, http.MethodGet, "/provider/me", full.Token, nil); code != http.StatusOK || !strings.Contains(body, e2eEmail) {
		t.Fatalf("/provider/me with full token: %d %s", code, body)
	}

	// The temporary password no longer works; the new one does.
	if code, _ := call(t, srv, http.MethodPost, "/provider/auth/login", "", map[string]string{"email": e2eEmail, "password": e2eTempPass}); code != http.StatusUnauthorized {
		t.Fatalf("temporary password still accepted: %d", code)
	}
	if again := login(t, srv, e2eFinalPass); again.PasswordChangeRequired {
		t.Fatal("password change flag not cleared")
	}
}

// AT-CORE-2: logout revokes EVERY outstanding token, not just the caller's.
func TestLogout_RevokesAllTokens(t *testing.T) {
	srv := newE2EServer(t)
	changePassword(t, srv, login(t, srv, e2eTempPass).Token, e2eTempPass, e2eFinalPass)
	a := login(t, srv, e2eFinalPass).Token
	b := login(t, srv, e2eFinalPass).Token

	if code, _ := call(t, srv, http.MethodPost, "/provider/auth/logout", a, nil); code != http.StatusNoContent {
		t.Fatalf("logout: %d", code)
	}
	for name, tok := range map[string]string{"caller": a, "other session": b} {
		if code, body := call(t, srv, http.MethodGet, "/provider/me", tok, nil); code != http.StatusUnauthorized || !strings.Contains(body, "revoked") {
			t.Errorf("%s token after logout: %d %s", name, code, body)
		}
	}
}

// AT-CORE-2: changing the password invalidates every existing session
// immediately; only the token returned by the change works.
func TestPasswordChange_InvalidatesAllSessions(t *testing.T) {
	srv := newE2EServer(t)
	changePassword(t, srv, login(t, srv, e2eTempPass).Token, e2eTempPass, e2eFinalPass)
	a := login(t, srv, e2eFinalPass).Token
	b := login(t, srv, e2eFinalPass).Token

	fresh := changePassword(t, srv, a, e2eFinalPass, "rotated-password-1").Token
	for name, tok := range map[string]string{"caller": a, "other session": b} {
		if code, _ := call(t, srv, http.MethodGet, "/provider/me", tok, nil); code != http.StatusUnauthorized {
			t.Errorf("%s token after password change: %d, want 401", name, code)
		}
	}
	if code, _ := call(t, srv, http.MethodGet, "/provider/me", fresh, nil); code != http.StatusOK {
		t.Fatalf("token returned by the change: %d, want 200", code)
	}
}
