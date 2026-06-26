# ADR-016: Placement Engine

**Status:** Proposed
**Track:** B — Architecture
**Author:** Zecurity Engineering
**Reviewed:** 2026-06-26
**Depends on:** ADR-015 (Transport Control Plane)

---

## Purpose

> How does the controller decide which relay a connector is assigned to, and how is that decision propagated safely in a multi-replica deployment?

---

## Problem

In Track A (ADR-014), connectors learn their relay from a static `RELAY_ADDR` environment variable set at deploy time. The controller has no mechanism to:

- Assign a connector to a relay dynamically
- Reassign a connector when its relay fails
- Balance load across multiple relays
- Push a new relay assignment to a running connector

When a relay dies, Gap 3 (`expiry.go`) evicts it and recompiles ACL snapshots — but the connector keeps trying the dead relay indefinitely. Clients receive an empty `relay_addr` for that connector and fall back to direct-only, which fails if the client is on a different network segment.

This ADR defines the Placement Engine: the subsystem that owns relay assignment decisions and propagates them to connectors via the Transport Snapshot.

---

## Decision

A **Placement Engine** runs inside the controller as a background goroutine. It is the single source of truth for connector→relay assignment.

- It runs as a **distributed singleton** in multi-replica deployments (leader lease via Valkey).
- It **does not** know about ACLs, policies, users, devices, or resources.
- It reads from `relays` and `connectors` tables; writes to `connector_relay_placement`.
- It increments a **Transport Epoch** atomically after each completed reassignment batch.
- The Transport Compiler (ADR-017) reads placement state and publishes `TransportSnapshot`.

---

## Leader Lease

### Valkey Key

```
ztna:placement:leader
```

Value: controller replica ID (UUID generated at startup, stored in memory).

### Protocol

```
SET ztna:placement:leader <replica_id> NX PX 15000
```

- TTL: **15 seconds**
- Renewal interval: **5 seconds** (⅓ of TTL — safe against one missed tick)
- On renewal failure (Valkey unavailable): log error, do not run assignment loop this tick
- On lease loss (value changed under us): stop assignment loop immediately, re-enter election

### Failover

If the lease holder crashes, the TTL expires in ≤15 seconds. Any replica that wins the next `SET NX` becomes the new leader. No manual intervention required.

### Go Interface

```go
// LeaderLease is implemented by the Valkey-backed lease.
type LeaderLease interface {
    // TryAcquire attempts SET NX. Returns true if this replica holds the lease.
    TryAcquire(ctx context.Context) (bool, error)
    // Renew refreshes the TTL. Returns false if the lease was lost.
    Renew(ctx context.Context) (bool, error)
    // Release deletes the key if it still holds our value (Lua CAS delete).
    Release(ctx context.Context) error
}
```

Valkey key name: `ztna:placement:leader`
Replica ID key: `ztna:placement:leader:id:{replica_id}` (for debugging — not used in election logic)

---

## Assignment Algorithm

### Interface

```go
// Scheduler assigns unplaced or displaced connectors to relays.
type Scheduler interface {
    // Assign returns a relay ID for the given connector, or "" if no relay is available.
    Assign(ctx context.Context, connector AssignableConnector, relays []AvailableRelay) (relayID string, err error)
}

type AssignableConnector struct {
    ID             string
    RemoteNetworkID string
    WorkspaceID    string
}

type AvailableRelay struct {
    ID           string
    PublicAddr   string
    ObservedIP   string
    AddressScope string
    ConnectorCount int // current load
}
```

### Default: LeastLoaded

```go
type LeastLoadedScheduler struct{}

func (s *LeastLoadedScheduler) Assign(_ context.Context, _ AssignableConnector, relays []AvailableRelay) (string, error) {
    if len(relays) == 0 {
        return "", nil
    }
    best := relays[0]
    for _, r := range relays[1:] {
        if r.ConnectorCount < best.ConnectorCount {
            best = r
        }
    }
    return best.ID, nil
}
```

Load = number of active `connector_relay_placement` rows per relay. Queried fresh each assignment batch.

