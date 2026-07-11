package middleware

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// RequireProvider verifies a provider-scoped JWT (aud=provider) and confirms the
// caller is an ACTIVE row in provider_users, then injects the provider Actor.
//
// It NEVER calls WorkspaceGuard — provider identity has no tenant. A tenant JWT
// fails here because it lacks aud=provider (enforced by VerifyProviderToken).
// Must run before any provider handler; handlers read the Actor via
// provider.ActorFromContext.
func RequireProvider(secret string, store *provider.Store) func(http.Handler) http.Handler {
      return func(next http.Handler) http.Handler {
              return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
                      raw := r.Header.Get("Authorization")
                      if raw == "" {
                              writeProviderJSON(w, http.StatusUnauthorized, "missing Authorization header")
                              return
                      }
                      parts := strings.SplitN(raw, " ", 2)
                      if len(parts) != 2 || parts[0] != "Bearer" {
                              writeProviderJSON(w, http.StatusUnauthorized, "malformed Authorization header")
                              return
                      }

                      claims, err := provider.VerifyProviderToken(secret, parts[1])
                      if err != nil {
                              writeProviderJSON(w, http.StatusUnauthorized, "invalid or expired provider token")
                              return
                      }

                      // Allowlist check: the email must be an ACTIVE provider_users row.
                      // GetByEmail returns ErrProviderUserNotFound for missing OR disabled.
                      user, err := store.GetByEmail(r.Context(), claims.Email)
                      if errors.Is(err, provider.ErrProviderUserNotFound) {
                              writeProviderJSON(w, http.StatusForbidden, "not a provider user")
                              return
                      }
                      if err != nil {
                              writeProviderJSON(w, http.StatusInternalServerError, "provider lookup failed")
                              return
                      }

                      // Role comes from the DB (source of truth), not the token — so a role
                      // change or disable takes effect on the next request, no re-login.
                      actor := provider.Actor{
                              UserID: user.ID,
                              Email:  user.Email,
                              Role:   user.Role,
                      }
                      ctx := provider.WithActor(r.Context(), actor)
                      next.ServeHTTP(w, r.WithContext(ctx))
              })
      }
}

func writeProviderJSON(w http.ResponseWriter, status int, msg string) {
      w.Header().Set("Content-Type", "application/json")
      w.WriteHeader(status)
      _ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}