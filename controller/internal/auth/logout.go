package auth

import (
	"context"
	"fmt"
	"log"
	"net/http"

	"github.com/golang-jwt/jwt/v5"
	"github.com/yourorg/ztna/controller/internal/audit"
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
// This is the browser (admin UI) path: it sends the access token and we also
// clear the refresh cookie via Set-Cookie with MaxAge=-1 so the browser stops
// re-presenting a token that no longer matches Redis. The Rust client uses
// the ClientService.Logout RPC over its pinned TLS channel instead
// (certificate audit follow-up); both share EndSession.
func (s *serviceImpl) LogoutHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// No Authorization header: nothing to identify the session, nothing
		// to invalidate. Still 204 for idempotency.
		if authHeader := r.Header.Get("Authorization"); authHeader != "" {
			s.EndSession(r.Context(), extractBearer(authHeader))
		}

		// Best-effort cookie clear for browser callers.
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

// EndSession deletes the refresh session the access token belongs to (its sid
// claim; the legacy per-user key for a token without one). The user's other
// sessions — other tabs, the client daemon — stay signed in.
// The token may be expired (the user is logging out) but its signature is
// enforced (HS256 only). Idempotent and silent: a malformed token, no session,
// or a Redis failure all look the same to the caller, so nothing leaks about
// session liveness. Shared by /auth/logout and ClientService.Logout.
func (s *serviceImpl) EndSession(ctx context.Context, accessToken string) {
	if accessToken == "" {
		return
	}
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	claims := &jwtClaims{}
	_, err := parser.ParseWithClaims(
		accessToken, claims,
		func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected alg: %v", t.Header["alg"])
			}
			return []byte(s.cfg.JWTSecret), nil
		},
	)
	if err != nil || claims.Subject == "" {
		// Malformed / wrong signature — do not touch Redis.
		return
	}
	ended, err := s.redisClient.EndRefreshSession(ctx, claims.Subject, claims.SessionID)
	// PENDING-09 C-1 (D-3): devices enrolled under this session end with it,
	// whether or not the session still existed (a sign-out with an expired
	// token, or after the session idled out) and even if the Valkey delete
	// failed. AFTER the delete: EnrollDevice re-checks the session after
	// inserting its device, so either that re-check sees the session gone or
	// this revoke sees the device row. A browser session's sid is on no device.
	s.sessionEnded(ctx, claims.Subject, claims.SessionID, "signed_out")
	if err != nil {
		// Logged server-side only — reporting it would leak liveness.
		log.Printf("auth logout: delete refresh session user=%s: %v", claims.Subject, err)
		return
	}
	// Audit only a session this call actually ended, so replaying a token
	// can't flood the (kept-forever) admin stream (PENDING-11 P-9).
	if ended {
		s.record(ctx, audit.Entry{
			TenantID:    claims.TenantID,
			ActorUserID: claims.Subject,
			ActorEmail:  claims.Email,
			Action:      ActionSignOut,
			TargetType:  "user",
			TargetID:    claims.Subject,
			Details:     map[string]any{"client": IsClientChannel(ctx)},
		})
	}
}
