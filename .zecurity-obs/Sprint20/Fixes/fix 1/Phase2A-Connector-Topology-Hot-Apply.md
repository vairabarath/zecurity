---
type: fix-phase
sprint: 20
fix: 1
phase: 2-A
title: Connector Topology Hot-Apply (transport-map swap)
status: live-accepted
depends_on: [1]
component: client
tags:
  - client
  - tunnel
  - net-stack
  - fix-01
---

# Fix 01 · Phase 2-A — Connector Topology Hot-Apply

> Parent: [[Fix01-Client-Tunnel-Restart-On-Snapshot-Change]] · Umbrella: [[Phase2-Full-Hot-Apply]] ·
> Previous: [[Phase1-AppliedConfig-Restart-Decision]] (done; read Finding P1-A).
> Status: **IMPLEMENTED, LIVE-ACCEPTED 2026-10-05** ([[Phase2A-Live-Acceptance-2026-10-05]]). Plan approved
> 2026-10-05 (D1–D5 as written). Unit tests pass. Not committed.

Phase 2-A is the first slice of Phase 2. It covers **connector topology changes only**. For a
connector-only change it swaps the transport map inside the running `net_stack` instead of doing a
full VPN restart. Everything structural keeps the existing Phase 1 full restart.

## Objective

```
connector-only change → classify TransportOnly → build new transport map → publish into the running
net_stack → TUN/routes/nft/listeners untouched → existing flows untouched → NEW flows use the new map
```

The three-way decision:

| Class | Action |
|---|---|
| `NoChange` | keep the tunnel (Phase 1 behaviour, unchanged) |
| `TransportOnly` | hot-apply the connector transport map |
| `Structural` | existing full `handle_down` → `handle_up` (unchanged) |

## Step 1: source inspection (2026-10-05, branch `feat/sprint20-m1-phase2` @ `12b8415`)

Working tree: only docs modified or untracked (Phase 1/Phase 2 docs, run sheet, Fix02-04, handoff,
playbook, lab scripts, `.localdev/`). **No client source modified.** Phase 1 is `6d26479`.

| Item | Current source | Matches the investigation? |
|---|---|---|
| `AppliedConfig` | `runtime.rs:46-54`: identity PEMs + `entries: Vec<AppliedEntry>`; `AppliedEntry` = address, port, protocol, `remote_network_id`, `preferred_connector_id`, `coords` (`runtime.rs:33-41`) | yes |
| `effective_config` | `daemon.rs:3202-3291`: SPIFFE-filtered, entries sorted by (address, port, protocol, rn, preferred); coords preferred-first, rest sorted | yes |
| `needs_full_restart` | `daemon.rs:828-838`: `true` if applied/acl/device is missing, else `effective_config != applied` | yes |
| `run_restart_decision` | `daemon.rs:843-875`: `task_dead \|\| needs_full_restart` → `down_up()`, else keep | yes |
| `perform_tunnel_restart` | `daemon.rs:881-910`: returns early if `tun_slot` is `None`, else `run_restart_decision` with a down/up closure | yes |
| `TunHandle` | `runtime.rs:7-14`: `abort`, `route_count`, `applied`, stored as `Option<Arc<TunHandle>>` (`runtime.rs:137`) | yes |
| `net_stack::run` | `net_stack.rs:210-215`: `(dev, allowed_entries, transports: Arc<HashMap<(Ipv4Addr,u16), Option<Vec<Arc<ClientTransport>>>>>, relay_resync)`; map read **only** at flow accept, `net_stack.rs:326` | yes |
| map builder | `build_transports_by_resource_with_crl`, `daemon.rs:3320-3375`: per-connector `ClientTransport` dedup by `connector_id` (`:3352`); a fresh `TunnelPool` + `RelayPool` per connector (`:3399`, `:3412`); `?` makes it all-or-nothing | yes |
| `relay_crl` | `runtime.rs:135`; set once by `handle_up` (`daemon.rs:652-658`) and shared across restarts | yes; usable for hot-apply |
| Callers of `restart_tunnel_if_running` | IPC Sync `:290`, IPC Resources `:325`, PostLoginState `:522`, early transport resync `:2638`, 60 s tick `:2699` → `register_restart_request` → `perform_tunnel_restart` | yes, 5 callers |
| `TunHandle` construction sites | `daemon.rs:763` (handle_up), tests `:1388`, `:1422` | — |
| Existing tests | `daemon.rs` `restart_decision_tests` (`:1307-1615`), coordinator tests (`:1040-1304`), `daemon_tests.rs` (`effective_config_*`, `build_transports_*`, `resolve_*`), `transport.rs` mocks, `net_stack.rs` handshake tests | — |
| tokio | 1.52.1, `full` feature → `tokio::sync::watch` available | — |

