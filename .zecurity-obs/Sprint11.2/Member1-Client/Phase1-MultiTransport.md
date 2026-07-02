---
type: phase
member: M1
sprint: 11.2
phase: 1
title: Client — Multiple Transports & Multi-Connector Failover
status: completed
commits: f88600c, 8f9235f
depends_on:
  - Sprint11.2/Member2-Go/Phase1-Proto
  - Sprint11.2/Member2-Go/Phase2-Compiler
---

# Phase 1 — Client: Multiple Transports & Multi-Connector Failover

## Goal

Build a transport list per resource instead of a single transport. Try connectors
in priority order (preferred first) when opening tunnel streams. Switch the
tunnel handshake to framed JSON so partial reads cannot corrupt the protocol.

## Files

| File | Change |
|---|---|
| `client/src/daemon.rs` | `build_transports_by_resource` returns `Vec<Arc<ClientTransport>>`; add `ordered_connectors_for_entry` |
| `client/src/net_stack.rs` | `relay_tcp_to_quic` accepts `Vec`; failover loop; `write_framed_json` / `read_framed_json` |

## daemon.rs

### Transport map type change

```rust
// Before:
HashMap<(Ipv4Addr, u16), Option<Arc<ClientTransport>>>

// After:
HashMap<(Ipv4Addr, u16), Option<Vec<Arc<ClientTransport>>>>
```

### ordered_connectors_for_entry

```rust
fn ordered_connectors_for_entry<'a>(
    entry: &AclEntry,
    rn: &'a AclRemoteNetwork,
) -> Vec<&'a AclConnector> {
    // preferred connector first (if set and present),
    // then remaining connectors in declaration order
}
```

### Transport cache

Per-connector transport objects are cached by connector address within a single
`build_transports_by_resource` call. Multiple resources in the same RN sharing
a connector do not rebuild the TLS pool.

## net_stack.rs

### Framed handshake helpers

```rust
const MAX_TUNNEL_HANDSHAKE_SIZE: usize = 16 * 1024;

async fn write_framed_json<W, T>(writer: &mut W, value: &T) -> Result<()>
// 4-byte BE length + JSON body; errors if body > 16 KB

async fn read_framed_json<R, T>(reader: &mut R) -> Result<T>
// reads 4-byte length, validates <= 16 KB, reads body, deserializes
```

### Failover loop in relay_tcp_to_quic

```rust
async fn relay_tcp_to_quic(
    transports: Vec<Arc<ClientTransport>>,  // was: single Arc<ClientTransport>
    ...
) -> Result<()> {
    for transport in transports {
        match transport.open_authenticated_stream().await {
            Ok(stream) => { /* use this stream */ break }
            Err(e) => { warn!(...); continue }  // try next
        }
    }
}
```

## Implementation Checklist

- [x] **M1-C1** `daemon.rs` — return type of `build_transports_by_resource` changed to `Vec<Arc<ClientTransport>>`
- [x] **M1-C2** `daemon.rs` — `ordered_connectors_for_entry()`: preferred first, rest in order
- [x] **M1-C3** `daemon.rs` — per-call transport cache; same connector addr → same TLS pool
- [x] **M1-C4** `net_stack.rs` — `relay_tcp_to_quic` iterates transport list; logs warn on failure, continues to next
- [x] **M1-C5** `net_stack.rs` — `write_framed_json` / `read_framed_json` helpers (16 KB max)
- [x] **M1-C6** `net_stack.rs` — tunnel handshake send uses `write_framed_json`
- [x] **M1-C7** `daemon_tests.rs` — updated tests for new transport list return type
- [x] **Build gate:** `cd client && cargo build` passes
