---
type: phase
member: M1
sprint: 18
phase: 5
title: Crash / Lease Recovery
status: done
depends_on: [3]
tags: [go, outbox, recovery, lease, platform, pending-15]
---

# Phase 5 — Crash / Lease Recovery

> Depends on Phase 3 (claiming + leases). Full spec: [[PENDING-15-Durable-Outbox-Infrastructure]] §5.

## Goal

A worker that claims an event and crashes before completing must not strand it forever. The
**reaper** resets expired-lease `processing` events back to a retryable `failed` state — without
ever clearing a newer lease acquired concurrently.

## Files

| File | Change |
| --- | --- |
| `controller/internal/outbox/reaper.go` | **new** — lease-aware reaper scan + reset |
| `controller/internal/outbox/store.go` | **new** — `ReapAbandoned` (or in `reaper.go`) |

## Steps

- [x] **M18-5** `ReapAbandoned(ctx, lockWindow time.Duration) (int, error)` — resets `processing`
      events whose lease has expired:

```text
status = processing
AND claimed_at <= NOW() - lockWindow
```

```text
processing
    ↓
reaper detects expired lease
    ↓
failed
retry_count++
next_attempt_at = NOW() + backoff(retry_count)
lease_id = NULL
claimed_at = NULL
```

- [x] **M18-5** **Lease-aware reaping** (PENDING-15 §5, verbatim) — the reaper may only transition an
      event whose stored `lease_id` is the same expired lease it identified:

```sql
WITH expired_leases AS (
    SELECT id, lease_id
      FROM outbox_events
     WHERE status = 'processing'
       AND claimed_at <= NOW() - :lock_window
)
UPDATE outbox_events o
   SET status          = 'failed',
       retry_count     = retry_count + 1,
       next_attempt_at = NOW() + :backoff,
       lease_id        = NULL,
       claimed_at      = NULL,
       updated_at      = NOW()
  FROM expired_leases e
 WHERE o.id = e.id
   AND o.lease_id = e.lease_id    -- still the same (expired) lease
   AND o.status = 'processing'    -- not already completed/failed by another worker
```

- [x] **M18-5** The expired lease must be re-checked inside the UPDATE (`o.lease_id = e.lease_id`)
      — a newer lease acquired between the reaper's scan and its update must never be cleared.

## Rules

- Reaping counts as a retry attempt — repeated crash/reap cycles must eventually reach
  `max_retries`.
- At-least-once redelivery applies after lease expiry; handlers remain responsible for idempotency.
- Reaper processor defaults live with the processor config (Phase 7): lock window 30s (5s–1h);
  reaper interval 30s (1s–5m) and must **not exceed** the lock window.

## Build gate

`cd controller && go build ./...`; PostgreSQL integration tests for abandoned-event recovery and
the lease-replacement race (see Phase 10).
