---
type: task
status: planned
sprint: 10.2
member: M2
phase: 1
depends_on: []
unlocks:
  - Sprint10.2-M3-Phase1
---

# M2 Phase 1 — ACL Relay Discovery Contract

## Goal

Provide the Client with every address and exact identity required to attempt a
secure Relay fallback.

## Files

- `proto/client/v1/client.proto`
- `controller/internal/policy/compiler.go`
- `controller/internal/client/service.go`
- `controller/internal/connector/enrollment.go`
- `controller/internal/connector/control_stream.go`
- `controller/cmd/server/main.go`
- Generated Go protobuf files

## Contract

Append fields without changing existing field numbers:

```proto
string relay_addr       = 6;
string connector_id     = 7;
string connector_spiffe = 8;
string relay_spiffe_id  = 9;
```

`relay_spiffe_id` is required. A Relay address without an exact expected Relay
identity is not sufficient for secure TLS verification.

## Controller Behavior

1. Read optional `RELAY_ADDR` and `RELAY_SPIFFE_ID`.
2. Require both together or leave Relay discovery disabled.
3. Query the newest active Connector for:
   - `lan_addr`
   - `id`
   - `spiffe_id`
4. Populate the same discovery fields in snapshots returned to Clients and
   snapshots pushed to Connectors.
5. Invalidate or rebuild cached snapshots when Relay configuration changes on
   process restart.

## Tests

- Relay fields are empty when Relay is disabled.
- Configured Relay address and SPIFFE ID are returned together.
- Active Connector ID and exact SPIFFE are populated.
- Missing active Connector leaves Connector identity fields empty.
- Existing ACL entries and direct Connector address remain unchanged.

## Build Check

```bash
buf generate
cd controller
go test ./internal/policy/... ./internal/client/... ./internal/connector/...
go build ./...
```

