---
type: adr
status: pending
id: PENDING-10
domain: operations
priority: P1
created: 2026-07-03
related: []
tags: [pending, adr, operations, observability, metrics]
---

# Pending ADR 10 — Observability: Metrics, Tracing, Health

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.
> *(Current state below is from a presence/absence scan — confirm before scoping.)*

> **Correction (2026-09-01, verified against source):** still **PENDING**, but no longer a clean
> greenfield — a narrow metrics slice exists. `controller/internal/metrics/metrics.go` (104 lines)
> registers reconcile-only counters/gauges (reports, drift, resyncs, tombstones reaped) and serves
> Prometheus text at `/metrics` (`controller/cmd/server/main.go:642`); there is a single `/health`
> handler (`main.go:356`). **Absent:** distributed tracing, SLOs, a readiness/liveness split, and
> every operator signal named below — relay capacity, probe/migration rates, ACL compile latency,
> heartbeat health, tunnel throughput, cert-expiry runway. Scope this as "extend the existing
> registry", not "introduce metrics".

## Context / Current State

There is structured logging (e.g. `tracing` in Rust services, connector `ConnectorLog`), but no
evident **metrics/tracing/SLO** layer for the control and data planes. Operating a relay fleet +
connectors + shields across tenants (see PENDING-07) needs quantitative signals: relay
capacity/`connection_count` trends, probe/migration rates, ACL compile latency, heartbeat health,
tunnel throughput/errors, cert-expiry runway. The capacity data already exists internally
(relay heartbeat reports `connection_count`/`max_connections`) but isn't exposed as operator metrics.

## Problem — Decision Needed

What observability stack and signal set do we standardize on?

## Options

### Option A — Prometheus metrics + OpenTelemetry traces
`/metrics` on each service; OTel traces across control-stream + tunnel setup; Grafana dashboards.
- **Pros:** industry standard; self-hostable; composes with existing infra. **Cons:** instrumentation
  effort across Go + 3 Rust services.

### Option B — Vendor APM (Datadog/Honeycomb/Grafana Cloud)
- **Pros:** fastest to dashboards + alerting. **Cons:** cost; egress of operational data.

### Option C — Minimal health/metrics endpoints only
Just liveness/readiness + a few counters; defer tracing.
- **Pros:** cheap MVP. **Cons:** limited insight into distributed failures (relay migration,
  failover convergence).

## Recommendation (non-binding)
Option A (Prometheus + OTel) as the standard, starting with the highest-value metrics: relay
capacity/fill, migration/probe rates, ACL compile latency & version lag, cert-expiry runway,
heartbeat freshness. Feeds directly into the operator plane (PENDING-07) and failover convergence
(ADR-016 Phase 3D).

## Open Questions
- Self-host vs vendor (cost vs speed)? Per-tenant metric isolation for the operator console?
- Minimum viable dashboard/alert set for launch?

## Rough Effort / Priority
**M, P1.** Needed before real production customers.
