---
type: phase
member: M1
sprint: 17
phase: 6
title: Users Deprovision + SideEffectSink → durable outbox
status: done
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
| `controller/internal/identity/device_trust.go` | **consumed** — contract merged from `feat/identity-device-trust-contract` (NOT redefined; `SideEffectSink`, `DeviceTrustEvent`, constructors live here) |
| `controller/internal/scim/side_effect_sink_outbox.go` | **new** — `DurableOutboxSink` implements `identity.SideEffectSink` via `outbox.Enqueue` + `NewDeviceTrustRevokeEvent`/`NewDeviceTrustReEnrollmentRequired` |
| `controller/internal/scim/directory_service.go` | **edit** — `Deprovision`/`Reactivate` (tx: status + `Revoker.BumpGenerationTx` + `sink.Enqueue`); ctor takes `sink` + `revoker` |
| `controller/internal/scim/users.go` | **edit** — `DELETE /Users` (soft-delete) + `active=false→Deprovision` / `active=true→Reactivate` dispatch; drop `handleDeleteNotImplemented` |
| `controller/internal/identity/revocation.go` | **edit** — add `BumpGenerationTx` (tx-aware generation bump) |
| `controller/cmd/server/main.go` | **edit** — wire `scim.NewDurableOutboxSink(outboxStore)` + `identityRevoker` into `NewDirectoryService` |
| `controller/internal/scim/deprovision_integration_test.go` | **new** — DB tests (suspend/delete/reactivate + same-tx enqueue + abort-tx invariant) |

## Steps (all complete)
- [x] Consume the merged `identity.SideEffectSink` + `DeviceTrustEvent` contract (single source of truth; the spec's `identity/side_effect_sink.go` plan is superseded by the merged `device_trust.go` — `Type` folded into `outbox.EventType`).
- [x] `DurableOutboxSink` (the only impl): maps a `DeviceTrustEvent` → `outbox.Enqueue(ctx, tx, identity.NewDeviceTrustRevokeEvent(...))` / `...ReEnrollmentRequired(...)`. Enqueue runs in the **caller's tx** → commits atomically with the identity mutation.
- [x] Deprovision (one tx): `active=false`→`suspended` / `DELETE`→`deleted` (soft-delete tombstone) + `identity_generation` bump (`Revoker.BumpGenerationTx`) + audit + `policy.Notifier` + `SideEffectSink.Enqueue(device.trust.revoke.requested)`. Unscoped/non-scim-owned → 409.
- [x] Reactivate: `active=true` → `status='active'` + enqueue `device.trust.re_enrollment_required`; **no generation bump** (devices were already revoked on suspend; re-enroll via login per ADR-028).
- [x] Event `Type` strings match ADR-025 §5.1 exactly (`device.trust.revoke.requested`, `device.trust.re_enrollment_required`). SCIM only enqueues; never revokes a device synchronously.

## Rules honored
- Identity effects (suspend/delete + generation + session kill + ACL invalidation) are fully correct and tested **without** relying on the outbox handler existing (verified with a fake sink).
- The enqueue is transactional but delivery/execution is asynchronous (outbox → PENDING-13). A forced enqueue error **rolls back the whole tx** (user stays active, generation unbumped) — this is the integration-test invariant, the opposite of "device failure rolls back identity."
- Downstream (device) failure must never roll back the committed identity mutation — exactly why the outbox exists.

## Build gate
`go build ./...` + tests: deprovision identity effects (suspend/delete + generation + session kill) with a **fake sink**; integration asserting an `outbox_events` row is written **in the same transaction**; forced-enqueue-error aborts the whole tx. All green against live Postgres (`PKI_TEST_DATABASE_URL`).

## Notes / deferrals (honest)
- We ENQUEUE only; the PENDING-13 consumer (Track 1) is a separate branch. Not built here (per sprint boundary).
- Phase 8 `scim_identity_conflicts` row on collision: still only 409 (unchanged from Phase 5).
- The `name`/profile column gap (Phase 5) is unaffected.
