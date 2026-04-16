---
type: task
status: pending
sprint: 4
member: M3
phase: 4
depends_on:
  - "M2 Phase 1: connector.proto ShieldHealth message + shields field in HeartbeatRequest"
  - "buf generate done"
  - "M2 Phase 2: shieldSvc.UpdateShieldHealth() method exists"
unlocks:
  - Controller updates Shield status from Connector heartbeats
tags:
  - go
  - grpc
  - connector
  - shield-health
---

# M3 · Phase 4 — Connector Heartbeat: ShieldHealth Processing

**Depends on: buf generate done (HeartbeatRequest has shields field), shieldSvc exists.**

---

## Goal

Modify the Connector's `Heartbeat` handler on the Controller to process the `shields` repeated field. The Connector now reports which Shields are alive; the Controller updates their `last_heartbeat_at` in the DB.

---

## File to Modify

`controller/internal/connector/heartbeat.go`

---

## Checklist

### Heartbeat handler modification

- [ ] After the existing connector row update (`UPDATE connectors SET ...`), add Shield health processing:
  ```go
  for _, sh := range req.Shields {
      s.db.Exec(ctx, `
          UPDATE shields
             SET last_heartbeat_at = to_timestamp($1),
                 status            = $2,
                 version           = $3,
                 updated_at        = NOW()
           WHERE id           = $4
             AND connector_id = $5
      `, sh.LastHeartbeatAt, sh.Status, sh.Version, sh.ShieldId, connectorID)
  }
  ```
- [ ] `connectorID` comes from SPIFFE context (already extracted at top of handler)
- [ ] Log shield health updates at `debug` level (avoid noisy info logs)
- [ ] Do not fail the heartbeat if a shield update fails — log error and continue

### UpdateShieldHealth helper (if used)

- [ ] If shieldSvc.UpdateShieldHealth() is the pattern, implement it in `internal/shield/heartbeat.go`:
  ```go
  func (s *service) UpdateShieldHealth(ctx context.Context, shieldID, status, version string, lastHeartbeatAt int64, connectorTenantID string) error
  ```
- [ ] The connector's Heartbeat handler calls this for each `sh` in `req.Shields`

---

## Build Check

```bash
cd controller && go build ./...
# Heartbeat handler compiles with ShieldHealth field access
```

---

## Notes

- `to_timestamp($1)` converts Unix timestamp (int64) to TIMESTAMPTZ in PostgreSQL.
- The Shield's `status` field in `ShieldHealth` is a string: `"active"` or `"disconnected"`. The DB CHECK constraint validates it.
- The Connector's disconnect watcher in `internal/connector/heartbeat.go` marks Connectors disconnected — the Shield disconnect watcher (in `internal/shield/heartbeat.go`) marks Shields disconnected. These are separate goroutines.
- **Do not modify** the existing disconnect watcher logic for Connectors in this file.

---

## Related

- [[Sprint4/Member3-Go-DB-GraphQL/Phase5-AgentServer-Rust]] — the Connector aggregates Shield health from agent_server.rs
- [[Sprint4/Member4-Rust-Shield-CI/Phase4-Heartbeat-Renewal]] — Shield heartbeat sends to Connector :9091
