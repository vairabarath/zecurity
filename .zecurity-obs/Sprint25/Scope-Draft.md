---
type: sprint-scope-draft
sprint: 25
title: Provider Dashboard — usage metering and reports (F32, F33)
status: DRAFT for Barath's sign-off (2026-11-21) — the three review questions are answered (below); nothing is binding until sign-off; then Sprint25/path.md, the phase files and a Decision Record amendment (D-56 … D-61)
baseline: main cb441cc9 (Sprint 24 merged: X1, A1–A3, T1, R1, M1, M2 + review fixes; Sprint 24 acceptance run still to do)
---

# Sprint 25 — scope draft

> **Status: draft.** Theme chosen on 2026-11-21: usage metering. The three review questions were answered on 2026-11-21 (see "Answered"): active users is a meter, a purged tenant's usage is kept with its id **and name**, and usage thresholds raise F26 alerts. D-56 … D-61 are still proposals until Barath signs off; the binding plan will be `Sprint25/path.md`.

## Goal

Sprint 24 made the fleet observable (F23 charts, F26 alerts, F27 timeline). Sprint 25 makes it **measurable per tenant**: how much each tenant used, per day and per month, in units that a later billing feature (F34) can price.

- **F32 usage collection:** session time, bytes relayed and connector time, per tenant, stored exactly enough to be billed later.
- **F33 per-tenant usage report:** the provider sees each tenant's usage (this month, past months, per day), the fleet's top users, and can export it.
- **Usage alerts (F26 extension):** a tenant crossing a monthly usage threshold raises an F26 **tenant** alert to the provider. It warns; it never blocks or limits anything.

**Not billing.** Sprint 25 measures, reports and alerts; it prices nothing, issues no invoice (F34 needs its own decision) and enforces nothing. Plans (B1) keep their count limits (users, resources, …); usage is shown next to the plan, not enforced against it.

| Feature | Today | Proposed phases | Size |
|---|---|---|---|
| **F32** Usage collection | ⬜ Nothing is metered. M1 stores **rates and counts** every minute (7 days) and hourly roll-ups (400 days); R1 gives per-tenant **cumulative** relay byte counters in every heartbeat. | **U1** usage ledger + collector | 🟡 Medium (migration + background job; no agent change) |
| **F33** Per-tenant usage report | ⬜ | **U2** read API, **U3** console report + export | 🟡 Medium |
| **F26** usage alerts | F26 has built-in tenant rule kinds (A1) and delivery (A2) | **U4** `tenant.usage_over` rule kind | 🟢 Small (after U1) |
| **Sprint 24 follow-ups** | Sprint 24 acceptance run not yet done | **X2** (whatever the run finds) | 🟢 Small |

## Why a separate usage ledger (not M1's metric store)

M1 is built for charts; usage needs to be **countable**:

| | M1 metric store (F23) | Proposed usage ledger (F32) |
|---|---|---|
| What it stores | Rates and gauges (bytes/s, sessions now) | Quantities (bytes, session-minutes, connector-minutes) |
| Precision | A 1-minute sample of a 60 s rate; hourly **averages** after 7 days | **Exact** bytes from R1's cumulative counters (deltas, not integrated rates); whole minutes for time meters |
| Retention | 7 days of samples, 400 days of hourly averages | Long enough for billing disputes (D-58) |
| Gaps | A gap is a gap (D-49) | A gap is **unmetered**, never estimated; the report says which hours are incomplete |

Integrating M1's rates would give "about this much" (a 7-day-old minute is gone; an hourly average times 3,600 is an estimate). Billing later needs numbers that add up, so U1 counts at the source.

## What already exists (reuse, don't rebuild)

