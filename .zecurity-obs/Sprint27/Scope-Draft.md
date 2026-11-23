---
type: sprint-scope-draft
sprint: 27
title: Provider Dashboard — provider access and API (F19, F20, F35)
status: DRAFT — theme chosen 2026-11-23 (provider access); Q1–Q6 answered 2026-11-23; D-71 … D-76 are proposals until signed off
baseline: Sprint 26 docs (#249, D-62 … D-70); Sprint 25 stack #244 … #248 and Sprint 26 code not merged yet
---

# Sprint 27 — scope draft

> **Status: draft.** Theme chosen on 2026-11-23: **provider access and API**, three Can-have features that each need a decision (⚖️). All six review questions were answered on 2026-11-23 (see "Answered"). D-71 … D-76 below are still **proposals** until sign-off. The binding plan will be `Sprint27/path.md`, the phase files and a Decision Record amendment, after sign-off.
>
> **Prerequisites before any Sprint 27 code:**
> 1. Sprint 26 (F34) merged and live-checked; its findings become Sprint 27 **X4**.
> 2. The Sprint 24 and Sprint 25 acceptance runs closed.
>
> F34 matters here because the proposed `billing` role (D-73) gives access to F34's plan cards, settings and invoices.

## Goal

Today the provider console has **two roles** (D-05, D-31):
- `super-admin` can do everything;
- `relay-ops` gets `relay.*` except revoke and delete, plus the alert list and the timeline's relay rows.

Every provider request comes from a person signed in with a password, plus TOTP for super-admins (D-24 … D-28).

Sprint 27 makes provider access fit how a provider actually works:
- **F19 more roles:** people who need to read everything (audits, compliance), run billing, or help tenants (support) get a role that does exactly that. They don't get super-admin.
- **F20 break-glass:** the emergency path (`recover-admin` on the controller host) stays the only way back in. Every use becomes **loud**: an alert, an email to every super-admin, and a banner in the console.
- **F35 API keys:** scripts and integrations (monitoring, billing export, reporting) call the provider API with a revocable, expiring, **read-only** key **scoped to one role**, without a person's password or TOTP and without a session.

| Feature | Today | Proposed phases | Size |
|---|---|---|---|
| **F19** Provider admin roles | ◐ two roles; a prefix rule in `decide()`; the `provider_users.role` CHECK allows two values (`025_`) | **O1** role matrix + new roles, **O2** console per role | 🟡 Medium (migration, authz, console) |
| **F20** Break-glass account | ◐ `zecurity-controller provider recover-admin` on the controller host, audited `provider_user.recover_admin` by `system:cli` | **G1** loud break-glass | 🟢 Small (alert kind, email, banner) |
| **F35** Provider API + API keys | ◐ the REST API exists; only session tokens authenticate | **K1** API keys (controller), **K2** keys console | 🟡 Medium (migration, auth middleware, console) |
| **Sprint 26 follow-ups** | Sprint 26 acceptance run not done | **X4** | 🟢 Small |

**No agent change** (relay, connector, shield and client are untouched). **No external IdP** (D-29 stays "later"). **No impersonation or support grants** (F31 is a separate sprint).

## What already exists (reuse, don't rebuild)

| Building block | Where | Used by |
|---|---|---|
| `decide(actor, action, target)`: the one authorization chokepoint; prefix rule plus `relayOpsAlsoAllowed` and `superAdminOnlyRelayActions` | `controller/internal/provider/authz.go` | O1 replaces the prefix rule with an explicit matrix; same chokepoint |
| Roles CHECK `('super-admin', 'relay-ops')`, one role per operator | `025_provider_users.sql` | O1: a new migration widens it (never edit 025) |
| Operator management with lock-out guards (no self-demotion, never zero super-admins), role change bumps `session_generation` | `internal/provider` (D-25, D-27) | O1/O2: a new role is assigned the same way |
| `IssueSession` is the only path to a full session; super-admin needs `otp` in `amr` | `internal/provider/identity.go` | K1: an API key **never** goes through it and never yields a session |
| TOTP enrolment, enforcement (`RequiresTOTP`), reset by another super-admin | `internal/provider/totp*.go` | O1: TOTP for the new roles (D-74) |
| `recover-admin` CLI: temporary password, clears TOTP, ends sessions, audited | `cmd/server/recover_admin.go` | G1 adds the alert, email and banner |
| Alert kinds, evaluator, email and webhook delivery (F26) | `internal/alerting` (D-50 … D-53) | G1: a platform event `provider.break_glass_used`; K1 maybe a key-expiry warning |
| F14 SMTP sender to active super-admins | `invitation.Emailer.SendPlain`, `certalert` | G1 email |
| Provider audit with before/after, append-only (F17) | `provider_audit_logs` (`051_`) | All writes; K1 audits the key as the actor |
| Console role gating (`roles.ts`, `Layout.tsx`, `api-surface.test.ts`) | provider console | O2, K2 |

## Proposed phases

### X4 — Sprint 26 follow-ups (first, small)
Whatever the Sprint 26 acceptance run finds. Then tick F34.

### O1 — Role matrix and new roles (controller, migration `075_provider_roles`)
- **Explicit matrix:** `decide()` reads a table: role → set of allowed actions (and action prefixes where a whole namespace is meant). It replaces today's prefix rule plus its two exception maps.
  - Existing roles keep **exactly** today's behaviour. A table-driven test lists every action constant and its expected outcome per role, so nothing changes by accident.
- **New roles (D-72, D-73):** `auditor`, `billing` and `support`. `075_provider_roles` widens the role CHECK in a new file.
- **Unchanged rules:**
  - Only super-admins manage operators.
  - The lock-out guards stay (no self-demotion, never zero active super-admins).
  - A role change still bumps `session_generation` (ends that person's sessions).
- **TOTP (D-74):** `RequiresTOTP` covers the new roles.
- **Tests (DB-backed):**
  - the full action × role table;
  - an unknown action is denied for every role except super-admin;
  - a new action constant without a matrix entry fails a test (so new endpoints can't silently open to a role);
  - role change ends sessions;
  - TOTP required for the new roles;
  - the lock-out guards still hold.

### O2 — Console per role (provider console)
- `roles.ts` and navigation follow the matrix:
  - **auditor** sees every page read-only, with no write buttons;
  - **billing** sees Plans, Billing settings, Invoices, Usage and the tenant list (name, plan, status), and no relay or tenant actions;
  - **support** sees Tenants (list and detail, with connectors, shields and agents), the tenant's alerts and timeline rows, and Health; no billing pages, no audit log, no write buttons.
- The operator dialogs offer the new roles, with one line on what each can do.
- Hiding buttons stays **UX only**; `decide()` is the control (as today). `api-surface.test.ts` lists any new calls.

### G1 — Loud break-glass (controller + console)
- `recover-admin` gains a required `--reason`. It is stored in the audit row (`provider_user.recover_admin`, actor `system:cli`) and never printed elsewhere.
- Every use:
  - raises a **platform** alert event **`provider.break_glass_used`**: severity critical, delivered to every matching channel (F26), no secrets in the payload;
  - emails **every active super-admin** (F14 sender): who was recovered, when, and the reason;
  - shows a **console banner** to super-admins until one of them acknowledges it (with a note; audited).
- The alert kind needs a new migration if the kinds CHECK lists platform kinds (`077_break_glass_alert_kind`; never edit `069_` or `072_`).
- **No standing break-glass account** (D-75): the controller host is the break-glass boundary.

### K1 — Provider API keys (controller, migration `076_provider_api_keys`)
- **Table** `provider_api_keys`:
  - `id`, `name`, `role`, `prefix` (first characters, shown in lists), `secret_hash`;
  - `created_by`, `created_at`, `expires_at` (required), `last_used_at`, `last_used_ip`;
  - `revoked_at`, `revoked_by`, `revoke_reason`; optional `allowed_cidrs`.
- **Format:** `zpk_<id>_<secret>`, with a 256-bit random secret.
  - Only a SHA-256 of the secret is stored (enough for a high-entropy secret; constant-time compare).
  - The full key is **shown once** at creation (`Cache-Control: no-store`), never logged, audited or returned again (like the provisioning token rule).
- **Authentication:** `Authorization: Bearer zpk_…` is handled by its own middleware, before the session path. It yields an `Actor` with `Kind = api_key`, the key's role and the key id.
  - It **never calls `IssueSession`** and never yields a session or a refresh.
  - The auth, TOTP, operator-management and API-key routes refuse it (403).
  - `decide()` applies the key's role exactly as for a person.
- **Scope (D-76):**
  - A key has one role from `relay-ops`, `auditor`, `billing`, `support`; **never `super-admin`**.
  - **Read-only (Q2):** a key may use only the **read** actions of its role. `decide()` refuses every write action for an `api_key` actor, whatever the role. Writes by key are a later decision.
  - Expiry is required for every key, at most 365 days (Q3).
  - Optional source CIDR allowlist.
  - Rate-limited per key (fails closed).
- **Audit:**
  - keys can't write, so the audit trail for a key is its reads: those audited for people (e.g. `tenant.read`, `tenant.usage.read`, `invoice.read`) are audited the same way for keys, with actor `api_key:<name> (<prefix>)` and the key id;
  - `last_used_at` is updated at most once a minute.
- **Management:** super-admin only (`api_key.create`, `api_key.revoke`, `api_key.read`), with a reason and before/after audit. Revoking takes effect immediately.
- **Expiry warning (Q6, this sprint):** a built-in F26 **platform** rule kind `api_key.expiring`: fires once per key 14 days before `expires_at` (email and webhook to the provider; the payload names the key and prefix, never the secret), resolves when the key is revoked, expires or is replaced. It needs the alert-kind migration (with G1's `077_`, or its own file). Revoked keys never alert.
- **Tests (DB-backed):**
  - a key authenticates with its role's **read** actions only; every write → 403, for every key role;
  - a revoked or expired key → 401;
  - a key can't reach auth, TOTP, operator or key routes;
  - a super-admin key can't be created;
  - the secret is never in logs, audit or responses after creation;
  - the CIDR allowlist is enforced;
  - the rate limit fails closed;
  - `IssueSession` is never called for a key (mutation check).

### K2 — API keys console (provider console)
- **Settings → API keys** (super-admin):
  - the list shows name, role, prefix, created, expires, last used and status;
  - **Create key** (name, role, expiry, optional CIDRs, reason) opens a show-once dialog with copy (an addition to the clipboard allowlist in `static-guards.test.ts`, like the provisioning token);
  - **Revoke** (reason, typed key name).
- Writes go into `api-surface.test.ts` on purpose (D-30 amended).

## Decisions to make first (Phase 0, proposed D-71 … D-76)

| Proposed ID | Question | Recommendation |
|---|---|---|
| **D-71** | Scope of Sprint 27 | F19 (`auditor`, `billing` roles and an explicit role matrix), F20 (loud break-glass), F35 (provider API keys), plus X4. **No agent change, no external IdP (D-29), no impersonation or support grants (F31), no partner scoping (F37), no step-up beyond today's TOTP.** |
| **D-72** | How roles are expressed | **One role per operator** (as today). `decide()` uses an **explicit role → actions matrix** instead of the prefix rule; existing roles keep exactly today's permissions (proved by a full table test); a new action is denied to every non-super-admin role until the matrix grants it. **Amends D-05** (the prefix rule) **and D-31** (its exception becomes a matrix entry, same outcome). |
| **D-73** | ✔ The new roles (Q1, answered: auditor, billing **and support now**; billing may assign a tenant's plan) | **`auditor`:** every read the console has (relays, pools, tenants, PKI, health, metrics, alerts, timeline, usage, invoices, the provider audit log); **no writes**; tenant reads are audited as for super-admins. **`billing`:** plan cards (F34), billing settings, invoices (issue, mark paid, void, regenerate, resend), usage reads, the tenant list and tenant plan assignment; **no** relay, pool, tenant lifecycle (suspend, delete), quota, alert or operator actions. **`support`:** tenant reads (list, detail with connectors, shields, devices and agent status), the tenant's alerts and timeline rows, health; **no** billing (invoices, usage, plan cards), **no** provider audit log, **no writes**; every tenant read is audited. Impersonation and support grants stay with F31. **Amends D-06** (tenant reads and plan assignment no longer super-admin only) **and D-68/D-70** (billing actions open to `billing`). |
| **D-74** | ✔ MFA for the new roles (Q4, answered) | TOTP **mandatory** for `auditor`, `billing` and `support` (they read tenant data or handle money), as for super-admins (D-28); `relay-ops` stays optional. **Extends D-28.** |
| **D-75** | ✔ Break-glass (Q5, answered: on this computer, the controller host) | The **`recover-admin` CLI on the controller host is the only break-glass path**; no standing break-glass account and no console or API route for it. Every use needs a reason and raises a critical platform alert (`provider.break_glass_used`), an email to every active super-admin and a console banner until acknowledged. **Extends D-26 / ADR-029.** |
| **D-76** | ✔ API keys (Q2 answered: **read-only first**; Q3 answered: every key expires, **at most 365 days**) | Provider API keys `zpk_<id>_<secret>`, hashed at rest, shown once, **one role per key from `relay-ops` / `auditor` / `billing` / `support`, never `super-admin`**, **read actions only** (every write refused for a key), required expiry (≤ 365 days) with an `api_key.expiring` platform alert 14 days before (Q6), optional CIDR allowlist, per-key rate limit. A key is an `Actor` of kind `api_key`. It **never mints a session** and never reaches auth, TOTP, operator or key routes. Sensitive reads are audited with the key as the actor. Super-admin creates and revokes keys with a reason. **Amends D-30** (key create and revoke are console writes). |

**Answered (2026-11-23):**
1. **Q1 roles:** yes, `auditor` and `billing`, and **`support` now** (read tenants only; no impersonation). `billing` may assign a tenant's plan → D-73.
2. **Q2 keys and writes:** **read-only keys first**; writes by key are a later decision → D-76.
3. **Q3 key lifetime:** every key expires, **at most 365 days** → D-76.
4. **Q4 TOTP:** **mandatory** for the new roles (`auditor`, `billing`, `support`) → D-74.
5. **Q5 break-glass:** **only the CLI on this computer** (the controller host); no standing account → D-75.
6. **Q6 key expiry alert:** **this sprint**: platform rule kind `api_key.expiring`, 14 days before expiry → K1, D-76.

**Review questions (as asked):**
1. **Q1 roles:** are `auditor` and `billing` the right two to add now? Should `billing` be allowed to **assign a tenant's plan** (recommended yes: it changes what the tenant pays), or only read it? Should a `support` role come now (read tenants only) or with F31?
2. **Q2 keys and writes:** may an API key **write** (e.g. a `relay-ops` key draining a relay, a `billing` key issuing invoices)? Recommended yes, within its role. The alternative is read-only keys for the first release.
3. **Q3 key lifetime:** maximum expiry 365 days? Must every key expire (recommended yes)?
4. **Q4 TOTP:** mandatory for `auditor` and `billing` (recommended), or optional like `relay-ops`?
5. **Q5 break-glass:** keep the CLI as the only break-glass path (recommended), or also a standing sealed account?
6. **Q6 key expiry alert:** add the `api_key.expiring` platform rule this sprint (small), or later?

## Suggested split (two members)

| Member | Phases | Order |
|---|---|---|
| **Barath (M2)** | **O1** role matrix + roles (authz is the security core), **G1** break-glass (his `recover-admin` and alerting are close) | O1 first (the contract for O2 and K1), then G1 |
| **Sathiya (M1)** | **X4**, **K1** API keys, **K2** keys console, **O2** console per role | X4 → K1 (after O1's matrix) → K2 → O2 |

The contract is O1's matrix and roles. K1 applies a key's role through the same `decide()`, so O1 merges first.

**Migrations (to reserve):**
- `075_provider_roles` (O1);
- `076_provider_api_keys` (K1);
- `077_provider_alert_kinds` (G1 `provider.break_glass_used` and K1 `api_key.expiring`, if the kinds CHECK lists them; one file, owned by G1, K1 adds its kind there or in `078_`).

Latest expected after Sprint 26: `074_invoices`. Check `ls controller/migrations | tail -3` first. There is no migration framework. Label the PRs `[schema: reset DB]`.

**Conflict zones:**
- `controller/internal/provider/authz.go` (O1 rewrites the rule; K1 adds key actions; one owner at a time);
- the provider auth middleware and `identity.go` (K1; `IssueSession` untouched);
- `provider_users` handlers (O1 roles);
- `cmd/server/recover_admin.go` (G1);
- `internal/alerting` kinds (G1, K1);
- `cmd/server/main.go`;
- `provider-console/src/auth/roles.ts`, `Layout.tsx`, `api-surface.test.ts`, `static-guards.test.ts` (clipboard allowlist), the operator dialogs.

## Not in Sprint 27 (proposed)
- **F31** impersonation or support grants (decisions doc Q9); the `support` role in this sprint is read-only.
- API keys that write (Q2: read-only first).
- External provider IdPs (OIDC, SSO) (D-29); recovery codes; step-up re-authentication for destructive actions (Q10).
- **F37** partner / reseller scoping (target scoping in `decide()`); several roles per operator.
- OAuth client credentials, signed requests or mTLS for the API (keys only).
- F22, F28, F29, F36, F38, F39, F40.

## Test gates (as in Sprint 26)
- Build, vet and the full Go suite with every DB-backed test running (a skipped DB test is a failed case). Console tests, lint and build.
- **Mutation checks:**
  - the prefix rule left in place (a new `relay.*` action opens to relay-ops without a matrix entry);
  - an `auditor` write allowed;
  - a `billing` relay or tenant-lifecycle action allowed;
  - TOTP not required for a new role;
  - an API key reaching `IssueSession` or an auth / TOTP / operator / key route;
  - an API key allowed a write action (any role);
  - a `support` user reading invoices, usage or the audit log, or writing;
  - a revoked or expired key accepted;
  - a `super-admin` key created;
  - the key secret in a log, audit row or later response;
  - the break-glass alert or email skipped;
  - the key-expiry alert firing for a revoked key, firing twice, or carrying the secret;
  - `recover-admin` without a reason.
- **Live acceptance on the lab:**
  - **Roles:**
    - an `auditor` sees every page with no write buttons, and the API refuses every write (403);
    - a `billing` user manages plan cards and invoices, gets 403 on relay, tenant lifecycle and operator routes, and its tenant reads are audited;
    - a `support` user reads tenant detail (audited), gets 403 on invoices, usage, the audit log and every write;
    - all three must enrol TOTP;
    - relay-ops and super-admin are unchanged (spot-check the D-31 and D-41 rules).
  - **Break-glass:** `recover-admin --reason` → the critical alert (webhook, `scope: platform`), an email to each super-admin, a console banner until acknowledged; audit rows with the reason.
  - **API keys:**
    - a `relay-ops` key lists relays; draining a relay with it → 403 (read-only);
    - an `auditor` key reads the audit log and can't write;
    - a `billing` key reads invoices for an export; issuing one with it → 403;
    - revoke → the next call is 401;
    - an expired key → 401;
    - a key from outside its CIDR → 401;
    - `/provider/auth/*` and `/provider/users` → 403 with a key;
    - a key expiring within 14 days (test clock or a short expiry) → one `api_key.expiring` alert (email and webhook, no secret); revoking it resolves the alert;
    - the secret appears only in the create response.

## Next steps
1. ~~Answer Q1–Q6~~ (done 2026-11-23); sign off D-71 … D-76 and the split.
2. On sign-off: write `Sprint27/path.md` and the two phase files, amend the Decision Record (D-71 …), switch `CLAUDE.md` to Sprint 27, add a Session Log entry.
3. Before O1 starts: Sprint 26 merged and its acceptance run done (findings → X4).
