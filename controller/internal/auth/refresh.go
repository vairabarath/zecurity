package auth

import (
	"crypto/subtle"
	"encoding/json"
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
		parser := jwt.NewParser(jwt.WithoutClaimsValidation())
		claims := &jwtClaims{}
		_, err = parser.ParseWithClaims(
			extractBearer(authHeader), claims,
			func(t *jwt.Token) (interface{}, error) {
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

		// Step 3 — Look up stored refresh token in Redis by user_id.
		// Called: redis.go → GetRefreshToken()
		storedToken, found, err := s.redisClient.GetRefreshToken(ctx, userID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "server error")
			return
		}
		if !found {
			// Token expired (>7 days) or user signed out.
			writeJSONError(w, http.StatusUnauthorized, "refresh token expired")
			return
		}

		// Step 4 — Compare tokens using constant-time comparison.
		// Prevents timing attacks that could leak the stored token byte-by-byte.
		if subtle.ConstantTimeCompare([]byte(cookieToken), []byte(storedToken)) != 1 {
			writeJSONError(w, http.StatusUnauthorized, "refresh token mismatch")
			return
		}

		// Step 5 — Issue new access JWT.
		// Called: session.go → issueAccessToken()
		accessToken, err := s.issueAccessToken(userID, tenantID, role, email)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "token issue failed")
			return
		}

		// Slide the refresh token TTL so active users are never logged out.
		// Only truly inactive users (no requests for 7 days) will be logged out.
		ttl, perr := time.ParseDuration(s.cfg.JWTRefreshTTL)
		if perr != nil {
			ttl = 7 * 24 * time.Hour
		}
		s.redisClient.SetRefreshToken(ctx, userID, cookieToken, ttl)

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
