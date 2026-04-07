package middleware

import (
	"fmt"
	"net/http"
	"strings"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yourorg/ztna/controller/internal/appmeta"
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
				jwt.WithIssuer(appmeta.ControllerIssuer),
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
