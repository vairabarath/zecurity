# ADR-018: Track A → Track B Migration Strategy

**Status:** Proposed
**Track:** B — Architecture (migration phase)
**Author:** Zecurity Engineering
**Reviewed:** 2026-06-26
**Depends on:** ADR-015 (Transport Control Plane), ADR-016 (Tiered Relay Selection), ADR-017 (Transport Propagation)

---

## Purpose

Define the exact sequence for migrating from the Track A stabilization state (ADR-014) to the Track B target architecture (ADR-015) without a flag day or forced client upgrade.

This ADR does **not** redesign anything. It specifies:
- What ships in what order
- How old clients survive during the transition
- What gets removed and when
- What proto field numbers become reserved forever

---

## Starting Point — Track A State

| Element | Location | State |
|---------|----------|-------|
| `ACLConnector.relay_addr` | `client.proto` field 4 | Active — per-connector relay addr |
| `ACLConnector.relay_spiffe_id` | `client.proto` field 5 | Active — per-connector relay SPIFFE |
| `ACLSnapshot.relay_addr` | `client.proto` field 6 | Deprecated — workspace-level fallback |
| `ACLSnapshot.relay_spiffe_id` | `client.proto` field 9 | Deprecated — workspace-level fallback |
| Connector `RELAY_ADDR` env var | `connector/src/config.rs:77` | **Removed in ADR-016/Sprint 11** — replaced by `LabelledRelayList` (field 17) dynamic selection |
| `build_transports_by_resource` | `client/src/daemon.rs:727` | Reads relay from `ACLConnector` fields 4+5 |
| `GetActiveRelay()` in compiler | `controller/internal/policy/compiler.go:160` | Active — populates deprecated fields 6+9 |

---

## Target State — Track B

| Element | Location | State |
|---------|----------|-------|
| `ACLConnector` fields 4+5 | `client.proto` | **Reserved** — never reused |
| `ACLSnapshot` fields 6+9 | `client.proto` | **Reserved** — never reused |
| `TransportSnapshot` | `connector.proto` field 16 + client RPC | New — publishes resulting per-connector relay coords from `connector_relay_placement` |
| `GetTransportSnapshot` RPC | `client.proto` | New — client polls separate transport cache |
| Connector `RELAY_ADDR` | `connector/src/config.rs` | **Removed in ADR-016/Sprint 11** — relay selected via `LabelledRelayList` (field 17), not `TransportSnapshot` |
| `GetActiveRelay()` | `compiler.go` | **Removed** — transport compiler owns relay data |

---

## Migration Phases

### Phase 1 — Add Transport Snapshot (non-breaking)

Ship the new proto messages and RPC. Nothing is removed. Old clients continue to work on ACL snapshot relay fields.

**Changes:**
- Add to `connector.proto`:
  ```protobuf
  message TransportSnapshot { ... }            // field 16 on ConnectorControlMessage
  message TransportRemoteNetwork { ... }
  message TransportConnector { ... }
  ```
- Add to `client.proto`:
  ```protobuf
  rpc GetTransportSnapshot(GetTransportSnapshotRequest)
      returns (GetTransportSnapshotResponse);
  message TransportSnapshot { ... }
  ```
- Build Transport Compiler in controller
- Controller pushes `TransportSnapshot` on control stream open alongside `ACLSnapshot`
- Note: Placement Engine (old ADR-016) is superseded. ADR-016 now implements tiered relay selection via `LabelledRelayList` (field 17) — connector self-selects within the controller-approved pool, then reports `ConnectorRelayState`. `TransportSnapshot` (field 16 and the client RPC) carries the resulting relay topology; it is not an input to connector relay selection.
- Controller serves `GetTransportSnapshot` RPC

**Client behavior:** Old clients ignore `GetTransportSnapshot`. New clients begin populating Transport Cache from it but **still fall back to ACLConnector fields 4+5** if Transport Cache is empty (convergence window safety).

**Gate:** All 57 existing tests pass + new Transport Compiler tests pass.

---

### Phase 2 — Connector Dynamic Relay Selection *(delivered by ADR-016 / Sprint 11)*

> **This phase is now fully handled by ADR-016 (Tiered Relay Selection), not by TransportSnapshot.**
>
> ADR-016 introduces `LabelledRelayList` (field 17 on `ConnectorControlMessage`). The connector receives a capacity-labelled relay pool from the controller, instantly connects to a random Tier 1 relay, then background-probes and migrates via make-before-break. `RELAY_ADDR` is removed from connector config in Sprint 11.
>
> `TransportSnapshot` (field 16 and the client RPC) is **not** the vehicle for connector relay assignment — it publishes the resulting connector→relay topology after ADR-016 selection. Clients consume it in Phase 3.

