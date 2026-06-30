# ADR-016: Tiered Background Optimization for Connector Relay Selection

**Status:** Proposed
**Track:** B - Architecture
**Author:** Zecurity Engineering
**Reviewed:** 2026-06-27
**Depends on:** ADR-014 (Relay Stabilization), ADR-015 (Transport Control Plane)
**Supersedes:** ADR-016-Placement-Engine (deleted), ADR-016-Alt-Connector-Self-Select (deleted)

---

## Purpose

Define how connectors select relays without startup delay, thundering-herd behavior, or controller-side latency guessing.

The controller remains the source of truth for relay eligibility. It labels active relays by capacity and pushes the eligible set to connectors. Connectors choose and optimize only inside that controller-approved set: random startup selection first, then background latency probing and make-before-break migration with best-effort drain.

---

## Decision

The controller pushes a versioned `LabelledRelayList` to connectors over the existing connector control stream. A connector immediately registers with a random Tier 1 relay, then probes Tier 1 + Tier 2 relays in the background for RTT/reachability and migrates only when another eligible relay is meaningfully better.

This replaces two rejected approaches:

- A controller placement engine: too much controller complexity, and the controller cannot observe connector-to-relay RTT.
- Connector self-selection over all relays: violates controller-controlled eligibility and previously used an unsound IP-proximity heuristic.

Relays are platform-level in v1. A valid relay may authenticate connectors and clients from all workspaces. Workspace isolation remains enforced by mTLS/SPIFFE, connector/client trust-domain checks, ACLs, and tunnel authorization, not by relay-list filtering.

---

## Capacity And Eligibility

`fill_ratio = connection_count / max_connections`

`connection_count` means active bridged client relay streams across all workspaces. `registered_connectors` remains separate telemetry for observability and does not by itself make a relay exhausted.

| Tier | Thresholds | Connector behavior |
|---|---|---|
| High / Tier 1 | enter `< 45%`, exit `>= 50%` | Eligible for instant startup and background optimization |
| Medium / Tier 2 | enter `< 75%`, exit `>= 80%` | Used for startup only if no Tier 1 exists; otherwise background optimization only |
| Low / Exhausted | `>= 80%` | Not offered for new selection; existing attached connectors may continue until migration |

Low/exhausted and inactive relays are omitted from `LabelledRelayList`; the proto intentionally has only High and Medium labels. Connectors treat absence from the current list as not eligible for new selection.

All configurable values:

| Variable | Default | Description |
|---|---|---|
| `RELAY_TIER1_ENTER` | 0.45 | fill_ratio below which a relay enters Tier 1 |
| `RELAY_TIER1_EXIT` | 0.50 | fill_ratio at or above which a relay exits Tier 1 |
| `RELAY_TIER2_ENTER` | 0.75 | fill_ratio below which a relay enters Tier 2 |
| `RELAY_TIER2_EXIT` | 0.80 | fill_ratio at or above which a relay exits Tier 2 (becomes exhausted) |
| `RELAY_LABEL_HOLDDOWN_SECS` | 60 | Minimum seconds a candidate label must be stable before promotion |
| `RELAY_REPROBE_INTERVAL_SECS` | 300 | Background re-probe interval when on a healthy relay |
| `RELAY_DRAIN_TIMEOUT_SECS` | 30 | Seconds to drain old relay before force-close on migration |
| `RELAY_MAX_CONCURRENT_PROBES` | 5 | Max parallel probe connections per probe cycle |
| `RELAY_RECONNECT_BASE_SECS` | 5 | Initial backoff delay on disconnected retry |
| `RELAY_RECONNECT_MAX_SECS` | 120 | Maximum backoff delay cap |
| `RELAY_RECONNECT_BACKOFF_FACTOR` | 2.0 | Exponential multiplier per retry |

The controller applies hysteresis and hold-down before publishing a label change:

1. Compute `candidate_label` from the latest heartbeat.
2. If `candidate_label == capacity_label`, clear `pending_capacity_label` and `pending_label_since`.
3. If `candidate_label != capacity_label` and differs from `pending_capacity_label`, set `pending_capacity_label = candidate_label` and `pending_label_since = now`.
4. If `candidate_label == pending_capacity_label` and the hold-down elapsed, promote it: set `capacity_label = candidate_label`, set `last_label_changed_at = now`, clear pending fields, increment the relay-list version, and push a new `LabelledRelayList`.

