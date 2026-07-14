---
type: phase
member: M2
sprint: 14
phase: 2
title: Connector + Client Consume /relay.crl
status: planned
depends_on:
  - Sprint14/Member1-Go/Phase3-Relay-CRL-Generation-Endpoint
  - Sprint14/Member2-Rust/Phase1-Hardened-CRL-Manager
tags: [rust, crl, relay, connector, client, revocation, pending-02]
---

# Phase 2 — Connector + Client Consume /relay.crl

> Depends on Phase 3 (the `/relay.crl` endpoint) and M2 Phase 1 (the hardened manager).

## Goal

Make the connector and client **reject a revoked relay certificate** when dialing the relay — closing
the "stale `LabelledRelayList` / stolen relay key" gap that the controller's own check (Phase 4) and
the broadcast cannot cover for cached/offline consumers.

Both already verify the relay's server cert (chain to Intermediate CA + exact relay SPIFFE) but check
no revocation:
- Connector: `ExactRelaySpiffeVerifier` (`connector/src/relay_client.rs:310`).
- Client: `ExactSpiffeVerifier` with `relay_roots = intermediate_ca` (`client/src/relay_pool.rs:61`).

## Files

| File | Change |
|------|--------|
| `connector/src/relay_client.rs` | add relay-CRL check in the relay server-cert verifier |
| `client/src/relay_pool.rs` | same on the client |
| (config) `connector`/`client` | `/relay.crl` URL; wire a hardened-manager instance for the relay CRL (Intermediate CA issuer) |

## Approach
- Instantiate a hardened CRL manager (M2 Phase 1) pointed at the controller's **`/relay.crl`**, with
  the **Intermediate CA** as the expected issuer (both components already hold it as the relay trust
  anchor).
- In each relay server-cert verifier, after the existing chain + exact-SPIFFE check, extract the
  relay leaf serial and:
  - if `manager.has_valid_cache()` and `manager.is_revoked(serial)` → **reject** the handshake;
  - if `!manager.has_valid_cache()` → **fail closed** (reject) — do not dial a relay whose revocation
    state is unknown. (Confirm the availability trade-off during implementation; the ADR mandates
    fail-closed for security-critical hops.)
- Refresh 60s + jitter (shared with M2 Phase 1).

## Tests
- Connector/client refuse a relay cert whose serial is in the relay CRL.
- With no valid cached CRL → fail closed.
- E2E: revoke a relay (Phase 2), a connector/client holding a **cached** `LabelledRelayList` refuses
  to dial it.

## Build Check
```bash
cd connector && cargo build && cd ../client && cargo build
```

## Implementation Checklist
- [ ] **M2-F1** `connector/src/relay_client.rs` — relay-CRL check via the hardened manager; fail-closed.
- [ ] **M2-F2** `client/src/relay_pool.rs` — same.
- [ ] **Build gate:** `cd connector && cargo build && cd ../client && cargo build`

## Pre-Implementation Corrections (validated review — codex)
- **Full connector wiring, not just the verifier (must-fix).** The connector's dial path is
  `relay_selector.rs → relay_client::run_session` (`relay_client.rs:152`). Thread the CRL manager
  through `RelaySelectorConfig`, `spawn_session`, `run_session`, `RelayClient::connect`, **and** the
  verifier — the verifier constructor alone is not enough.
- **Evict already-established connections (must-fix).** A handshake-only check leaves **live** relay
  connections usable after revocation: the client `RelayPool` caches QUIC connections by address
  (`client/src/relay_pool.rs:152-185`) and the connector holds a persistent relay session. On a CRL
  update that adds a live relay's serial, **close/replace** those connections (or bound connection
  lifetime + revalidate before reuse). Handshake rejection alone is insufficient.

## Post-Phase Fixes
_None yet._
