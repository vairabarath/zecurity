package auth

import (
	"context"
	"net/http"

	"github.com/yourorg/ztna/controller/graph/model"
)

// Service is the contract between Member 4 (consumer)
// and Member 2 (implementor).
// Member 4 depends on this interface.
// Member 2 writes the concrete implementation.
// Neither touches the other's files.
type Service interface {
	// InitiateAuth builds the IdP redirect URL with PKCE.
	// Called by the initiateAuth GraphQL mutation resolver.
	// workspaceName is optional — set during signup flow, empty during normal login.
	// connectionID is optional — the discovery-API connection id; when set it
	// selects that exact connection (the only selector), else the platform IdP
	// for `provider` is resolved.
	InitiateAuth(ctx context.Context, provider string, workspaceName, connectionID *string) (*model.AuthInitPayload, error)

	// CallbackHandler handles GET /auth/callback.
	// Google redirects here after user authenticates.
	// Verifies state, exchanges code, calls Bootstrap,
	// issues JWT, sets refresh cookie, redirects React.
	CallbackHandler() http.Handler

	// DiscoveryHandler handles GET /workspaces/{slug}/auth — the public,
	// read-only login-discovery endpoint. Returns the IdPs a workspace can use
	// (Enterprise first, Bootstrap fallback). PENDING-04 / ADR-024.
	DiscoveryHandler() http.Handler

	// RefreshHandler handles POST /auth/refresh.
	// Reads httpOnly refresh cookie, issues new JWT.
	RefreshHandler() http.Handler

	// LogoutHandler handles POST /auth/logout. Invalidates the caller's
	// refresh token in Redis so subsequent /auth/refresh calls fail. For
	// browser callers it also clears the refresh_token cookie so the
	// browser doesn't keep re-presenting a dead token.
	LogoutHandler() http.Handler

	// RefreshSession exchanges an access token (expired is fine; signature
	// verified) plus the single-use refresh token for a fresh pair, rotating
	// the refresh token. The core of /auth/refresh, also behind the
	// ClientService.RefreshSession RPC the Rust client uses over its pinned
	// TLS channel. Errors are *SessionError; Dead() means sign in again.
	RefreshSession(ctx context.Context, accessToken, refreshToken string) (newAccessToken, newRefreshToken string, err error)

	// EndSession deletes the session the access token names. Idempotent and
	// silent. The core of /auth/logout and of ClientService.Logout.
	EndSession(ctx context.Context, accessToken string)

	// ── Sprint 7: ClientService primitives ──────────────────────────────────
	// These power the Rust CLI's TokenExchange / EnrollDevice gRPC handlers,
	// which run their own PKCE flow against a localhost redirect URI rather
	// than the controller's web callback.

	// (Login now flows through provider adapters selected per connection —
	// see idp_adapter.go / providers.IdentityProvider — rather than a single
	// service method.)

	// IssueSession starts one login: it stores a new refresh session and
	// returns an access JWT bound to it (sid claim), the access TTL in seconds
	// (so callers can populate expires_in) and the raw refresh token.
	// generation is stamped as the "gen" claim for session-generation revocation
	// (PENDING-04 Phase 5). Sessions are per login: a new one never displaces
	// the user's other sessions (admin UI, client daemon).
	IssueSession(ctx context.Context, userID, tenantID, role, email string, generation int) (accessToken string, expiresIn int64, refreshToken string, err error)

	// InvalidateUserSessions deletes all of the user's server-side refresh sessions so a
	// generation bump (identity.Revoker) takes effect immediately rather than at
	// the current access token's expiry. Satisfies identity.SessionInvalidator.
	InvalidateUserSessions(ctx context.Context, userID string) error

	// VerifyAccessToken parses and verifies a Zecurity JWT and returns the
	// embedded identity claims. Returns an error if the token is invalid,
	// expired, or signed with a different key/issuer.
	VerifyAccessToken(token string) (*AccessTokenClaims, error)
}

// AccessTokenClaims is the public view of the JWT payload, returned by
// VerifyAccessToken. Mirrors the internal jwtClaims but uses public field
// names so callers do not depend on the internal session.go struct.
type AccessTokenClaims struct {
	UserID   string
	TenantID string
	Role     string
	Email    string
	// Generation is the users.identity_generation the token was minted under
	// (the "gen" claim; 0 for tokens that predate generations). Compared with
	// the current value before issuing device certificates (certificate audit
	// bugs 4/5), as /auth/refresh does.
	Generation int
}