`last_label_changed_at` updates only on promotion.

The existing `ListWorkspacesForRelay(ctx, relayID)` is not an eligibility table. It derives affected workspaces from `connector_relay_placement -> connectors.tenant_id` and is used to notify workspaces whose ACL snapshots may reference a relay that changed metadata, label, or expiry state.

---

## Protocol Changes

Relay heartbeat adds active-stream capacity data:

```protobuf
// proto/relay/v1/relay.proto
uint32 connection_count = N;    // active bridged client streams
uint32 max_connections  = N+1;  // RELAY_MAX_CONNECTIONS
```

The relay must add runtime instrumentation for this: increment when a client lookup bridge starts, decrement when that bridge ends, and report the current count in heartbeat only. Probes do not include load data — they carry only the echoed `request_id`.

Relay probing uses a lightweight request/response after QUIC mTLS. Probes do not expose relay load; load is reported only to the controller via heartbeat and represented to connectors only as `RelayCapacityLabel`.

```protobuf
message ProbeRequest  {
  string connector_id = 1;
  uint64 request_id   = 2; // random nonce generated per probe; echoed by relay
}
message ProbeResponse {
  uint64 request_id = 1; // must match ProbeRequest.request_id
}
```

`ProbeResponse` has no status or error field in v1. On rate-limit, concurrent-limit, malformed, or unauthorized probe attempts, the relay closes the stream/connection. The connector treats close, timeout, or missing response as probe failure.

The connector must validate two things before accepting a probe result:
1. `ProbeResponse.request_id` matches the outstanding `ProbeRequest.request_id` — discards stale, out-of-order, or replayed responses.
2. The QUIC peer SPIFFE ID (from the mTLS handshake) matches `LabelledRelayInfo.spiffe_id` for that `relay_addr` — discards responses from wrong endpoints. A mismatch is treated as probe failure.

Connector control stream adds:

```protobuf
enum RelayCapacityLabel {
  RELAY_CAPACITY_HIGH   = 0;
  RELAY_CAPACITY_MEDIUM = 1;
}

message LabelledRelayInfo {
  string relay_id = 1;
  string relay_addr = 2; // host:9093
  string spiffe_id = 3;
  RelayCapacityLabel label = 4;
}

message LabelledRelayList {
  repeated LabelledRelayInfo relays = 1;
  uint64 version = 2;
}

// ConnectorControlMessage oneof body:
LabelledRelayList relay_list = 17;
```

Field 16 remains reserved for `TransportSnapshot` (ADR-015/ADR-017). Field 17 is the relay list.

---

## Connector Behavior

### Startup

When a current `LabelledRelayList` is available:

1. If a persisted relay ranking exists, connect to the first valid ranked relay. Valid means the relay is still present in the current `LabelledRelayList` as Tier 1 or Tier 2.
2. If no valid ranked relay exists, choose a random Tier 1 relay.
3. If no Tier 1 relay exists, choose a random Tier 2 relay and log a warning.
4. If no Tier 1 or Tier 2 relay exists, enter disconnected/backoff and retry after `RELAY_REPROBE_INTERVAL_SECS`.
5. Register with the relay, begin routing, report `ConnectorRelayState`, and start background probing.

If no current relay list is available yet, a fresh state file may be used for fast reconnect to `ranked[0]`. When the list arrives, immediately revalidate: if the active relay is absent/exhausted, switch to the first valid ranked relay, then random Tier 1, then random Tier 2; if none exist, enter disconnected/backoff.

### Background Optimization

After registration:

1. Probe all Tier 1 + Tier 2 relays, including the current relay. Run at most `RELAY_MAX_CONCURRENT_PROBES` (default 5) probes in parallel; queue the rest.
2. Measure RTT from dial start to `ProbeResponse`, including handshake time.
3. Score each relay as `score = rtt_ms`. Capacity is already reflected by the controller-assigned High/Medium label.
4. **Exhausted active relay:** if the current relay is absent from the current `LabelledRelayList` (it crossed the exhausted threshold), skip the improvement threshold and migrate to the best valid relay immediately.
5. **Normal migration:** migrate only if `current_score - best_score > max(current_score * 0.15, 10ms)`. Both relative (15%) and absolute (10ms) conditions must hold.
6. Otherwise keep the current relay and re-probe after `RELAY_REPROBE_INTERVAL_SECS` (default 300s).

