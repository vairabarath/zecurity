---
type: planning
status: planned
sprint: 21
tags:
  - sprint21
  - dependencies
  - execution-path
  - team-coordination
  - provider-dashboard
  - provider-identity
  - provider-read-api
  - provider-console
---

# Sprint 21 — Provider Dashboard Phase 2: Dedicated Provider Console (Login, Roles, Read-Only Fleet View)

> **Read this before writing a single line of code.**
> - **Scope source of truth:** `docs/provider-dashboard-architecture-decisions.md` → **"Decision record — 2026-09-23"** (D-01 … D-23).
> - **Evidence:** `docs/provider-dashboard-architecture-discovery.md`.
> - **Sprint 20 known issues carried into this sprint:** `.zecurity-obs/Sprint20/path.md` → **Known Issues** (KI-1 … KI-3).
>
> This sprint **introduces no architectural decisions**. Where this plan picks an *implementation* approach, it is marked as such and may be changed in code review without reopening the Decision Record. Anything that looks like a new decision is listed under **Open questions**. Stop and ask; don't decide it in code.

## Sprint Goal

Give provider operators a **dedicated provider console** (its own Vite + React project, D-18) with a **hardened login and a working role system**:
- super-admins decide who can sign in and with which role;
- operators then see **true fleet and tenant state**, read-only.

**Order, and why:**
1. **Secure the identity** (H).
2. **Stand up the dedicated console with login and roles** (C).
3. **Let super-admins manage operators** (U).
4. **Add the read APIs** (R) **and the read pages** (P).

Login and roles must be solid before Sprint 22 adds dangerous buttons (suspend tenant, relay revoke). The read APIs let a super-admin read across every tenant (D-15), so they come only after the identity is hardened.

**What exists today:**
- The tenant dashboard (`admin/`) has **tenant** roles only (`ADMIN` / `MEMBER` / `VIEWER`) and **no** provider UI, so nothing needs splitting out of it.
- The provider/tenant mixing is in the **backend**: provider login uses Google through the shared auth service, with the tenant `JWT_SECRET` and issuer. Phase H replaces it with **local provider accounts** and a provider-only token (Decision Record amendment 2026-09-26, D-24…D-29).

```text
TODAY                                                  AFTER THIS SPRINT
-----                                                  -----------------
Provider JWT signed with the tenant JWT_SECRET         Dedicated provider signing key + issuer (D-25)
  and the shared "zecurity-controller" issuer

Provider login = Google OAuth (external dependency)    Local accounts: email + password, Argon2id (D-24)

No logout; a stolen token lives its full TTL           Logout + session_generation revocation (D-25)

Provider API: create/revoke/delete relay, /me, /users  REST reads under /provider/*: relays, tenants,
  — nothing to READ fleet or tenants                     provider audit, certificate expiry (D-04/05/06/15)

No provider UI (admin/ is tenant-only)                Dedicated provider-console/ (Vite + React, D-18):
                                                         email + password login, forced first password change, role-aware nav

Operators only via PROVIDER_BOOTSTRAP_EMAILS           Create-only bootstrap super-admin (D-26); super-admins add operators, change roles,
  (super-admins only; relay-ops needs raw SQL)           disable / re-enable (audited, lock-out-proof)

No way to see fleet/tenant state                       Read-only pages: relays, tenants, audit,
                                                         certificate health

Sprint 20 KI-1 / KI-2 / KI-3; JWTs in .env.example     Fixed; .env.example scrubbed
```

## Development rule — schema changes reset the local database (Sprint 21 decision)

Zecurity is **pre-production**. For this phase the team has chosen to **reset the development database whenever the schema changes**. There is **no migration framework**: no `golang-migrate`, Goose or similar, and none may be added.

