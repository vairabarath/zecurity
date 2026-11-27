---
type: phase
sprint: 27
owner: Barath (Person 1, M2)
features: [F19 provider admin roles, F20 break-glass]
phases: [O1, G1]
decisions: [D-71, D-72, D-73, D-74, D-75, D-05, D-06, D-26, D-28, D-31, D-50]
status: signed off 2026-11-23 (by Sathiya; Barath reviews in PR #252); active since 2026-11-27 — O1 next (migration 075_; latest on main is 074_invoices)
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
