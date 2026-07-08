package auth

import (
	"fmt"
	"log"
	"net/http"

	"github.com/golang-jwt/jwt/v5"
)

// LogoutHandler handles POST /auth/logout.
//
// Invalidates the caller's server-side refresh session so subsequent
// /auth/refresh calls fail. The access token in the Authorization header
// identifies whose session to end — we accept expired tokens (a user
// logging out after 20 minutes idle should not be told "your token is too
// old to log you out"). The signature IS still verified so an attacker
// cannot forge a logout for another user.
//
// Idempotent by design: whether or not a refresh session existed, we
// return 204. Leaking "session was active" via a different status code
// helps an attacker probe live sessions.
//
// Two callers, symmetric to /auth/refresh:
//
//   - Browser (admin UI) — sends Cookie. We also clear the cookie via
//     Set-Cookie with MaxAge=-1 so the browser stops re-presenting a
//     token that no longer matches Redis.
//   - CLI daemon (Rust client) — sends X-Refresh-Token header, has
//     nothing on its own state we can clear. It clears local state
//     itself after this call returns.
func (s *serviceImpl) LogoutHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			// No token to identify the session — nothing to invalidate.
			// Still 204 for idempotency.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		// Parse without expiry validation — user is logging out; a
		// just-expired token is fine. Signature IS enforced (HS256 only).
		parser := jwt.NewParser(jwt.WithoutClaimsValidation())
		claims := &jwtClaims{}
		_, err := parser.ParseWithClaims(
			extractBearer(authHeader), claims,
			func(t *jwt.Token) (interface{}, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, fmt.Errorf("unexpected alg: %v", t.Header["alg"])
				}
				return []byte(s.cfg.JWTSecret), nil
			},
		)
		if err != nil || claims.Subject == "" {
			// Malformed / wrong signature — do not touch Redis. Do not
			// leak the reason. 204 keeps the response shape the same as
			// the success path.
			w.WriteHeader(http.StatusNoContent)
			return
		}

		if err := s.redisClient.DeleteRefreshToken(ctx, claims.Subject); err != nil {
			// Log server-side but still 204 — the caller can't do anything
			// with the failure and reporting it leaks liveness.
			log.Printf("auth logout: delete refresh session user=%s: %v", claims.Subject, err)
		}

		// Best-effort cookie clear for browser callers. No-op for the CLI
		// path (no cookie to clear).
		http.SetCookie(w, &http.Cookie{
			Name:     "refresh_token",
			Value:    "",
			Path:     "/auth/refresh",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   true,
			MaxAge:   -1,
		})

		w.WriteHeader(http.StatusNoContent)
	})
}
