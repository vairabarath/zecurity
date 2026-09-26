---
type: test-plan
sprint: 21
owner: M1 (Sathiya) + M2 (Barath)
status: planned
tags: [sprint21, tests, acceptance, provider-dashboard, provider-identity, provider-rbac, provider-read-api, provider-console]
---

# Sprint 21 — Acceptance Test Plan

> **Purpose.** Prove that the dedicated provider console has its own local login (email + password, Argon2id, provider-only JWT, instant revocation) and a working, lock-out-proof role system; that it shows true data read-only (its only writes are logout and operator management); and that the Sprint 20 carry-overs are closed.
> - The sprint is **not done** until §1 passes and every phase table below is green.
> - Go tests are table-driven `*_test.go`. DB-backed tests use the CI env vars (`ENROLLMENT_TEST_DATABASE_URL`, `SHIELD_TEST_DATABASE_URL`, `PKI_TEST_DATABASE_URL`, `AUTH_TEST_VALKEY_URL`). **A skipped DB test is a failed acceptance case.**
> - **There is no migration framework and no upgrade path for existing databases.** Schema acceptance is the development-workflow checks in §6. Recreate the local DB after pulling any `[schema: reset DB]` PR.

---

## 1. The core invariant — the provider boundary holds

### AT-CORE-1 — A tenant identity can never read provider data

- A tenant access JWT (signed with `JWT_SECRET`, `iss=zecurity-controller`), sent as `Authorization: Bearer` to **every** `/provider/*` read route → **401**.
- A token signed with `PROVIDER_JWT_SECRET` but `iss=zecurity-controller`, or without `aud=provider` → **401**.

### AT-CORE-2 — Logout is immediate and total

- A provider user holds two valid tokens (two browser sessions) and calls `POST /provider/auth/logout` with one.
- **Both** tokens get **401** on the next request, well before their `exp`.

### AT-CORE-3 — The console writes only what Sprint 21 allows

- The console's API client exposes GET methods plus exactly (static test):
  - `POST /provider/auth/login`, `POST /provider/auth/password`, `POST /provider/auth/logout`;
  - `POST /provider/users`, `PATCH /provider/users/{id}`, `POST /provider/users/{id}/disable`, `POST /provider/users/{id}/enable`, `POST /provider/users/{id}/reset-password`.
- A UI review finds no create, revoke, delete, suspend or edit controls outside the super-admin **Provider users** page.
- A network capture of a relay-ops session shows no non-GET request except the auth calls.

### AT-CORE-4 — The provider plane can't be locked out

- Across any sequence of operator-management calls, including concurrent ones, at least one **active super-admin** always remains.
- If every super-admin loses access, `PROVIDER_BOOTSTRAP_RESET=true` with the bootstrap email and password restores the bootstrap account (the break-glass path) without SQL.

### AT-CORE-5 — Passwords never leak

- No password or temporary password appears in controller logs, `provider_audit_logs.details`, any GET response, or browser `localStorage`.
- `password_hash` is never returned by any API.

---

## 2. Phase H — Provider identity foundation (M2)

| ID | Scenario | Expected |
|----|----------|----------|
| AT-H.1 | Controller started without `PROVIDER_JWT_SECRET`, with one under 32 bytes, or with it equal to `JWT_SECRET` | Startup fails with a clear fatal message |
| AT-H.2 | Fresh DB, `PROVIDER_BOOTSTRAP_EMAIL` + a valid `PROVIDER_BOOTSTRAP_PASSWORD` | One `super-admin` created with `must_change_password=true`; `provider_user.bootstrap_create` audited; the log tells you to change the password and remove the variable |
| AT-H.3 | Restart with a **different** `PROVIDER_BOOTSTRAP_PASSWORD` | The existing account is unchanged (hash, role, disabled state); a warning says to remove the variable |
| AT-H.4 | Bootstrap login → forced change | Login → `password_change_required: true` and a `pwc` token; `/provider/me` with it → 403 `password_change_required`; `POST /provider/auth/password` → new full token (`iss=zecurity-provider`, `aud=provider`, `gen` incremented); the old token → 401 |
| AT-H.5 | Wrong password / unknown email / disabled account | Identical 401 `invalid_credentials` body, similar latency (dummy Argon2id verify) |
| AT-H.6 | Brute force | The 6th failure for one email within 15 min → 429 + `Retry-After`, even with the correct password; 20 failures from one IP → 429; a success clears the email counter |
| AT-H.7 | Valkey down | Login → 503 `login_unavailable` (fails closed) |
| AT-H.8 | Provider user disabled (via U) or logged out | Existing tokens → 403 / 401 on the next request; `session_generation` incremented |
| AT-H.9 | Password storage | `provider_users.password_hash` is an Argon2id PHC string (`$argon2id$v=19$m=65536,t=3,p=2$…`); it verifies after a parameter bump |
| AT-H.10 | Google provider login removed; CORS | `/provider/auth/initiate` and `/provider/auth/callback` → 404; the controller starts without `PROVIDER_GOOGLE_REDIRECT_URI`; tenant Google login still works. With `PROVIDER_CONSOLE_ORIGIN` set, CORS headers appear only for that exact origin on `/provider/*`, none on tenant routes |
| AT-H.11 | Identity service seam | Every provider token is minted by `IdentityService.IssueSession` (`issueProviderToken` is unexported); role, email and `gen` come from the DB row; password-login tokens carry `amr: ["pwd"]` |