**Changes delivered by Sprint 11 (ADR-016):**
- `connector/src/config.rs` — `RELAY_ADDR` / `RELAY_SPIFFE_ID` removed entirely
- `connector/src/control_stream.rs` — handle `LabelledRelayList` body variant (field 17)
- `connector/src/relay_selector.rs` — three-phase state machine (instant startup, background optimization, make-before-break)
- Direct-only mode if no `LabelledRelayList` received and no persisted ranking available

**Gate:** *(completed in Sprint 11)* Connector selects relay dynamically from controller-pushed list. `RELAY_ADDR` env var no longer required.

---

### Phase 3 — Client Switches to Transport Cache (non-breaking)

Client's `build_transports_by_resource` reads relay from Transport Cache (keyed by `remote_network_id`) instead of `ACLConnector` fields 4+5.

**Changes:**
- `client/src/daemon.rs` — add `TransportCache` struct; `fetch_and_store_acl` also calls `GetTransportSnapshot`
- `build_transports_by_resource` — join ACL entries → Transport Cache on `remote_network_id` to resolve relay, not `ACLConnector.relay_addr`
- ACL snapshot relay fields (4+5) are no longer read — but still present in proto (removal is Phase 4)

**Compatibility:** If `GetTransportSnapshot` returns empty (old controller), client falls back to reading `ACLConnector` fields 4+5. Backward compat preserved.

**Gate:** Client uses Transport Cache for relay routing. Removing ACLConnector relay fields from a test snapshot causes no regression.

---

### Phase 4 — Remove Track A Transitional Elements (breaking — coordinated deploy)

Remove the deprecated relay fields from the ACL pipeline and reserve their proto numbers.

**Requires:** Phases 1–3 fully deployed. All clients updated to Phase 3+. All connectors updated to Phase 2+. Compatibility window (see below) elapsed.

**Changes:**

| File | Change |
|------|--------|
| `proto/client/v1/client.proto` | Reserve `ACLConnector` fields 4+5; reserve `ACLSnapshot` fields 6+9 |
| `controller/internal/policy/compiler.go:158–174` | Remove `GetActiveRelay()` block; remove `RelayAddr`/`RelaySpiffeId` from returned `ACLSnapshot` |
| `controller/internal/policy/store.go` | `GetActiveRelay()` can be removed if no other callers |
| `client/src/daemon.rs` | Remove ACLConnector fields 4+5 fallback path in `build_transports_by_resource` |
| `connector/src/config.rs` | Remove `relay_addr` field entirely |

**buf generate** must be run after proto changes. All downstream codegen (Go stubs, admin TS hooks) must be regenerated.

---

## Proto Changes — Exact Reserved Statements

### `proto/client/v1/client.proto`

**ACLConnector** — after Phase 4:
```protobuf
message ACLConnector {
  string connector_id          = 1;
  string connector_tunnel_addr = 2;
  string connector_spiffe      = 3;
  reserved 4;                       // was: relay_addr (Track A — ADR-014)
  reserved "relay_addr";
  reserved 5;                       // was: relay_spiffe_id (Track A — ADR-014)
  reserved "relay_spiffe_id";
}
```

**ACLSnapshot** — after Phase 4:
```protobuf
message ACLSnapshot {
  uint64 version                            = 1;
  string workspace_id                       = 2;
  int64  generated_at                       = 3;
  repeated ACLEntry entries                 = 4;
  reserved 5;                               // was: connector_tunnel_addr
  reserved "connector_tunnel_addr";
  reserved 6;                               // was: relay_addr (Track A — ADR-014)
  reserved "relay_addr";
  reserved 7;                               // was: connector_id
  reserved "connector_id";
  reserved 8;                               // was: connector_spiffe
  reserved "connector_spiffe";
  reserved 9;                               // was: relay_spiffe_id (Track A — ADR-014)
  reserved "relay_spiffe_id";
  repeated ACLRemoteNetwork remote_networks = 10;
}
```

### `proto/connector/v1/connector.proto`

**ConnectorControlMessage** — Phase 1 addition:
```protobuf
oneof body {
  // ... existing fields 1–15 unchanged ...
  TransportSnapshot transport_snapshot = 16;  // Controller → Connector topology propagation, not relay selection
}
```

---

## Removal Table

| Track A element | File | Phase | Action |
|----------------|------|-------|--------|
| `ACLConnector.relay_addr` field 4 | `client.proto` | 4 | Reserve field 4 + name |
| `ACLConnector.relay_spiffe_id` field 5 | `client.proto` | 4 | Reserve field 5 + name |
| `ACLSnapshot.relay_addr` field 6 | `client.proto` | 4 | Reserve field 6 + name |
| `ACLSnapshot.relay_spiffe_id` field 9 | `client.proto` | 4 | Reserve field 9 + name |
| `GetActiveRelay()` call in compiler | `compiler.go:160` | 4 | Delete lines 158–174 |
| `RelayAddr`/`RelaySpiffeId` on ACLSnapshot | `compiler.go:176–184` | 4 | Remove from return struct |
| `GetActiveRelay()` method | `store.go` | 4 | Delete if no other callers |
| `relay_addr` on `ConnectorConfig` | `connector/src/config.rs` | ~~4~~ **2 ✓** | Removed in Sprint 11 (ADR-016) — no longer present |
| Relay fallback in `build_transports_by_resource` | `client/src/daemon.rs:762–781` | 4 | Replace with Transport Cache lookup |

