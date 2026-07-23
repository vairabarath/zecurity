---
type: phase
member: M3
sprint: 15
phase: 2
title: ACL-Diff Teardown
status: planned
depends_on: [Phase1-Active-Session-Registry]
tags: [rust, connector, acl, revocation, pending-08, pending-09]
---

# Phase 2 — ACL-Diff Teardown

> Depends on Phase 1 (registry). This is the phase that actually implements bounded
> PENDING-09 Option B — no new revocation RPC, reuses the connector's existing proactive
> ACL push (`NotifyPolicyChange`) plus its 15-second health-reconciliation heartbeat.

## Goal

On every new ACL snapshot, kill exactly the live tunnels whose `(spiffe_id,
resource_id)` pair is no longer authorized — whether the cause is a group revoke, a
device revoke, or (once Sprint 15's M1 work lands) a posture failure under enforce mode.
This mechanism doesn't care which cause it was; it just reacts to the ACL shrinking.

## Critical scoping rule

**Kill scope is always `(spiffe_id, resource_id)` — never "all connections for this
SPIFFE."** ACL entries are keyed per-resource (`allowed_spiffe_ids` on each `AclEntry`,
`connector/src/policy/mod.rs`). A device can be authorized for resource A and lose
resource B in the very same snapshot; a device-wide kill would incorrectly drop the
still-valid session to A. Verified against `resolve_resource` — there is no global
per-device allow-list, only per-resource ones.

## Files

| File | Change |
|------|--------|
| `connector/src/policy/mod.rs` | snapshot-diff helper |
| `connector/src/control_stream.rs` | diff-and-cancel in the `AclSnapshot` handler; authorization/registration race fix |
| `connector/src/agent_tunnel.rs` | `RelaySession::relay_stream()` — fix the confirmed `d2s` child-task leak |

## Design

1. **`connector/src/policy/mod.rs`** — before the existing `policy_cache.update(snap)`
   overwrites the stored snapshot (`self.snapshot.write() = Some(snapshot)`), flatten the
   **previous** snapshot into `HashSet<(spiffe_id, resource_id)>` by iterating each
   entry's `allowed_spiffe_ids`. Expose this as a small helper, e.g.
   `fn allow_set(&AclSnapshot) -> HashSet<(String, String)>`, so both old and new
   snapshots can be flattened the same way for a diff.
