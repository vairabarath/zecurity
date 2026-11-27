---
type: phase
sprint: 24
owner: Barath (Person 1, M2)
features: [F23 active session timeseries, incl. per-tenant relay bandwidth]
phases: [R1, M1, M2]
decisions: [D-49, D-54, D-40]
status: signed off 2026-11-20; ready for implementation
---

# Sprint 24 — Barath: R1 per-workspace relay counters, M1 metric store, M2 charts (F23)

Read `Sprint24/path.md` first (order, conflict zones, rules). Binding decisions: Decision Record amendment **2026-11-20** — **D-49** (resolution, retention), **D-54** (per-tenant relay bandwidth). Also binding:
- **D-40 (extended):** per-tenant relay series are tenant-identifying → super-admin only; relay-ops see relay totals.
- **D-42:** relay bandwidth and headroom (B2): same units (bytes/s), same window and restart detection.
- **D-02:** single controller; the sampler is process-local.
- **D-04:** `internal/providerquery` doesn't import `net/http`.
- Proto changes are **additive only**.

Order: **R1 and M1 in parallel → M2** (after M1 is merged). Per-tenant relay series read "Not reported" until R1 is merged.

---

## R1 — Per-workspace relay counters (relay, proto, controller) — D-54

### What exists
- `proto/relay/v1/relay.proto` `HeartbeatRequest` fields 1–12; F5 cumulative counters `optional uint64 bytes_client_to_connector = 9;` / `bytes_connector_to_client = 10;`.
- `relay/src/session.rs`: global `ACTIVE_STREAMS`, `BYTES_CLIENT_TO_CONNECTOR`, `BYTES_CONNECTOR_TO_CLIENT`, counted as bytes flow (not when a bridge ends). Each bridged stream knows its workspace via `identity.trust_domain`.
- `relay/src/heartbeat.rs` builds the request (fields 9, 10 around line 204); tests capture what was sent (`SentBytes`).
- `controller/internal/relay/heartbeat.go`: `Heartbeat` (line ~69) and `bandwidthSample` (line ~243) turn the counters into rates; B2 tests in `bandwidth_test.go`.

### Build
- **Proto (additive):**
  ```proto
  message WorkspaceTraffic {
    string trust_domain = 1;
    uint64 bytes_client_to_connector = 2; // cumulative since relay start
    uint64 bytes_connector_to_client = 3; // cumulative since relay start
    uint32 active_streams = 4;            // open bridged streams now
  }
  // in HeartbeatRequest:
  repeated WorkspaceTraffic workspace_traffic = 13;
  ```
  `buf generate`; no other field changes.
- **Relay:** a per-trust-domain counter map next to the globals (e.g. `DashMap`/`Mutex<HashMap<String, Arc<WorkspaceCounters>>>` with atomics inside, so the hot path takes no lock after the first lookup per stream). The bridge holds an `Arc` to its workspace's counters for the stream's lifetime and adds to both the global and the per-workspace counters.
  - **Bounded:** one entry per workspace the relay serves. An entry with zero active streams whose workspace is no longer allowed (D-40 policy) is dropped; otherwise kept so cumulative counters never go backwards while the process lives.
  - The global counters must stay equal to the sum of the per-workspace ones (test).
  - The heartbeat sends every entry with traffic or open streams.
- **Controller:**
  - map `trust_domain` → workspace (existing workspace lookup); **unknown, deleted or revoked trust domains are ignored and logged at debug/warn, never stored**;
  - per-workspace rates over the same window as B2, with the same restart detection (a counter decrease or an uptime shorter than the gap → no rate for that window, never negative);
  - keep the latest per-workspace values in memory (D-02) for M1's sampler; an older relay (no field 13) → "not reported" for that relay.

### Tests
- Relay: two workspaces' streams counted separately; sum equals the globals; entry dropped only when idle and disallowed; heartbeat includes field 13.
- Controller: rate from two samples; restart → no rate; unknown trust domain ignored; relay without field 13 → not reported (not 0).
- **Mutation checks:** count a stream into the wrong workspace → the sum/split test fails; store an unknown trust domain → its test fails.

