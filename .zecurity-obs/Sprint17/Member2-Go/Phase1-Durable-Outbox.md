---
type: phase
member: M2
sprint: 17
phase: 1
title: Durable Outbox Infrastructure (SUPERSEDED — shipped as Sprint 18)
status: superseded
depends_on: []
tags: [go, platform, outbox, reliability, pending-15, superseded]
---

# Phase 1 (M2) — Durable Outbox Infrastructure — SUPERSEDED

> **This phase is no longer part of Sprint 17.** The durable outbox was implemented and merged into
> `fixed-pendings` as **Sprint 18** (PENDING-15, 33/33 phases). See:
> - `controller/internal/outbox/*` (store, processor, handler registry, backoff, recovery)
> - `controller/migrations/033_outbox_events.sql`
> - [[PENDING-15-Durable-Outbox-Infrastructure]] (status: IMPLEMENTED) · `Sprint18/path.md`
>
> Sprint 17 consumes it via `outbox.Enqueue(ctx, tx, evt)`. Nothing to build here.
> The `DurableOutboxSink` adapter that wraps it now lives in M1
> [[Sprint17/Member1-Go/Phase6-Deprovision-and-SideEffectSink]].
