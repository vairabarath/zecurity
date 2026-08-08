---
type: phase
sprint: 16
stage: 2
phase: 8
title: Shield `local_target`
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase7-Connector-Delivery-Branch
status: not-started
tags: [sprint16, shield, connector, go, rust, proto, nftables, security]
---

# Sprint 16 · Phase 8 — Shield `local_target`

> Goal: let a shield-protected resource declare **which local address the shield dials**
> (`127.0.0.1` vs its LAN IP) instead of always dialing the address the connector passed down.
> Depends on **Phase 7**.

## Why

Today the connector calls `open_relay_session(&shield_id, &acl_entry.address, port, protocol)` and the
shield dials that address ([shield/src/tunnel.rs:58](../../../shield/src/tunnel.rs#L58)). Since a shield
protects the host it runs on, that address is the shield's own LAN IP. A service bound **only to
loopback** — the common hardening posture — is therefore unreachable, even though the shield is running
on the very host that could reach it.

`local_target` makes the shield-side dial target an explicit property of the resource.

## ⚠️ 8.0 comes from Phase 5, and it is the interesting part

`local_target` was originally planned as `ACLEntry` field 13 because migration 030 adds all three
addressing columns together. **That grouping was wrong for the wire** and the field was removed before
it shipped (`reserved 13`). Full reasoning:
[[Sprint16/Member3-Go-Rust/Phase5-Proto-ACL-Emission-GraphQL]] § *Design correction*.

The short version: `ACLSnapshot` reaches only clients and connectors, neither of which dials a
resource's on-host address. The **Shield** does — and it receives
`shield.v1.ResourceInstruction`, never an `ACLEntry`. So the delivery vehicle is different, and this
phase builds it.

A useful contrast, and the reason this is the *right* home:

| | Bumps on a `local_target` edit? | Consequence |
|---|---|---|
| ACL snapshot version | **No** (`ACLRelevantUpdate` excludes it; asserted by `acl_relevance_test.go`) | no fleet-wide `restart_tunnel_if_running` |
| Shield snapshot generation | **Yes** — it is a content fingerprint over `desiredForShield`'s rows | the shield re-applies, which is exactly what should happen |

That asymmetry is the design working: the recipient that needs the change gets it; the fleet that does
not, is not disturbed.

## Tasks

### 8.0 — Deliver `local_target` to the Shield
- [ ] `proto/shield/v1/shield.proto` — `ResourceInstruction` **fields 1–6 are in use; never renumber.**
      Add `string local_target = 7;`
- [ ] `internal/resource/store.go`:
      - `PendingRow` += `LocalTarget string`
      - `desiredForShield`'s `SELECT` += `COALESCE(local_target, '')` and the matching `Scan` target.
        ⚠️ Note the existing `COALESCE(host, '')` on the same query (added in Phase 4) — the same
        nullable-column trap applies to the new column.
      - `BuildShieldSnapshot` inherits it automatically (it hashes whatever `desiredForShield` returns),
        so the generation bump comes for free. **Verify** it changes when `local_target` changes,
        otherwise shields will never re-apply.
- [ ] `internal/connector/control_stream.go` — populate it in **all three** `ResourceInstruction{}`
      construction sites: **~169** (snapshot), **~220** (single push), **~532** (batch).
      ⚠️ Three sites is the whole risk of this task. A missed site means `local_target` is silently `""`
      on one delivery path, and the shield falls back to its LAN IP — a resource that works after a full
      resync but not after an incremental push, which is a miserable bug to chase.
- [ ] Codegen: `buf generate` **and** `cargo build --manifest-path shield/Cargo.toml`.
      ⚠️ Expect the same class of fallout as Phase 5.2: any exhaustive `ResourceInstruction` literal in
      Rust tests stops compiling. Prefer `..Default::default()` in new test helpers.

### 8.1 — `shield/src/resources.rs` — `validate_host` accepts `local_target`
> ⚠️ **This touches a non-negotiable project rule** (`resource.host == detect_lan_ip()`). The check must
> stay **equally strict** — only more explicitly sourced. Read this task twice before editing.

Current implementation ([shield/src/resources.rs:94](../../../shield/src/resources.rs#L94)):
```rust
pub fn validate_host(resource_host: &str) -> bool {
    if resource_host == "127.0.0.1" { return true; }
    match util::detect_lan_ip() {
        Some(my_ip) => my_ip == resource_host,
        None => false,
    }
}
```

- [ ] The **allowed set is unchanged**: `{ "127.0.0.1", detect_lan_ip() }`. `local_target` selects *from*
      that set; it does not extend it. An instruction naming anything else is rejected exactly as today.
- [ ] Empty `local_target` → current behaviour (validate `instruction.host`). Backward compatible with
      an un-upgraded controller.
- [ ] `None` from `detect_lan_ip()` still returns `false` — **fail closed**, unchanged.
- [ ] Do **not** accept a hostname here. This is a shield-local dial target, not a resolvable name; the
      shield is not a resolver. Accepting names would put a resolver inside the shield, which the sprint
      explicitly defers (*Shield-as-segment-gateway*).
- [ ] The rejection log line must say which value failed and where it came from — a `local_target`
      typo currently surfaces as the misleading "resource host does not match this shield's LAN IP".

### 8.2 — `shield/src/tunnel.rs` — dial `local_target`
- [ ] `handle_tunnel_open_tcp` / `handle_tunnel_open_udp` dial the resource's validated `local_target`
      when set, else the current `destination`.
- [ ] The `local_target` used must be the one **stored for that resource** in the shield's active state
      — not one read out of the tunnel-open message. The tunnel-open path is per-connection and
      connector-driven; taking a dial target from it would hand the connector free-form dialing inside
      the shield, which is the same confused-deputy shape Stage 1 removed one layer up.
- [ ] `check_port` already treats an unparseable address as "not listening" rather than panicking; keep
      that property for `local_target`.

## Build gates

```bash
buf generate
cd controller && go build ./... && go vet ./...
cargo build --manifest-path shield/Cargo.toml
cd connector && cargo build && cargo test     # proto change is shared — re-verify
```

## Verify

- [ ] A protected resource with no `local_target` behaves **identically** to before (regression).
- [ ] `local_target = "127.0.0.1"` → the shield dials loopback; a loopback-only service becomes
      reachable.
- [ ] `local_target` = the shield's LAN IP → accepted.
- [ ] `local_target` = any **other** IP → rejected, with `status: "failed"` and a log naming the field.
- [ ] `local_target` = a hostname → rejected.
- [ ] Editing `local_target` bumps the **shield generation** and the shield re-applies…
- [ ] …and does **not** bump the **ACL version** (`acl_relevance_test.go` guards this; confirm no
      tunnel restart is observed on the client).
- [ ] nftables `chain resource_protect` is still flushed + rebuilt atomically in one transaction —
      unchanged by this phase, but re-verify, since it is a standing non-negotiable rule.

## Notes

- Shields heartbeat to the Connector `:9091` only, never directly to the Controller. Nothing in this
  phase changes delivery topology — `local_target` rides the existing `ResourceInstruction` piggyback.
  **No new RPCs.**
- This phase is independent of the resolver work: a shield-delivered resource is **never** resolved.
  If you find yourself calling Phase 6's resolver from shield code, stop — the two axes have been
  conflated (see Phase 7's warning).

## Post-Phase Fixes

_(none yet)_