**Discrepancies with the investigation report:**

1. The existing test `restart_decision_vn_plus_1_connector_absent_restarts` (`daemon.rs:1463`) asserts
   that a connector removal does a **full restart**. Phase 2-A makes that case `TransportOnly`, so
   this test **must change** (to assert hot-apply with 0 down/up calls). It is an existing test being
   rewritten, not a new one.
2. The investigation didn't mention that `remote_network_id` sits on the entry. Phase 2-A treats an
   RN change on a resource as **Structural** (fail-closed; see D3).
3. No other discrepancy found.

## Step 2: classifier (pure)

New `pub(crate) enum ConfigDelta { NoChange, TransportOnly, Structural }` and
`pub(crate) fn classify_delta(applied: Option<&AppliedConfig>, acl, transport, device) -> ConfigDelta`
in `daemon.rs`, next to `needs_full_restart`.

```
applied/acl/device missing                         → Structural   (same as needs_full_restart today)
candidate == applied                               → NoChange
identity differs (spiffe_id, cert, key, tpm, ca)   → Structural   (Phase 1 policy kept)
resource key set differs                            → Structural
  key = (address, port, protocol, remote_network_id), as a sorted multiset over entries
otherwise (only preferred_connector_id and/or coords differ) → TransportOnly
```

TransportOnly is decided by **proving** equality of everything else, not by recognising connector
fields. Any field added to `AppliedEntry`/`AppliedConfig` later fails this equality → Structural.
To keep that guarantee, the check compares the full entries with the connector fields cleared
(`preferred_connector_id = ""`, `coords = []`) rather than an explicit allow-list of fields.
`needs_full_restart` stays, as `classify_delta(...) != NoChange`, so the existing callers and tests are
untouched.

## Step 3: transport map into the running net_stack

**Mechanism: `tokio::sync::watch`.** Checked against the alternatives:
- `watch` keeps exactly the latest value, publishes atomically, and needs no lock across `.await`.
  `borrow()` is a short read guard, so the accept path at `net_stack.rs:326` does
  `let map = rx.borrow().clone();` (an `Arc` clone) and drops the guard before any await. The loop
  never awaits on it, so the smoltcp tick is unchanged.
- `mpsc` would queue stale maps and needs draining each tick. Not needed: only the latest map
  matters.
- `Arc<RwLock<Arc<Map>>>` would also work, but `watch` adds "receiver gone" detection: `send()` errors
  when the task has died. That is the fallback trigger below.

Changes:
- `net_stack.rs`: `pub type TransportMap = HashMap<(Ipv4Addr,u16), Option<Vec<Arc<ClientTransport>>>>`.
  `run(.., transports: watch::Receiver<Arc<TransportMap>>, ..)`. At accept, the existing
  `match transports.get(&(ip, port))` reads from the snapshot taken at that moment. A small helper
  `transports_for(rx, key)` is extracted for unit tests.
- `runtime.rs`: `TunHandle` gains `transport_tx: watch::Sender<Arc<TransportMap>>`. `Debug` stays
  key-free.
- `daemon.rs` `handle_up`: `let (tx, rx) = watch::channel(transports)`. `rx` goes to `net_stack::run`
  and `tx` into `TunHandle`. A small `allowed_entries_for(acl, device)` helper is extracted from the
  existing filter (`:632-641`) and used by both `handle_up` and hot-apply so the two can't drift.

Hot-apply `hot_apply_transport_map(state, build)` inside `run_restart_decision`, so it runs inside the
coordinator pass (serialised with every other restart/apply; see Step 8):

