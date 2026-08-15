---
type: adr
status: implemented
id: PENDING-02
domain: security
priority: P0
created: 2026-07-03
resolved: 2026-07-22
related:
  - ADR-014-Relay-Stabilization
  - Relay-E2E-Flow-and-Security-Review (F2)
tags: [pending, adr, security, pki, revocation]
---

# Pending ADR 02 — Certificate Revocation (CRL/OCSP) Enforcement

> **Status: IMPLEMENTED (Option A — CRL everywhere).** Decided and shipped in July 2026 on the
> `fixed-pendings` branch. See **Resolution** below. Verified against the code 2026-08-11.

## Resolution — Implemented (Option A, CRL)

**Decision:** Option A. CRL is the mesh-wide baseline, enforced **fail-closed at every trust
boundary**. OCSP remains deliberately out of scope.

**The three gaps in "Current State" below are all closed:**

| Gap as filed | Status |
|---|---|
| Relay never checks a CRL on outer QUIC mTLS | ✅ `relay/src/crl.rs` (`WorkspaceCrlManager`, refresh 60s / 15s retry) + `relay/src/listener.rs` rejects a revoked connector/client before bridging, with a distinct close reason per state |
| Controller doesn't check relay heartbeat revocation | ✅ `internal/connector/relay_revocation.go` (`RelayRevocationChecker`: in-memory revoked-serial set, 60s refresh **plus** on-demand refresh from the `OnRelayRevoked` hook), consulted in **both** `UnarySPIFFEInterceptor` and `StreamSPIFFEInterceptor` (`internal/connector/spiffe.go`) and again in `control_stream.go`. Additionally `RecordHeartbeat` and `MarkProvisioned` both gate `status NOT IN ('revoked','deleted')`, so a revoked relay can neither heartbeat nor re-provision |
| Connector inner-client CRL is the only enforcement point | ✅ still present (`connector/src/crl.rs`, `/ca.crl?workspace_id=`), and no longer the only one |

**Delivered beyond the original scope:**
- **Connector certificate revocation** — migration `029_connector_revocation.sql` (`revoked_at`,
  `revocation_reason`), `revokeConnector` GraphQL mutation, revoked connector serials published on
  the **same workspace CRL** as client devices (`internal/pki/workspace.go` `GenerateClientCRL`
  unions `client_devices` + `connectors`, both Workspace-CA-signed). Revocation drops the connector
  from **both** planes immediately (`NotifyPolicyChange` + `NotifyTopologyChange`).
- **Client-side relay CRL enforcement** — `client/src/crl.rs` + `tunnel_pool.rs` / `relay_pool.rs`.
- **Connector-side relay CRL enforcement** — `connector/src/relay_client.rs` `verify_revocation`.
- **Continuous revocation, not just at handshake** — `monitor_relay_revocation` tears down a *live*
  relay connection when that relay is revoked mid-session.

**Fail-closed by construction.** Every verifier uses a 3-state
`RevocationStatus { Revoked, NotRevoked, Unavailable }` (Rust) / `Ready()` gate (Go), so "no valid
CRL" is **not representable** as "not revoked". Covered by tests in all three Rust crates
(`relay_verifier_fails_closed_without_crl`, `relay_connection_fails_closed_without_crl`,
`rejects_mismatched_authority_key_identifier`, expiry/window cases) and
`TestGenerateRelayCRL_FailsClosedWithoutIntermediate` in Go.

**Open questions from below, now answered:**
- *Propagation latency* — 60s periodic refresh at every verifier, **plus** an immediate on-demand
  refresh when a revoke happens, so the practical window is near-zero for controller-side denial.
- *Distribution + signing* — two on-demand endpoints: `GET /ca.crl?workspace_id=<id>`
  (Workspace-CA-signed) and `GET /relay.crl` (Intermediate-CA-signed,
  `internal/relay/crl_handler.go`). Both unauthenticated: a CRL is self-authenticating.
- *Fetch failure* — fail-closed, everywhere (above).
- *Per-workspace or one platform CRL* — **both**, split by issuer: per-workspace for
  connector/client/device certs, one platform CRL for relays.

**Residual, non-blocking:** the relay cert TTL is still 30 days (`RELAY_CERT_TTL`) with no relay
renewal path (`internal/relay/store.go` notes "and future renewal"). The original recommendation to
shorten it was explicitly defense-in-depth, and its premise — "the 30-day relay cert is the weak
link" — assumed no revocation enforcement existed. With fail-closed CRL checks at every hop plus
the DB status gate, this is a hardening item, not a hole. Track it with the relay lifecycle work.

**Revoke triggers are wired to the correct planes**, as this doc required: relay revoke is a
provider action (`POST /provider/relays/{id}/revoke`), connector and client-device revoke are
tenant-admin GraphQL mutations.

**Commits:** `243f6df` (controller relay cert revocation) → `ef6647f` (relay CRL in connector +
client) → `dfb2c65` (relay peer revocation + connector revoke + hardening) → merged `50bcfda`.

**On adoption as a numbered ADR:** the decision is made and shipped; promote to the next free
`ADR-0NN` whenever the team does a docs pass. Kept here so the audit trail stays with the original
problem statement.

---

## Context / Current State

> *Historical — this described the state on 2026-07-03, before the Resolution above.*

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
