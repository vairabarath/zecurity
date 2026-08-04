---
type: phase
sprint: 16
stage: 1
phase: 2
owner: M3
depends_on: [Sprint16-Phase1]
status: not-started
tags: [sprint16, client, rust, identity, handshake]
---

# Sprint 16 · Phase 2 — Client Sends `resource_id`

> Goal: the client asserts **resource identity** on the tunnel handshake. Nothing new is computed —
> `resource_id` is already on the ACL entry the client used to build the route; this phase just
> carries it through to the wire.
> Depends on **Phase 1** (connector must accept the field first).

## Current state (verified)

- `client/src/net_stack.rs` (~109):
  ```rust
  #[derive(Serialize)]
  struct TunnelRequest {
      destination: String,
      port: u16,
      protocol: String,
  }
  ```
  built in `relay_tcp_to_quic(...)` (~467) from the `destination` string + port.
- `client/src/daemon.rs` — `build_transports_by_resource(...)` returns
  `HashMap<(Ipv4Addr, u16), Option<Vec<Arc<ClientTransport>>>>`, consumed by
  `net_stack::run(dev, allowed_entries, transports, relay_resync)`.
- **The identity is already in hand:** the map is built by iterating ACL entries, and
  `AclEntry.resource_id` is populated. It is simply dropped on the floor today.

## The one real design choice

The transports map is keyed by `(Ipv4Addr, u16)` and its value carries only the transport list. To get
`resource_id` to `relay_tcp_to_quic`, the **value** must carry it. Two options:

| Option | Shape | Verdict |
|---|---|---|
| **A (recommended)** | value becomes a small struct: `ResourceTarget { resource_id: String, transports: Vec<Arc<ClientTransport>> }` | Keeps one map, one lookup, no desync risk. Sets up Stage 2 (synthetic IP → target) cleanly. |
| B | a second parallel map `(Ipv4Addr,u16) → String` | Two structures to keep in sync; a missing entry becomes a silent auth failure. Avoid. |

Take **Option A**. Note the existing three-state semantics must be preserved exactly:

```text
Some(target with transports)  → managed resource, connector online  → tunnel
Some(target, transports empty)→ managed resource, connector offline → FAIL CLOSED
None (absent)                → unmanaged traffic                   → no tunnel route
```

## Tasks

### 2.1 — Carry `resource_id` in the transports map
`client/src/daemon.rs`

- [ ] Introduce `ResourceTarget { resource_id: String, transports: Vec<Arc<ClientTransport>> }`
      (`pub(crate)` so `daemon_tests.rs` can construct it).
- [ ] `build_transports_by_resource(...)` populates `resource_id` from the ACL entry it is already
      iterating. **Do not re-derive or look it up again.**
- [ ] Preserve the `Some(empty) = fail closed` vs `None = unmanaged` distinction — losing it turns a
      fail-closed case into an unmanaged passthrough (a security regression).
- [ ] Update the existing tests in `client/src/daemon_tests.rs` that assert on the map shape
      (`build_transports_*`, `connector_with_relay_addr_*`, `two_connectors_*`).

### 2.2 — Send it on the handshake
`client/src/net_stack.rs`

- [ ] `TunnelRequest` += `resource_id: String`.
- [ ] Thread the target through `run(...)` → the smoltcp flow setup → `relay_tcp_to_quic(...)` so the
      request is built with the id for **that** flow.
- [ ] Keep sending `destination` this phase — the connector cross-checks it
      (`destination_mismatch` denial). It is removed only if/when Stage 2 drops it.
- [ ] Do **not** change `ClientTransport`, `relay_pool.rs`, or `transport.rs` — transport selection is
      unrelated to identity, and the relay never sees this handshake.

## Build gate

```bash
cd client && cargo build && cargo test
```

Expect the existing suite to stay green — baseline is **38 tests** (verified 2026-07-28, after the
PENDING-02 merge). If a `build_transports_*` test fails, it is the map-shape change — update the test,
don't weaken the assertion.

## Verify (manual)

- [ ] Tunnel opens to an existing IP resource; the connector logs `auth_path=resource_id`
      (proves the new field is being read, not the legacy fallback).
- [ ] A shield-routed resource still goes via the shield.
- [ ] Connector-offline case still **fails closed** (empty transports), not passthrough.
- [ ] Unmanaged traffic is still untunnelled.

## Then

Proceed to **Phase 3** (in [[Sprint16/Member3-Go-Rust/Phase1-Connector-Identity-Handshake]]) to make
`resource_id` **mandatory** on the connector and delete the legacy destination path — that closes
**GATE 1** and is the Stage 1 merge point.

## Notes

- This phase is intentionally tiny. If it grows beyond the two files above, something is being
  over-built: Stage 1 is only "carry the identity you already have."
- Stage 2's synthetic-IP work will replace the map **key** (`Ipv4Addr` → synthetic IP) while keeping
  this same `ResourceTarget` **value** — which is why Option A above is the right shape now.
