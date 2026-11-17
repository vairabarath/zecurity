package auth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/yourorg/ztna/controller/internal/identity"
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
	// MFAAt (PENDING-06) is when this session last passed MFA (Unix seconds;
	// omitted when it never did). Copied from the refresh session at every
	// mint, so it is at most one access-token lifetime behind the session.
	MFAAt int64 `json:"mfa_at,omitempty"`
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
	return s.IssueSessionWithAuth(ctx, userID, tenantID, role, email, generation, SessionAuth{})
}

// IssueSessionWithAuth is IssueSession for a sign-in whose authentication
// context is known (PENDING-06): the session records when it authenticated
// and, when the IdP proved MFA (M-3), when it passed MFA.
func (s *serviceImpl) IssueSessionWithAuth(ctx context.Context, userID, tenantID, role, email string, generation int, sa SessionAuth) (string, int64, string, error) {
	refreshToken, err := s.issueRefreshToken(ctx, userID, sa)
	if err != nil {
		return "", 0, "", err
	}
	sid, _ := splitRefreshToken(refreshToken)
	var mfaAt int64
	if !sa.MFAAt.IsZero() {
		mfaAt = sa.MFAAt.Unix()
	}
	accessToken, err := s.issueSessionAccessToken(userID, tenantID, role, email, generation, sid, mfaAt)
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
	err := s.redisClient.DeleteAllRefreshSessions(ctx, userID)
	// The user's devices end with their sessions (PENDING-09 C-1), even if
	// the Valkey delete failed: the intent is that every session is over.
	s.userSessionsEnded(ctx, userID, "sessions_revoked")
	return err
}

// VerifyAccessToken parses and verifies a Zecurity-issued JWT and returns
// the public claims view used by gRPC handlers.
func (s *serviceImpl) VerifyAccessToken(tokenStr string) (*AccessTokenClaims, error) {
	claims, err := s.verifyAccessToken(tokenStr)
	if err != nil {
		return nil, err
	}
	out := &AccessTokenClaims{
		UserID:     claims.Subject,
		TenantID:   claims.TenantID,
		Role:       claims.Role,
		Email:      claims.Email,
		Generation: claims.IdentityGeneration,
		SessionID:  claims.SessionID,
	}
	if claims.MFAAt != 0 {
		out.MFAAt = time.Unix(claims.MFAAt, 0)
	}
	if claims.ExpiresAt != nil {
		out.ExpiresAt = claims.ExpiresAt.Time
	}
	return out, nil
}

// issueAccessToken creates a signed short-lived JWT.
// exp = JWTAccessTTL (default 15 minutes) from now. Signed with HS256 using JWT_SECRET.
// Called by: CallbackHandler() in callback.go (Step 8), RefreshHandler() in refresh.go (Step 5).
func (s *serviceImpl) issueAccessToken(userID, tenantID, role, email string, generation int) (string, error) {
	return s.issueSessionAccessToken(userID, tenantID, role, email, generation, "", 0)
}

