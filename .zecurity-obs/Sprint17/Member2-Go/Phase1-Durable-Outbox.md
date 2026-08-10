---
type: phase
member: M2
sprint: 17
phase: 1
title: Durable Outbox Infrastructure (PENDING-15)
status: planned
depends_on: []
tags: [go, platform, outbox, reliability, pending-15]
---

# Phase 1 (M2) — Durable Outbox Infrastructure

> Depends on nothing — Day 1. Source of truth: [[PENDING-15-Durable-Outbox-Infrastructure]].
> Reusable platform infra; does **not** implement SCIM, identity, or device logic.

## Goal
A durable, transactional, retrying delivery layer between an identity decision and its async side-effects.

## Files
| File | Change |
| --- | --- |
| `controller/migrations/033_outbox_events.sql` | **new** — `outbox_events` (033; 030 skipped) |
| `controller/internal/outbox/*.go` | **new** — Enqueue, ClaimEvents, processor, recovery, handler registry |

## Steps
- [ ] `outbox_events(id, event_type, workspace_id, payload, status[pending|processing|done|failed], retry_count, next_attempt_at, lease_id, claimed_at, correlation_id, created_at, updated_at)` + claim/processing/workspace indexes.
- [ ] `Enqueue(ctx, tx, evt)` — inserts within the **caller's** transaction (so it commits atomically with the identity mutation).
- [ ] `ClaimEvents(ctx, limit)` — `FOR UPDATE SKIP LOCKED`, ordered by `next_attempt_at`; sets `processing` + `lease_id` + `claimed_at`.
- [ ] Processing lifecycle (done/failed + backoff via `next_attempt_at`); crash/lease recovery (reaper returns stuck `processing` past lease timeout to retryable); generic handler registry; background processor; idempotency; observability.

## Rules
- Outbox does **not** own handlers or device/SCIM logic — only at-least-once delivery.

## Build gate
`go build ./...` + tests: transactional enqueue, concurrent claim (no double-claim), retry/backoff, crash/lease recovery.
