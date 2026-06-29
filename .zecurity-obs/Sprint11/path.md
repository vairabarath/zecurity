---
type: planning
status: planned
sprint: 11
tags:
  - sprint11
  - relay
  - connector
  - controller
  - adr-016
  - transport-control-plane
---

# Sprint 11 — ADR-016: Tiered Relay Selection & Background Optimization

> **Read this before writing a single line of code.**
> This file is the source of truth for execution order.

---

## Sprint Goal

Implement ADR-016: the controller labels active relays by capacity tier and pushes the eligible list to connectors. Connectors connect instantly to a random Tier 1 relay, then silently probe and migrate to the best relay in the background with make-before-break.

This sprint is the Track B control plane. It does **not** change the data plane (QUIC tunnel, inner mTLS). It changes **how a connector knows which relay to use** — replacing static `RELAY_ADDR` env var with dynamic, controller-pushed relay assignment.

```text
Before (Track A):
  Controller → ACL snapshot → client knows relay addr per connector
  Connector → static RELAY_ADDR env var

After (Track B / this sprint):
  Relay → heartbeat with connection_count + max_connections → Controller
  Controller → capacity label (High/Medium/Exhausted) → LabelledRelayList
  Controller → push LabelledRelayList → Connector control stream
  Connector → random Tier 1 pick → instant register → report ConnectorRelayState
  Connector → background probe → score → make-before-break migrate if better
  Controller → UpsertPlacement → NotifyPolicyChange → ACL recompile
  Client → 15s poll → GetACLSnapshot → new relay_addr per connector
```

---

## Key Design Decisions (from ADR-016)

| Decision | Detail |
|---|---|
| Relay eligibility | Platform-level in v1 — all active relays serve all workspaces; workspace isolation via mTLS/SPIFFE |
| Tier 1 (High) | `fill_ratio < 45%` enter, `>= 50%` exit — eligible for instant startup + optimization |
| Tier 2 (Medium) | `fill_ratio < 75%` enter, `>= 80%` exit — optimization only; startup fallback if no Tier 1 |
| Exhausted | `>= 80%` — dropped from `LabelledRelayList`; existing connectors may stay until migration |
| `connection_count` | Active bridged client relay streams only (not registered connectors) |
| Hysteresis | Candidate label must be stable for `RELAY_LABEL_HOLDDOWN_SECS` (60s) before push |
| Startup | Random Tier 1 pick — no probe delay; spreads 1,000 connectors without storms |
| Background probe | Parallel QUIC probes, max `RELAY_MAX_CONCURRENT_PROBES` (5), score = RTT + fill penalty |
| Migration trigger | `current_score - best_score > max(current_score × 0.15, 10ms)` — both conditions required |
| Exhausted active relay | Immediate migration regardless of score threshold |
| Make-before-break | New relay registered before old drained; drain timeout = `RELAY_DRAIN_TIMEOUT_SECS` (30s) |
| Persisted ranking | Top 5 relays written atomically to state dir after each probe cycle |
| Failover | Validate ranking against current `LabelledRelayList` before attempting; skip absent/exhausted |
| Probe security | Validate `request_id` echo + QUIC peer SPIFFE matches `LabelledRelayInfo.spiffe_id` |
| Backoff | Exponential: `RELAY_RECONNECT_BASE_SECS=5`, `RELAY_RECONNECT_MAX_SECS=120`, factor 2.0 |
| ADR-015 alignment | Controller owns eligibility; connector selects within approved pool; controller records outcome |

---

## Dependency Graph

```text
Phase A — Proto changes (M2)
  ↓
Phase B — Relay telemetry: connection_count reporting (M4)
  ↓
Phase C — Controller: label state machine + LabelledRelayList push (M2)
  ↓
Phase D — Connector: probe, ranking, selector, make-before-break (M3)
  ↓
Phase E — Integration & end-to-end validation (M2 + M3)
```

---

## Execution Path

### Phase A — M2: Proto Changes