// issueSessionAccessToken is issueAccessToken with the sid claim set, and
// mfa_at when the session has passed MFA (0 = it has not).
func (s *serviceImpl) issueSessionAccessToken(userID, tenantID, role, email string, generation int, sid string, mfaAt int64) (string, error) {
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
		MFAAt:              mfaAt,
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
func (s *serviceImpl) issueRefreshToken(ctx context.Context, userID string, sa SessionAuth) (string, error) {
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
		AuthAt:        now,
	}

	if err := s.redisClient.SetRefreshSession(ctx, userID, sid, sess, ttl); err != nil {
		return "", fmt.Errorf("store refresh session: %w", err)
	}

	// M-3: MFA proven by the IdP at this sign-in marks the session.
	if !sa.MFAAt.IsZero() {
		if err := s.redisClient.SetSessionMFA(ctx, userID, sid, sa.MFAAt, maxLifetime); err != nil {
			return "", err
		}
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

// ── PENDING-06: MFA on a session ─────────────────────────────────────────────

// SessionAuth is what a sign-in proved beyond identity. MFAAt is non-zero
// when the IdP's verified token carried an MFA method (amr), see MFAFromAMR.
type SessionAuth struct {
	MFAAt time.Time
}

// mfaAMR are the RFC 8176 authentication method references that prove more
// than a password was used. "mfa" is the IdP's own summary (Entra, Okta);
// the rest are second factors or phishing-resistant single factors that
// IdPs report without "mfa".
var mfaAMR = map[string]struct{}{
	"mfa": {}, "otp": {}, "hwk": {}, "swk": {}, "fpt": {}, "face": {},
	"iris": {}, "retina": {}, "vbm": {}, "sc": {}, "sms": {}, "tel": {},
}

// MFAFromAMR reports whether a verified ID token's amr claim proves MFA
// (M-3). Only the amr claim counts; acr values are IdP-specific and are not
// interpreted.
func MFAFromAMR(amr []string) bool {
	for _, m := range amr {
		if _, ok := mfaAMR[strings.ToLower(strings.TrimSpace(m))]; ok {
			return true
		}
	}
	return false
}

// IdPMFAAt is when the IdP last performed MFA for this sign-in: its
// auth_time, when the verified amr proves MFA. A silent sign-in on a live IdP
// session can carry an amr from long ago, so the mark is never "now": a
// missing auth_time (0) means the IdP's MFA does not count, and an auth_time
// in the future (clock skew) is capped at `now`. The policy compiler's MFA
// freshness window then judges the real age of the MFA.
func IdPMFAAt(amr []string, authTime int64, now time.Time) time.Time {
	if !MFAFromAMR(amr) || authTime <= 0 {
		return time.Time{}
	}
	at := time.Unix(authTime, 0)
	if at.After(now) {
		at = now
	}
	return at
}

// SessionAuthFromAMR builds the SessionAuth for a sign-in at `now` from the
// IdP's amr and auth_time (see IdPMFAAt).
func SessionAuthFromAMR(amr []string, authTime int64, now time.Time) SessionAuth {
	return SessionAuth{MFAAt: IdPMFAAt(amr, authTime, now)}
}

// StampSessionMFA records that the claims' session passed MFA at `at` and
// mints a fresh access token carrying the mark.
func (s *serviceImpl) StampSessionMFA(ctx context.Context, claims *AccessTokenClaims, at time.Time) (string, int64, error) {
	if claims == nil || claims.UserID == "" || !validSessionID(claims.SessionID) {
		return "", 0, ErrSessionNotFound
	}
	stored, found, err := s.redisClient.GetRefreshSession(ctx, claims.UserID, claims.SessionID)
	if err != nil {
		return "", 0, err
	}
	if !found {
		return "", 0, ErrSessionNotFound
	}
	// This mints a new access token, so it must not outlive a suspension or
	// a session revocation: the same lifecycle and generation check as
	// RefreshSession. Skipped only without a pool (unit tests).
	if s.cfg.Pool != nil {
		var status string
		var currentGen int
		if err := s.cfg.Pool.QueryRow(ctx,
			`SELECT status, identity_generation FROM users WHERE id = $1`, claims.UserID,
		).Scan(&status, &currentGen); err != nil {
			return "", 0, ErrSessionNotFound
		}
		if identity.CheckLifecycle(status) != nil || identity.CheckGeneration(claims.Generation, currentGen) != nil {
			return "", 0, ErrSessionNotFound
		}
	}
	// The mark lives as long as the session can (its absolute cap).
	ttl := s.refreshTTL()
	if stored.MaxLifetimeAt != 0 {
		ttl = time.Until(time.Unix(stored.MaxLifetimeAt, 0))
		if ttl <= 0 {
			return "", 0, ErrSessionNotFound
		}
	}
	if err := s.redisClient.SetSessionMFA(ctx, claims.UserID, claims.SessionID, at, ttl); err != nil {
		return "", 0, err
	}
	token, err := s.issueSessionAccessToken(claims.UserID, claims.TenantID, claims.Role, claims.Email, claims.Generation, claims.SessionID, at.Unix())
	if err != nil {
		return "", 0, err
	}
	accessTTL, perr := time.ParseDuration(s.cfg.JWTAccessTTL)
	if perr != nil {
		accessTTL = 15 * time.Minute
	}
	return token, int64(accessTTL.Seconds()), nil
}

// ClearUserSessionMFA removes the MFA mark from every session of the user.
func (s *serviceImpl) ClearUserSessionMFA(ctx context.Context, userID string) error {
	return s.redisClient.ClearAllSessionMFA(ctx, userID)
}

// SignInMFALabel is the sign-in audit detail for a session's MFA state:
// "idp" when the IdP proved MFA, "none" otherwise. Never the amr list itself.
func SignInMFALabel(sa SessionAuth) string {
	if sa.MFAAt.IsZero() {
		return "none"
	}
	return "idp"
}
