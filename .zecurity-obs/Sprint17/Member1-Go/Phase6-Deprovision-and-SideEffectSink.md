---
type: phase
member: M1
sprint: 17
phase: 6
title: Users Deprovision + SideEffectSink → durable outbox
status: planned
depends_on: [5]
tags: [go, identity, scim, deprovision, revocation, sideeffectsink, pending-05, pending-15]
---

# Phase 6 — Users: Deprovision + Reactivate + SideEffectSink → outbox

> Depends on Phase 5. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §5, §5.1, §8 · [[PENDING-05-SCIM-Implementation-Plan]] P6.
> **Reconciled 2026-08-18:** the durable outbox already shipped (Sprint 18, `controller/internal/outbox/*`).
> There is **no interim sink** — the `SideEffectSink` is backed by the real `outbox.Enqueue` from the first commit.

## Goal
Make deprovision cut Zecurity access **now** (sessions + policy, no outbox needed) and **durably** emit
device-trust events by enqueuing them into the merged outbox inside the same identity transaction.

## Files
| File | Change |
| --- | --- |
| `controller/internal/identity/side_effect_sink.go` | **new** — `SideEffectSink` interface + `DeviceTrustEvent` type |
| `controller/internal/scim/side_effect_sink_outbox.go` | **new** — `DurableOutboxSink` adapts `SideEffectSink` → `outbox.Enqueue` |
| `controller/internal/scim/users.go` | deprovision / reactivate handlers |
| `controller/cmd/server/main.go` | construct `DurableOutboxSink` from the existing `outbox.Outbox`; inject into the SCIM engine |

## Steps
- [ ] Define `SideEffectSink { Enqueue(ctx, tx pgx.Tx, evt DeviceTrustEvent) error }` + `DeviceTrustEvent{WorkspaceID, UserID, Type, Reason, CorrelationID}`. The interface lives in `identity` so `scim`/`identity` never import `outbox` directly and unit tests can inject a fake.
- [ ] `DurableOutboxSink` (production, the only impl): marshal `DeviceTrustEvent` → `json.RawMessage` and call the merged `outbox.Enqueue(ctx, tx, outbox.Event{EventType: evt.Type, WorkspaceID: evt.WorkspaceID, UserID: &evt.UserID, CorrelationID: evt.CorrelationID, Payload: payload})`. Enqueue runs in the **caller's tx**, so it commits atomically with the identity mutation.
- [ ] Deprovision (one tx): `active=false`→suspend / `DELETE`→soft-delete + `identity_generation` bump + `Revoker` session kill + audit + `policy.Notifier` + `SideEffectSink.Enqueue(device.trust.revoke.requested)`.
- [ ] Reactivate: `active=true` → enqueue `device.trust.re_enrollment_required`; new sessions only. Repeated DELETE idempotent.
- [ ] Event `Type` strings match ADR-025 §5.1 exactly (`device.trust.revoke.requested`, `device.trust.re_enrollment_required`) — PENDING-13 registers the outbox handler that executes them. **SCIM only enqueues; it never revokes a device synchronously.**

## Rules
- Identity effects (suspend/delete + generation + session kill + ACL invalidation) must be fully correct and tested **without** relying on the outbox handler existing.
- The enqueue is transactional but delivery/execution is asynchronous (outbox → PENDING-13). Never present device revocation as synchronously guaranteed in the SCIM response.
- Downstream (device) failure must never roll back the committed identity mutation — that is exactly why the outbox exists.

## Build gate
`go build ./...` + tests:
- deprovision identity effects (suspend/delete + generation + session kill) with a **fake sink**, no DB outbox needed;
- an integration test asserting a `outbox_events` row is written **in the same transaction** as the deprovision, and that a forced enqueue error aborts the whole tx (identity not mutated).
