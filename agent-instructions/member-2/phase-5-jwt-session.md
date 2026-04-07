# Phase 5 — JWT Issuance + Refresh Token

Issues short-lived access JWTs and random refresh tokens stored in Redis.
The JWT field names MUST match Member 4's Claims struct in `middleware/auth.go`.

---

## File: `internal/auth/session.go`

**Path:** `internal/auth/session.go`

```go
package auth

import (
    "context"
    "crypto/rand"
    "encoding/base64"
    "fmt"
    "time"

    "github.com/golang-jwt/jwt/v5"
)

// jwtClaims is the payload for JWTs issued by this controller.
// Field names must match Member 4's Claims struct in middleware/auth.go exactly.
// Coordinate with Member 4 before changing any json tag.
// Called by: issueAccessToken(), verifyAccessToken() below
type jwtClaims struct {
    TenantID string `json:"tenant_id"` // coordinate: must match middleware/auth.go
    Role     string `json:"role"`      // coordinate: must match middleware/auth.go
    jwt.RegisteredClaims
    // Subject (sub) = user_id — set via RegisteredClaims.Subject
    // Issuer  (iss) = "ztna-controller" — set via RegisteredClaims.Issuer
    // Expiry  (exp) = now + 15 min — set via RegisteredClaims.ExpiresAt
}

// issueAccessToken creates a signed short-lived JWT.
// exp = 15 minutes from now.
// Signed with HS256 using JWT_SECRET.
// Called by: CallbackHandler() in callback.go (Step 8), RefreshHandler() in refresh.go (Step 5)
func (s *serviceImpl) issueAccessToken(userID, tenantID, role string) (string, error) {
    ttl, err := time.ParseDuration(s.cfg.JWTAccessTTL)
    if err != nil {
        ttl = 15 * time.Minute
    }

    now := time.Now()
    claims := jwtClaims{
        TenantID: tenantID,
        Role:     role,
        RegisteredClaims: jwt.RegisteredClaims{
            Subject:   userID,
            Issuer:    s.cfg.JWTIssuer,
            IssuedAt:  jwt.NewNumericDate(now),
            ExpiresAt: jwt.NewNumericDate(now.Add(ttl)),
        },
    }

    token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
    signed, err := token.SignedString([]byte(s.cfg.JWTSecret))
    if err != nil {
        return "", fmt.Errorf("sign JWT: %w", err)
    }

    return signed, nil
}

// issueRefreshToken creates a random 256-bit refresh token,
// stores it in Redis keyed to the user_id, and returns the token value.
// The raw token is set as an httpOnly cookie — never returned in the body.
// Called by: CallbackHandler() in callback.go (Step 9)
func (s *serviceImpl) issueRefreshToken(ctx context.Context,
    userID string) (string, error) {

    // Generate random token
    raw := make([]byte, 32) // 32 bytes = 256 bits
    if _, err := rand.Read(raw); err != nil {
        return "", fmt.Errorf("generate refresh token: %w", err)
    }
    token := base64.RawURLEncoding.EncodeToString(raw)

    // Parse TTL
    ttl, err := time.ParseDuration(s.cfg.JWTRefreshTTL)
    if err != nil {
        ttl = 7 * 24 * time.Hour
    }

    // Store in Redis
    // Called: redis.go → SetRefreshToken()
    if err := s.redisClient.SetRefreshToken(ctx, userID, token, ttl); err != nil {
        return "", fmt.Errorf("store refresh token: %w", err)
    }

    return token, nil
}

// verifyAccessToken parses and verifies an access JWT.
// Returns the claims if valid.
// Used in tests. In production, Member 4's middleware/auth.go handles this.
// Called by: tests only (not used in production code paths)
func (s *serviceImpl) verifyAccessToken(tokenStr string) (*jwtClaims, error) {
    claims := &jwtClaims{}
    token, err := jwt.ParseWithClaims(
        tokenStr, claims,
        func(t *jwt.Token) (interface{}, error) {
            if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
                return nil, fmt.Errorf("unexpected alg: %v", t.Header["alg"])
            }
            return []byte(s.cfg.JWTSecret), nil
        },
        jwt.WithIssuer(s.cfg.JWTIssuer),
        jwt.WithExpirationRequired(),
    )
    if err != nil || !token.Valid {
        return nil, fmt.Errorf("invalid token: %w", err)
    }
    return claims, nil
}
```

---

## Phase 5 Checklist

```
✓ JWT contains tenant_id (json tag "tenant_id" — matches middleware)
✓ JWT contains role (json tag "role" — matches middleware)
✓ JWT subject = user_id
✓ JWT issuer = "ztna-controller" (matches middleware)
✓ JWT exp = 15 minutes from now
✓ Signed with HS256
```