```
1. read state: tun_handle (Arc), acl, transport, device, relay_crl; candidate = effective_config(...)
   relay_crl None → Structural fallback
2. build the new map COMPLETELY (build_transports_by_resource_with_crl, all-or-nothing). No lock held.
   Err → log "transport map build failed, retaining existing map" → return Err (no swap; see Step 8)
3. re-take state.write(); require Arc::ptr_eq(current tun_handle, handle read in 1) and that
   classify_delta(handle.applied, *current* acl/transport/device) is still TransportOnly and its
   candidate == the one built. Otherwise → Structural fallback (state moved underneath us).
4. tx.send(Arc::new(map)): Err (receiver gone = net_stack died) → Structural fallback
5. replace tun_handle with Arc::new(TunHandle { same abort, route_count, transport_tx, applied: candidate })
6. log "transport map hot-applied successfully"
```

`AppliedConfig` changes only after a successful publish. No partial state is possible: the map is
built off to the side and published with a single `send`.

`TunHandle.transport_tx` can't be moved out of the `Arc<TunHandle>` being replaced. So it is
`Arc<watch::Sender<..>>`, and the new handle clones the `Arc`.

## Step 4: transport reuse (D1)

Rebuild **all** connectors' `ClientTransport`/pools on each hot-apply, with no reuse cache.

Why this is safe, from source:
- Existing flows hold neither the map nor `ClientTransport`. `relay_tcp_to_quic` consumes the
  `Vec<Arc<ClientTransport>>` in its `for` loop (`net_stack.rs:480`) and keeps only the selected
  `AuthenticatedStream`.
- That stream's quinn `SendStream`/`RecvStream` each hold a `ConnectionRef`
  (`quinn-0.11.9/src/send_stream.rs:35`, `recv_stream.rs:53`), which keeps the connection and its
  endpoint driver alive (`connection.rs:927-941`, `endpoint.rs:381`).

Cost: one new QUIC handshake per connector for the first new flow after an apply, and the per-transport
direct-path cooldown state (`transport.rs:55-56`) resets. That reset is the same as what a full restart
does today.

The old map: the previous `Arc<TransportMap>` is dropped by the `watch` channel on `send`, plus any
in-flight accept snapshot. Its pools close their **idle** connections when the last reference goes
(quinn implicit close). Connections used by live flows stay up.

## Step 5: existing flows

**Phase 2-A policy decision:** a flow whose connector disappears is **not** force-closed. The existing
data-plane behaviour handles it: QUIC keep-alive 10 s / default idle timeout 30 s → relay read error →
relay task ends.

Flow ownership isn't tracked (no flow → connector state exists, `net_stack.rs:199-206`), and adding it
is out of scope. The known follow-up stays separate: Finding 28 (the smoltcp socket isn't closed when
the relay task ends).

- Flow A→manoj, inkyank removed: the swap touches only the map, and A holds its own `ConnectionRef` →
  A continues.
- Flows A, B→manoj, inkyank added: the swap adds a new pool and touches nothing else → both continue.

## Step 6: preferred connector

Ordering comes from `resolve_entry_coords` → `ordered_transport_connectors_for_entry`
(`daemon.rs:3158-3200`): the entry's `preferred_connector_id` goes first, then the snapshot order. A
change of preferred connector changes `AppliedEntry.preferred_connector_id` and `coords[0]` →
TransportOnly → the new map lists B first → new flows try B first. Existing flows are never re-selected
(`relay_tcp_to_quic` selects once).

## Step 7: connector return

A returns → A is back in `coords` → TransportOnly → the new map includes a fresh A transport. B flows
are untouched, and new flows follow the new order. No restart.

## Step 8: failure behaviour

| Failure | Behaviour | Rationale |
|---|---|---|
| Map build fails (bad coords, unresolvable addr, cert/pool build error) | **D2: keep the existing map, `AppliedConfig` unchanged, no restart, return `Err`** with log `transport map build failed, retaining existing map` | A full restart calls the same builder in `handle_up` (`:675`), which would fail the same way and leave the VPN **down**. Keeping the working VPN is strictly better. `AppliedConfig` is unchanged, so the next sync re-attempts. |
| One connector can't be built | same as above (the builder is all-or-nothing) | no partial map |
| All connectors disappear | TransportOnly; the new map has `None` slots → new flows fail closed (`net_stack.rs:360-364`) | same result as a full restart today, minus killing the existing flows |
| `watch` receiver gone (net_stack exited) | `send` Err → full restart | the data plane is dead anyway |
| net_stack task dead | `task_dead` check first → full restart (Phase 1, unchanged) | — |
| Race with another sync | all callers go through the coordinator (`register_restart_request`), so passes are serialised. Plus the `ptr_eq` + re-classify check before publishing → Structural fallback if anything moved | IPC Down/`react_to_device_directive` call `handle_down` outside the coordinator: `ptr_eq` fails or `tun_handle` is `None` → no publish into a dead handle |
| `relay_crl` missing | Structural fallback | can't build without it; `handle_up` always sets it, so this is defensive |
| Classifier can't decide | Structural | fail closed |

