---
type: phase
sprint: 27
owner: Sathiya (Person 2, M1)
features: [Sprint 26 follow-ups (F34), F35 provider API keys, F19 console per role]
phases: [X4, K1, K2, O2]
decisions: [D-71, D-72, D-73, D-74, D-76, D-30, D-40, D-50, D-53]
status: signed off 2026-11-23 (by Sathiya; Barath reviews in PR #252); planned — starts after Sprint 26 is merged and live-checked
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

Filled in after the Sprint 26 acceptance run. Each finding gets:
- a separate small change;
- its decision, if one is needed;
- a test;
- a live recheck.

Then tick F34.

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

---

## Post-Phase Fixes

_None yet._
