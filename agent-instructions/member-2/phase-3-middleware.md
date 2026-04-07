# Phase 3 — Middleware

Two middleware components. Both run before any GraphQL resolver executes.

---

## File 1: `controller/internal/middleware/auth.go`

**Path:** `controller/internal/middleware/auth.go`

```go
package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

type Claims struct {
	TenantID string `json:"tenant_id"`
	Role     string `json:"role"`
	jwt.RegisteredClaims
}

// AuthMiddleware verifies the JWT and injects TenantContext.
//
// On valid JWT   → calls next with TenantContext in ctx
// On invalid JWT → returns 401 JSON, stops the chain
//
// Public routes must be registered outside this middleware.
// No bypass logic lives here — route registration handles that.
func AuthMiddleware(secret string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

			raw := r.Header.Get("Authorization")
			if raw == "" {
				writeJSON401(w, "missing Authorization header")
				return
			}

			parts := strings.SplitN(raw, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" {
				writeJSON401(w, "malformed Authorization header")
				return
			}

			claims := &Claims{}
			token, err := jwt.ParseWithClaims(
				parts[1], claims,
				func(t *jwt.Token) (interface{}, error) {
					// Enforce expected signing method.
					// Skipping this check allows alg=none attacks.
					if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
						return nil, fmt.Errorf("unexpected alg: %v", t.Header["alg"])
					}
					return []byte(secret), nil
				},
				jwt.WithIssuer("ztna-controller"),
				jwt.WithExpirationRequired(),
			)
			if err != nil || !token.Valid {
				writeJSON401(w, "invalid or expired token")
				return
			}

			if claims.Subject == "" || claims.TenantID == "" || claims.Role == "" {
				writeJSON401(w, "token missing required claims")
				return
			}

			ctx := tenant.Set(r.Context(), tenant.TenantContext{
				TenantID: claims.TenantID,
				UserID:   claims.Subject,
				Role:     claims.Role,
			})

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func writeJSON401(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	fmt.Fprintf(w,
		`{"errors":[{"message":%q,"extensions":{"code":"UNAUTHORIZED"}}]}`, msg)
}
```

---

## File 2: `controller/internal/middleware/workspace.go`

**Path:** `controller/internal/middleware/workspace.go`

```go
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
```

---

## Verification Checklist

```
[ ] AuthMiddleware returns 401 on missing header
[ ] AuthMiddleware returns 401 on expired token
[ ] AuthMiddleware returns 401 on wrong signing method
[ ] AuthMiddleware injects TenantContext on valid token
[ ] WorkspaceGuard returns 403 when workspace status != 'active'
[ ] WorkspaceGuard calls next when workspace status = 'active'
```
