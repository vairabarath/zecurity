---
type: phase
member: M1
sprint: 14
phase: 4
title: Controller Revocation Enforcement
status: done
depends_on:
  - Sprint14/Member1-Go/Phase1-Relay-Cert-History-Data-Model
  - Sprint14/Member1-Go/Phase2-Provider-Revoke-Transaction
tags: [go, relay, spiffe, revocation, enforcement, pending-02]
---

# Phase 4 — Controller Revocation Enforcement

> Depends on Phase 1 (revocation state) and Phase 2 (revoke hook for cache refresh).
> **First behavior change in the sprint** — do it after A/B/C are proven. Test the *non-revoked*
> path hard: a bug here can lock out legitimate relays.

## Goal

Make the controller reject a **revoked relay** at Provision **and** Heartbeat — without adding a DB
query to every RPC. The controller is the CRL's source of truth, so it checks its **database via an
in-memory cache**, not by parsing its own `/relay.crl`.

## Files

| File | Change |
|------|--------|
| `controller/internal/connector/spiffe.go` | add revocation check in `verifyRelayCertificate` (`:226`) |
| `controller/cmd/server/main.go` | construct + wire the `RevocationChecker`; refresh on revoke |

## The checker
- A small `RevocationChecker` with an in-memory set of revoked (unexpired) relay serials, loaded at
  startup from `ListRevokedRelaySerials()` and refreshed (a) inside the Phase-2 revoke transaction and
  (b) on a periodic tick as a backstop.
- `verifyRelayCertificate` (`connector/spiffe.go:226`): after the existing Intermediate-CA chain
  check, look up `leaf.SerialNumber` (normalize hex to match `relay_certificates.serial`) and **reject**
  if revoked.
- **Fail closed:** if the checker's state cannot be established (never loaded), reject rather than
  allow. Never allow on lookup error.

> The SPIFFE interceptor (`connector/spiffe.go:150`) runs on **every** unary/stream RPC and relays
> heartbeat ~every 30s — hence the cache, not a per-RPC DB query.

## Wiring (main.go)
- Build the checker with the relay store; inject into the interceptor path (the interceptor takes a
  `WorkspaceStore`; add the checker alongside, or give the validator a revocation hook — confirm the
  cleanest seam during implementation).
- Call `checker.Refresh()` from the revoke transaction (Phase 2) so revocation takes effect on the
  relay's next RPC, not only on the next periodic tick.

## Tests
- Revoked serial → `verifyRelayCertificate` rejects; non-revoked → passes.
- Checker never loaded → **fail closed** (reject).
- Integration: a revoked relay's Provision/Heartbeat gRPC is denied (`PermissionDenied`/`Unauthenticated`).

## Build Check
```bash
cd controller && go build ./...
```

## Implementation Checklist
- [x] **M1-D1** `RevocationChecker` (cache + DB) + authenticated relay check; fail-closed.
- [x] **M1-D2** wire the checker in `main.go`; refresh **post-commit** through `OnRelayRevoked`.
- [x] **Build gate:** `cd controller && go build ./...`

## Pre-Implementation Corrections (validated review — codex)
- **Provision is NOT covered by `verifyRelayCertificate` (must-fix).** `RelayService/Provision` is in
  the SPIFFE interceptor's skip list (`connector/spiffe.go:162`) — the relay has no client cert yet at
  Provision. So the verifier enforcement covers **Heartbeat + other authenticated relay RPCs only**,
  **not** Provision. Correct the goal/acceptance accordingly.
- **Deny re-provisioning of a revoked relay via a state guard, not the verifier.** Since the verifier
  can't run at Provision, denial of a revoked relay re-provisioning must come from the **Provision-time
  DB status guard** in `MarkProvisioned` / `provision.go` (see Phase 2's race fix), not from this
  phase's cache check.
- **Publish to cache after commit.** The checker must be refreshed **post-commit** of the revoke
  transaction (see Phase 2), never from inside it.

## Post-Phase Fixes
_None yet._
