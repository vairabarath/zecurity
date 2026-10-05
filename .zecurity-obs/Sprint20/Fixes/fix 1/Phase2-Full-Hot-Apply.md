---
type: fix-phase
sprint: 20
fix: 1
phase: 2
title: Full Hot-Apply
status: not-implemented
depends_on: [1]
blocked_by: [Q3]
component: client
tags:
  - client
  - tunnel
  - net-stack
  - fix-01
---

# Fix 01 · Phase 2 — Full Hot-Apply

> Parent: [[Fix01-Client-Tunnel-Restart-On-Snapshot-Change]].
> Status: **NOT IMPLEMENTED — future stage after Phase 1 validation and the Q3 decision.**

Phase 2 is the larger architectural change. It must **not** be implemented as part of Phase 1.

> **Split (2026-10-05):** Phase 2 runs in slices. **2-A — connector topology hot-apply
> (transport-map swap only)** → [[Phase2A-Connector-Topology-Hot-Apply]] (status: live-accepted 2026-10-05, [[Phase2A-Live-Acceptance-2026-10-05]]). Resource/route/listener hot-apply, flow ownership/selective close and the P1-A
> handshake-timeout fix stay in this umbrella doc for later slices.

Goal: keep unaffected long-lived flows alive even when the configuration genuinely changes.

## Preconditions

- Phase 1 is implemented, tested, and live-validated (U5/G9).
- Q3 (how the client handles vN+1 during connector renewal) is resolved using Phase 1 live evidence.
  See the parent doc, "⚠ Unresolved design decision / risk: vN+1".

## Scope

- **Update channel** from the daemon into `net_stack::run`, polled without blocking in the smoltcp
  loop, so the running data plane accepts updates without destroying the TUN/interface/`SocketSet`.
- **Device-agnostic, testable net-stack update handling.** `net_stack::run` currently requires a
  real `tun::AsyncDevice`, so the loop core needs a harness.
- **Listener / interface-address add/remove** for added/removed resources.
- **Incremental TUN route add/remove** (nft rules, `ip route replace`/`del`) without the global
  `cleanup_policy_routes()` flush (`tun.rs:63`).
- **Transport configuration updates for new flows**: swap the transport map. Existing flows keep
  their own `Arc<ClientTransport>` / QUIC connection.
- **`transport.rs` connector ownership:** `ClientTransport` gains `connector_id` (today only the
  optional `RelayContext` has it, `transport.rs:44-57`).
- **Flow → connector tracking:** `ActiveRelay` records the chosen connector plus a per-flow close
  handle (today it has neither, `net_stack.rs:199-206`, `:480-555`).
- **Close only flows affected** by a connector/resource that is removed *permanently*, subject to the
  Q3 decision for transient vN+1 removal.
- **Keep unaffected existing flows alive.**
- **New flows use the new transport configuration.** A resource with no valid connector keeps failing
  closed for new flows (`Some(None)`, `net_stack.rs:360-364`).
- **Fallback:** if an incremental route change or apply fails, do a full restart (fail-closed).
- A full down/up stays for genuinely structural changes.
- Revocation semantics must not be weakened: a resource or access removal must still close its
  flows.
- Live U5/G9 validation after Phase 2.

## Files (expected)

| File | Change |
|---|---|
| `client/src/daemon.rs` | hot-apply dispatch from the Phase 1 decision point |
| `client/src/runtime.rs` | `TunHandle` carries the data-plane update sender |
| `client/src/net_stack.rs` | update receiver in `run`; listener/address add/remove; transport-map swap; per-flow connector tracking + selective close; device-agnostic loop core |
| `client/src/tun.rs` | incremental flow/route add/remove without the global flush |
| `client/src/transport.rs` | `ClientTransport` carries `connector_id` |
| net-stack / transport tests + test harness | Phase 2 tests |

Not expected to change: `tunnel_pool.rs` (QUIC connections already outlive the transport map,
`tunnel_pool.rs:320-372`; list it only if repo evidence later proves it's needed), `relay_pool.rs`,
`crl.rs`, `proto/`, all controller code (including the notifier files).

## Tests

These go in the `client/src/net_stack.rs` tests and need the device-agnostic loop harness.
`MockDirect`/`MockRelay` in `transport.rs` can be reused.

- An ACL change that keeps an existing flow's resource → the flow stays alive.
- A transport-only change → the existing flow stays alive.
- A connector disappears permanently → only dependent flows close.
- Unaffected flows stay alive across any of the above.
- New flows use the newly applied transport configuration.
- TUN routes are added/removed incrementally (`tun.rs`), with no global flush.
- A failed hot-apply falls back to a full restart.
- A renewal sequence (with the Q3 vN+1 behaviour) doesn't reset a long-lived TCP connection.

## Acceptance — **Not yet implemented / Not yet satisfied**

- [ ] Existing unaffected long-lived flows survive applicable configuration changes.
- [ ] Removing a connector/resource closes only the flows that depend on it.
- [ ] New flows use the updated transport configuration.
- [ ] Incremental route changes work.
- [ ] A failed hot-apply falls back safely to a full restart.
- [ ] Phase 2 unit tests pass (`cd client && cargo build && cargo test`).
- [ ] **Live, PENDING / NOT YET RUN:** U5 and G9 pass after Phase 2.

## Input from Phase 1 live run (2026-10-02)

Read `Phase1-AppliedConfig-Restart-Decision.md` → **Finding P1-A** first. When a connector is lost without a clean shutdown, its pooled QUIC connection stays "open" for up to the quinn idle timeout, and every new tunnel pays the 5 s `TUNNEL_HANDSHAKE_TIMEOUT` before falling through to the next connector. Today the full VPN restart flushes that dead transport as a side effect. Hot-apply removes the restart, so Phase 2 must explicitly drop the transports for removed connectors when it applies a change, and should evict or mark-failed a pooled connection after a handshake timeout. Fix-connector (`6362d1f`) means renewals no longer bump versions; connector loss/return (v+1 on disconnect, v+1 on shield move, v+1 on return) is now the main restart trigger Phase 2 must handle.
