---
type: phase
sprint: 10.1
member: M3
phase: 1
status: planned
depends_on:
  - Sprint10.1-M2-Phase1
  - Sprint10.1-M2-Phase2
---

# M3 Phase 1 — Relay Multi-Workspace mTLS

## What You're Building

Configure the Relay to authenticate Connector and Client chains from all
workspaces using the Platform Intermediate CA as its only client trust anchor.

## Files to Touch

- `relay/src/main.rs`
- `relay/src/listener.rs`
- `relay/src/session.rs`
- `relay/src/spiffe.rs`
- Relay TLS integration tests

## Requirements

1. Load `RELAY_TLS_CERT`, `RELAY_TLS_KEY`, and `RELAY_CLIENT_CA`.
2. Require peer certificates during QUIC TLS handshake.
3. Trust only the configured Platform Intermediate CA.
4. Require peers to present `leaf + Workspace CA`.
5. After chain verification, require exact SPIFFE formats:

```text
spiffe://<workspace-domain>/connector/<uuid>
spiffe://<workspace-domain>/client_device/<uuid>
```

6. On Register, require message `connector_id` and `spiffe_id` to exactly match
   the verified certificate.
7. On Lookup, require Client and stored Connector trust domains to match.
8. Never accept a SPIFFE URI from JSON as proof of identity.

## Test Matrix

- Valid Connector from Workspace A registers.
- Valid Connector from Workspace B registers.
- Valid Client A can look up Connector A.
- Client A cannot look up Connector B.
- Self-signed, unknown Workspace CA, leaf-only, expired, wrong-EKU, malformed
  SPIFFE, and message-identity mismatch cases fail.

## Build Check

```bash
cd relay
cargo test
cargo build
```

## Post-Phase Fixes

*(Empty)*
