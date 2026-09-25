---
type: phase
member: M2
person: Barath
sprint: 21
phase: 1
execution: H
title: Provider Identity Hardening (D-16)
status: planned
depends_on: []
schema_change: true   # [schema: reset DB] — 037_provider_identity_binding.sql
tags:
  - go
  - provider
  - auth
  - jwt
  - security
  - provider-dashboard
---

# Phase 1 (H) — Provider Identity Hardening (D-16)

> **Decision Record:** D-16, with consequence #5 ("new provider-signing-key configuration, provider-specific verification in `VerifyProviderToken` / `RequireProvider`, a generation column on `provider_users`, `sub` and `hd` columns or checks, a logout endpoint; reconcile `PROVIDER_BOOTSTRAP_EMAILS` with `sub` binding").
> **Blocks:** M1-R3 (route wiring) and M1-P (console login). Land it early.

## Problem (verified)

- **Shared signing key.** Provider JWTs are HS256-signed with the **tenant** `JWT_SECRET`:
  - `IssueProviderToken(pa.s.cfg.JWTSecret, …)` (`controller/internal/auth/provider_auth.go:133`);
  - `RequireProvider(mustEnv("JWT_SECRET"), …)` (`controller/cmd/server/main.go:354`).
  - The only thing separating provider and tenant tokens is `aud=provider`.
- **Shared issuer.** Provider tokens use `appmeta.ControllerIssuer = "zecurity-controller"` (`internal/provider/session.go`, `internal/appmeta/identity.go:7`), the same issuer as tenant tokens.
- **Email-only identity.** The callback gates on `store.GetByEmail(googleClaims.Email)` (`provider_auth.go` callback). No `sub` or `hd` is checked or stored.
  - `GoogleClaims` has `Sub` and `EmailVerified` but **no `hd` field** (`internal/auth/idtoken.go:20-27`).
  - `provider_users` has only `email, role, disabled_at` (`migrations/025_provider_users.sql`).
- **No logout or revocation.** A provider token is valid until its 15 min expiry (`main.go:342`). There is no logout endpoint and no generation check.
  - `RequireProvider` re-reads `provider_users` by **email** on every request (`internal/middleware/provider.go`). So disable and role changes already apply on the next request, but a leaked token can't be revoked.
- **Shared OAuth state key.** The provider OAuth `state` is signed with `JWT_SECRET` too (`provider_auth.go` `generateSignedState(pa.s.cfg.JWTSecret)` / `verifySignedState`).
- **JSON-only callback.** The callback returns the token as JSON, which is unusable for a browser SPA on a separate origin (D-18). The tenant flow redirects to `AllowedOrigin + "/auth/callback#token="` (`internal/auth/callback.go:152`).

## Goal

A provider session is:
- signed with a **provider-only** key and issuer;
- bound to one Google account (`sub`) in the corporate domain (`hd`);
- revocable at once through logout.

The console can complete login through a browser redirect.

## Files

| File | Change |
|------|--------|
| `controller/migrations/037_provider_identity_binding.sql` (new) | Columns on `provider_users` (H1) |
| `controller/internal/appmeta/identity.go` | `ProviderIssuer = "zecurity-provider"` |
| `controller/internal/provider/session.go` | Provider key + issuer + `gen` claim |
| `controller/internal/provider/store.go` | `GetByID`, `BindGoogleSub`, `BumpSessionGeneration`; `Disable` bumps generation; `ProviderUser` gains fields |
| `controller/internal/auth/idtoken.go` | `GoogleClaims.HD string \`json:"hd"\`` |
| `controller/internal/auth/provider_auth.go` | Provider key for state + token; `hd` + `sub` checks; redirect or JSON response |
| `controller/internal/middleware/provider.go` | Load by ID; `gen` / email / active checks |
| `controller/internal/provider/handler.go` | `Logout` handler |
| `controller/cmd/server/main.go` | New env, fail-fast validation, logout route, CORS for `/provider/*` |
| `controller/.env.example` | `PROVIDER_JWT_SECRET`, `PROVIDER_ALLOWED_HD`, `PROVIDER_CONSOLE_ORIGIN` (placeholders) |
| Tests | `internal/provider/session_test.go`, `internal/middleware/provider_test.go`, new `internal/auth/provider_auth_test.go`, `internal/provider/store_test.go` (DB) |

## Steps

