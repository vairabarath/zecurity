# ADR-014: Relay Stabilization

**Status:** Proposed
**Track:** A — Stabilization
**Author:** Zecurity Engineering
**Branch:** `integration/relay-merge`
**Reviewed:** 2026-06-25

---

## Purpose

Make the existing relay implementation correct and production-ready.

This ADR does **not** redesign the transport architecture.
It does **not** introduce TransportSnapshot, a Placement Engine, or independent transport versioning.

It answers one question only:

> What must be fixed before we can confidently ship the existing relay architecture?

All architectural improvements belong to Track B (ADR-015).

---

## Current State

The `relay-preparation` branch introduced relay infrastructure and was merged into `integration/relay-merge`. The following gaps were identified during the architecture audit.

---

## Gap 1 — Per-Connector Relay Routing

**Severity:** Critical

**Problem:**
`CompileACLSnapshot` calls `store.GetActiveRelay(ctx)` — a global `LIMIT 1 ORDER BY last_heartbeat_at DESC` query. All connectors in all workspaces receive the same relay address regardless of which relay they are actually registered on.

In a multi-relay deployment:
- Connector 1 is registered on Relay A
- Connector 2 is registered on Relay B
- Both connectors receive Relay A's address in the snapshot (whichever heartbeated most recently)
- Clients trying to reach Connector 2 attempt to open a relay tunnel on Relay A — which has no registration for Connector 2 — and fail

**Fix:**

Extend `GetConnectorsForRemoteNetworks` in `store.go` to LEFT JOIN `connector_relay_placement` and `relays`. LEFT JOIN so connectors without a placement row still appear (direct-only for that connector).

Extend `RemoteNetworkConnectorsRow` with two new fields:

```go
type RemoteNetworkConnectorsRow struct {
    RemoteNetworkID string
    ConnectorID     string
    LanAddr         string
    TrustDomain     string
    RelayAddr       string  // empty if connector has no placement or relay has no public addr
    RelayID         string  // empty if no placement; used to build SPIFFE ID
}
```

SQL:

```sql
SELECT c.remote_network_id::text,
       c.id::text,
       COALESCE(c.lan_addr, ''),
       COALESCE(c.trust_domain, ''),
       COALESCE(
         CASE
           WHEN r.public_addr IS NOT NULL AND r.public_addr != ''
             THEN r.public_addr
           WHEN r.address_scope = 'public' AND r.observed_ip IS NOT NULL
             THEN r.observed_ip::text || ':9093'
           ELSE ''
         END, ''
       ),
       COALESCE(r.id::text, '')
  FROM connectors c
  LEFT JOIN connector_relay_placement crp ON crp.connector_id = c.id
  LEFT JOIN relays r ON r.id = crp.relay_id AND r.status = 'active'
 WHERE c.remote_network_id = ANY($1::uuid[])
   AND c.status = 'active'
 ORDER BY c.remote_network_id, c.last_heartbeat_at DESC NULLS LAST
```

Add relay coordinates to `ACLConnector` in `proto/client/v1/client.proto`:

```protobuf
message ACLConnector {
  string connector_id          = 1;
  string connector_tunnel_addr = 2;
  string connector_spiffe      = 3;
  // Transitional fields — Track A only.
  // Track B (ADR-015) moves relay routing to TransportSnapshot and deprecates these.
  // When Track B ships: reserve fields 4 and 5, never reuse the numbers.
  string relay_addr            = 4;
  string relay_spiffe_id       = 5;
}
```

Update the compiler loop in `compiler.go` to populate `relay_addr` and `relay_spiffe_id` on each `ACLConnector` using `appmeta.RelaySPIFFEID(row.RelayID)`.

Keep the global relay block (`GetActiveRelay`, lines 152–168 in `compiler.go`) for the duration of Track A. It populates deprecated snapshot-level fields 6 and 9 for backward compatibility with old clients that haven't updated yet. Remove it in Track B.

**Files:** `store.go`, `compiler.go`, `proto/client/v1/client.proto`

---

## Gap 2 — Relay Event Propagation

**Severity:** Critical

**Problem:**
`relay/heartbeat.go` records relay heartbeats but never calls `NotifyPolicyChange`. When a relay:
- Comes online for the first time
- Changes its public address or IP
- Goes offline (heartbeat expires)

...no snapshot is invalidated. Clients continue to hold stale relay addresses indefinitely.

**Fix:**

In `relay/heartbeat.go`, call `NotifyPolicyChange` when relay metadata changes. The heartbeat handler already detects metadata changes via the `metadataChanged` flag in `cacheRelayHeartbeat`. Extend the `heartbeatStore` interface to accept a policy notifier, or inject it separately. Call notify when `metadataChanged` is true and a DB write occurs.

This requires the relay `Service` to hold a reference to the policy notifier (same pattern as `control_stream.go`).

