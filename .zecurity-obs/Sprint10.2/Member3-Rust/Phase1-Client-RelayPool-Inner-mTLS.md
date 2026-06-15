---
type: task
status: planned
sprint: 10.2
member: M3
phase: 1
depends_on:
  - Sprint10.2-M2-Phase1
unlocks:
  - Sprint10.2-M3-Phase2
---

# M3 Phase 1 — Client RelayPool & Inner mTLS

## Goal

Create the Client-side counterpart to Connector `RelayHandler`.

```text
Client RelayPool
  -> outer Relay QUIC mTLS
  -> Lookup(connector_id)
  -> Relay ACK
  -> inner Client-to-Connector TLS 1.3 mTLS
  -> authenticated stream ready for TunnelRequest
```

## Files

- `client/src/relay_pool.rs` — new
- `client/src/main.rs`
- `client/src/tunnel_pool.rs` — shared authenticated stream type/helpers
- `client/Cargo.toml` only if required

## Outer Relay QUIC Requirements

1. Present Client leaf plus Workspace CA.
2. Trust only the Platform Intermediate CA from the saved CA bundle.
3. Require exact `relay_spiffe_id`.
4. Use ALPN `ztna-relay-v1`.
5. Pool one healthy QUIC connection per Relay address.
6. Resolve Relay DNS again after connection failure.

## Lookup Protocol

Send 4-byte big-endian length-prefixed JSON:

```json
{"type":"lookup","connector_id":"<uuid>"}
```

Read and validate the framed Relay ACK. Do not start inner TLS after a negative
ACK.

## Inner mTLS Requirements

1. Run `tokio-rustls` Client TLS over the Relay-bridged QUIC stream.
2. Require TLS 1.3.
3. Present Client leaf plus Workspace CA.
4. Trust the Client's Workspace CA.
5. Require exact `connector_spiffe`, not a Connector-role prefix.
6. Return a common authenticated bidirectional stream.
7. Send no `TunnelRequest` bytes before inner TLS succeeds.

## Tests

- Wrong Relay SPIFFE is rejected.
- Negative/malformed/oversized ACK is rejected.
- Wrong Connector SPIFFE is rejected.
- Wrong-workspace Connector is rejected.
- Client presents a complete outer and inner certificate chain.
- Framed Lookup JSON matches Relay protocol.

## Build Check

```bash
cd client
cargo test
cargo build
```

