package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
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
	Email    string `json:"email"`
	// IdentityGeneration is users.identity_generation at mint time (PENDING-04
	// Phase 5). Refresh re-reads the current generation and rejects a token that
	// is behind — the mechanism for mass session revocation. Omitted when 0 so
	// legacy tokens stay compact and are treated as "no generation check".
	IdentityGeneration int `json:"gen,omitempty"`
	// SessionID (sid) names the refresh session this access token belongs to,
	// so logout ends only that login (refresh:{<user>}:<sid>). Omitted on tokens
	// minted before per-login sessions; those map to the legacy key.
	SessionID string `json:"sid,omitempty"`
	jwt.RegisteredClaims
	// Subject (sub) = user_id    — set via RegisteredClaims.Subject
	// Issuer  (iss) = appmeta.ControllerIssuer — set via RegisteredClaims.Issuer
	// Expiry  (exp) = now + 15 min      — set via RegisteredClaims.ExpiresAt
}

// IssueSession starts one login: it stores a new refresh session and mints an
// access token bound to it (sid claim). It returns the access token, its TTL in
// seconds (for expires_in fields) and the raw refresh token. Each call is an
// independent session; it never displaces the user's other sessions.
func (s *serviceImpl) IssueSession(ctx context.Context, userID, tenantID, role, email string, generation int) (string, int64, string, error) {
	refreshToken, err := s.issueRefreshToken(ctx, userID)
	if err != nil {
		return "", 0, "", err
	}
	sid, _ := splitRefreshToken(refreshToken)
	accessToken, err := s.issueSessionAccessToken(userID, tenantID, role, email, generation, sid)
	if err != nil {
		return "", 0, "", err
	}
	ttl, perr := time.ParseDuration(s.cfg.JWTAccessTTL)
	if perr != nil {
		ttl = 15 * time.Minute
	}
	return accessToken, int64(ttl.Seconds()), refreshToken, nil
}

// InvalidateUserSessions deletes every server-side refresh session of the user
// so a generation bump (identity.Revoker) takes effect immediately. Satisfies
// identity.SessionInvalidator.
func (s *serviceImpl) InvalidateUserSessions(ctx context.Context, userID string) error {
	return s.redisClient.DeleteAllRefreshSessions(ctx, userID)
}

// VerifyAccessToken parses and verifies a Zecurity-issued JWT and returns
// the public claims view used by gRPC handlers.
func (s *serviceImpl) VerifyAccessToken(tokenStr string) (*AccessTokenClaims, error) {
	claims, err := s.verifyAccessToken(tokenStr)
	if err != nil {
		return nil, err
	}
	return &AccessTokenClaims{
		UserID:     claims.Subject,
		TenantID:   claims.TenantID,
		Role:       claims.Role,
		Email:      claims.Email,
		Generation: claims.IdentityGeneration,
	}, nil
}

// issueAccessToken creates a signed short-lived JWT.
// exp = JWTAccessTTL (default 15 minutes) from now. Signed with HS256 using JWT_SECRET.
// Called by: CallbackHandler() in callback.go (Step 8), RefreshHandler() in refresh.go (Step 5).
func (s *serviceImpl) issueAccessToken(userID, tenantID, role, email string, generation int) (string, error) {
	return s.issueSessionAccessToken(userID, tenantID, role, email, generation, "")
}

// issueSessionAccessToken is issueAccessToken with the sid claim set.
func (s *serviceImpl) issueSessionAccessToken(userID, tenantID, role, email string, generation int, sid string) (string, error) {
	ttl, err := time.ParseDuration(s.cfg.JWTAccessTTL)
	if err != nil {
		ttl = 15 * time.Minute
	}

	now := time.Now()
	claims := jwtClaims{
		TenantID:           tenantID,
		Role:               role,
		Email:              email,
		IdentityGeneration: generation,
		SessionID:          sid,
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

// issueRefreshToken starts a new refresh session: a fresh session id plus a
// random 256-bit secret, stored in Redis as a RefreshSession (token +
// original_iat + max_lifetime_at) under refresh:{<userID>}:<sid>. Returns the
// token value "<sid>.<secret>".
//
// The raw token is set as an httpOnly cookie by the caller — never in body.
// Called by: CallbackHandler() in callback.go (Step 9).
//
// ADR-006: original_iat is preserved across rotations; max_lifetime_at caps
// the absolute session lifetime independently of the rolling idle TTL.
func (s *serviceImpl) issueRefreshToken(ctx context.Context, userID string) (string, error) {
	sid, err := randomToken(16)
	if err != nil {
		return "", fmt.Errorf("generate session id: %w", err)
	}
	secret, err := randomToken(32)
	if err != nil {
		return "", fmt.Errorf("generate refresh token: %w", err)
	}
	token := sid + "." + secret

	ttl, err := time.ParseDuration(s.cfg.JWTRefreshTTL)
	if err != nil {
		ttl = 7 * 24 * time.Hour
	}

	maxLifetime, err := time.ParseDuration(s.cfg.JWTRefreshMaxLifetime)
	if err != nil {
		maxLifetime = 30 * 24 * time.Hour
	}

	now := time.Now().Unix()
	sess := RefreshSession{
		Token:         token,
		OriginalIAT:   now,
		MaxLifetimeAt: now + int64(maxLifetime.Seconds()),
	}

	if err := s.redisClient.SetRefreshSession(ctx, userID, sid, sess, ttl); err != nil {
		return "", fmt.Errorf("store refresh session: %w", err)
	}

	return token, nil
}

// randomToken returns n random bytes, base64url-encoded without padding.
func randomToken(n int) (string, error) {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// splitRefreshToken returns the session id of a "<sid>.<secret>" refresh
// token. A token without a well-formed sid (minted before per-login sessions)
// returns ok=false and maps to the legacy key. The sid comes from the client,
// so it is validated before it is used in a Redis key.
func splitRefreshToken(token string) (sid string, ok bool) {
	sid, _, found := strings.Cut(token, ".")
	if !found || !validSessionID(sid) {
		return "", false
	}
	return sid, true
}

// validSessionID accepts exactly what randomToken(16) produces: 22 base64url
// characters.
func validSessionID(sid string) bool {
	if len(sid) != 22 {
		return false
	}
	for _, c := range sid {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
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
