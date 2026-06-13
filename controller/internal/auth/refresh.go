package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// RefreshHandler handles POST /auth/refresh.
// Registered as a public route in main.go (no auth middleware).
// Called by: main.go route registration (Member 4 wires this: mux.Handle("/auth/refresh", authSvc.RefreshHandler())).
//
// Flow:
//
//  1. Read refresh_token from httpOnly cookie
//  2. Read user_id from the expired (or expiring) access JWT in Authorization header
//     Note: we parse the JWT without verifying expiry — we just need the user_id
//     to look up the refresh token in Redis
//  3. Look up refresh token in Redis by user_id        → calls redis.go → GetRefreshToken()
//  4. Compare cookie value with stored value (constant-time)
//  5. Issue new access JWT                              → calls session.go → issueAccessToken()
//  6. Return new access JWT in JSON body
//
// The refresh token itself is NOT rotated on every refresh.
// It expires after 7 days and the user must log in again.
func (s *serviceImpl) RefreshHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// Step 1 — Read refresh token from httpOnly cookie.
		// This cookie was set by CallbackHandler() in callback.go (Step 9).
		cookie, err := r.Cookie("refresh_token")
		if err != nil || cookie.Value == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing refresh token")
			return
		}
		cookieToken := cookie.Value

		// Step 2 — Read user_id from the access JWT (without verifying expiry).
		// The access JWT is sent in the Authorization header even when expired.
		// We only need the sub (user_id), tenant_id, and role claims.
		authHeader := r.Header.Get("Authorization")
		if authHeader == "" {
			writeJSONError(w, http.StatusUnauthorized, "missing authorization header")
			return
		}

		// Parse JWT skipping claims validation (exp check) — we just need the identity.
		// Signature IS verified; expiry IS NOT (refresh accepts expired tokens by design).
		//
		// P9-F1: explicitly enforce HS256 in the keyFunc — without this, an attacker
		// presenting a token with alg=none (or RS256 confusion) could bypass signature.
		// Mirrors session.go::verifyAccessToken and middleware/auth.go::AuthMiddleware.
		parser := jwt.NewParser(jwt.WithoutClaimsValidation())
		claims := &jwtClaims{}
		_, err = parser.ParseWithClaims(
			extractBearer(authHeader), claims,
			func(t *jwt.Token) (interface{}, error) {
				if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
					return nil, fmt.Errorf("unexpected alg: %v", t.Header["alg"])
				}
				return []byte(s.cfg.JWTSecret), nil
			},
		)
		if err != nil {
			writeJSONError(w, http.StatusUnauthorized, "invalid access token")
			return
		}

		userID := claims.Subject
		tenantID := claims.TenantID
		role := claims.Role
		email := claims.Email

		if userID == "" || tenantID == "" {
			writeJSONError(w, http.StatusUnauthorized, "token missing claims")
			return
		}

		if email == "" && s.cfg.Pool != nil {
			if err := s.cfg.Pool.QueryRow(ctx,
				`SELECT email FROM users WHERE id = $1`,
				userID,
			).Scan(&email); err != nil {
				writeJSONError(w, http.StatusUnauthorized, "token missing claims")
				return
			}
		}

		// Step 3 — Look up stored refresh session in Redis by user_id.
		// Called: valkey.go → GetRefreshSession()
		stored, found, err := s.redisClient.GetRefreshSession(ctx, userID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "server error")
			return
		}
		if !found {
			// Idle TTL expired or user signed out.
			writeJSONError(w, http.StatusUnauthorized, "refresh token expired")
			return
		}

		// Step 4 — Compare tokens using constant-time comparison.
		// Prevents timing attacks that could leak the stored token byte-by-byte.
		if subtle.ConstantTimeCompare([]byte(cookieToken), []byte(stored.Token)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "refresh token mismatch")
			return
		}

		// ADR-006: enforce absolute lifetime cap from the initial OAuth signin.
		// Beyond this, the user must re-authenticate via the full OAuth flow.
		// MaxLifetimeAt=0 means "no cap" — only set on legacy sessions issued
		// before this code shipped; treat as still-valid for the rolling TTL window.
		if stored.MaxLifetimeAt != 0 && time.Now().Unix() > stored.MaxLifetimeAt {
			s.redisClient.DeleteRefreshToken(ctx, userID)
			writeJSONError(w, http.StatusUnauthorized, "refresh token max lifetime exceeded")
			return
		}

		// Step 5 — Issue new access JWT.
		// Called: session.go → issueAccessToken()
		accessToken, err := s.issueAccessToken(userID, tenantID, role, email)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "token issue failed")
			return
		}

		// ADR-006 — Rotate the refresh token on every use.
		// Generate a new 256-bit token, replace the stored value (preserving
		// OriginalIAT and MaxLifetimeAt), and overwrite the cookie. The old
		// token value is now invalid — a replay attempt by a stolen cookie
		// will fail the constant-time compare on its next refresh.
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "token rotate failed")
			return
		}
		newToken := base64.RawURLEncoding.EncodeToString(raw)

		ttl, perr := time.ParseDuration(s.cfg.JWTRefreshTTL)
		if perr != nil {
			ttl = 7 * 24 * time.Hour
		}

		rotated := RefreshSession{
			Token:         newToken,
			OriginalIAT:   stored.OriginalIAT,
			MaxLifetimeAt: stored.MaxLifetimeAt,
		}
		if err := s.redisClient.SetRefreshSession(ctx, userID, rotated, ttl); err != nil {
			writeJSONError(w, http.StatusInternalServerError, "token rotate failed")
			return
		}

		// Set new cookie value. Attributes mirror what callback.go set on initial issue.
		http.SetCookie(w, &http.Cookie{
			Name:     "refresh_token",
			Value:    newToken,
			Path:     "/auth/refresh",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   true,
			MaxAge:   int(ttl.Seconds()),
		})

		// Step 6 — Return new access JWT in JSON body.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"access_token": accessToken,
		})
	})
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
