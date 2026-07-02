---
type: planning
status: completed
sprint: 11.2
tags:
  - sprint11.2
  - client
  - connector
  - controller
  - multi-connector-failover
  - framed-tunnel-handshake
  - preferred-connector
  - shield-peer-failover
---

# Sprint 11.2 — Multi-Connector Failover & Framed Tunnel Handshake

> **Status: COMPLETED**
> All items shipped in commits `5c8f65c`, `f88600c`, and `8f9235f`.

---

## Sprint Goal

Two related improvements to client-to-connector tunnel reliability:

1. **Multiple transports per resource** — the client builds one `ClientTransport`
   per connector in the remote network (not just the first). Resources get a
   prioritised list: preferred connector first, rest in declaration order.

2. **Multi-connector failover** — when opening a tunnel stream, the client
   tries each transport in order. If connector N is unreachable, it falls
   through to connector N+1 transparently — no error surfaced to the user.

3. **Framed tunnel handshake** — both client and connector switch from raw
   JSON writes to a 4-byte length-prefixed frame format, consistent with the
   relay protocol. Prevents partial-read bugs on slow or congested links.

4. **`preferred_connector_id` in ACLEntry** — shield routes can now declare
   a preferred connector. The client places that connector first in the
   transport list, falling back to other connectors automatically.

---

## Dependency Graph

```text
Phase A — Proto: preferred_connector_id field (M2)
  ↓
Phase B — Controller: populate preferred_connector_id in ACL compiler (M2)
  ↓
Phase C — Client: multiple transports + failover + framed handshake (M1)
  ↓
Phase D — Connector: framed tunnel handshake (M3)

Phase E — Shield: peer connector failover (M3) [independent]
```

---

## Execution Path

### Phase A — M2: Proto Change

> See [[Sprint11.2/Member2-Go/Phase1-Proto]].

- [x] **M2-A1** `proto/client/v1/client.proto` — add `preferred_connector_id = 10` to `AclEntry`
- [x] **M2-A2** `buf generate` — regenerate Go stubs
- [x] **Build gate:** `cd controller && go build ./...`

### Phase B — M2: Controller ACL Compiler

> Depends on Phase A. See [[Sprint11.2/Member2-Go/Phase2-Compiler]].

- [x] **M2-B1** `controller/internal/policy/compiler.go` — populate `preferred_connector_id` on `AclEntry` for shield routes
- [x] **M2-B2** `controller/internal/policy/store.go` — query updates to supply preferred connector
- [x] **M2-B3** `controller/internal/shield/heartbeat.go` — track the Shield's current Connector so the ACL compiler can emit `preferred_connector_id`
- [x] **M2-B4** `controller/internal/connector/control_stream.go` — plumb preferred connector data through control stream
- [x] **Build gate:** `cd controller && go build ./...`

### Phase C — M1: Client Multiple Transports & Failover

> Depends on Phase A + B. See [[Sprint11.2/Member1-Client/Phase1-MultiTransport]].

- [x] **M1-C1** `client/src/daemon.rs` — `build_transports_by_resource` returns `HashMap<(Ipv4Addr, u16), Option<Vec<Arc<ClientTransport>>>>` (list, not single)
- [x] **M1-C2** `client/src/daemon.rs` — add `ordered_connectors_for_entry()`: preferred connector first, rest in declaration order
- [x] **M1-C3** `client/src/daemon.rs` — per-entry transport cache keyed by connector address; avoids rebuilding TLS pool for the same connector used by multiple resources
- [x] **M1-C4** `client/src/net_stack.rs` — `relay_tcp_to_quic` accepts `Vec<Arc<ClientTransport>>`; iterates in order, continues to next on connection failure
- [x] **M1-C5** `client/src/net_stack.rs` — add `write_framed_json` / `read_framed_json`: 4-byte big-endian length prefix + JSON body; max 16 KB
- [x] **M1-C6** `client/src/net_stack.rs` — use `write_framed_json` for tunnel handshake send
- [x] **Build gate:** `cd client && cargo build`

### Phase D — M3: Connector Framed Tunnel Handshake

> Depends on Phase C. See [[Sprint11.2/Member3-Rust/Phase1-FramedHandshake]].

- [x] **M3-D1** `connector/src/device_tunnel.rs` — replace raw JSON read with `read_framed_json`: 4-byte length prefix + JSON body; max 16 KB (raised from 4 KB)
- [x] **M3-D2** `connector/src/device_tunnel.rs` — replace raw JSON write (`send_response`) with `write_framed_json`
- [x] **M3-D3** `connector/src/device_tunnel.rs` — add `write_framed_json` / `read_framed_json` helpers (mirrors client-side implementation)
- [x] **M3-D4** `connector/src/policy/mod.rs` — supporting change for preferred connector routing
- [x] **Build gate:** `cd connector && cargo build`

### Phase E — M3: Shield Peer Connector Failover

> Independent of Phases A–D. See [[Sprint11.2/Member3-Shield/Phase1-PeerConnectorFailover]].

- [x] **M3-E1** `proto/shield/v1/shield.proto` — add `PeerConnectorList`, `PeerConnector` messages; field 14 on `ShieldControlMessage`
- [x] **M3-E2** `connector/src/policy/mod.rs` — `PolicyCache::peers_of_connector()` with tests
- [x] **M3-E3** `connector/src/agent_server.rs` — `ShieldRegistry` carries `policy_cache`; `build_peer_connector_list()`; `derive_grpc_addr()` converts `:9092` → `:9091`
- [x] **M3-E4** `connector/src/agent_server.rs` — piggyback `PeerConnectorList` on control message after every `ShieldHealthReport`
- [x] **M3-E5** `connector/src/main.rs` — pass `policy_cache` into `ShieldRegistry::new()`
- [x] **M3-E6** `shield/src/types.rs` — peer connector state storage and reconnect handling
- [x] **M3-E7** `shield/src/control_stream.rs` — handle `PeerConnectorList`; trigger failover on primary disconnect
- [x] **M3-E8** `shield/src/renewal.rs` — attempt peer connector on renewal/reconnect failure
- [x] **M3-E9** `shield/src/enrollment.rs` + `shield/src/main.rs` — wire peer list through startup
- [x] **Build gate:** `cd connector && cargo build` and `cd shield && cargo build`

---

## Final Build Gates

- [x] `cd controller && go build ./...`
- [x] `cd client && cargo build`
- [x] `cd connector && cargo build`
- [x] `cd shield && cargo build`

---

## Acceptance Criteria

- [x] Client builds a transport list per resource; preferred connector first.
- [x] If preferred connector is unreachable, client falls through to next connector — no user-visible error.
- [x] Client and connector use identical 4-byte-length-prefixed framing for tunnel handshake.
- [x] `preferred_connector_id` populated by controller for shield routes.
- [x] `MAX_TUNNEL_HANDSHAKE_SIZE` is 16 KB on both sides (was 4 KB connector-side).
- [x] Shield receives `PeerConnectorList` after every health report; fails over to sibling connector on primary disconnect.
- [x] `derive_grpc_addr` correctly converts `:9092` tunnel addr to `:9091` gRPC addr for IPv4, hostname, and IPv6.

---

## Commits

| Commit | Date | Description |
|---|---|---|
| `5c8f65c` | 2026-07-01 | feat(shield): add peer connector failover |
| `f88600c` | 2026-07-02 | client: support multiple transports per resource |
| `8f9235f` | 2026-07-02 | feat: add multi-connector failover and framed tunnel handshake |