2. **`connector/src/control_stream.rs`**, in the `Some(CBody::AclSnapshot(snap)) => { ... }`
   match arm:
   - Compute `old_set = allow_set(&previous_snapshot)` **before** calling `policy_cache.update(snap)`.
   - Compute `new_set = allow_set(&snap)`.
   - For every `(spiffe, resource)` in `old_set - new_set`: look up the registry's inner
     `HashMap<SessionId, CancellationToken>` for that key and `.cancel()` **every**
     token in it — a `(spiffe,resource)` pair can have more than one live session
     (Phase 1's nested-map design). Do **not** remove the outer key here; each
     cancelled task's own drop guard removes its own `SessionId` (and the outer key,
     once its inner map is empty) as it unwinds — this diff-and-cancel step only
     signals cancellation, it never owns deregistration itself.
   - This runs inline on the control-stream task. `policy_cache.update()` is synchronous
     (no `.await` inside — it's a `parking_lot::RwLock` write), and
     `CancellationToken::cancel()` is cheap and non-blocking, so an inline loop over
     dropped pairs does not meaningfully stall the control stream even with many
     affected tunnels.

## Authorization/registration race — unconditional re-check, not a version comparison

A plain "diff snapshots and cancel dropped pairs" design has a window: a tunnel can
authorize, then finish registering into the Phase-1 registry **after** a diff-and-cancel
pass has already run and scanned the registry for the very change that should have
excluded it. Such a tunnel is never observed by that scan and would survive
indefinitely on stale authorization.

**Gating the re-check on "did the ACL version change since authorization" is not safe
and must not be used.** Policy versions in this system are currently **process-local**
— a controller restart can reset the counter without the underlying policy content
necessarily lining up with what a version-equality check assumes, so a version
comparison can miss a real content change. Skip version comparison from the
control-flow decision entirely:

1. Register the session (Phase 1) once ACL resolution succeeds.
2. **Immediately after registering, unconditionally re-run the same
   `is_allowed(resource, spiffe)` check** against whatever the *current* policy cache
   holds — no "only if the version differs" gate. This reuses the same cheap check
   `handle_stream` already performed to authorize in the first place; running it once
   more right after registration is negligible overhead for the correctness it buys.
3. If that re-check fails, cancel the just-registered token immediately — don't wait
   for a future snapshot's diff pass, since there may not be a "next" diff soon.
4. Policy version numbers may still be logged for diagnostics (Phase 3), but must never
   decide *whether* the re-check runs.

## Relay child-task cancellation — confirmed bug, not a hypothetical

`RelaySession::relay_stream()` (`connector/src/agent_tunnel.rs`) spawns a separate
device-to-Shield `d2s` task and only calls `d2s.abort()` as the **last line of the
function**, after its own `while let Some(event) = self.event_rx.recv().await` loop
exits **normally**. If the *outer* tunnel task (the one registered in Phase 1) is
cancelled externally via the registry, the outer future is dropped mid-`.await` inside
`relay_stream()` — the `d2s.abort()` line never executes, and `d2s` is orphaned,
continuing to relay one direction of a stream whose other direction is already gone.

Fix — pick one:
- **Shared token:** pass the same `CancellationToken` used for the outer task's
  registration down into `relay_stream()`, and have it also gate/cancel `d2s` (e.g.
  `d2s`'s task also selects on `token.cancelled()`, or `d2s`'s `AbortHandle` is captured
  and explicitly `.abort()`'d as part of the outer cancellation path, not just at normal
  loop exit).
- **Unified scope:** restructure `relay_stream()` so both directions run inside the same
  `tokio::select!` as the outer task's cancellation check, instead of one direction
  being a detached child task — this is the more invasive but structurally cleaner fix.

Either way, do not ship this phase with `d2s.abort()` reachable only via the function's
normal-exit path.

## Verified mechanics (from design review)

- `.abort()`/token-cancellation on a task currently inside `tokio::io::copy_bidirectional`
  (`device_tunnel.rs`) correctly drops the task at its next await point, which drops
  both `TcpStream`s and closes the sockets. `copy_bidirectional` is a standard
  cancellation-safe future — no `spawn_blocking`, no FFI, no non-awaiting loop in the way.
- `relay_udp`'s `tokio::select!` loop is also cancellation-safe.
- `RelaySession::relay_stream()`'s outer loop is itself cancellation-safe (a
  `tokio::select!`-driven `recv().await`), but its **child task `d2s` is not covered**
  by that cancellation — see above. This is a confirmed structural gap, fixed by this
  phase, not an open question to verify later.

## Tests

- Unit: snapshot diff with a resource entry dropping one SPIFFE out of several — only
  that `(spiffe,resource)` pair appears in `old_set - new_set`.
- Unit: same device, two resources, one drops — the registry lookup only cancels the
  dropped pair's tunnel(s); the other resource's tunnel for the same SPIFFE is untouched.
- Unit: two sessions sharing the same `(spiffe,resource)` key — a diff-and-cancel pass
  cancels **both** of their tokens, not just one (regression test for Phase 1's
  key-collision fix, exercised from this phase's diff logic).
- Unit/integration: a session that authorizes and registers just after a diff-and-cancel
  pass has already scanned the registry (and thus missed it) is still caught by the
  **unconditional** re-verification-after-registration step — this test must not rely on
  or check ACL version numbers at all, since the fix explicitly does not gate on them.
- Integration: open a tunnel, push a new ACL snapshot excluding its `(spiffe,resource)`,
  confirm the tunnel's socket closes within one control-stream processing cycle.
- Integration (relay-routed): open a relay-routed tunnel, cancel it externally, confirm
  **both** the outer task and `d2s` terminate — this is the regression test for the
  confirmed leak; it must fail before the fix and pass after.

## Build Check
```bash
cd connector && cargo build
```

## Implementation Checklist
- [ ] **M3-G1** `policy/mod.rs` — flatten previous snapshot into `HashSet<(spiffe,resource)>` before overwrite.
- [ ] **M3-G2** `control_stream.rs` — diff old vs. new on `AclSnapshot` receipt; cancel **every** token in the dropped pair's inner session map (not just one), never remove the outer key directly.
- [ ] **M3-G3** Authorization/registration race fix — **unconditional** `is_allowed` re-check immediately after registration, no ACL-version gating on whether the re-check runs.
- [ ] **M3-G4** `agent_tunnel.rs` — `RelaySession::relay_stream()`'s `d2s` child task shares cancellation with the outer task (shared token or unified `select!`); verified via the relay-routed cancellation regression test.
- [ ] **Build gate:** `cd connector && cargo build`

## Post-Phase Fixes
_None yet._
