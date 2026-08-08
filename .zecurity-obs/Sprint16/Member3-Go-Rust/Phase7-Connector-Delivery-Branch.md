---
type: phase
sprint: 16
stage: 2
phase: 7
title: Connector Delivery Branch
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase6-Connector-Resolver-Module
status: not-started
tags: [sprint16, connector, rust, routing, delivery, fqdn, security, invariants]
---

# Sprint 16 · Phase 7 — Connector Delivery Branch

> Goal: wire Phase 6's resolver into `handle_stream` so a name-addressed resource is resolved and dialed,
> while shield-delivered resources keep going over the shield session **and never fall back to direct**.
> Depends on **Phase 6** (the resolver module and `Addressing` helper).

## ⚠️ Scope correction — read before touching anything

The obvious reading of this phase is *"after authorization: `route_type == "shield"` → shield session,
else resolve → dial"*. **That is not sufficient, and the missing part is not in the branch you'd expect
to edit.**

Authorization currently rejects **every** name-addressed resource before the branch is ever reached:

```rust
// connector/src/device_tunnel.rs:225 — inside the authorization match, NOT the route_type branch
Some(entry) if !req.destination.is_empty() && req.destination != entry.address => {
    (None, "destination_mismatch")
}
```

For a name-addressed resource `entry.address` is **empty**, while the post-Phase-9 client sends the
**synthetic** IP as `destination` → mismatch → deny. So this phase must amend Phase 1/3's authorization
block (task **7.0**), not only add a delivery branch.

**No live breakage today:** Phase 5 already emits these entries, but clients drop them at
[client/src/daemon.rs:657](../../../client/src/daemon.rs#L657) via `filter_map(...ok()?)`. This is a
future-phase gap, not a regression to hotfix.

## Tasks

### 7.0 — Scope the destination cross-check (**do this first**)
`connector/src/device_tunnel.rs` — the authorization `match`, ~line 225

- [ ] The cross-check is only meaningful when the ACL has a pinned address to compare against. Scope it:
      ```rust
      // The client may state where it thinks the resource is, but it must agree with the
      // ACL. For a NAME-addressed resource there is no pinned address to agree with —
      // the client's synthetic IP is client-local and deliberately meaningless here.
      Some(entry) if !entry.address.is_empty()
                  && !req.destination.is_empty()
                  && req.destination != entry.address => (None, "destination_mismatch"),
      ```
- [ ] ⚠️ **Do not delete the check.** Guarding it behind `!entry.address.is_empty()` keeps it fully
      strict for every IP resource, which is every resource that exists today. Deleting it re-opens the
      confused-deputy surface Stage 1 closed.
- [ ] Write the `destination_mismatch` negative test **before** this edit — it is Gate 1's outstanding
      case, it is now cheap (the data plane works), and it is the regression guard that fails if a later
      change removes the check instead of scoping it. See
      [[Sprint16/Member3-Go-Rust/Phase3-Connector-Requires-ResourceId]].
- [ ] Add a companion test: a **name-addressed** entry + a synthetic-looking `destination` is
      **allowed** (proves the scoping works), while a **pinned** entry + wrong `destination` is still
      denied.

### 7.1 — Branch on `route_type` first, then resolve
`connector/src/device_tunnel.rs`, after authorization succeeds

- [ ] Order is non-negotiable — **`route_type` outermost, `resolver` only inside the connector arm:**
      ```text
      route_type == "shield"                → existing shield session (open_relay_session)
                                              every shield offline → FAIL CLOSED. never dial direct.
      route_type == "connector" | "direct"  → match Addressing (Phase 6):
                                                Pinned(addr)   → dial addr          (unchanged today)
                                                Named{h, r}    → resolver.resolve() → dial result
                                                Invalid(why)   → DENY reason=why
      anything else                         → DENY reason=unknown_route_type        (already present)
      ```
- [ ] ⚠️ **Never write `match resolver.type { dns, static, shield }`.** `shield` is not a resolver type;
      delivery is `route_type` (field 7) and resolution is `resolver.type` (field 12) — two orthogonal
      axes. Collapsing them is how invariant #3 gets broken: a shield-delivered resource would become
      resolvable and therefore directly dialable, silently bypassing shield enforcement.
- [ ] The shield arm is **unchanged**. It already satisfies invariant #3 — it `return`s on both success
      and error and never falls through to the direct dial. Verify that property still holds after the
      edit; it is the easiest thing in this phase to break by accident.
- [ ] Both the TCP bridge **and** the UDP path (`relay_udp`) must take the resolved address. Grep every
      remaining use of `req.destination` as a dial target — there should be none.
- [ ] Resolver failure → deny with the typed reason from Phase 6 (`nxdomain` · `resolver_unavailable` ·
      `no_address_record` · `dial_failed` · `ambiguous_addressing`), each distinct in `AccessLogFields`.
- [ ] Keep using `connect_marked_tcp` (SO_MARK `CONNECTOR_EGRESS_MARK = 0x5b`) for the resolved address.
      ⚠️ Easy to lose here: the mark is what prevents the client/connector co-location routing loop that
      cost a day in Gate 1 ([[Sprint16/KNOWN-BUG-Tunnel-Data-Plane-Stall]]). A new dial path that
      forgets it reintroduces the loop.

### 7.2 — Logging and metrics
- [ ] Log `resource_id` **and** `hostname` and the **resolved** address. `resource_id` is the identity;
      the resolved IP is an observation.
- [ ] **Never** log or persist a synthetic IP as identity — it is client-local and meaningless to the
      connector. (In practice the connector never receives one as anything but `req.destination`, which
      7.0 stops comparing.)
- [ ] Emit resolution latency and cache hit/miss so the TTL clamp can be tuned from data rather than
      guesswork.

## Build gate

```bash
cd connector && cargo build && cargo test
```

## Verify

**Unit / integration (available now):**
- [ ] Pinned resource: dial target is `entry.address`, byte-identical behaviour to before this phase.
- [ ] Named resource with a `static` resolver: dials the configured address.
- [ ] Named resource, resolver absent → deny; unknown resolver type → deny.
- [ ] Shield-routed resource with `hostname` set is **still** delivered via the shield and **never**
      resolved.
- [ ] Shield-routed resource, all shields offline → fails closed (invariant #3).

**E2E (only after Phase 9 — noted deliberately):**
- [ ] No client can express a name-addressed resource until Phase 9's binding registry exists, so this
      phase has **no end-to-end proof**. Its gate is the unit tests above. The first E2E exercise of
      Phase 7 is Phase 9.5's `hosts`-entry test.

## Notes

- One policy decision per flow (invariant #5): the response path is pure byte movement — **no**
  re-authorization, **no** re-resolution, **no** new transport choice. `copy_bidirectional` on the
  already-chosen socket. Re-resolving on the response path would let a backend IP change mid-flow.
- Validate before resolve (invariant #2): no DNS query may be issued for an unauthorized request. With
  the ordering above this is automatic — keep it that way.
- Do not touch `relay/**`, `client/src/relay_pool.rs`, or `client/src/transport.rs`.

## Post-Phase Fixes

_(none yet)_
