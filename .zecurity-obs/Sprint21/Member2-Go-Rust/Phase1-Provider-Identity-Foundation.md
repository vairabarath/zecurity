---
type: phase
member: M2
person: Barath
sprint: 21
phase: 1
execution: H
title: Provider Identity Foundation (D-24…D-26, D-29 seam; D-16 retained parts)
status: done   # H-a merged in #104 (2026-09-28); H-b (CORS + DEV-1) on sprint21/m2-h-b-cors-devguide
depends_on: []
schema_change: true   # [schema: reset DB] — 037_provider_local_auth.sql
tags:
  - go
  - provider
  - auth
  - jwt
  - argon2id
  - identity
  - security
  - provider-dashboard
---

# Phase 1 (H) — Provider Identity Foundation

> **Decision Record:** amendment **2026-09-26**.
> - **D-24:** a Zecurity-owned **Provider Identity Service**, whose first authentication method is local email + password (Argon2id); no Google `sub`/`hd`, no provider OAuth flow.
> - **D-25:** a dedicated provider JWT key and issuer; `session_generation` revocation (retained from D-16).
> - **D-26:** create-only bootstrap with a forced first password change.
> - **D-28:** TOTP is mandatory for `super-admin` **before Sprint 22** (a separate follow-up, not this phase); recovery codes, rotation policies and refresh tokens are deferred.
> - **D-29:** external IdPs plug in later as optional authentication sources. This phase builds the seam they plug into.
>
> **ADR-029:** Provider Identity stays an **internal controller module** (`controller/internal/provider`) for now: same process, database and Valkey, served under `/provider/*`. It's not a separate service.
>
> **Blocks:** M1-C (console login), M2-U (operator management), M1-R3 (route wiring). Land it first.

## Problem (verified)

- **Provider login is Google OAuth.**
  - `auth.ProviderRoutes` serves `/provider/auth/initiate` and `/provider/auth/callback` (`controller/internal/auth/provider_auth.go`, `main.go:338-348`) and makes `PROVIDER_GOOGLE_REDIRECT_URI` a **required** env var (`mustEnv`, `main.go:341`).
  - The provider plane is internal-only (InkYank employees), so it shouldn't depend on an external login provider (D-24).
- **Shared signing key.** Provider JWTs are HS256-signed with the **tenant** `JWT_SECRET`:
  - `IssueProviderToken(pa.s.cfg.JWTSecret, …)` (`provider_auth.go:133`);
  - `RequireProvider(mustEnv("JWT_SECRET"), …)` (`main.go:354`).
- **Shared issuer.** Provider tokens use the tenant issuer `appmeta.ControllerIssuer = "zecurity-controller"` (`internal/provider/session.go`, `internal/appmeta/identity.go:7`). Only `aud=provider` separates the two.
- **No logout or revocation.** A token lives its full 15 min (`main.go:342`). `RequireProvider` looks the user up by **email** on every request (`internal/middleware/provider.go`).
- **Bootstrap re-activates on every start.**
  - `PROVIDER_BOOTSTRAP_EMAILS` seeds super-admins through `UpsertSuperAdmin`. It runs on **every** startup and forces each listed account back to an active super-admin (`store.go:129-150`, `main.go:1131-1151`).
  - That has no place in a password model: it would undo disables and role changes.
- **No login throttling.** There is no rate limiter anywhere in the controller, so a password endpoint needs one.
- **Argon2id is available:** `golang.org/x/crypto` is already a dependency (`go.mod`, currently indirect) and includes `argon2`. No new module is needed.

## Goal

The provider plane owns its identity through a **Provider Identity Service**:
- **authentication** (who you are) goes through a pluggable `Authenticator`. The only method in Sprint 21 is local email + password (Argon2id);
- **authorization** (roles), the **provider JWT** and **`session_generation`** belong to the identity service and never depend on the authentication method;
- operators receive a **provider-only** JWT (own key and issuer) that can be **revoked instantly** (`session_generation`);
- the first super-admin comes from a **create-only bootstrap** with a forced password change;
- the provider Google OAuth path is gone.

The tenant Google/OIDC login is untouched.

