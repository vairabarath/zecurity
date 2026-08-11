---
type: phase
sprint: 16
stage: 2
phase: 7
title: Connector Delivery Branch
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase6-Connector-Resolver-Module
status: done
tags: [sprint16, connector, rust, routing, delivery, fqdn, security, invariants]
---

# Sprint 16 · Phase 7 — Connector Delivery Branch

> Goal: wire Phase 6's resolver into `handle_stream` so a name-addressed resource is resolved and dialed,
> while shield-delivered resources keep going over the shield session **and never fall back to direct**.
> Depends on **Phase 6** (the resolver module and `Addressing` helper).

## ⚠️ Scope correction — and a correction to the correction

The obvious reading of this phase is *"after authorization: `route_type == "shield"` → shield session,
else resolve → dial"*. That is not sufficient: the authorization block, ~100 lines earlier, also needed
amending (task **7.0**), and `handle_stream`'s signature change rippled into **3 more files**.

**But this file originally claimed authorization "denies every name-addressed resource". That was
wrong.** The original arm read:

```rust
// connector/src/device_tunnel.rs — inside the authorization match
Some(entry) if !req.destination.is_empty() && req.destination != entry.address => {
    (None, "destination_mismatch")
}
```

With `destination: ""` — which is exactly what Phase 9.4 specifies — the **first clause is already
false**, so the arm never fired for a named resource. It only fires when the client sends a *non-empty*
destination, e.g. the synthetic IP.

So 7.0 is **defensive hardening plus a Phase 9.4 contract**, not a fix for a live break:
- it makes the invariant explicit rather than incidental;
- it survives Phase 9 changing its mind and sending the synthetic IP instead of an empty string.

**How this was found:** the first guard test sent `destination: ""` and passed with *and* without the
scoping — a test that guarded nothing. The real guard sends a non-empty destination, and was validated
by reverting the scoping and confirming exactly one test fails. **A passing test is not evidence of a
guard; revert the fix and watch it fail.**

Phase 9.4's contract is unchanged: **send `destination` empty for name-addressed resources.**

## Tasks

### 7.0 — Scope the destination cross-check (**do this first**)
`connector/src/device_tunnel.rs` — the authorization `match`, ~line 225

- [x] The cross-check is only meaningful when the ACL has a pinned address to compare against. Scope it:
      ```rust
      // The client may state where it thinks the resource is, but it must agree with the
      // ACL. For a NAME-addressed resource there is no pinned address to agree with —
      // the client's synthetic IP is client-local and deliberately meaningless here.
      Some(entry) if !entry.address.is_empty()
                  && !req.destination.is_empty()
                  && req.destination != entry.address => (None, "destination_mismatch"),
      ```
- [x] ⚠️ **Do not delete the check.** Guarding it behind `!entry.address.is_empty()` keeps it fully
      strict for every IP resource, which is every resource that exists today. Deleting it re-opens the
      confused-deputy surface Stage 1 closed.
- [x] Write the `destination_mismatch` negative test **before** this edit — it is Gate 1's outstanding
      case, it is now cheap (the data plane works), and it is the regression guard that fails if a later
      change removes the check instead of scoping it. See
      [[Sprint16/Member3-Go-Rust/Phase3-Connector-Requires-ResourceId]].
- [x] Add a companion test: a **name-addressed** entry + a synthetic-looking `destination` is
      **allowed** (proves the scoping works), while a **pinned** entry + wrong `destination` is still
      denied.

### 7.1 — Branch on `route_type` first, then resolve
`connector/src/device_tunnel.rs`, after authorization succeeds

- [x] Order is non-negotiable — **`route_type` outermost, `resolver` only inside the connector arm:**
      ```text
      route_type == "shield"                → existing shield session (open_relay_session)
                                              every shield offline → FAIL CLOSED. never dial direct.
      route_type == "connector" | "direct"  → match Addressing (Phase 6):
                                                Pinned(addr)   → dial addr          (unchanged today)
                                                Named{h, r}    → resolver.resolve() → dial result
                                                Invalid(why)   → DENY reason=why
      anything else                         → DENY reason=unknown_route_type        (already present)
      ```
- [x] ⚠️ **Never write `match resolver.type { dns, static, shield }`.** `shield` is not a resolver type;
      delivery is `route_type` (field 7) and resolution is `resolver.type` (field 12) — two orthogonal
      axes. Collapsing them is how invariant #3 gets broken: a shield-delivered resource would become
      resolvable and therefore directly dialable, silently bypassing shield enforcement.
