---
type: adr
status: pending
id: PENDING-15
domain: platform
priority: P1
created: 2026-08-08
related:
  - ADR-025-SCIM-Directory-Synchronization
  - PENDING-13-Client-Device-Lifecycle
  - PENDING-02-Certificate-Revocation-Enforcement
tags: [pending, adr, outbox, platform, reliability, scim-integration]
---

# Pending ADR 15 — Platform Durable Outbox Infrastructure

> **Status: PENDING — for team discussion.** On adoption, promote to the next free
> `ADR-0NN`.
>
> This PENDING defines only the reusable **platform outbox infrastructure** that
> ADR-025 (SCIM Directory Synchronization) depends on. It does NOT implement SCIM,
> identity lifecycle, device lifecycle, or any specific event handler. Those belong
> to ADR-025 and PENDING-13 respectively.

---

## Context / Current State

The Zecurity controller performs identity mutations that must trigger asynchronous,
durable side effects — for example, when SCIM deprovisions a user (suspend or
delete), all of that user's device trust must be revoked. Today the controller has
no durable outbox: side effects are performed inline or fire-and-forget, with no
retry, no persistence, and no delivery guarantee.

ADR-025 (§7 "Lifecycle and Device Trust", §15 "Transaction and Concurrency
Requirements", §17 "Final Review Status") explicitly requires:

> "External and device-side effects, including certificate revocation, are
> delivered through a durable outbox with retry and observability."

> "The ADR remains PROPOSED until the final architectural review and
> implementation-readiness check approve the resulting migrations, authorization
> model, **outbox**, and interoperability test plan."

The existing `audit_logs` table (migration 016) is an **append-only audit trail**,
not a durable retryable queue: it has no `status` column, no retry tracking, no
claiming mechanism, and no consumer loop. It cannot serve as the outbox.

The repository already has the pieces the outbox will *deliver* — the identity
pipeline (`identity.Service`, `identity.Resolver`, `identity.Revoker`), the device
model (`client_devices` table, `revokeClientDevice`, `pki.GenerateClientCRL`), and
the connector-side CRL enforcement (`connector/src/crl.rs`). What is missing is the
**durable, transactional, retrying delivery layer** between an identity decision
and those downstream side effects.

## Problem — Decision Needed

What generic outbox infrastructure do we build so that:

1. An identity-plane transaction can atomically commit a mutation, an audit event,
   and an outbox event — with **no event loss after COMMIT**.
2. A background consumer can **concurrently claim** events, dispatch them to
   registered handlers, and **retry** on failure with backoff.
3. A **crashed worker's in-flight events** recover to a retryable state.
4. Handlers are **idempotent** (at-least-once delivery).
5. The outbox is **provider-independent** — it does not know about SCIM, Okta, or
   devices.

## Design

### 1. Durable Outbox Database

**Migration:** `033_outbox_events.sql` (next available after 032; 030 was skipped).

```sql
CREATE TABLE outbox_events (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    event_type      TEXT        NOT NULL,
    tenant_id       UUID        NOT NULL REFERENCES workspaces(id) ON DELETE RESTRICT,
    user_id         UUID        REFERENCES users(id) ON DELETE SET NULL,
    correlation_id  UUID        NOT NULL,
    payload         JSONB       NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending'
                        CHECK (status IN ('pending', 'processing', 'done', 'failed')),
    retry_count     INT         NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    next_attempt_at TIMESTAMPTZ DEFAULT NOW(),
    lease_id        UUID,
    claimed_at      TIMESTAMPTZ,
    last_error      TEXT
);

-- Consumer claims pending or retryable-failed events ordered by next attempt time.
CREATE INDEX idx_outbox_claim
    ON outbox_events (status, next_attempt_at, retry_count)
    WHERE status IN ('pending', 'failed');

-- Tenant-scoped lookups and cleanup.
CREATE INDEX idx_outbox_tenant_event
    ON outbox_events (tenant_id, event_type);

CREATE INDEX idx_outbox_processing
    ON outbox_events (status, claimed_at)
    WHERE status = 'processing';
```

**Conventions followed:**
- UUIDs match all other tables (`gen_random_uuid()`).
- `tenant_id` is NOT NULL + `ON DELETE RESTRICT`; workspace deletion must resolve outstanding events
  explicitly rather than implicitly deleting committed work.
- `created_at` / `updated_at` TIMESTAMPTZ NOT NULL DEFAULT NOW(), matching every existing table.
- `claimed_at` is the lease timestamp; `updated_at` remains a general row-update timestamp.
- `status` CHECK constraint matches the four-state lifecycle below.

`correlation_id` identifies the originating logical operation across the identity mutation, audit
record, outbox processing, and downstream side effects. It is included in structured logs and in
event payloads where appropriate; it is not a substitute for handler idempotency. A producer may also
provide a `dedupe_key` for event types with a defined logical uniqueness rule, but PENDING-15 does not
impose a global deduplication rule that could suppress legitimate repeated events.

`last_error` is bounded to **4096 bytes** before persistence. The complete error may be emitted
through the existing logging mechanism, subject to normal sensitive-data restrictions.

The originating transaction/service generates `correlation_id` and passes it to `Enqueue`; the outbox
does not generate a replacement identifier. This preserves one correlation identity across the
mutation, audit record, outbox row, logs, and downstream side effects.

Workspace deletion must explicitly resolve or archive outstanding outbox events before the workspace
can be deleted. A hard workspace deletion must be rejected while any outbox rows remain. An explicit,
audited retention/archive operation must preserve completed or terminal event records before their
rows can be removed as part of approved workspace deletion. A committed outbox event must not disappear
implicitly through workspace cascade deletion, and PENDING-15 does not define automatic deletion of
durable events.

### 2. Transactional Enqueue

```go
// Enqueue inserts an outbox event within the caller's transaction.
// The event commits or rolls back with the caller's transaction — no event
// loss after COMMIT, no orphan events after ROLLBACK.
func (o *Outbox) Enqueue(ctx context.Context, tx pgx.Tx, evt Event) error
```

The identity transaction flow (ADR-025 §7):

```
identity mutation
+ audit event
+ outbox event
      ↓
same transaction (BEGIN → COMMIT)
      ↓
SCIM HTTP response returned to caller
```

The outbox row is inserted using the same `pgx.Tx` the identity mutation uses.
If the transaction rolls back, the outbox row is removed. If it commits, the row
is durable.

### 3. Concurrency-Safe Claiming

```go
// ClaimEvents atomically transitions eligible events to 'processing' and
// returns them to the caller for processing. Each returned event receives a
// unique lease_id and claimed_at timestamp.
func (o *Outbox) ClaimEvents(ctx context.Context, limit int) ([]OutboxEvent, error)
```

**Strategy:** A single atomic `WITH candidates ... FOR UPDATE SKIP LOCKED`
query ensures bounded batch size, deterministic ordering, concurrent-worker safety,
and no duplicate claims:

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

`max_retries` is configured on the `Outbox` processor/store and bound by the
implementation; it is not a per-call `ClaimEvents` argument. The `LIMIT` bounds the batch;
`ORDER BY next_attempt_at, created_at` gives
deterministic ordering; `FOR UPDATE SKIP LOCKED` prevents concurrent workers from
blocking each other or claiming the same eligible event; `RETURNING o.*` returns
exactly the claimed rows to the caller in one atomic statement.

**Concurrency guarantee:** `FOR UPDATE SKIP LOCKED` prevents two workers from
concurrently claiming the same eligible event. After lease expiry, an event may be
reclaimed for at-least-once delivery. Lease ownership prevents a stale worker from
completing or failing a newer claim.

`MarkDone` and `MarkFailed` must update only the current lease:

```sql
WHERE id = $1
  AND lease_id = $2
  AND status = 'processing'
```

Conceptually:

```go
MarkDone(ctx context.Context, eventID, leaseID uuid.UUID) error
MarkFailed(ctx context.Context, eventID, leaseID uuid.UUID, err error) error
```

Both methods must treat zero affected rows as a stale or lost lease, not as permission
to update the event without ownership.

The processor may pass cancellation/deadline context to handlers, but context
cancellation does not guarantee that a handler has stopped executing. The delivery
model therefore remains at-least-once with idempotent handlers; PENDING-15 does not
claim exactly-once processing or guaranteed absence of overlap after lease expiry.

### 4. Processing Lifecycle

State transitions:

```
pending → processing → done     (success)
pending → processing → failed   (handler error)
failed  → processing → done     (retry succeeds)
failed  → processing → failed   (retry fails, retry_count++)
```

Failure handling:

```
handler returns error
   ↓
retry_count++
next_attempt_at = NOW() + backoff(retry_count)
status = failed
   ↓
eligible for re-claim on next poll
```

**Default max retries:** `100` (per the ADR-025 implementation plan).
Configurable via `OUTBOX_MAX_RETRIES`, validated to the inclusive range `1..1000`. Exhausted events remain
in `failed` status with `retry_count >= max_retries` and the final `last_error`.
They are permanently ineligible for automatic claiming and are **never silently
discarded**.

**Backoff:** Exponential — `min(5m, 2^retry_count * base)` with jitter, so
transient failures back off quickly and persistent failures don't hammer downstream
services. The clock and jitter source must be injectable or otherwise controllable
so tests can deterministically verify `retry_count`, backoff, and `next_attempt_at`
without real sleeps or nondeterministic randomness.

### 5. Crash / Lease Recovery

If a worker claims an event (sets `status = 'processing'`) but crashes before
completing:

```
worker claims → status='processing' → worker crashes
```

An abandoned event must eventually become retryable. The processor runs a
**reaper** that periodically scans for events whose explicit lease has expired:

```text
status = processing
AND claimed_at <= NOW() - lockWindow
```

The reaper resets them to `failed`, clears the lease, increments the retry count,
and schedules the next attempt:

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

This counts as a retry attempt and prevents repeated worker crashes from bypassing
the retry limit. An expired lease permits at-least-once redelivery; handlers remain
responsible for idempotency.

```go
// ReapAbandoned resets processing events with expired claimed_at leases back to failed.
func (o *Outbox) ReapAbandoned(ctx context.Context, lockWindow time.Duration) (int, error)
```

The default processor configuration is a 1-second claim poll, a 30-second lock
window, and a 30-second reaper interval. Validated bounds are 100ms–1m for the
poll interval, 5s–1h for the lock window, and 1s–5m for the reaper interval; the
reaper interval must not exceed the lock window. These values use the
`idx_outbox_processing` index on `(status, claimed_at)` WHERE
`status = 'processing'`.

**The reaper must be lease-aware.** It may only transition an event whose currently
stored `lease_id` is the same expired lease it identified in its scan. If a newer
lease has already been acquired before the reaper's update completes, the reaper's
update must match zero rows and change nothing.

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

The `o.lease_id = e.lease_id` guard ensures the reaper can never clear or replace
a newer lease that was acquired between the reaper's scan and its update.

### 6. Generic Event Handler Framework

The outbox must be **provider-independent**. It dispatches based on `event_type`
to a registered handler:

```go
type EventHandler interface {
    Handle(ctx context.Context, evt OutboxEvent) error
}

type HandlerRegistry struct {
    handlers map[string]EventHandler
}
```

Registration at server startup:

```go
outbox.RegisterHandler("device.trust.revoke.requested", deviceRevocationHandler)
outbox.RegisterHandler("device.trust.re_enrollment_required", enrollmentHandler)
```

**PENDING-15 implements the registry mechanism only** (`RegisterHandler`, lookup,
dispatch). Event-specific handler registration is performed by the owning
subsystem — PENDING-13 registers `device.trust.revoke.requested` and related
handlers; ADR-025 registers any lifecycle handlers it owns directly. This keeps
the ownership boundary crystal clear: PENDING-15 never imports or references
device-revoke, certificate, or SCIM handler code.

**The outbox package itself contains NO event-type-specific logic.** It only
provides: enqueue, claim, process-via-registered-handler, complete, fail, reap.

**Do NOT implement** Okta/Entra/JumpCloud/Keycloak handlers inside the outbox.
**Do NOT implement** device revocation, certificate issuance, or any business
logic. Those handlers are registered by PENDING-13 / ADR-025 consumers.

Unknown event types (no registered handler) fail safely as terminal processing
failures: the event is marked `failed`, `retry_count` is set to `max_retries`,
`next_attempt_at` is set to `NULL`, and `last_error` records
`"no handler registered for event_type X"`. The event remains observable and
requires an operator or explicit administrative recovery process to become
eligible again. The processor logs a warning but does not crash or retry the
unknown event indefinitely.

Terminal recovery is explicit, never automatic. An operator-only recovery
operation must identify the event ID, require a reason, record the operator and
timestamp in the platform audit trail, clear the lease fields, and deliberately
schedule the event for another attempt. Recovery must not silently bypass the
configured retry limit; the operation must explicitly authorize a retry budget
reset or a corrected handler before requeueing.

The conceptual recovery operation is:

```go
Recover(ctx, eventID, operatorID, reason string, resetRetryBudget bool) error
```

It is valid only for terminal `failed` events, clears `lease_id` and `claimed_at`,
sets `next_attempt_at = NOW()`, and preserves the existing retry count unless the
explicit `resetRetryBudget` authorization is supplied. The operation is audited by
the owning administrative surface.

### 7. PENDING-13 Integration Boundary

The outbox is the integration boundary between ADR-025 (identity lifecycle) and
PENDING-13 (device lifecycle):

```
ADR-025
   ↓
identity mutation + audit + outbox event   (same transaction)
   ↓
COMMIT
   ↓
PENDING-15 durable outbox (this PENDING)
   ↓
PENDING-13 consumer (device lifecycle)
   ↓
device trust / certificate operation
```

**Events ADR-025 will eventually emit** (defined by ADR-025 §7, consumed by
PENDING-13):

| event_type | payload | triggered by |
|---|---|---|
| `device.trust.revoke.requested` | `{tenant_id, user_id, reason: "suspended"\|"deleted", correlation_id}` | SCIM `PATCH active=false`, `DELETE` |
| `device.trust.re_enrollment_required` | `{tenant_id, user_id, correlation_id}` | SCIM `PATCH active=true` (reactivation) |

**The outbox does NOT implement these handlers.** PENDING-15 defines only the
generic event-delivery mechanism. PENDING-13 owns the device-side consumer,
payload interpretation, device-trust revocation, certificate behavior, and
re-enrollment behavior. The outbox only guarantees at-least-once delivery.

### 8. Background Processor

**Location:** `controller/internal/outbox/processor.go` (or repository-equivalent).

The processor is a background goroutine started in `cmd/server/main.go`:

```go
// At server startup:
o := outbox.New(dbPool, logger)
o.RegisterHandler("device.trust.revoke.requested", ...)
o.RegisterHandler("device.trust.re_enrollment_required", ...)

// Only after every required handler has been registered:
go o.Run(ctx,
    outbox.WithPollInterval(1*time.Second),
    outbox.WithLockWindow(30*time.Second),
    outbox.WithMaxRetries(100),
    outbox.WithReaperInterval(30*time.Second),
)
```

The startup order must always be:

```text
construct outbox
    ↓
register all event handlers
    ↓
start processor
```

The processor must:

- **Continuously claim** eligible events (`ClaimEvents` loop).
- **Dispatch** each claimed event to its registered handler.
- **Mark successful** events as `done`.
- **Mark failed** events as retryable (`status='failed'`, incremented
  `retry_count`, `next_attempt_at` with backoff, `last_error` recorded) until
  `max_retries` is reached; exhausted events remain permanently ineligible.
- **Schedule retries** via `next_attempt_at` + exponential backoff with jitter.
- **Recover abandoned** processing events (reaper).
- **Respect context cancellation** — shut down cleanly on signal.
- **Never leak goroutines** — the claim loop and reaper goroutines exit on
  context cancellation.

### 9. Idempotency

**Delivery guarantee:** at-least-once.

The outbox provides **at-least-once** delivery — an event may be delivered to a
handler more than once (e.g., if the handler succeeds but the `MarkDone` update
fails before the worker crashes). This is the standard, safe default.

**Exactly-once external side effects are NOT the outbox's responsibility.** Each
registered handler must be **idempotent**. Event-specific idempotency semantics,
including device-trust and certificate behavior, are owned by the subsystem that
registers the handler (PENDING-13 for device events).

The handler contract is: `Handle(ctx, evt) error` — return `nil` on success (even
if the event was already applied), return an error if the side effect should be
retried.

### 10. Observability

The outbox must provide observability through the **existing project logging
conventions** (Go `log` package, matching `controller/internal/identity/`
usage). Do not invent a new observability framework.

Requirements:

- **Processing failures:** log the event type, tenant_id, user_id, retry number,
  and error message.
- **Retry count:** tracked in the `retry_count` column; logged on each failure.
- **Event status:** queryable via `status` column (`pending`, `processing`,
  `done`, `failed`).
- **Last error:** stored in `last_error` column; logged on each failure.
- **Processing/claim activity:** log when events are claimed and when they
  complete (success or final failure).
- **Permanently failed events:** after max retries, log a critical/warning-level
  message with event details. The event remains in `failed` status — never
  silently dropped. An operator can inspect `outbox_events` for stuck events.

Future: PENDING-10 (Observability) will add Prometheus metrics; this PENDING
provides the data model and logging foundation.

---

## Explicit Non-Goals

This PENDING does **NOT** implement:

- SCIM HTTP endpoints (`/scim/v2/Users`, `/scim/v2/Groups`)
- SCIM bearer tokens or token authentication
- SCIM provider profiles or mapping validation
- `identity.DirectoryService` (the SCIM-facing identity orchestration layer)
- SCIM identity conflicts or conflict resolution
- SCIM user provisioning or deprovisioning logic
- SCIM group synchronization
- Device enrollment or certificate issuance
- Certificate renewal (`RenewClientCert`)
- Certificate revocation implementation (CRL generation already exists in `pki.GenerateClientCRL`)
- Event handler registration — only the registry mechanism (`RegisterHandler`) is provided;
  event-specific handlers are registered by the owning subsystem (PENDING-13 / ADR-025)
- Device management UI
- Okta / Entra / JumpCloud / Keycloak integration
- Audit logging changes (the `audit_logs` table is unchanged)
- Message brokers (Kafka, Redis Streams, RabbitMQ) — PostgreSQL only

Those belong to ADR-025 implementation work or PENDING-13.

---

## Dependencies

### Depends on
- Existing PostgreSQL infrastructure (`pgx/v5`, `pgxpool.Pool`) — the same pool
  all controller services use.
- Existing server lifecycle (`cmd/server/main.go`) — the processor is wired in
  at startup alongside other background goroutines (mirroring
  `connector.RunDisconnectWatcher`, `shieldSvc.RunDisconnectWatcher`,
  `relay.RunExpiryLoop`).
- ADR-025's durable-outbox architectural requirement (§7, §15, §17).

### Consumed by
- ADR-025 SCIM lifecycle implementation — emits `device.trust.revoke.requested`
  and `device.trust.re_enrollment_required` events via the outbox.
- PENDING-13 device lifecycle — registers handlers that consume those events
  (e.g., `client.RevokeUserDevices`).
- Future platform components that require durable asynchronous side effects
  (e.g., notification dispatch, policy propagation).

### Does not own
- Identity lifecycle — owned by `identity.Service` / ADR-025.
- Device lifecycle — owned by PENDING-13.

---

## Acceptance Criteria

### Database
- `outbox_events` table created by migration `033_outbox_events.sql`.
- Four-state `status` model: `pending`, `processing`, `done`, `failed`.
- Indexes on `(status, next_attempt_at, retry_count)` for claiming and
  `(status, claimed_at)` for reaping.
- `tenant_id` NOT NULL + FK to `workspaces(id) ON DELETE RESTRICT` for tenant
  isolation; workspace deletion cannot implicitly delete committed outbox events.
- `lease_id` and `claimed_at` are present; `updated_at` is not used as the lease
  timestamp.
- `correlation_id` is preserved through enqueue, processing, and structured logs.
- `correlation_id` is generated by the originating transaction/service and is not
  replaced by the outbox.
- `last_error` is bounded to 4096 bytes before persistence.
- `OUTBOX_MAX_RETRIES` is validated as a positive bounded configuration value.
- Default poll, lock-window, and reaper intervals are documented and validated.

### Transactionality
- `Enqueue(ctx, tx, evt)` inserts within the caller's transaction.
- Rollback of the caller's transaction removes the outbox event.
- Commit of the caller's transaction persists the outbox event.

### Concurrency
- Multiple workers calling `ClaimEvents` concurrently do not receive the same
  eligible event in the same lease period.
- Claiming is atomic: a single SQL statement uses a candidate CTE,
  `FOR UPDATE SKIP LOCKED`, assigns a unique `lease_id` and `claimed_at`, and
  uses `UPDATE ... RETURNING`.
- `MarkDone` and `MarkFailed` require the current `lease_id` and
  `status='processing'`; stale workers affect zero rows.
- Processing leases recover after worker failure using `claimed_at`; the reaper
  resets `processing` → `failed`, increments `retry_count`, schedules backoff,
  and clears `lease_id`/`claimed_at`.
- The reaper is lease-aware: it may only transition an event whose stored
  `lease_id` still matches the expired lease it identified — a newer lease
  acquired between scan and update is never cleared.
- A stale-worker race is covered: lease A expires, lease B is claimed, and
  `MarkDone(lease A)` affects zero rows.

### Reliability
- Failed handlers increment `retry_count` and reschedule via `next_attempt_at`.
- Exponential backoff with jitter is applied between retries.
- Events are not silently lost — `failed` status persists with `last_error`.
- Abandoned `processing` events eventually become retryable.
- Repeated crash/reap cycles increment `retry_count` and eventually reach
  `max_retries`.
- Exhausted events remain in `failed`, are observable, and are permanently
  ineligible for automatic claiming.
- Terminal recovery requires an explicit operator identity, reason, and deliberate
  requeue authorization; it is never automatic.
- Unknown handlers become terminal `failed` events with `retry_count=max_retries`
  and `next_attempt_at=NULL`; they are not retried forever.

### Processing
- Successful event → `status = 'done'`.
- Failed event → `status = 'failed'`, `retry_count++`, `next_attempt_at` scheduled
  until exhausted; an exhausted event has `next_attempt_at=NULL`.
- Registered handlers receive the correct `OutboxEvent` with deserialized `payload`.
- Unknown event types (no registered handler) → marked `failed` with
  `retry_count=max_retries`, `next_attempt_at=NULL`, and
  `last_error = "no handler registered"`, logged, observable.

### Lifecycle
- Processor starts with the server only after all owning subsystems have
  registered their handlers.
- Processor stops cleanly on context cancellation.
- No goroutine leaks.

### Testing

Require tests for:

| Test | Type |
|---|---|
| Enqueue inserts with pending status | Unit + Postgres |
| Transaction rollback removes the event | **Postgres integration** |
| Transaction commit persists the event | **Postgres integration** |
| Concurrent claiming does not duplicate | **Postgres integration** |
| Unique `lease_id` and `claimed_at` assigned per claim | **Postgres integration** |
| Stale worker cannot `MarkDone` after lease replacement | **Postgres integration** |
| Stale worker cannot `MarkFailed` after lease replacement | **Postgres integration** |
| Successful processing → done | Unit + Postgres |
| Handler failure → retry scheduled | Unit + Postgres |
| Retry scheduling uses deterministic injected clock/jitter | Unit |
| Retry exhaustion → failed, not discarded | Unit + Postgres |
| Repeated crash/reap cycles reach max retries | **Postgres integration** |
| Abandoned event recovery (reaper) | **Postgres integration** |
| Reaper cannot reclaim a row after its expired lease has already been replaced by a newer lease | **Postgres integration** |
| Unknown event type becomes terminal and is not reclaimed forever | Unit + Postgres |
| Correlation ID preserved through processing/logging | Unit + Postgres |
| Workspace deletion cannot cascade-delete committed events | **Postgres integration** |
| Terminal recovery requires explicit authorization and reason | Unit + Postgres |
| `last_error` truncates at 4096 bytes | Unit |
| Graceful shutdown (context cancellation) | Unit |
| Idempotency: repeated delivery is safe | Handler-level test |

**PostgreSQL integration tests are mandatory** for transaction, concurrency, and
recovery correctness — mocks cannot prove these guarantees.

---

## Relationship to ADR-025

> ADR-025 defines the architectural requirement and the identity/device integration
> contract for the durable outbox. This PENDING task implements the reusable
> platform infrastructure required to satisfy that contract.

ADR-025 §7 requires that "External and device-side effects, including certificate
revocation, are delivered through a durable outbox with retry and observability."
ADR-025 §15 requires "Device certificate revocation or durable outbox enqueue" as
a transaction boundary. ADR-025 §17 states it remains PROPOSED "until the final
architectural review and implementation-readiness check approve the resulting
migrations, authorization model, outbox, and interoperability test plan."

This PENDING provides that outbox infrastructure so ADR-025 can emit
identity-lifecycle-triggered device-trust revocation events durably and reliably.

## Relationship to PENDING-13

> PENDING-13 owns device lifecycle and consumes relevant outbox events. This
> PENDING task does not implement device-trust or certificate operations.

PENDING-13 (Client Device Lifecycle & Cert Renewal) owns:
- Device enrollment and certificate issuance (already implemented in Sprint 7).
- Certificate renewal (not yet implemented).
- Device trust revocation implementation (`revokeClientDevice`, `RevokeUserDevices`).
- Device management UI.

PENDING-13 will register handlers for the outbox events this PENDING defines the
delivery mechanism for:
- `device.trust.revoke.requested` → `client.RevokeUserDevices(tenantID, userID, reason)`
- `device.trust.re_enrollment_required` → invalidate/revoke device trust state

This PENDING (PENDING-15) does **not** implement those handlers.

---

## Rough Effort / Priority

**M, P1.** Required by ADR-025 acceptance. Blocks SCIM deprovisioning from
reliably triggering device trust revocation.