## Locked implementation decisions (2026-09-28)

| # | Decision |
|---|----------|
| 1 | **Two PRs.** **H-a:** the identity system (schema `037`, identity service, passwords, tokens, store, rate limiter, auth endpoints, middleware, create-only bootstrap, provider Google OAuth removal, `.env.example` vars), labelled `[schema: reset DB]`. **H-b:** CORS for the console origin plus DEV-1 (`docs/database-development.md`, `agent.md`, `docker-compose.yml` comment). The H-a pieces must land together, or login breaks in between. |
| 2 | **Argon2id memory cap:** `providerMaxConcurrentHashes = 4` (a constant, no env var), a 2 s wait, then 503. |
| 3 | **Valkey down → login fails closed** with 503 `login_unavailable`. |
| 4 | **Thresholds:** Argon2id `m=64 MiB, t=3, p=2`; 5 failures per email and 20 per IP per 15 min; `Retry-After` on 429; **one** `provider_auth.rate_limit` audit row when a lockout starts; passwords 12–128 characters. |
| 5 | **Every password change and password reset invalidates every existing session immediately** (a `session_generation` bump in the same statement). This is an explicit acceptance criterion (AT-CORE-2), so the bump can't be removed quietly later. |

## Files

| File | Change |
|------|--------|
| `controller/migrations/037_provider_local_auth.sql` (new) | Columns on `provider_users` (H1) |
| `controller/internal/appmeta/identity.go` | `ProviderIssuer = "zecurity-provider"` |
| `controller/internal/provider/identity.go` (new) | Provider Identity Service: `Authenticator` interface, `LocalPasswordAuthenticator`, `IssueSession` (the only JWT minting path) |
| `controller/internal/provider/password.go` (new) | Argon2id hash/verify (PHC string), password policy, dummy hash for timing |
| `controller/internal/provider/ratelimit.go` (new) | Valkey login-attempt limiter |
| `controller/internal/provider/session.go` | Provider key + issuer; `gen`, `pwc` and `amr` claims; `issueProviderToken` **unexported** (only `IssueSession` calls it) |
| `controller/internal/provider/store.go` | `GetByID`, `SetPassword`, `BumpSessionGeneration`, `RecordLogin`, `CreateBootstrapSuperAdminIfNone` (single-statement, create-only); `Disable` bumps the generation; **remove** `UpsertSuperAdmin` |
| `controller/internal/provider/auth_handlers.go` (new) | `Login`, `ChangePassword`, `Logout` |
| `controller/internal/middleware/provider.go` | Provider key; load by ID; generation / disabled / `pwc` checks |
| `controller/internal/auth/provider_auth.go` | **Delete** (provider Google OAuth). Tenant auth files are untouched. |
| `controller/cmd/server/main.go` | Env + fail-fast; bootstrap; new routes; remove `ProviderRoutes` and `PROVIDER_GOOGLE_REDIRECT_URI`; CORS for `/provider/*` |
| `controller/.env.example` | `PROVIDER_JWT_SECRET`, `PROVIDER_BOOTSTRAP_EMAIL`, `PROVIDER_BOOTSTRAP_PASSWORD`, `PROVIDER_CONSOLE_ORIGIN` (placeholders); remove `PROVIDER_GOOGLE_REDIRECT_URI` and `PROVIDER_BOOTSTRAP_EMAILS` |
| Tests | `password_test.go`, `ratelimit_test.go`, `session_test.go`, `auth_handlers_test.go`, `middleware/provider_test.go`, `store_test.go` (DB) |

## Steps

### H1 — Schema (`037_provider_local_auth.sql`) · `[schema: reset DB]`

```sql
-- Sprint 21 Phase H (D-24/D-25/D-26): local provider accounts with revocable sessions.
ALTER TABLE provider_users
    ADD COLUMN password_hash        TEXT,                              -- Argon2id PHC string; NULL = no local password (reserved for optional SSO operators later, D-29)
    ADD COLUMN must_change_password BOOLEAN     NOT NULL DEFAULT FALSE,
    ADD COLUMN password_changed_at  TIMESTAMPTZ,
    ADD COLUMN session_generation   BIGINT      NOT NULL DEFAULT 1,
    ADD COLUMN last_login_at        TIMESTAMPTZ;
```

