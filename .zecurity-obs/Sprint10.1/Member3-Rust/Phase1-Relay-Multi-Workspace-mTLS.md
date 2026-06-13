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
   - `RELAY_TLS_KEY` is the Relay-host-generated private key.
   - `RELAY_TLS_CERT` and `RELAY_CLIENT_CA` are returned by Controller PKI after
     signing the Relay-generated CSR.
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
9. Fail startup if the Relay leaf does not match the configured private key or
   expected `RELAY_SPIFFE_ID`.
10. Do not generate, load, or require any CA private key in the Relay process.

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

### Relay Multi-Workspace TLS Configuration

- Added `relay/src/tls.rs` to build a QUIC/rustls server configuration that
  requires peer certificates and trusts only the provisioned Platform
  Intermediate CA.
- Relay startup now validates that its provisioned leaf has the expected Relay
  SPIFFE ID and matches its private key.
- Added post-handshake peer identity extraction that requires peers to present
  `leaf + Workspace CA` and accepts only exact Connector or Client-device
  SPIFFE roles.
- Added focused tests for exact Relay identity, key mismatch, leaf-only peer
  rejection, disallowed roles, and Connectors from two different workspaces.
- Wired the QUIC listener into Relay startup using `RELAY_BIND`, defaulting to
  `0.0.0.0:9093`.
- The listener rejects connections unless their authenticated certificate
  identity resolves to an allowed Connector or Client-device SPIFFE role.
- Added `session.rs` Register and Lookup handling:
  - Connector registration ID and SPIFFE ID must exactly match the authenticated
    Connector certificate.
  - Connector registrations are removed when their QUIC connection closes.
  - Client Lookup requires the authenticated `client_device` role and the same
    workspace trust domain as the registered Connector.
  - Reused Client QUIC connections can carry multiple concurrent Lookup streams.
  - Successful Lookup streams are bridged bidirectionally without interpreting
    tunneled payload bytes.

Current startup environment:

```text
RELAY_BIND=0.0.0.0:9093
```
