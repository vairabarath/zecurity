---
type: phase
member: M2
sprint: 17
phase: 2
title: DurableOutboxSink + Wiring + Reconciliation
status: planned
depends_on: [1]
tags: [go, platform, outbox, sideeffectsink, pending-15, pending-13]
---

# Phase 2 (M2) — DurableOutboxSink + Wiring + Reconciliation

> Depends on M2 Phase 1 (outbox) + M1 Phase 6 (`SideEffectSink` interface). Full spec:
> [[PENDING-15-Durable-Outbox-Infrastructure]] §7 · [[ADR-025-SCIM-Directory-Synchronization]] §5.1 · [[PENDING-05-SCIM-Implementation-Plan]] P10.

## Goal
Make device-trust delivery **durable** by backing M1's `identity.SideEffectSink` with the outbox, and
close the interim gap.

## Files
| File | Change |
| --- | --- |
| `controller/internal/outbox/side_effect_sink.go` | **new** — `DurableOutboxSink` implements `identity.SideEffectSink` |
| `controller/cmd/server/main.go` | swap interim `UnwiredSink` → `DurableOutboxSink` (one line, coordinate with M1) |

## Steps
- [ ] `DurableOutboxSink.Enqueue(ctx, tx, evt)` → `outbox.Enqueue(tx, evt)` — same transaction as the identity mutation.
- [ ] Swap the sink at wiring; verify the enqueue commits inside the deprovision tx and a downstream failure never rolls back the identity mutation.
- [ ] **Gap reconciliation:** replay device-trust intents recorded in `audit_logs` during the interim (UnwiredSink) window, once, idempotently.
- [ ] Device-event contract handoff to PENDING-13 (`device.trust.revoke.requested` → `RevokeUserDevices`); PENDING-13 owns the handler.

## Rules
- The event contract is ADR-025 §5.1 — do not invent new event shapes here.
- PENDING-13 owns execution; this phase only guarantees delivery.

## Build gate
`go build ./...` + end-to-end: deprovision → outbox row → (stub PENDING-13 consumer) → done; reconciliation replays interim intents exactly once.