- [x] The shield arm is **unchanged**. It already satisfies invariant #3 — it `return`s on both success
      and error and never falls through to the direct dial. Verify that property still holds after the
      edit; it is the easiest thing in this phase to break by accident.
- [x] Both the TCP bridge **and** the UDP path (`relay_udp`) must take the resolved address. Grep every
      remaining use of `req.destination` as a dial target — there should be none.
- [x] Resolver failure → deny with the typed reason from Phase 6 (`nxdomain` · `resolver_unavailable` ·
      `resolver_failure` ·
      `no_address_record` · `dial_failed` · `ambiguous_addressing`), each distinct in `AccessLogFields`.
- [x] Keep using `connect_marked_tcp` (SO_MARK `CONNECTOR_EGRESS_MARK = 0x5b`) for the resolved address.
      ⚠️ Easy to lose here: the mark is what prevents the client/connector co-location routing loop that
      cost a day in Gate 1 ([[Sprint16/KNOWN-BUG-Tunnel-Data-Plane-Stall]]). A new dial path that
      forgets it reintroduces the loop.

### 7.2 — Logging and metrics
- [x] Log `resource_id` **and** `hostname` and the **resolved** address. `resource_id` is the identity;
      the resolved IP is an observation.
- [x] **Never** log or persist a synthetic IP as identity — it is client-local and meaningless to the
      connector. (In practice the connector never receives one as anything but `req.destination`, which
      7.0 stops comparing.)
- [x] Emit resolution latency and cache hit/miss so the TTL clamp can be tuned from data rather than
      guesswork.

## Build gate

```bash
cd connector && cargo build && cargo test
```

## Verify

**Unit / integration (available now):**
- [x] Pinned resource: dial target is `entry.address`, byte-identical behaviour to before this phase.
- [x] Named resource with a `static` resolver: dials the configured address.
- [x] Named resource, resolver absent → deny; unknown resolver type → deny.
- [x] Shield-routed resource with `hostname` set is **still** delivered via the shield and **never**
      resolved.
