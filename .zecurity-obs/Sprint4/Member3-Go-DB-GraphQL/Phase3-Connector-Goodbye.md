---
type: task
status: pending
sprint: 4
member: M3
phase: 3
depends_on:
  - "M2 Phase 1: connector.proto Goodbye RPC added + buf generate done"
unlocks:
  - Graceful Connector shutdown (DISCONNECTED immediately, not after 90s)
tags:
  - go
  - grpc
  - connector
  - goodbye
---

# M3 · Phase 3 — Connector Goodbye RPC

**Depends on: connector.proto updated with Goodbye RPC + buf generate run.**

---

## Goal

Implement the `Goodbye` RPC handler for the Connector service on the Controller. When a Connector sends Goodbye on SIGTERM, the Controller immediately marks it DISCONNECTED rather than waiting 90 seconds for the disconnect watcher.

---

## File to Create

`controller/internal/connector/goodbye.go`

---

## Checklist

### goodbye.go

- [ ] Package `connector`
- [ ] Implement `Goodbye(ctx context.Context, req *pb.GoodbyeRequest) (*pb.GoodbyeResponse, error)`
- [ ] Extract `connectorID` from SPIFFE context (same pattern as `Heartbeat` and `RenewCert`):
  ```go
  connectorID := ctx.Value(spiffeEntityIDKey{}).(string)
  trustDomain := ctx.Value(trustDomainKey{}).(string)
  ```
- [ ] SQL:
  ```sql
  UPDATE connectors
     SET status = 'disconnected', updated_at = NOW()
   WHERE id = $1 AND trust_domain = $2
  ```
- [ ] Return `INTERNAL` on DB error
- [ ] Log: `tracing.Info("connector goodbye", "connector_id", connectorID)`
- [ ] Return `&pb.GoodbyeResponse{Ok: true}, nil` on success
- [ ] Add `Goodbye` to the `EnrollmentHandler` struct (or wherever ConnectorService RPCs live — check enrollment.go for pattern)

### Register Goodbye on gRPC handler

- [ ] Confirm `Goodbye` method is on the same struct as `Enroll`, `Heartbeat`, `RenewCert`
- [ ] `ConnectorServiceServer` interface requires `Goodbye` method — `go build ./...` will enforce this after buf generate

---

## Build Check

```bash
cd controller && go build ./...
# ConnectorServiceServer interface fully implemented (including Goodbye)
```

---

## Notes

- `Goodbye` is **best-effort**: if the Connector crashes without calling it, the disconnect watcher catches it after 90s. No retry needed.
- The `trust_domain` check in the SQL ensures a Connector can only disconnect itself, not other connectors.
- Context key types (`spiffeEntityIDKey{}`, `trustDomainKey{}`) are defined in `internal/connector/spiffe.go` — do not redefine.

---

## Related

- [[Services/Connector]] — update to document Goodbye RPC
- [[Sprint4/Member4-Rust-Shield-CI/Phase7-CI-Connector-Main]] — Connector binary calls Goodbye on SIGTERM
