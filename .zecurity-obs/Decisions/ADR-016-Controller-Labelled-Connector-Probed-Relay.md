# ADR-016: Tiered Background Optimization for Connector Relay Selection

**Status:** Proposed
**Track:** B — Architecture
**Author:** Zecurity Engineering
**Reviewed:** 2026-06-27
**Depends on:** ADR-014 (Relay Stabilization), ADR-015 (Transport Control Plane)
**Supersedes:** ADR-016-Placement-Engine (deleted), ADR-016-Alt-Connector-Self-Select (deleted)

---

## Purpose

Define the relay selection mechanism for Track B. The controller acts as a coarse capacity filter — labelling relays by tier based on load — and the connector handles instant startup via random selection within the eligible tier, followed by silent background latency optimization with zero-drop migration.

> Which component decides which relay a connector uses, and how is that decision made without startup delay or thundering herd?

---

## Motivation

**ADR-016 (Placement Engine)** is correct in principle but requires a leader lease, epoch management, and batch coordination — significant controller complexity for a problem the connector is better positioned to solve (network RTT is not observable from the controller).

**ADR-016-Alt (Connector Self-Select)** has two blocking issues:
1. The IP-proximity pre-filter (`XOR leading bits`) is unsound — numeric adjacency has near-zero correlation with RTT. It permanently excludes the lowest-latency relay before probing.
2. It contradicts ADR-015's stated principle: *"Controller is the single source of truth. Connectors never choose relays."*

**ADR-016 (this ADR)** resolves both:
- The controller controls eligibility via capacity labels (grounded in real heartbeat data, not heuristic).
- The connector chooses *within* the controller-approved eligible pool — consistent with ADR-015.
- Instant startup (random selection from Tier 1) eliminates the probe-before-connect delay.
- Background optimization with make-before-break migration eliminates packet loss on relay switch.

---

## Decision

The controller labels each active relay with a capacity tier based on reported fill ratio. It pushes a `LabelledRelayList` to connectors via the control stream. Connectors pick a random Tier 1 relay at startup for zero-latency online, then asynchronously probe all eligible relays and migrate to the lowest-latency one if it is meaningfully better — establishing the new connection before cutting over the old one.

---

## Capacity Tiers

| Tier | Fill Ratio | Connector Action |
|---|---|---|
| **High (Tier 1)** | enter < 45%, exit ≥ 50% | Eligible for instant startup + background optimization |
| **Medium (Tier 2)** | enter < 75%, exit ≥ 80% | Eligible for background optimization only |
| **Low / Exhausted** | ≥ 80% | Dropped from snapshot — connector never sees it |

`fill_ratio = connection_count / max_connections`

Thresholds use a **dead-band** to prevent label oscillation: a relay must cross the entry threshold
to gain a tier and the exit threshold to lose it — there is a gap between the two. A label change
is not published to connectors until the new label has been stable for `RELAY_LABEL_HOLDDOWN_SECS`
(default 60s). See Architectural Review — Gap 1 for rationale.

Thresholds are configurable: `RELAY_TIER1_ENTER`, `RELAY_TIER1_EXIT`, `RELAY_TIER2_ENTER`,
`RELAY_TIER2_EXIT`, `RELAY_LABEL_HOLDDOWN_SECS`.

---

## Relay Telemetry Reporting

Relays append connection count and configured capacity to their existing heartbeat payload:

```protobuf
// In RelayHeartbeat (proto/relay/v1/relay.proto):
uint32 connection_count = N;    // current active registered connections
uint32 max_connections  = N+1;  // configured ceiling (RELAY_MAX_CONNECTIONS env var)
```

The relay also responds to lightweight probe connections:

```protobuf
message ProbeRequest  { string connector_id = 1; }
message ProbeResponse { uint32 connection_count = 1; uint32 capacity = 2; }
```

After QUIC mTLS handshake, the relay detects `ProbeRequest` (distinct from `RegisterMsg`), responds with `ProbeResponse`, and closes the connection without registering the connector.

---

## Controller: Tiered Snapshot Generation

