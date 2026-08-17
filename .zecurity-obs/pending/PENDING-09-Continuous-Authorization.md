---
type: adr
status: pending
id: PENDING-09
domain: zero-trust
priority: P2
created: 2026-07-03
related:
  - ADR-001-Sprint8-ACL-Snapshot-Caching
  - PENDING-08-Device-Posture-Health
  - PENDING-06-MFA-Step-Up-Auth
tags: [pending, adr, zero-trust, authorization]
---

# Pending ADR 09 — Continuous / Re-evaluated Authorization

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.
>
> **Partial implementation note (Sprint 15):** a bounded variant of **Option B only** shipped as a
> side effect of PENDING-08's connector session-registry work — no new revocation RPC; the connector
> diffs `(spiffe_id, resource_id)` between the previous and newly-applied ACL snapshot in
> `control_stream.rs`'s `AclSnapshot` handler and cancels tunnels for pairs that dropped out, bounded
> by the existing heartbeat/snapshot-expiry interval, not immediate. **Options A (push-based
> immediate revocation) and C (risk-scored step-up) are still fully open**, and the team explicitly
> deferred promoting this to an ADR until the bounded-latency approach is validated in practice —
> see `.zecurity-obs/Sprint15/path.md` line ~362. Status stays `pending` because the actual decision
> (which option to standardize on) has not been made; Sprint 15 is a tactical interim measure, not a
> ratified choice.

## Context / Current State

Authorization is effectively **connect-time**. The connector/client enforce the local ACL
snapshot, which refreshes on a ~60s TTL and on policy-change notifications (ADR-001 caching +
epoch/CAS ADR-013). But there is no explicit model for **re-evaluating an in-flight session** when
something changes mid-session: group membership revoked, device posture degrades (PENDING-08),
risk rises, or a token is revoked. Long-lived flows (SSH/RDP) can outlive the authorization that
permitted them.

## Problem — Decision Needed

How aggressively do we re-evaluate live sessions, and on which triggers?

## Options

### Option A — Snapshot-TTL re-evaluation (mostly already true)
Rely on the 60s ACL refresh: new flows use the new snapshot; formalize + tighten the interval and
ensure existing flows are re-checked, not just new ones.
- **Pros:** builds on what exists. **Cons:** up-to-TTL lag; doesn't kill established connections.

### Option B — Event-driven session revocation
On revocation/policy-change/posture-drop, actively tear down affected live tunnels (client daemon
+ connector cooperate).
- **Pros:** true continuous authz; bounded exposure. **Cons:** needs a revocation-signal path to
  data plane + connection kill logic.

### Option C — Risk-scored step-up
Continuous signals feed a risk score; threshold crossing forces step-up (PENDING-06) or drop.
- **Pros:** strongest posture. **Cons:** most complex; needs posture + step-up first.

## Recommendation (non-binding)
Formalize A now (guarantee existing flows are re-checked each refresh, not just new ones), add B
for the high-value triggers (deprovision / device revoke), and treat C as the long-term goal once
PENDING-08 posture exists.

## Open Questions
- Which triggers must kill live connections vs. only block new ones?
- Acceptable revocation-to-teardown latency? Interaction with relay drain semantics (ADR-017)?

## Rough Effort / Priority
**A: S · B: M · C: L**, P2.
