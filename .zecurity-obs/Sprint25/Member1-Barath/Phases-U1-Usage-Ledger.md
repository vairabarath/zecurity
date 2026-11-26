---
type: phase
sprint: 25
owner: Barath (Person 1, M2)
features: [F32 usage collection]
phases: [U1a, U1b]
decisions: [D-57, D-58, D-59, D-54, D-02, D-46]
status: signed off 2026-11-21; ready for implementation (after the Sprint 24 acceptance run)
---

# Sprint 25 — Barath: U1 usage ledger and collector (F32)

Read `Sprint25/path.md` first (order, conflict zones, rules). Binding decisions: Decision Record amendment **2026-11-21 (usage)** — **D-57** (what is metered), **D-58** (storage, retention, deletion; amends D-46), **D-59** (gaps and accuracy), with **D-54** (R1 counters), **D-02** (single controller), **D-49** (M1 unchanged).

Order: **U1a (schema, merged first) → U1b (collector)**. U1a is the contract with Sathiya's U2: merge it early, on its own.

---

## U1a — Usage schema (`071_usage.sql`)

### Build
- `usage_hourly`
  - `tenant_id UUID NOT NULL`, `meter TEXT NOT NULL`, `hour TIMESTAMPTZ NOT NULL`, `quantity BIGINT NOT NULL CHECK (quantity >= 0)`, `minutes_collected SMALLINT NOT NULL CHECK (minutes_collected BETWEEN 0 AND 60)`; primary key `(tenant_id, meter, hour)`.
  - `meter` CHECK: `relay_bytes_to_clients`, `relay_bytes_to_connectors`, `session_minutes`, `connector_minutes`, `shield_minutes` (D-57).
  - `hour` on the hour, time-zone independent (same epoch CHECK as M1's `metric_rollups_on_hour`).
  - **Completeness** (D-59): an hour is complete when `minutes_collected = 60`. The collector counts the minutes it ran per hour in a separate row set (see `usage_collector_minutes`), and readers join it; or keep `minutes_collected` on every row of the hour — pick one in the PR and say why. Recommendation: a small `usage_collector_minutes (minute TIMESTAMPTZ PRIMARY KEY)` table (one row per collected minute, fleet-wide): completeness is the same for every tenant, so it isn't repeated per row, and a reader counts minutes per bucket.
  - No foreign key to `workspaces` (usage outlives a purged tenant, D-58).
- `usage_active_users`
  - `tenant_id UUID`, `month DATE` (first day of the UTC month, CHECK day = 1), `user_id UUID`, `first_seen TIMESTAMPTZ`; primary key `(tenant_id, month, user_id)`.
  - **Purged-tenant counts:** `usage_active_user_counts (tenant_id, month, users INTEGER)`, written at purge (D-58: no user ids of a deleted tenant kept). Readers take the count from rows for live tenants and from this table for purged ones.
- `usage_tenants`
  - `tenant_id UUID PRIMARY KEY`, `name TEXT`, `slug TEXT`, `plan_name TEXT`, `updated_at TIMESTAMPTZ`, `purged_at TIMESTAMPTZ NULL`.
  - Kept current by the collector (upsert per tick for active tenants, cheap) and frozen at purge.
- `usage_relay_baselines` — **so a controller restart loses no bytes:**
  - `relay_id UUID`, `workspace_id UUID`, `relay_started_at TIMESTAMPTZ` (heartbeat time minus uptime, rounded to the second), `to_clients BIGINT`, `to_connectors BIGINT`, `at TIMESTAMPTZ`; primary key `(relay_id, workspace_id)`.
  - The last counter values already counted. Written in the **same transaction** as the usage they produced.
- Grants: `ztna_app` SELECT, INSERT, UPDATE, DELETE as needed (UPDATE on `usage_hourly` for the upsert, DELETE for retention).

### Tests (DB-backed)
- Each CHECK refuses a bad row (negative quantity, unknown meter, off-hour timestamp, month not on day 1).
- Upsert adds to an existing hour; the primary keys refuse duplicates.

---

## U1b — Usage collector

### What exists
- `relay.WorkspaceTrafficTracker` (`internal/relay/workspace_traffic.go`, R1): per relay, the latest cumulative counters per workspace, restart detection (`uptime + 5 s slack >= gap`, counters going down). **In memory only:** it starts empty after a controller restart.
- `metricstore.Sampler` (`internal/metricstore/sampler.go`, M1): a minute tick; `tenantCounts` defines a live session (`client_devices` not revoked, `session_idle_until > now`, `session_max_until > now`), an active connector (`connectors.status = 'active'`) and an active shield.
- S4 purge: `internal/tenantlifecycle/purge.go`.

### Build
1. **Byte deltas (D-57, D-59).**
   - A hook on the relay heartbeat path, next to `recordWorkspaceTraffic` (`internal/relay/heartbeat.go`), passes each heartbeat's per-workspace counters to the usage collector (an interface; `relay` must not import the usage package's DB code if that creates a cycle).
   - For each (relay, workspace): load the baseline (memory, else `usage_relay_baselines`). Same relay run (same `relay_started_at` within the slack) and counters not lower → delta = current − baseline. New run, or a counter lower → **new baseline, delta 0** (the bytes after the last heartbeat before the restart are lost; D-59). No baseline at all (first sight) → baseline only, delta 0.
   - Unknown or revoked trust domain → nothing stored (R1 already drops it; keep that true).
   - Write the delta into `usage_hourly` for the **hour of the heartbeat's arrival** (`ON CONFLICT … DO UPDATE SET quantity = quantity + EXCLUDED.quantity`) and the new baseline into `usage_relay_baselines`, **in one transaction**. A failed transaction leaves both unchanged, so the next heartbeat counts the whole span (no loss, no double count).
   - Batch per heartbeat (one transaction per relay heartbeat, all its workspaces).