- `email` stays the stable operator key (unique, lowercase), as D-29 requires.
- **Never edit `025`.** Add the new file and label the PR `[schema: reset DB]`.

### H2 — Configuration (`main.go`)

- **`PROVIDER_JWT_SECRET`:** required. `log.Fatalf` if it is missing, under 32 bytes, or byte-equal to `JWT_SECRET`.
- **`PROVIDER_BOOTSTRAP_EMAIL` / `PROVIDER_BOOTSTRAP_PASSWORD`:** see H6.
- **`PROVIDER_CONSOLE_ORIGIN`:** optional. It's used **only** for the CORS allowlist when the console runs on its own origin; dev uses the Vite proxy.
- **Removed:**
  - `PROVIDER_GOOGLE_REDIRECT_URI` is no longer read.
  - `PROVIDER_BOOTSTRAP_EMAILS` (plural) is no longer read.
  - If either is still set, log `<VAR> is ignored — provider login is local (D-24)`.
- **`appmeta.ProviderIssuer = "zecurity-provider"`.**

### H3 — Passwords (`password.go`)

- **Argon2id**, `golang.org/x/crypto/argon2.IDKey`, with parameters `m=64 MiB, t=3, p=2`, a 16-byte random salt and a 32-byte key.
  - Stored as a PHC string: `$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>`.
  - Verify reads the parameters **from the stored string**, so they can be raised later without invalidating hashes.
  - Compare with `subtle.ConstantTimeCompare`.
- **Policy (minimal; rotation policies are deferred by D-28):**
  - 12–128 characters;
  - not equal to the email;
  - on change, not equal to the current password.
- **Timing:** unknown email, disabled account, or `password_hash IS NULL` → still run one verify against a fixed dummy hash, so response time doesn't reveal which accounts exist.
- **Memory cap (locked 2026-09-28):**
  - Each hash uses about 64 MiB, so unbounded parallel logins could exhaust memory (20 concurrent ≈ 1.3 GB).
  - A semaphore allows at most `providerMaxConcurrentHashes = 4` hashes (a **constant**, not an env var), shared by hash and verify, including the dummy verify.
  - A request waits up to 2 s for a slot, then gets **503** `login_unavailable`.

### H3b — Identity service seam (`identity.go`, D-24/D-29)

```go
// Authenticator is one way to prove "who you are". Sprint 21 ships only LocalPasswordAuthenticator;
// OIDC / Google Workspace / Azure AD / Okta authenticators plug in here later (D-29).
type Authenticator interface {
    Method() string                                       // "pwd" (later e.g. "oidc")
    Authenticate(ctx context.Context, in Credentials) (*ProviderUser, error)
}

type IdentityService struct { store *Store; key []byte; /* ttl, limiter, audit */ }

// IssueSession is the ONLY place a provider JWT is minted. It reads role, email and
// session_generation from the DB row, never from the authenticator, and stamps amr.
func (s *IdentityService) IssueSession(ctx context.Context, u *ProviderUser, amr []string, pwcOnly bool) (token string, expiresIn int64, err error)
```

- The login handler **calls the authenticator, then `IssueSession`**. It never builds claims itself.
- Adding an authentication method later means a new `Authenticator` plus a route. **Roles, token format, revocation and the middleware don't change.**
- **Keep it minimal:** one interface, one implementation, one issuer function. No plugin registry, no provider tables. Those come with the first real external IdP.

### H4 — Tokens (`session.go`)

```go
type ProviderClaims struct {
    Role  string `json:"role"`
    Email string `json:"email"`
    Gen   int64  `json:"gen"`           // must equal provider_users.session_generation
    PWC   bool   `json:"pwc,omitempty"` // password-change-only token (forced change, H5)
    AMR   []string `json:"amr"`         // how the user authenticated: ["pwd"] now; TOTP adds "otp" (D-28)
    jwt.RegisteredClaims                 // Subject=provider_user_id, Issuer=ProviderIssuer, Audience=[provider]
}
func issueProviderToken(key []byte, u ProviderUser, amr []string, pwc bool, ttl time.Duration) (string, error) // unexported: only IdentityService.IssueSession calls it
func VerifyProviderToken(key []byte, token string) (*ProviderClaims, error) // HS256 only + ProviderIssuer + aud=provider + exp required
```

