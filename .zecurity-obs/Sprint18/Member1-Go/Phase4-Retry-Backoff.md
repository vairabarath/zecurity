---
type: phase
member: M1
sprint: 18
phase: 4
title: Retry / Backoff
status: done
depends_on: [3]
tags: [go, outbox, retry, backoff, platform, pending-15]
---

# Phase 4 — Retry / Backoff

> Depends on Phase 3 (`MarkFailed`). Full spec: [[PENDING-15-Durable-Outbox-Infrastructure]] §4.
> Use PENDING-15's exact defaults and bounds — do not introduce new retry semantics.

## Goal

A failed handler schedules a retry with exponential backoff and jitter, tracking `retry_count`
until `max_retries`; exhausted events stay observable in `failed` and are never silently
discarded.

## Files

| File | Change |
| --- | --- |
| `controller/internal/outbox/backoff.go` | **new** — exponential backoff + jitter (injectable clock/jitter) |
| `controller/internal/outbox/store.go` | **new** — `MarkFailed` failure handling (retry increment + `next_attempt_at`) |

## Steps

- [x] **M18-4** Failure lifecycle (PENDING-15 §4):

```text
handler returns error
   ↓
retry_count++
next_attempt_at = NOW() + backoff(retry_count)
status = failed
   ↓
eligible for re-claim on next poll
```

- [x] **M18-4** `MarkFailed` records `last_error` (bounded to 4096 bytes) and increments
      `retry_count`, updating only the current lease (Phase 3's `id + lease_id + status='processing'`).
- [x] **M18-4** Backoff — exponential: `min(5m, 2^retry_count * base)` with jitter, so transient
      failures back off quickly and persistent ones don't hammer downstream services.
- [x] **M18-4** `OUTBOX_MAX_RETRIES` — default **100**, validated to the inclusive range
      `1..1000`. Read via the existing env-var convention (`envOr`/`mustEnv` in `cmd/server/main.go`).
- [x] **M18-4** Retry exhaustion: events with `retry_count >= max_retries` remain `failed` with the
      final `last_error`, are **permanently ineligible** for automatic claiming, and are **never
      silently discarded**.
- [x] **M18-4** Injectable clock + jitter source so tests deterministically verify `retry_count`,
      backoff, and `next_attempt_at` without real sleeps or nondeterministic randomness.

## Rules

- Do not change the backoff formula, the default, or the bounds — PENDING-15 is the contract.
- Exhaustion is terminal, not auto-deleted; recovery is Phase 8's explicit operator path.
- `next_attempt_at = NULL` is reserved for terminal (exhausted or unknown-handler) events — see
  Phase 6.

## Build gate

`cd controller && go build ./...`; unit tests with deterministic clock/jitter proving backoff
values and exhaustion behavior (see Phase 10).
