---
type: task
status: done
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

- [x] After connector row update, iterate `req.Shields` and call `h.ShieldSvc.UpdateShieldHealth()` for each entry
- [x] `connectorID` from SPIFFE context (already extracted at top of handler)
- [x] Errors logged via `log.Printf` — heartbeat never fails due to shield update errors
- [x] `ShieldSvc shield.Service` added to `EnrollmentHandler` struct in `enrollment.go`

### UpdateShieldHealth helper

- [x] Implemented by M2 in `internal/shield/heartbeat.go` — `UpdateShieldHealth(ctx, shieldID, connectorID, status, version string, lastHeartbeatAt int64) error`
- [x] Added to `shield.Service` interface in `config.go`

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
