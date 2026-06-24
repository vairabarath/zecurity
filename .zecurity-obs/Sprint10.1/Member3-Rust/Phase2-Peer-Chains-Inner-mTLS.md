---
type: phase
sprint: 10.1
member: M3
phase: 2
status: planned
depends_on:
  - Sprint10.1-M3-Phase1
---

# M3 Phase 2 — Peer Chains & Inner mTLS

## What You're Building

Make Connector and Client present complete chains to the Relay, verify the
Relay's exact identity, and protect Relay-bridged payloads with inner
Client-to-Connector mTLS.

## Files to Touch

- `connector/src/relay_client.rs`
- `client/src/relay_pool.rs`
- `client/src/tunnel_pool.rs`
- Shared TLS helpers where appropriate

## Requirements

### Outer Relay QUIC

1. Connector sends `connector leaf + Workspace CA`.
2. Client sends `client leaf + Workspace CA`.
3. Both trust Platform Intermediate CA from their existing CA bundle.
4. Both require the configured exact Relay SPIFFE URI.
5. Neither peer receives or trusts a Relay-created self-signed CA.

### Inner Client-to-Connector mTLS

1. After Relay Lookup succeeds and streams are bridged, Connector acts as the
   inner TLS server and Client acts as the inner TLS client.
2. Reuse existing Client and Connector certificates and exact SPIFFE checks.
3. Require TLS 1.3.
4. Send `TunnelRequest`, `TunnelResponse`, and resource bytes only inside the
   inner TLS session.
5. Do not weaken the existing direct QUIC verification path.

## Tests

- Relay certificate with wrong SPIFFE ID is rejected.
- Leaf-only peer presentation is rejected by Relay.
- Inner mTLS rejects wrong-workspace and wrong-role certificates.
- Relay-captured bridged bytes do not contain a known plaintext TunnelRequest
  marker or payload marker.
- Direct QUIC path remains operational.

## Build Check

```bash
cd connector && cargo test && cargo build
cd client && cargo test && cargo build
```

## Post-Phase Fixes

*(Empty)*
