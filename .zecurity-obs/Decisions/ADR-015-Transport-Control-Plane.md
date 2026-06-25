# ADR-015: Transport Control Plane

**Status:** Proposed
**Track:** B — Architecture
**Author:** Zecurity Engineering
**Reviewed:** 2026-06-25
**Depends on:** ADR-014 (Relay Stabilization) — Track B begins only after Track A is deployed

---

## Purpose

Redesign the transport subsystem from first principles.

This ADR describes the long-term target architecture only. It does not contain migration details, current implementation fixes, or stabilization patches. Those belong to ADR-014 (Track A).

It answers one question:

> What should the transport architecture look like when it is done?

**This ADR intentionally describes the target architecture, not the implementation order. The rollout order is defined by the Relay Roadmap.**

---

## Starting Point

When Track B begins, the system will be in the state left by ADR-014:

- ACLSnapshot carries per-connector relay coords in `ACLConnector` (fields 4+5) as transitional fields
- `connector_relay_placement` is read by the compiler
- Relay events propagate via `NotifyPolicyChange`
- Connectors still have static `RELAY_ADDR`
- ACLSnapshot still mixes authorization and transport

Track B replaces this transitional state with a clean architecture.

---

## Problem

Authorization and transport are different domains. They currently share one snapshot, one compiler, one cache, and one propagation path.

| Domain | Changes when |
|--------|-------------|
| Authorization | Policy mutation — access rules, group membership, device revocation |
| Transport | Topology change — relay failure, connector registration, placement rebalancing, relay address change |

Coupling them creates unnecessary invalidation: a relay heartbeat recompiles ACL snapshots. A policy update recomputes network topology. Neither should trigger the other.

---

## Shared Topology Vocabulary

Authorization and Transport are independent, but their outputs must be correlatable at the consumer side.

The following identifiers are shared between control planes:

- **Resource ID** — immutable identity of a protected resource
- **Remote Network ID** — immutable identity of a network segment

These are topology references — join keys. Neither plane owns them. An ACL entry carries `remote_network_id` so the consumer can look up the transport path for that network. Everything beyond these identifiers belongs exclusively to one plane.

---

## Decision

Transport becomes a first-class control-plane subsystem.

The controller maintains two completely independent compilation pipelines:

```
                    Controller

        +------------------------------+
        |                              |
        |      Placement Engine         |
        |                              |
        +--------------+---------------+
                       |
               Placement State
                       |
        +--------------+--------------+
        |                             |
        ▼                             ▼

 ACL Compiler                Transport Compiler

        │                             │

        ▼                             ▼

 ACL Snapshot             Transport Snapshot
```

Neither compiler owns the other's responsibilities.

---

## ACL Snapshot (authorization only)

Contains:

- Workspace ID
- ACL entries: resource ID, address, port, protocol, allowed SPIFFE IDs, route type, shield ID, `remote_network_id` (shared join key)
- Version + Epoch

Does **not** contain:

- Relay addresses
- Connector addresses
- Tunnel endpoints

---

## Transport Snapshot (connectivity only)

Contains:

- Remote networks
- Per remote network: active connector list
- Per connector: `connector_id`, `connector_tunnel_addr`, `connector_spiffe`, `relay_addr`, `relay_spiffe_id`
- Transport version + Epoch

Does **not** contain:

- Access rules
- Identities
- Authorization policy

---

## Placement Engine

The Placement Engine is an independent controller subsystem. Its only responsibility is assigning connectors to relays.

It does not know ACLs, policies, users, devices, or resources.

### Distributed Singleton

The Placement Engine must run as a logically-singleton process in a multi-replica controller deployment. This is enforced via a distributed leader lease (Valkey `SET NX` with TTL). Only the lease holder runs placement assignments. The election protocol is specified in ADR-016.

### Atomic Epoch

The Placement Engine increments the Transport Epoch as an atomic unit after completing a full reassignment batch — after all connectors previously on a failed relay have received a new assignment. This prevents the Transport Compiler from reading a partially-migrated placement state.

### Scheduling

The assignment algorithm is intentionally pluggable. Possible strategies: least loaded, geographic affinity, latency, capacity, availability zone. The default algorithm and its contract are specified in ADR-016.

---

## Transport Compiler

The Transport Compiler publishes placement. It does not make placement decisions.

```
Placement State (connector_relay_placement JOIN relays)

↓

Transport Snapshot
```

---

## Connector Architecture

Each connector maintains two independent caches:

```
Connector

├── ACL Cache      (from ConnectorControlMessage.acl_snapshot, field 11)
└── Transport Cache (from ConnectorControlMessage.transport_snapshot, field 16)
```

### Control Stream Delivery

Both snapshots travel over the existing bidirectional `ConnectorControlMessage` stream. A new variant is added:

```protobuf
message TransportSnapshot {
  // Per remote-network transport topology for this connector's workspace.
  repeated TransportRemoteNetwork remote_networks = 1;
  uint64 version                                  = 2;
}

message TransportRemoteNetwork {
  string remote_network_id          = 1;  // shared join key
  repeated TransportConnector connectors = 2;
}

message TransportConnector {
  string connector_id          = 1;
  string connector_tunnel_addr = 2;
  string connector_spiffe      = 3;
  string relay_addr            = 4;
  string relay_spiffe_id       = 5;
}

message ConnectorControlMessage {
  oneof body {
    // ... existing fields 1–15 unchanged ...
    TransportSnapshot transport_snapshot = 16;  // Controller → Connector
  }
}
```