## 3. Phase C — Provider console foundation (M1)

| ID | Scenario | Expected |
|----|----------|----------|
| AT-C.1 | Dedicated app | `provider-console/` is its own Vite + React project; `npm ci && npm run lint && npm test && npm run build` pass; `admin/` is unchanged and builds; runs on its own origin (dev :5174); no OAuth or callback route exists |
| AT-C.2 | Login | Email + password form → Home shows email, role, last login, expiry. A wrong password shows a generic message; a 429 shows the wait time |
| AT-C.3 | Forced change | A user with `must_change_password` lands on Change password and can't open any other page until it succeeds; afterwards the new token is used |
| AT-C.4 | Session end | After 15 min, after logout (any tab), or after another admin changes this user's role, disables them or resets their password, the next request → 401 → Login with a notice |
| AT-C.5 | Role guard | relay-ops opening `/users` (or any super-admin route) → Forbidden page; the nav never shows super-admin sections to relay-ops |
| AT-C.6 | Provider users page | super-admin lists operators; adds a `relay-ops` operator (temporary password shown **once**, then gone); changes a role; disables and re-enables; resets a password (shown once). Each action has a confirmation dialog; 409 codes show as readable messages; your own row has no destructive controls |
| AT-C.7 | No leaks, no tenant calls | A network capture shows no `/graphql` or tenant `/auth/*` requests; passwords and tokens never appear in `localStorage` or the URL |

## 4. Phase U — Operator management API (M2)

| ID | Scenario | Expected |
|----|----------|----------|
| AT-U.1 | Role matrix | super-admin → 2xx on all operator routes; relay-ops → 403; a `pwc` token → 403; tenant JWT → 401 |
| AT-U.2 | Add + first login | `POST /provider/users {email, role: relay-ops}` → 201 with `temporary_password` (and `Cache-Control: no-store`). That person logs in → forced change → can read relays, gets 403 on tenants, audit, certificates and users |
| AT-U.3 | Duplicates | Active email → 409 `already_exists`; disabled email → 409 `exists_disabled`; comparison is case-insensitive |
| AT-U.4 | Role change | `PATCH` → 200; the user's old token → 401; one `provider_user.role_change` audit row with `from`/`to`; a same-role PATCH is a no-op, not audited |
| AT-U.5 | Disable / enable / reset | Disable → the user's tokens 401 at once; enable → a new login works and the old token stays invalid. Reset → the old password fails, old tokens 401, the temp password forces a change. One audit row each, with no password in `details` |
| AT-U.6 | Guards | Self-disable, self-demote or self-reset → 409 `cannot_modify_self`; demoting or disabling the last active super-admin → 409 `last_super_admin` |
| AT-U.7 | Concurrency | Two super-admins demote each other concurrently → exactly one succeeds; one active super-admin remains |
| AT-U.8 | Audit + privacy | Refused or failed calls write no audit row; no response contains `password_hash`; `temporary_password` appears only in create and reset responses |

## 5. Phase R — Provider read APIs (M1)

| ID | Scenario | Expected |
|----|----------|----------|
| AT-R.1 | Role matrix | relay-ops: `/provider/relays*` 200; `/provider/tenants*`, `/provider/audit`, `/provider/certificates` → 403. super-admin: all 200 |
| AT-R.2 | Relay list liveness | Relay whose DB `last_heartbeat_at` is 4 min old but Valkey `relay:heartbeat:last` is fresh → the response shows the fresh time and `status=active` |
| AT-R.3 | Relay detail | Allowlists, full cert history (`is_current` on the current serial, revoked rows show `revoked_at`), attachments; **no** `enrollment_token_jti` |
| AT-R.4 | Tenant list/detail | Counts match seeded data per status; detail lists remote networks, connectors and shields; **no** `encrypted_*`, key material, tokens or user identities (OQ-2) |
| AT-R.5 | Tenant detail audit (per OQ-1) | If decided "audit": one `tenant.read` row per detail request, target = tenant; list requests not audited |
| AT-R.6 | Provider audit query | Filters (action, target, email, time range) each narrow correctly; keyset pagination stable (no duplicates or gaps across pages); limit clamped at 200 |
| AT-R.7 | Certificate expiry | Every kind present (root, intermediate, workspace CA, controller gRPC, relay, connector, shield, client device); buckets correct; revoked or deleted excluded; the controller cert matches `openssl s_client -connect localhost:9090` |
| AT-R.8 | Bad input | Malformed cursor, limit or `within` → 400; unknown id → 404 |

