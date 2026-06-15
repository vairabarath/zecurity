---
type: task
status: planned
sprint: 10.2
member: M3
phase: 3
depends_on:
  - Sprint10.2-M3-Phase2
unlocks: []
---

# M3 Phase 3 — Relay Integration & Security Validation

## Goal

Prove the complete direct-first and Relay fallback behavior under positive and
negative security cases.

## Test Matrix

| Case | Expected Result |
|------|-----------------|
| Direct Connector reachable | Direct path selected |
| Direct Connector unreachable | Relay fallback succeeds |
| Relay wrong SPIFFE | Client rejects outer QUIC |
| Connector wrong SPIFFE | Client rejects inner TLS |
| Client and Connector different workspaces | Relay or inner TLS rejects |
| Relay Lookup unknown Connector | Negative ACK returned |
| Connector ACL denies request | Denial returned; no second fallback |
| Relay disconnects | Pool reconnects on next attempt |
| Concurrent Client streams | One pooled Relay connection, independent Lookup streams |

## Confidentiality Test

Use known plaintext markers in:

- `TunnelRequest`
- application payload

Capture bytes bridged by Relay and assert neither marker appears. Lookup
metadata may remain visible.

## Operational Validation

1. Start Controller with `RELAY_ADDR` and `RELAY_SPIFFE_ID`.
2. Start Relay and provision its certificate.
3. Start Connector and verify registration.
4. Start Client with direct route available and confirm direct path.
5. Block direct Connector route and confirm Relay fallback.
6. Remove block and confirm new traffic returns to direct path.

## Final Build Check

```bash
buf generate
cd controller && go test ./internal/policy/... ./internal/client/... ./internal/connector/... && go build ./...
cd relay && cargo test && cargo build
cd connector && cargo test && cargo build
cd client && cargo test && cargo build
```

