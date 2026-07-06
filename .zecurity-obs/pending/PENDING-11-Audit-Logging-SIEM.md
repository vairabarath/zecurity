---
type: adr
status: pending
id: PENDING-11
domain: operations
priority: P2
created: 2026-07-03
related:
  - PENDING-10-Observability
tags: [pending, adr, operations, audit, siem, compliance]
---

# Pending ADR 11 — Audit Logging & SIEM Export

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.
> *(Current state below is from a presence/absence scan — confirm before scoping.)*

## Context / Current State

Per-access decisions exist (`ConnectorLog` carries dest/port/action/allow-deny/spiffe up the
control stream), but there is no evident **tamper-evident admin audit trail** (who created/changed
groups, access rules, enrollments, relays, IdP config) nor a **SIEM export** (syslog/webhook/S3)
for customers' security teams. A ZTNA product is a security control point — enterprise buyers and
compliance regimes (SOC 2 / ISO 27001) expect an exportable audit log of both admin actions and
access events.

## Problem — Decision Needed

What do we audit, how is it stored/tamper-evidenced, and how do customers consume it?

## Options

### Option A — First-party audit log + export APIs
Structured audit events (admin actions + access decisions) to an append-only store; export via
webhook / syslog / object storage; per-tenant scoping.
- **Pros:** full control; per-tenant isolation. **Cons:** build + retention/storage design.

### Option B — Emit to the observability pipeline (PENDING-10) and let customers pull
Treat audit as a log stream in the same telemetry backend.
- **Pros:** reuses PENDING-10. **Cons:** audit needs stronger integrity/retention guarantees than
  ops telemetry; harder per-tenant export.

### Option C — Access-event export only (defer admin-action audit)
Ship the access log (already partly captured) first.
- **Pros:** smaller. **Cons:** misses admin-action audit that compliance actually asks for.

## Recommendation (non-binding)
Option A, scoped in two slices: (1) admin-action audit (config changes) — what auditors ask for
first, (2) access-event export building on existing `ConnectorLog`. Keep it separate from ops
telemetry for integrity/retention reasons.

## Open Questions
- Retention + tamper-evidence requirements (hash chaining? WORM storage?)?
- Export formats/targets customers need (Splunk HEC, syslog, S3, webhook)?
- Which entities/actions are in the v1 audit schema?

## Rough Effort / Priority
**M, P2** (P1 if a compliance/enterprise deal requires it sooner).
