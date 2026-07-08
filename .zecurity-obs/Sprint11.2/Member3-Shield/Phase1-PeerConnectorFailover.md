---
type: phase
member: M3
sprint: 11.2
phase: 2
title: Shield — Peer Connector Failover
status: completed
commit: 5c8f65c
depends_on: []
---

# Phase 2 — Shield: Peer Connector Failover

## Goal

Allow a Shield to fail over to a sibling Connector on the same Remote Network
when its primary Connector goes down. The Connector piggybacks a
`PeerConnectorList` to each Shield after every `ShieldHealthReport`, built from
the current ACL snapshot. The Shield stores this list and dials a peer on
reconnect if its primary is unreachable.

## Files

| File | Change |
|---|---|
| `proto/shield/v1/shield.proto` | Add `PeerConnectorList` and `PeerConnector` messages; add field 14 to `ShieldControlMessage` |
| `connector/src/agent_server.rs` | `ShieldRegistry` gets `policy_cache`; add `build_peer_connector_list()`; add `derive_grpc_addr()` |
| `connector/src/policy/mod.rs` | Add `PolicyCache::peers_of_connector()`; tests |
| `connector/src/main.rs` | Wire `policy_cache` into `ShieldRegistry::new()` |
| `shield/src/control_stream.rs` | Handle `PeerConnectorList` message; store peers; failover logic |
| `shield/src/types.rs` | Peer connector state storage and reconnect handling |
| `shield/src/renewal.rs` | Failover attempt on renewal failure |
| `shield/src/enrollment.rs` | Peer list plumbing on initial enrollment |
| `shield/src/main.rs` | Wire peer connector state |

## Protocol Change

```protobuf
// proto/shield/v1/shield.proto

// Connector → Shield — piggybacked after every ShieldHealthReport.
PeerConnectorList peer_connector_list = 14;

message PeerConnectorList {
  repeated PeerConnector peers = 1;
}

message PeerConnector {
  string connector_id   = 1;
  string connector_addr = 2; // host:9091 (Shield-facing gRPC)
}
```

## Key Design Details

### Address derivation

ACL snapshot stores `connector_tunnel_addr` as `host:9092` (QUIC data plane).
Shield needs `host:9091` (gRPC). `derive_grpc_addr()` strips the last port
and appends `:9091`. IPv6 bracketed forms (`[::1]:9092`) handled via `rfind(':')`.

```rust
pub(crate) fn derive_grpc_addr(tunnel_addr: &str) -> String {
    if let Some(idx) = tunnel_addr.rfind(':') {
        format!("{}:9091", &tunnel_addr[..idx])
    } else {
        format!("{tunnel_addr}:9091")
    }
}
```

### Piggybacking trigger

Connector calls `build_peer_connector_list()` after every `ShieldHealthReport`
it processes. If the list is non-empty, it appends `PeerConnectorList` to the
outgoing `ShieldControlMessage`. Empty list is not sent (Shield keeps its
current list as-is).

### `PolicyCache::peers_of_connector`

```rust
pub fn peers_of_connector(&self, connector_id: &str) -> Vec<(String, String)>
// Returns (connector_id, connector_tunnel_addr) for all connectors in the
// same Remote Network as connector_id, including itself.
// Returns empty Vec if snapshot missing or connector not in any RN.
```

### Shield failover flow

```
Shield ── health report ──► Connector
Connector ◄── current ACL snapshot
Connector ──► PeerConnectorList { peers: [C2, C3] } ──► Shield
Shield stores peer list

(Primary connector goes down)
Shield ──► tries peer C2 at host:9091
Shield ──► on success, re-enrolls / re-attaches to C2
```

## Implementation Checklist

- [x] **M3-E1** `proto/shield/v1/shield.proto` — add `PeerConnectorList`, `PeerConnector` messages; field 14 on `ShieldControlMessage`
- [x] **M3-E2** `connector/src/policy/mod.rs` — `PolicyCache::peers_of_connector()` with full test coverage (same-RN peers, connector not found, missing snapshot)
- [x] **M3-E3** `connector/src/agent_server.rs` — `ShieldRegistry` carries `policy_cache: Arc<PolicyCache>`; `build_peer_connector_list()` builds sorted peer list; `derive_grpc_addr()` converts `:9092` → `:9091`
- [x] **M3-E4** `connector/src/agent_server.rs` — piggyback `PeerConnectorList` on `ShieldControlMessage` after every `ShieldHealthReport`
- [x] **M3-E5** `connector/src/main.rs` — pass `policy_cache` to `ShieldRegistry::new()`
- [x] **M3-E6** `shield/src/types.rs` — peer connector state: store list, track current peer, reconnect state
- [x] **M3-E7** `shield/src/control_stream.rs` — handle incoming `PeerConnectorList`; update stored peers; trigger failover on primary disconnect
- [x] **M3-E8** `shield/src/renewal.rs` — attempt peer connector on renewal/reconnect failure
- [x] **M3-E9** `shield/src/enrollment.rs` + `shield/src/main.rs` — wire peer connector plumbing through enrollment and startup
- [x] **Tests:** `derive_grpc_addr` — IPv4, hostname, IPv6 bracketed, no-port cases; `peers_of_connector` — same-RN, not-found, empty-snapshot
- [x] **Build gate:** `cd connector && cargo build` and `cd shield && cargo build` pass
