---
type: adr
status: accepted
id: ADR-027
former_id: PENDING-02
domain: security
priority: P0
created: 2026-07-03
decided: 2026-08-10
related:
  - ADR-014-Relay-Stabilization
  - Relay-E2E-Flow-and-Security-Review (F2)
tags: [adr, security, pki, revocation]
---

# ADR-027 — Certificate Revocation (CRL/OCSP) Enforcement

> **Status: ACCEPTED (2026-08-10).** Promoted from `PENDING-02`.
>
> **Decision: Option A** — CRL as the mesh-wide baseline, implemented at all three
> trust boundaries this doc named: relay outer QUIC mTLS (per-workspace
> `WorkspaceCrlManager`, fail-closed 3-state check — `relay/src/crl.rs`,
> `relay/src/listener.rs`), controller relay-heartbeat validation
> (`RelayRevocationChecker`, fails closed on an unready/unloaded state —
> `controller/internal/connector/relay_revocation.go`, `spiffe.go`), and the
> connector's existing inner client mTLS check (hardened to the same 3-state
> pattern — `connector/src/crl.rs`, `connector/src/device_tunnel.rs`). All four CRL
> consumers (relay, connector×2, client) refresh on a 60s + 0–15s jitter cycle.
> Client-side relay-revocation checking was added beyond this doc's original
> scope (`client/src/crl.rs`, `relay_pool.rs`, `tunnel_pool.rs`). OCSP remains
> unimplemented, consistent with this doc's own recommendation to defer it.
> **Implemented in commit `dfb2c65`**, independently verified end-to-end
> (code read + tests run, not just the commit message) — every fail-open/closed
> path checked fails closed, revoked-but-unexpired certs are rejected at every
> layer, and dedicated tests now cover the two previously-untested spots
> (the relay listener's and connector's own accept/stream match arms).

## Context / Current State

Revocation coverage across the mesh is **incomplete**:
- The **relay** validates the cert chain + SPIFFE but **never checks a CRL** on its outer QUIC
  mTLS (`relay/src/tls.rs`), so a revoked-but-unexpired connector or client is still bridged.
- The **controller** verifies relay heartbeat certs against the Intermediate CA but does **not**
  check revocation (`internal/relay/spiffe.go`), so a revoked relay authenticates for up to its
  30-day cert life.
- The **connector** *does* consume a CRL for inner client mTLS (`connector/src/crl.rs`) — so a
  revoked client can't reach the resource — but that's the only enforcement point.
- No OCSP anywhere. The original roadmap explicitly deferred CRL/OCSP ("DB status flag is
  sufficient for now").

## Problem — Decision Needed

What revocation mechanism do we standardize on, and at which trust boundaries must it be enforced?

## Options

### Option A — CRL everywhere (extend the existing pattern)
Reuse the connector's CRL fetch/cache pattern on the relay (outer verifier) and the controller
(relay heartbeat verifier). Controller publishes one signed CRL; all verifiers pull+cache+refresh.
- **Pros:** consistent with what already exists; offline-tolerant (cached). **Cons:** revocation
  latency = refresh interval; CRL size grows.

### Option B — OCSP (stapling / responder)
- **Pros:** near-real-time; small responses. **Cons:** new responder infra; availability coupling
  unless stapled; more complexity.

### Option C — Short TTLs + fast rotation instead of revocation
Lean on short cert lifetimes (already 7d connector/client) as the revocation window.
- **Pros:** simplest. **Cons:** 30-day relay cert is the weak link; doesn't cover urgent revocation.

## Recommendation (non-binding)
Option A (CRL) as the mesh-wide baseline — enforce at relay outer mTLS + controller relay
heartbeat + keep connector inner check. Shorten the relay cert TTL (see PENDING-13-style renewal)
as defense-in-depth. Consider OCSP later only if revocation latency becomes a real requirement.

## Dependency on the Provider Plane
CRL *enforcement* (verifiers checking revocation) is **backend-only and independent of the
dashboard** — ship it now. The *trigger* to revoke differs by cert type: revoking a **relay** is a
provider action that belongs in the operator plane ([[PENDING-07-Provider-Dashboard-Vision]]);
revoking a **client device** is tenant-admin ([[PENDING-13-Client-Device-Lifecycle]]). Wire the
revoke triggers to the right plane as those land.

## Open Questions
- Acceptable revocation-propagation latency (drives refresh interval)?
- CRL distribution endpoint + signing key management; behavior on fetch failure (fail-open vs
  fail-closed — must be fail-closed for security-critical hops)?
- Does the relay need per-workspace CRLs or one platform CRL?

## Rough Effort / Priority
**M, P0** (security). Pairs naturally with PENDING-01.
