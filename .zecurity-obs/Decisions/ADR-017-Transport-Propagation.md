# ADR-017: Transport Propagation

**Status:** Proposed
**Track:** B — Architecture
**Author:** Zecurity Engineering
**Reviewed:** 2026-06-26
**Depends on:** ADR-015 (Transport Control Plane), ADR-016 (Tiered Relay Selection)

---

## Purpose

Define the propagation pipeline for `TransportSnapshot` — how topology changes
reach connectors and clients, independently of ACL policy propagation.

---

## Problem

In Track A, every topology event (relay heartbeat, relay expiry, connector
registration) calls `NotifyPolicyChange`, which recompiles the ACL snapshot and
pushes it to all connectors in the workspace. This coupling has two costs:

1. **Wasted recompilation.** A relay address change has nothing to do with access
   rules. Recompiling and pushing ACL snapshots to every connector in the
   workspace on every relay heartbeat metadata change is unnecessary work.

2. **Wrong invalidation scope.** ACL invalidation is workspace-scoped — every
   connector gets a new snapshot. Transport invalidation is topology-scoped —
   only connectors affected by the placement change need a new snapshot. In a
   workspace with 50 connectors, a single relay failure should push to only the
   connectors registered on that relay, not all 50.

---

## Decision

Introduce a `TransportNotifier` parallel to the existing `policy.Notifier`.
Transport and ACL propagation are fully independent pipelines sharing no state.

```
Topology event
    ↓
TransportNotifier.NotifyTopologyChange(affectedConnectorIDs []string)
    ↓
TransportCache.Invalidate(connectorID)   ← per-connector, not per-workspace
    ↓
TransportCompiler.Compile(workspaceID)   ← one snapshot per workspace
    ↓
Push to affected connectors via ConnectorControlMessage field 16
```

ACL events continue to flow through `policy.Notifier` unchanged.

---

## Transport Notifier

```go
// package transport

// Notifier tracks transport topology versions and drives proactive push.
// Keyed by connectorID — topology changes are scoped to affected connectors,
// not broadcast to the entire workspace.
type Notifier struct {
    cache    *SnapshotCache
    mu       sync.Mutex
    versions map[string]*atomic.Uint64 // connectorID → version

    // pushHook is fired after NotifyTopologyChange, non-blocking by contract.
    // Receives the workspaceID so the async worker can compile + push the
    // workspace TransportSnapshot. Mirrors policy.Notifier.RegisterPushHook.
    pushHook func(workspaceID string)
}

func NewNotifier(cache *SnapshotCache) *Notifier

// NotifyTopologyChange increments the version for each affected connector,
// invalidates their cached slots, then fires pushHook(workspaceID) once.
// workspaceID is used only for the push hook — the cache is keyed by connector.
func (n *Notifier) NotifyTopologyChange(ctx context.Context, workspaceID string, affectedConnectorIDs []string) error

func (n *Notifier) RegisterPushHook(fn func(workspaceID string))

// Version returns the current transport version for connectorID (0 if never changed).
func (n *Notifier) Version(connectorID string) uint64
```

---

## Trigger Table

| Event | Source | Affected scope | Notification |
|-------|--------|---------------|-------------|
| Relay comes online (first heartbeat) | `relay/heartbeat.go` | All connectors placed on this relay | `NotifyTopologyChange(workspaceID, connectorIDs)` |
| Relay metadata changes (IP/address) | `relay/heartbeat.go` | All connectors placed on this relay | `NotifyTopologyChange(workspaceID, connectorIDs)` |
| Relay heartbeat expires (eviction) | `relay/expiry.go` | All connectors placed on that relay | `NotifyTopologyChange(workspaceID, connectorIDs)` |
| Connector registers with relay | `connector/control_stream.go` | That connector only | `NotifyTopologyChange(workspaceID, []string{connectorID})` |
| Connector reconnects (stream re-open) | `connector/control_stream.go` | That connector only | push current snapshot on stream open (no version bump needed) |
| Connector self-selects new relay (ADR-016) | `connector/control_stream.go` — on `ConnectorRelayState` received | That connector only | `NotifyTopologyChange(workspaceID, []string{connectorID})` |

**Note:** Connector reconnect does not bump the transport version — it just pushes the
current snapshot on stream open. A version bump is only warranted when placement
state actually changes.

---

## Transport Compiler

```go
// package transport

// CompileTransportSnapshot builds a TransportSnapshot for the given workspace.
// Reads connector_relay_placement JOIN relays JOIN connectors.
// Returns error on any DB failure — callers must not cache a partial result.
func CompileTransportSnapshot(
    ctx context.Context,
    store *Store,
    notifier *Notifier,
    workspaceID string,
) (*connectorv1.TransportSnapshot, error)
```

The compiler reads:

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
             THEN host(r.observed_ip) || ':9093'
           ELSE ''
         END, ''
       ),
       COALESCE(r.id::text, '')
  FROM connectors c
  LEFT JOIN connector_relay_placement crp ON crp.connector_id = c.id
  LEFT JOIN relays r ON r.id = crp.relay_id AND r.status = 'active'
 WHERE c.workspace_id = $1
   AND c.status = 'active'
 ORDER BY c.remote_network_id, c.last_heartbeat_at DESC NULLS LAST
