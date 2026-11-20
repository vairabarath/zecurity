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

---

## M2 — Console charts

### Build
- A small chart component (no heavy library unless already in the console; check `provider-console/package.json`), ranges **1 h / 24 h / 7 d / 30 d**, gaps drawn as gaps, units as in B2 (bytes/s formatting reused).
- **Relay detail:** connections; bandwidth to clients / to connectors, with link capacity (B2) as a reference line when configured.
- **Tenant detail** (super-admin): active sessions, connectors and shields online, relay bandwidth (R1) — "Not reported" when no data.
- **Overview** (`provider-console/src/pages/Home.tsx`): fleet sessions sparkline (own section; A3 adds the alerts badge).
- Read-only: no new console writes; `api-surface.test.ts` unchanged except new GETs if the test lists reads.

### Tests
- `npm test`, `npm run lint`, `npm run build`; chart renders gaps; relay-ops view hides tenant charts.

---

## Live check (each phase)

Record in `.zecurity-obs/Planning/Pending-Live-Tests.md`:
- **R1:** two tenants' clients download through one relay at once; per-tenant rates sum to the relay total (B2) and match each client's measured rate; an older relay build reads "Not reported".
- **M1:** stored samples match the live values; roll-up after an hour (or test interval); retention pruning with a test setting; relay-ops 403 on tenant series.
- **M2:** charts on relay detail, tenant detail and Overview for each range.

## Post-Phase Fixes

_None yet._