### Direct-Only Fallback

Transport is an enhancement, not a requirement. If a connector has not received a Transport Snapshot, or its assigned relay is unreachable, it operates in direct-only mode. The relay path is enabled transparently when a Transport Snapshot arrives.

This ensures cold start works without a relay assignment, and relay maintenance windows do not require connector restarts.

### Connector No Longer Has Static RELAY_ADDR

`connector/src/config.rs` removes `relay_addr` and `relay_spiffe_id`. The connector's relay assignment comes from the controller via `transport_snapshot` (field 16) on stream open. On receiving a `TransportSnapshot`, the connector compares its current relay connection against the assigned relay. If different, it reconnects.

Bootstrap order:
1. Connector connects to controller (control stream).
2. Controller pushes current `ACLSnapshot` and `TransportSnapshot` on stream open.
3. Connector connects to assigned relay and registers.

---

## Client Architecture

The client maintains two independent caches:

```
Client

├── ACL Cache      (from GetACLSnapshot RPC — unchanged)
└── Transport Cache (from GetTransportSnapshot RPC — new)
```

Runtime flow:

```
User requests resource

↓

ACL Cache → authorized? (uses remote_network_id from ACLEntry as join key)

↓

Transport Cache → keyed by remote_network_id → connector → relay

↓

Establish tunnel
```

### GetTransportSnapshot RPC

A new unary RPC parallel to `GetACLSnapshot`. Same version-check and polling pattern. No new delivery mechanism.

```protobuf
rpc GetTransportSnapshot(GetTransportSnapshotRequest)
    returns (GetTransportSnapshotResponse);
```

### Convergence Window

ACL and Transport snapshots are independently versioned. There will be brief windows where a resource is authorized but has no transport entry (new resource added: ACL updates first, Transport catches up). This causes transient unavailability, not a security issue. The window is bounded by Transport propagation latency. This is an accepted tradeoff.

---

## Propagation

### ACL Propagation (unchanged)

Fan-out: workspace-scoped.
Trigger: policy mutations only.

### Transport Propagation

Fan-out: **topology-scoped** — only connectors affected by the placement change, not all connectors in the workspace.

Triggers:
- Relay heartbeat timeout (relay failure → eviction)
- Relay metadata change (address, IP, port change)
- Connector registration
- Connector reconnect
- Placement Engine reassignment
- Relay comes online

On control stream open: controller pushes both current ACL Snapshot and current Transport Snapshot immediately.

---

## Relay Failover

```
Relay A dies
↓
Heartbeat expires
↓
Controller marks Relay A inactive
↓
Placement Engine acquires leader lease
↓
Compute new placement for all connectors on Relay A
↓
Write all placement rows (full batch)
↓
Transport Epoch++ (atomic with batch completion)
↓
Compile Transport Snapshot
↓
Push to affected connectors
↓
Connectors compare assigned relay vs current
↓
Different → disconnect from A, connect to new relay
↓
Register on new relay
↓
ConnectorRelayState upstream (field 15)
↓
Controller confirms connector_relay_placement
↓
Placement converges
```

Controller is the single source of truth. Connectors never choose relays.

### Convergence Observable

The Placement Engine maintains a `placement_generation` counter, incremented on each completed reassignment batch. Exposed via admin API and metrics. Connectors include `transport_version` in heartbeats, allowing the controller to report "placement_generation=N, connectors_converged=X/Y."

---

## Migration from Track A to Track B

When Track B ships, the following Track A transitional elements are removed:

| Track A element | Track B action |
|----------------|---------------|
| `ACLConnector.relay_addr` (field 4) | Reserve the field number — do not reuse |
| `ACLConnector.relay_spiffe_id` (field 5) | Reserve the field number — do not reuse |
| Snapshot-level `relay_addr` (field 6) | Reserve — already marked deprecated in Track A |
| Snapshot-level `relay_spiffe_id` (field 9) | Reserve — already marked deprecated in Track A |
| `GetActiveRelay()` call in compiler | Remove |
| `client/src/daemon.rs` per-connector relay from `ACLConnector` | Replace with Transport Cache lookup |
| Connector static `RELAY_ADDR` | Remove |

The detailed migration strategy is specified in ADR-018.

---

## Design Principles

- Single source of truth for placement (controller only)
- Independent consistency domains (ACL and Transport never cross-invalidate)
- Shared topology vocabulary (Resource ID, Remote Network ID as join keys)
- Topology-scoped transport fan-out
- Stateless relay layer
- Placement Engine as distributed singleton
- Transport as enhancement, not requirement
- Deterministic reconciliation
- Observable convergence

---

## Out of Scope

This ADR intentionally does not define:
- Transport Snapshot schema in full detail
- Placement algorithm specifics
- Leader election protocol
- Migration rollout steps
- Database schema changes

---

## Follow-up ADRs

| ADR | Topic |
|-----|-------|
| ADR-016 | Placement Engine — leader election, scheduling algorithm, epoch semantics |
| ADR-017 | Transport Propagation — fan-out, triggers, delivery guarantees, convergence SLA |
| ADR-018 | Migration Strategy — phased rollout from Track A state to Track B state |