Future schedulers (geographic affinity, latency, capacity) implement the same interface and are selected via controller config.

---

## Epoch Semantics

### Purpose

Prevents the Transport Compiler from reading a partially-migrated placement state. If relay A fails and 10 connectors need reassignment, the epoch must not increment until all 10 rows are written.

### Storage

New column on the `relays` table is insufficient — epoch is workspace-scoped and cross-relay. Use a dedicated table:

```sql
CREATE TABLE IF NOT EXISTS transport_epoch (
    workspace_id  UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    epoch         BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### Increment Protocol

```sql
-- Called once after all placement rows for a batch are written.
INSERT INTO transport_epoch (workspace_id, epoch, updated_at)
VALUES ($1, 1, NOW())
ON CONFLICT (workspace_id) DO UPDATE
    SET epoch = transport_epoch.epoch + 1,
        updated_at = NOW()
RETURNING epoch;
```

The epoch increment is a single serialized write per workspace per batch. The Transport Compiler reads this value and embeds it in `TransportSnapshot.version`.

### Placement Generation Counter

```sql
CREATE TABLE IF NOT EXISTS placement_generation (
    id         SERIAL PRIMARY KEY,
    generation BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- Single row, id=1.
```

Incremented (also atomically) after each completed batch across all workspaces. Exposed via admin API for observability.

---

## Database

### Modified: `connector_relay_placement`

Add `placed_at` and `placement_generation` columns if not present:

```sql
ALTER TABLE connector_relay_placement
    ADD COLUMN IF NOT EXISTS placed_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN IF NOT EXISTS placement_generation BIGINT NOT NULL DEFAULT 0;
```

### New: `transport_epoch`

See Epoch Semantics above.

### New: `placement_generation`

See Epoch Semantics above.

### Query: Unplaced or Displaced Connectors

```sql
-- Connectors with no placement row, or whose placed relay is now inactive.
SELECT c.id::text, c.remote_network_id::text, w.id::text AS workspace_id
  FROM connectors c
  JOIN remote_networks rn ON rn.id = c.remote_network_id
  JOIN workspaces w ON w.id = rn.workspace_id
  LEFT JOIN connector_relay_placement crp ON crp.connector_id = c.id
  LEFT JOIN relays r ON r.id = crp.relay_id AND r.status = 'active'
 WHERE c.status = 'active'
   AND (crp.connector_id IS NULL OR r.id IS NULL)
 ORDER BY w.id, c.id;
```

### Query: Available Relays with Load

```sql
SELECT r.id::text,
       COALESCE(r.public_addr, ''),
       COALESCE(host(r.observed_ip), ''),
       COALESCE(r.address_scope, ''),
       COUNT(crp.connector_id) AS connector_count
  FROM relays r
  LEFT JOIN connector_relay_placement crp ON crp.relay_id = r.id
 WHERE r.status = 'active'
 GROUP BY r.id
 ORDER BY connector_count ASC;
```

---

## Placement Engine Loop

```go
func (e *Engine) Run(ctx context.Context) {
    ticker := time.NewTicker(e.interval) // default: 30s
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            ok, err := e.lease.TryAcquire(ctx)
            if err != nil || !ok {
                continue // not leader this tick
            }
            if err := e.runBatch(ctx); err != nil {
                e.log.Error("placement batch failed", "err", err)
            }
        }
    }
}

