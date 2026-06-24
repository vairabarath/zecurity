---
type: phase
status: planned
sprint: 10.3
member: M3
phase: 2
depends_on:
  - Sprint10.3-M2-Phase1
unlocks:
  - Sprint10.3-Shared-Phase1
---

# M3 Phase 2 — Certificate Lifecycle & Trust-Bundle Hardening

## Goal

Make Relay and Connector certificate renewal reliable without restart, prevent
partial state writes, and remove trust decisions based on PEM ordering.

## Current Risks

- Relay provisioning reuses certificate files based only on file existence.
- Relay certificate material is written directly to final paths.
- Relay has no proactive renewal and runtime TLS reload.
- Connector Relay registration and `RelayHandler` retain certificate bytes
  loaded before Connector renewal.
- Connector treats the first certificate as Workspace CA and the last as
  Platform Intermediate CA.

## Requirements

1. On Relay startup, validate:
   - Relay leaf exact SPIFFE identity and configured Relay ID.
   - Relay leaf/private-key match.
   - Relay leaf chain to the configured Intermediate CA.
   - Certificate validity and configurable renewal window.
2. Write key and certificates through restrictive temporary files, flush as
   required, and atomically rename complete material into place.
3. Never replace the last known-good material with a partial or invalid set.
4. Add Relay renewal using authenticated provisioning/renewal semantics and
   reload the QUIC listener certificate without process restart.
5. Make Connector certificate renewal notify/rebuild:
   - Persistent Relay registration TLS material.
   - `RelayHandler` inner TLS server material.
6. Reconnect Relay registration after renewed Connector material is active.
7. Parse CA certificates and select them using Basic Constraints,
   subject/issuer relationships, and signature-chain validation.
8. Reject ambiguous, unrelated, duplicate, or malformed CA bundles.
9. Replace duplicated raw bundle parameters with a typed, validated certificate
   material structure where practical.

## Required Tests

- Existing expired or wrong-identity Relay certificate triggers renewal/fails
  safely.
- Mismatched Relay key and certificate fail before listener startup.
- Interrupted write preserves previous valid certificate material.
- Connector renewal updates Relay registration and inner TLS without restart.
- Relay renewal updates presented certificate without restart.
- Reordered valid CA bundle succeeds through relationship-based selection.
- Unrelated or ambiguous CA bundle fails closed.

## Build Check

```bash
cd relay
cargo test
cargo build
cd ../connector
cargo test
cargo build
```