### H1 — Schema (`037_provider_identity_binding.sql`) · `[schema: reset DB]`

```sql
-- Sprint 21 Phase H (D-16): bind provider users to a Google account and make sessions revocable.
ALTER TABLE provider_users
    ADD COLUMN google_sub         TEXT UNIQUE,            -- bound on first successful login; NULL until then
    ADD COLUMN hosted_domain      TEXT,                   -- Google `hd` observed at bind time
    ADD COLUMN session_generation BIGINT NOT NULL DEFAULT 1;
```

- **Never edit `025`.** Add the new file and label the PR `[schema: reset DB]`.

### H2 — Configuration (`main.go`)

- **`PROVIDER_JWT_SECRET`:** required. `log.Fatalf` if it is missing, under 32 bytes, or byte-equal to `JWT_SECRET`.
- **`PROVIDER_ALLOWED_HD`:** the corporate Google Workspace domain, stored lowercase.
  - Required unless `ENV=development`.
  - In development an empty value is allowed: the `hd` check is skipped and startup logs `provider hd binding DISABLED (ENV=development)`.
- **`PROVIDER_CONSOLE_ORIGIN`:** optional, e.g. `https://provider.zecurity.local`. It enables the redirect in H6 and CORS.
- **`appmeta.ProviderIssuer = "zecurity-provider"`.**

### H3 — Tokens (`session.go`)

```go
type ProviderClaims struct {
    Role  string `json:"role"`
    Email string `json:"email"`
    Gen   int64  `json:"gen"`       // must equal provider_users.session_generation
    jwt.RegisteredClaims             // Subject=provider_user_id, Issuer=ProviderIssuer, Audience=[provider]
}
func IssueProviderToken(providerKey []byte, userID, role, email string, gen int64, ttl time.Duration) (string, error)
func VerifyProviderToken(providerKey []byte, token string) (*ProviderClaims, error) // HS256 + ProviderIssuer + aud=provider + exp required
```

- A token signed with `JWT_SECRET`, or carrying `iss=zecurity-controller`, fails verification.
- Sign and verify the provider OAuth `state` with the provider key, not `JWT_SECRET`.

### H4 — Callback (`provider_auth.go`, `idtoken.go`)

After `VerifyIDToken`, which already requires `email_verified` and a non-empty `sub`:

1. If `PROVIDER_ALLOWED_HD != ""` and `lower(claims.HD) != PROVIDER_ALLOWED_HD` → 403 `{"error":"provider_domain_not_allowed"}`.
2. `user := store.GetByEmail(email)`, as today. Missing or disabled → 403 `not_a_provider_user`.
3. **`sub` binding:**
   - If `user.GoogleSub == ""`: run `BindGoogleSub(id, sub, hd)`:
     `UPDATE provider_users SET google_sub=$2, hosted_domain=$3, updated_at=NOW() WHERE id=$1 AND google_sub IS NULL`.
     - 0 rows → reload. If a concurrent login bound a **different** `sub` → 403.
     - A unique violation on `google_sub` (the same Google account bound to another row) → 403 `provider_identity_conflict`.
     - Write the audit entry `provider_user.bind_sub` (target = provider user, details = `hd`).
   - Else if `user.GoogleSub != claims.Sub` → 403 `{"error":"provider_identity_mismatch"}`, with no token.
4. Issue the token with `gen = user.SessionGeneration`.

`UpsertSuperAdmin` (bootstrap seeding) must **not** clear `google_sub` or reset `session_generation` on restart.

### H5 — Middleware + logout

- `RequireProvider(providerKey []byte, store)`:
  - verify the token;
  - `user := store.GetByID(claims.Subject)`; missing or disabled → 403;
  - `claims.Gen != user.SessionGeneration` → **401** `{"error":"provider session revoked"}`;
  - `claims.Email != user.Email` → 401;
  - the role still comes from the DB.
- `POST /provider/auth/logout` (behind `RequireProvider`):
  - `BumpSessionGeneration(actor.UserID)`, which makes **every** outstanding token for that user invalid;
  - writes the audit entry `provider_session.logout`;
  - returns 204.
- `Store.Disable` also increments `session_generation`.

### H6 — Console login contract