On each relay heartbeat:
1. Compute `fill_ratio = connection_count / max_connections`.
2. Assign tier: `high` (< 0.50), `medium` (0.50–0.80), or drop (> 0.80).
3. Update `relays.capacity_label` in DB.
4. If the tier label changed → push updated `LabelledRelayList` to all active connector control streams.

Push also triggered on control stream open (current snapshot) and relay expiry (relay goes inactive).

### Proto

```protobuf
enum RelayCapacityLabel {
  RELAY_CAPACITY_HIGH   = 0;
  RELAY_CAPACITY_MEDIUM = 1;
}

message LabelledRelayInfo {
  string             relay_id  = 1;
  string             relay_addr = 2; // host:9093
  string             spiffe_id  = 3;
  RelayCapacityLabel label      = 4;
}

message LabelledRelayList {
  repeated LabelledRelayInfo relays  = 1;
  uint64                     version = 2; // monotonic; connector skips re-probe if unchanged
}

// In ConnectorControlMessage oneof body:
LabelledRelayList relay_list = 17;
```

Field 16 is reserved for `TransportSnapshot` (ADR-015/ADR-017). Field 17 is the relay list.

---

## Connector: Three-Phase State Machine

### Phase 1 — Instant Startup

On receipt of `LabelledRelayList`:
1. Filter to **Tier 1 (High)** relays only.
2. Pick one **at random** — no network probes, no RTT measurement.
3. Dial → mTLS handshake → register. Begin routing traffic immediately.
4. Report chosen relay to controller via `ConnectorRelayState` (field 15, unchanged).
5. Spawn Phase 2 background task.

**Why random over jitter-hashed:** deterministic jitter spreads boot storms but creates reachability
gaps when one connector ID always hashes to an overloaded relay. Uniform random distribution across
Tier 1 at boot achieves the same spread with no bias.

### Phase 2 — Background Optimization

Runs as an async task after Phase 1 registration is confirmed:
1. Take all **Tier 1 + Tier 2** relays from the snapshot, excluding the current active relay.
2. Execute parallel lightweight QUIC probe: `ProbeRequest → ProbeResponse`. Measure wall-clock RTT
   from dial start to `ProbeResponse` received (includes handshake).
3. Score each result: `score = rtt_ms + ceil(fill_ratio × 50)` (lower = better).
4. Probe the current active relay too (to get a fresh score for comparison).
5. If `(current_score - best_score) > max(current_score × 0.15, 10ms)` → trigger Phase 3.
   Both conditions must hold: relative improvement > 15% **and** absolute improvement > 10ms.
   This prevents noise-driven migrations on low-latency paths. See Architectural Review — Gap 2.
6. Otherwise: hold current relay, re-probe after `RELAY_REPROBE_INTERVAL_SECS` (default 300s).

Re-probe also triggered immediately when `LabelledRelayList.version` increments.

### Phase 3 — Make-Before-Break Migration

When Phase 2 identifies a significantly better relay:
1. **Dial new relay** — full mTLS handshake + registration. Do not cut over yet.
2. Once new relay registration is confirmed: **route all new outbound streams** to the new relay.
3. **Drain old relay** — allow open streams to close naturally. Force-close after `RELAY_DRAIN_TIMEOUT_SECS`
   (default 30s = 2 × client poll interval of 15s). Clients re-sync within 15s and route new
   requests to the new relay; existing sessions complete on the old relay within the drain window.
   See Architectural Review — Gap 3.
4. Tear down old relay connection.
5. **Report** new placement via `ConnectorRelayState` to controller.

Controller on receipt of `ConnectorRelayState`:
1. `UpsertPlacement(ctx, connectorID, relayID)` → `connector_relay_placement`
2. `NotifyPolicyChange` per workspace
3. ACL snapshot recompiles with updated `relay_addr` on `ACLConnector`
4. Clients fetch updated snapshot → route to new relay

### State Machine