- A token signed with `JWT_SECRET`, carrying `iss=zecurity-controller`, or missing `aud=provider` fails verification.
- Full tokens last 15 min (unchanged). Password-change-only tokens last 10 min.
- **`amr` is forward-compatible for D-28.** When mandatory TOTP lands before Sprint 22, `RequireProvider` can refuse super-admin tokens whose `amr` lacks `"otp"`, with no token-format change.

### H5 — Auth endpoints (`auth_handlers.go`)

| Route | Auth | Behaviour |
|-------|------|-----------|
| `POST /provider/auth/login` `{email, password}` | none (rate-limited) | See the flow below |
| `POST /provider/auth/password` `{current_password, new_password}` | `RequireProvider`; **`pwc` tokens allowed** | Verify the current password and the policy. Store a new hash, set `must_change_password=false` and `password_changed_at`, **bump `session_generation`**, audit `provider_user.password_change`. Return a **new full token**. |
| `POST /provider/auth/logout` | `RequireProvider` | **Bump `session_generation`**, audit `provider_session.logout`, return 204 |

**Login flow:**
1. **Rate limit (`ratelimit.go`, Valkey):**
   - per email: `provider:login:fail:email:<email>`;
   - per client IP: `provider:login:fail:ip:<ip>`;
   - counters use `INCR` with a 15 min TTL on the first failure;
   - at ≥ 5 failures per email or ≥ 20 per IP → **429** `{"error":"too_many_attempts"}` with `Retry-After`, **without** checking the password;
   - a successful login clears the email counter.
   - The IP is `r.RemoteAddr`. `X-Forwarded-For` is **not** trusted unless a trusted-proxy setting is added later.
   - If Valkey is down, fail **closed** for login: 503 `login_unavailable`. *(Locked 2026-09-28: the limiter is a security control, so silently continuing without it would disable brute-force protection unnoticed.)*
   - **Audit on lockout start:** when a counter first crosses its threshold (the failure that trips 5 per email or 20 per IP), write **one** `provider_auth.rate_limit` audit row:
     - `provider_user_id` NULL;
     - `provider_email` = the attempted email (lowercased);
     - target `provider_login` / `email:<email>` or `ip:<ip>`;
     - details `{scope, failures, window_seconds, retry_after_seconds}`;
     - `ip_address` set.

     Requests rejected **while** locked out are logged, not audited, so an attacker can't flood `provider_audit_logs`.
2. **Load the user by email.** Unknown, disabled, NULL hash, or wrong password → the **same** 401 `{"error":"invalid_credentials"}` (the dummy verify from H3 keeps the timing equal). Increment the failure counters.
3. **`must_change_password = true`** → 200 `{token (pwc=true, 10 min), expires_in, password_change_required: true}`.
4. **Otherwise** → 200 `{token (15 min), expires_in, password_change_required: false}`. Update `last_login_at`. Audit `provider_session.login`.
   - Individual failed attempts are **logged**, not audited, to keep `provider_audit_logs` meaningful. Only the lockout start above is audited.

Passwords are never logged, returned or stored in audit `details`.

### H6 — Bootstrap: create-only (D-26)

Bootstrap exists **only to create the first super-admin**. On startup:

1. **A `super-admin` row already exists** (in any state: `SELECT EXISTS (SELECT 1 FROM provider_users WHERE role = 'super-admin')`):
   - the bootstrap variables are **ignored entirely**, whatever email they name;
   - log once at info: `provider super-admin exists — PROVIDER_BOOTSTRAP_* ignored`;
   - if `PROVIDER_BOOTSTRAP_PASSWORD` is still set, also warn: `remove PROVIDER_BOOTSTRAP_PASSWORD from the environment`.
