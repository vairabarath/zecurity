package auth

import (
	"context"
	"net/http"
	"time"
)

var exchangeCodeForTokensHook = func(s *serviceImpl, ctx context.Context, code, codeVerifier string) (*GoogleTokenResponse, error) {
	return s.exchangeCodeForTokens(ctx, code, codeVerifier)
}

var verifyGoogleIDTokenHook = VerifyGoogleIDToken

// CallbackHandler handles GET /auth/callback.
// Registered as a public route in main.go (no auth middleware).
// Called by: main.go route registration (Member 4 wires this: mux.Handle("/auth/callback", authSvc.CallbackHandler())).
//
// Full sequence:
//
//  1. Read code + state from URL query params
//  2. Verify state HMAC (CSRF protection)                       → calls oidc.go → verifySignedState()
//  3. Retrieve and delete code_verifier from Redis (single use)  → calls redis.go → GetAndDeletePKCEState()
//  4. Exchange code for Google tokens (server-to-server)          → calls exchange.go → exchangeCodeForTokens()
//  5. Verify id_token (signature, aud, iss, exp, email_verified)  → calls idtoken.go → VerifyGoogleIDToken()
//  6. Extract identity claims (email, sub, name)
//  7. Call bootstrap.Bootstrap() → get tenant_id + user_id + role → calls bootstrap/bootstrap.go
//  8. Issue access JWT                                             → calls session.go → issueAccessToken()
//  9. Issue refresh token → store in Redis → set httpOnly cookie   → calls session.go → issueRefreshToken()
//  10. Redirect React to /auth/callback#token=<JWT>
//
// On any failure: redirect to /login?error=<reason>.
// Never show raw error details to the browser.
func (s *serviceImpl) CallbackHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()

		// fail redirects to the frontend login page with an error code — never exposes internal details.
		// Uses AllowedOrigin so the redirect goes to the React app, not back to the Go server.
		fail := func(reason string) {
			http.Redirect(w, r, s.cfg.AllowedOrigin+"/login?error="+reason, http.StatusFound)
		}

		// Step 1 — Read query params sent by Google after user authenticates.
		code := r.URL.Query().Get("code")
		state := r.URL.Query().Get("state")

		if code == "" || state == "" {
			fail("missing_params")
			return
		}

		// Step 2 — Verify state HMAC.
		// Prevents CSRF: if an attacker crafts a callback URL with a forged state,
		// the HMAC check fails here.
		// Called: oidc.go → verifySignedState()
		if err := verifySignedState(state, s.cfg.JWTSecret); err != nil {
			fail("invalid_state")
			return
		}

		// Step 3 — Retrieve and delete code_verifier from Redis.
		// Single use: GetAndDeletePKCEState deletes the key atomically.
		// If this returns false, the state expired (>5min) or was already used (replay).
		// Called: redis.go → GetAndDeletePKCEState()
		codeVerifier, workspaceName, found, err := s.redisClient.GetAndDeletePKCEState(ctx, state)
		if err != nil {
			fail("server_error")
			return
		}
		if !found {
			fail("state_expired")
			return
		}

		// Step 4 — Exchange code for tokens (server-to-server).
		// client_secret is used here — never exposed to the browser.
		// Called: exchange.go → exchangeCodeForTokens()
		tokenResp, err := exchangeCodeForTokensHook(s, ctx, code, codeVerifier)
		if err != nil {
			fail("token_exchange_failed")
			return
		}

		// Step 5 — Verify id_token.
		// ALL six checks must pass (see idtoken.go): signature, aud, iss, exp, email_verified, sub.
		// Called: idtoken.go → VerifyGoogleIDToken()
		googleClaims, err := verifyGoogleIDTokenHook(ctx, tokenResp.IDToken, s.cfg.GoogleClientID)
		if err != nil {
			fail("invalid_id_token")
			return
		}

		// Step 6 — Extract identity.
		// Use Sub (provider_sub) as the identity anchor, NOT email.
		// Emails can change. Sub is immutable.
		email := googleClaims.Email
		providerSub := googleClaims.Sub
		name := googleClaims.Name
		if name == "" {
			name = email // fallback if Google doesn't return name
		}

		// Step 7 — Bootstrap.
		// Direct Go function call — no network, no HTTP.
		// Member 3 implements the real transaction. Until then, the stub returns test data.
		// Use workspaceName if provided (signup flow), otherwise fall back to Google name (returning login).
		// Called: bootstrap/bootstrap.go → (*bootstrap.Service).Bootstrap()
		bootstrapName := name
		if workspaceName != "" {
			bootstrapName = workspaceName
		}
		result, err := s.bootstrapSvc.Bootstrap(ctx, email, "google", providerSub, bootstrapName)
		if err != nil {
			fail("bootstrap_failed")
			return
		}

		// Step 8 — Issue access JWT.
		// Called: session.go → issueAccessToken()
		accessToken, err := s.issueAccessToken(result.UserID, result.TenantID, result.Role, email)
		if err != nil {
			fail("token_issue_failed")
			return
		}

		// Step 9 — Issue refresh token and set as httpOnly cookie.
		// Called: session.go → issueRefreshToken()
		refreshToken, err := s.issueRefreshToken(ctx, result.UserID)
		if err != nil {
			fail("refresh_issue_failed")
			return
		}

		// Set refresh token as httpOnly cookie.
		// httpOnly: JavaScript cannot read this — XSS cannot steal it.
		// SameSite=Strict: not sent on cross-site requests — CSRF protection.
		// Secure: only sent over HTTPS (set to false in development).
		// Path="/auth/refresh": cookie only sent to the refresh endpoint, reduces exposure.
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

		// Step 10 — Redirect React with JWT in URL fragment.
		// Fragment (#token=...) is NEVER sent to the server.
		// Only the browser reads it. React extracts it from window.location.hash
		// and stores in memory (Zustand), then clears the hash from the URL.
		// We redirect to the frontend callback route so React can process the fragment.
		http.Redirect(w, r, s.cfg.AllowedOrigin+"/auth/callback#token="+accessToken, http.StatusFound)
	})
}
