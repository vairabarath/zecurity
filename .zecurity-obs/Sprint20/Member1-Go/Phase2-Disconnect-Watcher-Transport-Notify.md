---
type: phase
member: M1
person: Sathiya
sprint: 20
phase: 2
execution: D
title: Disconnect Watcher → Transport Plane
status: planned
depends_on: [1]
tags:
  - go
  - connector
  - transport
  - adr-017
  - provider-dashboard
---

# Phase 2 (D) — Disconnect Watcher → Transport Plane

> Depends on Phase 1 (B): same package, M1 sequential. Decision Record prerequisite #6 (Q15).

## Problem (verified)

- `connector.RunDisconnectWatcher(ctx, pool, cfg, notifier PolicyChangeNotifier)` (`internal/connector/disconnect_watcher.go:14-40`) marks `active` connectors with `last_heartbeat_at` older than `CONNECTOR_DISCONNECT_THRESHOLD` (90 s) as `disconnected`.
- It then calls only `NotifyPolicyChange` per workspace. Wiring is at `cmd/server/main.go:~565`, which passes only `policyNotifier`.
- `markDisconnected` returns only `tenant_id` values (`RETURNING tenant_id::text`), not connector IDs.
- The transport snapshot (`internal/transport`, served by `ClientService.GetTransportSnapshot`) is never invalidated for these connectors. Clients keep trying the dead connector's coordinates until another topology event happens: fail-slow, per ADR-017.

Every other connector state transition already notifies both planes: stream open/close
(`control_stream.go`), health-report status change, `RevokeConnector`, and `Goodbye` after Phase B.

## Goal

A connector marked `disconnected` by the watcher is removed from the next `TransportSnapshot`
without waiting for any other event.

## Files

| File | Change |
|------|--------|
| `controller/internal/connector/disconnect_watcher.go` | `markDisconnected` returns `map[workspaceID][]connectorID`; watcher takes a topology notifier |
| `controller/cmd/server/main.go` (~565) | Pass `transportNotifier` |
| `controller/internal/connector/disconnect_watcher_test.go` (new) | DB-backed tests |

## Steps

### D1 — `markDisconnected`

```sql
UPDATE connectors
   SET status = 'disconnected', updated_at = NOW()
 WHERE status = 'active'
   AND last_heartbeat_at < NOW() - $1::interval
   AND tenant_id IN (SELECT id FROM workspaces WHERE status = 'active')   -- UNCHANGED (see I3)
RETURNING tenant_id::text, id::text;
```

Return `map[string][]string` (workspace → connector IDs), de-duplicated per workspace.

### D2 — `RunDisconnectWatcher`

```go
// TopologyChangeNotifier mirrors the transport-plane notifier used by
// control_stream.go (TransportNotifier) and relay.RunExpiryLoop.
func RunDisconnectWatcher(ctx context.Context, pool *pgxpool.Pool, cfg Config,
    policy PolicyChangeNotifier, topology TopologyChangeNotifier)
```

For each workspace with at least one transition:
1. `policy.NotifyPolicyChange(ctx, ws)` (existing behaviour; connector availability is an ACL input).
2. `topology.NotifyTopologyChange(ctx, ws, connectorIDs)` (new).

Both are nil-safe, and an error from one does not skip the other. Reuse the existing notifier
interface type from `control_stream.go` if one is already declared; don't declare a duplicate.

### D3 — wiring

```go
connector.RunDisconnectWatcher(ctx, db.Pool, connectorCfg, policyNotifier, transportNotifier)
```

## Invariants

- **I1** Every connector transition `active → disconnected` notifies **both** planes, whichever path causes it (stream close, Goodbye, watcher).
- **I2** Topology notifications carry the exact affected connector IDs (targeted, as elsewhere).
- **I3** The `workspaces.status = 'active'` predicate is **unchanged**. Suspension (D-07/D-08) will revisit it in its own sprint; don't pre-empt that here.
- **I4** Shield disconnect watcher (`shield/heartbeat.go` `RunDisconnectWatcher`) is unchanged. Shields are not in the transport snapshot.
- **I5** Revoked connectors are never touched (`WHERE status = 'active'`), consistent with Phase B.

## Tests (DB-backed, `ENROLLMENT_TEST_DATABASE_URL`)

- Two stale `active` connectors in workspace W1, one in W2, one fresh in W1 → the three stale ones become `disconnected`; topology fake receives `(W1, [c1,c2])` and `(W2, [c3])`; policy fake receives W1 and W2 once each.
- `revoked` connector with old heartbeat → untouched, no notification.
- Connector in a non-active workspace → untouched (I3 regression guard).
- Nil topology notifier → no panic; policy still notified.
- Topology notifier returns an error → policy notification still happens, and vice versa.

## Acceptance Criteria

- [ ] Kill a connector process (no clean shutdown). Within ~90–120 s it is `disconnected`, and the next client `GetTransportSnapshot` returns a new version without that connector's coordinates, with no other change in the workspace.
- [ ] Tests pass (not skipped).

## Build Check

```bash
cd controller && go build ./...
cd controller && go test ./internal/connector/...
```

## Implementation Checklist

- [ ] **M1-D1** `markDisconnected` returns workspace → connector IDs
- [ ] **M1-D2** `RunDisconnectWatcher` notifies the topology plane
- [ ] **M1-D3** `main.go` wiring
- [ ] **M1-D4** Tests
- [ ] **Build gate:** `cd controller && go build ./...`

## Post-Phase Fixes

_None yet._