### R1 implementation record (branch `feat/sprint24-r1-workspace-traffic`)
- **D-54 refined (2026-11-21, decided by Barath):** proto3 can't tell a `repeated` field that wasn't sent from an empty one. Pre-production, every relay is rebuilt with R1, so **an empty field 13 = no tenant traffic (0)**; "Not reported" = the relay isn't reporting (silent, revoked, stale sample). No support flag; field 13 is the only proto change. Updated in the Decision Record, `path.md`, this file and `CLAUDE.md`.
- **Proto:** `message WorkspaceTraffic {trust_domain = 1; bytes_client_to_connector = 2; bytes_connector_to_client = 3; active_streams = 4}` and `repeated WorkspaceTraffic workspace_traffic = 13` in `HeartbeatRequest`. `buf generate`: only internal message indexes shifted in `relay.pb.go`; no field renumbered.
- **Relay:**
  - `relay/src/traffic.rs` (new): `TRAFFIC`, a `DashMap` from trust domain to `Arc<WorkspaceCounters>` (atomics; the bridge takes no lock after opening its stream).
  - `open_stream(trust_domain, workspace_id)` counts the stream **while holding the entry's map lock**, so `prune` either sees the open stream or ran before the entry existed: a bridge never counts into an entry that already left the table.
  - `session.rs`: `handle_lookup_stream` opens the guard next to `ACTIVE_STREAMS` (same span); `counted_copy` adds each chunk to the per-bridge count, the process-wide F5 counter **and** the workspace counter. `handle_connection` → `spawn_lookup` → `handle_lookup_stream` take the `workspace_id` the listener already derives for the revocation check and the D-40 policy.
  - `heartbeat.rs`: sends `TRAFFIC.snapshot()` (entries with traffic or open streams), then, after applying the policy, `prune`s idle entries whose workspace the policy refuses, against a **copy** of the policy (`WorkspacePolicy::snapshot`, no longer test-only; new `workspace_policy::allows`), so the policy lock is never taken inside the table's locks.
- **Controller:**
  - `internal/relay/workspace_traffic.go` (new): `WorkspaceTrafficTracker` (in memory, D-02) with `Record` and `Snapshot`. Rates on the **heartbeat DB-write cadence** (the B2 window), with B2's restart rules: a counter that went down or an uptime shorter than the gap (5 s slack) → no rate for that window; a workspace's first sample has no rate; a late heartbeat changes nothing. `ActiveStreams` is always the latest value.
  - `RelayWorkspaceTraffic {RelayID, MeasuredAt, WindowSeconds, Workspaces map[workspace id]WorkspaceRate}`; a workspace missing from a relay's view had no traffic there. **M1 must treat an old `MeasuredAt` as not reported.**
  - `Store.ActiveWorkspacesByTrustDomain`: `status = 'active'` only (a suspended tenant keeps `'active'`, D-45, and is included); unknown, deleted and inactive trust domains are absent.
  - `Service.recordWorkspaceTraffic`, called after a successful `RecordHeartbeat` (same cadence as B2): unknown trust domains are **logged and dropped, never stored**; a lookup failure records nothing for that window. Off unless `WithWorkspaceTraffic` is set; `main.go` wires `relayWorkspaceTraffic` for M1's sampler.
- **Tests:**
  - Relay: `traffic.rs` (4: separation, idle not reported, prune only idle and disallowed, lower-cased ids); `session.rs` `bridged_bytes_count_for_the_workspace_and_the_total` (exact per direction, open count, cumulative after close) and `lookup_bridges_count_for_their_own_workspace` (two workspaces through the real Register + Lookup handshakes); `heartbeat.rs` asserts the request carries field 13.
  - Controller: `workspace_traffic_test.go` (rates from two samples, restart by uptime and by counter decrease, slack, new workspace, late sample, idle relay, snapshot copy, heartbeat mapping with an unknown trust domain, empty field 13, lookup failure, off by default); `workspace_traffic_db_test.go` (lookup: active and suspended in, deleted and unknown out).
  - **Mutation checks, all caught (8):** client bytes into the wrong direction; a stream counted into another workspace; workspace counter never incremented; prune dropping an entry with an open stream; an unknown trust domain stored; restart by short uptime ignored; counter decrease ignored; inactive workspace returned by the lookup.
- **Gates (2026-11-21):** relay `cargo build`, `cargo test` (**151 passed**), `cargo clippy` (no warning in the R1 files); controller `go build ./...` and `go vet ./...` clean, full `go test ./...` with every DB env set **2,022 passed, 0 failed** (2 known skips: `TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed`, `TestEnroll_CSRSignatureInvalid`).
- **Live check:** pending (two tenants downloading through one relay at once; per-tenant rates sum to the relay total; a stopped relay reads "Not reported").

---

## M1 — Metric store and read API — D-49