2. **Time meters and active users (each minute).**
   - On the M1 minute tick (or a sibling ticker with the same minute alignment): for minute `m`, if `usage_collector_minutes` has no row for `m`, then in one transaction: insert the row for `m`; add each active tenant's live sessions, active connectors and active shields to `session_minutes`, `connector_minutes`, `shield_minutes` for `m`'s hour; insert `(tenant, month, user)` into `usage_active_users` for each user with a live session (`ON CONFLICT DO NOTHING`); upsert `usage_tenants`.
   - The `usage_collector_minutes` row makes the minute **idempotent**: a re-run or a restart within the same minute finds it and does nothing.
   - A minute the controller wasn't running has no row → the hour is incomplete (D-59). **No catch-up** of past minutes.
   - **One definition:** extract `tenantCounts`' predicates into one shared query (or SQL fragments) used by both M1 and U1b, so F23 and F32 can't disagree.
3. **Retention (D-58):** prune `usage_hourly`, `usage_collector_minutes`, `usage_active_users`, `usage_active_user_counts` older than 25 months, and `usage_tenants` rows of purged tenants with no usage left. Run hourly with the collector; a test setting shortens it.
4. **Purge (D-58, amends D-46):** in `tenantlifecycle/purge.go`, in the purge transaction: set `usage_tenants.purged_at` (name, slug and plan name stay frozen), write `usage_active_user_counts` from the tenant's `usage_active_users` rows, then delete those rows. Nothing else in the purge changes.
5. Wire in `cmd/server/main.go` (own block). Config for tests: `USAGE_RETENTION`.

### Tests (DB-backed, with mutation checks)
- Byte deltas: two heartbeats → exact delta; three relays × two tenants add up; a delta spanning an hour boundary lands in the later hour.
- Relay restart (uptime < gap) and counter decrease → new baseline, delta 0, never negative or the full counter.
- **Controller restart:** tracker emptied, baseline loaded from `usage_relay_baselines` → the bytes between the last pre-restart heartbeat and the first post-restart one are counted once.
- Failed transaction (inject an error) → neither usage nor baseline written; the next heartbeat counts the whole span once.
- Unknown trust domain → no row.
- Time meters: a minute counted once even when the tick runs twice; a skipped minute leaves the hour with 59 collected minutes (incomplete).
- Active users: one user on two devices counted once per month; a user active last month not counted this month; month boundary in UTC.
- Purge: name and slug kept, `purged_at` set, user rows replaced by the count; other tenants untouched.
- Retention removes only rows older than the setting.
- **Mutation checks:** delta from the rate instead of the counters; restart producing a negative or full-counter delta; baseline not persisted (controller restart double-counts or loses); minute idempotence removed; incomplete hour reported complete; unknown trust domain stored; active user counted twice; purge keeping user ids or dropping the name.