| Building block | Where | Used by |
|---|---|---|
| R1 per-workspace cumulative relay counters (`bytes_client_to_connector`, `bytes_connector_to_client`, `active_streams`) per heartbeat, trust domain → workspace mapping, restart detection (uptime, counter going down) | `controller/internal/relay/workspace_traffic.go`, proto field 13 (D-54) | U1 bytes relayed (deltas) |
| M1 minute sampler with tenant counts: live device sessions (`session_idle_until`, `session_max_until`), active connectors, active shields | `controller/internal/metricstore/sampler.go` (`tenantCounts`) | U1 session-minutes, connector-minutes (same query, counted instead of sampled) |
| Metric read API patterns (series, windows, role split) | `providerquery/metrics.go`, `GET /provider/metrics/{series}` (D-49) | U2 API shape |
| Tenant detail and its audit (OQ-1: every view writes `tenant.read`) | console `TenantDetail.tsx`, `provider/read_handlers.go` | U3 usage section |
| Charts (hand-written SVG, gaps as breaks) | console `MetricChart.tsx` (M2) | U3 daily usage chart |
| Client-side CSV/JSON export (formula cells neutralised, row cap) | console audit export (F38, PR #190) | U3 export |
| Plans (B1) and their assignment | `plans`, `workspaces.plan_id` (migration 062) | U3 shows the plan beside the usage |
| Tenant deletion and purge (S4, D-45/D-46) | `tenant_purges`, `tenantexport` | D-58: what happens to a deleted tenant's usage |

## Proposed phases

### X2 — Sprint 24 follow-ups (first, small)
The Sprint 24 acceptance run (both, on a reset lab DB) happens before Sprint 25 starts; anything it finds lands here, as X1 did for Sprint 23. Possible candidates already known (each only if Barath wants it):
- **D-51 follow-up:** fold the F14 daily certificate email into an F26 built-in rule (D-51 said "later").
- Tracker: tick F23, F26, F27 after the acceptance run.

### U1 — F32 usage ledger and collector (controller, migration `071_usage`)
- **Table** `usage_hourly (tenant_id, meter, hour, quantity BIGINT, complete BOOLEAN)` — one row per tenant, meter and UTC hour; integers only. Proposed meters (D-57):
  - `relay_bytes_to_clients`, `relay_bytes_to_connectors` — bytes the relays bridged for the tenant, from R1's **cumulative counters**: the controller adds the **delta** between two heartbeats of the same relay run. A relay restart (uptime shorter than the gap, or a counter going down) starts a new baseline: the bytes since that relay's last heartbeat before the restart are lost (at most one heartbeat interval), never guessed.
  - `session_minutes` — for each minute, the number of the tenant's client devices with a live session (the M1 query), added up.
  - `connector_minutes`, `shield_minutes` — for each minute, the number of the tenant's active connectors / shields, added up.
  - **`active_users`** (monthly) — the number of **distinct users** of the tenant with a live client session at any minute of the UTC calendar month (answered 2026-11-21). A distinct count can't be added up from hours, so it has its own table: `usage_active_users (tenant_id, month, user_id, first_seen)`, one row per user per month (idempotent insert each minute), and the monthly count is `count(*)`. Daily distinct users are not stored (the monthly figure is the billing one).
- **Collector:** runs with the M1 sampler tick (one controller, D-02). Time meters are written per minute with an idempotent upsert keyed by the minute (a restart or a re-run never double-counts). Byte deltas are accumulated in memory and flushed every minute; the flush and its "already counted up to" marker are written in one transaction, so a crash loses at most the unflushed minute and never counts it twice.
- **Completeness:** an hour is `complete` when the collector ran every minute of it; a controller outage leaves the hour incomplete (and its usage lower). Nothing is back-filled or estimated.
- **Retention and deletion (D-58):** usage is kept 25 months. A **purged tenant's usage is kept**, with its **id and name** (answered 2026-11-21): U1 adds `usage_tenants (tenant_id, name, slug, plan_name, purged_at)`, a snapshot written when the tenant is created or renamed and frozen at purge, so reports and a later invoice can still name it. The per-user rows of `usage_active_users` are reduced to the monthly count at purge (no user ids of a deleted tenant are kept). This is an exception to the S4 purge (D-45/D-46), so D-58 **amends D-46**.
- **Tests:** DB-backed: deltas across heartbeats, relay restart, counter reset, unknown trust domain ignored, two relays for one tenant add up, idempotent re-run of a minute, crash between accumulate and flush, hour completeness, UTC hour boundaries; mutation checks as in Sprint 24.

### U2 — F33 usage read API (controller)
- `GET /provider/usage?tenant_id=&from=&to=&granularity=hour|day|month` → per meter, per bucket, with `complete` per bucket; month and day buckets are UTC calendar boundaries (D-57). `active_users` is returned for month buckets only.
- Works for a **purged** tenant too (its usage and snapshot name, marked deleted).
- `GET /provider/usage/tenants?month=YYYY-MM&meter=&sort=` → the fleet: each tenant's month-to-date (or that month's) usage, sortable, keyset-paged.
- Super-admin only (tenant data, D-40 extended); relay-ops get **403**. New `decide()` action `usage.read` (D-60).
- A read of one tenant's usage is audited like a tenant detail view (`tenant.usage_read`, OQ-1); the fleet list writes one `usage.fleet_read` row per request (D-60).