A new `LabelledRelayList.version` triggers immediate re-probe.

### Migration

Migration is make-before-break:

1. Dial and register with the new relay.
2. After registration succeeds, route new outbound streams to the new relay.
3. Keep the old relay connection alive for drain.
4. Force-close the old relay after `RELAY_DRAIN_TIMEOUT_SECS` (default 30s).
5. Report the new placement using `ConnectorRelayState`.

`RELAY_DRAIN_TIMEOUT_SECS` (default 30s) is operator-configured independently. The recommended value is `2 × client ACL poll interval` (currently 15s → 30s), since new client requests should route to the new relay after their next poll. However, the connector has no visibility into the client poll interval — this is a documentation recommendation only, not a derived value. If the client poll interval changes, the operator must update `RELAY_DRAIN_TIMEOUT_SECS` accordingly.

Controller handling of `ConnectorRelayState` remains:

1. `UpsertPlacement(ctx, connectorID, relayID)` into `connector_relay_placement`.
2. During the Track A compatibility window, notify affected policy cache/workspaces so ACL snapshots continue to carry relay coordinates for old clients.
3. After ADR-017/ADR-018 Track B propagation is enabled, notify the transport pipeline instead; `TransportSnapshot` carries relay coordinates and ACL recompilation is not required for relay-only placement changes.
4. During migration, the controller may perform both notifications until all clients have moved to `TransportSnapshot`.
5. Clients fetch the updated ACL/transport data according to their version and route new requests to the new relay.

### Persisted Ranking

After each probe cycle, the connector atomically writes the top 5 ranked relays to its state directory:

```json
{
  "list_version": 7,
  "probed_at": "2026-06-27T10:34:00Z",
  "entries": [
    {
      "rank": 0,
      "relay_id": "<uuid>",
      "relay_addr": "relay1.example.com:9093",
      "spiffe_id": "spiffe://zecurity.in/relay/<uuid>",
      "score": 12,
      "rtt_ms": 8
    }
  ]
}
```

Startup validation:

| State file condition | Action |
|---|---|
| Fresh (`probed_at < 1h`) and list version matches | Use first ranked relay still present as Tier 1 or Tier 2 |
| Fresh but list version differs | Use first valid ranked relay, then re-probe immediately |
| Stale (`probed_at >= 1h`) | Use first valid ranked relay, then re-probe immediately |
| No current relay list yet | Use fresh `ranked[0]`; on list arrival, switch away immediately if it is absent/exhausted |
| Missing/corrupt state | Random Tier 1, then Tier 2 fallback, then disconnected/backoff |

Valid ranked relay means present in the current `LabelledRelayList` as Tier 1 or Tier 2. Absent means inactive, exhausted, or otherwise not eligible.

If the active relay drops unexpectedly:
1. Filter persisted ranking to relays still present in the **current** `LabelledRelayList` as Tier 1 or Tier 2. Skip any entry absent from the current list — do not attempt dead or exhausted endpoints.
2. Attempt the first valid ranked entry. If unreachable within 5s, try the next, down to `ranked[4]`.
3. If all valid ranked entries fail, probe the current `LabelledRelayList` directly.
4. If probing yields nothing, enter disconnected/backoff with exponential delay starting at `RELAY_RECONNECT_BASE_SECS` (default 5s), doubling each attempt up to `RELAY_RECONNECT_MAX_SECS` (default 120s), with jitter factor `RELAY_RECONNECT_BACKOFF_FACTOR` (default 2.0).

---

## Operational Safeguards

- Random Tier 1 startup avoids probe storms during large connector restarts.
- Tier hysteresis plus hold-down prevents label oscillation near thresholds.
- Probe rejection is cheap: close the stream/connection and rely on connector timeout.
- Probe abuse controls: per-connector rate limit, per-probe timeout, concurrent probe cap (`RELAY_MAX_CONCURRENT_PROBES`, default 5), and relay-side audit logs for excessive probe attempts.
- Exhausted relays are not offered for new selection, but existing attached connectors may remain until normal migration.
- Probe responses carry only `request_id`; relay load is never exposed to connectors through probes — it flows to the controller via heartbeat only.

