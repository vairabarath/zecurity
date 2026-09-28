package middleware

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

// Provider console CORS (Sprint 21 Phase H-b; D-18).
//
// The provider console is a separate app on its own origin, so browsers only
// let it call /provider/* when the controller explicitly allows that one
// origin. Scope is deliberately narrow:
//   - only request paths under /provider/;
//   - only the exact configured origin (never "*");
//   - no credentials — the console sends its bearer token in Authorization;
//   - tenant routes (/graphql, /auth/*, …) never get CORS headers.
//
// It wraps the whole mux rather than individual routes because provider routes
// use method patterns ("POST /provider/auth/login"): a browser preflight
// (OPTIONS) would match none of them and the mux would answer 405.

const (
	providerPathPrefix       = "/provider/"
	providerCORSAllowMethods = "GET, POST, PATCH, OPTIONS"
	providerCORSAllowHeaders = "Authorization, Content-Type"
	providerCORSMaxAgeSecs   = "600"
)

// NewProviderCORS returns the CORS wrapper for PROVIDER_CONSOLE_ORIGIN. An empty
// origin disables CORS entirely (development uses the Vite proxy, so the
// console is same-origin). A malformed origin is an error: the caller refuses
// to start instead of silently never matching.
func NewProviderCORS(origin string) (func(http.Handler) http.Handler, error) {
	if origin == "" {
		return func(next http.Handler) http.Handler { return next }, nil
	}
	if err := validateOrigin(origin); err != nil {
		return nil, fmt.Errorf("PROVIDER_CONSOLE_ORIGIN %q: %w", origin, err)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !strings.HasPrefix(r.URL.Path, providerPathPrefix) {
				next.ServeHTTP(w, r)
				return
			}
			// The response depends on the Origin header; caches must not mix them.
			w.Header().Add("Vary", "Origin")
			allowed := r.Header.Get("Origin") == origin

			if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
				// Preflight: answered here, never passed on (it carries no token,
				// so RequireProvider would reject it, and the mux has no OPTIONS
				// routes).
				if !allowed {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				h := w.Header()
				h.Set("Access-Control-Allow-Origin", origin)
				h.Set("Access-Control-Allow-Methods", providerCORSAllowMethods)
				h.Set("Access-Control-Allow-Headers", providerCORSAllowHeaders)
				h.Set("Access-Control-Max-Age", providerCORSMaxAgeSecs)
				w.WriteHeader(http.StatusNoContent)
				return
			}

			if allowed {
				w.Header().Set("Access-Control-Allow-Origin", origin)
			}
			// A foreign origin still reaches the handler (and its auth), but gets
			// no CORS headers, so the browser withholds the response.
			next.ServeHTTP(w, r)
		})
	}, nil
}

// validateOrigin accepts exactly scheme://host[:port] with http or https: the
// form browsers send in the Origin header (no path, query, fragment, userinfo
// or trailing slash).
func validateOrigin(origin string) error {
	if origin == "*" || strings.Contains(origin, "*") {
		return fmt.Errorf("wildcards are not allowed")
	}
	u, err := url.Parse(origin)
	if err != nil {
		return err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("scheme must be http or https")
	}
	if u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("must be exactly scheme://host[:port]")
	}
	return nil
}
