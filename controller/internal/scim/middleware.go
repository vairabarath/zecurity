package scim

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
)

// contextKey is a private type for scim request-context values. Using a
// private type prevents collisions with other packages' context keys.
type contextKey int

const (
	// tokenContextKey carries the authenticated *Token through the request.
	tokenContextKey contextKey = iota
)

// TokenFromContext returns the SCIM token bound to the request, or nil if the
// request was not authenticated by the SCIM bearer middleware.
func TokenFromContext(ctx context.Context) *Token {
	t, ok := ctx.Value(tokenContextKey).(*Token)
	if !ok {
		return nil
	}
	return t
}

// AuthMiddleware authenticates bearer tokens for the SCIM endpoints
// (/scim/v2/*). These are machine-to-machine tokens minted per
// (workspace, connection) scope — never user JWTs.
//
// On a valid token  → calls next with the *Token in the request context.
// On any failure    → fails closed with 401 JSON, stops the chain.
//
// The plaintext is never inspected; Lookup computes the HMAC and searches by
// digest only. Expired, revoked, or unknown tokens all return the same generic
// 401 so the caller cannot distinguish them (no user enumeration).
func (s *Store) AuthMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := r.Header.Get("Authorization")
		if raw == "" {
			writeSCIM401(w, "missing Authorization header")
			return
		}

		parts := strings.Fields(raw)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			writeSCIM401(w, "malformed Authorization header")
			return
		}

		token, err := s.Lookup(r.Context(), parts[1])
		if err != nil {
			// Single generic response for not-found / expired / revoked.
			writeSCIM401(w, "invalid or expired token")
			return
		}

		ctx := context.WithValue(r.Context(), tokenContextKey, token)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func writeSCIM401(w http.ResponseWriter, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	// RFC 7644 SCIM error envelope (status 401).
	detail, _ := json.Marshal(msg)
	_, _ = w.Write([]byte(`{"schemas":["urn:ietf:params:scim:api:messages:error"],"detail":` + string(detail) + `,"status":"401"}`))
}