---

## Implementation Checklist

### Proto

- Add relay heartbeat `connection_count` and `max_connections`.
- Add `ProbeRequest` (with `request_id`) and `ProbeResponse` (echoing `request_id`).
- Add `RelayCapacityLabel`, `LabelledRelayInfo`, and `LabelledRelayList`.
- Add `relay_list = 17` to `ConnectorControlMessage`.
- Run `buf generate`.

### Relay

- Add `RELAY_MAX_CONNECTIONS`.
- Track active bridged client streams separately from registered connectors.
- Handle probe request/response without registering the connector or returning load data.
- Close probe streams on rate-limit, concurrent-limit, malformed, or unauthorized probes.

### Controller

- Persist relay capacity metadata: `capacity_label`, `pending_capacity_label`, `pending_label_since`, `last_label_changed_at`, `connection_count`, `max_connections`.
- Apply the exact pending-label state machine and push `LabelledRelayList` only after promotion.
- Push current `LabelledRelayList` on connector control stream open. Push a new version when a relay is added, removed/expired, its public address/SPIFFE changes, or its promoted capacity label changes.
- Preserve `ListWorkspacesForRelay(ctx, relayID)` as affected-workspace notification only; do not add workspace relay eligibility tables in v1.

### Connector

- Add relay probe, ranking, and selector modules.
- Persist top 5 relay rankings with atomic write.
- Validate persisted rankings against the current `LabelledRelayList` before reconnecting, except for temporary fast reconnect before the first list arrives.
- Implement random Tier 1 startup, Tier 2 fallback, background scoring, thresholded migration, exhausted-active forced migration, and make-before-break drain.
- Validate probe responses: check `request_id` matches and QUIC peer SPIFFE matches `LabelledRelayInfo.spiffe_id` before accepting result.
- Use RTT-only probe scoring; do not calculate or consume relay `connection_count` in the connector.
- Validate persisted rankings against current `LabelledRelayList` before failover — skip absent/exhausted entries.
- Implement exponential backoff on disconnected retry using `RELAY_RECONNECT_*` config.
- Replace static `RELAY_ADDR` / `RELAY_SPIFFE_ID` configuration with control-stream relay selection; add all config vars from the configurable values table.

---

## Test Plan

- Unit: capacity label hysteresis and hold-down transitions.
- Unit: relay active-stream counter increments/decrements around bridged client streams.
- Unit: probe rate/concurrency rejection closes stream and connector treats it as failure.
- Unit: startup ranking validation skips absent/exhausted relays.
- Unit: random Tier 1 selection distributes boot load.
- Unit: migration threshold requires both relative and absolute improvement.
- Unit: connector probe score is RTT-only and ignores relay load.
- Unit: active relay becomes exhausted (absent from list) → immediate migration regardless of score delta.
- Unit: make-before-break does not switch new streams until new registration succeeds.
- Unit: probe response with mismatched `request_id` is discarded.
- Unit: probe response from wrong SPIFFE peer is treated as failure.
- Unit: failover skips persisted ranking entries absent from current `LabelledRelayList`.
- Unit: exponential backoff increments correctly and caps at `RELAY_RECONNECT_MAX_SECS`.
- Integration: relay crosses exhausted threshold, disappears from new selection, and connectors migrate.
- Integration: `ConnectorRelayState` updates `connector_relay_placement`, triggers Track A ACL propagation and/or Track B transport propagation according to the migration phase, and clients route new requests to the new relay.
- Simulation: 1,000 simultaneous connector startups do not place more than 2x average connections on any Tier 1 relay.

---

## Open Questions

- Should the controller alert when no Tier 1 relays exist and connectors must start on Tier 2?
- Is the migration threshold of 15% and 10ms correct for production RTT variance?
- Should `RELAY_DRAIN_TIMEOUT_SECS` be derived from the client ACL poll interval instead of configured independently?
- Is `RELAY_MAX_CONNECTIONS` sufficient for v1, or should capacity later include CPU/memory/network telemetry?
