# Relay E2E Findings

Source review: `.zecurity-obs/Relay-E2E-Flow-and-Security-Review.md`

This document tracks the current implementation status of the relay/client/controller findings.

## Implemented Findings

### F5 — Probe Rate-Limit Key Is Attacker-Controlled

**Status:** Implemented.

**Original issue:** Relay probe rate limiting was keyed by `connector_id` from the Probe message body. A misbehaving authenticated connector could send probes with arbitrary connector IDs to bypass rate limits and grow the tracker map.

**Implemented fix:**
- `relay/src/session.rs` now validates Probe body `connector_id` against the authenticated mTLS SPIFFE identity.
- Rate limiting is keyed by `identity.entity_id`, not untrusted message body data.
- Stale probe rate windows are pruned.
- Added tests for connector identity mismatch and client-role rejection.

**Files:**
- `relay/src/session.rs`

**Verification:**
- `cargo test --manifest-path relay/Cargo.toml session`

### F6 — Client Relay Fallback Missing Timeouts

**Status:** Implemented.

**Original issue:** Relay fallback could stall indefinitely during relay stream establishment or connector tunnel handshake.

**Implemented fix:**
- `client/src/transport.rs` adds a bounded relay fallback timeout.
- `client/src/net_stack.rs` bounds TunnelRequest/TunnelResponse handshake with `TUNNEL_HANDSHAKE_TIMEOUT`.

**Files:**
- `client/src/transport.rs`
- `client/src/net_stack.rs`

**Verification:**
- `cargo check --manifest-path client/Cargo.toml`

### F7 — Unbounded Client Net Stack Queues/Buffers

**Status:** Implemented.

**Original issue:** `client/src/net_stack.rs` used unbounded TUN and per-flow channels plus an uncapped per-flow write buffer.

**Implemented fix:**
- TUN input/output queues are bounded.
- Per-flow TCP-to-connector and connector-to-TCP queues are bounded.
- smoltcp loop uses `try_send` and closes overloaded flows.
- Both `write_buf.extend(...)` paths are guarded by `FLOW_WRITE_BUF_CAP`.

**Files:**
- `client/src/net_stack.rs`

**Verification:**
- `cargo fmt --manifest-path client/Cargo.toml --check`
- `cargo check --manifest-path client/Cargo.toml`

### F9 — `max_connections == 0` Permanently Labels Relay High

**Status:** Implemented.

**Original issue:** A relay reporting `max_connections = 0` could be treated as high capacity.

**Implemented fix:**
- `max_connections == 0` now maps to `low`.
- Low relays are excluded from `LabelledRelayList`.
- Capacity label tests cover the zero-capacity case.

**Files:**
- `controller/internal/relay/capacity_label.go`
- `controller/internal/relay/capacity_label_test.go`
- `controller/internal/relay/store.go`

### F4 — Relay Eviction Status CHECK Mismatch

**Status:** Implemented.

**Original issue:** `EvictExpiredRelays` marked relays as `inactive`, but the `relays.status` CHECK constraint only allowed `pending`, `active`, and `deleted`.

**Implemented fix:**
- Added a migration that allows `inactive` in `relays.status`.
- Existing store behavior already matches the intended lifecycle:
  - expired active relays become `inactive`;
  - heartbeat revives non-deleted relays to `active`;
  - `BuildLabelledRelayList` only publishes `active` relays.

**Files:**
- `controller/migrations/024_add_inactive_relay_status.sql`
- `controller/internal/relay/store.go`

### F11 — LabelledRelayList Version Inconsistency

**Status:** Implemented.

**Original issue:** Broadcast path used wall-clock `time.Now().Unix()` while connect-time push used the store-derived version.

**Implemented fix:**
- `BuildLabelledRelayList` now produces a deterministic content-addressed version.
- Broadcast path no longer overwrites the store/list version.
- Added unit tests for order independence and content sensitivity.

**Files:**
- `controller/cmd/server/main.go`
- `controller/internal/relay/store.go`
- `controller/internal/relay/labelled_list_version_test.go`

### F15 — Debug Print And Log Field Typo

**Status:** Implemented.

**Original issue:** Controller had a raw debug `println`, and client daemon logged `connetor_addr`.

**Implemented fix:**
- Removed controller debug `println`.
- Renamed `connetor_addr` to `connector_addr`.
- Cleaned duplicate "address address" log wording.

**Files:**
- `controller/internal/connector/control_stream.go`
- `client/src/daemon.rs`

## Partially Addressed Findings

### F3 — Client Rebuilds Transports On ACL Changes

**Status:** Partially implemented.

**Implemented behavior:**
- Client restarts/rebuilds the tunnel on action-triggered ACL version changes:
  - `sync`
  - `resources` after TTL refresh
  - login/PostLoginState
- Client receives multiple connector candidates and tries the next connector on connect failure or `SHIELD_NOT_ATTACHED`.

**Remaining gap:**
- No standalone background ACL refresh loop that restarts the tunnel without user/IPC action.

## Open Findings

The following are not implemented yet:

- F1 — Relay provisioning is unauthenticated.
- F2 — No CRL/revocation check on relay outer mTLS or controller heartbeat mTLS.
- F8 — Relay SAN allowlist is self-asserted on self-provision path.
- F10 — Relay certificate renewal is missing.
- F12 — Direct connector trust store is broader than necessary.
- F13 — Provision lacks rate limiting/quota.
- F14 — Relay public address auto-detection has deployment limitations.