### Live check (U1)
- Two tenants download known amounts through one relay at once: each tenant's hourly bytes match its client's measurement within one heartbeat's bytes; the two add up to the relay's own counter deltas.
- Restart the relay mid-download: loss at most one heartbeat interval.
- Restart the controller mid-download: no loss, no double count.
- Stop the controller for 5 minutes: those minutes unmetered, the hour incomplete.
- A client on the direct path: its bytes are not counted (D-57).

### Implementation record (U1a + U1b, branch `feat/sprint25-u1a-usage-schema`, 2026-11-21)
Built by Sathiya (with Claude Code) while Barath ran the Sprint 24 acceptance run; Barath reviews.
- **U1a, `071_usage.sql`:**
  - Tables: `usage_hourly`, `usage_collector_minutes`, `usage_active_users`, `usage_active_user_counts`, `usage_tenants`, `usage_relay_baselines`.
  - Time checks are zone-independent: on the hour and on the minute by epoch, the month on day 1.
  - Quantities and counts are non-negative.
  - No foreign keys, because usage outlives a purge.
  - Privileges: 053's default privileges already give `ztna_app` full DML, so the file only **revokes UPDATE on `usage_collector_minutes`** (a collected minute is never rewritten). Granting is not enough here.
- **Completeness:** `usage_collector_minutes`, one row per collected minute fleet-wide (the recommended option above). Missing usage rows in a complete hour mean 0.
- **Shared definitions:** `providerquery/activity.go` (`CountedTenantSQL`, `LiveClientSessionSQL`, `ActiveConnectorSQL`, `ActiveShieldSQL`), used by M1's `tenantCounts` and the collector. One definition each.
- **Bytes** (`usage.Collector.RecordRelayCounters`, called from `relay.recordWorkspaceTraffic` through the `relay.WorkspaceUsageSink` interface):
  - One transaction per relay heartbeat.
  - A per-relay **run marker** row (tenant id = nil UUID) in `usage_relay_baselines`, locked `FOR UPDATE`.
  - Same run = R1's rule (`uptime + 5 s >= gap`).
  - Deltas:
    - first sight of a relay (no marker): baseline only, 0 counted;
    - a new run (relay restart): the counters themselves (everything since the restart is new; only the bytes between the last counted heartbeat and the restart are lost);
    - a tenant new in a watched run, or its counters lower (entry re-created): the counters themselves;
    - otherwise the difference.
  - A late or repeated heartbeat counts nothing.
  - Bytes go to the hour the heartbeat arrived in.
  - **Refinement of the phase plan:** a relay restart counts the new run's counters instead of 0. That loses less, and it stays within D-59: exact, never negative or doubled, at most the bytes after the last heartbeat before the restart lost.
- **Minutes** (`CollectMinute`, on its own minute ticker, `usage-collector` worker):
  - One transaction under a transaction-scoped advisory lock.
  - Writes the minute's `usage_collector_minutes` row first (a conflict means already counted: nothing written).
  - Adds `session_minutes`, `connector_minutes` and `shield_minutes` (zero counts not stored).
  - Inserts each live-session user into `usage_active_users` for the UTC month.
  - Upserts `usage_tenants` only when the name, slug or plan changed, never after a purge.
- **Purge (D-58):**
  - `usage.FreezeTenant` runs in `tenantlifecycle.Purger.destroy`'s transaction, before `tenant_purge`.
  - It writes or freezes the snapshot (`purged_at`) and turns `usage_active_users` into `usage_active_user_counts`.
  - It takes the same advisory lock as the minute collection, so no minute can add user ids for the tenant after the freeze.