### What exists
- Relay heartbeat values kept by the controller: `connection_count`, `max_connections`, B2 bandwidth rates (migrations 023, 063; `internal/relay`).
- Client device sessions: `internal/devicesession` (`session_idle_until`, `session_max_until`; a device with both in the future has a live session).
- Connectors and shields online: existing status/heartbeat columns (as used by the tenant detail counts, `internal/providerquery/tenants.go`).
- Prometheus gauges (PENDING-10) are live only; not a store.

### Build
- **Schema `068_metric_samples.sql`:**
  - `metric_samples (series TEXT, subject_type TEXT, subject_id UUID NULL, ts TIMESTAMPTZ, value DOUBLE PRECISION, PRIMARY KEY (series, subject_type, subject_id, ts))` for 1-minute samples, and `metric_rollups` with the same key at hourly `ts` plus `min`, `max`, `avg`, `count`. (Or one table with a `resolution` column; pick one and document it.) Index for `(series, subject_id, ts DESC)`.
  - `subject_type`: `relay`, `tenant`, `fleet`; `subject_id` null for fleet.
  - App role: insert/select/delete on these tables only.
- **Series:**
  - relay: `relay.connections`, `relay.bytes_to_clients_per_sec`, `relay.bytes_to_connectors_per_sec`;
  - tenant: `tenant.active_sessions`, `tenant.connectors_online`, `tenant.shields_online`, and from R1 `tenant.relay_bytes_to_clients_per_sec`, `tenant.relay_bytes_to_connectors_per_sec`, `tenant.relay_streams` (summed over relays);
  - fleet: totals of the above.
- **Job** (in `main.go`, own block): every minute writes one sample per series and subject from current values; **no sample when the value isn't known** (relay silent, not reported) → a gap, never 0. Hourly: roll up the previous hour; prune 1-minute samples older than 7 days and roll-ups older than 400 days (both configurable for tests, defaults from D-49).
- **Shared relay reader:** the read of current relay connections/bandwidth/headroom is one function used by M1 and by A1's capacity rule (agree with Sathiya; whoever merges first owns it).
- **Read API:** `GET /provider/metrics/{series}?subject=<id>&from=&to=&step=` (`step` = `1m` or `1h`; server picks the resolution when omitted, caps points per response).
  - Action `metrics.read`: relay and fleet relay series → relay-ops and super-admin; tenant series (incl. per-tenant relay) → super-admin only (D-40 extended). Server-side in `decide()`.
  - Query service in `internal/providerquery` (no `net/http`); returns points with gaps as missing points; never returns secrets.
  - Unknown series → 404; bad range → 400.

### Tests
- Sampler writes the expected rows; unknown value → no row; roll-up min/max/avg correct; retention prunes at the boundary.
- API: authz per role (relay-ops on a tenant series → 403); gaps preserved; step and caps.
- DB-backed tests run (no skip).
- **Mutation checks:** write 0 for an unknown value → the gap test fails; allow relay-ops tenant series → the authz test fails.

### M1 implementation record (branch `feat/sprint24-m1-metric-store`)
- **Choices agreed with Barath (2026-11-21):**
  1. **Two actions:** `relay.metrics.read` (relay-ops and super-admin) and `tenant.metrics.read` (super-admin only), so `decide()`'s prefix rules settle it, instead of one `metrics.read` with per-series logic.
  2. **Fleet totals** are the sum over the relays that report; a gap only when none does (not a gap whenever any relay is silent).
  3. **Two tables**, `metric_samples` (1 minute) and `metric_rollups` (hourly, with min / max / avg / count), rather than one with a resolution column.
- **Schema `068_metric_samples.sql`:**
  - primary key `(series, subject_id, ts)`; `subject_type` is the series' prefix (CHECK); fleet rows use the nil UUID (primary key columns can't be NULL; CHECK ties it to `fleet`);
  - samples on the minute and roll-ups on the hour, checked on the **epoch** (not `date_trunc`, which follows the session time zone: in Asia/Kolkata an hour starts at :30 UTC; tested);
  - values finite (`<> 'NaN'`: Postgres treats NaN as equal to NaN); roll-ups `min <= avg <= max` (the roll-up clamps the mean, because a float mean can round past its max: avg(0.1, 0.1, 0.1) = 0.10000000000000002; tested);
  - `ts` indexes for retention; app role: samples SELECT/INSERT/DELETE, roll-ups also UPDATE (upsert); no foreign keys.
