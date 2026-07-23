---
type: phase
member: M3
sprint: 13
phase: 4
title: Client Transport Recovery Hardening
status: done
depends_on:
  - Sprint13/Member3-Go-Rust/Phase3-Client-Transport-Cache
tags: [rust, client, transport, recovery, concurrency, acl-decoupling, pending-03]
---

# Phase 4 — Client transport recovery hardening

> Hardens the single-controller implementation delivered in Phase 3. This phase does not alter the
> TransportSnapshot architecture or introduce persistent/global versioning for multiple controller
> replicas.

## Goal

Close four client-side correctness races in transport synchronization and recovery:

1. Serialize the complete transport `known_version → fetch → store` transaction.
2. Serialize tunnel down/up lifecycle transitions.
3. Measure relay-recovery cooldown from recovery completion.
4. Signal transport resynchronization only for connectivity failures, while treating malformed
   authenticated responses, denials, and identity failures as fail-closed protocol/security events.

## Why

The transport plane is already separated from ACL authorization, but its scheduler and early
relay-recovery paths run concurrently. Without coordination, an older response can overwrite a
newer snapshot, ACL and transport changes can overlap tunnel restarts, and a long recovery can start
another burst immediately after finishing. The handshake path also currently classifies JSON and
frame-size errors as transport failures, causing unnecessary topology re-polls.

## Files

| File | Change |
|------|--------|
| `client/src/runtime.rs` | Add shared `transport_sync_lock` and `tunnel_restart_lock` mutexes and initialize them in `new_shared()` |
| `client/src/daemon.rs` | Hold the transport lock across known-version read, network fetch, and snapshot store; serialize tunnel restarts; introduce shared completion-based recovery state |
| `client/src/net_stack.rs` | Add typed `FramedJsonError` and classify only I/O errors/timeouts as transport failures |
| `client/src/daemon.rs` tests | Cover recovery single-flight and completion-based cooldown; add a concurrent transport-sync regression test through an injectable fetch seam |
| `client/src/net_stack.rs` tests | Cover malformed JSON, disconnected I/O, and oversized frame classification |

## F1 — Serialize transport synchronization

Add `transport_sync_lock: Arc<tokio::sync::Mutex<()>>` to `RuntimeState`. In
`fetch_and_store_transport`, clone and acquire it before reading `known_version`, and retain the
guard until the fetch result and `transport_last_sync_at` are stored.

The lock must cover the entire operation. A store-only lock does not prevent two requests from
reading the same old version and completing out of order.

## F2 — Serialize tunnel restarts

Add `tunnel_restart_lock: Arc<tokio::sync::Mutex<()>>` to `RuntimeState`. Acquire it at the start of
`restart_tunnel_if_running`, then re-check `tun_slot` after obtaining the lock before executing
`handle_down` followed by `handle_up`.

This makes the down/up pair one lifecycle transition and prevents ACL polling and transport
recovery from replacing or aborting each other's tunnel.

## F3 — Start cooldown when recovery finishes

Replace the scheduler-local start timestamp and atomic flag with shared state:

```rust
#[derive(Debug, Default)]
struct TransportRecoveryState {
    running: bool,
    last_finished_at: Option<tokio::time::Instant>,
}
```

Extract `may_start_transport_recovery` so the decision is deterministic and unit-testable. Set
`running = true` before spawning recovery. When `run_transport_recovery` returns, set
`running = false` and record `last_finished_at = Some(Instant::now())`.

Required ordering:

```text
recovery finishes → cooldown begins → next recovery may start
```

## F4 — Security-harden handshake failure classification

Introduce a private `FramedJsonError` in `client/src/net_stack.rs` with distinct `Io`, `Encode`,
`Decode`, and `FrameTooLarge` variants. Change `write_framed_json` and `read_framed_json` to return
that type.

The current implementation treats framed QUIC stream I/O errors as transport failures. This is
safe for the present call path because these helpers operate on the authenticated relay/connector
stream, but it is broader than an explicit connectivity-kind allowlist. Tightening it to the
following remains an open hardening item:

```rust
impl FramedJsonError {
    fn is_transport_failure(&self) -> bool {
        matches!(
            self,
            Self::Io(error)
                if matches!(
                    error.kind(),
                    std::io::ErrorKind::ConnectionReset
                        | std::io::ErrorKind::ConnectionAborted
                        | std::io::ErrorKind::BrokenPipe
                        | std::io::ErrorKind::UnexpectedEof
                        | std::io::ErrorKind::TimedOut
                        | std::io::ErrorKind::NotConnected
                )
        )
    }
}
```

