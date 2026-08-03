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

	// ── Sprint 7: ClientService primitives ──────────────────────────────────
	// These power the Rust CLI's TokenExchange / EnrollDevice gRPC handlers,
	// which run their own PKCE flow against a localhost redirect URI rather
	// than the controller's web callback.

	// (Login now flows through provider adapters selected per connection —
	// see idp_adapter.go / providers.IdentityProvider — rather than a single
	// service method.)

	// IssueAccessToken signs and returns a Zecurity access JWT for the given
	// user, plus its TTL in seconds (so callers can populate expires_in).
	IssueAccessToken(userID, tenantID, role, email string) (token string, expiresIn int64, err error)

	// IssueRefreshToken generates a fresh refresh token, stores it in Redis,
	// and returns the raw value.
	IssueRefreshToken(ctx context.Context, userID string) (string, error)

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
}
