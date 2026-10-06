package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

const testConsoleOrigin = "https://provider.zecurity.test"

// corsServe runs a request through the provider CORS wrapper around a handler
// that records whether it was reached.
func corsServe(t *testing.T, origin, method, path string, headers map[string]string) (*httptest.ResponseRecorder, bool) {
	t.Helper()
	wrap, err := NewProviderCORS(origin)
	if err != nil {
		t.Fatal(err)
	}
	reached := false
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(http.StatusOK)
	})
	req := httptest.NewRequest(method, path, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	wrap(next).ServeHTTP(rec, req)
	return rec, reached
}

func preflightHeaders(origin string) map[string]string {
	return map[string]string{
		"Origin":                         origin,
		"Access-Control-Request-Method":  "POST",
		"Access-Control-Request-Headers": "authorization, content-type",
	}
}

func TestProviderCORS_ConsoleOriginPreflight(t *testing.T) {
	rec, reached := corsServe(t, testConsoleOrigin, http.MethodOptions, "/provider/auth/login", preflightHeaders(testConsoleOrigin))
	if rec.Code != http.StatusNoContent || reached {
		t.Fatalf("preflight: code=%d reached=%v, want 204 answered by the wrapper", rec.Code, reached)
	}
	h := rec.Header()
	if h.Get("Access-Control-Allow-Origin") != testConsoleOrigin ||
		h.Get("Access-Control-Allow-Methods") != providerCORSAllowMethods ||
		h.Get("Access-Control-Allow-Headers") != providerCORSAllowHeaders {
		t.Fatalf("preflight headers: %v", h)
	}
	if h.Get("Access-Control-Allow-Credentials") != "" {
		t.Fatal("Allow-Credentials must not be sent (bearer tokens only)")
	}
}

func TestProviderCORS_ConsoleOriginRequest(t *testing.T) {
	rec, reached := corsServe(t, testConsoleOrigin, http.MethodPost, "/provider/auth/login", map[string]string{"Origin": testConsoleOrigin})
	if !reached || rec.Header().Get("Access-Control-Allow-Origin") != testConsoleOrigin {
		t.Fatalf("reached=%v ACAO=%q", reached, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	if rec.Header().Get("Vary") != "Origin" {
		t.Fatalf("Vary = %q, want Origin", rec.Header().Get("Vary"))
	}
}

// A foreign origin gets no CORS headers: its preflight is refused, and its
// simple requests still reach the handler (and its auth) but the browser
// withholds the response.
func TestProviderCORS_ForeignOriginGetsNothing(t *testing.T) {
	const evil = "https://evil.example"
	rec, reached := corsServe(t, testConsoleOrigin, http.MethodOptions, "/provider/auth/login", preflightHeaders(evil))
	if rec.Code != http.StatusForbidden || reached || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign preflight: code=%d reached=%v ACAO=%q", rec.Code, reached, rec.Header().Get("Access-Control-Allow-Origin"))
	}
	rec, reached = corsServe(t, testConsoleOrigin, http.MethodGet, "/provider/me", map[string]string{"Origin": evil})
	if !reached || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("foreign request: reached=%v ACAO=%q", reached, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

// Tenant routes are untouched, even when called from the console origin.
func TestProviderCORS_TenantRoutesUntouched(t *testing.T) {
	for _, path := range []string{"/graphql", "/auth/refresh", "/auth/callback", "/api/invitations", "/providerx"} {
		rec, reached := corsServe(t, testConsoleOrigin, http.MethodOptions, path, preflightHeaders(testConsoleOrigin))
		if !reached || rec.Header().Get("Access-Control-Allow-Origin") != "" || rec.Header().Get("Vary") != "" {
			t.Errorf("%s: reached=%v headers=%v (tenant routes must pass through untouched)", path, reached, rec.Header())
		}
	}
}

// With PROVIDER_CONSOLE_ORIGIN unset, nothing gets CORS headers and requests
// pass straight through (dev uses the Vite proxy: same origin).
func TestProviderCORS_DisabledWhenUnset(t *testing.T) {
	rec, reached := corsServe(t, "", http.MethodOptions, "/provider/auth/login", preflightHeaders(testConsoleOrigin))
	if !reached || rec.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("unset origin: reached=%v ACAO=%q", reached, rec.Header().Get("Access-Control-Allow-Origin"))
	}
}

func TestNewProviderCORS_RejectsMalformedOrigins(t *testing.T) {
	for _, bad := range []string{
		"*",
		"https://*.zecurity.test",
		"provider.zecurity.test",           // no scheme
		"ftp://provider.zecurity.test",     // wrong scheme
		"https://provider.zecurity.test/",  // trailing slash never matches a browser Origin
		"https://provider.zecurity.test/x", // path
		"https://user@provider.zecurity.test",
		"https://provider.zecurity.test?q=1",
	} {
		if _, err := NewProviderCORS(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, good := range []string{testConsoleOrigin, "http://localhost:5174", "https://10.0.0.5:8443"} {
		if _, err := NewProviderCORS(good); err != nil {
			t.Errorf("%q rejected: %v", good, err)
		}
	}
}
