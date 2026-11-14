package middleware

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

type fakeConsolePolicy struct {
	required bool
	err      error
}

func (f fakeConsolePolicy) Required(context.Context, string) (bool, error) { return f.required, f.err }

// PENDING-06 M-6: the console-wide MFA requirement.
func TestRequireConsoleMFA(t *testing.T) {
	cases := []struct {
		name     string
		policy   fakeConsolePolicy
		tc       *tenant.TenantContext
		wantCode int
		wantBody string
	}{
		{"not required, no MFA", fakeConsolePolicy{}, &tenant.TenantContext{TenantID: "ws"}, 200, ""},
		{"required, MFA passed", fakeConsolePolicy{required: true},
			&tenant.TenantContext{TenantID: "ws", MFAAt: time.Now().Add(-time.Hour)}, 200, ""},
		{"required, no MFA", fakeConsolePolicy{required: true}, &tenant.TenantContext{TenantID: "ws"}, 403, `"code":"MFA_REQUIRED"`},
		{"policy unavailable fails closed", fakeConsolePolicy{err: errors.New("db down")},
			&tenant.TenantContext{TenantID: "ws", MFAAt: time.Now()}, 503, `"code":"UNAVAILABLE"`},
		{"no tenant context", fakeConsolePolicy{}, nil, 401, `"code":"UNAUTHORIZED"`},
	}
	for _, c := range cases {
		reached := false
		h := RequireConsoleMFA(c.policy)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			reached = true
			w.WriteHeader(200)
		}))
		req := httptest.NewRequest(http.MethodPost, "/graphql", nil)
		if c.tc != nil {
			req = req.WithContext(tenant.Set(req.Context(), *c.tc))
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.wantCode {
			t.Errorf("%s: status %d, want %d", c.name, rec.Code, c.wantCode)
		}
		if reached != (c.wantCode == 200) {
			t.Errorf("%s: handler reached = %v", c.name, reached)
		}
		if c.wantBody != "" && !strings.Contains(rec.Body.String(), c.wantBody) {
			t.Errorf("%s: body %q lacks %q", c.name, rec.Body.String(), c.wantBody)
		}
	}
}

// AuthMiddleware carries the token's sid and mfa_at into the TenantContext.
func TestAuthMiddleware_CarriesSessionAndMFA(t *testing.T) {
	const secret = "test-secret-test-secret-test-secret"
	at := time.Now().Add(-time.Minute).Truncate(time.Second)
	var got tenant.TenantContext
	h := AuthMiddleware(secret)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got, _ = tenant.Get(r.Context())
	}))
	for _, mfaAt := range []int64{0, at.Unix()} {
		tok := signTestToken(t, secret, Claims{TenantID: "ws", Role: "admin", Email: "a@example.com", SessionID: "sid-1", MFAAt: mfaAt})
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if got.SessionID != "sid-1" {
			t.Fatalf("session id = %q", got.SessionID)
		}
		if mfaAt == 0 && !got.MFAAt.IsZero() {
			t.Fatalf("mfa_at absent but MFAAt = %v", got.MFAAt)
		}
		if mfaAt != 0 && !got.MFAAt.Equal(at) {
			t.Fatalf("MFAAt = %v, want %v", got.MFAAt, at)
		}
	}
}

func signTestToken(t *testing.T, secret string, c Claims) string {
	t.Helper()
	c.RegisteredClaims = jwt.RegisteredClaims{
		Subject:   "user-1",
		Issuer:    appmeta.ControllerIssuer,
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
	}
	tok, err := jwt.NewWithClaims(jwt.SigningMethodHS256, c).SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return tok
}
