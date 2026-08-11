---
type: phase
sprint: 16
stage: 1
phase: 1
title: Connector Accepts `resource_id` (tolerant)
owner: M3
depends_on: []
status: done
tags: [sprint16, connector, rust, identity, handshake, security]
---

# Sprint 16 · Phase 1 — Connector Accepts `resource_id` (tolerant)

> Goal: the connector authorizes and dials by **resource identity**, not by the address string the
> client sent. Tolerant this phase (accepts either form) so the client can roll out in Phase 2.
> Depends on nothing — start here.

## Why (the bug this closes)

Today the connector does:

```rust
// connector/src/device_tunnel.rs (~185)
let decision = match acl.resolve_resource(&req.destination, req.port, &req.protocol) { ... }
```

`req.destination` is a **client-supplied string**. It must match an ACL entry, so this is not
wide-open — but the connector then uses that client-provided value as the dial target. The client is
effectively choosing the address; the connector merely checks it appears in the ACL. That's a
confused-deputy / SSRF-shaped surface, and it's also what blocks FQDN resources (the client cannot
name something it can't express as an IP).

After this phase the connector dials **the address on its own ACL entry** for the authorized
`resource_id`. The client's string becomes, at most, a cross-check.

## Root cause, exactly located

```rust
// connector/src/policy/mod.rs:12
pub struct ResourceAcl {
    pub resource_id: String,
    pub allowed_spiffe_ids: Vec<String>,
    pub route_type: String,
    pub shield_id: String,
    //  ⬅ NO `address` FIELD
}
```

**That missing field is the whole bug.** The ACL lookup result doesn't carry the resource's address, so
the only address available to dial is the client's `req.destination`. Add `address` to `ResourceAcl` and
the dependency inverts: the connector dials what its own ACL says.

## Current state (verified)

- `ACLEntry.resource_id` **already exists** (proto field 1) and the connector already logs it
  (`"deny spiffe_id={} resource={} ..."`). **No proto change is needed in Stage 1.**
- The handshake is **plain serde JSON**, not protobuf:
  - client: `TunnelRequest { destination, port, protocol }` — `client/src/net_stack.rs` (~109)
  - connector: `TunnelRequest { destination, port, protocol }` with
    `#[serde(default = "default_tcp")]` on `protocol` — `connector/src/device_tunnel.rs:41`
    (so an optional field with `#[serde(default)]` matches existing style)
- The handler is **`handle_stream(...)`** — `connector/src/device_tunnel.rs:133`.
- `connector/src/policy/` is a **single file** (`mod.rs`), not a directory of modules.
- The connector already branches on delivery: `if acl_entry.route_type == "shield"` → shield relay
  session (`open_relay_session`); else → direct TCP/UDP bridge.

### Two pieces already exist — do not rebuild them

| Existing | Where | Consequence |
|---|---|---|
| `fn find_entry_by_id(snapshot, resource_id) -> Option<&AclEntry>` | `policy/mod.rs:109` (private) | the new lookup is a thin wrapper — reuse it |
| `pub fn is_allowed(resource_id, client_spiffe_id) -> bool` | `policy/mod.rs:42` | **identity-based SPIFFE authorization is already implemented** |

**Revised scope:** this phase is **~1 new function + 1 struct field + 1 handler reorder** — smaller
than the original 4-file estimate, because the two functions above are already in place.

## Tasks

### 1.1 — ACL lookup by identity
`connector/src/policy/mod.rs`

- [x] **`ResourceAcl` += `pub address: String`** — the dial target, sourced from the ACL entry.
      *This is the change that fixes the bug; everything else follows from it.*
- [x] Populate `address` in the **existing** `resolve_resource(...)` too (one line, `entry.address.clone()`).
- [x] Add the identity lookup — reuse the existing private `find_entry_by_id`:

```rust
/// Look up a resource by identity. Returns None when no snapshot is loaded, the
/// resource is unknown, or the port/protocol don't match the entry — callers deny.
pub fn resolve_by_resource_id(
    &self,
    resource_id: &str,
    port: u16,
    protocol: &str,
) -> Option<ResourceAcl> {
    let guard = self.snapshot.read();
    let snapshot = guard.as_ref()?;
    let entry = find_entry_by_id(snapshot, resource_id)?;
    if entry.port != port as u32 || !entry.protocol.eq_ignore_ascii_case(protocol) {
        return None;                 // a resource authorized on :5432 is NOT reachable on :22
    }
    Some(ResourceAcl { /* resource_id, allowed_spiffe_ids, route_type, shield_id, address */ })
}
```

