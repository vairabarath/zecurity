---
type: phase
member: M1
sprint: 18
phase: 3
title: Concurrency-Safe Claiming
status: planned
depends_on: [1]
tags: [go, outbox, concurrency, platform, pending-15]
---

# Phase 3 — Concurrency-Safe Claiming

> Depends on Phase 1 (schema). Full spec: [[PENDING-15-Durable-Outbox-Infrastructure]] §3.

## Goal

Multiple workers must never claim the same eligible event. Claiming is one **atomic** SQL
statement that transitions eligible events to `processing`, assigning each a unique `lease_id`
and `claimed_at`, and returns exactly the claimed rows.

## Files

| File | Change |
| --- | --- |
| `controller/internal/outbox/store.go` | **new** — `ClaimEvents`, `MarkDone`, `MarkFailed` (transactional store methods) |

## Steps

- [ ] **M18-3** `ClaimEvents(ctx, limit)` — single atomic statement (PENDING-15 §3, verbatim):

```sql
WITH candidates AS (
    SELECT id
      FROM outbox_events
     WHERE (status = 'pending'
            OR (status = 'failed' AND retry_count < :max_retries))
       AND next_attempt_at <= NOW()
     ORDER BY next_attempt_at, created_at
     LIMIT $1
     FOR UPDATE SKIP LOCKED
)
UPDATE outbox_events o
   SET status       = 'processing',
       lease_id     = gen_random_uuid(),
       claimed_at   = NOW(),
       updated_at   = NOW()
  FROM candidates c
 WHERE o.id = c.id
RETURNING o.*;
```

- [ ] **M18-3** `max_retries` is a configuration value on the `Outbox` store/processor — it is
      **not** a per-call `ClaimEvents` argument. `LIMIT` bounds the batch; `ORDER BY
      next_attempt_at, created_at` gives deterministic ordering.
- [ ] **M18-3** Stale-worker protection — `MarkDone`/`MarkFailed` update **only** the current lease:

```sql
WHERE id = $1
  AND lease_id = $2
  AND status = 'processing'
```

```go
func (o *Outbox) MarkDone(ctx context.Context, eventID, leaseID uuid.UUID) error
func (o *Outbox) MarkFailed(ctx context.Context, eventID, leaseID uuid.UUID, err error) error
```

- [ ] **M18-3** Zero affected rows from `MarkDone`/`MarkFailed` is a **stale or lost lease** — return
      an explicit error, never update the event without ownership.

## Rules

- `FOR UPDATE SKIP LOCKED` is mandatory — concurrent workers must neither block nor double-claim.
- Delivery is **at-least-once**: after lease expiry an event may be reclaimed. The outbox does not
  claim exactly-once processing.
- Do not remove or reorder the candidate-filter conditions; retry eligibility is defined by
  PENDING-15 exactly as written.

## Build gate

`cd controller && go build ./...`; PostgreSQL integration test proving concurrent workers never
receive the same eligible event (see Phase 10).
