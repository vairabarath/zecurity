package auth

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yourorg/ztna/controller/internal/identity"
)

// RefreshHandler handles POST /auth/refresh — the browser (admin UI) path.
// Registered as a public route in main.go (no auth middleware).
//
// The refresh token arrives only in the httpOnly cookie the browser attaches;
// the rotated token goes back via Set-Cookie and the JSON body carries only
// the new access token. The Rust client used to send its refresh token here
// in an X-Refresh-Token header, over plain HTTP by default — anyone watching
// the network could take over the session (certificate audit follow-up). It
// now uses the ClientService.RefreshSession RPC over the TLS channel pinned
// to its saved anchors, and a header-borne token is refused here like a
// missing one. Both paths share RefreshSession, so the session rules are
// identical.
func (s *serviceImpl) RefreshHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presentedToken := refreshCookie(r)
		if presentedToken == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing refresh token")
			return
		}
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		accessToken, newToken, err := s.RefreshSession(r.Context(), extractBearer(authHeader), presentedToken)
		if err != nil {
			var se *SessionError
			if errors.As(err, &se) {
				writeJSONError(w, se.Status, se.Msg)
			} else {
				writeJSONError(w, http.StatusInternalServerError, "server error")
			}
			return
		}

		http.SetCookie(w, &http.Cookie{
			Name:     "refresh_token",
			Value:    newToken,
			Path:     "/auth/refresh",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   true,
			MaxAge:   int(s.refreshTTL().Seconds()),
		})
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"access_token": accessToken})
	})
}

// SessionError is a refused refresh. Status is what the browser path returns
// (401: the session is dead, 500: a server error) and Msg its message.
type SessionError struct {
	Status int
	Msg    string
}

func (e *SessionError) Error() string { return e.Msg }

// Dead reports whether the session is dead and the caller must sign in again.
func (e *SessionError) Dead() bool { return e.Status == http.StatusUnauthorized }

func dead(msg string) error { return &SessionError{Status: http.StatusUnauthorized, Msg: msg} }

func serverError(msg string) error {
	return &SessionError{Status: http.StatusInternalServerError, Msg: msg}
}

