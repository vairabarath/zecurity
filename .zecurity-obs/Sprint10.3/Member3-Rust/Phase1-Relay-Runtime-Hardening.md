---
type: phase
status: in-progress
sprint: 10.3
member: M3
phase: 1
depends_on: []
unlocks:
  - Sprint10.3-Shared-Phase1
---

# M3 Phase 1 — Relay Runtime Resource & Routing Hardening

## Goal

Keep authenticated but slow, broken, or excessive peers from exhausting Relay
and Connector resources, and make Lookup failures deterministic.

## Current Risks

- Relay spawns one task per accepted connection and Lookup without an explicit
  bound.
- Initial Relay messages and Connector inner TLS handshakes have no deadline.
- Closed Connector registrations may remain visible until asynchronous cleanup
  completes.
- A Connector `open_bi()` failure ends Lookup without a structured negative
  ACK.
- Relay SPIFFE parsing accepts uppercase UUID text despite documenting
  canonical lowercase UUIDs.

## Requirements

1. Configure explicit QUIC idle timeout, keepalive, and maximum concurrent
   bidirectional streams.
2. Bound accepted Relay connections and active Lookup bridge tasks using
   semaphores or equivalent admission control.
3. Add deadlines for:
   - Initial Register/Lookup stream acceptance.
   - Framed Register/Lookup message reads.
   - Connector inner TLS handshake.
4. Release admission permits on every success, error, timeout, and disconnect.
5. Check Connector connection health during Lookup.
6. Evict a registration when its connection is closed or `open_bi()` proves it
   unusable, without deleting a newer replacement registration.
7. Return negative ACKs for unavailable Connector, cross-workspace Lookup,
   admission rejection, and Connector stream-open failure when the Client
   stream remains writable.
8. Require canonical lowercase hyphenated UUID text in Relay SPIFFE parsing.
9. Add metrics/log fields for rejection reason, active connections, active
   bridges, and timeout class.

## Required Tests

- Slow first-message peer times out.
- Slow Connector inner TLS handshake times out.
- Connection and Lookup limits reject excess work without task growth.
- Permits are released after disconnect and failed handshake.
- Closed Connector entry is evicted and Lookup receives a negative ACK.
- Replaced Connector registration is not deleted by stale cleanup.
- Uppercase UUID SPIFFE identity is rejected.

## Build Check

```bash
cd relay
cargo test
cargo build
cd ../connector
cargo test
cargo build
```

## Implementation Progress

### Completed: Runtime Timeouts and Concurrency Limits

- Relay QUIC transport now has explicit idle timeout, keepalive, and maximum
  incoming bidirectional streams.
- Relay refuses connections above `RELAY_MAX_CONNECTIONS`.
- Relay rejects Lookup bridges above `RELAY_MAX_LOOKUP_BRIDGES` with a negative
  ACK.
- Relay applies deadlines to QUIC handshake, initial stream acceptance, and
  framed Register/Lookup messages.
- Connector rejects Relay tunnel streams above
  `RELAY_MAX_TUNNEL_STREAMS` and applies
  `RELAY_INNER_HANDSHAKE_TIMEOUT_SECS` to inner TLS mTLS.
- Connector outer QUIC advertises the same incoming stream limit and applies
  `RELAY_IDLE_TIMEOUT_SECS`.
- Admission permits remain held for the complete connection, Lookup bridge, or
  Connector Relay tunnel task and release automatically on every exit path.

Relay runtime environment variables:

```text
RELAY_MAX_CONNECTIONS          default 1024
RELAY_MAX_LOOKUP_BRIDGES       default 4096
RELAY_MAX_BIDI_STREAMS         default 128 per connection
RELAY_IDLE_TIMEOUT_SECS        default 60
RELAY_HANDSHAKE_TIMEOUT_SECS   default 10
RELAY_MESSAGE_TIMEOUT_SECS     default 10
```

Connector runtime environment variables:

```text
RELAY_MAX_TUNNEL_STREAMS             default 256
RELAY_INNER_HANDSHAKE_TIMEOUT_SECS   default 10
RELAY_IDLE_TIMEOUT_SECS              default 60
```

Remaining in this phase:

- Stale Connector registration eviction.
- Structured negative ACK for Connector stream-open failure.
- Canonical lowercase Relay SPIFFE UUID enforcement.
- Full slow-peer and load integration tests.