## 6. Phase P — Provider console read pages (M1)

| ID | Scenario | Expected |
|----|----------|----------|
| AT-P.1 | Role-aware nav | relay-ops sees Relays only; super-admin sees Relays, Tenants, Audit, Certificates, Provider users |
| AT-P.2 | Relays | List and detail match `GET /provider/relays*` for a live relay (status, last heartbeat, capacity, cert expiry, history) |
| AT-P.3 | Tenants / Audit / Certificates | Pages match their API responses; audit filters and pagination work, and the audit shows Phase H/U actions; certificate buckets and colours match `summary` |
| AT-P.4 | Read-only | The read pages have no mutating controls; the API-surface test (AT-CORE-3) still passes; build and lint pass |

## 7. Phase K — Sprint 20 cleanup (M2)

| ID | Scenario | Expected |
|----|----------|----------|
| AT-K.1 | KI-1: relay renewal plan | Backdated 15m cert → renews about 9 min after issue (± jitter), **no loop**; 1h → about 36 min; 30 d unchanged; existing certs without metadata keep today's schedule (option A) |
| AT-K.2 | KI-2: window respected | Shield inside the configured window → exactly one `ReEnroll`; outside → none; window source is the chosen option (A: the controller-sent value, default 48h until received) |
| AT-K.3 | KI-2: no churn | Connector sends at most one `ReEnroll` per shield per stream per 10 min; the shield ignores a `ReEnroll` within 60 s of a successful renewal. Live with `SHIELD_CERT_TTL=24h`: no 15 s renewal loop |
| AT-K.4 | KI-3 | Resolved by removal (D-24): `.env.example` has no `PROVIDER_GOOGLE_REDIRECT_URI`; Sprint 20 Known Issues marks KI-3 resolved |
| AT-K.5 | JWT scrub | `grep -nE 'eyJ[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.' controller/.env.example` → no match; the CI guard fails on a planted JWT |
| AT-K.6 | Proto (KI-2 option A only) | `buf generate` diff is additive only (new message/field); no renumbering |

## 8. Development workflow (schema changes)

| ID | Scenario | Expected |
|----|----------|----------|
| AT-DEV.1 | Docs | `docs/database-development.md` exists and states that **a schema change requires `docker compose down -v && docker compose up -d`**, that there is no migration framework and nothing upgrades an existing database, the next free schema file number, and the `[schema: reset DB]` PR label. `agent.md` links it from a "Local database" section; `docker-compose.yml` points to it |
| AT-DEV.2 | Fresh reset | From a clean checkout: `cd controller && docker compose down -v && docker compose up -d`, then `go run ./cmd/server` → boots; `\d provider_users` shows `password_hash`, `must_change_password`, `password_changed_at`, `session_generation`, `last_login_at` |
| AT-DEV.3 | Schema PR hygiene | The PR adding `037_provider_local_auth.sql` is labelled `[schema: reset DB]`; no existing schema file is modified (`git diff --stat` touches only the new file under `controller/migrations/`) |
| AT-DEV.4 | No migration tooling | `controller/go.mod` has no `golang-migrate`, `goose` or similar; no `schema_migrations` table is created |

## 9. Phase V — Sprint 20 live verification (M1)

| ID | Scenario | Expected |
|----|----------|----------|
| AT-V.1 | Run completed | Every row of `.zecurity-obs/Sprint20/Live-Acceptance-Run-Sheet.md` has time, evidence and PASS/FAIL |
| AT-V.2 | Close-out | `.zecurity-obs/Sprint20/path.md` boxes ticked only for passing rows; failures filed in the Sprint 20 phase files' Post-Phase Fixes |
| AT-V.3 | Hygiene | Session Log entry; worktree and `/tmp/s20-live` removed; local DB reset before resuming Sprint 21 work |

## 10. Regression gate

```bash
buf generate                                                  # clean diff unless KI-2 option A
cd controller && go build ./... && go vet ./... && go test ./...
cd connector && cargo build && cargo test
cd relay && cargo build && cargo test
cargo build --manifest-path shield/Cargo.toml && cargo test --manifest-path shield/Cargo.toml
cd client && cargo build
cd admin && npm run build
cd provider-console && npm ci && npm run lint && npm test && npm run build
```

Existing suites that must stay green without weakening their assertions (signature updates allowed):
`internal/provider/*_test.go`, `internal/middleware/provider_test.go`, `internal/relay/*_test.go`, `internal/auth/*_test.go`, `internal/connector/*_test.go`, `internal/shield/*_test.go`, `internal/pki/*_test.go`, and the `connector` / `relay` / `shield` / `client` Rust suites.
