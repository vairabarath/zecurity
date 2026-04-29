package middleware

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/yourorg/ztna/controller/internal/tenant"
)

// RequireRole returns a middleware that allows only callers whose JWT role
// matches one of the supplied roles. Must run after AuthMiddleware (which
// injects the TenantContext). Returns 403 for wrong role, 401 if context
// has no identity at all.
func RequireRole(roles ...string) func(http.Handler) http.Handler {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[strings.ToLower(r)] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tc, ok := tenant.Get(r.Context())
			if !ok {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthenticated"}) //nolint:errcheck
				return
			}
			if !allowed[strings.ToLower(tc.Role)] {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusForbidden)
				json.NewEncoder(w).Encode(map[string]string{"error": "forbidden"}) //nolint:errcheck
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
