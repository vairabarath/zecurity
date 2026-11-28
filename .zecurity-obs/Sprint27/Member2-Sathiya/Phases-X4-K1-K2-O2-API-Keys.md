---
type: phase
sprint: 27
owner: Sathiya (Person 2, M1)
features: [Sprint 26 follow-ups (F34), F35 provider API keys, F19 console per role]
phases: [X4, K1, K2, O2]
decisions: [D-71, D-72, D-73, D-74, D-76, D-30, D-40, D-50, D-53]
status: signed off 2026-11-23 (by Sathiya; Barath reviews in PR #252); active since 2026-11-27 — X4 done (#260); K1 parts 1 and 2 (PR #264, stacked on G1 #263 → O1 #262), live PASS; K2 (PR #266), live PASS; O2 built (stacked on K2), live PASS, not committed yet
---

# Sprint 27 — Sathiya: X4 follow-ups, K1 API keys, K2 keys console, O2 console per role

Read `Sprint27/path.md` first (order, conflict zones, rules). Binding decisions: Decision Record amendment **2026-11-23, provider access**:
- **D-76** read-only API keys with the expiry alert;
- **D-72/D-73** the role matrix and roles (O2 mirrors them);
- **D-74** TOTP.

These build on **D-30** (console writes), **D-40** and **D-53**.

Order: **X4 → K1 (after O1 merges) → K2 → O2**. K1's expiry-alert part waits for Barath's `077_provider_alert_kinds`.

---

## X4 — Sprint 26 follow-ups

**Done (2026-11-27).** The Sprint 26 acceptance run (live, 2026-11-26, on `main` + the dev test clock `BILLING_TEST_NOW`) passed with one finding, fixed and merged in **#260** (`7ee717bc`). F34 is ticked.

- **Finding: a purged tenant's final invoice was missing.** The U1 usage prune deleted a purged tenant's `usage_tenants` snapshot (name, plan) as soon as it had no usage, but the month close needs it to draft the tenant's final invoice (D-67). **Fix:** the prune keeps the snapshot for 4 months after a purge; a DB test covers it. **Live recheck:** charlie (suspended, deleted and purged mid-month) got its final November invoice. Recorded in `Sprint26/path.md` → Post-Sprint Fixes and `Pending-Live-Tests.md`.
- **Also in #260:** the dev-only billing test clock (`BILLING_TEST_NOW`, refused outside `ENV=development`).
- **Not checked live** (unit tests cover them): CSV/JSON invoice export, the print view, the retention prune (nothing old enough).
- **Open for Barath's review** (recorded in the Sprint 26 phase files, not findings): `PlanForMonth` later-month step; regenerate keeps the month-start currency (D-63 vs D-64); prune refuses a cutoff inside retention; settings GET = `plan.read`; resend ignores `email_tenants`.

---

## K1 — Provider API keys (`076_provider_api_keys.sql`) — D-76

### What exists
- `middleware.RequireProvider` (session JWT, `session_generation`, `RequiresTOTP` on the DB row).
- `IssueSession` (`internal/provider/identity.go`): **not touched by K1**.
- O1's matrix and `IsWriteAction`; `decide()`.
- Provider audit (`provider.WithChange`); F26 evaluator and delivery; the provisioning-token "shown once, `no-store`" pattern.

### Build
- **`076_provider_api_keys.sql`**: `provider_api_keys`:
  - `id UUID PK`, `name TEXT NOT NULL` (1–64 characters, unique among active keys);
  - `role TEXT NOT NULL CHECK (role IN ('relay-ops','auditor','billing','support'))` (no `super-admin`, enforced in the DB too);
  - `prefix TEXT NOT NULL`, `secret_sha256 BYTEA NOT NULL`;
  - `created_by`, `created_at`;
  - `expires_at TIMESTAMPTZ NOT NULL CHECK (expires_at <= created_at + INTERVAL '365 days')`;
  - `last_used_at`, `last_used_ip`;
  - `allowed_cidrs CIDR[]`;
  - `revoked_at`, `revoked_by`, `revoke_reason`.
  - Grants for `ztna_app`.
- **Format:** `zpk_<id-without-dashes>_<base64url 32 random bytes>`. The prefix is the first 12 characters, shown in lists.
- **Middleware step** (before the session path in `RequireProvider`, or a wrapper):
  - `Authorization: Bearer zpk_…` → look up by id, constant-time compare SHA-256, check not revoked, not expired, source IP inside `allowed_cidrs` if set, rate limit (process-local token bucket per key; fails closed);
  - otherwise 401 with a generic `invalid_api_key` (no hint which check failed);
  - success → `Actor{Kind: api_key, KeyID, Name, Prefix, Role}`.
  - It never sets `amr`, never issues a session, and isn't accepted where a session-only route is mounted (auth, TOTP, operators, API keys) → 403.
- **`decide()`**: `actor.Kind == api_key && IsWriteAction(action)` → denied (D-76 read-only), whatever the role. Then the role matrix as for people.
- **Audit:** reads that write an audit row for people (`tenant.read`, `tenant.usage.read`, `usage.fleet.read`, `invoice.read`, `invoice.fleet.read`) write the same row with actor `api_key:<name> (<prefix>)` and `key_id` in details.
- **`last_used_at` / `last_used_ip`:** updated at most once a minute per key.
- **Management routes** (super-admin only; session actors only):
  - `GET /provider/api-keys` (no secrets);
  - `POST /provider/api-keys` (`{name, role, expires_at, allowed_cidrs?, reason}` → 201 with the full key **once**, `Cache-Control: no-store`);
  - `POST /provider/api-keys/{id}/revoke` (`{reason, confirm_name}`).
  - Actions `api_key.read`, `api_key.create`, `api_key.revoke`; audited with before/after (never the secret or hash).
- **Expiry alert (Q6, after `077_`):** an evaluator signal for `api_key.expiring`:
  - fires once per active key when `expires_at - now <= days_before` (14);
  - resolves when the key is revoked or expired, or when no longer within the window;
  - target = key id; payload = key name and prefix, role, expires_at; no secret.
- Wire in `cmd/server/main.go` (own block).

### Tests (DB-backed)
- **Create:**
  - the full key only in the 201 body;
  - the list shows the prefix only;
  - `super-admin` role refused (handler and DB CHECK);
  - expiry beyond 365 days refused;
  - missing reason → 400;
  - audit has no secret or hash.
- **Auth:**
  - a valid key → its role's reads work;
  - every write action → 403 for every role (table over all write actions);
  - revoked or expired → 401;
  - wrong secret or unknown id → 401 (same body);
  - outside the CIDR → 401;
  - a burst over the rate limit → 429, and a limiter error → refused.
- **Routes:** auth, TOTP, operators and API-key routes with a key → 403. **No call to `IssueSession`** (spy).
- **Audit:** `tenant.read` by a support key writes a row with the key as the actor.
- **Expiry alert:** fires once at 14 days (test clock); never for a revoked key; resolves on revoke; payload without the secret.
- **Mutation checks:**
  - the write refusal removed;
  - the `IssueSession` path reachable;
  - expiry not checked;
  - the CIDR check skipped;
  - the secret written to the audit;
  - a non-constant-time compare (lint or test hook);
  - the alert firing for a revoked key.

### Live check (K1)
- One key per role.
- **Reads and writes:**
  - a `relay-ops` key: `GET /provider/relays` 200, drain → 403;
  - an `auditor` key: `GET /provider/audit` 200;
  - a `billing` key: `GET /provider/invoices` 200, issue → 403;
  - a `support` key: tenant detail 200 (audited as the key).
- **Refusals:**
  - revoke → 401 on the next call;
  - expired → 401;
  - outside CIDR → 401;
  - `/provider/auth/*`, `/provider/users` and `/provider/api-keys` → 403 with a key.
- **Expiry alert:** a key expiring in under 14 days → one `api_key.expiring` alert (webhook and email, no secret); revoke → resolved.

---

### Implementation record (part 1: keys; 2026-11-27)

Branch `feat/sprint27-k1-api-keys`, **stacked on G1** (PR #263, itself on O1 #262; K1 uses O1's matrix and `IsWriteAction`, and its expiry alert needs G1's `077_`). It was rebased onto G1 on 2026-11-27 before its first push (conflicts in `authz.go`, `authz_matrix_test.go`, `main.go` and the docs: both sides added actions, routes and rows; kept both). The **expiry alert (`api_key.expiring`) is not built yet**: it needs G1's `077_provider_alert_kinds` (Barath). Everything else in "Build" is done.

- **`076_provider_api_keys.sql`:** the table as specified, plus:
  - a CHECK on the prefix shape and hash length;
  - a CHECK tying `revoked_at`, `revoked_by` and `revoke_reason` together;
  - a unique index on `lower(name)` among unrevoked keys;
  - a trigger that makes a key immutable except for last-use and revocation, and makes revocation terminal (SQLSTATE 42501).
- **Key format and auth (`internal/provider/apikeys.go`):**
  - `zpk_<32 hex>_<43 base64url>`; the prefix is the first 12 characters; the stored SHA-256 is of the whole key, compared with `subtle.ConstantTimeCompare`.
  - `APIKeyAuthenticator.Authenticate` checks, in order: shape, lookup by id, hash, not revoked, not expired, the role is a key role, source IP inside `allowed_cidrs` (when set), then the rate limit.
  - Every key failure is `ErrInvalidAPIKey` → **401 `{"error":"invalid_api_key"}`**, the same body for every cause.
  - The rate limit is a process-local token bucket (10/s, burst 30, D-02); over it, or on a limiter error, the request is refused (429 `rate_limited`, fail closed).
  - `last_used_at` and `last_used_ip` are written at most once a minute per key.
- **Middleware (`internal/middleware/provider.go`):**
  - A bearer token starting with `zpk_` is **never** passed to the JWT verifier. With the new `AcceptAPIKeys(auth)` option it is authenticated as a key; without it (and on password-change or MFA-stage routes) the response is **403 `api_key_not_allowed`**.
  - `RefuseProviderAPIKey` wraps the login route the same way.
  - The actor is `Actor{Kind: api_key, Role, KeyID, KeyName, KeyPrefix, Email: "api_key:<name> (<prefix>)"}`, with no `amr` and no session.
- **`decide()`:** an `api_key` actor is refused every write action (`IsWriteAction`, unknown = write) and is never treated as super-admin; then the role matrix applies as for people.
- **New actions:** `api_key.read`, `api_key.create`, `api_key.revoke` are super-admin only (no role row), so auditor is excluded from `api_key.read` in the O1 boundary test.
- **Routes (`cmd/server/main.go`):**
  - `requireProviderOrKey` wraps every provider route after the operator block (63 routes).
  - Session only: `/provider/auth/*` (login via `RefuseProviderAPIKey`), `/provider/me`, `/provider/users*` and the three key routes.
  - The wiring test `cmd/server/tenant_suspension_wiring_test.go` now expects `requireProviderOrKey(` on suspend and resume.
- **Key routes (`apikey_handlers.go`):**
  - `GET /provider/api-keys`: no secret, no hash; status `active`, `expired` or `revoked`.
  - `POST /provider/api-keys` (`{name, role, expires_at, allowed_cidrs?, reason}`): **201 `{api_key, key}` with `no-store`**. Refusals: 400 `invalid_role` (incl. super-admin), `invalid_expires_at` (past, or ≥ 365 days less a minute), `invalid_cidr`, `invalid_name`, `reason_required`; 409 `name_taken`.
  - `POST /provider/api-keys/{id}/revoke` (`{reason, confirm_name}`): 400 `confirmation_mismatch`; 409 `already_revoked`.
  - Audited `api_key.create` (reason; after: name, role, prefix, expiry, CIDRs) and `api_key.revoke` (reason, name, prefix; before/after status). Never the key or its hash.
- **Audit of key reads:**
  - `tenant.read`, the usage reads and the invoice reads go through `Actor.AuditUserID()` (nil for a key) and `Actor.AuditDetails()` (adds `key_id`), so a key's read is recorded as `api_key:<name> (<prefix>)`.
  - `InsertAuditTx` stores an empty user id as NULL.

**Tests:**
- Unit (`apikeys_test.go`):
  - every action × every key role: writes refused, reads follow the matrix, a super-admin key gets nothing;
  - key format and parsing (rejects upper-case hex, wrong lengths and wrong scheme);
  - create validation; `ipAllowed`; the token bucket (an unconfigured limiter errors);
  - `AuditUserID` / `AuditDetails`;
  - static guards: a constant-time compare, and no `IssueSession`, `IssueMFAToken` or `IssuePasswordChangeToken` call in the key code or the middleware.
- DB-backed e2e (`apikeys_e2e_test.go`, wired like `main.go`):
  - **create, list, revoke:** the key once with `no-store`, the list shows the prefix only, every refusal listed above, the DB refuses a super-admin key and a 366-day key, revocation is terminal in the DB, a revoked name can be reused, and no key or hash appears in the audit;
  - **auth:** each role's reads 200 and writes 403; session-only routes 403 for a key, incl. login, logout, password and TOTP;
  - **audit:** the support key's `tenant.read` is recorded as the key (NULL user id, `key_id`), and `last_used_at` is set;
  - **refusals:** wrong secret, unknown id and malformed keys all get the same 401 body; revoked → 401; expired → 401;
  - **no session path:** a spy verifier confirms no key ever reached the JWT verifier;
  - **CIDR and rate limit:** inside 200, outside 401; a burst of 3 then 429; a limiter error → 429.
- **Mutation checks (11, each caught):**
  - write refusal removed; `IssueSession` reachable from the middleware;
  - expiry not checked; revocation not checked; CIDR skipped;
  - the key in the create audit; a non-constant-time compare;
  - the rate limit ignored; a limiter error allowed;
  - keys accepted on session-only routes; super-admin allowed as a key role.

**Gate (2026-11-27):** `go build ./... && go vet ./...` clean; full `go test -count=1 ./...` with every DB variable set: 50 packages ok, 0 failures, only the 2 known hard-coded skips; the three key e2e tests ran (none skipped). (A first run failed in many DB packages at 0.00 s because the local Postgres was recreated mid-run, and on the wiring test that expected `requireProvider(` on suspend/resume, now `requireProviderOrKey(`; the re-run is clean.)

**Live check (2026-11-27): PASS, 52 of 52 checks.**
- **Setup:** local controller from the branch (sha256 `55b0010a…`) on a fresh DB through `076_`; driver `~/.cache/zecurity-s27/k1live.py`.
- **Create:** one key per role (shown once); a super-admin key → 400 `invalid_role`; a 366-day key → 400; the list shows prefixes only.
- **Reads and writes:**
  - relay-ops key: relays 200, drain 403, tenants 403;
  - auditor key: audit 200, invoices 200, relay create 403;
  - billing key: invoices 200, regenerate 403, settings change 403;
  - support key: tenant detail 200 (audited as `api_key:k1-support (zpk_…)`), invoices 403, suspend 403.
- **Session-only routes:** `/provider/me`, `/provider/users`, `/provider/api-keys`, login, logout and TOTP verify → 403 for every key.
- **Refusals:** wrong secret → 401; revoked → 401 on the next call; outside its CIDR → 401; a 70-second key 200 before expiry and 401 after; a burst of 45 calls → 429 after the first 30.
- **No leaks:** the audit has `api_key.create` rows and no key; the controller log has no key.
- The first run's driver mis-compared tuple results ("FAIL" where the printed value equals the wanted one, 12 lines); the helper was fixed and every value checked: 52 as expected, 0 real failures.
- Not run yet: the expiry alert (after `077_`), the console (K2).

### Implementation record (part 2: expiry alert; 2026-11-27)

Same branch, on top of G1's `077_` (which seeds the rule: warning, enabled, `{"days_before": 14}`, fire 0 s, renotify 0, id `…0012`).

- **Signal** `alerting.Sources.apiKeyExpiring` replaces G1's "not evaluated" placeholder.
  - One observation per key that is not revoked, not yet expired and expires within `days_before` days of the evaluator's clock (14 by default).
  - Target `api_key` / key id / key name.
  - Summary: `Provider API key <name> (<prefix>, role <role>) expires at <RFC3339>`. Never the key or its hash.
- **Fires once per key** (renotify 0). **Resolves** when the key is revoked, expires, or leaves the window (a replaced key is revoked).
- **Tests:**
  - `internal/alerting/apikey_alert_db_test.go` (test clock): the seed is as agreed; keys 10 days and exactly 14 days out fire, 20 days out doesn't, a revoked key and an expired key never fire; the payload has key, prefix and role but no hash; no re-fire on later ticks.
  - Resolves on revoke; the 20-day key fires once it enters the window; resolves on expiry; resolves when `days_before` is lowered.
  - `TestNewProviderKinds` no longer expects "not evaluated".
- **Mutation checks (5, each caught):** fires for a revoked key; fires for an expired key; window ignored; hash in the payload; signal not wired.
- **Gate (2026-11-27, combined O1 + G1 + K1):** `go build`, `go vet`, full `go test -count=1 ./...` with every DB variable: 51 packages ok, 0 failures, the 2 known skips; `TestAPIKeyExpiringDB`, the three key e2e tests and the break-glass tests ran.
- **Live check (2026-11-27): PASS.**
  - **Setup:** controller from the branch (sha256 `b43fa0f0…`, eval 10 s) on ports 18080/19090, because another controller started from the Orca app held 8080/9090 on the same local DB. Fresh DB through `077_`; local webhook receiver; mail capture.
  - **Channels:** a webhook channel, and an email channel to `ops@lab.test` only (so a delivery claimed by the other controller could not reach a real inbox).
  - **Keys:** two auditor keys, expiring in 10 and 30 days.
  - **Fired:** within one tick the webhook got one `api_key.expiring` (firing, warning, platform) for the 10-day key only, delivered 204, and one email reached `ops@lab.test`. Both name the key and prefix, never the key. No re-fire after 25 s more.
  - **Resolved:** revoking the key delivered `resolved` by webhook and email. Both deliveries went through this controller (both emails are in the local capture).
  - The controller log has no key.


---

## K2 — API keys console

### Build
- `src/api/apiKeys.ts` (`listApiKeys`, `createApiKey`, `revokeApiKey`), all listed in `api-surface.test.ts` on purpose (writes per D-30 as amended).
- **Settings → API keys** (super-admin nav):
  - the table shows name, role, prefix, created by and at, expires (with "expires in N days" and a warning under 14), last used, status;
  - empty state: "No API keys. Create one for a script or integration that only needs to read."
- **Create key dialog:**
  - name, role (relay-ops / auditor / billing / support with one line each on what it can read), expiry (presets 30 / 90 / 365 days or a date, at most 365), optional CIDRs, reason;
  - then a **show-once** dialog with the full key, a copy button and "You won't see this key again";
  - this dialog joins the clipboard allowlist in `static-guards.test.ts`; the key is never stored in session or local storage.
- **Revoke dialog:** reason and typed key name (D-17 style).

### Tests
- Create sends the right body; the show-once dialog clears the key on close and never writes it to storage (session-storage dump check); revoke needs the typed name; the expiry presets; the "expires soon" warning; relay-ops and the new roles see no API keys page.
- `npm test`, `npm run lint`, `npm run build`.

### Live check (K2)
Create, copy, use (a curl read), revoke from the console; the key isn't visible after the dialog closes.

### Implementation record (2026-11-27)

Branch `feat/sprint27-k2-keys-console`, **stacked on K1** (PR #264 → G1 #263 → O1 #262). Console only; no controller change.

- **`src/api/apiKeys.ts`:** `listApiKeys`, `createApiKey`, `revokeApiKey` (+ the `ApiKey` type, `API_KEY_ROLES`). `api-surface.test.ts` lists the two writes (`POST /provider/api-keys`, `POST /provider/api-keys/{id}/revoke`) and the read on purpose.
- **Settings → API keys** (`src/pages/APIKeys.tsx`, `/settings/api-keys`, section `api-keys` under Governance, super-admin only in `roles.ts`):
  - the table shows name (and CIDRs), role, the 12-character prefix only, created at and by, expiry with "expires in N days" (warning style under 14, `data-testid="expires-soon"`), last used (time and IP, or "Never"), status, and Revoke for unrevoked keys;
  - empty state and an info panel (read-only, shown once, alert 14 days before expiry, audited).
- **`src/components/APIKeyDialogs.tsx`:**
  - **Create:** name; role with one line on what it can read; expiry presets 30 / 90 / 365 days (default 90) or a date, at most 365 days; optional CIDRs (commas or new lines, shape-checked; the server validates); reason. `allowed_cidrs` is left out when empty.
  - **Show once** (`KeyShownOnceDialog`): the full key, a Copy button and "You won't see this key again". The key lives only in the dialog's props: closing drops it, and it is never stored. This dialog joins the clipboard allowlist in `static-guards.test.ts`.
  - **Revoke:** `ConfirmDangerDialog` with a reason and the key name typed exactly.
- **Choices:**
  - The 365-day preset ends 10 minutes short of a year, because the server refuses ≥ 365 days less a minute.
  - A picked date expires at 23:59 UTC that day.
  - `ROLE_LABELS` gains Auditor / Billing / Support (the key roles; O2 does the navigation).
- **Tests** (`src/pages/APIKeys.test.tsx`, 9):
  - **access:** super-admin nav entry; relay-ops get Forbidden, no nav entry, no key requests;
  - **list:** fields, prefix only, the expires-soon warning, revoked rows without Revoke;
  - **create:** the exact body (role, 30-day expiry, CIDRs, reason); the key shown once, absent from session and local storage, gone from the page after close; the default 90 days with CIDRs omitted; a bad CIDR blocks submit; a server error (`name_taken`) stays in the dialog;
  - **revoke:** blocked until the full name is typed, then sends reason + `confirm_name`;
  - **format helpers:** presets inside the limit, expiry texts, the 14-day warning.
- **Mutation checks (7, each caught):** key kept on the page after close; key written to sessionStorage; revoke without the typed name; create without a reason; the 365-day preset over the limit; the expires-soon warning removed; the page opened to relay-ops.
- **Gate:** `npm test` 66 files / 865 tests pass; `npm run lint` clean; `npm run build` OK.
- **Live check (2026-11-27): PASS.**
  - **Setup:** controller from the branch (sha256 `aa2c25c1…`) on :8080 with a fresh DB through `077_`, console dev server on :5174, driven in Orca's built-in browser; signed in as the super-admin with password + TOTP.
  - **Page:** API keys appears under Governance; the empty state is shown.
  - **Create:** created `k2-live-reader` (Support, 30 days, reason). The show-once dialog displayed an 80-character `zpk_…` key, captured straight into a 0600 file, never printed.
  - **Use:** `curl` with it: `GET /provider/tenants` 200; `GET /provider/invoices`, `POST /provider/tenants` and `GET /provider/me` 403.
  - **After closing:** the key isn't in the DOM, sessionStorage or localStorage. The list shows `zpk_770ae338…`, Support and "expires in 29 days"; after a reload, Last used shows the curl call (`::1`).
  - **Revoke:** "Revoke key" stayed disabled with a partly typed name and worked with the full name. The row shows "Revoked by …", the toast "API key k2-live-reader revoked." appeared, and the next curl call got 401 `invalid_api_key`.
  - **Audit:** `api_key.create` and `api_key.revoke` with their reasons; no key in the audit or the controller log.

---

## O2 — Console per role — D-72, D-73

### Build
- `src/auth/roles.ts`: `PROVIDER_ROLES` gains `auditor`, `billing`, `support` with labels. Page access follows the matrix:
  - **auditor**: every page, write buttons hidden;
  - **billing**: Plans, Billing settings, Invoices, Usage, Tenants (list; plan column; plan assignment);
  - **support**: Tenants (list, detail), the tenant's alerts and timeline, Health.
- Routes guarded by role (`guards.tsx`); a page outside the role → `/forbidden`. Hiding is UX only; the API refuses anyway.
- **Operator dialogs:** the role select offers the new roles with one line each; TOTP note ("must set up two-step verification at first sign-in").
- **Break-glass banner** (with G1's endpoints): super-admins see "Break-glass recovery used for <email> at <time>: <reason>" with **Acknowledge** (note required).

### Tests
- Navigation per role, `/forbidden` for pages outside the role, no write buttons for auditor and support, the operator dialog role options, the banner and its acknowledgement.
- `npm test`, `npm run lint`, `npm run build`.

### Live check (O2)
- Sign in as each new role: the nav shows exactly its pages and there are no write buttons where it can't write.
- The break-glass banner appears after G1's live step and clears on acknowledge.

### Implementation record (2026-11-28)

Branch `feat/sprint27-o2-console-roles`, **stacked on K2** (PR #266 → K1 #264 → G1 #263 → O1 #262). Mostly console; one small controller read change.

- **The role matrix in the console:**
  - `src/auth/role-matrix.json` is a copy of the server's `roleActions`.
  - **`TestConsoleRoleMatrix`** (controller, `internal/provider/console_matrix_test.go`) fails if the two differ, so the console can't drift.
  - `roles.ts`:
    - `PROVIDER_ROLES` = 5 roles; `ROLE_LABELS`, `ROLE_HELP` (one line each); `TOTP_REQUIRED_ROLES`;
    - **`can(role, action)`** uses the server's action names; super-admin may do everything; an unknown role nothing;
    - `rolesWith(action)`.
  - `tsconfig.app.json` gains `resolveJsonModule`.
- **Pages per role:**
  - Each section is shown to `rolesWith(<the read its page needs>)`, not to a named list:
    - alerts / timeline: `alert.read`; relays and pools: `relay.read`;
    - tenants: `tenant.list.read`; plans and billing settings: `plan.read`; usage: `tenant.usage.read`; invoices: `invoice.read`;
    - certificates: `cert.read`; audit: `audit.view`; operators: `provider_user.manage`; API keys: `api_key.read`.
  - The tenant detail route needs `tenant.read` (`TENANT_DETAIL_ROLES`), so billing gets Forbidden there.
  - A page outside the role → `/forbidden` (existing `RequireRole`).
- **Write buttons follow `can()`:**
  - **relay components:** drain/resume `relay.drain`, revoke/delete `relay.revoke`, move `relay.move`, pools `relay.pool.*`, Add relay `relay.create`;
  - **tenant sections:** suspend, delete, quotas, pool, plan, first-admin, Create tenant, each by its own action;
  - **Plans:** New card / Duplicate `plan.create`; Edit / Remove price `plan.update`; Delete `plan.delete`; Make default `plan.set_default`; the "Change the billing currency" link `billing.settings.write`;
  - **Invoices:** Regenerate, Issue, Mark paid, Void, Resend by `invoice.write`;
  - **Billing settings:** a **read-only mode** (fields disabled, no reason, no Save) without `billing.settings.write`;
  - **Alerts:**
    - Rules tab `alert.rule.read` with Edit by `alert.rule.update`; Channels tab `alert.channel.read` with its buttons by `alert.channel.update`;
    - the scope filter only in the full view; tenant names by `tenant.read`;
    - support's description says "Tenant alerts…".
  - **Timeline:** tenant, rule and channel links and the tenant focus each follow their read; support gets a tenant-only description.
  - **Overview:** each tile is read only by roles allowed it (relays and pools, firing alerts, certificates, audit, health, the trend series), so billing makes **no** Overview requests and support only alerts + health. "Your access" text and a link for relay-ops, billing and support.
- **Billing on the tenant list (D-73 "tenant list (name, plan, status)"):**
  - **controller:** `TenantSummary.current_plan {id, name}`, from a join in the list/detail query; named so to keep the response guard against any `lan_` key fragment; DTO surface test updated; new `TestListTenants_CurrentPlan`;
  - **console:** a Plan column for everyone; **Change plan** on the list only for roles with `tenant.plan.assign` and without `tenant.read` (billing; super-admins change it on the tenant page). The dialog is the existing `ChangePlanDialog`, exported with a minimal tenant shape. Names link to the detail only with `tenant.read`.
- **Operator dialogs:** the role select lists the 5 roles with a help line, plus "Must set up two-step verification at first sign-in" for the TOTP roles. Operator counts gain "Other roles".
- **Break-glass banner (G1's API):**
  - `src/api/breakGlass.ts`: list, acknowledge (in `api-surface.test.ts`);
  - `components/BreakGlassBanner.tsx` in `Layout`, only for `break_glass.read` (super-admins): "Break-glass recovery used for <email> at <time>: <reason>", host, and **Acknowledge** with a required note; `already_acknowledged` closes it quietly.
  - **Test helper:** `test/fetch.ts` answers `GET /provider/break-glass` with no events when a test doesn't route it, and records it in `bannerCalls`, not `calls`, so page tests keep counting only the page's requests.
- **Existing tests updated:** RelayDetail used `auditor` as its unknown-role example (now `owner`); a Tenants fixture gained a plan.
- **Tests** (`src/pages/ConsolePerRole.test.tsx`, 24):
  - the 5 roles and help; `can()` spot checks;
  - **navigation:** exactly the pages per role (relay-ops, auditor, billing, support);
  - **Forbidden:** pages outside each role, with no request;
  - **Overview:** billing none, support alerts + health, auditor its 7 reads;
  - **write buttons:**
    - auditor: Plans (no New card, edit, delete, default or currency link), Billing settings (read-only), Invoices (no Regenerate), Alerts rules (no Edit);
    - billing: Plans (create and edit, no delete or default); the tenant list shows the plan and Change plan, with no detail link;
    - support: the tenant list (link, no Change plan, no Create tenant); Alerts (Active/History only, tenant wording);
  - **operator dialog:** help line, no two-step note for relay-ops;
  - **banner:** shown to super-admin, acknowledge needs a note, sends it, and the banner clears; no other role reads it.
- **Mutation checks (9, each caught):** console matrix drift (Go test); tenants page for every role; auditor gets New card; billing settings never read-only; banner for every role; billing gets detail links; Overview reads relays for every role; ack without a note; tenant detail route open to billing.
- **Gate:**
  - console: `npm test` 67 files / 889 tests, `npm run lint` clean, `npm run build` OK;
  - controller: full `go test -count=1 ./...` with every DB variable: 51 packages ok, 0 failures, the 2 known skips; `TestConsoleRoleMatrix` and `TestListTenants_CurrentPlan` ran.
- **Live check (2026-11-28): PASS.**
  - **Setup:** controller from the branch (sha256 `dd5c539f…`) on a fresh DB through `077_`; console dev server; Orca's built-in browser; driver `~/.cache/zecurity-s27/o2ui.py` (each role in its own tab, password + TOTP via stdin).
  - **auditor:**
    - nav: Overview, Alerts, Timeline, Relays, Relay pools, Tenants, Plans, Usage, Invoices, Billing settings, Certificates, Audit log;
    - Plans showed only History, plus the currency link, then fixed (above);
    - Billing settings read-only; Alerts rules without Edit; Operators and API keys Forbidden.
  - **billing:** nav: Overview, Tenants, Plans, Usage, Invoices, Billing settings. Tenants: "Change plan of S27 Acme", no detail link. Plans: New card, Edit, Duplicate, Remove price, no Delete or Make default. Invoices: Regenerate drafts. Relays and Alerts Forbidden.
  - **support:** nav: Overview, Alerts, Timeline, Tenants. The tenant name links to the detail. Alerts: Active and History only. Invoices and Plans Forbidden.
  - **relay-ops:** unchanged (Add relay; Tenants Forbidden).
  - **Banner:** `recover-admin --email bg@s27.test --reason "O2 live check: lost phone"`, then the main super-admin saw the banner with the operator and reason. Acknowledge was disabled until a note was typed; after acknowledging, the banner cleared. Audit `break_glass.acknowledge` has the note; the temporary password never appeared on the page.

---

## Post-Phase Fixes

_None yet._
