---
type: phase
member: M1
sprint: 18
phase: 6
title: Generic Handler Registry
status: planned
depends_on: [1]
tags: [go, outbox, handlers, platform, pending-15]
---

# Phase 6 — Generic Handler Registry

> Depends on Phase 1 (schema) — independent of claiming/recovery. Full spec:
> [[PENDING-15-Durable-Outbox-Infrastructure]] §6.

## Goal

A **provider-independent** dispatch mechanism: the outbox routes each event by `event_type` to a
registered handler. The outbox package contains **no** event-specific business logic.

## Files

| File | Change |
| --- | --- |
| `controller/internal/outbox/handler.go` | **new** — `EventHandler` interface + `HandlerRegistry` |

## Steps

- [ ] **M18-6** `EventHandler` interface + `HandlerRegistry` (PENDING-15 §6, verbatim):

```go
type EventHandler interface {
    Handle(ctx context.Context, evt OutboxEvent) error
}

type HandlerRegistry struct {
    handlers map[string]EventHandler
}
```

- [ ] **M18-6** Registry mechanism only: `RegisterHandler(eventType, handler)`, lookup by
      `event_type`, dispatch. Concurrent registration is not expected after startup but must not
      race — guard the map.
- [ ] **M18-6** **Unknown event types** (no registered handler) fail safely as **terminal**
      failures: `status='failed'`, `retry_count=max_retries`, `next_attempt_at=NULL`,
      `last_error="no handler registered for event_type X"`. The processor logs a warning but does
      **not** crash or retry the unknown event indefinitely.
- [ ] **M18-6** Event-specific handler registration is performed by the **owning subsystem**:
      PENDING-13 registers `device.trust.revoke.requested` / `device.trust.re_enrollment_required`;
      ADR-025 registers any lifecycle handlers it owns. Sprint 18 registers **none** of them — it
      only provides the seam.
- [ ] **M18-6** Handler contract: return `nil` on success (even if the event was already applied —
      idempotency); return an error if the side effect should be retried. Exactly-once external
      effects are the handler's responsibility.

## Rules

- **Do NOT implement** Okta/Entra/JumpCloud/Keycloak handlers, device revocation, certificate
  issuance, or any business logic inside the outbox package.
- The outbox only provides: enqueue, claim, process-via-registered-handler, complete, fail, reap.
- The outbox package must not import or reference device-revoke, certificate, or SCIM handler code.

## Build gate

`cd controller && go build ./...`; unit + PostgreSQL tests for unknown-handler terminal behavior
(see Phase 10).