## Step 9: tests (planned)

In the `daemon.rs` `restart_decision_tests` (fake state, injectable down/up and builder closures), the
`daemon_tests.rs` builder tests (real test certs), and the `net_stack.rs` tests (helper level).

| # | Test | Asserts |
|---|---|---|
| 1 | NoChange | 0 down/up, 0 builds, receiver unchanged |
| 2 | connector added (A → A+B) | TransportOnly; 0 down/up; 1 publish; new map's slot has 2 transports; `applied` updated |
| 3 | connector removed (A+B → B) | TransportOnly; 0 down/up; new map has no A (by `connector_id` in the applied coords); old flow `Vec` still alive |
| 4 | connector returns (B → A+B) | as 2, ordering preferred-first |
| 5 | preferred changes | TransportOnly; new `applied.entries[0].coords[0].connector_id == B` |
| 6 | resource added / removed / IP / port / protocol / RN changed | Structural; down/up called once; no publish |
| 7 | identity change; applied/acl/device missing | Structural |
| 8 | build failure (builder `Err`; plus a real invalid `connector_tunnel_addr` in `daemon_tests.rs`) | 0 down/up; receiver still holds the old map; `applied` unchanged; returns `Err` |
| 9 | receiver dropped | send fails → down/up called once |
| 10 | handle replaced during build (race) | no publish into the stale handle → Structural fallback |
| 11 | `transports_for` after a swap returns the new map; a `Vec` cloned before the swap still holds the old `Arc`s | net_stack read path |
| — | rewrite `restart_decision_vn_plus_1_connector_absent_restarts` → asserts hot-apply | existing test changes meaning (discrepancy 1) |

**Not unit-testable here:** real TCP flow survival through a swap (`net_stack::run` needs a root TUN
device). This is proven at the reference level (above) and must be proven live.

Gates: `cd client && cargo build && cargo test`. Report the delta against the pre-change baseline,
taken first.

## Step 10: logs

Emitted once per decision (decisions happen only on a version bump, never per poll):

- `effective config unchanged, keeping tunnel` (existing)
- `transport-only change, hot-applying connector map` (+ entry/connector counts)
- `transport map hot-applied successfully`
- `transport map build failed, retaining existing map` (+ error)
- `hot-apply not possible (<reason>), falling back to full restart`
- `structural configuration change, restarting VPN` (replaces `snapshot changed, restarting VPN`.
  **Live scripts that grep the old string must be updated:** `p1-mon.sh`, playbook §6)

## Step 11: live plan (do not run without approval)

Instruments (playbook): `fc_hold.py` long-lived socket through manoj, `p1-newconn.sh`, client journal,
connector journals, `ip -o link show zecurity0` (ifindex) sampled every 2 s, `nft list table inet
zecurity_client | sha256sum` before/after.

1. Hold via manoj; stop inkyank → expect `transport-only … hot-applied`, 0 `restarting VPN`, same
   ifindex/nft hash, hold 0 FAIL, new connections via manoj (manoj `tunnel_opened ok`). Expect the
   5 s penalty before the sync (P1-A, out of scope).
2. Start inkyank → hot-applied, same ifindex, hold survives, new connections follow the order.
3. Shield move (preferred change) → hot-applied, hold survives, new flows go to the new preferred
   connector (its journal shows `tunnel_opened ok`).
4. Resource edit → `structural … restarting VPN` (control).

## Out of scope (Phase 2-B and later)

Resource/ACL hot-apply; route/nft/listener/address hot-apply; flow ownership and selective close;
handshake-timeout evict/deprioritise (P1-A); cert-renewal → pool rebuild; transport reuse cache;
controller/connector changes; migrations.