**Files:** `relay/heartbeat.go`, `relay/service.go` (or wherever `Service` is constructed)

---

## Gap 3 — Relay Expiry Eviction

**Severity:** High

**Problem:**
There is no background job that marks relays inactive when their heartbeat expires. A relay that crashes or goes offline keeps its `status = 'active'` row in the database indefinitely. The `GetActiveRelay` query (and after Gap 1's fix, the JOIN query) will continue returning a dead relay.

**Fix:**

Add a background goroutine (started in `main.go` alongside other background workers) that runs on a configurable interval (default: `2 × heartbeat_interval = 60s`). It executes:

```sql
UPDATE relays
   SET status = 'inactive'
 WHERE status = 'active'
   AND last_heartbeat_at < NOW() - INTERVAL '90 seconds'
RETURNING id::text
```

For each returned relay ID, call `NotifyPolicyChange` for all workspaces that have connectors attached to that relay (via `connector_relay_placement`). This recompiles their snapshots with the dead relay absent.

**Files:** new `relay/expiry.go` or extend `relay/heartbeat.go`

---

## Gap 4 — Client Uses Global Relay Coordinates

**Severity:** Critical

**Problem:**
`build_transports_by_resource` in `client/src/daemon.rs` takes `relay_addr: &str` and `relay_spiffe_id: &str` as function parameters — snapshot-level globals. All connectors in all remote networks receive the same relay context. Even after Gap 1 adds per-connector relay coords to `ACLConnector`, the client will ignore them.

**Fix:**

Change `build_transports_by_resource` to read relay coordinates from `connector.relay_addr` and `connector.relay_spiffe_id` instead of the function parameters. Remove the `relay_addr` and `relay_spiffe_id` parameters from the function signature.

```rust
// Before
fn build_transports_by_resource(
    entries: &[AclEntry],
    remote_networks: &[AclRemoteNetwork],
    device: &DeviceInfo,
    relay_addr: &str,
    relay_spiffe_id: &str,
) -> Result<...>

// After
fn build_transports_by_resource(
    entries: &[AclEntry],
    remote_networks: &[AclRemoteNetwork],
    device: &DeviceInfo,
) -> Result<...>
```

Inside the function, the `relay_base_present` guard becomes per-connector:

```rust
let relay_base_present = !connector.relay_addr.is_empty()
    && !connector.relay_spiffe_id.is_empty();
```

`RelayPool::new` receives `connector.relay_spiffe_id` instead of the snapshot-level value.

**Files:** `client/src/daemon.rs`

---

## What Track A Does NOT Fix

| Item | Reason |
|------|--------|
| Connector drops static `RELAY_ADDR` | Requires TransportSnapshot + control stream changes — Track B |
| Active relay reassignment (Placement Engine) | Track B — Placement Engine is an architectural addition |
| Independent transport versioning and epochs | Track B |
| TransportSnapshot protocol | Track B |
| Connector receives relay from controller at runtime | Track B |

**Connector behavior in Track A:**
Each connector still has a statically configured `RELAY_ADDR`. It connects to that relay at startup and stays there. If the relay dies, the connector retries indefinitely (existing behavior). Track A does not change this.

What Track A *does* fix is the **client side**: clients now route each connector via its actual relay (from `connector_relay_placement`), instead of using a single global relay for all connectors. This is the primary correctness fix for multi-relay deployments.

Full failover (connector automatically moves to a new relay) is a Track B feature.

---

## Proto Field Numbering Note

`ACLConnector` fields 4 and 5 added in this ADR are **transitional**.

Track B (ADR-015) introduces `TransportSnapshot` and removes relay information from `ACLSnapshot`. When Track B ships, fields 4 and 5 in `ACLConnector` must be **reserved** — not reused. Proto field numbers are permanent.

Plan ahead: do not allocate fields 6+ in `ACLConnector` for other purposes before Track B is designed.

---

## Implementation Checklist

- [x] `proto/client/v1/client.proto` — add `relay_addr=4`, `relay_spiffe_id=5` to `ACLConnector`
- [x] `buf generate` — regenerate Go stubs
- [x] `store.go` — extend `RemoteNetworkConnectorsRow`; LEFT JOIN in `GetConnectorsForRemoteNetworks`
- [x] `compiler.go` — populate relay coords on `ACLConnector` in connector loop
- [x] `relay/heartbeat.go` — call `NotifyPolicyChange` when relay metadata changes
- [x] `relay/expiry.go` — background eviction goroutine + notify on expiry
- [x] `client/src/daemon.rs` — update `build_transports_by_resource` signature + per-connector relay context
- [x] Build gate: `cd controller && go build ./...` and `cd client && cargo build`
- [x] Proto regeneration: `buf generate` + `cd admin && npm run codegen`

---

## Follow-up

Track B (ADR-015) begins after this ADR is implemented and deployed.
