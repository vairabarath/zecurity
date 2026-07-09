# Relay Engineering Roadmap

**Last updated:** 2026-06-25
**Branch:** `integration/relay-merge`

---

## Overview

The relay work is split into two independent tracks with a strict sequencing dependency.

**Track A must ship before Track B begins.**

```
Phase 1 (Complete)
    ACL Propagation
    Epoch/CAS
    Resource ACL Coherence

Phase 2 (Track A — Relay Stabilization)
    Make the existing relay architecture correct

Phase 3 (Track B — Transport Control Plane)
    Redesign transport as a first-class subsystem
```

---

## Why Two Tracks

Authorization and transport are different problems.

Track A fixes correctness in the current design.
Track B replaces the current design with a better one.

Attempting both at once risks shipping neither.

**The one technical tension to keep in mind:**
Track A adds transitional proto fields (`ACLConnector.relay_addr=4`, `ACLConnector.relay_spiffe_id=5`) that Track B will deprecate. These fields are intentionally short-lived. When Track B ships, they are reserved — never reused. Do not build anything in Track B that depends on them being permanent.

---

## Phase 1 — Foundation (Complete)

| Work item | ADR | Status |
|-----------|-----|--------|
| ACL propagation — heartbeat broadcast to all connectors in workspace | ADR-001 | ✅ Done |
| Epoch/CAS for snapshot cache — prevents stale compile insertion race | ADR-013 | ✅ Done |
| Resource ACL coherence — ACL snapshot invalidated on resource mutations | Sprint 8 fix | ✅ Done |

---

## Phase 2 — Track A: Relay Stabilization

**ADR:** ADR-014
**Goal:** Multi-relay deployments route clients to the correct relay per connector. Relay events propagate. Dead relays are evicted.

### Deliverables

| Item | Description | Files |
|------|-------------|-------|
| A1 | Add `relay_addr=4`, `relay_spiffe_id=5` to `ACLConnector` proto (transitional) | `proto/client/v1/client.proto` |
| A2 | Extend `GetConnectorsForRemoteNetworks` — LEFT JOIN `connector_relay_placement` + `relays` | `store.go` |
| A3 | Populate per-connector relay coords in compiler loop | `compiler.go` |
| A4 | Relay heartbeat calls `NotifyPolicyChange` on metadata change | `relay/heartbeat.go` |
| A5 | Background relay expiry eviction — marks inactive + notifies affected workspaces | `relay/expiry.go` (new) |
| A6 | `build_transports_by_resource` reads per-connector relay from `ACLConnector` | `client/src/daemon.rs` |

### What Track A does NOT change

- Connector static `RELAY_ADDR` — still configured at startup
- ConnectorControlMessage — no new variants
- ACLSnapshot structure beyond `ACLConnector` additions
- Independent transport versioning

### Correctness guarantee after Track A

In a healthy multi-relay deployment where each connector is on its configured relay, clients route to the correct relay per connector. Relay failures are detected and snapshots are invalidated. Dead relays are evicted.

Full automatic failover (connector moves to a new relay without restart) requires Track B.

### Build gate

```bash
cd controller && go build ./...
cd client && cargo build
buf generate
```

---

## Phase 3 — Track B: Transport Control Plane

**ADR:** ADR-015 (target architecture)
**Depends on:** Phase 2 complete and deployed

ADR-015 defines the target architecture. This roadmap defines the delivery order.
Each phase below is independently shippable. The team can stop at any phase and still have a working production system.

---

### Phase 3A — Transport Snapshot

**Goal:** Establish `TransportSnapshot` as the delivery channel for transport topology. Both connectors and clients consume it. ACLSnapshot no longer needs to carry relay information after this phase.

**Production state after 3A:** Multi-relay routing is correct, transport and authorization are delivered independently. Connector still has static `RELAY_ADDR`. No automatic failover yet.

