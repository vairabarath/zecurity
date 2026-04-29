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
// Field names MUST match Member 4's Claims struct in middleware/auth.go exactly.
// Coordinate with Member 4 before changing any json tag.
// Used by: issueAccessToken(), verifyAccessToken() below.
// Read by: Member 4's middleware/auth.go for JWT verification on every request.
type jwtClaims struct {
	TenantID string `json:"tenant_id"` // coordinate: must match middleware/auth.go
	Role     string `json:"role"`      // coordinate: must match middleware/auth.go
	jwt.RegisteredClaims
	// Subject (sub) = user_id    — set via RegisteredClaims.Subject
	// Issuer  (iss) = appmeta.ControllerIssuer — set via RegisteredClaims.Issuer
	// Expiry  (exp) = now + 15 min      — set via RegisteredClaims.ExpiresAt
}

// IssueAccessToken is the public wrapper around issueAccessToken used by
// gRPC handlers (e.g. the ClientService TokenExchange RPC). It returns the
// signed token plus the TTL in seconds so callers can populate expires_in
// fields without re-parsing the configured TTL.
func (s *serviceImpl) IssueAccessToken(userID, tenantID, role string) (string, int64, error) {
	token, err := s.issueAccessToken(userID, tenantID, role)
	if err != nil {
		return "", 0, err
	}
	ttl, perr := time.ParseDuration(s.cfg.JWTAccessTTL)
	if perr != nil {
		ttl = 15 * time.Minute
	}
	return token, int64(ttl.Seconds()), nil
}

// IssueRefreshToken is the public wrapper around issueRefreshToken.
func (s *serviceImpl) IssueRefreshToken(ctx context.Context, userID string) (string, error) {
	return s.issueRefreshToken(ctx, userID)
}

// VerifyAccessToken parses and verifies a Zecurity-issued JWT and returns
// the public claims view used by gRPC handlers.
func (s *serviceImpl) VerifyAccessToken(tokenStr string) (*AccessTokenClaims, error) {
	claims, err := s.verifyAccessToken(tokenStr)
	if err != nil {
		return nil, err
	}
	return &AccessTokenClaims{
		UserID:   claims.Subject,
		TenantID: claims.TenantID,
		Role:     claims.Role,
	}, nil
}

// issueAccessToken creates a signed short-lived JWT.
// exp = JWTAccessTTL (default 15 minutes) from now. Signed with HS256 using JWT_SECRET.
// Called by: CallbackHandler() in callback.go (Step 8), RefreshHandler() in refresh.go (Step 5).
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
// The raw token is set as an httpOnly cookie by the caller — never returned in the body.
// Called by: CallbackHandler() in callback.go (Step 9).
func (s *serviceImpl) issueRefreshToken(ctx context.Context, userID string) (string, error) {
	// Generate random token: 32 bytes = 256 bits of entropy.
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	token := base64.RawURLEncoding.EncodeToString(raw)

	// Parse TTL from config (default 7 days).
	ttl, err := time.ParseDuration(s.cfg.JWTRefreshTTL)
	if err != nil {
		ttl = 7 * 24 * time.Hour
	}

	// Store in Redis keyed by user_id.
	// Called: redis.go → SetRefreshToken()
	if err := s.redisClient.SetRefreshToken(ctx, userID, token, ttl); err != nil {
		return "", fmt.Errorf("store refresh token: %w", err)
	}

	return token, nil
}

// verifyAccessToken parses and verifies an access JWT. Returns the claims if valid.
// Used in tests only. In production, Member 4's middleware/auth.go handles JWT verification.
// Called by: tests (not used in production code paths).
func (s *serviceImpl) verifyAccessToken(tokenStr string) (*jwtClaims, error) {
	claims := &jwtClaims{}
	token, err := jwt.ParseWithClaims(
		tokenStr, claims,
		func(t *jwt.Token) (interface{}, error) {
			// Enforce HS256 — blocks alg=none and alg=RS256 confusion attacks.
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
