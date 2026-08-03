package auth

import (
	"errors"
	"net/http"
	"time"

	"github.com/yourorg/ztna/controller/internal/idp"
)

// CallbackHandler handles GET /auth/callback (registered public in main.go).
//
// Trust model (ADR-024): the ONLY values taken from the browser are `code` and
// `state`. Everything that decides what happens — the PKCE verifier, the OIDC
// nonce, and the connection_id that selects the adapter — comes from the Redis
// scratchpad keyed by the HMAC-verified state. The connection is re-resolved
// server-side from the stored connection_id and fails closed if it was deleted
// or disabled during the redirect window.
//
// Sequence:
//  1. Read code + state.
//  2. Verify state HMAC (CSRF).
//  3. Retrieve + delete the login scratchpad (single-use).
//  4. Re-resolve the connection from the stored connection_id (fail closed).
//  5. Select the adapter via the factory (single switch point).
//  6. adapter.Authenticate → AuthenticationContext (exchange + verify + nonce).
//     7–10. Bootstrap → issue JWT + refresh cookie → redirect the SPA.
func (s *serviceImpl) CallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// fail redirects to the SPA login with an error code — never leaks internals.
		fail := func(reason string) {
			http.Redirect(w, r, s.cfg.AllowedOrigin+"/login?error="+reason, http.StatusFound)
		}

		// Step 1 — the only two values trusted from the browser.
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")
		if code == "" || state == "" {
			fail("missing_params")
			return
		}

		// Step 2 — verify state HMAC (CSRF).
		if err := verifySignedState(state, s.cfg.JWTSecret); err != nil {
			fail("invalid_state")
			return
		}

		// Step 3 — retrieve + delete the scratchpad (single-use; replay-safe).
		pkce, found, err := s.redisClient.GetAndDeletePKCEState(ctx, state)
		if err != nil {
			fail("server_error")
			return
		}
		if !found {
			fail("state_expired")
			return
		}

		// Step 4 — re-resolve the connection from the STORED connection_id.
		// Fail closed if it was deleted or disabled mid-login.
		conn, err := s.idpStore.GetByID(ctx, pkce.ConnectionID)
		if err != nil {
			if errors.Is(err, idp.ErrConnectionNotFound) {
				fail("authentication_failed") // connection deleted during redirect window
				return
			}
			fail("server_error")
			return
		}
		if conn.Status != "active" {
			fail("authentication_failed") // connection disabled during redirect window
			return
		}

		// Step 5 — select the adapter (the single provider switch point).
		adapter, err := providerForFn(conn, s.googleCreds())
		if err != nil {
			fail("authentication_failed")
			return
		}

		// Step 6 — exchange the code + verify the token (incl. nonce). Uses only
		// the server-side verifier/nonce and the cryptographically verified token.
		authCtx, err := adapter.Authenticate(ctx, code, pkce.CodeVerifier, s.cfg.RedirectURI, pkce.Nonce)
		if err != nil {
			fail("authentication_failed")
			return
		}

		// Step 7 — identity anchor is Subject, never email.
		email := authCtx.Email
		name := authCtx.Name
		if name == "" {
			name = email
		}
		bootstrapName := name
		if pkce.WorkspaceName != "" {
			bootstrapName = pkce.WorkspaceName
		}

		// Step 8 — Bootstrap (existing login flow; Phase 5 replaces its internals
		// with the identity pipeline). Provider/Subject come from the adapter.
		result, err := s.bootstrapSvc.Bootstrap(ctx, email, authCtx.Provider, authCtx.Subject, bootstrapName)
		if err != nil {
			fail("bootstrap_failed")
			return
		}

		// Step 9 — issue access JWT.
		accessToken, err := s.issueAccessToken(result.UserID, result.TenantID, result.Role, email)
		if err != nil {
			fail("token_issue_failed")
			return
		}

		// Step 10 — issue refresh token as an httpOnly cookie.
		refreshToken, err := s.issueRefreshToken(ctx, result.UserID)
		if err != nil {
			fail("refresh_issue_failed")
			return
		}
		ttl, _ := time.ParseDuration(s.cfg.JWTRefreshTTL)
		http.SetCookie(w, &http.Cookie{
			Name:     "refresh_token",
			Value:    refreshToken,
			Path:     "/auth/refresh",
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   true,
			MaxAge:   int(ttl.Seconds()),
		})

		// Redirect the SPA with the JWT in the URL fragment (never sent to a server).
		http.Redirect(w, r, s.cfg.AllowedOrigin+"/auth/callback#token="+accessToken, http.StatusFound)
	})
}
