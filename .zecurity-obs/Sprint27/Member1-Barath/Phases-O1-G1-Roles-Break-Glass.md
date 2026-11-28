---
type: phase
sprint: 27
owner: Barath (Person 1, M2)
features: [F19 provider admin roles, F20 break-glass]
phases: [O1, G1]
decisions: [D-71, D-72, D-73, D-74, D-75, D-05, D-06, D-26, D-28, D-31, D-50]
status: signed off 2026-11-23 (by Sathiya; Barath reviews in PR #252); active since 2026-11-27 — O1 built by Sathiya (takeover, 2026-11-27, branch feat/sprint27-o1-role-matrix), live PASS; G1 next
---

# Sprint 27 — Barath: O1 role matrix and roles (F19), G1 loud break-glass (F20)

Read `Sprint27/path.md` first (order, conflict zones, rules). Binding decisions: Decision Record amendment **2026-11-23, provider access**:
- **D-72** role matrix, deny by default;
- **D-73** `auditor`, `billing`, `support`;
- **D-74** TOTP for the new roles;
- **D-75** loud break-glass.

These build on **D-05/D-31** (today's rule), **D-06**, **D-26/ADR-029** (`recover-admin`) and **D-50** (alert scopes).

Order: **O1 (merged first: the contract for K1 and O2) → G1**.

---

## O1 — Role matrix and new roles (`075_provider_roles.sql`)

### What exists
- `decide(actor, action, target)` in `controller/internal/provider/authz.go`: super-admin → allow; relay-ops → `relay.*` minus `superAdminOnlyRelayActions`, plus `relayOpsAlsoAllowed` (`alert.read`, `timeline.relay_audit`).
- Role constants `RoleSuperAdmin`, `RoleRelayOps`; the role CHECK in `025_provider_users.sql`; one role per operator.
- `RequiresTOTP` (`internal/provider/totp.go`): super-admin, or anyone with TOTP enabled.
- Operator management with lock-out guards; a role change bumps `session_generation`.

### Build
- **`075_provider_roles.sql`**: drop and re-add the `provider_users` role CHECK with `super-admin`, `relay-ops`, `auditor`, `billing`, `support` (a new file; never edit `025_`).
- **Matrix** (`authz.go`): `var roleActions = map[string]actionSet{…}`.
  - Each set is an explicit list of actions; a namespace grant (e.g. `relay.*`) is written as a prefix entry and still subject to the exclusion list.
  - `decide()`: super-admin → allow; otherwise allowed iff the role's set contains the action. Keep `decide()` the only chokepoint.
- **Existing roles:** `relay-ops` = today's outcome exactly (`relay.*` minus revoke and delete, plus `alert.read`, `timeline.relay_audit`, and the D-41 pool actions it has today).
- **New roles (D-73):**
  - **`auditor`**: every **read** action (`*.read`, `audit.view`, `health.read`, `relay.metrics.read`, `tenant.metrics.read`, `tenant.usage.read`, `usage.fleet.read`, `invoice.read`, `plan.read`, the billing settings read); no write action.
  - **`billing`**: `plan.read`, `plan.create`, `plan.update` (plan cards incl. price, D-63), `tenant.plan.assign`, the billing settings read and write (`billing.settings.write`), `invoice.read`, `invoice.write`, `tenant.usage.read`, `usage.fleet.read`, and the tenant list read (name, plan, status).
    - Decide in the PR whether the tenant list needs its own action (`tenant.list`) so `billing` doesn't get full tenant detail; recommended: yes, a narrow `tenant.list.read`.
  - **`support`**: `tenant.read` (list and detail with agents), the tenant's alerts (`alert.read` for tenant scope) and timeline rows, `health.read`; no billing, usage, plan-card or audit reads; no writes.
- **Read vs write helper:** `IsWriteAction(action) bool`, used by K1 to refuse key writes.
  - Every action constant is classified once (a table next to the constants).
  - A test fails on an unclassified action.
- **TOTP (D-74):** `RequiresTOTP` → true for `super-admin`, `auditor`, `billing`, `support`, or anyone with TOTP enabled.
- Role labels and validation in the operator handlers accept the new roles; only super-admins assign roles (unchanged).

### Tests (DB-backed where they touch the store)
- **Full table:** every action constant × every role → expected allow / deny. `super-admin` and `relay-ops` rows equal today's `decide()` (generate the expectation from the current code before the rewrite and keep it as a fixture).
- **Deny by default:**
  - an unknown action is denied for every role except super-admin;
  - a test fails if any action constant is missing from the matrix or from the read/write classification.
- `auditor`: no write allowed. `support`: no billing, usage, plan, audit read and no write. `billing`: no relay, pool, tenant lifecycle, quota, alert or operator action.
- TOTP required for the three new roles (sign-in without TOTP → MFA enrolment stage, no full session).
- Role change to and from a new role ends sessions; the lock-out guards still hold (can't demote the last super-admin, can't demote self).
- **Mutation checks:**
  - prefix rule kept (a new `relay.*` action opens to relay-ops);
  - a missing matrix entry not caught;
  - an auditor write allowed;
  - billing given `tenant.suspend`;
  - support given `invoice.read`;
  - TOTP not required for `billing`.

### Live check (O1)
- Create one operator per new role (super-admin). Each must enrol TOTP.
- Over the API:
  - auditor: every write → 403;
  - billing: issue an invoice → 200, suspend a tenant → 403;
  - support: tenant detail → 200 (audited), `GET /provider/invoices` → 403.
- relay-ops spot-checks unchanged (drain 200, revoke 403).

### Implementation record (built by Sathiya, takeover 2026-11-27)

Barath's phase, built by Sathiya at Sathiya's instruction (as P1a–P2 in Sprint 26). Branch `feat/sprint27-o1-role-matrix` from `main` `6d25b6bd`.

- **`075_provider_roles.sql`:** drops and re-adds `provider_users_role_check` with the five roles; `025_` untouched.
- **Matrix (`internal/provider/authz.go`):**
  - `roleActions`: an explicit action list per role (`relay-ops`, `auditor`, `billing`, `support`). `decide()`: super-admin → allow; otherwise allowed iff the role's list has the action. The prefix rule, `relayOpsAlsoAllowed` and `superAdminOnlyRelayActions` are gone.
  - `relay-ops` = the pre-O1 outcome exactly: 14 actions, compared with a dump of the old `decide()` over all 62 action constants taken before the rewrite. It includes the audit-row name `relay.placement_rejected`, which the old prefix rule allowed.
  - `actionClasses`: every `Action*` constant is `read`, `write` or `auditOnly` (audit-row names such as `invoice.issue`, `tenant.purged`). `IsWriteAction` = anything not a classified read (unknown → write, fail closed) for K1.
  - `Roles` lists the five roles; `ValidRole` uses it.
- **Choice made in the PR (the phase file asked for it):** a narrow **`tenant.list.read`** gates `GET /provider/tenants`, so `billing` gets the tenant list (the same summary rows as other roles: name, slug, status, counts, suspension/deletion state) but not the detail with agents (`tenant.read`). super-admin, auditor and support have both.
- **Role → access (D-73):**
  - `auditor`: every classified read (relays, pools, relay and tenant metrics, tenants, usage, plan cards + billing settings via `plan.read`, invoices, alerts, alert rules and channels, timeline, provider audit log, PKI, health). Not the operator list (`provider_user.manage` is a write): operator management stays super-admin only.
  - `billing`: `plan.read`, `plan.create`, `plan.update`, `tenant.plan.assign`, `billing.settings.write`, `invoice.read`, `invoice.write`, `tenant.usage.read`, `tenant.list.read`. Plan delete and set-default stay super-admin only, as the phase file lists.
  - `support`: `tenant.read`, `tenant.list.read`, `alert.read`, `health.read`.
- **Alert and timeline visibility (`internal/alerting/reads.go`, `handler.go`, `handler_timeline.go`):** `viewFor(role)`: super-admin and auditor see everything; **support sees tenant-scope alerts except `tenant.usage_over`** (support never sees usage, D-73), no platform alerts and no audit rows in the timeline; every other role keeps the relay-ops view (unchanged). Choice to confirm: excluding usage alerts from support.
- **TOTP (D-74):** `RequiresTOTP` → `roleRequiresTOTP` (super-admin, auditor, billing, support) or TOTP enabled.
- Doc comments updated where they said "super-admin only" for billing, invoices, usage and health. The console (`roles.ts`, operator dialogs) is O2.

**Tests:**
- `authz_matrix_test.go`:
  - every `Action*` constant (parsed from `authz.go`) has a class, and the class table has nothing else;
  - the full table: every constant × every role plus non-roles (`""`, `admin`, `Auditor`, `billing `, `api_key`) against an expectation written independently of `roleActions`, with relay-ops pinned to the pre-O1 fixture;
  - deny by default for unknown actions (incl. a new `relay.*` verb);
  - role boundaries; `IsWriteAction`; tenant list vs detail; `RequiresTOTP`; `ValidRole`.
- DB-backed:
  - `TestTOTP_NewRolesMustEnrol`: password change → `mfa=enroll` → enrol → full session → later sign-in asks `verify`;
  - `TestChangeRole_NewRolesEndSessions`: role changes to and from each new role end sessions; new roles get 403 on operator routes; an unknown role → 400;
  - `TestOperatorGuards_LastSuperAdmin_NewRoles`: the last super-admin can't be demoted to a new role, and the DB CHECK accepts the new roles and refuses `owner`;
  - `TestAlertsDB_NewRoleViews`: auditor = super-admin row for row; support only the tenant's connector alert (no relay, pool or usage alert, no audit rows); billing 403 on alerts and timeline; no new role manages rules.
- **Mutation checks (each made a test fail):**
  - the old prefix rule kept;
  - an unclassified constant;
  - an auditor write (`relay.drain`);
  - billing given `tenant.suspend`;
  - support given `invoice.read`;
  - TOTP optional for billing;
  - relay-ops losing drain;
  - support shown usage alerts;
  - support given the full alert view.

**Gate (2026-11-27):** `go build ./... && go vet ./...` clean; full `go test -count=1 ./...` with every DB variable set (PKI, AUTH, ENROLLMENT, RESOURCE, SHIELD, `AUTH_TEST_VALKEY_URL` db 15): 50 packages ok, 0 failures, only the 2 known hard-coded skips; the six new DB-backed tests ran (none skipped). No console change in O1.

**Live check (2026-11-27): PASS.**
- **Setup:** local controller from the branch (sha256 `c2b7e2c4…`) on a fresh DB (migrations through `075_`), mail capture on 127.0.0.1:2525; driver `~/.cache/zecurity-s27/o1live.py`.
- **TOTP:** each of `auditor`, `billing` and `support` was created by the super-admin. After its password change each got `mfa=enroll`, its enrol-stage token got 403 on `/provider/me`, and it enrolled; every later sign-in asked for `verify`. `relay-ops` got a full session without TOTP.
- **Route matrix: 119 of 120 checks as expected** (17 reads and 12 writes per role, the TOTP verify prompts and the audit check).
  - auditor: every read 200 except `/provider/users` (403); every write 403.
  - billing: tenants list 200, tenant detail 403; plans, billing settings, invoices and usage 200; created a card (201), changed billing settings (200) and assigned a plan (200); relay, tenant suspend, quota, plan delete, alert-rule and operator writes 403; alerts and timeline 403.
  - support: tenant list and detail, health, alerts and timeline 200; invoices, usage, plans, billing settings, audit and every write 403.
  - relay-ops: unchanged (relay reads, relay create 201, resume 204, revoke 403, tenant routes 403).
  - The one non-match: relay-ops **drain → 409 `invalid_state`**, because the test relay was never enrolled (`pending`; no relay host for this check). Authorization passed (not 403); drain itself is unchanged.
- **Audit:** tenant detail reads are audited as `tenant.read` under `auditor@s27.test` and `support@s27.test`. Billing's writes are audited under its own email with the reason: `plan.create`, `billing.settings.update`, `invoice.issue`.
- **Invoice:** with the dev test clock (`BILLING_TEST_NOW=2026-12-01T06:05:00Z`), November closed with 1 draft. `billing` issued it (204); auditor and support void → 403; billing suspend with the correct slug → 403.
- Not run live: a real drain (needs an enrolled relay); the console (O2).

---

## G1 — Loud break-glass (`077_provider_alert_kinds.sql`)

### What exists
- `zecurity-controller provider recover-admin --email <super-admin>` (`cmd/server/recover_admin.go`): it sets a temporary password (stdout only), clears TOTP and ends sessions. It is audited `provider_user.recover_admin` by `system:cli`.
- F26: alert kinds and the CHECK in `069_alerts.sql` / `072_usage_alert_kind.sql` (it lists the platform kinds), evaluator state, A2 delivery.
- F14 SMTP sender (`invitation.Emailer.SendPlain`) and the active super-admin query.

### Build
- **`077_provider_alert_kinds.sql`:**
  - replace the `alert_rules_kind_scope` CHECK (new file) adding **platform** kinds `provider.break_glass_used` (G1) and `api_key.expiring` (for Sathiya's K1);
  - seed their built-in rules (`provider.break_glass_used` critical, enabled; `api_key.expiring` warning, enabled, parameter `days_before` = 14).
  - Coordinate the exact seed with Sathiya before merging.
- **`recover-admin --reason "<text>"` (required):**
  - refused without it (exit code ≠ 0, nothing changed);
  - the reason goes into the audit row's details (never echoed elsewhere).
  - In the same DB transaction as the recovery, it writes a **break-glass event** row (who was recovered, when, the reason, the host name).
- **Alert:** the evaluator (or a direct event insert, the way A1 records one-off events) raises `provider.break_glass_used` once per event, severity critical, delivered by A2 to every matching channel.
  - Payload: the recovered operator's email, the time and the reason. No temporary password or secret.
- **Email:** to every active super-admin (F14 sender): subject "Zecurity: break-glass recovery used", the operator, the time and the reason. The audit records the recipient count, not addresses.
- **Console banner:**
  - `GET /provider/break-glass` (super-admin) returns unacknowledged events;
  - `POST /provider/break-glass/{id}/ack` (super-admin, note required) acknowledges one; audited `break_glass.acknowledge`.
  - The banner itself is O2/console work: Sathiya adds it with O2, or Barath adds a minimal one here; agree in review.

### Tests
- `recover-admin` without `--reason` → refused, no change.
- With a reason: one audit row (with the reason), one event row, one alert event, one email per active super-admin.
- The alert payload, email and logs never contain the temporary password.
- Ack: super-admin only, note required, audited; relay-ops and the new roles → 403.
- **Mutation checks:** reason optional; alert skipped; email skipped; temporary password in the payload; ack without a note.

### Live check (G1)
On the controller host:
1. Run `recover-admin --email <test super-admin>`: refused (no reason).
2. Run it with `--reason`: the webhook receiver gets `provider.break_glass_used` (critical, `scope: platform`, no password); each active super-admin gets the email; the console shows the banner; acknowledging it with a note clears it (audited).

### Implementation record
_Fill in when built (branch, choices, tests, gates, mutation checks)._

---

## Post-Phase Fixes

_None yet._