- [x] Keep `resolve_resource` — Phase 1 is tolerant; **Phase 3** removes its use.
- [x] Tests: mirror the existing `resolve_resource_matches_network_tuple` and
      `resolve_resource_returns_none_on_port_mismatch` for the id path, plus
      `resolve_by_resource_id_returns_none_on_protocol_mismatch`.

### 1.2 — Handshake: optional `resource_id`, dial from the ACL
`connector/src/device_tunnel.rs` — `struct TunnelRequest` (line 41) and `handle_stream(...)` (line 133)

- [x] `TunnelRequest` += `#[serde(default)] resource_id: Option<String>` (tolerant: old clients omit it).
- [x] Reorder the top of `handle_stream` to this exact sequence (**invariants #1, #2, #3**):

```text
1. req.resource_id present?
     yes → acl = resolve_by_resource_id(id, req.port, &req.protocol)
              None → DENY  reason=unknown_resource        (no dial attempted)
     no  → acl = resolve_resource(&req.destination, …)     // legacy, Phase 1 only
              None → DENY  reason=no_acl_match

2. client SPIFFE ∈ acl.allowed_spiffe_ids?
     no  → DENY  reason=unauthorized_spiffe

3. req.destination non-empty && req.destination != acl.address?
     yes → DENY  reason=destination_mismatch               (never silently prefer one)

4. branch:
     acl.route_type == "shield" → open_relay_session(acl.shield_id, …)
                                   shield down → FAIL (never fall back to direct)
     else                       → dial acl.address          ← NOT req.destination
```

- [x] **Dial `acl.address` everywhere `req.destination` was used as the target** (both the TCP bridge
      and `relay_udp`). Grep for `req.destination` and audit every hit.
- [x] Preserve the existing shield branch semantics unchanged.
- [x] `emit_access_log` / `AccessLogFields` — log `resource_id` as the identity; keep `destination` as
      the *observed* value only.

### 1.3 — Keep the legacy path alive (this phase only)
- [x] When `resource_id` is absent, fall back to today's `resolve_resource(&req.destination, ...)`.
- [x] Log which path was taken (`auth_path=resource_id|legacy_destination`) so the Phase 3 cutover can
      be verified from logs before the legacy branch is deleted.

## Build gate

```bash
cd connector && cargo build && cargo test
```

## Verify (manual, before moving on)

> The positive path was exercised on a live stack (Gate 1); the negative cases were deferred while the
> data plane was broken and were finally written **during Phase 7**, as automated tests rather than
> manual checks. The authoritative list lives in
> [[Sprint16/Member3-Go-Rust/Phase3-Connector-Requires-ResourceId]] § *GATE 1*; the test names are
> noted inline below.

- [x] An existing IP resource still connects (legacy path, since the client isn't updated yet).
- [x] A hand-crafted handshake with a valid `resource_id` connects, and the connector logs
      `auth_path=resource_id`.
- [x] A handshake with a valid `resource_id` but a **wrong port** is denied.
      *(`wrong_port_is_denied_as_unknown_resource`, added 2026-08-10 with Phase 7's harness.)*
- [x] A handshake with a valid `resource_id` and a **mismatched `destination`** is denied.
      *(`destination_mismatch_is_denied_for_pinned_resources`.)*
      📌 **Write this one before Phase 7** — task 7.0 modifies exactly this check.
- [x] A handshake with an unknown `resource_id` is denied — and **no dial is attempted**.
      *(`unknown_resource_id_is_denied_without_dialing`.)*
- [x] A shield-routed resource still goes via the shield session; with the shield down it **fails**
      (never silently dials direct).
      *(`shield_route_is_never_resolved_and_fails_closed`.)*

---

# Follow-up: Phase 3 — Require `resource_id`

> **Moved.** Phase 3 now has its own file:
> [[Sprint16/Member3-Go-Rust/Phase3-Connector-Requires-ResourceId]].
> It carries tasks 3.1/3.2, the four denial reasons, and the **GATE 1** evidence.
> Kept as a pointer so the "do this only after Phase 2 ships everywhere" sequencing note isn't lost.

## Notes

- Do **not** touch `client/src/relay_pool.rs`, `client/src/transport.rs`, or `relay/**`. The relay
  forwards an already-authenticated stream and never parses this handshake.
- The handshake is JSON, so adding a field is backward/forward compatible — but **Phase 3 makes it
  mandatory**, which is a breaking change for old clients. Sequence it as written.
