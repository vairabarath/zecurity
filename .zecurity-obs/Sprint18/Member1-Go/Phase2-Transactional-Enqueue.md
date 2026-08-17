---
type: phase
member: M1
sprint: 18
phase: 2
title: Transactional Enqueue
status: done
depends_on: [1]
tags: [go, outbox, transactions, platform, pending-15]
---

# Phase 2 — Transactional Enqueue

> Depends on Phase 1 (schema). Full spec: [[PENDING-15-Durable-Outbox-Infrastructure]] §2.

## Goal

`Enqueue(ctx, tx, evt)` must insert an outbox event inside the **caller's** transaction so the
event commits or rolls back with the identity mutation — **no event loss after COMMIT, no orphan
events after ROLLBACK**.

## Files

| File | Change |
| --- | --- |
| `controller/internal/outbox/store.go` | **new** — `Enqueue` + supporting event model |
| `controller/internal/outbox/event.go` | **new** — `Event` / `OutboxEvent` types (or folded into `store.go` per package convention) |

## Steps

- [x] **M18-2** `Enqueue(ctx, tx, evt)` inserts a row with `status='pending'`, `next_attempt_at=NOW()`,
      `retry_count=0`, and the caller-supplied `correlation_id` — using the **same `pgx.Tx`** the
      identity mutation uses, never the pool.

```go
func (o *Outbox) Enqueue(ctx context.Context, tx pgx.Tx, evt Event) error
```

- [x] **M18-2** The identity transaction flow (ADR-025 §8) must be the target integration shape:

```text
identity mutation
+ audit event
+ outbox event
      ↓
same transaction (BEGIN → COMMIT)
      ↓
SCIM HTTP response returned to caller
```

- [x] **M18-2** Rollback removes the outbox row; commit persists it. Verify both paths in tests
      (see Phase 10).

## Rules

- `Enqueue` takes a `pgx.Tx` — it must **not** fall back to the pool if the tx is `nil`; fail
  instead.
- The outbox does **not** generate `correlation_id` — the originating service generates it and
  passes it in.
- No outbox-side validation of payload contents — the outbox is provider-independent and treats
  `payload` as opaque JSONB.

## Build gate

`cd controller && go build ./...`; PostgreSQL integration test proving COMMIT persists the row and
ROLLBACK removes it (written here or in Phase 10 per convention).
