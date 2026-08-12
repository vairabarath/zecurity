---
type: phase
member: M1
sprint: 18
phase: 10
title: Testing
status: planned
depends_on: [1, 2, 3, 4, 5, 6, 7, 8, 9]
tags: [go, outbox, tests, postgres, platform, pending-15]
---

# Phase 10 — Testing

> Depends on Phases 1–9. Full spec: [[PENDING-15-Durable-Outbox-Infrastructure]] "Acceptance
> Criteria" + "Testing" tables. **PostgreSQL integration tests are mandatory** for transaction,
> concurrency, and recovery guarantees — mocks cannot prove them.

## Goal

Convert every relevant PENDING-15 acceptance criterion into implementation tests. This is the
final build gate that proves the outbox delivers what PENDING-15 requires.

## Files

| File | Change |
| --- | --- |
| `controller/internal/outbox/*_test.go` | **new** — unit + PostgreSQL integration tests |

## Steps — mandatory PostgreSQL integration tests

- [ ] **M18-10** Transaction **rollback** removes the outbox event.
- [ ] **M18-10** Transaction **commit** persists the outbox event.
- [ ] **M18-10** **Concurrent claiming** does not duplicate — two+ workers never receive the same
      eligible event in the same lease period.
- [ ] **M18-10** Unique `lease_id` + `claimed_at` assigned per claim.
- [ ] **M18-10** **Stale `MarkDone`** after lease replacement affects zero rows.
- [ ] **M18-10** **Stale `MarkFailed`** after lease replacement affects zero rows.
- [ ] **M18-10** **Abandoned-event recovery** (reaper resets expired `processing` → retryable
      `failed`, +1 retry, backoff, cleared lease).
- [ ] **M18-10** **Lease-replacement race** — reaper cannot reclaim a row after its expired lease
      was already replaced by a newer lease (the lease-A-expires / lease-B-claimed case).
- [ ] **M18-10** **Retry exhaustion** — repeated crash/reap cycles reach `max_retries`; event
      stays `failed`, observable, permanently ineligible.
- [ ] **M18-10** **Unknown-handler terminal behavior** — `failed`, `retry_count=max_retries`,
      `next_attempt_at=NULL`, `last_error` set; not reclaimed forever.
- [ ] **M18-10** **Workspace-deletion protection** — deleting a workspace cannot cascade-delete
      committed events (`ON DELETE RESTRICT` blocks it).
- [ ] **M18-10** **Terminal-recovery authorization** — `Recover` is terminal-only, requires a
      reason, requires operator identity, preserves retry budget unless `resetRetryBudget`.

## Steps — unit tests

- [ ] **M18-10** Retry/backoff: `retry_count`, backoff values, and `next_attempt_at` with a
      deterministic injected clock and jitter.
- [ ] **M18-10** `last_error` truncates at **4096 bytes**.
- [ ] **M18-10** Graceful shutdown (context cancellation) — claim loop + reaper exit, no goroutine
      leaks.
- [ ] **M18-10** Processor behavior: success → `done`; handler failure → retry scheduled; retry
      exhaustion → terminal `failed`; unknown event → terminal; claim → dispatch → complete cycle.
- [ ] **M18-10** `correlation_id` preserved through processing and logging.
- [ ] **M18-10** Idempotency: repeated delivery is safe (handler-level test — handler contract is
      "nil on success even if already applied").

## Rules

- Enqueue-rollback / enqueue-commit / concurrent-claim / stale-lease / reaper-race / retry-
  exhaustion / unknown-handler / workspace-delete / recovery tests are **PostgreSQL integration
  tests**, not mocks.
- All tests must pass against a real Postgres instance (existing test harness convention in
  `controller/internal/...`).
- Do not add tests for SCIM or device handlers — PENDING-13 / ADR-025 own those.

## Build gate

```bash
cd controller && go build ./... && go vet ./... && go test ./internal/outbox/...
```