### U3 — F33 console
- **Tenant detail → Usage:** this month so far and the last 12 months per meter (bytes shown as GB, time as hours), a daily chart for the selected month (incomplete days marked), the tenant's plan beside it.
- **Usage page** (super-admin nav): the fleet for a month, sortable by any meter, linking to each tenant.
- **Export:** CSV and JSON of the shown month (per tenant per day, or fleet per tenant), client-side like F38; the file states which days are incomplete.
- Deleted tenants appear with their kept name and a "Deleted" badge (no link to a tenant page that no longer exists).
- New reads go into `api-surface.test.ts` on purpose; no new writes (U4's rule kind uses the existing A3 rule writes).

### U4 — usage alerts (F26 extension, after U1)
- A new built-in **tenant** rule kind **`tenant.usage_over`** (D-61) with parameters `meter` and a monthly `threshold` (e.g. `relay_bytes_to_clients` over 500 GB, `active_users` over 50). One rule applies to every tenant; several rules can exist (different meters or levels, e.g. 80 % and 100 % of a figure).
- **Fires once per tenant per month** when the month-to-date usage crosses the threshold; **resolves at the UTC month rollover** (usage restarts). No re-notify while firing unless the rule sets one. An incomplete hour never resolves it (A1 rule: read errors never resolve).
- Scope **tenant** (`tenant_id` set, D-50): delivered to the provider only, like every tenant alert; relay-ops never see it.
- Rules are managed in the existing Alerts console (A3), super-admin only, reason and before/after audit (D-53). **Warns only:** nothing is blocked, throttled or limited.
- Evaluated by the A1 evaluator from the ledger (U2's query), not from M1.

## Decisions to make first (Phase 0, proposed D-56 … D-61)

| Proposed ID | Question | Recommendation |
|---|---|---|
| **D-56** | Scope of Sprint 25 | F32 + F33 (+ X2). **No pricing or invoicing** (F34 later, with its own decision). No agent change. |
| **D-57** | What is metered, and how | Meters: relay bytes per direction (exact, from R1 cumulative counters), session-minutes, connector-minutes, shield-minutes, and **active users per month** (distinct users with a live session in the month; answered 2026-11-21). **UTC** hours, days and calendar months. **Only relayed traffic is metered:** a client that reaches its connector **directly** (Sprint 20 direct path) sends no bytes through a relay, so those bytes are not counted. Metering direct traffic would need connector byte counters (an agent change) — not this sprint. |
| **D-58** | Storage, retention, deletion | Postgres table `usage_hourly`, integer quantities. Kept **25 months** (a full year back plus the current year for disputes; longer than D-49's 400 days because these are billable quantities). When a tenant is **purged** (S4), its usage is **kept with its id and name** (snapshot in `usage_tenants`) until retention ends, because a later invoice or dispute needs to name it (answered 2026-11-21); per-user active-user rows are reduced to the monthly count. **Amends D-46** (the purge keeps this one record). |
| **D-59** | Gaps and accuracy | A minute the controller didn't collect is **unmetered** (lower usage, never estimated); a relay restart loses at most the bytes since its last heartbeat; reports mark incomplete hours and days. No back-fill from M1 (estimates must not mix with counts). |
| **D-60** | Who sees usage, and audit | **Super-admin only** (`usage.read`; relay-ops 403, D-40 extended). Reading one tenant's usage is audited like a tenant detail view (`tenant.usage_read`, OQ-1); the fleet usage list is not audited per tenant (one `usage.fleet_read` row per request). **Tenant-facing usage** (in the admin UI) is a later feature; the API is shaped so it can be added with a tenant filter. |
| **D-61** | Usage alerts | New built-in tenant rule kind `tenant.usage_over` (meter + monthly threshold), fires once per tenant per month, resolves at month rollover, provider-only delivery (D-50), super-admin managed (D-53); **warns only, never enforces** (answered 2026-11-21). |

**Answered (2026-11-21):**
1. **Active users per month is a meter** → D-57, U1 (`usage_active_users`).
2. **A purged tenant's usage is kept by id and name** until retention ends → D-58 (`usage_tenants` snapshot; amends D-46).
3. **Usage thresholds raise alerts** (F26 tenant alerts to the provider, warning only) → D-61, U4.

**Still open for sign-off:** D-56 … D-61 as a whole, the 25-month retention (D-58), and the split below.

## Suggested split (two members, as in Sprint 24)

| Member | Phases | Order |
|---|---|---|
| **Barath (M2)** | **U1** usage ledger + collector (builds on his R1 counters and M1 sampler) | U1 first; U2 can start against U1's table as soon as the migration is merged |
| **Sathiya (M1)** | **X2**, **U2** read API, **U3** console, **U4** usage alerts (builds on A1–A3) | X2 → U2 → U3 → U4 |

U1 and U2 share one contract: the `usage_hourly` table and meter names. Proposal: Barath merges `071_usage` (schema only, with its constraint tests) first, so U2 builds against it while U1's collector is still in progress.

**Migrations (to reserve):** `071_usage` (U1: `usage_hourly`, `usage_active_users`, `usage_tenants`); U4 adds a rule kind, and `069_alerts`' `alert_rules_kind_scope` CHECK lists the kinds, so U4 needs `072_usage_alert_kind` (a new file; never edit 069). Latest today: `070_alert_delivery_queue`. Check `ls controller/migrations | tail -3` before adding; no migration framework; `[schema: reset DB]`.

**Conflict zones:** `controller/internal/relay/workspace_traffic.go` (U1 reads the cumulative counters; R1 code), `controller/internal/metricstore/sampler.go` (U1 runs on its tick), `controller/cmd/server/main.go` (job and routes), `controller/internal/provider/authz.go` (`usage.read`), the A1 evaluator and rule-kind registry (U4), the S4 purge (U1's D-58 exception), `provider-console/src/api/api-surface.test.ts`, `TenantDetail.tsx`, the provider navigation (`roles.ts`, `Layout.tsx`).

## Not in Sprint 25 (proposed)
- **F34** billing plans and invoicing (prices, invoices, payment): needs its own decision; Sprint 25's usage is its input.
- **Direct-path traffic metering** (connector byte counters, agent change).
- **Tenant-facing usage** in the admin UI (later; D-60 keeps the API ready).
- F22 tunnel success rate, F40 SLA tracking; F28/F29 relay config push and version pinning; F19, F20, F31, F35, F37 (each needs a decision); F39 custom dashboards.
- Tenant-facing alert delivery (D-50: later, admin UI first), including tenant-facing usage alerts.
- Any enforcement on usage (throttling, blocking, hard caps): U4 only warns.

## Test gates (as in Sprint 24)
- Build, vet and the full Go suite with every DB-backed test running (a skipped DB test is a failed case); console tests, lint and build; mutation checks on the metering rules.
- **Live acceptance on the lab:**
  - Two tenants download known amounts through a relay at the same time: each tenant's `relay_bytes_*` for the hour matches what its client measured (within the bytes of one heartbeat), and the two add up to the relay's own counters for the same span.
  - A relay restarted mid-download: usage continues after the restart; the loss is at most one heartbeat interval, never a negative or doubled count.
  - The controller stopped for 5 minutes: those minutes are unmetered and the hour is marked incomplete; restarting doesn't double-count the minutes before the stop.
  - Session-minutes and connector-minutes for a known window (e.g. one client session for 30 minutes = 30 session-minutes).
  - A client on the **direct path**: its bytes are not counted (as D-57 says), and the report doesn't pretend otherwise.
  - Active users: three users with sessions in the month (one on two devices) = 3; a user active only last month isn't counted this month.
  - Purge a tenant with usage: its usage stays readable with its name and a "Deleted" mark; its per-user active-user rows are gone, the monthly count remains.
  - Usage alert: a rule `relay_bytes_to_clients` over a small threshold fires once for the tenant that crosses it (email and webhook, `scope: tenant`, its `tenant_id`), not for the other tenant; it doesn't fire again that month; relay-ops don't see it.
  - Console: tenant usage, fleet usage page, export; relay-ops get 403 and see no Usage page.

## Next steps
1. Barath signs off D-56 … D-61, the retention and the split (the three review questions are answered).
2. On sign-off: write `Sprint25/path.md` and the two phase files, and amend the Decision Record (D-56 …).
3. Before U1/U2 start: run the Sprint 24 acceptance run and record anything it finds as X2.
