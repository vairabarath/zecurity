---
type: task
status: done
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

- [x] Package `connector`
- [x] Implement `Goodbye(ctx context.Context, req *pb.GoodbyeRequest) (*pb.GoodbyeResponse, error)`
- [x] Extract `connectorID`, `trustDomain`, `role` from SPIFFE context using `SPIFFEEntityIDFromContext`, `TrustDomainFromContext`, `SPIFFERoleFromContext`
- [x] SQL: `UPDATE connectors SET status='disconnected', updated_at=NOW() WHERE id=$1 AND trust_domain=$2`
- [x] Return `INTERNAL` on DB error
- [x] Log: `log.Printf("connector goodbye: connector_id=%s trust_domain=%s", ...)`
- [x] Return `&pb.GoodbyeResponse{Ok: true}, nil` on success
- [x] `Goodbye` added as method on `EnrollmentHandler` (same struct as `Enroll`, `Heartbeat`, `RenewCert`)

### Register Goodbye on gRPC handler

- [x] `Goodbye` on `EnrollmentHandler` — `go build ./...` enforces interface compliance

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