- **Retention:** `Prune` runs hourly with the collector: 25 months by default (`USAGE_RETENTION` overrides). It also removes old baselines and the snapshots of purged tenants with no usage left.
- **Tests:**
  - `internal/usage/schema_db_test.go`: constraints in a +05:30 session time zone, upsert, uniqueness, `ztna_app` privileges.
  - `internal/usage/collector_db_test.go`:
    - exact deltas across two relays, two tenants and an hour boundary;
    - first sight, late and repeated heartbeats;
    - relay restart and counter reset;
    - controller restart (new process) and a failed write;
    - minutes counted once per minute, with deleted workspaces not metered;
    - active users distinct per UTC month;
    - freeze keeps the name and drops user ids;
    - retention.
  - `tenantlifecycle` purge end state: the snapshot is frozen with the tombstone's slug, and no active-user rows remain.
- **Mutation checks, all caught (20):**
  - U1a (7): meter CHECK, on-the-hour CHECK, non-negative quantity, on-the-minute CHECK, month on day 1, the UPDATE revoke, the active-user primary key.
  - U1b (13):
    - first sight counting the counters;
    - a restart or reset taking the difference;
    - a late heartbeat counted;
    - same-run detection inverted;
    - the baseline not persisted;
    - minute idempotence removed;
    - the live-session definition loosened;
    - the counted-tenant definition loosened (the first test missed this; the deleted workspace now has live sessions and agents);
    - active users not distinct;
    - the freeze keeping user ids;
    - the snapshot rewritten after purge;
    - pruning inside the retention;
    - the purge not calling the freeze.
- **Gates (2026-11-21):**
  - `go build ./... && go vet ./...` clean.
  - Full `go test ./...` with `PKI_TEST_DATABASE_URL`, `AUTH_TEST_VALKEY_URL` (`/15`), `ENROLLMENT_`, `RESOURCE_`, `SHIELD_` and `AUTH_TEST_DATABASE_URL`: 0 failures, only the 2 known skips (`TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed`, `TestEnroll_CSRSignatureInvalid`).
  - Note: without the extra `*_TEST_DATABASE_URL` variables, about 226 DB tests in `graph/resolvers`, `connector`, `resource` and `shield` skip; set them all.
- **Live check:** pending (lab, after the Sprint 24 acceptance run): the U1 live check above.

---

## Post-Phase Fixes

### Fix: F32-1 — relay restart lost up to 5 minutes of bytes (live run 2026-11-24)
**Issue:** a relay restart lost every byte since the last *DB-written* heartbeat (≈155 MB in the live run), not since the last heartbeat (D-59).

**Root Cause:** `controller/internal/relay/heartbeat.go` returns early on throttled heartbeats (the B2 DB-write cadence, every 5 minutes), and the usage collector was only called from `recordWorkspaceTraffic`, after the DB write.

**Fix Applied:** the throttled path now passes the counters to the usage collector too; R1's rate tracker keeps the DB-write cadence (its window is unchanged).
```go
// heartbeat.go, throttled path
if !shouldWriteDB {
	// F32-1: usage counts every heartbeat, not only the DB-write cadence.
	s.recordWorkspaceUsage(ctx, relayID, receivedAt, req)
	...
```
`workspace_traffic.go`: the trust-domain lookup moved into `workspaceSamples`; `recordWorkspaceTraffic` (DB-write heartbeats: tracker + usage) and `recordWorkspaceUsage` (throttled heartbeats: usage only) share it. A failed lookup records nothing; the next heartbeat counts the span (cumulative counters).

**Tests:** `internal/relay/usage_every_heartbeat_test.go` — 3 heartbeats in one DB-write interval reach usage, the tracker sees one (mutation checks: removing the throttled call, and recording the tracker on throttled heartbeats, each fail it); a failed lookup on a throttled heartbeat records nothing. Live recheck: loss = the bytes since the last heartbeat (< 30 s).
