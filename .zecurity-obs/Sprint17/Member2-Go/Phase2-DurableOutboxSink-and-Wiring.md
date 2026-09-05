---
type: phase
member: M2
sprint: 17
phase: 2
title: DurableOutboxSink + Wiring (SUPERSEDED — folded into M1 Phase 6)
status: superseded
depends_on: [1]
tags: [go, platform, outbox, sideeffectsink, pending-15, pending-13, superseded]
---

# Phase 2 (M2) — DurableOutboxSink + Wiring — SUPERSEDED

> **This phase is no longer a separate M2 task.** With the outbox already merged (Sprint 18), the
> `DurableOutboxSink` collapses to a ~15-line adapter (`SideEffectSink` → `outbox.Enqueue`) and there is
> **no interim sink and no gap reconciliation** to do — the durable path exists from the first commit.
>
> That adapter now lives in M1: [[Sprint17/Member1-Go/Phase6-Deprovision-and-SideEffectSink]].
>
> **Still owned elsewhere (not this sprint):** the outbox *handler* that consumes
> `device.trust.revoke.requested` / `device.trust.re_enrollment_required` and executes device/cert
> revocation is **PENDING-13**. SCIM only enqueues; PENDING-13 registers the handler.
>
> If Sathiya prefers to own the `DurableOutboxSink` adapter for boundary cleanliness, it's a trivial
> coordination — otherwise M1 writes it in Phase 6.
