---
type: phase
member: M2
sprint: 14
phase: 2
title: Connector + Client Consume /relay.crl
status: in-progress
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
- [x] **M2-F1** `connector/src/relay_client.rs` — relay-CRL check via the hardened manager; fail-closed.
- [x] **M2-F2** `client/src/relay_pool.rs` — same, including active cached-connection eviction.
- [x] **Build gate:** `cd connector && cargo build && cd ../client && cargo build`
- [ ] **M2-F3 E2E gate:** revoke a Relay through the Controller and prove a connector/client holding
  a cached `LabelledRelayList` closes/refuses it. Requires completed M1 Phase C endpoint and a test
  environment that permits local QUIC sockets.

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

### Fix: Thread Relay CRL through every Connector dial path
**Issue:** Relay selection, probing, migration, and persistent sessions all construct relay TLS
connections. Updating only the verifier constructor would leave alternate dial paths unchecked.

**Fix Applied:** `CrlManager` is carried through `RelaySelectorConfig`, probe calls, session spawning,
`RelayClient::connect`, and `ExactRelaySpiffeVerifier`. Every relay handshake now fails closed when
CRL state is unavailable and rejects listed serials.

### Fix: Evict live relay connections after revocation
**Issue:** Handshake-only verification leaves already-established QUIC connections usable after a
new CRL revokes their certificate.

**Fix Applied:** Connector sessions monitor the authenticated relay serial and close within one
second of revocation or CRL expiry. Client cached connections now have the same active monitor;
the existing 60-second cache lifetime remains defense in depth.

### Fix: Preserve one Client CRL refresh lifecycle across tunnel restarts
**Issue:** ACL/transport changes restart the tunnel. Creating a new manager and refresh task inside
every `handle_up` would leak duplicate 60-second polling loops.

**Fix Applied:** The client stores the Relay CRL manager in `RuntimeState`, reuses it across tunnel
restarts, and makes background refresh startup idempotent.

### Fix: Update probe integration tests for fail-closed CRL enforcement
**Issue:** `probe_relays` gained the required Relay CRL manager argument, leaving four existing
integration tests uncompilable.

**Fix Applied:** The shared test PKI now generates and installs a verified, unexpired empty CRL;
all four probe tests pass it through the production API. They compile successfully; runtime socket
execution remains environment-gated.