2. **No super-admin exists and `PROVIDER_BOOTSTRAP_EMAIL` is set:**
   - `PROVIDER_BOOTSTRAP_PASSWORD` must be set and pass the policy, otherwise `log.Fatalf`;
   - create the account as `super-admin` with that hash and `must_change_password=true`;
   - audit `provider_user.bootstrap_create` (system actor, `provider_email = "system:bootstrap"`);
   - log: `created bootstrap provider super-admin <email> — change the password at first login, then remove PROVIDER_BOOTSTRAP_PASSWORD`.
3. **No super-admin exists and no bootstrap email is set:** warn that no provider operator can sign in until one is bootstrapped.

**Implementation rules:**
- Do the existence check and the insert in **one statement**: `INSERT … SELECT … WHERE NOT EXISTS (… role = 'super-admin')`. Two starts can then never create two bootstrap admins, even though D-02 means one instance.
- If the bootstrap email already exists as a **non**-super-admin (only possible after manual DB edits), the insert conflicts on `email`. Log an error and do nothing: never promote, overwrite or re-enable an existing row.
- There is **no reset flag** and no environment variable that changes an existing account. Once the first super-admin exists, the bootstrap environment has no effect.

**Recovery if every super-admin loses access** (not built in Sprint 21): a future controller CLI command. See **Future: break-glass recovery CLI** below.

### Future: break-glass recovery CLI (documented, not built in Sprint 21)

Recovery will be an explicit, operator-run **command**, not an environment flag that changes behaviour on every start. Planned shape:

```bash
# run on the controller host, with the controller's DATABASE_URL (shell access to the controller host is the trust boundary)
zecurity-controller provider recover-admin --email ops@inkyank.com
```

- It prints a one-time temporary password to the terminal. Nothing is logged or stored in plaintext.
- For that account it sets `must_change_password=true`, `role='super-admin'` and `disabled_at=NULL`, and bumps `session_generation`, so all existing tokens die.
- It creates the account if it's missing.
- It audits `provider_user.recovery` with the actor `system:cli` plus the OS user and hostname.
- It needs an interactive confirmation (or `--yes`) and refuses to run against a database it can't reach.
- It reuses Phase H's `password.go`, `Store` and audit helpers, so it's a thin subcommand.

**Target:** before production, and at the latest alongside the mandatory super-admin TOTP follow-up (D-28), since TOTP adds a second way to be locked out.

**Until then** (pre-production):
- keep **at least two active super-admins** (Phase U already refuses to remove the last one);
- in a development environment, a lost bootstrap account is recovered by resetting the local DB, which the pre-production rule already treats as disposable.

### H7 — Middleware (`RequireProvider`)

`RequireProvider(providerKey []byte, store)`:
1. `VerifyProviderToken`.
2. `user := store.GetByID(claims.Subject)`. Missing or disabled → 403.
3. `claims.Gen != user.SessionGeneration` → **401** `{"error":"provider session revoked"}`.
4. `claims.Email != user.Email` → 401.
5. `claims.PWC` and the route isn't `POST /provider/auth/password` → **403** `{"error":"password_change_required"}`.
6. The role comes from the DB (unchanged).

### H8 — Remove the provider Google OAuth path; add CORS

- **Delete** `internal/auth/provider_auth.go` and its routes (`/provider/auth/initiate`, `/provider/auth/callback`), and remove the `mustEnv("PROVIDER_GOOGLE_REDIRECT_URI")` call.
  - Tenant Google/OIDC (`/auth/callback`, IdP connections) is **unchanged**.
  - Git history keeps the code for reference if optional SSO is designed later (D-29).
- **CORS:** when `PROVIDER_CONSOLE_ORIGIN` is set, `/provider/*` answers preflight and adds:
  - `Access-Control-Allow-Origin: <origin>` (exact match; never `*`);
  - `Allow-Headers: Authorization, Content-Type`;
  - `Allow-Methods: GET, POST, PATCH, OPTIONS`;
  - no credentials (bearer only).

  Other origins and tenant routes get no CORS headers.

### H9 — Tests

See **Tests** below. The auth boundary tests get shared review with M1.

## Invariants

