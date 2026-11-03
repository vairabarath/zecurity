---
type: phase
member: M3
sprint: 13
phase: 1
title: Transport Proto + Transport Compiler + GetTransportSnapshot RPC
status: done
depends_on: []
tags: [go, proto, transport, compiler, acl-decoupling, pending-03]
---

# Phase 1 — Transport plane data path (non-breaking)

> Executes **ADR-018 Phase 1** + the compiler/RPC parts of **ADR-017**.
> Day-1 start — no dependencies. Everything here is additive; nothing is removed.

## Goal

Stand up the **read/serve** half of the transport plane: a `TransportSnapshot` message, a controller
`Transport Compiler` that builds it per workspace, a `GetTransportSnapshot` poll RPC for the client,
and a proactive push on connector stream-open. After this phase the data *exists and is served* — it
is not yet consumed by the client (Phase 3) and topology events don't yet drive it (Phase 2).

## Why (context)

Relay coordinates currently ride inside the ACL snapshot (`compiler.go:118-119` per-connector,
`:163` snapshot-level via `GetActiveRelay()`). This phase creates the *parallel* place for them to
live so that, later, a relay change updates transport **without** touching authorization. The join
key that lets the client correlate the two planes — `remote_network_id` — already exists in
`ACLEntry` (field 9) and `ACLRemoteNetwork` (field 1); the Transport Snapshot is keyed on the same
identifier.

## Files

| File | Change |
|------|--------|
| `proto/connector/v1/connector.proto` | add `TransportSnapshot` / `TransportRemoteNetwork` / `TransportConnector`; set `TransportSnapshot transport_snapshot = 16` in `ConnectorControlMessage` (slot already reserved at line ~97) |
| `proto/client/v1/client.proto` | add `rpc GetTransportSnapshot` + request/response messages |
| `controller/internal/transport/store.go` | `Store` — the `connector_relay_placement JOIN relays JOIN connectors` read (ADR-017 SQL) |
| `controller/internal/transport/cache.go` | `SnapshotCache` keyed by `workspaceID` |
| `controller/internal/transport/compiler.go` | `CompileTransportSnapshot(ctx, store, notifier, workspaceID)` |
| `controller/internal/transport/service.go` (or resolver) | `GetTransportSnapshot` handler — version-check like `GetACLSnapshot` |
| `controller/internal/connector/control_stream.go` | push `TransportSnapshot` on stream open, next to the ACL push |
| `controller/cmd/server/main.go` | construct + wire the transport service/cache |

## Proto (exact shapes — from ADR-015/017)

`connector.proto` (add messages; do **not** renumber anything):
```protobuf
message TransportSnapshot {
  repeated TransportRemoteNetwork remote_networks = 1;
  uint64 version                                  = 2;
}
message TransportRemoteNetwork {
  string remote_network_id               = 1; // shared join key with ACLEntry.remote_network_id
  repeated TransportConnector connectors = 2;
}
message TransportConnector {
  string connector_id          = 1;
  string connector_tunnel_addr = 2;
  string connector_spiffe      = 3;
  string relay_addr            = 4;
  string relay_spiffe_id       = 5;
}
// in ConnectorControlMessage oneof body { ... field 16 already reserved for this ... }
TransportSnapshot transport_snapshot = 16;
```

`client.proto` (mirror `GetACLSnapshot`):
```protobuf
rpc GetTransportSnapshot(GetTransportSnapshotRequest) returns (GetTransportSnapshotResponse);

message GetTransportSnapshotRequest {
  string access_token = 1;
  string device_id    = 2;
  uint64 known_version = 3; // 0 = always return
}
message GetTransportSnapshotResponse {
  TransportSnapshot snapshot = 1;
  bool              up_to_date = 2; // true when known_version == current; snapshot omitted
}
```

Then: `buf generate` (repo root) → `cd controller && go build ./...`.

## Compiler (ADR-017 has the authoritative SQL)

`CompileTransportSnapshot` reads `connector_relay_placement JOIN relays (status='active') JOIN
connectors`, workspace-scoped, and groups connectors by `remote_network_id`. It is the **same JOIN**
the ACL compiler uses for `GetConnectorsForRemoteNetworks`, just workspace-scoped instead of
remote-network-scoped. Return an error on any DB failure — never cache a partial result. Reuse the
relay-address resolution logic that already exists in `policy/compiler.go:195`
(`resolveConnectorRelayAddr`) so public/observed-IP handling stays identical across both planes.

## Push-on-open

In `control_stream.go`, on a newly accepted connector stream, send the current `TransportSnapshot`
(field 16) right after the existing ACL snapshot push. Get-or-compile from the cache.

## Do NOT

- Do not remove or stop populating `ACLConnector` 4+5 or `ACLSnapshot` 6+9 — that's the deferred
  Phase 4.
- Do not rewire relay heartbeat/expiry triggers yet — that's Phase 2.
- Do not make the connector *select* a relay from this snapshot — relay selection is ADR-016
  (`LabelledRelayList`, field 17). This snapshot only *publishes* the resulting topology.

## Build check

```bash
buf generate
cd controller && go build ./...
cd controller && go test ./internal/transport/...
```

## Implementation checklist

- [x] **A1 (architecture-corrected)** transport messages are owned by `client.proto`; connector control-message field 16 is retired and reserved because connectors do not consume client routing topology
- [x] **A2** proto: `GetTransportSnapshot` RPC
- [x] **A3** generated Go/Rust bindings are present and the Go build is green
- [x] **A4** `internal/transport`: store + cache + compiler
- [x] **A5** `GetTransportSnapshot` handler (`known_version` / `up_to_date` version check)
- [x] **A6 (architecture-corrected)** snapshots are served to the sole consumer through the client polling RPC; connector stream-open push was intentionally removed
- [x] **Build gate:** `cd controller && go build ./...`

## Post-Phase Fixes

### Fix: Transport delivery moved from connector push to client poll

**Issue:** The original phase plan placed `TransportSnapshot` on connector control-message field 16
and pushed it when a connector stream opened, although the client—not the connector—is the routing
topology consumer.

**Fix applied:** Commit `90803e4` removed the unused connector push path, reserved field 16, and
made `client.v1.ClientService/GetTransportSnapshot` the delivery path. The compiler, cache, store,
and version-aware handler remain implemented under `controller/internal/transport/`.

**Verification:** `go build ./...` and the focused controller transport/policy/connector/relay tests
pass. Cache and notifier unit coverage exists; the database compiler integration test still requires
`TEST_DATABASE_URL`, and the cross-plane AT-CORE integration test remains outstanding.