- **Series (15)** in `providerquery/metrics.go` (registry with subject, unit and whether it is tenant data):
  - relay: `relay.connections`, `relay.bytes_to_clients_per_sec`, `relay.bytes_to_connectors_per_sec`;
  - tenant: `tenant.active_sessions`, `tenant.connectors_online`, `tenant.shields_online`, `tenant.relay_bytes_to_clients_per_sec`, `tenant.relay_bytes_to_connectors_per_sec`, `tenant.relay_streams`;
  - fleet: `fleet.connections`, `fleet.bytes_to_clients_per_sec`, `fleet.bytes_to_connectors_per_sec` (relay data: relay-ops too), `fleet.active_sessions`, `fleet.connectors_online`, `fleet.shields_online` (tenant data: super-admin only).
- **Sampler `internal/metricstore`** (writes; `providerquery` stays read-only):
  - every minute (`Run`/`Tick`), timestamps on the minute, `ON CONFLICT DO NOTHING` (a restart in the same minute keeps the first sample);
  - relays from the shared reader `providerquery.RelayLoads` (as A1): only `status = 'active'` relays are sampled (inactive = offline = gap); a rate is written only when known; a truncated relay list gives no fleet relay totals;
  - tenants (`workspaces.status = 'active'`, so suspended tenants included): live sessions = devices not revoked with `session_idle_until` and `session_max_until` in the future; connectors and shields with `status = 'active'`;
  - per-tenant relay series from R1's tracker: only views of **active** relays no older than `TrafficMaxAge` (main.go: 2 × `RELAY_HEARTBEAT_DB_WRITE_INTERVAL` + 1 min); no fresh view → no tenant relay sample (not reported); a workspace missing from a fresh view = 0 there (D-54 refined); a rate unknown on a fresh view (restart window) → no byte sample for that tenant (streams still written);
  - each new hour: roll up the previous **two** hours (upsert, so a restart near the boundary still covers the hour) and prune (`METRIC_SAMPLE_RETENTION`, `METRIC_ROLLUP_RETENTION`, defaults 7 d / 400 d); a failed step is retried next tick; `Collect` returns what it read with the first error.
- **Read API** `GET /provider/metrics/{series}?subject=&from=&to=&step=` (`provider/metrics_read.go`, query in `providerquery`, no `net/http`):
  - unknown series → 404 (before authorization: it reveals nothing); authorization per series (`CanReadMetricSeries`); `from`/`to` RFC 3339 (default the last 24 h);
  - step `1m` or `1h`, default `1m` up to 24 h, `1h` beyond; codes `invalid_subject` (relay/tenant need a UUID, fleet takes none), `invalid_range` (from ≥ to, beyond 400 days, or `1m` before the 7-day retention), `invalid_step`, `too_many_points` (> 2,000);
  - response `{series, subject (null for fleet), unit, step, from, to, points[]}`; hourly points add `min`, `max`, `count`; **a missing point is a gap**. Not audited (aggregates, like the relay list).
- **Tests:** `metricstore/sampler_test.go` (6: active-only and known-rate relay samples, fleet gap and truncation, tenant counts, R1 sums over fresh active relays, not reported with no fresh view, every registered series is written with its subject); `metricstore/sampler_db_test.go` (6, real DB: write on the minute and idempotent, schema refusals incl. NaN and an offset time zone, roll-up min/max/avg/count and re-run, mean clamp, retention boundary, `Collect` from real relay/tenant/device/agent rows); `provider/metrics_read_test.go` (4: role matrix over all subject kinds incl. tenant JWT 401, minute points with a gap, hourly roll-ups and auto step, every refusal); `authz_test.go` `TestDecide_MetricsMatrix`.
- **Mutation checks, all caught (12):** unknown relay rate written as 0; offline relay sampled; stale R1 view used; partial tenant sum with an unknown rate; tenant relay 0 with no reporting relay; fleet gap written as 0; roll-up mean not clamped; same-minute rewrite replacing the sample; expired sessions counted; relay-ops allowed tenant series; 1-minute read past 7 days; fleet sessions readable by relay-ops.
- **Open question (not decided here):** per-tenant series of a **purged** tenant (S4, D-46) stay until the retention prunes them (≤ 400 days). They hold only counts and rates keyed by the tenant id, not tenant content, but D-46 destroys tenant data. Deleting them in the purge would touch S4's purge functions: for Barath to decide.
- **Gates (2026-11-21):** controller `go build ./...` and `go vet ./...` clean; full `go test ./...` with every DB env set **2,039 passed, 0 failed** (2 known skips: `TestUpdateScimConfig_CanonicalKeyMismatchFailsClosed`, `TestEnroll_CSRSignatureInvalid`). No console change.
- **Live check:** pending (stored samples match live values; a roll-up after an hour; retention with a test setting; relay-ops 403 on tenant series).

---

## M2 — Console charts

