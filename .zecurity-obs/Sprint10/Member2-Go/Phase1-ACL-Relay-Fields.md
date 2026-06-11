---
type: phase
sprint: 10
member: M2
phase: 1
status: planned
---

# M2 Phase 1 — ACL Snapshot Relay Fields

## What You're Building

Extend `ACLSnapshot` with three new fields so clients know how to reach the relay and authenticate the relay-bridged connector connection. Extend the controller to populate these from env config.

## Files to Touch

| File | Change |
|------|--------|
| `proto/client/v1/client.proto` | Add fields 6–8 to `ACLSnapshot` |
| `controller/internal/policy/compiler.go` | Populate new fields in `CompileACLSnapshot` |
| `controller/cmd/server/main.go` | Read `RELAY_ADDR` env var, pass to compiler |

## Do NOT Touch

- `connector/` anything
- `client/` anything
- `proto/connector/v1/connector.proto`

---

## Step 1 — Proto

In `proto/client/v1/client.proto`, add to `ACLSnapshot`:

```proto
message ACLSnapshot {
  uint64 version                 = 1;
  string workspace_id            = 2;
  int64  generated_at            = 3;
  repeated ACLEntry entries      = 4;
  string connector_tunnel_addr   = 5;
  string relay_addr              = 6;  // relay QUIC address e.g. "relay.example.com:9093" — empty = relay disabled
  string connector_id            = 7;  // connector UUID — used in relay LookupMsg
  string connector_spiffe        = 8;  // full connector SPIFFE URI — client validates relay-bridged cert against this
}
```

Run `buf generate` from repo root. Run `cd controller && go build ./...` before continuing.

---

## Step 2 — Controller Config

Add `RelayAddr string` to the server config struct and read it:

```go
cfg.RelayAddr = os.Getenv("RELAY_ADDR")
```

Pass it into wherever `CompileACLSnapshot` is called.

---

## Step 3 — Compiler

In `controller/internal/policy/compiler.go`, extend the existing connector DB query (around line 140) to also fetch `id` and `spiffe_id`:

```go
var connectorTunnelAddr, connectorID, connectorSPIFFE string
var lanAddr string
_ = pool.QueryRow(ctx,
    `SELECT COALESCE(lan_addr, ''), COALESCE(id, ''), COALESCE(spiffe_id, '')
     FROM connectors
     WHERE tenant_id = $1
       AND status = 'active'
     ORDER BY last_heartbeat_at DESC NULLS LAST LIMIT 1`,
    workspaceID,
).Scan(&lanAddr, &connectorID, &connectorSPIFFE)
```

Set the new fields on the returned snapshot:

```go
return &clientv1.ACLSnapshot{
    WorkspaceId:         workspaceID,
    Version:             version,
    GeneratedAt:         time.Now().Unix(),
    Entries:             entries,
    ConnectorTunnelAddr: connectorTunnelAddr,
    RelayAddr:           relayAddr,
    ConnectorId:         connectorID,
    ConnectorSpiffe:     connectorSPIFFE,
}, nil
```

---

## Build Check

```bash
buf generate          # from repo root
cd controller && go build ./...
```

---

## Post-Phase Fixes

*(Empty — add fixes here as discovered)*