## Open decisions (recommended default, applied unless told otherwise)

- **D1** rebuild all connector transports on apply (no reuse cache).
- **D2** on map build failure: keep the working VPN and map, return `Err`, retry on the next sync (no
  full restart).
- **D3** a `remote_network_id` change on a resource = Structural.
- **D4** identity change = Structural (Phase 1 policy).
- **D5** rename the restart log line to `structural configuration change, restarting VPN`; update the
  live scripts that grep it.

## Files (planned)

| File | Change |
|---|---|
| `client/src/daemon.rs` | `ConfigDelta`, `classify_delta`, `allowed_entries_for`, `hot_apply_transport_map`, dispatch in `run_restart_decision`/`perform_tunnel_restart`, `handle_up` creates the watch channel; tests |
| `client/src/runtime.rs` | `TunHandle.transport_tx` |
| `client/src/net_stack.rs` | `TransportMap` alias, `watch::Receiver` param, `transports_for` helper at accept; tests |
| `client/src/daemon_tests.rs` | classifier + real-builder failure tests |

Not changed: `tun.rs`, `transport.rs`, `tunnel_pool.rs`, `relay_pool.rs`, `crl.rs`, controller,
connector, proto.

## Implementation (2026-10-05, uncommitted, on `12b8415`)

### Production files
| File | Change |
|---|---|
| `client/src/daemon.rs` | `ConfigDelta`; `structural_view` + `classify_applied` + `classify_delta` (pure); `needs_full_restart` now `#[cfg(test)]` = `classify_delta == Structural`; `allowed_entries_for` (shared by `handle_up` + hot-apply); `HotApplyOutcome` + `hot_apply_transport_map`; `transport_apply_pending`; three-way dispatch in `run_restart_decision(state, build, down_up)`; `perform_tunnel_restart` passes the real builder; `handle_up` creates the `watch` channel and stores the sender; the 60 s tick (`sync_and_restart_if_changed`) also re-evaluates when a TransportOnly change is pending (D2 retry) |
| `client/src/runtime.rs` | `TunHandle.transport_tx: Arc<watch::Sender<Arc<TransportMap>>>` |
| `client/src/net_stack.rs` | `pub type TransportMap`; `run(.., transports: watch::Receiver<Arc<TransportMap>>, ..)`; `transports_for()` read at flow accept (the only read site) |

Not touched: `tun.rs`, `transport.rs`, `tunnel_pool.rs`, `relay_pool.rs`, `crl.rs`, controller, connector, proto, migrations.

### Deviation from the plan
- **D2 retry trigger.** The plan said "retry on the next sync". A sync without a version bump never reached the
  decision, so a failed build would have waited for the next *bump*. Added `transport_apply_pending()`: the 60 s
  tick re-enters the coordinator when the running tunnel has a pending **TransportOnly** delta. A Structural
  pending (e.g. a renewed device cert) is deliberately not retried there, so Phase 1 behaviour is unchanged.
