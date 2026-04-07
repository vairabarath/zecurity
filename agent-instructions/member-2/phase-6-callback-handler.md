# Phase 6 — The Callback Handler

This is the most complex file. Every step is sequential and must succeed for the
flow to complete. If any step fails, the user is redirected to the login page
with an error — never left at a broken state.

---

## Bootstrap Stub (Write This First)

Member 2 writes this stub in `internal/bootstrap/bootstrap.go` temporarily
until Member 3 implements the real one. When Member 3 is done, Member 3
replaces the stub body. Member 2's `callback.go` does not change at all.

**Path:** `internal/bootstrap/bootstrap.go`

```go
package bootstrap

import "context"

// Result holds the output of the bootstrap transaction.
// Called by: CallbackHandler() in auth/callback.go (Step 7)
// Implemented by: Member 3 (real transaction)
// Current: stub returning test data
type Result struct {
    TenantID string
    UserID   string
    Role     string
}

// Bootstrap finds or creates the user+tenant for a given identity.
// Called by: CallbackHandler() in auth/callback.go (Step 7)
// TODO: Member 3 replaces this with the real transaction
func Bootstrap(ctx context.Context,
    email, provider, providerSub, name string,
) (*Result, error) {
    // TODO: Member 3 replaces this with the real transaction
    return &Result{
        TenantID: "00000000-0000-0000-0000-000000000001",
        UserID:   "00000000-0000-0000-0000-000000000002",
        Role:     "admin",
    }, nil
}
```

---

## File: `internal/auth/callback.go`

**Path:** `internal/auth/callback.go`