```
Startup
  │
  ├─ state file present + fresh → connect to ranked[0] immediately
  ├─ state file present + stale/version mismatch → connect to ranked[0] + re-probe in background
  └─ state file missing → recv LabelledRelayList → pick random Tier1 → dial
  │
  ▼
Phase1Connected  ←──────────────────────────────┐
  │                                             │
  ├─ spawn background probe task                │
  │    → probe Tier1 + Tier2 relays             │
  │    → persist top 5 to state file            │
  │         ↓ improvement > max(15%, 10ms)      │
  │    → dial new relay → register              │
  │    → route new streams to new relay         │
  │    → drain old relay (30s timeout)          │
  │    → report ConnectorRelayState             │
  └─────────────────────────────────────────────┘ (loop)
  │
  ├─ recv new LabelledRelayList.version → re-probe immediately
  ├─ primary relay drops → attempt ranked[1..4] from state file → re-probe if all fail
  └─ all relays unreachable → Disconnected (retry after backoff)
```

---

## Persisted Relay Ranking

After every probe cycle, the connector writes the **top 5 scored relay entries** to a state file
on disk. This serves two purposes:

1. **Instant failover** — when the active relay dies, the connector immediately attempts the next
   ranked entry from the state file without any re-probe delay.
2. **Fast restart** — when the connector process restarts, it reads the state file and connects to
   `ranked[0]` immediately, with no cold-probe delay before routing traffic.

### State File Format

```json
{
  "list_version": 7,
  "probed_at": "2026-06-27T10:34:00Z",
  "entries": [
    {
      "rank": 0,
      "relay_id": "<uuid>",
      "relay_addr": "relay1.example.com:9093",
      "spiffe_id": "spiffe://platform/relay/<uuid>",
      "score": 12,
      "rtt_ms": 8,
      "fill_ratio": 0.08
    }
    // ... up to rank 4
  ]
}
```

Stored in the connector state directory (same location as other connector durable state).
The file is rewritten atomically (write to `.tmp`, rename) after every completed probe cycle.

### Staleness Handling on Restart

The connector checks two conditions when reading the state file at startup:

| Condition | Action |
|---|---|
| `probed_at` is **< 1 hour** ago AND `list_version` matches current `LabelledRelayList` | Trust ranking — connect to `ranked[0]` directly, skip Phase 1 random pick |
| `probed_at` is **< 1 hour** ago but `list_version` differs (pool changed) | Use ranking for instant connect, immediately kick off background re-probe |
| `probed_at` is **≥ 1 hour** ago (stale) | Use ranking for instant connect only, kick off background re-probe immediately |
| State file missing or corrupt | Fall back to Phase 1 random Tier 1 pick |

In all cases the connector is online and routing traffic before the re-probe completes.
The re-probe runs in the background and triggers Phase 3 migration if a better relay is found.

### Ranking Depth: Top 5

The ranking stores 5 entries (increased from 3) to provide greater failover depth:
- `ranked[0]` — primary (active)
- `ranked[1]` — first failover (no probe delay)
- `ranked[2]` — second failover
- `ranked[3]`, `ranked[4]` — deep fallback for multi-relay failures

---

## Failover

If the active relay connection drops unexpectedly (not a planned Phase 3 migration):
1. Read `ranked[1]` from persisted state — no probe latency, survives process restart.
2. Attempt `ranked[1]` immediately.
3. If `ranked[1]` fails within 5s → attempt `ranked[2]`, then `ranked[3]`, then `ranked[4]`.
4. If all 5 entries fail → trigger full re-probe of current `LabelledRelayList`.
5. If re-probe yields nothing → direct-only mode, retry probe after 30s backoff.

Persisted entries are invalidated when a new `LabelledRelayList` version arrives with a higher
version number — they are replaced after the next probe cycle completes.

---

## Load Balancing Analysis

| Scenario | Behavior |
|---|---|
| 1,000 connectors boot simultaneously | Each picks a random Tier 1 relay. Uniform distribution. No probe storm. |
| One Tier 1 relay fills to 80% | Label changes to `low` → dropped from next snapshot. Connectors re-probe and migrate away via Phase 3. |
| Geographically skewed topology | Phase 2 RTT probe finds the nearest relay regardless of IP prefix — no XOR heuristic. |
| All Tier 1 relays exhausted | Connectors fall back to Tier 2 for startup (controller should alert on this). |
| Pool-change pushed to all connectors | Version increment triggers re-probe, but Phase 1 path is instant (no storm on startup). |