1. **A tenant JWT can never authenticate a provider route.** This holds for a wrong key, a wrong issuer, and a missing `aud`.
2. **Logout, password change, password reset (U), disable and role change (U)** each invalidate **every** outstanding token for that user on the next request.
3. **No enumeration:** login responses and timing are identical for unknown email, disabled account, no password, and wrong password.
4. **Brute force is bounded** per account and per IP. With Valkey down, login fails closed.
5. **A `pwc` token can only change the password.**
6. **Bootstrap is create-only.** It creates the first super-admin and does nothing once any super-admin exists. No environment variable can change, reset, promote or re-enable an existing account.
7. **Passwords and tokens are never logged, returned or audited.** Hashes are never returned by any API.
8. **No change** to tenant auth, the tenant `JWT_SECRET`, tenant Google/OIDC, or relay provisioning tokens.

## Tests

| Test | Kind | Asserts |
|------|------|---------|
| `TestIdentityService_SingleMintingPath` | unit | tokens come only from `IssueSession`; role, email and `gen` come from the DB row, not authenticator input; `amr=["pwd"]` for password login |
| `TestArgon2id_HashVerify_PHC` | unit | round-trip; parameters parsed from the stored string; wrong password fails; constant-time compare used |
| `TestPasswordPolicy` | unit | length bounds; not the email; not the current password |
| `TestVerifyProviderToken_RejectsTenantSecret` / `…ControllerIssuer` / `…MissingAudOrExp` / `…AlgNone` | unit | each → error |
| `TestProviderConfig_FailsFast` | unit | missing, short, or equal-to-`JWT_SECRET` key → startup error; ignored-var warning |
| `TestLogin_Success_Audited` | DB | 200, full token (`iss=zecurity-provider`, `gen`), `last_login_at` set, one `provider_session.login` row |
| `TestLogin_NoEnumeration` | DB | unknown / disabled / NULL-hash / wrong password → identical 401 body; the dummy verify runs |
| `TestLogin_RateLimited` | DB + Valkey | the 6th failure for an email → 429 + `Retry-After`, even with the right password; success clears the counter; IP limit |
| `TestLogin_ValkeyDown_FailsClosed` | unit | limiter error → 503 |
| `TestLogin_RateLimitAuditedOnceOnTrip` | DB + fake limiter | the threshold-crossing failure writes exactly one `provider_auth.rate_limit` row (email scope, then IP scope); further 429s while locked write none |
| `TestPasswordHash_ConcurrencyCap` | unit | at most `providerMaxConcurrentHashes` hashes run at once; a waiter past 2 s gets `login_unavailable` (503) |
| `TestPasswordChange_InvalidatesAllSessions` | DB | two live full tokens → `POST /provider/auth/password` → both old tokens 401 at once; only the returned token works |
| `TestForcedChange_PWCTokenScope` | DB | a `must_change` login → `pwc` token; `/provider/me` → 403 `password_change_required`; `/provider/auth/password` → new full token; old tokens 401 |
| `TestLogout_RevokesAllTokens` | DB | two tokens → logout → both 401; audit row |
| `TestRequireProvider_GenerationMismatch401` / `DisabledUser403` / `EmailMismatch401` | DB | as named |
| `TestBootstrap_CreateOnly` | DB | first start creates a super-admin with `must_change` and one `provider_user.bootstrap_create` row; a second start with a **different** password doesn't change the hash; warning when the password is still set |
| `TestBootstrap_IgnoredOnceSuperAdminExists` | DB | with any super-admin present (including a disabled one), a bootstrap email naming a **new** address creates nothing, and naming an existing address changes nothing (hash, role, disabled state, generation) |
| `TestBootstrap_NoPromotionOfExistingRow` | DB | no super-admin exists, and the bootstrap email belongs to a relay-ops row → error logged, the row is unchanged, no super-admin created |
| `TestBootstrap_SingleStatement` | DB | two concurrent bootstrap runs create exactly one super-admin |
| `TestProviderGoogleRoutesRemoved` | HTTP | `/provider/auth/initiate` and `/provider/auth/callback` → 404; controller starts without `PROVIDER_GOOGLE_REDIRECT_URI` |
| `TestProviderCORS_OnlyConsoleOrigin` | unit | exact origin echoed; others get no header; tenant routes unaffected |