- **Tunnel gone during build** (IPC Down / device directive raced the build): `TunnelGone` → no publish, **no
  restart** (restarting would undo a user's Down). The plan had grouped this under "Structural fallback".
- **Log names.** The success log is `transport map hot-applied successfully` (with `resources`, `reachable`). The
  fallback log is `hot-apply not possible, falling back to full restart` (with `reason`), followed by the
  structural line.

### Tests (delta vs pre-change baseline 110 → 134, +24 new; 0 failed)
`daemon.rs::restart_decision_tests` (shared harness: real `watch` channel = running net_stack, fake builder,
counting down_up):
- NoChange: `restart_decision_nothing_changed_skips` (0 restart, 0 build, no swap)
- TransportOnly → hot-apply, 0 restart: `connector_added_hot_applies_without_restart`,
  `connector_removed_hot_applies_and_new_flows_cannot_select_it`, `connector_returns_hot_applies_with_preferred_first`,
  `preferred_connector_change_hot_applies`, `connector_coords_change_hot_applies`,
  `hot_apply_keeps_same_data_plane_handle` (same task id, same channel)
- **Rewritten existing test:** `restart_decision_vn_plus_1_connector_absent_restarts` →
  `restart_decision_vn_plus_1_connector_absent_hot_applies` (all connectors gone → hot-apply, new flows fail closed)
- Structural → restart, no build, no swap: `restart_decision_entry_added_restarts`,
  `restart_decision_resource_access_removed_restarts`, `resource_{ip,port,protocol,remote_network}_change_is_structural`,
  `resource_and_connector_change_together_is_structural`, `restart_decision_identity_change_restarts`,
  `identity_and_connector_change_together_is_structural`
- Unknown/unclassifiable: `classify_unknown_inputs_are_structural`, `classify_non_connector_field_differences_are_structural`
- Failure safety: `map_build_failure_retains_existing_map_and_vpn` (+ the pending retry succeeds),
  `transport_apply_pending_ignores_structural_and_nochange`, `receiver_gone_falls_back_to_full_restart`,
  `tunnel_replaced_during_build_is_not_published`, `config_changed_during_build_falls_back`,
  `restart_decision_dead_task_restarts_even_on_transport_only_change`
- Phase 1 tests kept: dead task, version/timestamp bump, tun_handle None, error propagation, needs_full_restart ×2

`net_stack.rs::tests` (real `relay_tcp_to_quic` over in-memory duplex "connectors"):
- `new_flows_read_the_latest_published_map`
- `established_flow_survives_map_swap_that_removes_its_connector` (bidirectional bytes after the swap, with the
  old map and the sender dropped)
- `connector_added_leaves_existing_flows_and_new_flow_uses_new_map` (A and B undisturbed; new flow C opens on
  the new preferred connector)

`daemon_tests.rs`: `build_transports_is_all_or_nothing_on_invalid_connector_coords` (real builder).

Not unit-testable here: real TCP through smoltcp/TUN across a swap (`net_stack::run` needs a root TUN). Live only.

### Commands
- `cd client && cargo build` → OK, 0 warnings from the changed code (1 pre-existing `posture_tests::running_as_root` dead-code warning)
- `cargo build --release` → OK
- `cargo test` → **134 passed, 0 failed** (baseline before the change: 110 passed)
- rustfmt: the new/changed code is formatted. `daemon.rs` still has 36 pre-existing unformatted hunks
  (37 at HEAD) and `runtime.rs`/`daemon_tests.rs` keep their pre-existing ones; no mass reformat.

### Live-script updates (D5)
`p1-mon.sh`, `fc-mon.sh` (repo `live-lab-scripts/` + `~/s20-run/`), and `~/s20-run/mon.sh` count both
`structural configuration change, restarting VPN` and the old `snapshot changed, restarting VPN`, plus
hot-apply start/ok/build-failed/fallback. Playbook §6 log table and §7.4 were updated.

## Acceptance

- [x] Plan approved
- [x] Implemented; `cargo build` + `cargo test` pass (110 → 134, +24, 0 failed)
- [x] Live: connector loss → hot-applied, 0 restarts, hold survives, same ifindex (Row A, 2026-10-05)
- [x] Live: connector return → hot-applied, 0 restarts, hold survives, same ifindex (Row B, 2026-10-05)
- [x] Live: shield's own connector removed → shield fails over, 2 hot-applies, 0 restarts (Row C, 2026-10-05)
- [x] Live: resource change still restarts — `structural configuration change, restarting VPN`,
      ifindex 12 → 13, hold reset (Row D, the negative control, 2026-10-05)
- [x] Live: flow that **owned** the removed connector is lost (Row C's flow-loss expectation) —
      not observed in session 1 (the hold had hit its cap); re-run in session 2: hold on 48410 via
      inkyank → `FAIL TimeoutError` 10 s after the kill, shield → manoj2, 3 hot-applies, 0 restarts,
      ifindex 14 unchanged (2026-10-05)

Session totals: session 1 — 4 hot-applies, 1 structural restart; session 2 (Row C re-run) — 3 hot-applies, 0 restarts; 0 unintended restarts. Full record in
[[Phase2A-Live-Acceptance-2026-10-05]], which also carries the findings that the lab connectors run
outside systemd (so `pkill -f cbundle` / `systemctl stop zecurity-connector` are no-ops), that a raw
SQL write never reaches the client, and that the 5-second stale-pool symptom was observed live.