```go
package auth

import (
    "fmt"
    "net/http"
    "time"

    "github.com/yourorg/ztna/controller/internal/bootstrap"
)

// CallbackHandler handles GET /auth/callback.
// Registered as a public route in main.go (no auth middleware).
// Called by: main.go route registration (Member 4 wires this up)
//
// Full sequence:
//   1. Read code + state from URL query params
//   2. Verify state HMAC (CSRF protection)                    → calls oidc.go → verifySignedState()
//   3. Retrieve and delete code_verifier from Redis (single use) → calls redis.go → GetAndDeletePKCEState()
//   4. Exchange code for Google tokens (server-to-server)      → calls exchange.go → exchangeCodeForTokens()
//   5. Verify id_token (signature, aud, iss, exp, email_verified) → calls idtoken.go → VerifyGoogleIDToken()
//   6. Extract identity claims (email, sub, name)
//   7. Call bootstrap.Bootstrap() → get tenant_id + user_id + role → calls bootstrap/bootstrap.go
//   8. Issue access JWT                                         → calls session.go → issueAccessToken()
//   9. Issue refresh token → store in Redis → set httpOnly cookie → calls session.go → issueRefreshToken()
//  10. Redirect React to /auth/callback#token=<JWT>
//
// On any failure: redirect to /login?error=<reason>
// Never show raw error details to the browser.
func (s *serviceImpl) CallbackHandler() http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        ctx := r.Context()

        // Helper: redirect to login with error code
        fail := func(reason string) {
            http.Redirect(w, r,
                "/login?error="+reason,
                http.StatusFound)
        }

        // Step 1 — Read query params
        code := r.URL.Query().Get("code")
        state := r.URL.Query().Get("state")

        if code == "" || state == "" {
            fail("missing_params")
            return
        }

        // Step 2 — Verify state HMAC
        // This prevents CSRF: if an attacker crafts a callback URL
        // with a forged state, the HMAC check fails here.
        // Called: oidc.go → verifySignedState()
        if err := verifySignedState(state, s.cfg.JWTSecret); err != nil {
            fail("invalid_state")
            return
        }

        // Step 3 — Retrieve and delete code_verifier from Redis
        // Single use: GetAndDeletePKCEState deletes the key atomically.
        // If this returns false, the state expired or was already used.
        // Called: redis.go → GetAndDeletePKCEState()
        codeVerifier, found, err := s.redisClient.GetAndDeletePKCEState(ctx, state)
        if err != nil {
            fail("server_error")
            return
        }
        if !found {
            // State expired (>5 min) or replay attempt
            fail("state_expired")
            return
        }

        // Step 4 — Exchange code for tokens (server-to-server)
        // client_secret is used here — never exposed to the browser
        // Called: exchange.go → exchangeCodeForTokens()
        tokenResp, err := s.exchangeCodeForTokens(ctx, code, codeVerifier)
        if err != nil {
            fail("token_exchange_failed")
            return
        }

        // Step 5 — Verify id_token
        // ALL six checks must pass (see idtoken.go)
        // Called: idtoken.go → VerifyGoogleIDToken()
        googleClaims, err := VerifyGoogleIDToken(ctx,
            tokenResp.IDToken, s.cfg.GoogleClientID)
        if err != nil {
            fail("invalid_id_token")
            return
        }

        // Step 6 — Extract identity
        // Use Sub (provider_sub) as the identity anchor, NOT email.
        // Emails can change. Sub is immutable.
        email := googleClaims.Email
        providerSub := googleClaims.Sub
        name := googleClaims.Name
        if name == "" {
            name = email // fallback if Google doesn't return name
        }

        // Step 7 — Bootstrap
        // This is a direct Go function call — no network, no HTTP.
        // Member 3 implements this function.
        // Until Member 3 finishes, the stub returns test data.
        // Called: bootstrap/bootstrap.go → Bootstrap()
        result, err := bootstrap.Bootstrap(ctx,
            email, "google", providerSub, name)
        if err != nil {
            fail("bootstrap_failed")
            return
        }

        // Step 8 — Issue access JWT
        // Called: session.go → issueAccessToken()
        accessToken, err := s.issueAccessToken(
            result.UserID, result.TenantID, result.Role)
        if err != nil {
            fail("token_issue_failed")
            return
        }

        // Step 9 — Issue refresh token
        // Called: session.go → issueRefreshToken()
        refreshToken, err := s.issueRefreshToken(ctx, result.UserID)
        if err != nil {
            fail("refresh_issue_failed")
            return
        }

        // Set refresh token as httpOnly cookie
        // httpOnly: JavaScript cannot read this — XSS cannot steal it
        // SameSite=Strict: not sent on cross-site requests — CSRF protection
        // Secure: only sent over HTTPS (set to false in development)
        ttl, _ := time.ParseDuration(s.cfg.JWTRefreshTTL)
        http.SetCookie(w, &http.Cookie{
            Name:     "refresh_token",
            Value:    refreshToken,
            Path:     "/auth/refresh",    // only sent to the refresh endpoint
            HttpOnly: true,
            SameSite: http.SameSiteStrictMode,
            Secure:   true,              // set false in development via config
            MaxAge:   int(ttl.Seconds()),
        })

        // Step 10 — Redirect React with JWT in URL fragment
        // Fragment (#token=...) is NEVER sent to the server.
        // Only the browser reads it. React extracts it from window.location.hash
        // and stores in memory (Zustand), then clears the hash from the URL.
        http.Redirect(w, r,
            "/#token="+accessToken,
            http.StatusFound)
    })
}
```

---

## Phase 6 Checklist

```
✓ missing code or state → redirect /login?error=missing_params
✓ invalid state HMAC → redirect /login?error=invalid_state
✓ Redis state not found → redirect /login?error=state_expired
✓ Google token exchange fails → redirect /login?error=token_exchange_failed
✓ id_token verification fails → redirect /login?error=invalid_id_token
✓ bootstrap fails → redirect /login?error=bootstrap_failed
✓ refresh token set as httpOnly SameSite=Strict cookie
✓ cookie Path = "/auth/refresh" (not "/" — reduces exposure)
✓ React redirected to /#token=<JWT> (hash fragment)
```