---

## Comparison

| Dimension | ADR-016 (Placement Engine) | ADR-016-Alt (Self-Select) | ADR-016 (This ADR) |
|---|---|---|---|
| Startup latency | Await assignment from controller | Probe before connecting | Zero — random Tier1 pick |
| Load filter | LeastLoaded heuristic at assign time | IP XOR (unsound) | Capacity label from real heartbeat data |
| RTT optimization | None (controller can't measure) | Yes, but blocked by bad pre-filter | Yes — background, after connected |
| Packet loss on relay switch | N/A (connector stays put) | Full reconnect | Make-before-break: zero drop |
| Controller complexity | High — leader lease, epoch | Low | Low — label compute + list push |
| ADR-015 coherent | Yes | No | Yes — controller controls eligibility |
| Thundering herd | N/A | Jitter (deterministic bias) | Random at startup (unbiased) |

---

## Tradeoffs

**Pros:**
- Zero startup latency — connector is online and routing traffic before any probe completes.
- No thundering herd — random Tier 1 pick at boot distributes load without storms.
- RTT optimization uses real network measurement, not a proxy metric.
- Make-before-break guarantees zero packet loss during relay migration.
- Controller retains authority over the eligible pool (ADR-015 coherent).
- No leader lease, no epoch, no batch coordination.

**Cons:**
- A connector may start on a suboptimal relay and take up to 5 minutes to migrate.
- Brief window where two relay connections are open simultaneously (during Phase 3 drain).
- More connector code than passive assignment — probe logic, state machine, drain timer.
- `capacity_label` column adds a DB write on every heartbeat where tier changes.

---

## Open Questions

1. **Tier 1 exhausted at boot** — if no Tier 1 relays exist, connector falls back to Tier 2 for startup. Should the controller alert on this? Should the connector log a warning?
2. **Migration threshold (15%)** — is this the right sensitivity? Lower = more migrations (churn), higher = slower convergence.
3. **Drain timeout (30s)** — appropriate for expected stream lifetimes? Should it be configurable?
4. **Capacity field source** — `RELAY_MAX_CONNECTIONS` env var is the simplest; dynamic derivation from system resources is possible but complex. Env var is recommended for v1.
5. **Geographic policy** — if a workspace requires connectors to use relays in a specific region, the tier label alone cannot enforce it. A future ADR-016-C could add a `region` tag to `LabelledRelayInfo` that connectors filter on before Phase 1.

---

## Implementation Checklist

### Proto
- [ ] `proto/relay/v1/relay.proto` — add `connection_count`, `max_connections` to relay heartbeat; add `ProbeRequest`, `ProbeResponse`
- [ ] `proto/connector/v1/connector.proto` — add `RelayCapacityLabel`, `LabelledRelayInfo`, `LabelledRelayList`; add field 17 to `ConnectorControlMessage`
- [ ] `buf generate` — regenerate Go + Rust stubs

### Relay
- [ ] `relay/src/config.rs` — add `RELAY_MAX_CONNECTIONS` env var
- [ ] `relay/src/session.rs` — detect `ProbeRequest` on new connection, respond with `ProbeResponse { connection_count, capacity }`, close without registering
- [ ] `relay/src/protocol.rs` — add `ProbeRequest` / `ProbeResponse` framing

### Controller
- [ ] `controller/internal/relay/heartbeat.go` — compute `fill_ratio` from heartbeat fields; update `relays.capacity_label`; push `LabelledRelayList` if label changed
- [ ] `controller/internal/connector/control_stream.go` — push `LabelledRelayList` on stream open and on relay pool change
- [ ] DB migration — add `capacity_label` column to `relays` table

### Connector
- [ ] `connector/src/relay_probe.rs` (new) — parallel `ProbeRequest`/`ProbeResponse`, RTT measurement, score computation
- [ ] `connector/src/relay_ranking.rs` (new) — top-5 `RelayRanking` struct, atomic state file write/read, staleness check on startup
- [ ] `connector/src/relay_selector.rs` (new) — three-phase state machine: instant startup (from state file or random Tier1), background optimization, make-before-break migration
- [ ] `connector/src/control_stream.rs` — handle `LabelledRelayList` → trigger Phase 1
- [ ] `connector/src/relay_client.rs` — dual-connection support for Phase 3 drain
- [ ] `connector/src/config.rs` — remove static `RELAY_ADDR`/`RELAY_SPIFFE_ID`; add `RELAY_REPROBE_INTERVAL_SECS`, `RELAY_MIGRATION_THRESHOLD_PCT`, `RELAY_DRAIN_TIMEOUT_SECS`
- [ ] Build check: `cd connector && cargo build` passes

### Tests
- [ ] Unit: tier label computation (`fill_ratio → label`)
- [ ] Unit: Phase 1 random selection distributes uniformly across Tier 1
- [ ] Unit: Phase 2 score comparison with 15% threshold
- [ ] Unit: Phase 3 make-before-break — new connection established before old torn down
- [ ] Unit: state file write → process restart → connect to ranked[0] without re-probe
- [ ] Unit: stale state file (> 1 hour) → connect to ranked[0] + background re-probe fires
- [ ] Unit: state file version mismatch → connect to ranked[0] + re-probe fires immediately
- [ ] Unit: ranked[0] unreachable on restart → fall through ranked[1..4] from state file
- [ ] Simulation: 1,000 connectors boot → verify no single Tier 1 relay receives > 2× average connections
- [ ] Integration: relay crosses 80% threshold → drops from snapshot → connectors migrate
- [ ] Integration: controller records new `ConnectorRelayState` → ACL recompile → client routes correctly

---

## Architectural Review — Gaps Identified and Resolution

This section documents findings from the design review of ADR-016 and its predecessors, including
which issues remain open (with their fixes) and which were resolved by the existing system design.

---

### Gap 1 — No Hysteresis on Tier Transitions *(Open — fix required)*

**Issue:** Tier boundaries are hard thresholds (`fill_ratio < 0.50 = High`). A relay sitting at
49.9% capacity is Tier 1. One new connection pushes it to 50.1% → label changes to Medium →
controller pushes new `LabelledRelayList` → all connected connectors re-probe immediately. Some
migrate away → fill drops to 49.9% → label flips back to High → re-probe again. At 1,000
connectors, a relay oscillating on the 50% boundary triggers ~1,000 simultaneous re-probes every
few seconds — a thundering herd caused by the design itself.

**Fix applied:** Two mitigations combined:

1. **Dead-band gap between tiers** — boundaries are asymmetric so a relay must cross a gap before
   switching tier:
   - Tier 1: enter when `fill_ratio < 0.45`, exit when `fill_ratio ≥ 0.50`
   - Tier 2: enter when `fill_ratio < 0.75`, exit when `fill_ratio ≥ 0.80`
   - Exhausted: enter when `fill_ratio ≥ 0.80`
2. **Hold-down window** — a relay's label must remain stable for **60 seconds** before a
   label-change push is issued to connectors. Transient spikes do not trigger re-probes.

Thresholds and hold-down window are configurable via controller env vars
(`RELAY_TIER1_ENTER`, `RELAY_TIER1_EXIT`, `RELAY_LABEL_HOLDDOWN_SECS`).

---

### Gap 2 — Improvement Threshold Fails at Low RTT *(Open — fix required)*

**Issue:** Migration triggers when best candidate score is `> 15%` better than current. At low
RTT (e.g., current relay = 4ms, candidate = 3ms → 25% improvement = 1ms difference), this is
below single-measurement jitter on a QUIC handshake (±1–2ms). Spurious migrations would fire
constantly on local-network deployments.

**Fix applied:** Threshold requires **both** conditions to be true:
- Relative improvement > 15% **AND**
- Absolute improvement > 10ms

```
migrate_if: (current_score - best_score) > max(current_score * 0.15, 10ms)
```

This prevents noise-driven migrations on low-latency paths while still catching meaningful
improvements on high-latency paths (e.g., cross-region).

---

### Gap 3 — Make-Before-Break "Zero Packet Loss" Claim Scope *(Resolved)*

**Issue raised:** Make-before-break protects new streams but not existing open client sessions.
After 30s drain timeout, open streams are force-closed. Clients using those streams would lose
their session.

**Why this is resolved by the existing system design:**

The client synchronises with the controller every **15 seconds** by polling `GetACLSnapshot`.
The migration flow is:

```
1. Connector registers on new relay
2. Connector sends ConnectorRelayState to controller
3. Controller: UpsertPlacement → NotifyPolicyChange → ACL recompiles
4. Client polls GetACLSnapshot within ≤15s → receives updated relay_addr
5. Client's NEW requests route to new relay silently
6. Client's EXISTING open connections continue on old relay until they complete naturally
7. Old relay connection drains and closes after 30s
```

Existing connections drain naturally within the 15s client sync window. The 30s drain timeout
is calibrated to exactly `2 × client_poll_interval (15s)` — it covers the full sync window plus
a 15s buffer for in-flight requests. No sessions are force-closed while a client still needs them.

**Constraint this creates:** If the client poll interval changes, the drain timeout must change
with it. The drain timeout is defined as `RELAY_DRAIN_TIMEOUT_SECS = 2 × client_poll_interval`.

---

### Gap 4 — Cross-Workspace Probe Information Disclosure *(Resolved)*

**Issue raised:** `ProbeRequest` allows any authenticated connector to probe any relay and learn
its `connection_count` and `capacity` — including relays serving other workspaces.

**Why this is resolved by the controller label mechanism:**

The `LabelledRelayList` pushed to each connector is **workspace-scoped** — the controller builds
it from relays that serve that connector's workspace only. A connector never receives relay entries
for other workspaces and therefore never has the addresses to probe them.

No changes to the probe protocol are needed. The access control is enforced at the list-delivery
layer, not the probe layer.

---

### Gap 5 — Current Relay Re-Probed via New Handshake *(Moderate — accepted)*

**Issue:** Phase 2 probes the current active relay by opening a new QUIC handshake. This measures
handshake RTT to an already-connected relay, not the latency of the existing persistent connection.
The existing connection's heartbeat timing provides a more accurate RTT for free.

**Resolution:** Accepted as a known approximation. The existing connection's heartbeat interval
is coarse (30s) and not designed for latency measurement. A fresh probe gives a consistent
measurement basis across all relays (same handshake-included RTT metric). The slight inaccuracy
is acceptable given the 10ms absolute improvement floor (Gap 2 fix) — small RTT mismeasurements
cannot trigger spurious migrations.

---

### Gap 6 — ADR-016-Alt IP Proximity Pre-Filter *(Superseded)*

**Issue (from ADR-016-Alt review):** The `ip_proximity_score` pre-filter (`XOR leading bits`)
measures numeric IP adjacency which has near-zero correlation with RTT. It permanently excludes
the lowest-latency relay before probing.

**Why superseded:** ADR-016 eliminates the IP proximity pre-filter entirely. The capacity tier
label — computed from real `connection_count / max_connections` data reported in relay heartbeats —
replaces it as the pre-filter. The connector probes all Tier 1 + Tier 2 relays, not a
heuristic-selected subset. RTT measurement determines the final ranking.

---

### Gap 7 — ADR-016-Alt ADR-015 Conflict *(Superseded)*

**Issue (from ADR-016-Alt review):** ADR-016-Alt stated "the controller does not assign a relay,"
directly contradicting ADR-015's principle: *"Controller is the single source of truth. Connectors
never choose relays."*

**Why superseded:** ADR-016 resolves this by design. The controller controls **eligibility** via
the labelled relay list — it decides which relays are available for each workspace. The connector
selects **within** the controller-approved pool. This is coherent with ADR-015: the controller
retains authority, the connector only measures what the controller cannot (network RTT).

---

## Follow-up

If ADR-016 is adopted:
- ADR-016-Placement-Engine is superseded (deleted).
- ADR-016-Alt-Connector-Self-Select is superseded (deleted).
- ADR-017 (Transport Propagation) and ADR-018 (Migration Strategy) apply unchanged — only the mechanism that populates `connector_relay_placement` differs.
- Geographic/policy-based placement (region filter on `LabelledRelayInfo`) should be ADR-016-C if required.
