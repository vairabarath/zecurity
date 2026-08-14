---
type: phase
member: M1
sprint: 18
phase: 7
title: Background Processor
status: done
depends_on: [3, 4, 5, 6]
tags: [go, outbox, processor, platform, pending-15]
---

# Phase 7 — Background Processor

> Depends on Phases 3 (claim), 4 (retry), 5 (reaper), 6 (handler registry). Full spec:
> [[PENDING-15-Durable-Outbox-Infrastructure]] §8 + §9.

## Goal

A background goroutine (started in `cmd/server/main.go`) that continuously claims eligible events,
dispatches each to its registered handler, marks results, schedules retries, reaps abandoned
leases, and shuts down cleanly on context cancellation.

## Files

| File | Change |
| --- | --- |
| `controller/internal/outbox/processor.go` | **new** — `Outbox.Run(ctx, ...)` claim/dispatch/reap loop |
| `controller/cmd/server/main.go` | wire outbox construction + registration seam + processor start/stop (surgical edit; Sprint 17 M1 also touches this file) |

## Steps

- [x] **M18-7** Startup order (PENDING-15 §8, normative):

```text
construct outbox
    ↓
register all event handlers
    ↓
start processor
```

- [x] **M18-7** `Run` options (defaults from PENDING-15 §5): `WithPollInterval(1s)`,
      `WithLockWindow(30s)`, `WithMaxRetries(100)`, `WithReaperInterval(30s)` — all validated to the
      documented bounds.
- [x] **M18-7** Claim loop: repeatedly `ClaimEvents`; for each claimed event, look up the handler
      in the registry and dispatch; mark `done` on success; mark retryable `failed` on handler error
      until exhaustion (Phase 4); unknown handlers become terminal (Phase 6).
- [x] **M18-7** Reaper task: runs every reaper interval, calls `ReapAbandoned(lockWindow)` (Phase 5).
- [x] **M18-7** Context cancellation: claim loop and reaper goroutines exit on cancellation;
      graceful shutdown; **no goroutine leaks**.
- [x] **M18-7** Registration wiring seam only — this sprint registers **no** device handlers.
      PENDING-13 owns `device.trust.*` handlers; ADR-025 owns its lifecycle handlers. `main.go`
      exposes the seam so those subsystems can register before `Run`.

## Rules

- The processor must start **only after** every required handler has been registered — never before.
- Context passed to handlers may be cancelled, but cancellation does not guarantee a handler has
  stopped; delivery remains at-least-once with idempotent handlers.
- Do not add a per-event-type switch/dispatch inside the processor — it only consults the registry.

## Build gate

`cd controller && go build ./...`; tests for graceful shutdown, no goroutine leaks, and
end-to-end claim→dispatch→done/failed→reap (see Phase 10).
