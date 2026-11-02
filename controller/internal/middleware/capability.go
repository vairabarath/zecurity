package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"

	"github.com/yourorg/ztna/controller/internal/rbac"
	"github.com/yourorg/ztna/controller/internal/tenant"
)

// RoleSource resolves a caller's live workspace role (rbac.Checker).
type RoleSource interface {
	Role(ctx context.Context, tenantID, userID string) (string, error)
}

// RequireCapability allows only callers whose LIVE role (users.role, not the
// JWT claim) holds capability c — the REST counterpart of the GraphQL
// @requires directive (PENDING-19 §4.2). Must run after AuthMiddleware.
// Returns 401 without an identity, 403 when the role lacks c or the user is
// missing / not active, and 403 when the lookup fails (fail closed).
func RequireCapability(roles RoleSource, c rbac.Capability) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tc, ok := tenant.Get(r.Context())
			if !ok {
				writeCapabilityError(w, http.StatusUnauthorized, "unauthenticated")
				return
			}
			role, err := roles.Role(r.Context(), tc.TenantID, tc.UserID)
			if err != nil {
				if !errors.Is(err, rbac.ErrNoAccess) {
					log.Printf("require_capability: %s lookup failed: %v", c, err)
				}
				writeCapabilityError(w, http.StatusForbidden, "forbidden")
				return
			}
			if !rbac.Allows(role, c) {
				writeCapabilityError(w, http.StatusForbidden, "forbidden")
				return
			}
			next.ServeHTTP(w, r.WithContext(rbac.WithRole(r.Context(), role)))
		})
	}
}

func writeCapabilityError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg}) //nolint:errcheck
}
