---
type: phase
member: M1
sprint: 15
phase: 4
title: Retention Worker
status: planned
depends_on: [Phase1-Migration-and-Posture-Store]
tags: [go, posture, retention, pending-08]
---

# Phase 4 — Retention Worker

> Depends on Phase 1 (specifically the `device_profile_evaluations.report_id ON DELETE
> SET NULL` fix — without it this worker cannot delete a report still referenced as an
> evaluation's source). Independent of Phase 2/3's evaluation and ACL-gating work.

## Goal

One posture report every 5 minutes is 288/device/day with no cleanup today — an
unbounded-growth table. Define and build a real retention mechanism, not just "a
scheduled job somewhere."

## Files

| File | Change |
|------|--------|
| `controller/internal/posture/retention.go` | **new** — the worker itself |
| `cmd/server/main.go` | start/stop wiring |

## Design

- **Owner:** a background goroutine started from `cmd/server/main.go` alongside the
  server's other background workers, not an external cron job and not ad hoc.
- **Lifecycle:** `main.go` currently has no signal-derived shutdown context, so this
  phase creates one with `signal.NotifyContext(context.Background(), os.Interrupt,
  syscall.SIGTERM)`. The retention worker receives that context. HTTP listeners are
  held as `http.Server` values and shut down with a 10-second timeout; gRPC uses
  `GracefulStop` with a 10-second `Stop` fallback. Cancel/wait for the retention worker
  before process exit. This is the single server lifecycle used by the worker—not an
  assumed context that does not exist in the current entrypoint.
- **Cadence:** daily.
- **Retention window:** `POSTURE_RETENTION_DAYS`, positive integer, default **30**.
- **Batch size:** `POSTURE_RETENTION_BATCH_SIZE`, positive integer, default **2000**.
- **Mechanics:** delete `device_posture_reports` rows older than the window — cascades
  to `device_posture_observations` via the existing `ON DELETE CASCADE`. Delete in a
  **fixed batch size** per iteration (e.g. a few thousand rows), looping until a batch
  returns fewer than the batch size (explicit termination condition, not an unbounded
  single `DELETE` or an infinite loop).
- **Depends on the Phase 1 nullable FK:** `device_profile_evaluations.report_id` is
  `ON DELETE SET NULL` specifically so this worker can proceed — without that, deleting
  a report still cited as the latest evaluation's source would `RESTRICT` and either
  fail the batch or require excluding referenced rows (which would let referenced-but-stale
  reports accumulate forever, defeating the retention goal).
- `device_profile_evaluations` itself is never touched by this worker — it's derived
  state kept for the life of the device/profile, not raw history.

## Tests

- Unit: batch deletion terminates (doesn't loop forever) once no rows remain older than the window.
- Unit: a report referenced by a `device_profile_evaluations.report_id` is still deleted by the worker, and that evaluation's `report_id` becomes `NULL` afterward (not a delete failure).
- Unit: an evaluation with a `NULL` report_id is treated as stale/unsatisfied wherever evaluation freshness is checked (Phase 3's compile-time and cache-expiry logic).
- Integration: worker respects context cancellation — starting it, cancelling the context, and confirming it stops within a bounded time.
- Integration: SIGTERM-derived cancellation stops the worker and initiates graceful
  HTTP/gRPC shutdown; the 10-second fallback prevents indefinite shutdown.

## Build Check
```bash
cd controller && go build ./... && go test ./internal/posture/...
```

## Implementation Checklist
- [x] **M1-F0-1** `internal/posture/retention.go` — daily worker using `POSTURE_RETENTION_DAYS=30` and `POSTURE_RETENTION_BATCH_SIZE=2000`, explicit loop termination, context-cancellable.
- [x] **M1-F0-2** `cmd/server/main.go` — add `signal.NotifyContext`; start/wait for the worker; gracefully stop HTTP and gRPC with a 10-second bound.
- [x] **M1-F0-3** Test proving a referenced report is still deletable (regression test for the Phase 1 FK fix).
- [ ] **Build gate:** `cd controller && go build ./... && go test ./internal/...` (targeted posture/server tests pass; full-suite socket-dependent tests require the host environment).

## Post-Phase Fixes
_None yet._

## Progress

Implemented the context-cancellable daily retention worker, configurable retention
defaults, startup wiring, signal-aware lifecycle, graceful HTTP/metrics/gRPC shutdown,
and retention regression tests. PostgreSQL-backed posture tests pass when the Docker
database is available. Tests requiring local socket binding may be blocked in restricted
execution environments.
