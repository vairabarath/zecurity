---
type: phase
sprint: 25
owner: Sathiya (Person 2, M1)
features: [Sprint 24 follow-ups (F23, F26, F27), F33 per-tenant usage report, F26 usage alerts]
phases: [X2, U2, U3, U4]
decisions: [D-60, D-61, D-57, D-59, D-40, D-50, D-53]
status: signed off 2026-11-21; ready for implementation (X2 after the Sprint 24 acceptance run; U2 after U1a is merged)
---

# Sprint 25 — Sathiya: X2 follow-ups, U2 usage API, U3 usage console (F33), U4 usage alerts (F26)

Read `Sprint25/path.md` first (order, conflict zones, rules). Binding decisions: Decision Record amendment **2026-11-21 (usage)** — **D-60** (who sees usage, audit), **D-61** (usage alerts), **D-57** (meters), **D-59** (gaps), with **D-40** (relay-ops), **D-50** (alert scopes), **D-53** (rule management), **OQ-1** (tenant reads audited).

Order: **X2 → U2 → U3 → U4**. U2 starts when Barath's **U1a** (`071_usage`) is merged; it is tested against rows its tests insert, not against the collector.

---

## X2 — Sprint 24 follow-ups

Filled in after the Sprint 24 acceptance run (both, reset lab DB, current `main`). Each finding: a separate small change, its decision if one is needed, a test, a live recheck. Then tick F23, F26, F27 in the tracker.

---

## U2 — Usage read API — D-60, D-57, D-59

### What exists
- U1a tables: `usage_hourly`, `usage_collector_minutes`, `usage_active_users`, `usage_active_user_counts`, `usage_tenants`.
- Patterns: `providerquery` query services without `net/http` (D-04), keyset paging (`Page[T]`, cursors), M1's `GET /provider/metrics/{series}` window checks.
- Tenant read audit (OQ-1) in `provider/read_handlers.go`.

### Build
- `providerquery/usage.go`:
  - `TenantUsage(ctx, tenantID, from, to, granularity)` → per meter, per bucket (`hour` | `day` | `month`, UTC calendar boundaries), `quantity` and `complete` (every minute of the bucket in `usage_collector_minutes`; the current, unfinished bucket is `complete: false` and says so). `active_users` only for month buckets (rows for live tenants, `usage_active_user_counts` for purged ones). Limits: hour granularity ≤ 31 days, day ≤ 400 days, month ≤ 25 months.
  - `FleetUsage(ctx, month, meter, sort, cursor, limit)` → one row per tenant with every meter for that month, its name from `usage_tenants`, `purged` flag, plan name; keyset-paged by the sorted meter then tenant id.
  - `MonthToDate(ctx, month)` → per tenant, per meter: the one reader U4 uses.
- Handlers (`GET /provider/usage?tenant_id=&from=&to=&granularity=`, `GET /provider/usage/tenants?month=&meter=&sort=&cursor=&limit=`):
  - `decide()` action **`tenant.usage.read`** (super-admin only by the existing rule; relay-ops 403).
  - A per-tenant read writes a **`tenant.usage.read`** audit row (target tenant, the window); the fleet read writes one **`usage.fleet.read`** row. Both end in `.read`, so the incident timeline leaves them out (T1).
  - A purged tenant's usage is readable (D-58), marked `purged`.
- No writes.

### Tests (DB-backed)
- Buckets and UTC boundaries (a day in a non-UTC session time zone still splits at 00:00 UTC); quantities add up across buckets.
- Completeness: a bucket missing one collected minute is incomplete; the current bucket is incomplete.
- `active_users`: month buckets only; purged tenant read from the counts table.
- Fleet: sort by each meter, paging without repeats or gaps, purged tenants named.
- Authorization: relay-ops 403 on both routes; audit rows written with the right actions.
- **Mutation checks:** completeness ignored; relay-ops allowed; audit row missing; UTC boundary taken from the session time zone.

### Live check (U2)
Usage of two tenants after a known download (U1 live check), read per hour, day and month; relay-ops 403; audit rows present.

### U2 build record (branch `feat/sprint25-u2-usage-api`, stacked on U1 #245, 2026-11-21)
- **Query layer** (`providerquery/usage.go`):
  - `NewUsageWindow` widens `from` down and `to` up to whole UTC buckets. Limits: hour ≤ 31 days, day ≤ 400 days, month ≤ 25 months. Error codes: `invalid_granularity`, `invalid_range`.
  - `ReadUsageTenant` names the tenant from its workspace, else from its `usage_tenants` snapshot (marked `purged`); `ErrNotFound` when neither exists.
  - `ReadTenantUsage` returns **every** bucket of the window (zeros included) with `collected_minutes`, `minutes` and `complete` (all minutes collected, from `usage_collector_minutes`), and `active_users` on month buckets (rows for a live tenant, kept counts for a purged one).
  - `ReadMonthUsage` returns every tenant's usage for a UTC month, plus the month's completeness. It lists every counted tenant (zeros included) and every tenant with usage in the month (purged ones by snapshot). **U4's month-to-date reader.**
  - `PageFleetUsage` sorts by a meter, largest first, ties by tenant id. The keyset cursor is base64 of `value|tenant id`. Sorting is in Go: one month of every tenant is a small set before production; revisit if the fleet grows large.
