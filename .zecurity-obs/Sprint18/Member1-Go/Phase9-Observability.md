---
type: phase
member: M1
sprint: 18
phase: 9
title: Observability
status: planned
depends_on: [7]
tags: [go, outbox, logging, platform, pending-15]
---

# Phase 9 — Observability

> Depends on Phase 7 (processor). Full spec: [[PENDING-15-Durable-Outbox-Infrastructure]] §10.
> Use the **existing project logging conventions** — no new observability framework.

## Goal

Enough structured logging to observe outbox health: what was claimed, dispatched, completed,
failed, retried, reaped, and terminally failed — with `correlation_id` carried through so an
operator can trace one logical operation end-to-end.

## Files

| File | Change |
| --- | --- |
| `controller/internal/outbox/*.go` | `log` calls added to store/processor/reaper per existing conventions |

## Steps

- [ ] **M18-9** Log **claim activity**: when events are claimed (count + event IDs).
- [ ] **M18-9** Log **completion**: success and final failure, with event type, workspace ID,
      user ID, and `correlation_id`.
- [ ] **M18-9** Log **processing failures**: event type, workspace_id, user_id, retry number, and
      error message on every failure.
- [ ] **M18-9** Log **retry count** on each failure (from `retry_count`).
- [ ] **M18-9** Log **terminal failures** (after max retries / unknown handler) at warning/critical
      level with event details; the event stays in `failed` — never silently dropped. An operator
      can inspect `outbox_events` for stuck events.
- [ ] **M18-9** Log **reaper activity**: how many abandoned leases were recovered per pass.
- [ ] **M18-9** Include `correlation_id` in all outbox log lines so identity-mutation →
      outbox-processing → downstream side-effect can be correlated.

## Rules

- Use the existing Go `log` package conventions matching `controller/internal/identity/` — do not
  invent a new observability framework.
- `last_error` is bounded to 4096 bytes before persistence; the complete error may be logged,
  subject to normal sensitive-data restrictions.
- Metrics/Prometheus are deferred to PENDING-10 — this phase provides the data model and logging
  foundation only.

## Build gate

`cd controller && go build ./...`.
