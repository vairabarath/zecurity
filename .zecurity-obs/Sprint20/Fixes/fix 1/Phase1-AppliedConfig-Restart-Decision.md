---
type: fix-phase
sprint: 20
fix: 1
phase: 1
title: AppliedConfig / Restart Decision
status: implemented-live-pending
depends_on: []
blocks: [2]
component: client
tags:
  - client
  - tunnel
  - fix-01
---

# Fix 01 · Phase 1 — AppliedConfig / Restart Decision

> Parent: [[Fix01-Client-Tunnel-Restart-On-Snapshot-Change]] (finding, evidence, version lifecycle,
> client architecture facts).
> Status: **planned, not implemented.** Phase 1 is the first implementation stage. It must be
> completed **and live-validated** before Phase 2 begins.

Phase 1 is intentionally not a full hot-apply implementation. It only prevents unnecessary full
tunnel restarts when the effective configuration has not changed.

Phase 1 must not silently change controller behaviour or revocation semantics.

## Scope

1. Introduce an `AppliedConfig`: the effective configuration the *running* tunnel was actually built
   from. `handle_up` records it next to `TunHandle`.
2. Compute the effective configuration for a newly fetched snapshot.
3. Compare the new effective configuration against `AppliedConfig`. Do **not**:
   - compare only snapshot version numbers;
   - compare against the previously *stored* snapshot. The stored snapshot is overwritten before the
     restart decision (`daemon.rs:2578`, `:2704`), and a stored but never-applied snapshot is
     irrelevant.
4. Effective state that matters:
   - the SPIFFE-filtered allowed set of (ip, port, protocol) (`daemon.rs:630-639`, `:706-718`);
   - per entry: `remote_network_id`, `preferred_connector_id`, and the resolved `ConnCoords` set
     (`connector_id`, `connector_tunnel_addr`, `connector_spiffe`, `relay_addr`,
     `relay_spiffe_id`; `resolve_entry_coords`, `daemon.rs:2817-2835`);
   - the device identity (cert/key) used to build the pools.
5. Ignore fields that don't affect how the tunnel behaves:
   - snapshot revision / `version`;
   - `GeneratedAt`;
   - `workspace_id`, and the RN and entry `name`;
   - top-level `relay_addr` / `relay_spiffe_id` (unread by `resolve_entry_coords`);
   - connector order within an RN, apart from the preferred-first rule
     (`ORDER BY c.last_heartbeat_at DESC`, `controller/internal/policy/store.go:450`).
6. If there is **no effective change**:
   - do **not** call `handle_down()`;
   - do **not** call `handle_up()`;
   - do **not** destroy the running TUN/smoltcp data plane;
   - log the skip.
7. If there is a genuine change that Phase 1 cannot hot-apply (any effective change in Phase 1,
   including a real ACL/resource change and a structural identity/device change such as re-login or a
   cert/key change): **keep today's full restart.**
8. Send all five existing restart callers through **one decision point**, serialized through the
   existing `TunnelRestartCoordinator`:
   1. IPC `Sync` (`daemon.rs:282-297`)
   2. IPC `Resources` (`:323-325`)
   3. `PostLoginState` with the tunnel running (`:521-522`)
   4. early transport resync `run_transport_recovery` (`:2270-2273`)
   5. background tick `sync_and_restart_if_changed` (`:2329-2334`)
9. Add the Phase 1 unit tests (below).
10. Run the live U5/G9 acceptance after Phase 1.

Expected effect: this closes the pure-renewal case (vN → vN+2 seen at the 60 s tick). It is **not**
claimed to fully solve long-lived flows until the live U5/G9 run passes. It doesn't address vN+1
being observed (parent doc, "⚠ Unresolved design decision / risk: vN+1"), and genuine config
changes still reset every flow.

## Files (expected)

| File | Change |
|---|---|
| `client/src/daemon.rs` | `AppliedConfig` builder + effective delta; single decision point for the 5 restart callers; `handle_up` records the applied config; `perform_tunnel_restart` kept for real changes |
| `client/src/runtime.rs` | `TunHandle` carries `AppliedConfig` |
| `client/src/daemon_tests.rs` (+ `daemon.rs` tests) | Phase 1 tests |

Not touched in Phase 1: `net_stack.rs`, `tun.rs`, `transport.rs`, `tunnel_pool.rs`,
`relay_pool.rs`, `crl.rs`, `proto/`, all controller code (including the notifier files).

## Tests

`client/src/daemon_tests.rs` holds the pure delta tests. The decision tests go in the `daemon.rs`
tests and reuse the `UpToDateAcl`/`NewAcl`/transport fetchers and the coordinator `FakeWork`.

- A revision change with an unchanged effective config (`version`, `generated_at`, and connector
  order all differ) → no full TUN teardown.
- Renewal sequence vN → vN+1 → vN+2: vN → vN+2 is a no-op. vN+1 behaviour is asserted exactly as
  documented (restart in Phase 1), not silently assumed.
- A real ACL/resource change → the existing full restart still happens.
- A structural identity/device change → full restart still happens.
- `handle_down()` / the restart work is **not** invoked for a pure renewal or an effective no-op
  (assert a call count of 0).

## Implementation notes (2026-09-30, uncommitted)

- Decision point: `perform_tunnel_restart` → `run_restart_decision` (`daemon.rs`). All 5 callers
  already go through `restart_tunnel_if_running` → `TunnelRestartCoordinator`. The callers are
  unchanged.
- `handle_up` records `TunHandle.applied = effective_config(acl, transport, device)` from the exact
  inputs it used.
- Full restart when: there's no `applied`, no ACL or device, the effective config differs, **or the
  net_stack task has finished** (`AbortHandle::is_finished`). The last condition is an addition from
  review: before Phase 1, every version bump incidentally revived a dead data plane.
- Log lines: `effective config unchanged, keeping tunnel` (skip), `snapshot changed, restarting VPN`
  (restart; now logged only from `run_restart_decision`), `net_stack task is not running,
  restarting VPN` (dead task).
- Side effect to check in the live run: a device-cert renewal still triggers a restart on the next
  sync (the cert is part of `AppliedConfig`), same as before.

## Acceptance — **Not yet satisfied** (code + unit tests done 2026-09-30; live run pending)

- [x] Phase 1 unit tests pass (`cd client && cargo build && cargo test`): 110 passed (baseline 90;
      +9 `effective_config` in `daemon_tests.rs`, +11 `restart_decision_tests` in `daemon.rs`).
- [ ] **Live, PENDING / NOT YET RUN:** U5 passes.
- [ ] **Live, PENDING / NOT YET RUN:** G9 passes (`tunnel_hold.py`, ≥ 3 connector renewals at
      15 m TTL).
- [ ] A renewal or effective no-op doesn't cause an unnecessary full tunnel restart.
- [ ] A long-lived TCP connection stays alive when the effective configuration is unchanged
      (0 resets caused by snapshot synchronization).
- [ ] No `snapshot changed, restarting VPN` for renewal-only effective no-op changes.
- [ ] Record whether vN+1 was actually observed/applied during renewal, and what it did to the
      long-lived flow. This is the evidence needed to resolve Q3 before Phase 2.
