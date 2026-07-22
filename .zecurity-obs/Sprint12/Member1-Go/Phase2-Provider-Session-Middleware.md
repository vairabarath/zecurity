---
type: phase
member: M1
sprint: 12
phase: 2
title: Provider Session + RequireProvider Middleware + /provider Route Group
status: done
depends_on:
  - Sprint12/Member1-Go/Phase1-Provider-Data-Model-Authz
tags:
  - go
  - auth
  - oidc
  - middleware
  - provider
  - pending-07a
---

# Phase 2 — Provider Session + RequireProvider Middleware + /provider Route Group

> Depends on Phase 1 (the `provider_users` store must exist).
> Establishes the authenticated provider surface that M2 Phase 2 re-homes relay creation onto.

## Goal

Give provider staff an authenticated, **mechanically isolated** session and gate the `/provider`
API on it:

1. Issue a provider JWT with a distinct **audience (`aud=provider`)** by reusing the existing Google
   OIDC exchange — no new IdP.
2. `RequireProvider` middleware: enforce `aud=provider`, check the `provider_users` allowlist +
   `disabled_at`, inject a provider identity context, and **never** call `WorkspaceGuard`.
3. A `/provider` route group skeleton stacked `AuthMiddleware(provider) → RequireProvider`.

**Mechanical isolation requirement:** a tenant JWT must be rejected by `RequireProvider` (wrong/absent
`aud`), and a provider JWT must be rejected by the tenant `AuthMiddleware` (it lacks `tenant_id`,
which tenant auth requires at `middleware/auth.go:62`). Assert both in tests.

## Files

| File | Change |
|------|--------|
| `controller/internal/auth/session.go` (or a new `provider_session.go`) | issue provider JWT (`aud=provider`, provider claims, no `tenant_id`) |
| `controller/internal/auth/callback.go` (or new `provider_callback.go`) | `/provider/auth/callback` — Google exchange gated by the allowlist |
| `controller/internal/middleware/provider.go` (new) | `RequireProvider` middleware |
| `controller/internal/provider/context.go` (new, optional) | provider identity context accessors |
| `controller/cmd/server/main.go` | register provider callback + `/provider` route group skeleton |

## Provider session (session.go / provider_session.go)

```go
// Provider JWT claims — deliberately NO tenant_id, and aud=provider.
type providerClaims struct {
    ProviderUserID string `json:"provider_user_id"` // also RegisteredClaims.Subject
    Role           string `json:"role"`
    Email          string `json:"email"`
    jwt.RegisteredClaims                              // Issuer=ControllerIssuer, Audience=["provider"], HS256
}

// IssueProviderToken signs a provider-scoped JWT (reuses JWT_SECRET + HS256, mirrors
// issueAccessToken in session.go but with the provider audience + claim shape).
func (s *serviceImpl) IssueProviderToken(userID, role, email string) (token string, expiresIn int64, err error)
```

- Reuse the Google code exchange already in `callback.go` (`exchangeCodeForTokens`,
  `VerifyGoogleIDToken`). Only the **issuance step** differs (provider claims + audience + a distinct
  cookie name for the refresh token, e.g. `provider_refresh_token`).

## Provider callback (callback.go / provider_callback.go)

- `GET /provider/auth/callback`: run the standard Google exchange, then
  `providerStore.GetByEmail(email)`. If not found or disabled → fail (redirect/401), **do not**
  bootstrap or JIT-create. On success → `IssueProviderToken` + set the provider refresh cookie.

## middleware/provider.go

```go
// RequireProvider verifies a provider JWT and injects the provider identity.
//   - Parse HS256 with JWT_SECRET; enforce Issuer=ControllerIssuer AND Audience contains "provider".
//   - Reject if provider_users lookup (by sub/email) fails or disabled_at is set.
//   - Inject provider.Actor into ctx for downstream handlers + Authz.
//   - NEVER calls WorkspaceGuard — provider identity has no tenant.
func RequireProvider(secret string, store *provider.ProviderStore) func(http.Handler) http.Handler
```

- Model the context injection on the existing `tenant`/`spiffe` context pattern.
- On failure return 401/403 JSON consistent with `writeJSON401` in `middleware/auth.go`.

## main.go (route group skeleton)

- Register `GET /provider/auth/callback` as a **public** route (no middleware — it is the login).
- Create the `/provider` group behind `AuthMiddleware(provider) → RequireProvider`. Leave it empty
  except a trivial `GET /provider/me` (returns the actor) so the boundary is testable before M2
  hangs relay routes off it.

## Config

```
PROVIDER_BOOTSTRAP_EMAILS   comma-separated corp emails seeded as super-admin (Phase 1)
# Reuses existing GOOGLE_CLIENT_ID / GOOGLE_CLIENT_SECRET / JWT_SECRET.
```

## Tests

- Provider JWT passes `RequireProvider`; tenant JWT is rejected (missing/wrong `aud`).
- Provider JWT is rejected by tenant `AuthMiddleware` (missing `tenant_id`).
- Non-allowlisted email → callback fails; disabled user → `RequireProvider` rejects.

## Build Check

```bash
cd controller && go build ./...
cd controller && go test ./internal/middleware/... ./internal/auth/...
```

## Implementation Checklist

- [x] **M1-B1** provider JWT issuance (`aud=provider`) + `/provider/auth/callback` gated by allowlist
- [x] **M1-B2** `controller/internal/middleware/provider.go` — `RequireProvider`
- [x] **M1-B3** `controller/cmd/server/main.go` — `/provider` route group skeleton + `GET /provider/me`
- [x] **Build gate:** `cd controller && go build ./...`

## Post-Phase Fixes

_None yet._