### Build
- A small chart component (no heavy library unless already in the console; check `provider-console/package.json`), ranges **1 h / 24 h / 7 d / 30 d**, gaps drawn as gaps, units as in B2 (bytes/s formatting reused).
- **Relay detail:** connections; bandwidth to clients / to connectors, with link capacity (B2) as a reference line when configured.
- **Tenant detail** (super-admin): active sessions, connectors and shields online, relay bandwidth (R1) — "Not reported" when no relay is reporting (a gap), 0 when reporting relays carried none of its traffic.
- **Overview** (`provider-console/src/pages/Home.tsx`): fleet sessions sparkline (own section; A3 adds the alerts badge).
- Read-only: no new console writes; `api-surface.test.ts` unchanged except new GETs if the test lists reads.

### Tests
- `npm test`, `npm run lint`, `npm run build`; chart renders gaps; relay-ops view hides tenant charts.

### M2 implementation record (branch `feat/sprint24-m2-console-charts`)
- **Choices agreed with Barath (2026-11-21):**
  1. **Hand-written SVG chart, no library** (the console has none; ~0 bundle cost; gaps under our control).
  2. **Overview trend per role:** super-admins see **fleet sessions** (tenant data); relay-ops see **fleet relay connections** (relay data they may read), so relay-ops never get a 403 tile.
  3. **One range picker per page** (1 h / 24 h / 7 d / 30 d): every chart on the page follows it.
- **Refresh (correction to the first proposal):** charts are read once when they appear, again on a range change and on the page's Refresh, **no auto-refresh**: the console's no-polling rule (every page reads once per visit). Metric reads aren't audited, so changing the range on tenant detail writes no `tenant.read` entry (tested). The range lives in the page's top-level component, so a Refresh (which remounts the view) keeps it.
- **Code:**
  - `api/metrics.ts` (`getMetricSeries`, allowlisted query keys); `lib/metrics.ts` (ranges: 1 h and 24 h read 1-minute points, 7 d and 30 d hourly; `segments` breaks a line wherever a step is missing; `nearestIndex`, `summarize`, `niceMax`, `formatMetric` (B2 rate formatting), `sumSeries`); `lib/useMetricSeries.ts` (on `useDetailQuery`: StrictMode-safe, one request per key).
  - `components/MetricChart.tsx`: `MetricChart` (lines per series, gaps as breaks, a single point as a dot, hourly min–max band, dashed ceiling, hover tooltip with time and value or "no data", screen-reader summary with latest / low / high, skeleton while loading, "Not available for your role" / "Couldn't load" / an empty text instead of a zero line), `RangePicker` (`aria-pressed`), `Sparkline`.
  - `components/RelayHistory.tsx` on relay detail (not for a pending relay): connections; bandwidth to clients and to connectors. With a link capacity (B2), a **"Both directions"** line (the sum, only for minutes where both directions have a point) is drawn against the dashed **link capacity** ceiling: both flows share the link (D-42), so the capacity bounds their sum, never one direction.
  - `components/TenantHistory.tsx` on tenant detail: active sessions; connectors and shields online; relay traffic (R1), "Not reported: no relay reported this tenant's traffic in this range" when empty.
  - `pages/Home.tsx`: the trend tile next to the firing-alerts tile; its series is read in `Home` (like the other tiles), because `FleetTiles` remounts its children when the relay list arrives, which sent a second request when the tile read it itself (caught by the request pins).
  - `api-surface.test.ts`: `GET /provider/metrics/{series}` added to the reads on purpose; no write.
