---
type: phase
member: M1
sprint: 18
phase: 8
title: Terminal Event Recovery
status: planned
depends_on: [1, 6]
tags: [go, outbox, recovery, ops, platform, pending-15]
---

# Phase 8 — Terminal Event Recovery

> Depends on Phase 1 (store) + Phase 6 (terminal `failed` semantics). Full spec:
> [[PENDING-15-Durable-Outbox-Infrastructure]] §6 "Terminal recovery".

## Goal

An **explicit, operator-only** recovery path for events that reached a terminal `failed` state
(exhausted retries or unknown handler). Recovery is deliberate, audited, and never automatic.

## Files

| File | Change |
| --- | --- |
| `controller/internal/outbox/recover.go` | **new** — `Recover` operation (or folded into `store.go` per convention) |

## Steps

- [ ] **M18-8** Conceptual recovery operation (PENDING-15 §6):

```go
func (o *Outbox) Recover(ctx context.Context, eventID, operatorID uuid.UUID, reason string, resetRetryBudget bool) error
```

- [ ] **M18-8** Valid **only** for terminal `failed` events (`retry_count >= max_retries` or
      unknown-handler terminal state). Non-terminal or `done`/`processing` events are rejected.
- [ ] **M18-8** Clears `lease_id` and `claimed_at`, sets `next_attempt_at = NOW()`, and preserves
      the existing retry count **unless** `resetRetryBudget` is explicitly supplied.
- [ ] **M18-8** Requires a mandatory `reason` and records the operator identity + timestamp in the
      platform audit trail (`audit_logs`), as an audited operation by the owning administrative
      surface.
- [ ] **M18-8** Recovery must **not** silently bypass the configured retry limit — a retry-budget
      reset must be an explicit, authorized argument, never implied.

## Rules

- Terminal recovery is **never automatic** — no scheduled path may requeue a terminal event.
- Unknown-handler events also require this explicit path to become eligible again.
- This sprint establishes the **backend capability only**; no generic admin UI is built (per
  repository convention, a UI would be a frontend sprint's coordinated hand-off).

## Build gate

`cd controller && go build ./...`; unit + PostgreSQL tests: recovery requires explicit
authorization + reason; terminal-only eligibility; `resetRetryBudget` semantics (see Phase 10).