---

## Rollback Strategy

Each phase is independently rollbackable until Phase 4.

| Phase | Rollback |
|-------|---------|
| 1 | Redeploy controller without Transport Compiler — clients and connectors ignore unknown fields |
| 2 | *(delivered by ADR-016)* Redeploy connector binary without `relay_selector` — falls back to direct-only mode. `RELAY_ADDR` env var no longer available as fallback (removed in Sprint 11). |
| 3 | Redeploy client binary — reads ACLConnector fields 4+5 again (still populated until Phase 4) |
| 4 | **Cannot rollback** — reserved proto fields cannot be un-reserved. Roll forward only. Requires coordinated deploy with tested forward path. |

Phase 4 is the point of no return. It must not ship until Phase 3 client adoption reaches 100% of the fleet (or a hard cutover date is set).

---

## Compatibility Window

Old clients (Track A, reading `ACLConnector` fields 4+5) are supported until Phase 4 ships.

The controller continues to populate `ACLConnector` fields 4+5 (via the Track A compiler path) until Phase 4 removes it. Old clients remain fully functional.

Minimum compatibility window: **4 weeks after Phase 3 client release** before Phase 4 can ship. This gives time for auto-update propagation and manual fleet upgrades.

New clients (Phase 3+) that receive an old controller (without `GetTransportSnapshot`) fall back to reading ACLConnector fields 4+5 — no breakage.

---

## Implementation Checklist

### Phase 1 — Transport Snapshot Proto + Compiler
- [ ] Add `TransportSnapshot`, `TransportRemoteNetwork`, `TransportConnector` to `connector.proto` (field 16 on `ConnectorControlMessage`)
- [ ] Add `GetTransportSnapshot` RPC + request/response messages to `client.proto`
- [ ] `buf generate` — regenerate Go stubs
- [ ] Build Transport Compiler in `controller/internal/transport/compiler.go`
- [ ] Build `GetTransportSnapshot` handler in controller client service
- [ ] Controller pushes `TransportSnapshot` on control stream open (alongside ACLSnapshot)
- [ ] Build gate: `cd controller && go build ./...` passes

### Phase 2 — Connector Dynamic Relay Selection *(delivered by ADR-016 / Sprint 11)*
- [x] `connector/src/relay_selector.rs` (new) — three-phase state machine: instant startup, background optimization, make-before-break migration
- [x] `connector/src/relay_probe.rs` (new) — parallel QUIC probe, scoring, `request_id` + SPIFFE validation
- [x] `connector/src/relay_ranking.rs` (new) — persisted top-5 ranking, staleness check, atomic write
- [x] `connector/src/control_stream.rs` — handle `LabelledRelayList` body variant (field 17)
- [x] `connector/src/config.rs` — remove `RELAY_ADDR` / `RELAY_SPIFFE_ID`; add `RELAY_*` config vars
- [x] Build gate: `cd connector && cargo build` passes

### Phase 3 — Client Transport Cache
- [ ] `client/src/daemon.rs` — add `TransportCache`; fetch via `GetTransportSnapshot` alongside ACL
- [ ] `build_transports_by_resource` — resolve relay from Transport Cache on `remote_network_id`; fall back to ACLConnector fields 4+5 if cache empty
- [ ] Build gate: `cd client && cargo build` passes

### Phase 4 — Remove Track A Elements (coordinated deploy)
- [ ] Reserve `ACLConnector` fields 4+5 in `client.proto` (exact statements above)
- [ ] Reserve `ACLSnapshot` fields 6+9 in `client.proto` (exact statements above)
- [ ] `buf generate` — regenerate all stubs
- [ ] Remove `GetActiveRelay()` block from `compiler.go` lines 158–174
- [ ] Remove `GetActiveRelay()` from `store.go` (verify no other callers)
- [ ] Remove ACLConnector fallback from `client/src/daemon.rs`
- [ ] Remove `relay_addr` from `connector/src/config.rs`
- [ ] `cd admin && npm run codegen`
- [ ] Full build gate: all four components clean
- [ ] All existing tests pass with relay fields removed from ACL pipeline

---

## Follow-up

| ADR | Topic |
|-----|-------|
| ADR-016 | Tiered Relay Selection — capacity labelling, connector self-selection, make-before-break migration |
| ADR-017 | Transport Propagation — fan-out, triggers, delivery guarantees, convergence SLA |
