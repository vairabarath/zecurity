---
type: decision
status: accepted
date: 2026-04-29
related:
  - "[[Sprint8/path]]"
  - "[[Services/Controller]]"
tags:
  - adr
  - sprint8
  - policy-engine
  - acl
  - scaling
---

# ADR-001 — ACL Snapshot Caching Strategy

## Context

Sprint 8 introduces group-based access policy:

- Admin creates groups.
- Admin adds users to groups.
- Admin assigns resources to groups.
- Controller compiles this into ACL snapshots.
- Connectors and Clients enforce default-deny from local snapshots.

The Controller must serve snapshots to two consumers:

- Connector heartbeat path.
- Client `GetACLSnapshot` RPC.

Recompiling from DB on every heartbeat or client request would repeatedly walk:

```text
access_rules -> groups -> group_members -> users -> client_devices
```

That query path is correct, but it is not a cheap hot-path lookup.

## Decision

Use an in-memory per-workspace snapshot cache in the Controller for Sprint 8.

```go
type SnapshotCache struct {
    mu      sync.RWMutex
    entries map[string]*ACLSnapshot // workspace_id -> compiled snapshot
}
```

Required methods:

```go
func (c *SnapshotCache) Get(workspaceID string) (*ACLSnapshot, bool)
func (c *SnapshotCache) Set(workspaceID string, snapshot *ACLSnapshot)
func (c *SnapshotCache) Invalidate(workspaceID string)
```

Flow:

```text
policy mutation succeeds
  -> NotifyPolicyChange(workspace_id)
  -> invalidate workspace cache entry

GetACLSnapshot / Connector heartbeat
  -> cache hit: return cached snapshot
  -> cache miss: compile from DB, cache, return
```

## Security Rules

- Compile failure returns no snapshot.
- Do not serve stale snapshots after a failed compile.
- Missing snapshot means downstream default-deny.
- Connector and Client must treat missing/invalid snapshots as deny, not allow.

This prevents an admin removing access, a compile failing, and the system continuing to serve an older snapshot that still grants access.

## Alternatives Considered

### Option A — Recompile on Every Request

Pros:

- Simplest implementation.
- Always freshest data.
- No invalidation logic.

Cons:

- Bad fit for heartbeat loops.
- Causes repeated multi-table policy queries even when no policy changed.
- Does not scale beyond small deployments.

Use only for prototypes or internal tools where snapshot reads are rare.

### Option B — In-Memory Cache Per Workspace

Pros:

- Cheap hot path: map read on heartbeat/client snapshot requests.
- DB compile happens only after policy change or Controller restart.
- Simple implementation.
- No extra migration or snapshot serialization.

Cons:

- Cache is process-local.
- Controller restart makes cache cold.
- Multiple Controller replicas do not share compiled snapshots.

This is the accepted Sprint 8 approach.

### Option C — Store Compiled Snapshots in DB

Pros:

- Survives Controller restarts.
- Shared across multiple Controller replicas.
- Useful when horizontal scaling requires all Controller instances to serve the same compiled snapshot without local recompute.

Cons:

- Requires another migration.
- Requires serialization/versioning logic.
- Adds source-of-truth risk: live policy tables and compiled snapshot table can diverge.
- More operational complexity than Sprint 8 needs.

DB-stored compiled snapshots are useful only when we run multiple Controller replicas or need cross-process snapshot sharing.

## When To Revisit

Revisit Option C when any of these become true:

- Controller runs more than one replica behind a load balancer.
- Connector heartbeats are spread across multiple Controller instances.
- ACL compilation becomes measurably expensive after indexes and query tuning.
- Cold cache rebuild after Controller restart is too slow.
- We need auditable historical policy snapshots.
- We need monotonic snapshot delivery across Controller process restarts.

## Future Horizontal Scale Plan

Best-case path:

```text
single Controller
  -> in-memory cache per workspace
  -> add metrics for compile duration/cache hit rate
  -> keep Option B
```

Worst-case scale path:

```text
multiple Controllers
  -> add compiled_acl_snapshots table
  -> version snapshots by workspace_id + version
  -> mutations update policy version transactionally
  -> one compiler writes the compiled snapshot
  -> all replicas read compiled snapshot by version
```

If this migration happens later, keep raw policy tables as the source of truth:

- `groups`
- `group_members`
- `access_rules`
- `resources`
- `client_devices`

The compiled snapshot table should be treated as a cache/materialized view, not the canonical policy source.

## Consequences

- Sprint 8 remains simple and correct for a single Controller.
- The common heartbeat path avoids unnecessary DB work.
- Restart behavior is acceptable: first request recompiles.
- Horizontal scaling has a documented upgrade path.
