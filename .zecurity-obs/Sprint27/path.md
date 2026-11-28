---
type: sprint
sprint: 27
title: Provider Dashboard — provider access and API (F19, F20, F35)
status: ACTIVE — every phase merged 2026-11-28 (#262 → #267, main `8db309bf`; at Sathiya's go before Barath's review); acceptance run PASS 2026-11-28 (main `b6521928`); F19 ticked; left: fixes X5 (D-77) and X6 (D-78), then tick F20 and F35; Barath's review of the open points below
created: 2026-11-23
team:
  - Person 1 = Barath (M2): F19 — O1 role matrix and new roles; F20 — G1 loud break-glass
  - Person 2 = Sathiya (M1): X4 Sprint 26 follow-ups; F35 — K1 API keys, K2 keys console; F19 — O2 console per role
baseline: main 7ee717bc (Sprint 26 merged and live-checked: #257, #258, #259, #250, #251, #254, acceptance #260; latest migration 074_invoices)
---

# Sprint 27 — Provider access and API

## Goal

The provider console has two roles today (`super-admin`, `relay-ops`; D-05, D-31), and every provider request comes from a person signed in with a password (plus TOTP for super-admins). Sprint 27 makes provider access fit how a provider works:
- **F19 roles:** `auditor` (reads everything), `billing` (plan cards, billing settings, invoices, usage) and `support` (reads tenants to help them) each get exactly what they need. They don't get super-admin. `decide()` becomes an explicit role → actions table.
- **F20 break-glass:** `recover-admin` on the controller host stays the only way back in. Every use becomes **loud**: a critical alert, an email to every super-admin, and a console banner.
- **F35 API keys:** scripts call the provider API with a **read-only**, expiring, revocable key scoped to one role, without a person's password or TOTP and without a session. An alert fires 14 days before a key expires.

**No agent change. No external IdP. No impersonation. No keys that write.**

| Feature | Today | Phases (owner) |
|---|---|---|
| **F19** Provider admin roles | ◐ two roles, prefix rule in `decide()`, role CHECK in `025_` | **O1** role matrix + roles (Barath), **O2** console per role (Sathiya) |
| **F20** Break-glass account | ◐ `recover-admin` CLI, audited | **G1** loud break-glass (Barath) |
| **F35** Provider API + API keys | ◐ REST API with session tokens only | **K1** API keys (Sathiya), **K2** keys console (Sathiya) |
| Sprint 26 follow-ups | Sprint 26 acceptance not done | **X4** (Sathiya) |

Tracker: `.zecurity-obs/Planning/Provider-Dashboard-Features.md`. **Binding decisions:** `docs/provider-dashboard-architecture-decisions.md`, amendment **2026-11-23, provider access (D-71 … D-76)**, on top of D-01 … D-70. Scope discussion: `Sprint27/Scope-Draft.md` (PR #252).

**Prerequisites:**
1. ✅ Sprint 26 (F34) merged and live-checked (acceptance PASS 2026-11-26, #260); its one finding was fixed in #260, so **X4** is done. The `billing` role covers F34's endpoints.
2. Sprint 25 acceptance closed (2026-11-24, #253). **Open:** the Sprint 24 acceptance run (F23 live R1-1/R1-2 failed on 2026-11-23; F26, F27 live-checked but not ticked). Sprint 27 started on 2026-11-27 with this still open; it doesn't block O1 or K1 (no shared code), and stays on the list to close.

Sprint 27 became the active sprint in `CLAUDE.md` on 2026-11-27.

---

## Decisions (Phase 0, done 2026-11-23)

All six review questions were answered on 2026-11-23 (marked ✔). The full text is in the Decision Record.

| ID | Decision | Phase |
|---|---|---|
| **D-71** | **Scope:** F19 (roles `auditor`, `billing`, `support` and an explicit role matrix), F20 (loud break-glass), F35 (read-only provider API keys with an expiry alert), plus X4. **Not in scope:** agent changes, external IdPs (D-29), impersonation or support grants (F31), partner scoping (F37), several roles per operator, step-up beyond today's TOTP, keys that write. | all |
| **D-72** | **Role matrix.** One role per operator (as today). `decide()` uses an **explicit role → actions table**; existing roles keep **exactly** today's permissions, proved by a full action × role table test. An action missing from the table is **denied to every role except super-admin**, and a test fails when an action constant has no table entry. **Amends D-05** (the prefix rule) and **D-31** (its exception becomes a table entry with the same outcome). | O1 |
| **D-73** | ✔ Q1 **New roles:**<br>• **`auditor`**: every read the console has (relays, pools, tenants, PKI, health, metrics, alerts, timeline, usage, invoices, plan cards, billing settings, the provider audit log); **no writes**.<br>• **`billing`**: plan cards (create, edit), billing settings, invoices (issue, mark paid, void, regenerate, resend), usage reads, the tenant list and **tenant plan assignment**; **no** relay, pool, tenant lifecycle, quota, alert or operator actions.<br>• **`support`**: tenant reads (list; detail with connectors, shields, devices and agent status), the tenant's alerts and timeline rows, health; **no** billing (invoices, usage, plan cards), **no** provider audit log, **no writes**.<br>Every tenant read by any of them is audited as for super-admins (OQ-1). **Amends D-06** (tenant reads and plan assignment are no longer super-admin only) and **D-68/D-70** (billing actions open to `billing`). | O1, O2 |
| **D-74** | ✔ Q4 **TOTP is mandatory** for `auditor`, `billing` and `support`, as for super-admins (`RequiresTOTP`). `relay-ops` stays optional. **Extends D-28.** | O1 |
| **D-75** | ✔ Q5 **Break-glass:** `zecurity-controller provider recover-admin` **on the controller host** is the only break-glass path: no standing break-glass account, no console or API route. It now requires `--reason`. Every use raises a **critical platform alert** `provider.break_glass_used` (F26 delivery, no secrets in the payload), **emails every active super-admin** (F14 sender) and shows a **console banner** to super-admins until one acknowledges it (with a note, audited). **Extends D-26 / ADR-029.** | G1 |
| **D-76** | ✔ Q2, Q3, Q6 **Provider API keys:**<br>• `zpk_<id>_<secret>` with a 256-bit random secret; SHA-256 at rest, constant-time compare; **shown once** (`no-store`), never logged, audited or returned again.<br>• **One role per key** from `relay-ops`, `auditor`, `billing`, `support`; **never `super-admin`**.<br>• **Read-only (Q2):** a key may use only its role's **read** actions; every write is refused for a key.<br>• **Every key expires, at most 365 days (Q3).** Optional source CIDR allowlist; per-key rate limit (fails closed).<br>• A key is an `Actor` of kind `api_key`. It **never goes through `IssueSession`**, never yields a session, and is refused (403) on auth, TOTP, operator-management and API-key routes.<br>• Audited reads are audited with actor `api_key:<name> (<prefix>)`.<br>• **Expiry alert (Q6):** platform rule kind `api_key.expiring` fires once per key 14 days before expiry and resolves when the key is revoked, expires or is replaced; no secret in the payload.<br>• Super-admin creates and revokes keys, with a reason. **Amends D-30** (key create and revoke are console writes). | K1, K2 |

**Extends:** D-28 (TOTP for the new roles), D-26 (break-glass), D-50 (two platform alert kinds). **Amends:** D-05, D-06, D-30, D-31, D-68/D-70. **Unchanged:** D-02, D-24 … D-27 (local accounts, provider key and issuer, create-only bootstrap, lock-out guards), D-29, D-40 (relay-ops never see tenant data).

---

## Team assignments

**Person 1, Barath (M2):** O1 → G1. Phase file: `Member1-Barath/Phases-O1-G1-Roles-Break-Glass.md`.
**Person 2, Sathiya (M1):** X4 → K1 → K2 → O2. Phase file: `Member2-Sathiya/Phases-X4-K1-K2-O2-API-Keys.md`.

**Both:**
- review each other's PRs;
- run the Sprint 27 acceptance at the end (live, on a reset lab DB, current `main`).

### Phases and order

| Phase | Owner | Feature | Depends on | Migration |
|---|---|---|---|---|
| **X4** Sprint 26 follow-ups | Sathiya | F34 | the Sprint 26 acceptance run | none expected |
| **O1** Role matrix + roles | Barath | F19 | Sprint 26 merged | `075_provider_roles.sql` |
| **G1** Loud break-glass | Barath | F20 | O1 merged | `077_provider_alert_kinds.sql` (owned by G1; also holds K1's `api_key.expiring`) |
| **K1** API keys | Sathiya | F35 | O1 merged (keys use the matrix); `077_` merged before the expiry alert part | `076_provider_api_keys.sql` |
| **K2** Keys console | Sathiya | F35 | K1 merged | none |
| **O2** Console per role | Sathiya | F19 | O1 merged | none |

```
Barath:  O1 (matrix + roles, 075) ──► G1 (break-glass, 077 incl. api_key.expiring)
            │                                   │
Sathiya: X4 ┴──► K1 (keys, 076; expiry alert after 077) ──► K2 (keys console) ──► O2 (console per role)
```

- **O1 merges first:** the role matrix is the contract. K1 applies a key's role through the same `decide()`, and O2 mirrors the matrix in the console.
- **One alert-kind migration:** `077_provider_alert_kinds` replaces the kinds CHECK once (never edit `069_` or `072_`) with both new platform kinds. G1 owns the file; K1 adds no second kinds migration.
- **One authorization path:** people and keys both go through `decide()`. The read-only rule for keys lives there (an `api_key` actor + a write action → denied), not in handlers.
- Migration numbers assume Sprint 26's `073_` and `074_` are merged. Recheck before O1.

### Conflict zones (shared files)

| File / area | Who | Rule |
|---|---|---|
| `controller/internal/provider/authz.go` (`decide()`, actions, matrix) | Barath (O1), then Sathiya (K1) | O1 rewrites the rule and owns the matrix. K1 adds the key actions and the `api_key` write refusal **after O1 merges**; one owner at a time. |
| `controller/internal/middleware` (`RequireProvider`), `internal/provider/identity.go` | Sathiya (K1) | Key auth is its own middleware step before the session path. `IssueSession` is **not touched**. A key never sets `amr` or a session. |
| `internal/provider/totp.go` (`RequiresTOTP`) | Barath (O1) | Add the three new roles; nothing else changes. |
| `provider_users` handlers, operator dialogs | Barath (O1), Sathiya (O2) | New roles assignable by super-admins only; lock-out guards and `session_generation` bump unchanged. |
| `cmd/server/recover_admin.go` | Barath (G1) | `--reason` required; the alert, email and banner marker are written in the same run; the temporary password stays stdout-only. |
| `internal/alerting` (kinds, evaluator) | Barath (G1), Sathiya (K1) | Two platform kinds in one migration (`077_`); reuse A1/A2 state and delivery unchanged. |
| `controller/cmd/server/main.go` | both | Own block per phase; rebase before merge. |
| `provider-console/src/auth/roles.ts`, `Layout.tsx` | Sathiya (O2, K2) | Navigation follows the matrix; hiding is UX only. |
| `api-surface.test.ts`, `static-guards.test.ts` | Sathiya (K2) | Key create / revoke writes on purpose; the show-once key dialog joins the clipboard allowlist. |
| `controller/migrations/` | both | Reserved `075_` (O1), `076_` (K1), `077_` (G1); `[schema: reset DB]` in the PR title. |

---

## Rules (this sprint)

- Decisions **D-71 … D-76 are binding**, together with D-01 … D-70. If a phase needs another architectural choice, stop and ask; don't decide it in code.
- **Deny by default:** an action with no matrix entry is denied to every non-super-admin role; a test enforces that every action constant has an entry.
- **No behaviour change for existing roles:** the full action × role table for `super-admin` and `relay-ops` must equal today's `decide()` outcome (D-31, D-41, D-55 cases included).
- **Keys never write and never become sessions:** `decide()` refuses every write for an `api_key` actor; no code path calls `IssueSession` for a key; auth, TOTP, operator and key routes refuse keys.
- **Secrets:** the key secret is shown once and never logged, audited or returned; break-glass and expiry alerts carry no secrets; the `recover-admin` temporary password stays stdout-only.
- **Lock-out-proof (unchanged):** only super-admins manage operators and keys; no self-disable or self-demotion; never zero active super-admins; a role change ends that operator's sessions.
- **TOTP:** mandatory for `super-admin`, `auditor`, `billing`, `support`; never mint a session without `otp` in `amr` for them.
- **Tenant boundary:** `relay-ops` still never sees tenant data (D-40); `support` and `auditor` tenant reads are audited; `support` never sees invoices, usage or the provider audit log.
- **Single controller (D-02):** key rate limits and `last_used_at` throttling are process-local.
- Schema changes go in new numbered files only; there is no migration framework; recreate the local DB.
- Each phase ends with a **live check** recorded in `.zecurity-obs/Planning/Pending-Live-Tests.md` before its tracker box is ticked.
- **No commit, push, PR or merge without the product owner's explicit go** for that step.

## Test gates (each phase)

- **Controller:** `go build ./... && go vet ./...`, then the full `go test ./...` with the DB env (`PKI_TEST_DATABASE_URL`, `AUTH_TEST_VALKEY_URL` with a non-default database). 0 failures, only the known hard-coded skips.
- **Provider console:** `npm test`, `npm run lint`, `npm run build`.
- **Mutation checks:**
  - the prefix rule left in place (a new `relay.*` action opens to relay-ops without an entry);
  - an action constant without a matrix entry passing the test;
  - an `auditor` or `support` write allowed;
  - a `billing` relay, tenant-lifecycle, quota, alert or operator action allowed;
  - `support` reading invoices, usage or the audit log;
  - TOTP not required for a new role;
  - a key reaching `IssueSession` or an auth / TOTP / operator / key route;
  - a key allowed a write (any role);
  - a revoked or expired key accepted;
  - a `super-admin` key created;
  - the key secret in a log, audit row or later response;
  - the CIDR allowlist or rate limit not enforced;
  - `recover-admin` without a reason;
  - the break-glass alert or email skipped;
  - the expiry alert firing for a revoked key, twice, or with the secret.

## Acceptance (end of sprint)

Live run on a **reset lab DB with current `main`**. Record the Git SHA and latest migration; keep the logs.

- **Roles:**
  - each of `auditor`, `billing`, `support` must enrol TOTP before any page loads;
  - **auditor:** every page read-only, no write buttons; every write over the API → 403; tenant reads audited;
  - **billing:** plan cards, billing settings, invoices (issue, mark paid, void), usage and tenant plan assignment work; relay, pool, tenant lifecycle, quota, alert and operator routes → 403;
  - **support:** tenant list and detail (audited), the tenant's alerts and timeline rows; invoices, usage, the audit log and every write → 403;
  - **unchanged:** super-admin; relay-ops (create, drain, resume, move, capacity; revoke and delete → 403; no tenant data).
- **Break-glass:** `recover-admin --email … --reason …` on the controller host:
  - a critical `provider.break_glass_used` alert (webhook `scope: platform`, email);
  - an email to each active super-admin;
  - a console banner until acknowledged (audited);
  - the audit row has the reason;
  - without `--reason` it refuses.
- **API keys:**
  - create one key per role (shown once; the list shows only the prefix);
  - a `relay-ops` key lists relays, and a drain with it → 403;
  - an `auditor` key reads the audit log;
  - a `billing` key reads invoices, and issuing with it → 403;
  - a `support` key reads a tenant (audited as the key);
  - revoke → 401 on the next call; an expired key → 401; outside its CIDR → 401;
  - `/provider/auth/*`, `/provider/users` and the key routes → 403 with a key;
  - a key 14 days from expiry → one `api_key.expiring` alert (no secret), resolved by revoking it;
  - the secret appears only in the create response.
- **X4:** each Sprint 26 follow-up checked live.

Tracker: F19, F20, F35 ticked only after their live checks.

## Checklist

- [x] Sprint 26 merged and live-checked (#257 … #254, acceptance #260 `7ee717bc`, 2026-11-27); follow-up recorded as X4; tracker F34 ticked
- [x] Phase 0: Q1–Q6 answered 2026-11-23; D-71 … D-76 written into the Decision Record (amendment 2026-11-23, provider access)
- [x] Documents signed off 2026-11-23 (by Sathiya, as for Sprint 26; Barath reviews in PR #252)
- [x] X4: Sprint 26 follow-ups (Sathiya) — the one acceptance finding fixed in #260; no other follow-up
- [x] O1: Role matrix + roles `075_provider_roles` (built by Sathiya, takeover; live PASS 2026-11-27) — #262 merged `23ae7f53`
- [x] G1: Loud break-glass `077_provider_alert_kinds` (built by Sathiya, takeover; live PASS 2026-11-27) — #263 merged `314de6aa`
- [x] K1: API keys `076_provider_api_keys` + `api_key.expiring` alert (Sathiya; live PASS 2026-11-27) — #264 merged `74cf6f69`
- [x] K2: Keys console (Sathiya; live PASS 2026-11-27) — #266 merged `53790138`
- [x] O2: Console per role + break-glass banner (Sathiya; live PASS 2026-11-28) — #267 merged `8db309bf`
- [x] Sprint acceptance run — PASS 2026-11-28 on `main` `b6521928` (run by Sathiya; results below and in `Pending-Live-Tests.md`); tracker F19 ticked
- [x] D-77 and D-78 decided 2026-11-28 (Sathiya) from the run's two questions; written into the Decision Record (amendment 2026-11-28)
- [ ] X5: break-glass alert even when acknowledged before the first tick (D-77); live recheck; then tick F20
- [ ] X6: disabling or demoting a super-admin permanently revokes their API keys (D-78); live recheck; then tick F35

### Merged before review: open points for Barath (2026-11-28)

The whole stack was merged at Sathiya's go before Barath reviewed it. Choices made inside the phases, to confirm or change in review (details in each phase file's implementation record):
- **O1:**
  - a narrow `tenant.list.read` for billing's tenant list;
  - support doesn't see `tenant.usage_over` alerts;
  - auditor doesn't get the operator list.
- **G1:**
  - **decided 2026-11-28 (D-77):** an acknowledgement before the evaluator's first tick skipped the alert; every use must alert and the ack resolves it → fix X5;
  - the break-glass event table lives in `077_`;
  - the email is sent by a controller job, not the CLI;
  - the banner UI landed with O2.
- **K1 — decided 2026-11-28 (D-78, option c):** a key keeps working after its creator is disabled or demoted. The options were (a) keep it; (b) refuse keys whose creator is no longer an active super-admin; (c) revoke a user's keys when they're disabled or demoted. Decision: disabling or demoting the creator permanently revokes their keys (re-enabling never restores them) → fix X6. Flagged by an automated security review.
- **K2:** the 365-day preset ends 10 minutes short of a year; a picked date expires at 23:59 UTC.
- **O2:**
  - `TenantSummary.current_plan` on the list (D-73 "name, plan, status");
  - Change plan on the list only for roles without the tenant detail;
  - the Overview loads only the tiles a role may read.

### Acceptance run — 2026-11-28: PASS

`main` `b6521928`, migrations through `077_provider_alert_kinds`, reset local DB, one controller built from that commit (`ENV=development`, evaluator every 10 s, `BILLING_TEST_NOW=2026-12-01T06:05:00Z` so November closes, SMTP to a local capture server, webhook to a local receiver). For X4 the controller was restarted with `TENANT_DELETE_GRACE=2m` and a local purge export directory. Relay `suryavardhan` (Arch host 192.168.1.97, run in the foreground without installing, pool `s27-drain`). Drivers in `~/.cache/zecurity-s27` (not in the repo). Run by Sathiya.

| Stage | Result |
|---|---|
| **Roles (O1)** | setup 10/10; full API matrix 120/120 (the relay-ops drain, 409 on an unenrolled relay in the first pass, rerun on the real relay: drain 204, resume 204, both audited as relay-ops); invoice 8/8 (billing regenerates and issues; auditor, support, relay-ops void → 403) |
| **Break-glass (G1)** | 17/17: `--reason` required; critical alert, both super-admin emails (audited count 2), banner acknowledged with a note, alert resolved. Banner in the console (O2): shown to super-admin, submit disabled until a note, gone after ack, temporary password never on the page. **Finding → D-77:** an ack within one evaluator interval skipped the alert |
| **API keys (K1)** | 52/52 (one key per role, read-only, 403 on writes, auth/operator/key routes refused, revoke/expiry/CIDR → 401, rate limit, audited as the key). `api_key.expiring`: 6/6 with one webhook channel (fired once for the key 10 days out, one webhook and one email, not re-fired, resolved on revoke; one delivery per channel; no key in payload, email or log). The first pass counted every event twice because the harness had created two webhook channels |
| **Keys console (K2)** | 8/8 in the browser: key shown once (`zpk_…`), reads 200, writes 403; after closing, not in the page, localStorage or sessionStorage; list by prefix; revoke needs a reason and the typed name → Revoked, 401; create and revoke audited without the key |
| **Console per role (O2)** | navigation and buttons per role match the matrix: auditor every read section, no writes (billing settings read-only, operators and keys forbidden); billing tenants (Change plan, no detail), plans, usage, invoices, billing settings; support tenants, alerts (no rules or channels, no platform alerts), timeline; relay-ops relays (Add relay), pools (Create pool), alerts, timeline relay rows; super-admin everything incl. Operators and API keys |
| **X4** | a tenant suspended, deleted and purged mid-month kept its `usage_tenants` snapshot through the usage prune (controller restarted after the purge) and got one final November invoice (USD 10.00, "S27 X4 Purged", card "S27 Card") |

Not run: nothing from the acceptance list. Two questions came out of the run and were decided the same day (D-77, D-78); their fixes are X5 and X6 below.

## Post-Sprint Fixes

### X5 (pending): break-glass alert even when acknowledged early (D-77)
**Issue:** a `recover-admin` use acknowledged in the console before the evaluator's next tick raised no `provider.break_glass_used` alert and no webhook (email and banner still happened). Found in the acceptance run.

**Root cause:** the evaluator signal reads only unacknowledged events (`controller/internal/alerting/signals.go`, `breakGlassUsed`: `WHERE acknowledged_at IS NULL`), so an event acknowledged first is never seen as firing.

**Fix (to build):** every break-glass event fires the alert once, acknowledged or not, and the acknowledgement resolves it; DB test for an ack before the first tick; live recheck. Phase file: G1 → Post-Phase Fixes.

### X6 (pending): revoke a super-admin's API keys when they're disabled or demoted (D-78)
**Issue:** a key kept working after the super-admin who created it was disabled or demoted (K1 review question, from an automated security review).

**Fix (to build):** the operator disable and role-change paths revoke every active key the operator created in the same transaction (`revoked_at`, `revoked_by` = the acting super-admin, reason `creator disabled` / `creator demoted`) and record the count in the operator change's audit; re-enabling or re-promoting restores nothing; tests for disable, demote, re-enable (keys stay revoked), and keys of other super-admins untouched; live recheck (key → 401 after its creator is disabled; `api_key.expiring` resolves). Phase file: K1 → Post-Phase Fixes.

## Out of scope

- Impersonation or support grants (F31, decisions doc Q9); keys that write; OAuth client credentials, signed requests or mTLS for the API.
- External provider IdPs (D-29); recovery codes; step-up re-authentication (Q10); several roles per operator.
- Partner / reseller scoping (F37); F22, F28, F29, F36, F38, F39, F40.
- Agent (relay, connector, shield, client) changes.
