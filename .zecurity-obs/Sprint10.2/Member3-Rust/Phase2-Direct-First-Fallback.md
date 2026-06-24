---
type: task
status: planned
sprint: 10.2
member: M3
phase: 2
depends_on:
  - Sprint10.2-M3-Phase1
unlocks:
  - Sprint10.2-M3-Phase3
---

# M3 Phase 2 — Direct-First Relay Fallback

## Goal

Preserve direct Client-to-Connector QUIC as the preferred path and use Relay
only when direct stream establishment fails or exceeds two seconds.

## Files

- `client/src/tunnel_pool.rs`
- `client/src/net_stack.rs`
- `client/src/daemon.rs`
- `client/src/main.rs`

## Required Refactor

Direct QUIC currently returns concrete QUIC send/receive halves. Relay returns
an inner-TLS stream over bridged QUIC halves. Introduce one common authenticated
bidirectional stream interface used by `net_stack`.

The common stream must support:

- async read
- async write
- splitting for concurrent resource traffic relay
- `Send + Unpin + 'static`

## Fallback Rules

1. Attempt direct stream first.
2. Limit direct stream establishment to two seconds.
3. Use Relay only after direct connection error or timeout.
4. Do not use Relay after:
   - Connector identity validation failure
   - certificate revocation
   - ACL denial
   - malformed TunnelResponse
5. If Relay discovery is incomplete, remain direct-only.

## Daemon Wiring

During `Up`:

1. Read Relay and Connector discovery fields from ACL snapshot.
2. Build optional `RelayPool` only when all four fields are non-empty.
3. Thread it through `net_stack::run`.
4. Keep existing Connector config fallback for direct address.

## Tests

- Direct success never contacts Relay.
- Direct timeout uses Relay.
- Direct immediate connection error uses Relay.
- Missing RelayPool returns the direct error.
- Policy denial is returned without Relay retry.
- Existing direct QUIC behavior remains operational.

## Build Check

```bash
cd client
cargo test
cargo build
```

