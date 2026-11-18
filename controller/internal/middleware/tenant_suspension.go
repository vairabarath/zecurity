package middleware

import (
	"fmt"
	"log"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/tenant"
	"github.com/yourorg/ztna/controller/internal/tenantstate"
)

// TenantEligibilityGuard refuses every tenant console request (GraphQL and
// REST) while the tenant is suspended by the provider (Sprint 23 S1, D-45).
//
// It sits next to WorkspaceGuard, never inside it: WorkspaceGuard's
// status='active' query is a protected predicate (D-34) and stays unchanged;
// suspension is read only through internal/tenantstate.
//
// A suspended tenant's admin sessions are revoked at suspension and refresh is
// refused, but an access token issued before it stays valid for its short
// lifetime (≤15 m, no per-request revocation, see Claims.IdentityGeneration).
// This guard is what refuses that token's requests at once, mutations and
// reads alike. A lookup error refuses (fail closed).
//
// Runs after AuthMiddleware (requires TenantContext).
func TenantEligibilityGuard(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tc, ok := tenant.Get(r.Context())
			if !ok {
				writeJSON403(w, "no tenant context")
				return
			}
			if err := tenantstate.Check(r.Context(), pool, tc.TenantID); err != nil {
				if tenantstate.IsSuspended(err) {
					writeTenantSuspended(w)
					return
				}
				log.Printf("tenant_eligibility_guard: workspace=%s: %v", tc.TenantID, err)
				writeJSON403(w, "workspace inactive")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeTenantSuspended writes the suspended refusal: 403 with the
// TENANT_SUSPENDED code, so the admin UI shows the provider-suspension
// message rather than a generic error.
func writeTenantSuspended(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w,
		`{"errors":[{"message":%q,"extensions":{"code":%q}}]}`, tenantstate.Message, tenantstate.Code)
}