> See [[Sprint11/Member2-Go/Phase1-Proto]].

- [ ] **M2-A1** `proto/relay/v1/relay.proto` — add `connection_count` and `max_connections` to relay heartbeat
- [ ] **M2-A2** `proto/relay/v1/relay.proto` — add `ProbeRequest` (with `request_id`) and `ProbeResponse` (echoing `request_id`)
- [ ] **M2-A3** `proto/connector/v1/connector.proto` — add `RelayCapacityLabel` enum, `LabelledRelayInfo`, `LabelledRelayList` messages
- [ ] **M2-A4** `proto/connector/v1/connector.proto` — add `relay_list = 17` to `ConnectorControlMessage` oneof body
- [ ] **M2-A5** `buf generate` — regenerate Go stubs; confirm Rust prost stubs regenerate
- [ ] **Build gate:** `cd controller && go build ./...`

### Phase B — M4: Relay Telemetry

> Depends on Phase A. See [[Sprint11/Member4-Relay/Phase1-Telemetry]].

- [ ] **M4-B1** `relay/src/config.rs` — add `RELAY_MAX_CONNECTIONS` env var
- [ ] **M4-B2** `relay/src/session.rs` — add atomic counter for active bridged client streams; increment on bridge start, decrement on bridge end
- [ ] **M4-B3** `relay/src/heartbeat.rs` (or equivalent) — include `connection_count` and `max_connections` in heartbeat payload
- [ ] **M4-B4** `relay/src/session.rs` — add probe responder: detect `ProbeRequest` on new connection, respond with `ProbeResponse { connection_count, capacity, request_id }`, close without registering
- [ ] **M4-B5** Relay-side probe abuse controls: per-connector rate limit, per-probe timeout, max concurrent probe cap, audit log on excessive attempts
- [ ] **Build gate:** `cd relay && cargo build`

### Phase C — M2: Controller Label State Machine & List Push

> Depends on Phase A + B. See [[Sprint11/Member2-Go/Phase2-Label-StateMachine]].

- [ ] **M2-C1** DB migration — add columns to `relays`: `connection_count int`, `max_connections int`, `capacity_label text`, `pending_capacity_label text`, `pending_label_since timestamptz`, `last_label_changed_at timestamptz`
- [ ] **M2-C2** `controller/internal/relay/heartbeat.go` — persist `connection_count` and `max_connections` from heartbeat payload
- [ ] **M2-C3** `controller/internal/relay/heartbeat.go` — implement hysteresis state machine: compute `candidate_label`, manage pending/promotion fields, push `LabelledRelayList` only on promotion after hold-down elapsed
- [ ] **M2-C4** `controller/internal/connector/control_stream.go` — push current `LabelledRelayList` on connector control stream open
- [ ] **M2-C5** `controller/internal/connector/control_stream.go` — push updated `LabelledRelayList` when relay pool changes (relay added, removed/expired, address/SPIFFE changed, capacity label promoted)
- [ ] **M2-C6** Unit tests: hysteresis transitions, hold-down timer, push-on-promotion only
- [ ] **Build gate:** `cd controller && go build ./...`

### Phase D — M3: Connector Probe, Ranking, Selector, Migration

> Depends on Phase A + C. See [[Sprint11/Member3-Rust/Phase1-Probe-Ranking]], [[Sprint11/Member3-Rust/Phase2-Selector-Migration]].

- [ ] **M3-D1** `connector/src/relay_probe.rs` (new) — parallel QUIC probe, RTT measurement, `request_id` generation and validation, QUIC peer SPIFFE validation against `LabelledRelayInfo.spiffe_id`, score computation (`rtt_ms + ceil(fill_ratio × 50)`), concurrent probe cap
- [ ] **M3-D2** `connector/src/relay_ranking.rs` (new) — `RelayRanking` struct; atomic state file write/read (`list_version`, `probed_at`, top-5 entries); staleness check on startup; validation against current `LabelledRelayList`
- [ ] **M3-D3** `connector/src/relay_selector.rs` (new) — three-phase state machine:
  - Startup: ranked[0] if valid → random Tier 1 → random Tier 2 → backoff
  - Background optimization: probe all Tier 1+2, exhausted-active forced migration, normal threshold migration
  - Make-before-break: register new → route new streams → drain old (`RELAY_DRAIN_TIMEOUT_SECS`) → report `ConnectorRelayState`