The outer `tokio::time::timeout` expiry remains an explicit transport failure.

### Required security classification

| Failure | Classification | Trigger transport refresh? |
|---------|----------------|----------------------------|
| Handshake timeout | Transport failure | Yes |
| Connection reset, aborted connection, broken pipe, unexpected EOF, timed out, or not connected | Transport failure | Yes |
| Malformed authenticated JSON response | Protocol failure | No |
| Oversized or invalid frame | Protocol failure | No |
| Connector or ACL denial (`resp.ok == false`) | Policy/connector denial | No |
| Certificate or SPIFFE authentication failure | Authentication failure; fail closed | No |
| Other local/non-connectivity I/O error | Local error | No |

Only these conditions trigger `relay_resync`:

- connectivity-related framed-handshake I/O errors;
- handshake timeout;
- existing relay/connector transport failures.

JSON encoding/decoding and oversized frames are protocol failures. Log them and try the next
candidate, but do not mark them as transport failures. The client already depends on `thiserror =
"1"`; do not add or upgrade the dependency solely for this change.

The existing typed `TunnelOpenError::Authenticate` path remains fail closed. Do not convert
certificate or SPIFFE failures into relay discovery/recovery signals. Likewise, a decoded
`TunnelResponse` denial must not set `saw_transport_failure`; `SHIELD_NOT_ATTACHED` may try another
authorized candidate, while other denials return an error without refreshing topology.

### Security rationale

An authenticated peer can still send malformed application data. Treating that data as evidence of
a stale relay topology creates a recovery-amplification path: repeated malformed responses can force
unnecessary controller polling and tunnel churn. Typed classification confines recovery to failures
that can plausibly be repaired by fetching new transport coordinates, while identity and policy
failures remain fail closed.

## Tests

- `malformed_handshake_json_is_not_transport_failure`
- `disconnected_handshake_is_transport_failure`
- `oversized_handshake_is_not_transport_failure`
- connectivity-related `std::io::ErrorKind` values trigger refresh; unrelated I/O kinds do not
- connector/ACL denial does not notify `relay_resync`
- certificate/SPIFFE authentication failure fails closed and does not notify `relay_resync`
- `recovery_is_single_flight`
- `cooldown_is_measured_after_recovery_finishes`
- Concurrent transport sync: request A starts first and completes last; request B cannot read its
  `known_version` until A stores, and the final cached snapshot remains the newer version.
- Tunnel restart serialization: two concurrent restart requests cannot overlap their down/up
  lifecycle transitions.

## Build check

```bash
cd client
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test
```

If strict Clippy is blocked by pre-existing repository-wide warnings, record those warnings and
verify that this phase introduces none.

## Implementation checklist

- [x] **F1** serialize the full transport fetch/read/store operation
- [x] **F2** serialize tunnel restarts and re-check running state after locking
- [x] **F3** use shared recovery state and completion-based cooldown
- [x] **F4** connectivity I/O is allowlisted; protocol, denial, authentication, and unrelated I/O failures do not trigger transport recovery
- [x] **Security tests:** malformed/oversized frames, connectivity and unrelated I/O kinds, plus typed certificate/SPIFFE authentication classification are covered
- [x] **Tests:** recovery timing/single-flight, transport ordering, and shared tunnel-restart serialization are covered
- [x] **Build gate:** formatting, client build, and client tests pass

## Out of scope

- Persistent/global transport versions across controller restarts
- Multi-controller-replica version coordination
- Distributed cache invalidation or leader election

## Post-Phase Fixes

### Fix: Connectivity-specific framed-I/O classification

**Issue:** The initial typed handshake implementation treated every framed-stream I/O error as a
transport failure, which could trigger unnecessary recovery for unrelated/local I/O conditions.

**Fix applied:** `FramedJsonError::is_transport_failure()` now allowlists connection reset,
connection aborted, broken pipe, unexpected EOF, timeout, and not-connected. Malformed JSON,
oversized frames, encoding failures, unrelated I/O, denials, and typed authentication failures do
not enter transport recovery.

**Verification:** Classification, authentication, recovery single-flight/cooldown, transport
ordering, and restart-lock regression tests pass. The complete Client suite passes 43/43.
