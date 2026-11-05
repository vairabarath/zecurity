---
type: adr
status: pending
id: PENDING-12
domain: operations
priority: P2
created: 2026-07-03
related:
  - ADR-013-SnapshotCache-Epoch-CAS
  - ADR-016-Controller-Labelled-Connector-Probed-Relay
tags: [pending, adr, operations, ha, availability]
---

# Pending ADR 12 — Controller HA & Multi-Region

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.
> *(Current state below is partly inferred — confirm deployment model before scoping.)*

## Context / Current State

The controller is the control-plane hub (gRPC control streams to connectors/relays, GraphQL to
admin, Postgres + Valkey). Some multi-replica awareness exists in design — the ADR-016 Placement
Engine calls for a **distributed singleton via Valkey `SET NX` lease**, and the snapshot cache has
epoch/CAS (ADR-013) — but there's no documented **HA/DR** story: multi-replica control streams,
failover, Postgres/Valkey HA, or multi-region. A single-controller outage today likely means
connectors/clients keep running on cached ACL snapshots (data plane survives) but **no policy
changes, enrollments, or relay-list updates propagate**.

## Problem — Decision Needed

What availability target do we commit to, and what topology delivers it?

## Options

### Option A — Multi-replica single-region (HA)
N stateless controller replicas behind a load balancer; control streams shard across replicas;
Postgres + Valkey in HA; Placement Engine uses the Valkey lease (already designed).
- **Pros:** removes single point of failure; matches existing design hints. **Cons:** control-stream
  affinity + cross-replica fan-out (BroadcastRelayList/policy notify) must work across replicas.

### Option B — Active-active multi-region
Regional controllers; connectors/relays connect to nearest; global policy replication.
- **Pros:** latency + regional resilience. **Cons:** data replication + consistency complexity;
  much larger effort.

### Option C — Single controller + fast recovery
Accept a single instance with good backups + quick restart; rely on data-plane survivability.
- **Pros:** simplest. **Cons:** control-plane outages block policy/enrollment; not enterprise-grade.

## Recommendation (non-binding)
Target **Option A** for production (the pieces are partly anticipated). Confirm cross-replica
control-stream fan-out early — it's the crux (a policy change on replica 1 must reach a connector
whose stream is on replica 2). Defer B until multi-region demand is real.

## Open Questions
- Current deployment (single instance?) and target SLA/RTO/RPO?
- Does `BroadcastRelayList` / policy-notify already work cross-replica, or assume one process?
- Postgres/Valkey HA topology?

## Rough Effort / Priority
**A: M–L · B: XL**, P2 (rises with customer scale / SLA commitments).