The existing `internal/provider/*_test.go`, `internal/middleware/provider_test.go` and `internal/relay/admin_handler*_test.go` suites stay green, updated only for the new signatures. Tenant `internal/auth/*_test.go` must pass **unmodified**.

## Acceptance Criteria

- [x] AT-H.1 … AT-H.11 in [[Sprint21/Acceptance-Test-Plan]] pass. *(All covered by passing tests; the CORS half of AT-H.10 landed in H-b and was also verified live with curl against the real binary.)*
- [x] Live smoke test (2026-09-28, H-a). Run against a **throwaway database** on alternate ports (`:18080` / `:19090`) instead of resetting the dev DB; same schema files, same binary:
  - bootstrap creates the super-admin;
  - curl login → `password_change_required`;
  - change the password → full token;
  - `/provider/me` works;
  - logout → 401;
  - a tenant token on `/provider/me` → 401;
  - 6 bad passwords → 429.

## Build Check

```bash
cd controller && docker compose down -v && docker compose up -d   # schema change
cd controller && go build ./... && go vet ./... && go test ./internal/provider/... ./internal/middleware/... ./internal/auth/... ./internal/relay/...
```

## Implementation Checklist

- [x] H1 schema file `037_provider_local_auth.sql` (+ PR label `[schema: reset DB]`)
- [x] H2 config + fail-fast; ignored-var warnings
- [x] H3 Argon2id + policy + dummy hash
- [x] H3b identity service seam: `Authenticator`, `LocalPasswordAuthenticator`, `IssueSession` (only minting path)
- [x] H4 provider key/issuer; `gen`, `pwc`, `amr` claims
- [x] H5 login (rate-limited, no enumeration), change password, logout
- [x] H6 create-only bootstrap (ignored once any super-admin exists; single-statement insert; no reset flag)
- [x] H7 middleware (by ID, generation, disabled, `pwc` scope)
- [x] H8a provider Google OAuth removed (H-a, commit 3)
- [x] H8b CORS for `PROVIDER_CONSOLE_ORIGIN` (H-b)
- [x] H9 tests; build gate (H-a)
- [x] DEV-1 database development guide (`docs/database-development.md`), `agent.md` section, `docker-compose.yml` comment (H-b)

## Post-Phase Fixes

### Fix: dummy-hash failure cached for the life of the process
**Issue:** the dummy Argon2id hash used by `VerifyDummy` (equal-time rejection of unknown, disabled or no-password accounts) was built lazily through the hashing semaphore and cached in a `sync.Once`. If the first unknown-email login arrived while all `providerMaxConcurrentHashes` slots were busy, which is exactly the login flood the cap defends against, the build timed out. `ErrHashBusy` was then cached, and **every** later unknown-email login returned 503 until a restart.

**Root Cause:** lazy initialization through a bounded resource, with the error cached alongside the value.