- **Tests:** `lib/metrics.test.ts` (gaps, hourly step with min/max, unparseable points, range windows, helpers, sums), `components/MetricChart.test.tsx` (segments per run, screen-reader summary, band, ceiling, empty, refusal and failure texts, skeleton, sparkline), `components/History.test.tsx` (relay series with subject and step, 1 h → 7 d switches to hourly, both directions and ceiling only with a capacity and without an extra request; tenant series, a range change never reads `/provider/tenants`, "Not reported"). Pins updated on purpose: relay and tenant detail button lists include the four range buttons; the Overview request pins include the trend read, per role (relay-ops never asks for fleet sessions).
- **Mutation checks, all caught (8):** a gap drawn as a connected line; a missing direction summed as 0; relay-ops asking for fleet sessions; the capacity drawn against one direction; the hourly band dropped; the 24 h range read hourly; an empty range drawn as a chart instead of "Not reported"; the trend tile never reading its series.
- **Gates (2026-11-21):** provider console `npm test` **703 passed** (51 files), `npm run lint` clean, `npm run build` clean. No controller, schema or admin change.
- **Live check:** pending, together with R1 and M1 (charts on relay detail, tenant detail and Overview for each range; a stopped relay shows a break; relay-ops see no tenant charts).
- **Review fixes (Sathiya's review of #240, 2026-11-21):**
  1. **"Both directions" and the link-capacity ceiling on 1-minute ranges only** (`showsLinkCeiling`): an hourly point is the mean of the minutes that had a sample, so a sum of hourly means hides the minutes the link was full and showed headroom that wasn't there. On 7 d / 30 d a note says capacity is compared on 1 h and 24 h.
  2. **Scale to the data:** the ceiling is drawn to scale only while the data reaches a quarter of it (`CEILING_IN_SCALE`); below that it is labelled "(above this scale)", so small traffic on a large link never reads as a zero line.
  3. **Overview trend tile labelled as a trend** ("Relay connections, 24 h" / "Active sessions, 24 h") with the time of its latest point: it is a stored sample, apart from the live relay-connections tile.
  4. **The trend is not read until the role is known** (`useMetricSeries(null, …)` stays loading): one read per visit, for the right series.
  5. **The hover tooltip is not a live region** (`aria-hidden`, no `role="status"`); the figcaption carries the summary.
  6. **24 h axis ends show the day** (both ends no longer read the same clock time).
  7. **One window per page** (`useMetricWindow(range, round)`): every chart on a page reads the same `from`/`to`; the page's Refresh counter stays in the query key, so a Refresh within the same second still reads once more.
  - Tests added for each; **mutation checks, all caught (9):** ceiling on hourly ranges, ceiling always in scale, tooltip as a live region, Refresh counter dropped from the key, 24 h labels without the day, per-series windows, old tile label, sparkline without its time, trend read before the role is known. Gates: `npm test` **716 passed** (after merging main), lint, typecheck and build clean.
  - **Second review (#242, 2026-11-21):** the Sparkline took its headline and time from different lists and could crash on points with a value but an unparseable time (now: the latest drawable point for both, else "No data yet"); the Overview trend window is taken when the role is known, not at mount; the Sparkline gets the plain series name (", 24 h" stays on the tile label); `useMetricWindow` holds the window in state, not a memo (React may drop a memo, which would re-send every read). Tests added; 5 mutation checks caught. `npm test` **720 passed**, lint, typecheck and build clean.

---

## Live check (each phase)

Record in `.zecurity-obs/Planning/Pending-Live-Tests.md`:
- **R1:** two tenants' clients download through one relay at once; per-tenant rates sum to the relay total (B2) and match each client's measured rate; a stopped relay reads "Not reported" (a gap), an idle one 0.
- **M1:** stored samples match the live values; roll-up after an hour (or test interval); retention pruning with a test setting; relay-ops 403 on tenant series.
- **M2:** charts on relay detail, tenant detail and Overview for each range.

## Post-Phase Fixes

F23 live run 2026-11-23 (`main` `2ae68355`) found R1-1, R1-2, R1-3 and M1-1 (`Planning/Pending-Live-Tests.md`, "F23 findings"). Branch `fix/sprint24-f23-findings`. The design was agreed with Barath on 2026-11-23/24 (option D: no proto field change, no controller-side counter cache).

### Fix: R1-1 — a tenant's first window had no rate
**Issue:** a workspace absent from a relay's previous reading got no rate, so its first minute of traffic was missing.

**Root cause:** `Record` only rated a workspace that had a previous counter.

**Fix applied** (`internal/relay/workspace_traffic.go`, `Record`):
1. In a continuous window, a workspace absent from the previous reading rates **from zero**. This is safe because the relay reports every entry that ever carried bytes (`relay/src/traffic.rs` snapshot tests the cumulative counters), so an absent entry is a zero counter.
2. A window that isn't continuous gives no rate for **any** workspace on that relay (first reading, controller restart, relay restart).
3. A counter that went down gives no rate (a reset or a re-created entry).
4. No zero baseline if one of the workspace's trust domains was, in the previous reading, **unmatched or another workspace's**. Such a domain's counter may be long-lived, for example when a purged tenant's slug is reused. Details:
   - the per-domain map (trust domain → workspace id, `""` when unmatched) is kept in memory for one reading and never stored;
   - a skipped reading (lookup failure) keeps the previous map.
5. A window longer than `maxGap` (= `TrafficMaxAge`) has no rate. Every reading, rated or not, is the baseline for the next one.

**Known limitation (tested, documented):** an entry pruned and re-created within one window, whose new counter is above the old one, is undercounted (`TestWorkspaceTraffic_RecreatedEntryLargerThanBefore_KnownUndercount`). The exact fix is an entry epoch in `WorkspaceTraffic` (additive field), deferred.

### Fix: combined relay restart check
**Issue:** the old check `uptime + 5 ≥ gap` missed a relay that restarted within 5 s after a reading. A young relay (previous uptime below the gap) could slip past too.

**Fix applied:** restart if `U_now < U_prev + gap − 5 s` (`restarted()`).
- The uptime is the relay's `Instant` (monotonic), in whole seconds.
- 5 s covers delivery-delay differences and the rounding.
- A false positive costs one blank window, never a wrong number.

### Fix: the window uses the monotonic clock
**Issue:** `receivedAt := time.Now().UTC()` strips Go's monotonic reading. A backward wall-clock step on the controller shortened the gap and inflated rates (a 30 s step in a 60 s window doubled them).

**Fix applied** (`internal/relay/heartbeat.go`): the tracker gets `arrived := time.Now()`; the database still gets `arrived.UTC()`. The B2 relay totals still use database timestamps, which can't carry a monotonic reading (known; out of scope).

### Fix: R1-2 — partial tenant sums after a relay restart
**Issue:** a fresh view without a window (restart) dropped only the tenants it listed with an unknown rate. Others summed over the remaining relays, a partial total shown as real.

**Fix applied** (`internal/metricstore/sampler.go`, `tenantRelaySamples`):
- any used view with `WindowSeconds == 0` → no byte samples for **any** tenant that minute;
- stream counts are still written;
- the next continuous window recovers.

### Fix: R1-3 B — a silent relay's last rate repeated
**Issue:** rates are averages over the DB-write window. When a relay stopped, its last rate was written every minute until eviction (up to 150 s).

**Fix applied:**
- the relay byte series, fleet byte totals and the relay's tenant shares are used only while the relay is **live**: last heartbeat within `RelayLiveMaxAge` = 75 s;
- the heartbeat time is the Valkey liveness key, else the persisted heartbeat (`providerquery.EffectiveHeartbeat`, the console's definition);
- connections still follow the relay status.

R1-3 C (the console label) is in PR B.

### Fix: rule 6 — relays left out of the tenant sum
**Issue:** a relay left out of the sum (evicted, silent, or without a fresh reading) counted as 0 for its tenants. That is an undercount whenever traffic still flows, for example:
- connector streams draining (≤ 120 s) after the relay was evicted;
- a partition: relay alive but cut off from the controller, connectors still attached, which the F10-1 migration retry allows when no other relay exists.

**Fix applied (`tenantRelaySamples`):**
- **6a:** the tenant's bytes are a gap when either:
  - (i) it was carrying traffic (streams, bytes or an unknown rate) in the left-out relay's reading of the last `max(TrafficMaxAge, ResidualWindow)`;
  - or (ii) one of its unrevoked connectors confirmed a placement on that relay within `PlacementMaxAge` (60 s; `connector_relay_placement.last_confirmed`, refreshed by every 15 s health report). A connector that stops confirming has lost the controller and fails closed 15 s after its last authorization acknowledgement, so it can't be carrying traffic.
- **6b:** an active, **live** relay without a fresh view can't say which tenants it carries → no tenant relay sample at all that minute.
- A failed placement read → no tenant bytes that minute.

### Fix: M1-1 — a restart re-roll thinned an hour's roll-up
**Issue:** on start the sampler re-rolls the previous two hours. After a short retention had pruned part of that hour, the re-run overwrote the full roll-up (live run: count 54 → 2).

**Fix applied** (`Rollup`):
- `ON CONFLICT … DO UPDATE … WHERE EXCLUDED.count >= metric_rollups.count`;
- a re-run that covers fewer minutes keeps the fuller row;
- the `Tick` comment ("re-running is harmless") is corrected.

### R1 freshness thresholds (one table)
`W` = `RELAY_HEARTBEAT_DB_WRITE_INTERVAL` (default 5 min, lab 1 min). `TrafficMaxAge` = `2W + 1 min`, one value in `main.go` for both the tracker and the sampler.

| Threshold | Default (lab) | Where | Blanks |
|---|---|---|---|
| Relay heartbeat | 30 s | relay | — |
| Reading cadence | `W` = 5 min (1 min) | heartbeat DB write | — |
| Restart tolerance | 5 s | tracker `restartSlackSeconds` | that reading's rates, every tenant on the relay |
| Rule 5 max gap | `TrafficMaxAge` = 11 min (3 min) | tracker `maxGap` | that reading's rates, every tenant on the relay (still the next baseline) |
| Fresh view | `TrafficMaxAge` | sampler | the view isn't used (→ 6a / 6b) |
| R1-3 B live | 75 s | sampler `RelayLiveMaxAge` | relay byte series and fleet byte totals; the relay is left out of tenant sums (→ 6a) |
| Eviction | 90 s expiry, 60 s sweep (inactive after 90–150 s) | `relay/expiry.go` | relay `inactive`: no connections sample; dropped from connectors' relay lists |
| 6a reading cutoff | `max(TrafficMaxAge, 330 s)` = 11 min (5.5 min) | sampler `ResidualWindow` | bytes of tenants carrying traffic in a left-out relay's last reading |
| 6a placement | 60 s | sampler `PlacementMaxAge` | bytes of tenants with a connector confirming a placement on a left-out relay |
| 6b | live relay, view missing or older than `TrafficMaxAge` | sampler | every tenant relay sample that minute (bytes and streams) |
| Connector drain | 120 s | connector | input to `ResidualWindow` |
| Connector fail-closed | 15 s after the last ack; health every 15 s | connector | input to `PlacementMaxAge` |

**No conflicts (checked):**
- **Live (75 s) < eviction (90 s):** a silent relay stops contributing rates before it is evicted. Between 75 s and eviction it is active but not live, so 6a applies, not 6b (6b needs a live relay).
- **Rule 5 and freshness share `TrafficMaxAge`:** a gap is rejected exactly when the previous reading would no longer be fresh. Normal gaps are `W` + ≤ 30 s (30 s with Valkey down), always below `2W + 1 min`.
- **6a cutoff ≥ eviction worst case (150 s) + drain (120 s) + 60 s, for any `W`:** with lab `W` = 1 min, `TrafficMaxAge` (3 min) alone would end too early. That's why the cutoff is `max(TrafficMaxAge, 330 s)`.
- **Placement 60 s = four health reports**, well above the 15 s fail-closed bound.
- **A live relay with a stale view** marks 6a residuals and also hits 6b; 6b (nothing written) wins.

### Tests
- **Tracker:**
  - `TestWorkspaceTraffic_NewWorkspaceInContinuousWindowRatesFromZero`
  - `…_RelayRestartBlanksAllWorkspaces`
  - `…_ControllerFirstSampleHasNoRate`
  - `…_UnmappedDomainOnlyBlocksItsOwnWorkspace`
  - `…_UnmatchedForSeveralReadingsThenMatched`
  - `…_DisappearAndReappear`
  - `…_RecreatedEntryLargerThanBefore_KnownUndercount`
  - `…_IndependentRelays`
  - `…_JitterWithinToleranceStillRates` (replaces `UptimeWithinSlackStillRates`, whose case is now a restart)
  - `…_RestartJustAfterAReadingIsDetected`
  - `…_GapLongerThanMaxGapHasNoRate`
  - `…_SkippedReadingWithinMaxGapRatesOverLongerWindow`
  - `…_DomainThatChangedWorkspaceGetsNoZeroBaseline`
- **Heartbeat:**
  - `TestHeartbeat_UnmatchedDomainSurvivesASkippedReading`
  - `TestHeartbeat_TrafficTimeKeepsTheMonotonicClock`
- **Sampler:**
  - `TestTenantRelaySamples_NonContinuousViewWritesNoBytes`
  - `…_LeftOutRelayWithRecentReadingBlanksItsTenants`
  - `…_ResidualReadingPastCutoffCountsZero`
  - `…_PlacementOnLeftOutRelayBlanksTenant`
  - `…_LiveRelayWithoutFreshReadingWritesNothing`
  - `…_PlacementsUnknownWritesNoBytes`
  - `TestRelaySamples_SilentRelayHasNoByteSamples`
  - `TestRelayLive_ValkeyThenPersistedHeartbeat`
  - **Changed:** `TestTenantRelaySamples_SumOverFreshActiveRelays`'s offline relay is now 20 min old. A recent one blanks the tenant (6a).
- **Database:**
  - `TestRecentPlacements_ConfirmedAndUnrevokedOnly`
  - `TestRollup_RerunAfterPruneKeepsTheFullerRollup`
- **Relay:**
  - `idle_entry_with_bytes_is_still_reported`, the invariant R1-1 relies on;
  - `prune_then_reopen_restarts_at_zero`.
- **Proto:** the comment on `WorkspaceTraffic` fields 2–3 is corrected (cumulative since the entry was created, not since relay start). There is no field change.
