---
type: phase
member: M3
sprint: 15
phase: 1
title: Active-Session Registry
status: in-progress
depends_on: []
tags: [rust, connector, session-registry, pending-08, pending-09]
---

# Phase 1 — Active-Session Registry

> Depends on nothing — Day 1. Fully additive (nothing aborts anything yet) → zero risk
> to live tunnels until Phase 2 wires in the diff-and-abort logic.
>
> This phase directly satisfies both PENDING-08 §3 ("Connector active-session registry
> keyed by device SPIFFE ID and resource") and is the foundation for PENDING-09 Option B
> (bounded variant — see Sprint 15 `path.md` Key Design Decisions).

## Goal

Give the connector a live index of open tunnels keyed by `(spiffe_id, resource_id)` so a
future ACL change (Phase 2) can find and kill exactly the tunnels that lost
authorization — no more, no less.

## Why this needs care (corrected during design review — see below)

- `device_tunnel.rs`'s `listen()` currently does a bare `tokio::spawn` per accepted
  connection and discards the `JoinHandle` — there is nothing to register against.
- The two halves of the registry key become known at **different points**: `spiffe_id`
  right after the mTLS handshake completes (cert extraction), `resource_id` only after
  `handle_stream` parses the request and resolves the ACL entry.
- **An earlier version of this plan proposed "thread the `JoinHandle` into
  `handle_stream`" — this is structurally impossible and must not be attempted.** A
  `tokio::spawn`'s `JoinHandle` only exists in the *caller's* scope (`listen()`), while
  `handle_stream` runs *inside* the future that was just moved into `tokio::spawn`. A
  future cannot be handed a handle to itself; the handle doesn't exist yet at the point
  the future is constructed.
- **Correct design: use a `tokio_util::sync::CancellationToken`, created *before*
  `tokio::spawn`, and cloned into the future.** This sidesteps the circularity entirely
  — the token is a value, not a handle to the task, so it can be created first and
  moved in along with everything else the future needs.

## Files

| File | Change |
|------|--------|
| `connector/Cargo.toml` | **add `dashmap` and `tokio-util`** — neither is a dependency today |
| `connector/src/device_tunnel.rs` | shared registry + `CancellationToken`-based registration after ACL resolution |
| `connector/src/quic_listener.rs` | identical registration path for QUIC-accepted streams |

## Design

```rust
type SessionKey = (String /* spiffe_id */, String /* resource_id */);
type SessionId = uuid::Uuid; // fresh per registration, not per (spiffe,resource) pair
type SessionRegistry = DashMap<SessionKey, HashMap<SessionId, tokio_util::sync::CancellationToken>>;
```

**Why nested, not `Vec<CancellationToken>`:** a device can legitimately have more than
one live tunnel to the same resource at once (same `(spiffe_id, resource_id)` pair). If
cleanup removed the whole outer `(spiffe,resource)` key when *any one* of those tunnels
closed, every *other* still-live tunnel sharing that key would lose its token —
uncancellable from then on, a silent leak of exactly the mechanism this registry exists
to provide. A `Vec` has the same problem if cleanup ever removes by index/value rather
than tracking which entry belongs to which task. The nested map keyed by a
per-registration `SessionId` makes "remove only my own entry" unambiguous.

- Use `DashMap`, not the coarse `parking_lot::RwLock` the existing `PolicyCache` uses —
  registry writes (on spawn) and registry reads/removals (on ACL diff, Phase 2) happen
  concurrently and must not serialize behind one lock.
- **In `listen()` (and `quic_listener.rs`'s equivalent accept loop), before
  `tokio::spawn`:** create `let token = CancellationToken::new();` and
  `let session_id = Uuid::new_v4();`, clone the token into the future
  (`let task_token = token.clone();`), and race it inside the future's actual I/O loop
  via `tokio::select! { _ = task_token.cancelled() => { /* tear down */ }, result = copy_bidirectional(...) => { ... } }` (or the equivalent for `relay_udp`/relay
  `select!`, see Phase 2 for the relay child-task specifics).
- **`handle_stream()`: after** `let acl_entry = decision.unwrap();` (the existing ACL
  resolution success point) — insert into the nested map:
  `registry.entry((spiffe_id, resource_id)).or_default().insert(session_id, token.clone())`.
- **Use a drop guard keyed by `(SessionKey, SessionId)`** to unregister on every exit
  path — normal close, error, or external cancellation — not just the happy path.
  `struct RegistryGuard { key: SessionKey, session_id: SessionId, registry: Arc<SessionRegistry> }`,
  whose `Drop` impl removes **only its own `session_id`** from the inner map for `key`,
  and removes the outer `key` entirely **only if** the inner map is now empty. This is
  the piece that prevents one tunnel's cleanup from cancelling a sibling's token.
- This same registration path must be used identically by **both** `device_tunnel.rs`'s
  TCP accept loop **and** `quic_listener.rs`'s QUIC accept loop — QUIC streams also call
  `handle_stream` and were missing from the original design entirely. Do not implement
  registration once and assume it covers both transports.

## Tests

- Unit: two tunnels for the same `spiffe_id` to two different `resource_id`s produce two
  distinct outer registry entries (confirms the key is per-pair, not per-device).
- **Unit: two tunnels sharing the same `(spiffe_id, resource_id)` pair — closing one
  removes only its own `SessionId` entry; the other tunnel's token remains registered
  and cancellable.** This is the regression test for the key-collision bug — it must
  fail against a flat `Vec`/single-level design and pass against the nested map.
- Unit: a tunnel's entry is removed from the registry when the tunnel closes normally
  (no leak) — via the drop guard, not a manual cleanup call that could be forgotten on
  some exit path.
- Unit: a tunnel's entry is removed when its token is cancelled externally.
- Unit: the outer `(spiffe,resource)` key itself is removed once its last session closes, not left behind as an empty entry.
- Unit: concurrent spawn + a Phase-2-style removal on the same key don't panic/deadlock
  (exercise the `DashMap` under concurrent insert/remove).
- Unit: the QUIC accept path produces registry entries identical in shape/behavior to the TCP path.

## Build Check
```bash
cd connector && cargo build
```

## Implementation Checklist
- [x] **M3-F0** `connector/Cargo.toml` — add `dashmap` and `tokio-util` as dependencies.
- [ ] **M3-F1** Shared `DashMap<(spiffe_id, resource_id), HashMap<SessionId, CancellationToken>>` (nested, not flat); token + fresh `SessionId` created **before** `tokio::spawn` in `listen()`, registered **after** ACL resolution succeeds inside `handle_stream` — not the (impossible) post-spawn `JoinHandle` design.
- [x] **M3-F2** Identical registration path added to `connector/src/quic_listener.rs`'s accept loop.
- [x] Drop guard removes only its own `SessionId`; outer key removed only when the inner map is empty (no cross-session cancellation, no leak).
- [x] **Build gate:** `cd connector && cargo build`

## Post-Phase Fixes

### Remaining: create cancellation identity before spawning

**Issue:** The registry shape, shared TCP/QUIC registration path, and cleanup guard are
implemented, but `SessionRegistry::register()` currently creates the
`CancellationToken` and `SessionId` from inside `handle_stream`, after the connection
task has already been spawned.

**Required Fix:** Create a fresh token and session ID before `tokio::spawn` in both
accept loops, pass them through to `handle_stream`, and register those values only after
ACL resolution succeeds. Until this is done, M3-F1 and the overall phase remain open.

**Verification:** `cargo test -q` passed all 87 unit tests and all 4 loopback integration
tests (1 test ignored) on 2026-07-29.