**Fix Applied (`controller/internal/provider/password.go`, commit `a3fca4a`):**
```go
// BEFORE:
dummyHashOnce.Do(func() { dummyHash, dummyHashErr = HashPassword(ctx, "…") })
if dummyHashErr != nil { return dummyHashErr }

// AFTER:
const dummyHash = "$argon2id$v=19$m=65536,t=3,p=2$…"   // precomputed, production parameters
func VerifyDummy(ctx context.Context, password string) error {
    _, err := VerifyPassword(ctx, password, dummyHash) // still takes a slot → same timing and memory cap
    return err
}
```
- **Guard tests:** `TestDummyHash_UsesProductionParameters` (fails if the Argon2id parameters change and the constant isn't regenerated) and `TestVerifyDummy_RecoversAfterSaturation`.
- **Found by:** writing `TestLogin_HashCapacityExhausted_503`, before the PR.

## Implementation notes (H-a, 2026-09-28)

Deviations from the commit plan, recorded for future contributors:

- **The Google provider login was removed in commit 3, not commit 8.** Making token minting private (`issueProviderToken`, callable only from `IdentityService.IssueSession`) meant the old Google callback could no longer mint tokens. Removing it together with the token change kept every commit compiling. Within the branch, provider login is unavailable from commit 3 until commit 8 wires the local endpoints; the PR merges as one unit.
- **The identity service is built in `main.go` in commit 7** (`mustProviderIdentity()`), because `RequireProvider`'s new signature needs it. Commit 8 added bootstrap, routes and `.env.example`.
- **Bootstrap logic lives in `provider.Bootstrap`**, not in `main.go`, so every outcome is unit-testable. `main.go` only logs. `Store.HasSuperAdmin` gives a cheap early exit, so normal restarts don't spend a 64 MiB Argon2id hash. The authoritative, race-safe check stays inside `CreateBootstrapSuperAdminIfNone`.
- **Bootstrap serialization uses `pg_advisory_xact_lock` inside one transaction**, not the single `INSERT … WHERE NOT EXISTS` statement the spec suggested. Under READ COMMITTED, a single statement can't stop two concurrent bootstraps with **different** emails; the advisory lock can.
- **Mutation-tested serialization.** The first concurrency test (`TestBootstrap_SingleStatement`, 8 goroutines) still passed with the lock removed, because the race window is too small to trigger reliably, so it proved nothing. `TestBootstrap_SerializedByAdvisoryLock` holds the lock on a second session and asserts that a bootstrap attempt **blocks** until it's released. It fails deterministically without the lock. Both tests are kept.
- **Error surfaces:**
  - A login audit failure fails the login (500), because the session was never delivered.
  - Password-change and logout audit failures are logged only, because the state change already happened and failing the response would mislead the operator.
  - Individual failed attempts are logged. Only a lockout start is audited (one `provider_auth.rate_limit` row).

## Implementation notes (H-b, 2026-09-28)

- **CORS wraps the whole mux, not individual routes.** Provider routes use method patterns (`POST /provider/auth/login`), so a browser `OPTIONS` preflight matches none of them and the mux would answer 405 before any route-level middleware ran. `NewProviderCORS` wraps the mux and acts only on paths under `/provider/`.
- **Preflights are answered by the wrapper**: 204 for the console origin, 403 for any other. They're never passed on, because a preflight carries no token and `RequireProvider` would reject it.
- **Real requests from a foreign origin still reach the handler** and its auth, but get no CORS headers, so the browser withholds the response. There's no `Access-Control-Allow-Credentials` (bearer tokens only), and responses under `/provider/` carry `Vary: Origin`.
- **`PROVIDER_CONSOLE_ORIGIN` is validated at startup.** It must be exactly `scheme://host[:port]`, http or https, with no wildcard, path, trailing slash, query or userinfo, otherwise the controller refuses to start. An origin with a trailing slash would silently never match what browsers send.
- **Verified live** with curl against the real binary (throwaway DB): console preflight 204 with the headers; console login 200 with `Access-Control-Allow-Origin`; foreign preflight 403; foreign request with no ACAO; tenant route with no ACAO; no-Origin clients unaffected.

## Known existing issue (not introduced by H-a)

- **`internal/auth` `TestAuthIntegration_LoginBootstrapAndJWTIssue` never runs in CI.** CI's `AUTH_TEST_VALKEY_URL` ends in `/15`, the test's address helper can't parse that ("unknown port"), and it **skips**. CI's `ok` for `internal/auth` doesn't include it.
- **When forced to run, it fails** with `column "subject_claim" does not exist`. Its hard-coded schema-file list (`001`, `031`) misses the file that adds the column. It fails identically on unmodified `fixed-pendings` `a73876a`, so it's independent of H-a.
- **When it runs, it `FlushDB`s Valkey database 0**, the developer's local Valkey, through the `PKI_TEST_DATABASE_URL` fallback.
- **Tracked as Phase K item K6** ([[Sprint21/Member2-Go-Rust/Phase3-Sprint20-Cleanup]]).
- **The two #104 CI failures were different.** `internal/resource` and `internal/outbox` were latent defects exposed when H-a's `go.mod` change invalidated the Go test cache. They're fixed separately in PR #105 (throwaway DB for the resource test; outbox shutdown race and shutdown hang).
