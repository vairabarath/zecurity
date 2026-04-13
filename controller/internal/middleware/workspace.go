package middleware

import (
	"fmt"
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// WorkspaceGuard checks workspace status = 'active' before
// allowing the request through.
//
// Runs after AuthMiddleware (requires TenantContext).
// Runs before any GraphQL resolver.
//
// 'provisioning' → bootstrap transaction did not complete
// 'suspended'    → admin disabled the workspace
// 'deleted'      → workspace is gone
// All non-active states → 403, request stops
func WorkspaceGuard(pool *pgxpool.Pool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			tc, ok := tenant.Get(r.Context())
			if !ok {
				writeJSON403(w, "no tenant context")
				return
			}

			// Raw pool — this is infrastructure middleware, not a
			// tenant-scoped business query. tenant_id is explicit.
			var status string
			err := pool.QueryRow(r.Context(),
				"SELECT status FROM workspaces WHERE id = $1",
				tc.TenantID,
			).Scan(&status)

			if err != nil {
				writeJSON403(w, "workspace not found")
				return
			}

			if status != "active" {
				writeJSON403(w, fmt.Sprintf("workspace not active: %s", status))
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func writeJSON403(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	fmt.Fprintf(w,
		`{"errors":[{"message":%q,"extensions":{"code":"FORBIDDEN"}}]}`, msg)
}
