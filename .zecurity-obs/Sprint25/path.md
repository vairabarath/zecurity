---
type: sprint
sprint: 25
title: Provider Dashboard — usage metering and reports (F32, F33; usage alerts)
status: DRAFT pending Barath's sign-off (2026-11-21) — implementation may not start until the checklist's sign-off box is ticked and D-56 … D-61 are in the Decision Record
created: 2026-11-21
team:
  - Person 1 = Barath (M2): F32 — U1 usage ledger and collector
  - Person 2 = Sathiya (M1): X2 Sprint 24 follow-ups; F33 — U2 usage read API, U3 console; U4 usage alerts (F26 extension)
baseline: main cb441cc9 (Sprint 24 merged: X1, A1–A3, T1, R1, M1, M2 + review fixes #242; Sprint 24 acceptance run still to do)
---

# Sprint 25 — Usage metering and reports

## Goal

Sprint 24 made the fleet observable (F23 charts, F26 alerts, F27 timeline). Sprint 25 makes it **measurable per tenant**: how much each tenant used, per day and per month, in units a later billing feature (F34) can price. It **measures, reports and warns**; it prices nothing, issues no invoice and enforces nothing.

| Feature | Today | Phases (owner) |
|---|---|---|
| **F32** Usage collection | ⬜ nothing is metered; M1 stores rates and gauges (7 days, hourly averages 400 days); R1 gives per-tenant cumulative relay byte counters | **U1** usage ledger + collector (Barath) |
| **F33** Per-tenant usage report | ⬜ | **U2** read API, **U3** console + export (Sathiya) |
| **F26** usage alerts | F26 has built-in tenant rule kinds (A1) and delivery (A2) | **U4** `tenant.usage_over` rule kind (Sathiya) |
| Sprint 24 follow-ups | Sprint 24 acceptance run not yet done | **X2** (Sathiya) |

Tracker: `.zecurity-obs/Planning/Provider-Dashboard-Features.md`. **Binding decisions (after sign-off):** `docs/provider-dashboard-architecture-decisions.md` → amendment **2026-11-2x (D-56 … D-61)**, on top of D-01 … D-55. Scope discussion: `Sprint25/Scope-Draft.md` (PR #244).

---

## Decisions (Phase 0 — proposed, awaiting sign-off)

The three review questions were answered on 2026-11-21 (marked ✔). The rest is proposed until Barath signs off; full text goes into the Decision Record.

| ID | Decision | Phase |
|---|---|---|
| **D-56** | Scope: F32 + F33 + usage alerts (+ X2). **No pricing, invoicing or enforcement** (F34 later, own decision). **No agent change.** | all |
| **D-57** | Meters, per tenant, **UTC** hours / days / calendar months: `relay_bytes_to_clients`, `relay_bytes_to_connectors` (**exact**, deltas of R1's cumulative counters, not integrated rates), `session_minutes`, `connector_minutes`, `shield_minutes` (counted each minute), ✔ **`active_users`** (distinct users with a live client session in the month). **Only relayed traffic is metered:** direct-path bytes (client → connector without a relay) are not seen; metering them needs connector counters (agent change, not this sprint). | U1, U2 |
| **D-58** | Postgres ledger (`usage_hourly`, `usage_active_users`, `usage_tenants`), integer quantities, **kept 25 months**. ✔ A **purged tenant's usage is kept with its id and name** (`usage_tenants` snapshot) until retention ends; its per-user `usage_active_users` rows are reduced to the monthly count at purge (no user ids of a deleted tenant are kept). **Amends D-46** (the S4 purge keeps this record). | U1 |
| **D-59** | A minute the controller didn't collect is **unmetered** (never estimated, never back-filled from M1); a relay restart loses at most the bytes since its last heartbeat; hours with a missed minute are marked **incomplete**, and so are the days and months containing them. | U1, U2, U3 |
| **D-60** | Usage is **tenant data: super-admin only** (`tenant.usage.read`; relay-ops 403; D-40 extended). A read of one tenant's usage writes a `tenant.usage.read` audit row (as tenant detail, OQ-1); the fleet list writes one `usage.fleet.read` row per request. Tenant-facing usage (admin UI) is later; the API takes a tenant filter so it can be added. | U2, U3 |
| **D-61** | ✔ **Usage alerts:** a built-in **tenant** rule kind `tenant.usage_over` (meter + monthly threshold), fires **once per tenant per month** when month-to-date usage crosses the threshold, **resolves at the UTC month rollover**; provider-only delivery (D-50); super-admin managed with reason and before/after audit (D-53). **Warns only:** nothing is blocked, throttled or limited. | U4 |

**Extends:** D-40 (usage hidden from relay-ops), D-50 (a new tenant rule kind). **Amends:** D-46 (purge keeps the usage record). **Unchanged:** D-02, D-06, D-49 (M1 stays as it is), D-52, D-53, D-54.

---

## Team assignments

**Person 1 — Barath (M2):** U1. Phase file: `Member1-Barath/Phases-U1-Usage-Ledger.md` (written after sign-off).
**Person 2 — Sathiya (M1):** X2 → U2 → U3 → U4. Phase file: `Member2-Sathiya/Phases-X2-U2-U4-Usage-Report.md` (written after sign-off).

Both: review each other's PRs; the Sprint 24 acceptance run **before** U1/U2 start; the Sprint 25 acceptance run at the end (live, on a reset lab DB, current `main`).

### Phases and order

| Phase | Owner | Feature | Depends on | Migration |
|---|---|---|---|---|
| **X2** Sprint 24 follow-ups | Sathiya | F23, F26, F27 | the Sprint 24 acceptance run | none expected |
| **U1a** Usage schema | Barath | F32 | D-57, D-58 | `071_usage.sql` (`usage_hourly`, `usage_active_users`, `usage_tenants`) |
| **U1b** Usage collector | Barath | F32 | U1a merged, D-59 | none |
| **U2** Usage read API | Sathiya | F33 | U1a merged (the table is the contract), D-60 | none |
| **U3** Usage console + export | Sathiya | F33 | U2 merged | none |
| **U4** Usage alerts | Sathiya | F26 | U1b and U2 merged, D-61 | `072_usage_alert_kind.sql` (the `alert_rules_kind_scope` CHECK in `069_` lists the kinds; never edit `069_`) |

```
Barath:  U1a (schema) ──► U1b (collector) ─────────────┐
                │                                     │
Sathiya: X2 ──► U2 (API, against U1a's table) ──► U3 (console) ──► U4 (usage alerts)
```

- **U1a merges first** (schema only, with its constraint tests): the table and meter names are the contract between U1 and U2. U2 is built and tested against rows its tests insert; it doesn't wait for the collector.
- U3 needs real numbers only for its live check (after U1b).
- U4 reads month-to-date usage through U2's query (one reader; don't add a second usage query in the evaluator).
- Migration numbers checked against `main` (latest `070_alert_delivery_queue.sql`).

### Conflict zones (shared files)

| File / area | Who | Rule |
|---|---|---|
| `controller/internal/relay/workspace_traffic.go` (R1 tracker) | Barath (U1b) | Byte deltas come from the **cumulative counters and R1's restart detection** already there (uptime, counter going down); never from the per-second rates. Don't change the rate path M1 uses. |
| `controller/internal/metricstore/sampler.go` (M1) | Barath (U1b) | U1b runs on the same minute tick and reuses `tenantCounts`' definitions (live session, active connector, active shield): **one definition** of each, shared, so F23 charts and F32 usage never disagree. |
| `controller/internal/tenantlifecycle/purge.go` (S4) | Barath (U1b) | The D-58 exception only: freeze `usage_tenants`, reduce `usage_active_users` to counts. Nothing else in the purge changes. |
| `controller/internal/alerting/` (kinds, evaluator) | Sathiya (U4) | New kind in `kinds.go` and the evaluator; reuse A1's firing / resolved state and A2 delivery unchanged. |
| `controller/cmd/server/main.go` | both | Own block per phase (collector, routes); rebase before merge. |
| `controller/internal/provider/authz.go` (`decide()`) | Sathiya (U2) | `tenant.usage.read` (super-admin only by the existing rule: only `relay.*` and the listed exceptions reach relay-ops). No addition to `relayOpsAlsoAllowed`. |
| `provider-console/src/api/api-surface.test.ts` | Sathiya (U3) | New reads added on purpose; **no new writes** (U4 uses the existing A3 rule writes). |
| `TenantDetail.tsx`, navigation (`roles.ts`, `Layout.tsx`) | Sathiya (U3) | Usage section on tenant detail; a super-admin-only Usage page. |
| `controller/migrations/` | both | Reserved `071_` (U1a), `072_` (U4); `[schema: reset DB]` in the PR title. |

---

## Rules (this sprint)

- Decisions **D-56 … D-61 are binding after sign-off**, with D-01 … D-55. If a phase needs another architectural choice, stop and ask; don't decide it in code.
- **Counts, not estimates (D-59):** usage is counted at the source; never derived from M1 rates or roll-ups, never interpolated, never back-filled. A gap stays a gap and is marked incomplete.
- **Exactly once:** every write to the ledger is idempotent per (tenant, meter, minute) for time meters and per relay heartbeat for byte deltas; a controller restart, a re-run of a minute or a crash between accumulate and flush never counts twice. Tested with mutation checks.
- **Never negative:** a relay restart or counter reset starts a new baseline; it never produces a negative or huge delta.
- **Tenant boundary (D-40, D-60):** usage is tenant data; relay-ops get 403 on every usage read and never see usage alerts or tenant names. Tenant-scoped reads always filter by `tenant_id`.
- **Purge (D-58):** a purged tenant keeps only its `usage_tenants` snapshot (id, name, slug, plan name, purge time), its hourly usage and its monthly active-user **counts**; no user ids. Everything else S4 deletes stays deleted.
- **Usage alerts warn only (D-61):** no code path reads usage to refuse, slow or limit anything.
- **Single controller (D-02):** the collector is process-local; no cross-replica locking.
- **Bounded growth:** retention pruning (25 months, D-58) runs with the collector; `usage_active_users` is bounded by users per tenant per month.
- **No new console writes.** Reads go into `api-surface.test.ts` on purpose.
- Schema: new numbered files only, no migration framework, recreate the local DB (`docs/database-development.md`).
- Each phase ends with a **live check** recorded in `.zecurity-obs/Planning/Pending-Live-Tests.md` before its tracker box is ticked.
- **No commit, push, PR or merge without the product owner's explicit go** for that step.

## Test gates (each phase)

- **Controller:** `go build ./... && go vet ./...`, then the full `go test ./...` with the DB env (`PKI_TEST_DATABASE_URL`, and `AUTH_TEST_VALKEY_URL` so the Valkey tests run too): 0 failures, and only the known hard-coded skips (a skipped DB test is a failed case).
- **Provider console:** `npm test`, `npm run lint`, `npm run build`.
- **Mutation checks** for each new guard:
  - a byte delta taken from the rate instead of the counters;
  - a relay restart (counter going down) producing a negative or full-counter delta;
  - a minute counted twice after a re-run or restart;
  - an incomplete hour reported as complete;
  - an unknown trust domain's bytes stored;
  - `active_users` counting a user twice in a month (two devices), or counting last month's user;
  - purge keeping user ids, or dropping the tenant's name;
  - relay-ops reading usage → 403;
  - a usage alert firing twice in a month, or not resolving at the rollover;
  - a usage alert resolving on an incomplete read.
- Phase-specific tests are in the phase files.

## Acceptance (end of sprint)

Live run on a **reset lab DB with current `main`**. Record the Git SHA and latest migration, use traffic probes, keep the logs.

- **F32:**
  - two tenants download known amounts through a relay at the same time: each tenant's `relay_bytes_*` for the hour matches what its client measured (within one heartbeat's bytes), and the two add up to the relay's own counters for the same span;
  - a relay restarted mid-download: usage continues, the loss is at most one heartbeat interval, never negative or doubled;
  - the controller stopped for 5 minutes: those minutes are unmetered and the hour is marked incomplete; the minutes before the stop are not counted twice;
  - one client session for a known 30 minutes = 30 session-minutes; connector- and shield-minutes for a known window;
  - active users: three users with sessions in the month (one on two devices) = 3; a user active only last month isn't counted;
  - a client on the **direct path**: its bytes are not counted (D-57), and the report doesn't claim otherwise.
- **F33:**
  - tenant detail Usage (month to date, last 12 months, daily chart with incomplete days marked, plan beside it); fleet Usage page sorted by each meter; CSV and JSON export with incomplete days stated;
  - a purged tenant still shows its usage with its name and a "Deleted" mark; its per-user rows are gone, the monthly count remains;
  - relay-ops get 403 on the usage API and see no Usage page or section;
  - a tenant usage read writes `tenant.usage.read`; the fleet list writes one `usage.fleet.read`.
- **Usage alerts (U4):** a `relay_bytes_to_clients` rule with a small threshold fires once for the tenant that crosses it (email and webhook, `scope: tenant`, its `tenant_id`), not for the other tenant; it doesn't fire again that month; it resolves at the month rollover (test clock); relay-ops don't see it; the rule change is audited with a reason.
- **X2:** each follow-up from the Sprint 24 acceptance run checked live.

Tracker: F32 and F33 ticked only after their live checks.

## Checklist

- [ ] Sprint 24 acceptance run (both) done; follow-ups recorded as X2; tracker F23, F26, F27 ticked
- [ ] Phase 0: D-56 … D-61 decided and written into the Decision Record
- [ ] Documents reviewed (scope draft, Sprint 25 path, phase files, Decision Record amendment) — signed off by Barath; implementation may start
- [ ] X2 — Sprint 24 follow-ups (Sathiya)
- [ ] U1a — Usage schema `071_usage` (Barath)
- [ ] U1b — Usage collector (Barath)
- [ ] U2 — Usage read API (Sathiya)
- [ ] U3 — Usage console + export (Sathiya)
- [ ] U4 — Usage alerts `tenant.usage_over` (Sathiya)
- [ ] Sprint acceptance run (both); tracker F32, F33

## Post-Sprint Fixes

_None yet._

## Out of scope

F34 billing plans and invoicing (pricing, invoices, payment); direct-path traffic metering (connector byte counters, agent change); tenant-facing usage and tenant-facing alerts (admin UI first, later); any usage enforcement (throttling, blocking, hard caps); F22 tunnel success rate, F40 SLA tracking; F28/F29 relay config push and version pinning; F19, F20, F31, F35, F37 (each needs a decision); F39 custom dashboards.