// RefreshSession exchanges an access token (expired is fine; the signature
// is verified) plus the single-use refresh token for a fresh pair. Shared by
// /auth/refresh (browser) and ClientService.RefreshSession (Rust client).
// Errors are *SessionError.
//
// The refresh token is single-use and rotated on every call (ADR-006). Reuse
// of a rotated token fails the constant-time compare below.
//
// Flow:
//
//  1. Read user_id from the access JWT (signature verified, expiry not)
//  2. Enforce user lifecycle and session generation (PENDING-04 Phase 5)
//  3. Look up the refresh session in Redis by user_id
//  4. Compare presented value with stored value (constant-time)
//  5. Enforce the absolute lifetime cap (ADR-006)
//  6. Issue a new access JWT and rotate the refresh token
func (s *serviceImpl) RefreshSession(ctx context.Context, accessToken, refreshToken string) (newAccessToken, newRefreshToken string, err error) {
	if refreshToken == "" {
		return "", "", dead("missing refresh token")
	}
	if accessToken == "" {
		return "", "", dead("missing authorization header")
	}

	// Step 1 — Parse the JWT skipping claims validation (exp check) — we just
	// need the identity. Signature IS verified; expiry IS NOT (refresh accepts
	// expired tokens by design).
	//
	// P9-F1: explicitly enforce HS256 in the keyFunc — without this, an attacker
	// presenting a token with alg=none (or RS256 confusion) could bypass signature.
	// Mirrors session.go::verifyAccessToken and middleware/auth.go::AuthMiddleware.
	parser := jwt.NewParser(jwt.WithoutClaimsValidation())
	claims := &jwtClaims{}
	if _, err := parser.ParseWithClaims(
		accessToken, claims,
		func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, fmt.Errorf("unexpected alg: %v", t.Header["alg"])
			}
			return []byte(s.cfg.JWTSecret), nil
		},
	); err != nil {
		return "", "", dead("invalid access token")
	}

	userID := claims.Subject
	tenantID := claims.TenantID
	role := claims.Role
	email := claims.Email

	if userID == "" || tenantID == "" {
		return "", "", dead("token missing claims")
	}

	// Step 2 — Enforce lifecycle + session-generation revocation at refresh
	// (PENDING-04 Phase 5). Access tokens are short-lived, so gating the
	// refresh chain here kills every session for a suspended/locked user or a
	// bumped identity_generation — with no per-request DB read. The new access
	// token is re-stamped with the CURRENT generation. Skipped only when no
	// pool is configured (unit tests exercising token mechanics in isolation).
	currentGen := claims.IdentityGeneration
	if s.cfg.Pool != nil {
		var dbEmail, status string
		if err := s.cfg.Pool.QueryRow(ctx,
			`SELECT email, status, identity_generation FROM users WHERE id = $1`,
			userID,
		).Scan(&dbEmail, &status, &currentGen); err != nil {
			return "", "", dead("token missing claims")
		}
		if email == "" {
			email = dbEmail
		}
		if err := identity.CheckLifecycle(status); err != nil {
			s.redisClient.DeleteRefreshToken(ctx, userID)
			return "", "", dead("session revoked")
		}
		if err := identity.CheckGeneration(claims.IdentityGeneration, currentGen); err != nil {
			s.redisClient.DeleteRefreshToken(ctx, userID)
			return "", "", dead("session revoked")
		}
	}

	// Step 3 — Look up stored refresh session in Redis by user_id.
	// Called: valkey.go → GetRefreshSession()
	stored, found, err := s.redisClient.GetRefreshSession(ctx, userID)
	if err != nil {
		return "", "", serverError("server error")
	}
	if !found {
		// Idle TTL expired or user signed out.
		return "", "", dead("refresh token expired")
	}

	// Step 4 — Compare tokens using constant-time comparison.
	// Prevents timing attacks that could leak the stored token byte-by-byte.
	if subtle.ConstantTimeCompare([]byte(refreshToken), []byte(stored.Token)) != 1 {
		return "", "", dead("refresh token mismatch")
	}

	// Step 5 — ADR-006: enforce absolute lifetime cap from the initial OAuth
	// signin. Beyond this, the user must re-authenticate via the full OAuth
	// flow. MaxLifetimeAt=0 means "no cap" — only set on legacy sessions
	// issued before this code shipped; treat as still-valid for the rolling
	// TTL window.
	if stored.MaxLifetimeAt != 0 && time.Now().Unix() > stored.MaxLifetimeAt {
		s.redisClient.DeleteRefreshToken(ctx, userID)
		return "", "", dead("refresh token max lifetime exceeded")
	}

	// Step 6 — Issue new access JWT stamped with the current generation.
	// Called: session.go → issueAccessToken()
	newAccessToken, err = s.issueAccessToken(userID, tenantID, role, email, currentGen)
	if err != nil {
		return "", "", serverError("token issue failed")
	}

	// ADR-006 — Rotate the refresh token on every use. Generate a new 256-bit
	// token and replace the stored value (preserving OriginalIAT and
	// MaxLifetimeAt). The old token value is now invalid — a replay of a
	// stolen token fails the constant-time compare on its next refresh.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", "", serverError("token rotate failed")
	}
	newRefreshToken = base64.RawURLEncoding.EncodeToString(raw)

	rotated := RefreshSession{
		Token:         newRefreshToken,
		OriginalIAT:   stored.OriginalIAT,
		MaxLifetimeAt: stored.MaxLifetimeAt,
	}
	if err := s.redisClient.SetRefreshSession(ctx, userID, rotated, s.refreshTTL()); err != nil {
		return "", "", serverError("token rotate failed")
	}
	return newAccessToken, newRefreshToken, nil
}

// refreshTTL is the rolling idle lifetime of a refresh session.
func (s *serviceImpl) refreshTTL() time.Duration {
	ttl, err := time.ParseDuration(s.cfg.JWTRefreshTTL)
	if err != nil {
		return 7 * 24 * time.Hour
	}
	return ttl
}

// refreshCookie returns the refresh token from the browser's httpOnly
// cookie, or "" if there is none. There is deliberately no header fallback
// (see RefreshHandler).
func refreshCookie(r *http.Request) string {
	if cookie, err := r.Cookie("refresh_token"); err == nil {
		return cookie.Value
	}
	return ""
}

// writeJSONError writes a JSON error response with the given status code.
// Called by: RefreshHandler() above (on any failure step).
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// extractBearer extracts the token string from a "Bearer <token>" Authorization header.
// Called by: RefreshHandler() above (Step 2).
func extractBearer(header string) string {
	parts := strings.SplitN(header, " ", 2)
	if len(parts) == 2 && parts[0] == "Bearer" {
		return parts[1]
	}
	return ""
}