- **Handlers** (`provider/usage_read.go`):
  - `GET /provider/usage?tenant_id=&from=&to=&granularity=`: default the current UTC month by day.
  - `GET /provider/usage/tenants?month=YYYY-MM&sort=&cursor=&limit=`.
  - `CanReadUsage` uses `tenant.usage.read`, so relay-ops get 403.
  - Both reads are **audited and fail closed** like tenant detail (OQ-1): the read, the serialization and the audit row run in one REPEATABLE READ transaction, and the body is written only after COMMIT.
  - Audit rows: `tenant.usage.read` (target tenant; details view, granularity, from, to) and `usage.fleet.read` (target `usage`/month; details month, sort). 400, 403 and 404 responses are not audited.
  - Routes are added in `main.go` and the read-handler test mux.
- **Tests** (`provider/usage_read_test.go`):
  - super-admin only (relay-ops 403, tenant JWT 401, no audit on refusal);
  - hour buckets with one complete and one incomplete hour; another tenant's usage never shown;
  - the day window aligned to UTC;
  - month active users;
  - a purged tenant named by its snapshot, with kept counts;
  - unknown tenant 404, not audited;
  - every bad request returns 400 with its code and no audit row;
  - fail-closed when the audit insert fails (500, no data);
  - fleet sorted, paged without repeats, only the month counted, one audit row per request;
  - sort by active users.
- **Mutation checks, all caught (11):**
  - completeness ignored;
  - tenant filter dropped;
  - relay-ops allowed;
  - audit row missing;
  - fleet month without an upper bound (survived the first test, whose out-of-month row was on the already-first tenant; values now checked);
  - cursor repeating a row;
  - purged counts omitted;
  - purged tenant not named;
  - hour limit raised;
  - a 404 audited;
  - audit failure ignored (fail-open).
- **Gates (2026-11-21):** `go build ./... && go vet ./...` clean; full `go test ./...` with every test DB env set: 49 packages ok, 0 failures, the 2 known skips (`TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed`, `TestEnroll_CSRSignatureInvalid`).
- **Live check:** pending (after U1's live check on the lab).

---

## U3 — Usage console + export

### Build
- `provider-console/src/api/usage.ts` (`getTenantUsage`, `listFleetUsage`); both added to `api-surface.test.ts` as reads on purpose.
- **Tenant detail → Usage** (super-admin; tenant detail is already super-admin only):
  - this month so far and the last 12 months per meter (bytes as GB, minutes as hours, active users as a count);
  - a daily chart for the selected month (M2's `MetricChart`, bars or a line; incomplete days marked, never drawn as 0);
  - the tenant's plan beside the usage.
  - Read once when the section opens and again on a month change or Refresh (no polling).
- **Usage page** (`/usage`, super-admin nav only): the fleet for a month, sortable by each meter, keyset pages; purged tenants with their kept name and a "Deleted" badge (no link).
- **Export:** CSV and JSON of what is shown (tenant per day, or fleet per tenant), client-side like F38: formula cells neutralised, a row cap, and the file states which days or months are incomplete.

### Tests
- Per section: requests (one per open, per month change), units and rounding, incomplete days shown as incomplete (not 0), purged tenant badge without a link, export content and formula neutralising, relay-ops see no nav entry and no section (and the API refuses them anyway).
- `npm test`, `npm run lint`, `npm run build`.

### Live check (U3)
Tenant usage and fleet page against the U1 live numbers; export opened; a purged tenant shown with its name.

---

## U4 — Usage alerts (`tenant.usage_over`) — D-61, D-50, D-53

### What exists
- A1: `internal/alerting/kinds.go` (built-in kinds), the evaluator with firing → resolved state per (rule, target), "read errors never resolve"; A2 delivery; A3 rules console.
- `069_alerts`' `alert_rules_kind_scope` CHECK lists the kinds per scope.

### Build
- `072_usage_alert_kind.sql`: replace the `alert_rules_kind_scope` CHECK to add `tenant.usage_over` to the tenant kinds (drop and re-add the constraint in the new file; `069_` is never edited).
- Kind `tenant.usage_over`, scope **tenant**, parameters `meter` (one of D-57's meters) and `threshold` (a positive integer in the meter's unit: bytes, minutes or users), validated on create/update.
- Evaluation: per tick, `providerquery.MonthToDate` (U2's reader); a tenant whose month-to-date `quantity >= threshold` fires (target = tenant, `tenant_id` set). Once firing it stays firing for the rest of the month (usage only grows); **resolves at the UTC month rollover**. An incomplete or failed read never resolves it (A1 rule).
- Payload summary e.g. "Tenant acme used 512 GB relayed to clients this month (threshold 500 GB)"; delivered to the provider only (D-50), relay-ops never see it.
- Console (A3 rules form): the new kind with meter and threshold fields (unit shown); no new write route.

### Tests
- Fires once when crossing, not before; never twice in a month; resolves at the rollover (test clock); a read error doesn't resolve; another tenant under the threshold doesn't fire; relay-ops don't see it; the rule change is audited with a reason.
- **Mutation checks:** fires twice in a month; never resolves at the rollover; resolves on a read error; fires for every tenant.

### Live check (U4)
A small `relay_bytes_to_clients` threshold: the tenant that crosses it gets one alert (email and webhook, `scope: tenant`, its `tenant_id`); the other tenant none; no repeat that month.

---

## Post-Phase Fixes

_None yet._