- [ ] **M3-D4** `connector/src/relay_selector.rs` — failover: filter ranking to current list → try in order → full probe → exponential backoff
- [ ] **M3-D5** `connector/src/control_stream.rs` — handle `LabelledRelayList` body variant (field 17) → send to relay selector via watch channel
- [ ] **M3-D6** `connector/src/relay_client.rs` — dual-connection support during Phase 3 drain; route new streams to new relay while old drains
- [ ] **M3-D7** `connector/src/config.rs` — remove static `RELAY_ADDR` / `RELAY_SPIFFE_ID`; add all `RELAY_*` config vars from ADR-016 config table
- [ ] **Build gate:** `cd connector && cargo build`

### Phase E — M2 + M3: Integration & Validation

> Depends on Phases B–D. See [[Sprint11/Member2-Go/Phase3-Integration]], [[Sprint11/Member3-Rust/Phase3-Integration]].

- [ ] **TEAM-E1** Two connectors on different relays → each reports `ConnectorRelayState` → controller records distinct placements → ACL snapshot shows each `ACLConnector` with correct `relay_addr`
- [ ] **TEAM-E2** Relay crosses 80% capacity → label promoted to exhausted after hold-down → dropped from `LabelledRelayList` → connectors migrate to next best relay → new placement recorded
- [ ] **TEAM-E3** Connector process restart → reads persisted ranking → connects to `ranked[0]` immediately → background re-probe fires; clients continue routing through the 15s ACL sync window
- [ ] **TEAM-E4** All Tier 1 relays full → connector falls back to Tier 2 for startup → controller alert fires
- [ ] **TEAM-E5** Probe with mismatched `request_id` → discarded; probe from wrong SPIFFE peer → treated as failure
- [ ] **TEAM-E6** 1,000 simulated connectors boot simultaneously → no Tier 1 relay receives > 2× average connections
- [ ] **TEAM-E7** Background optimization finds relay with > 15% + 10ms improvement → make-before-break migration → no active mock traffic dropped

---

## Final Build Gates

- [ ] `buf generate`
- [ ] `cd controller && go build ./...`
- [ ] `cd controller && go test ./internal/relay/... ./internal/connector/...`
- [ ] `cd relay && cargo build`
- [ ] `cd relay && cargo test`
- [ ] `cd connector && cargo build`
- [ ] `cd connector && cargo test`

---

## Acceptance Criteria

- [ ] Connector starts without `RELAY_ADDR` env var and selects relay dynamically from controller push.
- [ ] Relay capacity label changes trigger `LabelledRelayList` push after hold-down; connectors re-probe immediately on version increment.
- [ ] Make-before-break migration: new streams route to new relay before old relay is torn down.
- [ ] Exhausted relay triggers immediate migration regardless of score threshold.
- [ ] Persisted ranking survives process restart; connector online before re-probe completes.
- [ ] Probe `request_id` mismatch and SPIFFE mismatch both treated as probe failure.
- [ ] Exponential backoff on disconnected retry; caps at `RELAY_RECONNECT_MAX_SECS`.
- [ ] Controller `connector_relay_placement` always reflects current live placement.

---

## Deferred

- Geographic/region/tag relay eligibility policy (ADR-016-C, future sprint).
- Workspace-scoped relay pools (currently platform-level in v1).
- `RELAY_MAX_CONNECTIONS` derived from system resources (CPU/memory telemetry).
- Client-side relay poll acceleration on migration (currently depends on 15s poll).
