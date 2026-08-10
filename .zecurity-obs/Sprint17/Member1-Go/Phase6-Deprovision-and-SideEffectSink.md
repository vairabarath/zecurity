---
type: phase
member: M1
sprint: 17
phase: 6
title: Users Deprovision + SideEffectSink boundary
status: planned
depends_on: [5]
tags: [go, identity, scim, deprovision, revocation, sideeffectsink, pending-05, pending-15]
---

# Phase 6 — Users: Deprovision + Reactivate + SideEffectSink

> Depends on Phase 5. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §5, §5.1, §8 · [[PENDING-05-SCIM-Implementation-Plan]] P6.
> **Locked (Q1): NO fake/best-effort device revocation. The interim sink records intent + signals loudly; it never revokes.**

## Goal
Make deprovision cut Zecurity access **now** (independent of PENDING-15), and emit device-trust events
through the `SideEffectSink` boundary that M2 will back with the durable outbox.

## Files
| File | Change |
| --- | --- |
| `controller/internal/identity/side_effect_sink.go` | **new** — `SideEffectSink` interface + `UnwiredSink` interim impl |
| `controller/internal/scim/users.go` | deprovision/reactivate handlers |

## Steps
- [ ] Define `SideEffectSink { Enqueue(ctx, tx, DeviceTrustEvent) error }` + `DeviceTrustEvent{workspace_id, user_id, type, reason, correlation_id}`. **M1 owns this interface** — it is the M1↔M2 contract.
- [ ] `UnwiredSink` (interim): writes an `audit_logs` record "device-trust <type> REQUESTED — NOT durably delivered (PENDING-15 unwired)"; returns nil so the identity tx commits; **never revokes, never fakes delivery**; a startup log warns delivery is unwired.
- [ ] Deprovision (one tx): `active=false`→suspend / `DELETE`→soft-delete + `identity_generation` bump + `Revoker` session kill + audit + `policy.Notifier` + `SideEffectSink.Enqueue(device.trust.revoke.requested)`.
- [ ] Reactivate: `active=true` → enqueue `device.trust.re_enrollment_required`; new sessions only. Repeated DELETE idempotent.

## Rules
- Identity effects must be fully correct and tested **without** PENDING-15.
- Downstream (device) failure must never roll back the committed identity mutation.

## Build gate
`go build ./...` + tests: deprovision identity effects (suspend/delete + generation + session kill) independent of the outbox; interim sink records-not-delivers.
