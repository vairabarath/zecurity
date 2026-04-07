# Phase 7 — Refresh Handler

Handles token refresh: reads the refresh token from httpOnly cookie,
validates it against Redis, and issues a new access JWT.

---

## File: `internal/auth/refresh.go`

**Path:** `internal/auth/refresh.go`

```go
package auth

import (
    "crypto/subtle"
    "encoding/json"
    "net/http"
    "strings"

    "github.com/golang-jwt/jwt/v5"
)

// RefreshHandler handles POST /auth/refresh.
// Registered as a public route in main.go (no auth middleware).
// Called by: main.go route registration (Member 4 wires this up)
//
// Flow:
//   1. Read refresh_token from httpOnly cookie
//   2. Read user_id from the expired (or expiring) access JWT
//      Note: we read the JWT without verifying expiry here — we just
//      need the user_id to look up the refresh token in Redis
//   3. Look up refresh token in Redis by user_id     → calls redis.go → GetRefreshToken()
//   4. Compare cookie value with stored value (constant-time)
//   5. Issue new access JWT                           → calls session.go → issueAccessToken()
//   6. Return new access JWT in JSON body
//
// The refresh token itself is NOT rotated on every refresh.
// It expires after 7 days and the user must log in again.
// Token rotation can be added later if needed.
func (s *serviceImpl) RefreshHandler() http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctx := r.Context()

        // Step 1 — Read refresh token cookie
        cookie, err := r.Cookie("refresh_token")
        if err != nil || cookie.Value == "" {
            writeJSONError(w, http.StatusUnauthorized, "missing refresh token")
            return
        }
        cookieToken := cookie.Value

        // Step 2 — Read user_id from the access JWT (without verifying expiry)
        // The access JWT is sent in the Authorization header even when expired.
        // We only need the sub (user_id) claim — not to trust the token.
        authHeader := r.Header.Get("Authorization")
        if authHeader == "" {
            writeJSONError(w, http.StatusUnauthorized, "missing authorization header")
            return
        }

        var userID, tenantID, role string
        parser := jwt.NewParser(jwt.WithoutClaimsValidation()) // skip exp check
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
        userID = claims.Subject
        tenantID = claims.TenantID
        role = claims.Role

        if userID == "" || tenantID == "" {
            writeJSONError(w, http.StatusUnauthorized, "token missing claims")
            return
        }

        // Step 3 — Look up stored refresh token
        // Called: redis.go → GetRefreshToken()
        storedToken, found, err := s.redisClient.GetRefreshToken(ctx, userID)
        if err != nil {
            writeJSONError(w, http.StatusInternalServerError, "server error")
            return
        }
        if !found {
            // Token expired or user signed out
            writeJSONError(w, http.StatusUnauthorized, "refresh token expired")
            return
        }

        // Step 4 — Compare tokens (constant-time to prevent timing attacks)
        if subtle.ConstantTimeCompare(
            []byte(cookieToken), []byte(storedToken)) != 1 {
            writeJSONError(w, http.StatusUnauthorized, "refresh token mismatch")
            return
        }

        // Step 5 — Issue new access JWT
        // Called: session.go → issueAccessToken()
        accessToken, err := s.issueAccessToken(userID, tenantID, role)
        if err != nil {
            writeJSONError(w, http.StatusInternalServerError, "token issue failed")
            return
        }

        // Step 6 — Return new access JWT
        w.Header().Set("Content-Type", "application/json")
        json.NewEncoder(w).Encode(map[string]string{
            "access_token": accessToken,
        })
    })
}

// writeJSONError writes a JSON error response with the given status code.
// Called by: RefreshHandler() above (on any failure step)
func writeJSONError(w http.ResponseWriter, status int, msg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(status)
    json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// extractBearer extracts the token from a "Bearer <token>" Authorization header.
// Called by: RefreshHandler() above (Step 2)
func extractBearer(header string) string {
    parts := strings.SplitN(header, " ", 2)
    if len(parts) == 2 && parts[0] == "Bearer" {
        return parts[1]
    }
    return ""
}
```

---

## Phase 7 Checklist

```
✓ missing refresh cookie → 401
✓ missing Authorization header → 401
✓ expired JWT readable without verifying exp (for user_id extraction)
✓ Redis lookup by user_id
✓ token comparison is constant-time (crypto/subtle)
✓ new access JWT returned in JSON body
✓ refresh token NOT rotated (acceptable for this sprint)
```

---

## Dependency Map — What Blocks What

```
Can start immediately after Member 4 Phase 1:
  config.go       ← needs auth.Service interface (Member 4)
  redis.go        ← needs docker-compose.yml (Member 4)
  oidc.go         ← no external deps beyond standard library
  idtoken.go      ← no external deps beyond standard library
  exchange.go     ← no external deps beyond standard library
  session.go      ← no external deps beyond standard library

Must wait for bootstrap stub before writing callback.go:
  callback.go     ← calls bootstrap.Bootstrap()
                     write stub yourself (see phase-6)
                     replace with real call when Member 3 ships

Must coordinate with Member 4 before writing session.go:
  jwtClaims struct field names must match middleware/auth.go Claims struct
  agree: "tenant_id" not "tenantId", "iss" = "ztna-controller"
```

---

## Integration with Member 3

```
✓ bootstrap.Bootstrap() stub in place before callback.go is tested
✓ stub replaced with real call when Member 3 ships — zero changes to callback.go
✓ Result struct fields: TenantID, UserID, Role — agreed with Member 3
```

## Integration with Member 4

```
✓ jwtClaims.TenantID json tag = "tenant_id" (matches middleware/auth.go)
✓ jwtClaims.Role json tag = "role" (matches middleware/auth.go)
✓ JWT issuer = "ztna-controller" (matches middleware/auth.go WithIssuer check)
✓ auth.NewService constructor signature matches main.go call in Member 4
✓ auth.Config fields match what Member 4 passes from env vars
```

---

## Summary

```
Phase 1  config.go + redis.go          ← foundation, unblocks all other phases
Phase 2  oidc.go                        ← PKCE generation, initiateAuth
Phase 3  idtoken.go                     ← Google id_token verification (JWKS)
Phase 4  exchange.go                    ← server-to-server token exchange
Phase 5  session.go                     ← JWT issuance + refresh token storage
Phase 6  callback.go                    ← full OAuth callback handler
Phase 7  refresh.go                     ← token refresh handler
```
