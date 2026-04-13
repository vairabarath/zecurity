# Phase 5 — Disconnect Watcher

Background goroutine that marks stale connectors as disconnected. Added to the same
file as the heartbeat handler (`heartbeat.go`).

---

## File to Modify: `controller/internal/connector/heartbeat.go`

Add to the same file as the Heartbeat handler.

### Function: `runDisconnectWatcher`

```go
// runDisconnectWatcher runs as a background goroutine, started alongside the gRPC server.
// Called by: main.go (Member 2 starts this with `go runDisconnectWatcher(ctx)`)
//
// Behavior:
//   - Ticks every cfg.HeartbeatInterval
//   - Marks connectors DISCONNECTED where:
//     status='active' AND last_heartbeat_at < NOW() - cfg.DisconnectThreshold
//   - Only affects connectors in active workspaces
//   - Uses cfg.HeartbeatInterval and cfg.DisconnectThreshold — no hardcoded durations
//
// SQL:
//   UPDATE connectors
//      SET status = 'disconnected', updated_at = NOW()
//    WHERE status = 'active'
//      AND last_heartbeat_at < NOW() - $1
//      AND tenant_id IN (SELECT id FROM workspaces WHERE status = 'active')
func runDisconnectWatcher(ctx context.Context)
```

### Key implementation notes

- **No hardcoded durations** — uses `cfg.HeartbeatInterval` for tick interval and `cfg.DisconnectThreshold` for staleness threshold.
- **Only active workspaces** — suspended workspace connectors are not marked disconnected (they're already inactive).
- **Context cancellation** — respects `ctx.Done()` for graceful shutdown.

---

## Phase 5 Checklist

```
✓ Runs as background goroutine
✓ Ticks every cfg.HeartbeatInterval
✓ Marks stale active connectors as 'disconnected'
✓ Uses cfg.DisconnectThreshold for staleness
✓ Only affects connectors in active workspaces
✓ No hardcoded durations
✓ Respects context cancellation for shutdown
```