- **Callback response:** when `PROVIDER_CONSOLE_ORIGIN` is set, respond `302 Location: <origin>/auth/callback#token=<jwt>&expires_in=<s>`. Otherwise keep today's JSON body, so curl and the Sprint 20 runbook §5.3 still work.
- **CORS:** for `/provider/*`, answer preflight and add `Access-Control-Allow-Origin: <PROVIDER_CONSOLE_ORIGIN>` (exact match; never `*`), `Allow-Headers: Authorization, Content-Type`, `Allow-Methods: GET, POST, DELETE, OPTIONS`. No credentials (bearer only).
  - Other origins get no CORS headers.
  - Tenant routes are unchanged.
- Document the contract in the phase notes for M1-P: fragment parameters, a 401 means re-login, and the logout call.

### H7 — Tests

See **Tests** below. The auth boundary tests get shared review with M1.

## Invariants

1. **A tenant JWT can never authenticate a provider route.** This holds for a wrong key, a wrong issuer, and a missing `aud`.
2. After logout, **no** token issued before it is accepted, even an unexpired one.
3. Once bound, a provider user can authenticate only with the same Google `sub`.
4. With `PROVIDER_ALLOWED_HD` set, only that `hd` can log in, whatever the email is.
5. Role and disabled status are read from the DB on every request (unchanged).
6. No token, ID token or secret is logged.
7. No change to tenant auth, the tenant `JWT_SECRET` usage, or relay provisioning tokens.

## Tests

| Test | Kind | Asserts |
|------|------|---------|
| `TestVerifyProviderToken_RejectsTenantSecret` | unit | a token signed with `JWT_SECRET` → error |
| `TestVerifyProviderToken_RejectsControllerIssuer` | unit | `iss=zecurity-controller` → error |
| `TestVerifyProviderToken_RequiresAudAndExp` | unit | missing `aud` or `exp` → error; `alg=none` → error |
| `TestProviderConfig_FailsFast` | unit | missing, short, or equal-to-`JWT_SECRET` key → startup error |
| `TestRequireProvider_GenerationMismatch401` | DB | bump generation → the old token gets 401 |
| `TestRequireProvider_DisabledUser403` / `EmailMismatch401` | DB | as named |
| `TestLogout_RevokesAllTokens` | DB | two tokens → logout → both 401; an audit row exists |
| `TestCallback_BindsSubOnFirstLogin` | DB + fake IdP | `google_sub` set; `provider_user.bind_sub` audit row |
| `TestCallback_SubMismatch403` | DB + fake IdP | bound user + different `sub` → 403, no token |
| `TestCallback_HDNotAllowed403` / `HDSkippedInDev` | fake IdP | as named |
| `TestCallback_ConcurrentFirstLogin` | DB | two concurrent binds with different `sub` → exactly one succeeds |
| `TestUpsertSuperAdmin_PreservesBinding` | DB | re-seeding keeps `google_sub` and `session_generation` |
| `TestCallback_RedirectsToConsoleOrigin` / `JSONWithoutOrigin` | unit | H6 |
| `TestProviderCORS_OnlyConsoleOrigin` | unit | exact origin echoed; others get no header; tenant routes unaffected |

The existing `internal/provider/*_test.go`, `internal/middleware/provider_test.go` and `internal/relay/admin_handler*_test.go` suites must stay green, updated only for the new signatures.

## Acceptance Criteria

- [ ] AT-H.1 … AT-H.9 in [[Sprint21/Acceptance-Test-Plan]] pass.
- [ ] Live: from a reset dev DB, bootstrap login binds `sub`, the console receives the token by redirect, logout invalidates it, and a tenant token gets 401 on `/provider/me`.

## Build Check

```bash
cd controller && docker compose down -v && docker compose up -d   # schema change
cd controller && go build ./... && go vet ./... && go test ./internal/provider/... ./internal/middleware/... ./internal/auth/... ./internal/relay/...
```

## Implementation Checklist

- [ ] H1 schema file `037_provider_identity_binding.sql` (+ PR label `[schema: reset DB]`)
- [ ] H2 config + fail-fast
- [ ] H3 token key/issuer/`gen`; state key
- [ ] H4 `hd` + `sub` binding in the callback
- [ ] H5 middleware by ID + generation; logout; `Disable` bumps generation
- [ ] H6 redirect + CORS
- [ ] H7 tests; build gate
- [ ] DEV-1 database development guide (`docs/database-development.md`) lands in the same PR or immediately after (see `path.md`)

## Post-Phase Fixes

_None yet._