| Item | Description | Files |
|------|-------------|-------|
| B1 | `TransportSnapshot` proto message + `transport_snapshot=16` in `ConnectorControlMessage` | `proto/connector/v1/connector.proto` |
| B2 | Transport Compiler — reads `connector_relay_placement` JOIN `relays`, produces `TransportSnapshot` | `policy/transport_compiler.go` (new) |
| B3 | Transport Cache — own Epoch/CAS, own version counter | `policy/transport_cache.go` (new) |
| B4 | `pushTransportSnapshot` in `control_stream.go` — push on stream open + on `NotifyTransportChange` | `connector/control_stream.go` |
| B5 | Transport propagation — topology-scoped fan-out, relay event triggers | `relay/heartbeat.go`, `relay/expiry.go` |
| B6 | `GetTransportSnapshot` RPC — client unary polling endpoint | `proto/client/v1/client.proto` |
| B7 | Client Transport Cache — `build_transports_by_resource` reads from `TransportSnapshot` | `client/src/daemon.rs` |
| B8 | Deprecate transitional `ACLConnector` relay fields (reserve 4, 5) | `proto/client/v1/client.proto` |
| B9 | Remove relay coords from ACLSnapshot (reserve fields 6, 9) | `compiler.go` |

---

### Phase 3B — Runtime Connector Assignment

**Goal:** Remove static `RELAY_ADDR` from the connector. The connector receives its relay assignment from the controller at runtime via `TransportSnapshot` on the control stream.

**Production state after 3B:** Connector configuration no longer requires a hardcoded relay address. Zero-config relay rotation. Connector cold start works via direct-only fallback.

| Item | Description | Files |
|------|-------------|-------|
| B10 | Connector handles `transport_snapshot` (field 16) — compare assigned vs current, reconnect if different | `connector/src/control_stream.rs` |
| B11 | Connector removes static `RELAY_ADDR` and `RELAY_SPIFFE_ID` from config | `connector/src/config.rs`, `connector/src/main.rs` |
| B12 | Direct-only fallback — connector operates without relay until `TransportSnapshot` arrives | `connector/src/relay_client.rs` |

---

### Phase 3C — Placement Engine

**Goal:** Controller proactively detects dead relays and reassigns their connectors. First time true automatic failover is possible without manual intervention.

**Production state after 3C:** A relay can fail and all affected connectors are automatically reassigned to healthy relays. No operator action required.

| Item | Description | Files |
|------|-------------|-------|
| B13 | Placement Engine — leader election via Valkey `SET NX`, dead relay detection, connector reassignment | `relay/placement_engine.go` (new) |
| B14 | Atomic Transport Epoch increment on batch completion | `relay/placement_engine.go` |
| B15 | Notify affected connectors on reassignment — topology-scoped push | `connector/control_stream.go` |

---

### Phase 3D — Automatic Failover Validation

**Goal:** End-to-end failover is tested, observable, and has a defined convergence SLA.

**Production state after 3D:** Operators can observe failover progress. Convergence time is bounded and measurable.

| Item | Description | Files |
|------|-------------|-------|
| B16 | `placement_generation` counter — incremented on each completed reassignment batch | `relay/placement_engine.go` |
| B17 | Convergence status exposed via admin API — "placement_generation=N, converged=X/Y" | `graph/relay.graphqls` (new resolver) |
| B18 | Connector `transport_version` reported in heartbeat | `proto/connector/v1/connector.proto` |

---

### Phase 3E — Scheduling Policies

**Goal:** Pluggable placement algorithms beyond least-loaded.

**Production state after 3E:** Operators can configure placement strategy per workspace or globally.

| Item | Description |
|------|-------------|
| B19 | Scheduling interface — pluggable algorithm (ADR-016) |
| B20 | Geographic affinity strategy |
| B21 | Capacity-weighted strategy |
| B22 | Administrative policy overrides |

ADR-016 defines the scheduling contract. Phase 3E depends on 3C.

---

## Decision Log

| Date | Decision | Rationale |
|------|----------|-----------|
| 2026-06-25 | Separate stabilization (Track A) from redesign (Track B) | Risk reduction — ship correctness fast, don't block on architecture |
| 2026-06-25 | Track A adds transitional proto fields; Track B deprecates them | Minimizes Track A scope without blocking Track B design |
| 2026-06-25 | Connector static RELAY_ADDR stays in Track A | Fixing it requires TransportSnapshot + control stream changes — Track B scope |
| 2026-06-25 | Transport fan-out is topology-scoped (Track B), not workspace-scoped | Relay failures affect specific connectors, not entire workspaces |
| 2026-06-25 | Placement Engine is a distributed singleton via Valkey lease | Prevents concurrent reassignment races in multi-replica controller |
| 2026-06-25 | Transport is enhancement, not requirement — direct-only fallback | Connector cold start must not require a relay assignment |
