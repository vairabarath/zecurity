package middleware

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/yourorg/ztna/controller/internal/provider"
)

// ProviderTokenVerifier validates a provider JWT. *provider.IdentityService
// implements it with the dedicated provider key (D-25).
type ProviderTokenVerifier interface {
	Verify(token string) (*provider.ProviderClaims, error)
}

// ProviderUserLookup loads a provider user by id, including disabled rows.
// *provider.Store implements it.
type ProviderUserLookup interface {
	GetByID(ctx context.Context, id string) (*provider.ProviderUser, error)
}

// ProviderOption adjusts RequireProvider for a single route.
type ProviderOption func(*providerOptions)

type providerOptions struct {
	allowPasswordChangeToken bool
}

// AllowPasswordChangeToken lets a password-change-only token (claim pwc=true)
// through. Use it ONLY on POST /provider/auth/password; every other route
// rejects such tokens with 403 password_change_required.
func AllowPasswordChangeToken() ProviderOption {
	return func(o *providerOptions) { o.allowPasswordChangeToken = true }
}

// RequireProvider authenticates a provider request and injects the provider
// Actor (Sprint 21 Phase H). In order:
//
//  1. Bearer token verified with the provider key: HS256, iss=zecurity-provider,
//     aud=provider, exp (a tenant JWT fails here).
//  2. The user is loaded BY ID; missing or disabled → 403.
//  3. Token gen ≠ provider_users.session_generation → 401 (logout, password
//     change/reset, disable or role change revoked it).
//  4. Token email ≠ current email → 401.
//  5. A password-change-only token without AllowPasswordChangeToken → 403.
//
// The role always comes from the DB row, never the token. It NEVER calls
// WorkspaceGuard — provider identity has no tenant.
func RequireProvider(verifier ProviderTokenVerifier, users ProviderUserLookup, opts ...ProviderOption) func(http.Handler) http.Handler {
	var o providerOptions
	for _, opt := range opts {
		opt(&o)
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := r.Header.Get("Authorization")
			if raw == "" {
				writeProviderJSON(w, http.StatusUnauthorized, "missing Authorization header")
				return
			}
			parts := strings.SplitN(raw, " ", 2)
			if len(parts) != 2 || parts[0] != "Bearer" || parts[1] == "" {
				writeProviderJSON(w, http.StatusUnauthorized, "malformed Authorization header")
				return
			}

			claims, err := verifier.Verify(parts[1])
			if err != nil {
				writeProviderJSON(w, http.StatusUnauthorized, "invalid or expired provider token")
				return
			}

			user, err := users.GetByID(r.Context(), claims.Subject)
			if errors.Is(err, provider.ErrProviderUserNotFound) {
				writeProviderJSON(w, http.StatusForbidden, "not a provider user")
				return
			}
			if err != nil {
				writeProviderJSON(w, http.StatusInternalServerError, "provider lookup failed")
				return
			}
			if user.DisabledAt != nil {
				writeProviderJSON(w, http.StatusForbidden, "not a provider user")
				return
			}
			if claims.Gen != user.SessionGeneration {
				writeProviderJSON(w, http.StatusUnauthorized, "provider session revoked")
				return
			}
			if claims.Email != user.Email {
				writeProviderJSON(w, http.StatusUnauthorized, "provider session revoked")
				return
			}
			if claims.PWC && !o.allowPasswordChangeToken {
				writeProviderJSON(w, http.StatusForbidden, "password_change_required")
				return
			}

			actor := provider.Actor{
				UserID: user.ID,
				Email:  user.Email,
				Role:   user.Role,
			}
			next.ServeHTTP(w, r.WithContext(provider.WithActor(r.Context(), actor)))
		})
	}
}

func writeProviderJSON(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