- A schema change is a new, numbered, plain SQL file in `controller/migrations/`. The next free number is **`037_`**.
  - The directory name is historical. **Nothing tracks which files have run and nothing upgrades an existing database.** Postgres runs every file in the directory, once, in lexical order, only when its data volume is first created (`docker-entrypoint-initdb.d`, `controller/docker-compose.yml`).
- **Any PR that adds a schema file requires every developer to recreate their local database:**
  ```bash
  cd controller
  docker compose down -v
  docker compose up -d
  ```
- Local development data is disposable during this phase. Re-seed with `PROVIDER_BOOTSTRAP_EMAIL` + `PROVIDER_BOOTSTRAP_PASSWORD` (Phase H) and a fresh tenant sign-up.
- A schema-changing PR must say so in its title or description (`[schema: reset DB]`), so reviewers and the other developer know to reset.
- DB-backed Go tests are unaffected: they create throwaway databases and run the SQL files from `controller/migrations/` themselves.
- Documenting this workflow is a sprint deliverable: **DEV-1**, the database development guide `docs/database-development.md` (below).

## Constraints from the Decision Record (binding)

| Ref | Constraint for this sprint |
|-----|----------------------------|
| **D-01** | Single provider, flat. No `provider_org`. Keep the `decide()` / `Target` seams. |
| **D-02** | Single controller instance. Process-local reads (e.g. the controller cert rotator's current cert, Valkey liveness) are fine. |
| **D-04** | Provider API is **REST under `/provider/*`**. Provider query services stay **independent of the HTTP layer**. No second GraphQL stack. |
| **D-05** | Keep `super-admin` / `relay-ops`. Add **read actions** to `decide()`. No role migration. |
| **D-06** | `tenant.*` is **super-admin only**, via the existing prefix rule with no special case. |
| **D-15** | Support access = **cross-tenant read APIs only**. No impersonation, no entry into tenant context. |
| **D-16 → D-24…D-29** | **Amended 2026-09-26.** Provider login is **local** (email + password, Argon2id). There is **no** Google `sub`/`hd` binding and no provider OAuth flow (D-24). **Kept:** a dedicated provider JWT key and issuer, and logout plus revocation via `session_generation` (D-25). Create-only bootstrap with a forced password change (D-26). Operator lifecycle incl. password reset (D-27). MFA, recovery codes, rotation policies and refresh tokens deferred to before Sprint 22 (D-28). Optional external IdPs later; don't block them (D-29). |
| **D-18** | Console is a **separate React app**: own build, own domain, network-locked. It may share components with `admin/`. |
| **D-21 / D-03** | No relay drain, no pools or regions. The relay list stays global. |
| **D-23** | Telemetry = **existing data only** (status, version, capacity label, `connection_count`/`max_connections`, last heartbeat, cert expiry). No new heartbeat fields. |
| **D-07…D-13, D-17** | Suspension, deletion, audit immutability roles and destructive-action safeguards are **not in this sprint**. The console is **read-only except operator management** (Phase U/C5). Don't change the `workspaces.status='active'` predicates. |
| **D-14** | Provider actions, incl. operator management, login, password changes, bootstrap and logout, are audited in `provider_audit_logs`. |

## Implementation choices made in this plan (not architectural — reviewable)

| Area | Choice | Why |
|------|--------|-----|
| Schema | `037_provider_local_auth.sql` adds `password_hash` (nullable, D-29), `must_change_password`, `password_changed_at`, `session_generation`, `last_login_at` to `provider_users`. | Additive; local DBs are reset (development rule). |
| Provider key | New env `PROVIDER_JWT_SECRET` (HS256, ≥ 32 bytes). Startup **fails** if it is missing, too short, or equal to `JWT_SECRET`. | D-25 "dedicated key"; HS256 matches the existing provider token code. |
| Provider issuer | New `appmeta.ProviderIssuer = "zecurity-provider"`. Tokens carry `iss=zecurity-provider`, `aud=provider`. | D-25 "dedicated issuer"; keeps `aud=provider` as a second wall. |
| Passwords | Argon2id (`m=64 MiB, t=3, p=2`), PHC string with parameters read on verify; policy 12–128 chars, not the email. Login is rate-limited in Valkey (5 failures per email, 20 per IP per 15 min → 429; fails closed if Valkey is down), and responses don't reveal whether an account exists (a dummy verify keeps timing equal). | D-24; a password endpoint must resist brute force and enumeration. |
| Identity service seam | `internal/provider/identity.go`: an `Authenticator` interface (Sprint 21: `LocalPasswordAuthenticator`) and `IdentityService.IssueSession`, the single JWT-minting path, which reads role, email and generation from the DB. Tokens carry `amr` (`["pwd"]`). No plugin registry or IdP tables yet. | D-24/D-29: authentication is pluggable, while roles, the JWT and revocation stay fixed. `amr` lets mandatory TOTP (D-28) be enforced without changing the token format. |
| Forced password change | A `must_change_password` login returns a **password-change-only** token (`pwc` claim, 10 min) that only `POST /provider/auth/password` accepts. | D-26 first-login change; the same flow serves operator temp passwords (U). |
| Revocation | Token claim `gen`. `RequireProvider` loads the user **by ID** and rejects if `gen ≠ session_generation`. Logout increments it. | The middleware already reads `provider_users` on every request, so this adds no extra lookup. |
| Console login | The console posts `{email, password}` to `POST /provider/auth/login` and receives a JSON token. There is no redirect or callback. | D-24: no OAuth flow. |
| CORS | `/provider/*` allows the single origin `PROVIDER_CONSOLE_ORIGIN`, with the Authorization header and no cookies. Dev uses the Vite proxy. | Separate domain (D-18); bearer token only. |
| Read services | New package `controller/internal/providerquery/`: plain Go + pgx, no `net/http`. Handlers in `internal/provider/` call it. | D-04 "query services independent of HTTP". |
| Read actions | `relay.read` (relay-ops gets it through the `relay.*` prefix), `tenant.read` (super-admin only through the prefix rule), existing `audit.view` (reused, not renamed), `cert.read` (super-admin: spans tenants). | D-05/D-06 with no special cases. |
| Relay liveness in reads | Last heartbeat = the fresher of `relays.last_heartbeat_at` and the Valkey key `relay:heartbeat:last:<id>`. | The DB write is throttled to 5 min (Sprint 20 Phase A). Valkey holds the real value. |
| Console stack | New top-level `provider-console/`: Vite, React 19, TypeScript, Tailwind 4, Radix (same versions as `admin/`). UI primitives are copied from `admin/src/components/ui/`, not extracted into a shared package. No Apollo: plain `fetch` against REST. | "Reuse admin UI where practical" without restructuring `admin/` into a workspace. |
| Console token | Held in memory, plus `sessionStorage` so a reload keeps it. 15 min provider TTL; **no refresh token**, so the user logs in again on expiry. | Refresh tokens are deferred (D-28). |
| Add operator / reset password | The server generates a random **temporary password**, stores only its hash with `must_change_password=true`, and returns it **once** (`Cache-Control: no-store`). The super-admin shares it out of band. | Super-admins never pick others' passwords, and there's no email infrastructure. |
| Bootstrap | `PROVIDER_BOOTSTRAP_EMAIL` + `PROVIDER_BOOTSTRAP_PASSWORD` create the first super-admin **only if the account doesn't exist**. A restart never overwrites it. Break-glass: `PROVIDER_BOOTSTRAP_RESET=true` resets that account (audited, loud log). | D-26; replaces `PROVIDER_BOOTSTRAP_EMAILS`, whose upsert re-activated accounts on every start. |
| Operator guards | No self-disable or self-demote; never zero active super-admins (checked under row locks); role, disable and enable changes bump `session_generation`; mutation + audit in one transaction. | Lock-out-proof, race-safe, and the console's cached role can't go stale. |
| Console writes | The console's only non-GET calls are logout and the four operator-management calls (super-admin). Everything else is read-only. | Operator management is the one write Sprint 21 needs. Other mutations wait for D-17 safeguards (Sprint 22). |

## Open questions (answer before the phase that needs them)

| # | Question | Needed by | Recommendation |
|---|----------|-----------|----------------|
| OQ-1 | Are **provider reads of tenant data** audited in `provider_audit_logs`? D-14 says "provider actions on tenants" are audited there, but doesn't say whether reads count. | M1-R3 (tenant endpoints) | Audit **tenant detail** reads (`tenant.read`, target = tenant) but not list pages. Detail is where support access happens (D-15). |
| OQ-2 | Does tenant detail include **tenant admin identities** (emails), or only counts and agents? | M1-R3 | Counts and agents only in Sprint 21. Add PII fields later, once OQ-1 is settled. |

## Team Assignments

| Member | Person | Role | Area |
|--------|--------|------|------|
| **M1** | Sathiya | Go + React | Sprint 20 live verification; **provider console** (foundation, login, roles, Provider users page, read pages); provider read actions and read APIs |
| **M2** | Barath | Go + Rust | Provider identity foundation (D-24…D-26, D-29 seam), incl. the console login endpoints; **operator management API** (D-27); KI-1, KI-2, KI-3; `.env.example` JWT cleanup; DEV-1 docs |

**Shared review** (both members approve): `internal/middleware/provider.go`, `decide()` read actions in `internal/provider/authz.go`, the operator-management guards, and the auth boundary tests (`AT-H`, `AT-U.1`, `AT-R.1`).

## Critical Rule: Conflict Zones

| File | Who | Rule |
|------|-----|------|
| `controller/cmd/server/main.go` | M2 (H: provider config, bootstrap, auth routes, CORS, provider Google routes removed; U: operator routes) + M1 (R: read routes) | Order: **M2-H → M2-U → M1-R3**. Each rebases onto the previous. Surgical edits only. |
| `controller/internal/middleware/provider.go` | M2 (H) | M2 owns; shared review. M1 only consumes `RequireProvider`. |
| `controller/internal/provider/session.go`, `store.go`, `handler.go` | M2 (H, U) | M2 owns. M1 adds **new** files (e.g. `read_handlers.go`) but doesn't edit these three. |
| `controller/internal/provider/authz.go` | M1 (R) | M1 adds the read actions and `Can…` methods; shared review. M2 doesn't edit. |
| `controller/internal/auth/provider_auth.go` | M2 (H) | **Deleted** by H (provider Google OAuth). Tenant auth files in `internal/auth/` are untouched. |
| `controller/internal/providerquery/` (new) | M1 (R) | M1 only. |
| `controller/internal/pki/controller_rotator.go` | M1 (R) | M1 adds a read-only accessor for the current cert's `NotAfter`/serial. Nothing else changes. |
| `controller/migrations/` | M2 (H) | **Only `037_provider_local_auth.sql`** this sprint. Never edit an existing schema file. The PR is marked `[schema: reset DB]`. |
| `controller/.env.example` | M2 (H, K) | M2 only: new provider env vars, KI-3 fix, JWT scrub. |
| `relay/src/**`, `connector/src/**`, `shield/src/**` | M2 (K) | M2 only. |
| `proto/**` | M2 (K, KI-2 option A only) | Additive only; **never renumber**; `buf generate` from repo root. |
| `provider-console/` (new) | M1 (C, P) | M1 only. `admin/` is **not** modified. |
| `docs/database-development.md` (new), `agent.md`, `controller/docker-compose.yml` comment | M2 (DEV-1) | Docs only. |

## Dependency Graph

```text
M1-V  Sprint 20 live verification        (Day 1; runs on the Sprint 20 code in a separate worktree)

M2-H  Provider identity foundation       (Day 1) ── identity service seam, schema 037, Argon2id, login + rate limit, forced change,
  │                                                  logout, key/issuer, generation, bootstrap, Google routes removed, CORS
  ├─► M1-C  Console foundation: dedicated app, login, logout, roles, nav   (C1–C4 need H)
  │           └─ C5 Provider users page ◄───────────────┐
  ├─► M2-U  Operator management API (add / role / disable / enable)  ──┘ (needs H)
  │
  └─► M1-R  Provider read actions + read APIs  (R1–R2 query services can start Day 1;
              R3 route wiring after H and U are merged)
                │
                ▼
        M1-P  Console read pages (needs C + R)

M2-K  Sprint 20 cleanup: KI-1, KI-2, KI-3, JWT scrub  (after U; depends only on H — .env.example)
DEV-1 Database development guide                     (with H — H ships the first schema change)

All phases → Acceptance gate (Acceptance-Test-Plan.md)
```

**Member order:**
- **Barath:** H → U → K (+ DEV-1 with H).
- **Sathiya:** V and R1–R2 (Day 1) → C1–C4 (after H) → C5 (after U) → R3–R4 → P.

**First demo (end of H + C + U):** the dedicated provider console, where the bootstrap super-admin signs in with email + password, changes the temporary password, adds a `relay-ops` operator (one-time temporary password), changes roles, disables, re-enables and resets passwords, and each person sees only what their role allows. This is the login-with-roles system required **before Sprint 22**.

> **Day-1 parallelism:** M2-H, M1-V, and M1-R's query-service layer (R1–R2, no routing) all start immediately. **If the sprint runs short, M2-K2 (KI-2) is the item that may move to Sprint 22.**

## Execution Path

### Phase V — M1: Sprint 20 Live Verification

> See [[Sprint21/Member1-Go/Phase1-Sprint20-Live-Verification]]. Runbook: `docs/sprint20-live-acceptance-runbook.md`.

- [ ] **M1-V1** Worktree at the Sprint 20 final commit (`46ab87d`), with prerequisites from runbook §3.1 (provider redirect URI, bootstrap email). These are **Sprint 20** settings: that code still uses Google provider login, which Sprint 21 Phase H removes.
- [ ] **M1-V2** Run runbook stages 1–8; fill in `.zecurity-obs/Sprint20/Live-Acceptance-Run-Sheet.md`.
- [ ] **M1-V3** Tick the passing boxes in `.zecurity-obs/Sprint20/path.md`. File failures as Post-Phase Fixes. Add a Session Log entry.

### Phase H — M2: Provider Identity Foundation (D-24…D-26, D-29 seam; D-25 keeps the D-16 key/issuer and revocation)

> See [[Sprint21/Member2-Go-Rust/Phase1-Provider-Identity-Foundation]].

- [ ] **M2-H1** `037_provider_local_auth.sql`: `password_hash` (nullable), `must_change_password`, `password_changed_at`, `session_generation BIGINT NOT NULL DEFAULT 1`, `last_login_at`. `[schema: reset DB]`.
- [ ] **M2-H2** Config: `PROVIDER_JWT_SECRET` (fatal if missing, < 32 bytes, or equal to `JWT_SECRET`), `PROVIDER_BOOTSTRAP_EMAIL`/`_PASSWORD`, `PROVIDER_CONSOLE_ORIGIN` (CORS only); warn if the removed `PROVIDER_GOOGLE_REDIRECT_URI` / `PROVIDER_BOOTSTRAP_EMAILS` are set. `appmeta.ProviderIssuer`.
- [ ] **M2-H3** Provider Identity Service seam: an `Authenticator` interface (local password is the only method) and `IssueSession`, the **only** place provider JWTs are minted. Argon2id password hashing (PHC), policy, dummy-hash timing. Tokens use the provider key and issuer with `gen`, `pwc` and `amr` claims.
- [ ] **M2-H4** `POST /provider/auth/login` (Valkey rate limit, no enumeration, audit on success), `POST /provider/auth/password` (forced and voluntary change → new token), `POST /provider/auth/logout`.
- [ ] **M2-H5** `RequireProvider`: load by ID; check `gen`, email and active status; confine `pwc` tokens to the password route. Create-only bootstrap + `PROVIDER_BOOTSTRAP_RESET` break-glass. `Disable` bumps the generation.
- [ ] **M2-H6** Delete the provider Google OAuth routes (`/provider/auth/initiate`, `/provider/auth/callback`, `provider_auth.go`) and the required `PROVIDER_GOOGLE_REDIRECT_URI`; add CORS for `/provider/*`.
- [ ] **M2-H7** Tests (auth boundary, Argon2id, rate limit, no enumeration, `pwc` scope, bootstrap; DB-backed); build gate.

### Phase C — M1: Provider Console Foundation (D-18, D-05, D-24…D-27)

> See [[Sprint21/Member1-Go/Phase2-Provider-Console-Foundation]].

- [ ] **M1-C1** `provider-console/`: a dedicated Vite + React 19 + TS + Tailwind 4 project; UI primitives copied from `admin/`; dev proxy `/provider` → `:8080` on port 5174.
- [ ] **M1-C2** Email + password login form (`POST /provider/auth/login`); forced Change password flow; 401 → Login; 403 → Forbidden / Change password; 429 message; logout; session countdown.
- [ ] **M1-C3** Roles: a single role matrix (`roles.ts`), `RequireRole` guard, role-aware nav.
- [ ] **M1-C4** Home: signed-in identity, role, last login, session expiry.
- [ ] **M1-C5** Provider users page (after M2-U): list, add operator, change role, disable/enable, confirmations, 409 messages.
- [ ] **M1-C6** Vitest tests (incl. API-surface test); README (own domain, network lock); build gate.

### Phase U — M2: Provider Operator Management (API)

> See [[Sprint21/Member2-Go-Rust/Phase2-Provider-Operator-Management]].

- [ ] **M2-U1** Store: `CreateOperator`, `ChangeRole`, `SetDisabled`, each transactional with an audit row and a `session_generation` bump.
- [ ] **M2-U2** Guards: no self-disable/demote/reset; never zero active super-admins (row-locked).
- [ ] **M2-U3** Routes (super-admin, `provider_user.manage`): `POST /provider/users` (returns a one-time temp password), `PATCH /provider/users/{id}`, `POST /provider/users/{id}/disable|enable|reset-password`; extend `GET /provider/users` (`last_login_at`, `must_change_password`, `has_password`; never `password_hash`).
- [ ] **M2-U4** Audit: `provider_user.create`, `.role_change`, `.disable`, `.enable`.
- [ ] **M2-U5** Tests (role matrix, guards incl. concurrency, audit only on success); build gate.

### Phase R — M1: Provider Read Actions + Read APIs (D-04, D-05, D-06, D-15)

> See [[Sprint21/Member1-Go/Phase3-Provider-Read-APIs]].

- [ ] **M1-R1** `decide()` read actions: `relay.read`, `tenant.read`, `cert.read` (+ reuse `audit.view`); `Can…` methods; authz tests.
- [ ] **M1-R2** `internal/providerquery/`: relay list/detail, tenant list/detail, provider audit query, certificate expiry. DB-backed tests.
- [ ] **M1-R3** Handlers + routes (after M2-H and M2-U merged): `GET /provider/relays`, `/provider/relays/{id}`, `/provider/tenants`, `/provider/tenants/{id}` (needs OQ-1/OQ-2), `/provider/audit`, `/provider/certificates`.
- [ ] **M1-R4** Tests: role matrix per endpoint, data correctness, pagination, no secret fields; build gate.

### Phase P — M1: Provider Console Read Pages (D-18, D-23)

> See [[Sprint21/Member1-Go/Phase4-Provider-Console-Read-Pages]].

- [ ] **M1-P1** Role-matrix entries: Relays (both roles); Tenants, Audit, Certificates (super-admin).
- [ ] **M1-P2** Read-only pages: Relays (list + detail), Tenants (list + detail), Audit, Certificates.
- [ ] **M1-P3** Vitest tests; build gate.

### Phase K — M2: Sprint 20 Cleanup

> See [[Sprint21/Member2-Go-Rust/Phase3-Sprint20-Cleanup]].

- [ ] **M2-K1** KI-1: fix relay renewal scheduling for the backdated `NotBefore` (Rust relay). Choose the option at phase start.
- [ ] **M2-K2** KI-2: wire the shield renewal window to the connector; add a shield-side debounce and a per-shield `ReEnroll` throttle on the connector.
- [ ] **M2-K3** KI-3: resolved by removal. Phase H deletes the provider Google login and `PROVIDER_GOOGLE_REDIRECT_URI` (D-24); confirm and close KI-3.
- [ ] **M2-K4** Remove committed JWTs and install commands containing tokens from `controller/.env.example`; add a CI grep guard.
- [ ] **M2-K5** Minor: stale "every 5 minutes" comment in `connector/src/crl.rs:30`.
- [ ] **M2-K6** Tests + build gate (relay, connector, shield, controller).

### DEV-1 — Database development guide (M2, lands with M2-H)

- [ ] **DEV-1a** `docs/database-development.md` (new), the database development guide. It covers:
  - **How the local database is built:** Postgres runs every SQL file in `controller/migrations/`, in lexical order, only when the `ztna_postgres` volume is first created. There is **no migration framework**, no record of applied files, and no upgrade path for an existing database.
  - **The rule:** any schema change means recreating the local DB (`cd controller && docker compose down -v && docker compose up -d`). Local data is disposable.
  - **Adding a schema change:** a new file with the next free number (`037_` at the start of Sprint 21); never edit an existing file; the PR is labelled `[schema: reset DB]`.
  - **After a reset:** re-seed with `PROVIDER_BOOTSTRAP_EMAIL` + `PROVIDER_BOOTSTRAP_PASSWORD` and a fresh tenant sign-up.
  - **How DB-backed Go tests get their schema:** throwaway databases, env vars; a skipped DB test fails acceptance.
  - **The existing duplicate prefixes** `016_`, `031_` and `034_`: they run in lexical order; leave them as they are.
  - **Scope:** this is a pre-production rule. Production upgrade strategy is out of scope and undecided.
- [ ] **DEV-1b** `agent.md`: a short "Local database" section that states the reset rule and links the guide.
- [ ] **DEV-1c** A one-line comment above the `./migrations` mount in `controller/docker-compose.yml` pointing to the guide.

## Final Build Gates

```bash
buf generate                                                  # only if KI-2 option A changes proto
cd controller && go build ./... && go vet ./... && go test ./...
cd connector && cargo build && cargo test
cd relay && cargo build && cargo test
cargo build --manifest-path shield/Cargo.toml && cargo test --manifest-path shield/Cargo.toml
cd client && cargo build
cd admin && npm run build                                     # unchanged; must still build
cd provider-console && npm ci && npm run lint && npm test && npm run build
```

DB-backed Go tests need the CI env vars (`ENROLLMENT_TEST_DATABASE_URL`, `SHIELD_TEST_DATABASE_URL`, `PKI_TEST_DATABASE_URL`, `AUTH_TEST_VALKEY_URL`). A DB test that **skips** does not count toward acceptance. After pulling any `[schema: reset DB]` PR, recreate the local DB before running the controller.

## Acceptance Criteria (sprint level)

- [ ] **The Provider Identity Foundation works end to end (D-24…D-26):**
  - every provider token is minted by the identity service (`IssueSession`), with `amr` recorded;
  - bootstrap creates the first super-admin once; first login forces a password change;
  - passwords are stored as Argon2id; login is rate-limited and doesn't reveal which accounts exist;
  - tokens are signed with the provider key and issuer, and tenant-signed or tenant-issuer tokens are rejected;
  - logout, disable, role change and password reset invalidate every outstanding token for that user;
  - the provider Google OAuth routes are gone, and tenant Google login is unaffected.
- [ ] **Provider read APIs return correct data** for relays (incl. true liveness), tenants, provider audit and certificate expiry, and **enforce the role matrix** (relay-ops: relays only; super-admin: everything).
- [ ] The **dedicated provider console** (`provider-console/`, own origin) handles email + password login, forced password change, logout and **role-aware** access: relay-ops sees relays only; super-admin sees everything.
- [ ] **Operator management works and is lock-out-proof:**
  - super-admins add operators (any role, one-time temporary password), change roles, disable, re-enable and reset passwords;
  - every change is audited (never with a password) and ends the affected user's sessions;
  - self-modification and removing the last super-admin are refused; `PROVIDER_BOOTSTRAP_RESET` is the break-glass path.
- [ ] The console **shows relays, tenants, audit and certificate health read-only**. Its only writes are the auth calls (login, change password, logout) and operator management.
- [ ] **KI-1, KI-2, KI-3 are fixed**, each with a regression test.
- [ ] **`controller/.env.example` contains no JWTs**, and a CI guard prevents re-adding them.
- [ ] **Development docs state** that a schema change requires recreating the local DB with `docker compose down -v && docker compose up -d`. A fresh reset boots the controller with `037_provider_local_auth.sql` applied.
- [ ] The **Sprint 20 live acceptance run** is complete and recorded (the Sprint 20 run sheet), with failures filed.
- [ ] All scenarios in [[Sprint21/Acceptance-Test-Plan]] pass.

## Out of Scope (tracked, not in this sprint)

- Provider **mutations** in the console other than operator management (relay create, revoke or delete stay API-only; tenant suspend and delete are Sprint 22+). D-17 safeguards come with those mutations.
- New provider roles or a finer role matrix (D-05 keeps two roles). `password_hash` is nullable so SSO-only operators can exist once an external IdP is added (D-29).
- Suspension (D-07/D-08), deletion (D-09…D-11, D-13), audit immutability roles (D-12).
- **Mandatory TOTP for `super-admin`**: required **before Sprint 22** (D-28). It's planned as the next step after Sprint 21, and takes priority over any external IdP. Backup recovery codes, password rotation policies and refresh tokens are deferred.
- External IdP login (OIDC, Google Workspace, Azure AD, Okta, Keycloak): later, as optional `Authenticator`s plugged into the identity service (D-29).
- A migration framework (**explicitly rejected** for the pre-production phase; see the development rule).
- Pools, regions, drain, relay intermediate CA (D-03, D-21, D-22).

## Notes for AI Agents Working on This Sprint

1. Read the phase file for the member's first unchecked phase whose `depends_on` items are all checked.
2. The Decision Record is binding. If you are about to choose something not covered by it or by "Implementation choices" above, stop and ask. OQ-1 and OQ-2 must be answered before M1-R3's tenant endpoints.
3. **Don't add migration tooling.** A schema change is a new numbered SQL file (`037+`) plus a `[schema: reset DB]` PR label. See `docs/database-development.md` once DEV-1 lands.
4. The console is read-only **except** logout and operator management (super-admin). No other button, form or route may call a mutating endpoint.
5. Never log, audit or return passwords (incl. temporary ones), `password_hash`, provider tokens, CA keys or `encrypted_*` columns.
6. On completion: tick the checkboxes here and in the phase file, record bug fixes in the phase file's **Post-Phase Fixes** and here under **Post-Sprint Fixes**, and add a Session Log entry.

## Post-Sprint Fixes

_None yet._