```

This is the same JOIN as `GetConnectorsForRemoteNetworks` in the ACL compiler,
but workspace-scoped rather than remote-network-scoped.

---

## Transport Snapshot Cache

Parallel to `policy.SnapshotCache`. Keyed by `workspaceID` (one snapshot per
workspace, shared across all connectors in it — connectors filter client-side by
remote network).

```go
// package transport

type SnapshotCache struct {
    mu    sync.RWMutex
    items map[string]*cacheEntry // workspaceID → entry
}

func (c *SnapshotCache) Get(workspaceID string) *connectorv1.TransportSnapshot
func (c *SnapshotCache) Set(workspaceID string, snap *connectorv1.TransportSnapshot)
func (c *SnapshotCache) Invalidate(workspaceID string)
```

---

## Control Stream Delivery (Connector)

On stream open, the controller immediately pushes the current `TransportSnapshot`
alongside the `ACLSnapshot` (same stream, field 16 on `ConnectorControlMessage`):

```go
// In control_stream.go — on new stream accepted:
func (h *Handler) handleStream(stream connector_v1.ConnectorService_ConnectServer) error {
    // existing: push ACLSnapshot (field 11)
    // new: push TransportSnapshot (field 16)
    snap, err := h.transportCompiler.GetOrCompile(ctx, workspaceID)
    if err != nil {
        return err
    }
    return stream.Send(&connectorv1.ConnectorControlMessage{
        Body: &connectorv1.ConnectorControlMessage_TransportSnapshot{
            TransportSnapshot: snap,
        },
    })
}
```

The proactive push hook fires `NotifyTopologyChange` → pushes to all currently
connected streams for the affected connectors. The push mechanism mirrors
`acl_push.go` — a registry of live streams keyed by connectorID.

---

## GetTransportSnapshot RPC (Client)

New unary RPC in `proto/client/v1/client.proto`, parallel to `GetACLSnapshot`:

```protobuf
rpc GetTransportSnapshot(GetTransportSnapshotRequest)
    returns (GetTransportSnapshotResponse);

message GetTransportSnapshotRequest {
  string access_token = 1;
  string device_id    = 2;
  uint64 known_version = 3; // client's current version; 0 = always return
}

message GetTransportSnapshotResponse {
  TransportSnapshot snapshot = 1;
  bool              up_to_date = 2; // true if known_version == current; snapshot omitted
}
```

Client polls on the same TTL as ACL (60s). If `up_to_date` is true, the client
skips deserialization and retains its cached `TransportSnapshot`. The client daemon
maintains a separate `transport_snapshot` field in `SharedState`, populated via a
`fetch_and_store_transport` background task mirroring `fetch_and_store_acl`.

---

## Convergence SLA

| Metric | Target |
|--------|--------|
| Relay metadata change → connector receives new TransportSnapshot | < 2s |
| Relay expiry (90s heartbeat gap) → snapshot recompiled | < 95s total |
| Client polling interval | 60s (same as ACL) |
| Max convergence window (client) | 120s (2 × poll interval) |

**Observable:** Each connector includes `transport_version` in its heartbeat. The controller exposes a metric:
`transport_convergence{workspace=X}` = fraction of connected connectors whose
reported `transport_version` equals the current compiled version.

---

## What This ADR Does NOT Define

- Relay selection algorithm, capacity labelling, or probe logic (ADR-016)
- Migration from Track A ACLConnector relay fields to TransportSnapshot (ADR-018)
- Full proto schema for `TransportSnapshot` (defined in ADR-015 and the proto files)

---

## Implementation Checklist

- [ ] `proto/connector/v1/connector.proto` — add `TransportSnapshot`, `TransportRemoteNetwork`, `TransportConnector` messages; add field 16 to `ConnectorControlMessage`
- [ ] `proto/client/v1/client.proto` — add `GetTransportSnapshot` RPC + request/response messages
- [ ] `buf generate` — regenerate Go stubs
- [ ] `controller/internal/transport/` — new package: `Notifier`, `SnapshotCache`, `Store`, `CompileTransportSnapshot`
- [ ] `relay/heartbeat.go` — replace `NotifyPolicyChange` calls with `TransportNotifier.NotifyTopologyChange` (query affected connectors from `connector_relay_placement`)
- [ ] `relay/expiry.go` — replace `NotifyPolicyChange` calls with `TransportNotifier.NotifyTopologyChange`
- [ ] `connector/control_stream.go` — push `TransportSnapshot` on stream open; register stream in transport push registry
- [ ] `controller/internal/connector/acl_push.go` — add parallel `transport_push.go` for topology-scoped push
- [ ] `controller/graph/resolvers/` — wire `GetTransportSnapshot` resolver
- [ ] `client/src/daemon.rs` — add `transport_snapshot` to `SharedState`; add `fetch_and_store_transport`; join ACL + Transport on `remote_network_id` in `build_transports_by_resource`
- [ ] Build gate: `cd controller && go build ./...` and `cd client && cargo build`
- [ ] Unit tests: `TransportNotifier`, `SnapshotCache`, `CompileTransportSnapshot`
- [ ] Integration test: topology change → correct connectors notified, correct snapshot version

---

## Follow-up

| ADR | Topic |
|-----|-------|
| ADR-018 | Migration Strategy — remove ACLConnector relay fields, connector drops static RELAY_ADDR |