- [x] Shield-routed resource, all shields offline → fails closed (invariant #3).

**E2E (only after Phase 9 — noted deliberately):**
- [x] No client can express a name-addressed resource until Phase 9's binding registry exists, so this
      phase has **no end-to-end proof**. Its gate is the unit tests above. The first E2E exercise of
      Phase 7 is Phase 9.5's `hosts`-entry test.

## Notes

- One policy decision per flow (invariant #5): the response path is pure byte movement — **no**
  re-authorization, **no** re-resolution, **no** new transport choice. `copy_bidirectional` on the
  already-chosen socket. Re-resolving on the response path would let a backend IP change mid-flow.
- Validate before resolve (invariant #2): no DNS query may be issued for an unauthorized request. With
  the ordering above this is automatic — keep it that way.
- Do not touch `relay/**`, `client/src/relay_pool.rs`, or `client/src/transport.rs`.

## Result

**Gate: PASS — 147 unit + 4 integration tests** (baseline 129). Zero build warnings; no new clippy
findings; `rustfmt` clean; `cargo build --manifest-path relay/Cargo.toml` still green.

**Files touched: 4 + 1** — `device_tunnel.rs`, `quic_listener.rs`, `relay_handler.rs`, `main.rs`, plus
`resolver.rs` for `UnavailableBackend` and `Resolved::cache_hit`. The File Map listed only
`device_tunnel.rs`; `handle_stream` has **three** call sites (`device_tunnel::listen`,
`quic_listener.rs:110`, `relay_handler.rs:183`) and `main.rs` wires all of them from one shared
`Arc<Resolver>`.

### Test harness (new)

`device_tunnel.rs` had no `mod tests` at all. Added one built on `tokio::io::duplex`, driving the real
framed-JSON handshake (4-byte BE length + body) and asserting on the **emitted `ConnectorLog`** rather
than the response bytes — that is what makes `action` / `error` / `route_type` and the typed resolver
reason observable.

⚠️ **Harness gotcha worth remembering:** a fresh `CrlManager` reports `Unavailable`, not `NotRevoked` —
fail-closed by construction — so `handle_stream` denies at the *first* check and never reaches
authorization. Every test calls the pre-existing `#[cfg(test)] CrlManager::install_test_cache(vec![])`.
No production code was changed to make any of this testable.

**18 tests:** 7.0 cross-check ×3 · delivery branch ×8 · unchanged auth paths ×3 · legacy `direct`
alias ×1 · real-socket byte round-trip ×1 · plus 3 in `resolver.rs` for `cache_hit`.

### Decisions taken during implementation

| Question | Resolution |
|---|---|
| Extend `AccessLogFields` with `hostname` / `resolved`? | **No.** Typed audit fields would need a `ConnectorLog` **proto** change — the same "who receives this message?" question that caught `local_target` in Phase 5. Structured `tracing` + `legacy_message` instead. |
| Where does "access allowed" log? | Moved to **after** the dial target is resolved, so resolution failure is a clean `deny` (invariant #7) rather than allow-then-error, and the allow line can carry the resolved address. A *dial* failure stays after allow with `action: "error"` — resolution succeeded, the resource is down. |
| Fatal if `/etc/resolv.conf` is unusable? | **No** — `main.rs` falls back to `UnavailableBackend`. A connector serving only IP-pinned resources must not gain a new startup failure; that would contradict *"existing IP resources behave identically."* Named resources then deny with `resolver_unavailable`, which is the truth. |

## Post-Phase Fixes

All four were found by a verification round **after** the phase gate was already green. A green gate
proves the specified tests pass; it does not prove every listed task was done.

### Fix: task 7.2's metrics bullet was silently skipped
**Issue:** *"Emit resolution latency and cache hit/miss so the TTL clamp can be tuned from data"* was
never implemented. The logging half of 7.2 shipped and the gate passed regardless.

**Fix applied:** `Resolved` gains `cache_hit: bool`, set at all five construction sites — `true` on
fast-path and double-check hits, `false` whenever a query was issued, and always `false` for `static`
(no cache, no query). `device_tunnel.rs` times the resolve and emits `resolved`, `cache_hit`, `stale`,
`resolve_us` at **`debug`** (it fires on every connection to a named resource; the access log above is
the audit record). The crate has **no metrics facility**, so structured logs are the only sink today.

3 tests assert the flag is trustworthy, including the subtle case: the *first* stale answer is
`cache_hit: false` (a query was attempted and failed) while the *backoff-served* one is `true`.

### Fix: a guard test that guarded nothing
**Issue:** the first named-resource `destination_mismatch` test sent `destination: ""` and passed with
*and* without the 7.0 scoping.

**Root cause:** the original arm was already inert for an empty destination — see the scope correction
above. The test was asserting behaviour that never depended on the fix.

**Fix applied:** the real guard sends a non-empty (synthetic) destination
(`named_resource_is_not_denied_for_a_non_empty_destination`), validated by reverting the scoping and
confirming exactly one test fails. The empty-destination test is kept, explicitly labelled a **Phase 9.4
contract test, not a 7.0 guard**.

### Fix: navigation hints leaked into the source
**Issue:** comments reading `// … (line ~482)` and `// ~487` — aids for applying the patch by hand —
were pasted into `device_tunnel.rs` and pointed at the wrong lines once the file grew.

**Fix applied:** removed. Keep line-number references outside code snippets.

### Fix: the legacy `direct` route_type had no test
**Issue:** `route_type == "direct"` is a legacy alias for `"connector"` kept for older ACL snapshots. It
reaches the same Phase 7.1 addressing code, but nothing covered it — a future tightening of the
route_type check would silently break named resources on old snapshots.

**Fix applied:** `legacy_direct_route_type_still_resolves`.

**Related files:** all four fixes are confined to `connector/src/device_tunnel.rs` and
`connector/src/resolver.rs`.

## Known limitations carried forward

- **No E2E proof exists yet.** No client can express a name-addressed resource until Phase 9, so the
  gate here is unit tests, deliberately. The first real exercise of Phases 6–7 is Phase 9.5's
  `hosts`-entry test — expect to find Phase 6/7 bugs *there*.
- **`ERR_ACCESS_DENIED` and `ERR_INTERNAL` are declared but unused** — the shield branch uses an
  `"INTERNAL"` literal. Pre-existing; left alone.
- **`handle_stream` now trips clippy's `too_many_arguments` (9/7)**, joining 8 others already in the
  crate on the same pattern. A context struct would be a crate-wide refactor, not this phase's job.
- **UDP is untested.** `relay_udp` binds a real socket; it takes the same `dial_ip`, and the TCP path
  covers the resolution logic.