func (e *Engine) runBatch(ctx context.Context) error {
    connectors, err := e.store.UnplacedConnectors(ctx)
    if err != nil { return err }
    if len(connectors) == 0 { return nil }

    relays, err := e.store.AvailableRelays(ctx)
    if err != nil { return err }

    // Group by workspace for epoch increment.
    byWorkspace := groupByWorkspace(connectors)
    for wsID, wsCons := range byWorkspace {
        for _, c := range wsCons {
            relayID, err := e.scheduler.Assign(ctx, c, relays)
            if err != nil { return err }
            if relayID == "" { continue } // no relay available
            if err := e.store.UpsertPlacement(ctx, c.ID, relayID); err != nil {
                return err
            }
        }
        if _, err := e.store.IncrementEpoch(ctx, wsID); err != nil {
            return err
        }
        e.notifier.NotifyTransportChange(wsID) // triggers Transport Compiler
    }
    return e.store.IncrementPlacementGeneration(ctx)
}
```

---

## Transport Compiler Integration

The Transport Compiler (specified in ADR-017) reads:

```sql
SELECT c.id::text, c.remote_network_id::text,
       COALESCE(c.lan_addr, ''),
       COALESCE(c.trust_domain, ''),
       COALESCE(
         CASE
           WHEN r.public_addr IS NOT NULL AND r.public_addr != '' THEN r.public_addr
           WHEN r.address_scope = 'public' AND r.observed_ip IS NOT NULL
             THEN host(r.observed_ip) || ':9093'
           ELSE ''
         END, ''
       ),
       COALESCE(r.id::text, '')
  FROM connectors c
  LEFT JOIN connector_relay_placement crp ON crp.connector_id = c.id
  LEFT JOIN relays r ON r.id = crp.relay_id AND r.status = 'active'
 WHERE c.remote_network_id = ANY($1::uuid[])
   AND c.status = 'active';
```

This is the same LEFT JOIN as ACL Gap 1 (`store.go`) — the Transport Compiler reuses this query pattern but publishes into `TransportSnapshot` instead of `ACLConnector`.

The Transport Epoch from `transport_epoch` is embedded as `TransportSnapshot.version`.

---

## Observability

### Admin API

```
GET /api/internal/placement
→ { "placement_generation": 42, "leader_replica_id": "abc-123", "unplaced_connectors": 0 }

GET /api/internal/placement/connectors/{connector_id}
→ { "connector_id": "...", "relay_id": "...", "placed_at": "...", "placement_generation": 42 }
```

### Metrics (Prometheus)

```
ztna_placement_generation_total          — counter, incremented per batch
ztna_placement_unplaced_connectors       — gauge, connectors with no active relay
ztna_placement_batch_duration_seconds    — histogram
ztna_placement_leader_lease_held         — gauge (0 or 1), per replica
```

### Convergence Check

A connector is **converged** when its `connector_relay_placement.placement_generation` matches `placement_generation.generation`. Exposed as `ztna_placement_converged_connectors` gauge.

---

## Implementation Checklist

- [ ] `controller/internal/placement/lease.go` — Valkey leader lease (TryAcquire, Renew, Release)
- [ ] `controller/internal/placement/scheduler.go` — `Scheduler` interface + `LeastLoadedScheduler`
- [ ] `controller/internal/placement/store.go` — `UnplacedConnectors`, `AvailableRelays`, `UpsertPlacement`, `IncrementEpoch`, `IncrementPlacementGeneration`
- [ ] `controller/internal/placement/engine.go` — `Engine.Run`, `runBatch`
- [ ] DB migration — `transport_epoch` table, `placement_generation` table, `connector_relay_placement` new columns
- [ ] `controller/cmd/server/main.go` — wire `Engine.Run` as goroutine alongside expiry loop
- [ ] `controller/internal/placement/engine_test.go` — unit tests: no relays available, single relay, multi-relay load balancing, epoch increment, batch idempotency
- [ ] `controller/internal/placement/lease_test.go` — lease acquisition, renewal, loss detection
- [ ] Admin API handler `GET /api/internal/placement`
- [ ] Prometheus metrics registration
- [ ] Build gate: `cd controller && go build ./...` passes
- [ ] Build gate: `cd controller && go test ./internal/placement/...` passes

---

## Follow-up ADRs

| ADR | Topic |
|-----|-------|
| ADR-017 | Transport Propagation — Transport Compiler, `TransportSnapshot` proto, `GetTransportSnapshot` RPC, control stream delivery, convergence SLA |
| ADR-018 | Migration Strategy — phased rollout from Track A state (static `RELAY_ADDR`) to Track B state (controller-pushed assignment) |
