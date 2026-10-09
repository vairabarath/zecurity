---
type: fix-phase
sprint: 20
fix: 5
phase: 5-C
title: Resource Hot-Apply (ResourceDelta) — implementation plan
status: implemented-pending-live
depends_on: [fix-01-phase-2a, fix-05-5a]
component: client
tags:
  - client
  - tunnel
  - net-stack
  - fix-05
---

# Fix 05 · Phase 5-C — Resource Hot-Apply (implementation plan)

> Source design: [[Fix05-Resource-Hot-Apply-Investigation]] (§6–§8, §15–§18).
> Status: **implemented 2026-10-08, unit gates + mutation check pass; live acceptance (L1–L15) NOT RUN. Not committed.**
> See [Implementation](#implementation-2026-10-08) at the end.

## 0. Scope

**In scope:**
- extend the classifier with `ResourceDelta`;
- hot-apply resource add/remove, IP change, port change, protocol change, access gained/lost and
  non-routable (No-DP) changes;
- resource + connector change applied as **one** combined apply with **one** map publish;
- the zero-resource state (the tunnel stays up);
- pre-mutation capacity validation (H1).

**Mandatory Structural (restart) boundaries — only these:** `remote_network_id` change (D3) and
device identity change (D4). Everything else is a classifier non-restart. A mutation failure leads to
a recovery restart, which is error handling, not a classification.

**Out of scope:**
- the 2-IP / smoltcp capacity fix;
- same-port wrong-key routing;
- RN-move hot-apply;
- controller, connector and shield changes;
- the error-recovery-at-zero-resources behaviour (FU-1).

Frozen: the Phase 2-A `hot_apply_transport_map` path (TransportOnly). Only its classification
neighbour changes: a mixed delta is now a ResourceDelta.

## A. Files to modify

### Production

| File | Functions / types | Why |
|---|---|---|
| `client/src/net_stack.rs` | **New** `pub(crate) fn routable_key(address, protocol, port) -> Option<(Ipv4Addr, u16)>` (IPv4 literal; protocol `tcp`, case-insensitive, or empty) | The single shared routable filter. It replaces the duplicate at `net_stack.rs:700-710` and `daemon.rs:702-714`. |
| | **New** `enum ResourceCmd { Remove{keys, ack}, RemoveAddrs{ips, ack}, Add{addrs, keys, ack} }`, `ack: oneshot::Sender<Result<()>>` | Sequenced, acknowledged loop mutations. |
| | `run(dev, allowed_entries, transports, resource_rx: mpsc::Receiver<ResourceCmd>, relay_resync)` | Sits next to the existing 2-A `watch` map channel (unchanged). |
| | Loop: drain `resource_rx` with `try_recv` once per pass, before `flows.service`; never await | The smoltcp tick is unchanged. |
| | `FlowTable::remove_listeners_and_close(keys)`, `FlowTable::add_listeners(keys)`, pending-close bookkeeping completed after `iface.poll` when every aborted handle is `relay_socket_removable` | Affected-flow close: match `sockets.get(h).local_endpoint()` against the removed `(ip, port)` → `abort()` + `terminated`. |
| | Address add/remove on the owned `iface` through `update_ip_addrs`, with a **checked** push (`Err` on overflow, never `let _ =`) | No silent address loss. |
| `client/src/tun.rs` | **New** pure `render_nft_batch(&BTreeSet<AllowedFlow>) -> String` | Desired state: `add table`, `add chain … type route hook output priority mangle; policy accept;`, `flush chain`, then one `add rule` per flow. An empty set gives an empty chain. |
| | **New** `TunManager::apply_nft_desired(flows)` | One `nft -f -` invocation (stdin). |
| | **New** `TunManager::add_routes(ips)` / `del_routes(ips)` | Per-IP `ip route replace X/32 dev zecurity0 table 105` / `ip route del X/32 table 105`. Keeps `policy_ips` accurate. |
| | **New** `TunManager::ensure_fwmark_rule()` | Reads `ip rule show`, then adds `fwmark 0x5a lookup 105 priority 49` only if absent. Never deletes it. |
| | Private command-runner seam (used only by the new methods); one shared helper for the rule text, also used by `configure_allowed_flows` | Rootless unit tests; startup and hot-apply rules can't diverge. |
| | `configure_allowed_flows`, `cleanup_policy_routes`, `create`, `cleanup`: **behaviour unchanged** | Startup and `handle_down` stay as they are. |
| `client/src/runtime.rs` | `TunHandle.resource_tx: Arc<mpsc::Sender<ResourceCmd>>`; `Debug` unchanged (key-free) | Cloned into every replacement `TunHandle`, like `transport_tx`. |
| `client/src/daemon.rs` | `ConfigDelta::ResourceDelta` | New class. |
| | `classify_applied`: the ResourceDelta rule (§B), with **exhaustive destructuring** of `AppliedConfig` / `AppliedEntry` | Fail-closed against future fields. |
| | **New** pure `plan_resource_apply(applied, candidate, current_ips, cap) -> ResourcePlan { removed, added, addr_del, addr_add, unapplied, target, flows_after }` | Delta and H1 capacity computation. |
| | **New** `hot_apply_resources(state, tun_slot, build, kernel)`; injectable `build` (map builder) and `kernel` (trait over the four `TunManager` methods) | The ordered apply (§C). |
| | `run_restart_decision`: `ResourceDelta` arm; `TransportOnly` arm unchanged (frozen 2-A) | Dispatch. |
| | `perform_tunnel_restart`: passes the real kernel adapter and `tun_slot` | Wiring. |
| | `transport_apply_pending` → `apply_pending` (TransportOnly **or** ResourceDelta) | 60 s retry for build failures and capacity-pending additions. |
| | `handle_up`: creates the `mpsc` channel, passes `resource_rx`, stores `resource_tx`, uses `routable_key`. Its three startup emptiness checks are **unchanged**. | Wiring. |
| | Logs: `resource change, hot-applying`, `resources hot-applied`, `resource additions pending: address capacity` (rate-limited), `no routable resources; tunnel kept up, nothing captured`, `resource apply failed after mutation, recovering with full restart` | Observability. |

**Not touched:** `transport.rs`, `tunnel_pool.rs`, `relay_pool.rs`, `crl.rs`, controller, connector,
shield, proto, migrations, and the body of `hot_apply_transport_map`.

### Tests

- `daemon.rs::restart_decision_tests`: harness extended with a fake kernel and a fake loop (records
  an ordered command log; can ack, fail or time out per command).
- `daemon_tests.rs`: pure `plan_resource_apply` and `routable_key` parity.
- `net_stack.rs`: the Fix 05-A `Lab` harness for the `FlowTable` commands.
- `tun.rs`: a new `#[cfg(test)]` module for the batch renderer, fwmark idempotence and command order
  via the runner seam.

### Documentation

- This file: an Implementation section is added after coding, with status implemented-pending-live,
  never "done".
- Investigation doc §8.C, Q8, §15 and §18 already aligned.
- Live-lab `p1-mon.sh` / `fc-mon.sh` (repo + `~/s20-run/`) count the new log lines in the same change.

## B. Data and control flow

```text
trigger (60 s tick / IPC Sync / IPC Resources / PostLoginState / early transport resync)
→ restart_tunnel_if_running → TunnelRestartCoordinator (existing serialisation)
→ perform_tunnel_restart (tun_slot None → Ok, unchanged)
→ run_restart_decision
     dead task → down_up (unchanged)
     classify_delta:
       NoChange      → keep
       TransportOnly → hot_apply_transport_map   (frozen 2-A)
       ResourceDelta → hot_apply_resources
       Structural    → down_up
```

Classifier (anything not explicitly matched is Structural):

1. `applied`, `acl` or `device` missing → Structural.
2. Candidate equals applied → NoChange.
3. `structural_view` equal → TransportOnly.
4. Identity differs → Structural (D4).
5. The same entry key `(address, port, protocol)` is in both with a different `remote_network_id` →
   Structural (D3).
6. Otherwise → ResourceDelta: entry additions/removals (routable or No-DP) plus any connector-field
   changes.

`hot_apply_resources`:

```text
PRE-MUTATION
  snapshot (handle, acl, transport, device, relay_crl) → candidate
  plan = plan_resource_apply(handle.applied, candidate, current IPs, IFACE_MAX_ADDR_COUNT)   (H1)
  map  = build(entries of plan.target)    full map: connector changes + slots for admitted
                                          additions; no removed or unapplied K
  lock tun_slot; Arc::ptr_eq(state.tun_handle, handle) else TunnelGone
MUTATION     remove side → ONE transport_tx.send(map) → add side
COMMIT       state.write(): ptr_eq re-check; tun_handle = TunHandle{same abort, transport_tx,
             resource_tx; applied = plan.target; route_count = |plan.flows_after|}; release tun_slot
```

**Combined resource + connector delta:** one map build and one publish, placed between the remove
side and the add side. Connector changes need no flow handling: existing flows hold only their
selected stream (2-A, `established_flow_survives_map_swap_that_removes_its_connector`).

From the configuration perspective the apply is atomic: `applied` changes in one write, and new flows
see either the old map or the new one. The kernel/smoltcp sequence itself is **not** atomic.

## C. Exact mutation ordering

**Removal side** (runs first; the final-resource case is identical — H2):

| # | Step |
|---|---|
| R1 | `ResourceCmd::Remove`: remove the listeners for removed K; abort every active socket whose `local_endpoint()` ∈ removed K |
| R2 | The ack completes only when every aborted socket is reset **and** reaped (FlowTable/`iface.poll` semantics). **Timeout 2 s → `Err` → recovery restart (H4).** |
| R3 | `apply_nft_desired(flows after removal)`; zero flows gives an empty chain |
| R4 | `ResourceCmd::RemoveAddrs(addr_del)` → ack |
| R5 | `del_routes(addr_del)` |

**Publish:** `transport_tx.send(map)`, exactly once per apply.

**Addition side:**

| # | Step |
|---|---|
| A0 | Capacity already validated pre-mutation (H1); unapplied additions excluded |
| A1 | `ensure_fwmark_rule()` (idempotent) |
| A2 | `ResourceCmd::Add`: smoltcp addresses first (checked push), then listeners → ack |
| A3 | `add_routes(addr_add)` |
| A4 | `apply_nft_desired(flows_after)`: **nft is always last** |
| A5 | Commit `applied = plan.target` |

Empty steps are skipped. A No-DP-only delta does only the publish and the commit.

## D. Failure and recovery boundary

| Point | Outcome |
|---|---|
| Plan / capacity / map build / `TunnelGone` / `ptr_eq` mismatch (**pre-mutation**) | No mutation, no restart. `applied` unchanged (capacity: see F). Retried by `apply_pending` on the next tick. |
| Any failure from the first mutation on (R1 command, publish, kernel command, smoltcp change): ack `Err` or 2 s timeout, closed channel, nft `Err`, route `Err`, checked-push `Err` | **Recovery restart** (`down_up`). `applied` is never set to a partial state. Not a classification. |
| Tunnel taken down while the apply held `tun_slot` (IPC Down / device directive) | No commit, no restart. `handle_down` completes cleanup. |
| Dead `net_stack` task | `down_up` (unchanged, checked first). |
| Recovery restart while zero resources are desired | `handle_up` refuses the empty startup state, so the VPN stays down (fail-closed). **FU-1 follow-up (H3)**, not solved in Phase 1. |

## E. Zero-resource behaviour

Removing the final K runs R1–R5 unchanged. What stays:

- `zecurity0` and `100.64.0.1/32`;
- the `net_stack` task with an empty `FlowTable`;
- `TunHandle` and both channels;
- the nft table with an empty chain;
- the fwmark rule (never removed);
- an empty table 105;
- a map with no slots for removed K;
- `relay_crl`.

`applied` becomes the zero-K target (a success). The log line `no routable resources; tunnel kept up,
nothing captured` fires.

A later addition is the normal addition side with `removed = ∅`. A1 re-ensures the fwmark rule (a
no-op when present), and no tunnel reconstruction is needed. The three emptiness checks in
`handle_up` stay startup-only.

> **Superseded 2026-10-09** by *Zero-resource startup* (end of this file): `handle_up` no longer
> refuses zero resources; it starts with the same empty state described above.

## F. AppliedConfig semantics (H1)

**Capacity:** free slots = `IFACE_MAX_ADDR_COUNT − 1 (100.64.0.1) − |current IPs − addr_del|`.

1. Removals free slots first.
2. Additions on an IP that already has an address always apply.
3. New IPs are admitted in **ascending IPv4 order** until the slots are full.
4. The rest are **unapplied/pending**.

Removals, connector changes and No-DP changes are never blocked by capacity.

| Case | `applied` after |
|---|---|
| success | `plan.target` = candidate |
| zero K | the zero-K candidate (No-DP entries kept): a success |
| capacity overflow | candidate minus the entries whose K is unapplied. The rest re-appears as a ResourceDelta (added = pending) every tick. If nothing is applicable: no mutation, no publish, and a rate-limited warning. Never silently dropped. |
| pre-mutation failure | unchanged |
| post-mutation failure | rebuilt by the recovery restart (`handle_up`) |

## G. Test changes

### Existing tests that change meaning

Rewrite these when implementing; they are **not** modified now.

| `daemon.rs` test | New expectation |
|---|---|
| `restart_decision_entry_added_restarts` | ResourceDelta, 0 down/up, 1 publish |
| `restart_decision_resource_access_removed_restarts` | ResourceDelta removal, 0 down/up |
| `resource_ip_change_is_structural` | ResourceDelta (remove + add) |
| `resource_port_change_is_structural` | ResourceDelta with an R1 close of `(ip, old)` |
| `resource_protocol_change_is_structural` | ResourceDelta (tcp→udp = removal) |
| `resource_and_connector_change_together_is_structural` | one combined apply, exactly 1 publish, ordering asserted |
| `classify_non_connector_field_differences_are_structural` | the `"tcp"→"TCP"` case becomes ResourceDelta (a dataplane no-op); the spiffe/tpm cases stay Structural |

**Unchanged:**
- `resource_remote_network_change_is_structural` (D3);
- `restart_decision_identity_change_restarts`, `identity_and_connector_change_together_is_structural` (D4);
- all 2-A TransportOnly tests and all Phase 1 tests.

Every ResourceDelta test asserts four things: down/up count, build count, the ordered command log,
and publish count.

### New tests

- **Classification:**
  - no change, add, remove, IP, port, protocol;
  - access gained / lost;
  - zero-K transition;
  - resource + connector together;
  - RN → Structural, identity → Structural;
  - metadata → NoChange;
  - No-DP-only → ResourceDelta with no kernel/loop work;
  - exhaustive-destructuring guard.
- **Dataplane:**
  - runtime add listener, then connect;
  - remove → the live flow gets RST, an unrelated flow keeps passing bytes, a new SYN gets RST;
  - the R1 ack waits for reset + reap, and the 2 s timeout gives `Err`;
  - a port change closes only `(ip, old)` flows;
  - final removal leaves no listeners and only `100.64.0.1`;
  - add after zero;
  - route and nft desired sets are correct after each step (no stale entries);
  - `render_nft_batch`, including the empty batch.
- **Combined:**
  - one publish;
  - remove commands before the publish, add commands after it;
  - existing flows untouched by the connector part.
- **Capacity:**
  - overflow detected before any mutation;
  - removals free slots first;
  - the ascending-IPv4 admission order;
  - an addition on an existing IP always applies;
  - fitting additions apply and overflowing ones stay pending, with a removal + connector change in the same delta;
  - `applied` excludes pending;
  - next tick: no mutation while it still doesn't fit;
  - the checked push `Err` is not silent.
- **Failure:**
  - validation failure → no mutation;
  - loop `Err` at R1/R4/A2 → 1 down/up;
  - R1 timeout → 1 down/up;
  - route `Err` → 1 down/up;
  - nft `Err` → 1 down/up;
  - `TunnelGone` → 0;
  - dead task → down/up.
- **Invariants:**
  - TUN handle and task unchanged at zero K;
  - the fwmark rule is never removed at zero K;
  - `ensure_fwmark_rule` doesn't add a duplicate;
  - re-add after zero works without a restart;
  - `routable_key` parity across `handle_up`, `net_stack` and the classifier.

### Gates

Record the baseline first, then report deltas:
- full `cargo test`;
- `cargo build --release`;
- rustfmt on new code only;
- clippy hits on the touched files = 0;
- mutation check: disable each fix line and record which new tests fail.

## H. Live acceptance criteria

> **Live run 2026-10-08:** [[Phase5C-Live-Acceptance-2026-10-08]]. L1–L5 and L7–L15 ✅; **L6 REOPENED**:
> the connector's ACL-diff teardown closes a removed resource's flow (FIN) before the client's 60 s poll
> runs R1 (`remove_listeners_and_close`), so the client reset was never observed on a pure removal.
> Function map and re-run plan are in the record's §6a. Status stays `implemented-pending-live`.

Run only with approval.

**Instruments:**
- ifindex sampler;
- `nft list table inet zecurity_client | sha256sum` and rule count;
- `ip rule | grep -c 'fwmark 0x5a'`;
- `ip route show table 105`;
- `fc_hold.py`;
- `p1-newconn.sh`;
- client journal counts.

| ID | Criterion |
|---|---|
| L1 | `structural configuration change, restarting VPN` = 0 for every resource row; = 1 for the RN and identity controls |
| L2 | `zecurity0` ifindex constant across every hot-applied row |
| L3 | fwmark rule count is exactly 1 throughout, including the zero state |
| L4 | table 105 and nft rules equal the expected set after every row |
| L5 | add resource → reachable; hold on another resource has 0 FAIL |
| L6 | remove resource → its hold reset, new connects refused, other flows survive |
| L7 | final-resource removal → 0 restarts, nft rule count 0, table 105 empty, tunnel up |
| L8 | re-add after zero → reachable, 0 restarts, same ifindex |
| L9 | port change → old-port hold reset by the client, new port OK |
| L10 | resource + connector in one change → one apply, 0 restarts, expected order in the journal |
| L11 | capacity overflow (3rd IP) → pending warning, ascending-IP admission, no silent loss, other changes applied, 0 restarts |
| L12 | `AppliedConfig` correctness: the apply log's resources/pending counts match the snapshot |
| L13 | Q5: an nft batch with an invalid last rule leaves the ruleset hash unchanged |
| L14 | Q5: flush/refill under a continuous probe → 0 failures |
| L15 | Q7: an existing connection to a newly added resource gets `ECONNRESET`; no `new TCP connection` log line for it |

Q6 network-move validation is not part of this phase.

## I. Approved decisions

| ID | Decision |
|---|---|
| H1 | Capacity: removals free slots first; existing-IP additions always apply; new IPs admitted in ascending IPv4 order; the rest pending (not in `AppliedConfig`, retried each tick, never silently dropped); removals and connector changes never blocked; not Structural; the capacity fix stays with the 2-IP investigation. |
| H2 | Removal order: close + listeners → ack (reset/reap) → nft → addresses → routes. The final resource is the same. |
| H3 | Recovery restart at zero desired K may leave the VPN down (fail-closed). Not solved in Phase 1 → **FU-1**. |
| H4 | R1 ack timeout 2 s; success only after reset + reap; timeout → `Err` → full-restart recovery; no partial apply. |

## J. Follow-ups (not in this phase)

- **FU-1:** error-recovery restart at zero desired resources (H3 / investigation I-Q3.3). **Resolved
  2026-10-09** by *Zero-resource startup* (live Z3: recovery at zero → tunnel up empty).
- **FU-2:** smoltcp address capacity (separate 2-IP investigation).
- **FU-3:** same-port wrong-key listener binding (separate).
- **FU-4:** RN-move hot-apply + controller `AutoMatchShield` on update (investigation §15 Q6).

## Acceptance checklist

- [x] Plan approved (2026-10-08, H1–H4)
- [x] Implementation authorized (2026-10-08)
- [x] Implemented; unit gates pass (deltas recorded)
- [x] Mutation check recorded (15/15 killed)
- [ ] Live L1–L15
- [ ] Committed (only when told)

## Implementation (2026-10-08)

Uncommitted working tree on `feat/sprint20-m1-phase2` (HEAD `86cda74`). Release binary sha256 `0f3adbbd1ec7be3a…`.

### Files changed (production)

| File | Change |
|---|---|
| `client/src/net_stack.rs` | `routable_key` (single routable filter, used by classifier, `handle_up`, `run`, apply); `ResourceCmd {Remove, RemoveAddrs, Add}` + `oneshot` acks; `run(…, resource_rx, …)`; loop drains `resource_rx` with `try_recv` after `iface.poll`, before `service`; `FlowTable::remove_listeners_and_close` (removes listeners, aborts sockets whose `local_endpoint()` ∈ keys, tags them), `close_pending`, `add_listeners`; `complete_pending_closes` acks a removal only when no tagged socket remains (RST dispatched by `iface.poll` and socket reaped by `service`); checked `add_iface_addrs` (capacity checked before any push → `Err`, no partial change), `remove_iface_addrs`. |
| `client/src/tun.rs` | `render_nft_batch` (add table / add chain / flush chain / add rule…; empty set = empty chain); `ResourceKernel` trait (`apply_nft_desired`, `add_routes`, `del_routes`, `ensure_fwmark_rule`, `addr_capacity`) implemented for `TunManager` via `nft -f -` and per-IP `ip route replace/del … table 105`; `ensure_fwmark_rule` reads `ip rule show`, adds only if absent, never deletes; `CmdRunner` seam; shared `mark_rule_match` + `FWMARK_RULE_ADD_ARGS` also used by `configure_allowed_flows` (same commands as before). `create`/`cleanup`/`cleanup_policy_routes` unchanged. |
| `client/src/runtime.rs` | `TunHandle.resource_tx: Arc<mpsc::Sender<ResourceCmd>>`. |
| `client/src/daemon.rs` | `ConfigDelta::ResourceDelta`; `resource_delta_allowed` (exhaustive destructuring of `AppliedConfig`/`AppliedEntry`; D4 identity → Structural; D3 same `(address, port, protocol)` with different RN set → Structural); `ResourcePlan` + pure `plan_resource_apply` (H1); `hot_apply_resources` (order §C, outcomes `Applied/NothingToApply/Superseded/TunnelGone/PreMutationFailed/MutationFailed`); `send_resource_cmd` with `RESOURCE_ACK_TIMEOUT = 2 s` (H4); `run_restart_decision` takes the kernel slot and gains the `ResourceDelta` arm; `resource_apply_pending` + `resource_pending` in the 60 s tick; `handle_up` creates the channel and uses `routable_key` (its three emptiness checks unchanged); new log lines (§A). |

Not touched: `transport.rs`, `tunnel_pool.rs`, `relay_pool.rs`, `crl.rs`, controller, connector, shield, proto, migrations. Live-lab `p1-mon.sh` / `fc-mon.sh` (repo + `~/s20-run/`) gained a `phase5c:` counter line.

### Gates (delta vs HEAD `86cda74`)

| Gate | HEAD | Now |
|---|---|---|
| `cargo test` (client) | 157 passed | 189 passed (**+32**), 0 failed |
| `cargo build --release` | ok | ok |
| clippy hits in new/changed code | — | **0** (remaining `daemon.rs` hits at 528, 5349, 5365, 5841–6222 are pre-existing, `git blame` → older commits) |
| rustfmt on new/changed code | — | clean; 2 remaining hunks are pre-existing adjacent lines (`use uuid::Uuid` order, `transport_pending` line), left as is |

Baseline count was taken from a clean `git worktree` of HEAD (same code as before editing).

### Tests

- **Meaning changed (7, assertions rewritten; names kept for traceability):** `restart_decision_entry_added_restarts`, `restart_decision_resource_access_removed_restarts`, `resource_ip_change_is_structural`, `resource_port_change_is_structural`, `resource_protocol_change_is_structural`, `resource_and_connector_change_together_is_structural` (all now ResourceDelta, 0 down/up, 1 build, exact ordered command log with `pub=0/1` marking the single publish), and the `"TCP"` case in `classify_non_connector_field_differences_are_structural` (spiffe/tpm cases still Structural).
- **Unchanged assertions:** D3 `resource_remote_network_change_is_structural`; D4 `restart_decision_identity_change_restarts`, `identity_and_connector_change_together_is_structural`; all Phase 1 and Phase 2-A tests. Harness plumbing only: `TunHandle { resource_tx }` in the two dead-task tests, kernel argument to `run_restart_decision`, `receiver_gone_falls_back_to_full_restart` also drops the new observer's receiver clone.
- **New (32):** classification (resource kinds, access gained/lost, D3/D4 dominance, metadata → NoChange, non-routable-only → no dataplane work); zero-K keeps task/channels, empty nft/routes, fwmark kept, re-add without restart; kernel desired-state after each step; H1 plan (ascending admission, existing-IP always, removals free first, removals never blocked) and the combined overflow+removal+connector apply with retry; failures (build → pre-mutation retry; loop `Err` at R1/R4/A2; R1 2 s timeout; kernel `Err` at each nft/route/fwmark step → 1 recovery restart, applied unchanged; tunnel replaced → no apply; dead task → down/up); dataplane `Lab` (runtime add serves; remove resets only affected flow, unrelated flow keeps passing bytes, new SYN refused, ack only after reset + reap; no-flow removal acks at once; final removal leaves no socket, re-add serves; checked address add/remove); `routable_key`; `tun.rs` batch, empty batch, single `nft -f -`, per-IP routes, fwmark add-only-when-absent / never duplicated.

### Mutation check (each line disabled alone; `restart_decision_tests`, `net_stack::tests`, `tun::tests`)

| # | Mutation | Result |
|---|---|---|
| M1 | D3 RN check disabled | killed (2) |
| M2 | ResourceDelta class removed | killed (19) |
| M3 | R1 acked before reset + reap | killed (1) |
| M4 | affected flows not aborted (lifecycle-only reset) | killed (1) — initially survived; test now asserts `rst_on_relay_failure == 0` |
| M4b | affected flows not touched | killed (2) |
| M5 | routes deleted before addresses | killed (4) |
| M6 | publish before remove side | killed (9) |
| M7 | nft not last on add side | killed (7) |
| M8 | H1 capacity ignored | killed (2) |
| M9 | commit candidate instead of plan target | killed (1) |
| M10 | post-mutation failure without recovery restart | killed (5) |
| M11 | fwmark added even when present | killed (1) |
| M12 | checked address push disabled | killed (1) |
| M13 | H4 ack timeout removed | killed (test hangs, 120 s guard) |
| M14 | zero-K treated as nothing-to-apply | killed (3) |

### Deviations from the plan text (behaviour unchanged)

1. `transport_apply_pending` was **not** renamed to `apply_pending`; a separate `resource_apply_pending` was added so the frozen 2-A function and its tests stay untouched. The tick retries either.
2. `hot_apply_transport_map` gained one line (`resource_tx: handle.resource_tx.clone()` in its `TunHandle` rebuild), required to compile after the struct change; its logic is unchanged.
3. `ResourceKernel` has an extra read-only `addr_capacity()` (defaults to `smoltcp::config::IFACE_MAX_ADDR_COUNT`) so H1 is testable with other capacities.
4. Pure planner and classifier tests live in `daemon.rs::restart_decision_tests` (not `daemon_tests.rs`); `routable_key` tests in `net_stack.rs`. Parity is structural: one function used at all four sites.
5. R1 also handles a listener that already received a SYN (abort + reap via the same tag). No dedicated test (timing-dependent); covered only by review.

### Known limits carried to live

- Production capacity is `IFACE_MAX_ADDR_COUNT = 2` (100.64.0.1 + one resource IP): a second **new** resource IP is held pending with `resource additions pending: address capacity` (H1, FU-2). Capacity is computed from the IPs in `AppliedConfig`; if `handle_up` already silently dropped one at startup (FU-2), free slots read 0.
- FU-1: a recovery restart while zero resources are desired leaves the VPN down (H3). *(Resolved
  2026-10-09, see Zero-resource startup; live Z3.)*

## Zero-resource startup (2026-10-09) — live-accepted, uncommitted

Live record: [[ZeroResource-Startup-Live-Acceptance-2026-10-09]] (Z0–Z3 pass).

Requirement (user): the VPN/TUN must start even when the device has no assigned resources. Zero
resources is a valid tunnel state; only the resource mapping is empty. The client logs clearly that
nothing is assigned and applies the mapping when a resource is assigned later.

### Change (client only)

| File | Change |
|---|---|
| `client/src/daemon.rs` | `handle_up` preconditions moved into pure `up_preflight(acl, device)`. It **fails closed only** when there is no ACL snapshot (never synced) or no device identity (not logged in). The three refusals are gone: `ACL snapshot has no entries`, `no accessible resources for this device`, `no TCP resources available`. With zero flows, `configure_allowed_flows(&[])` only clears stale policy, `net_stack` starts with an empty `FlowTable`, and `TunHandle` stores `applied` = zero-entry config with `route_count = 0`. New log `log_no_resources_assigned`: `no resources currently assigned to this device; tunnel up, waiting for resource assignment` (or `no routable (TCP) resources …` when entries exist but none is TCP/IPv4). `IpcResponse.synced_resources = Some(route_count)`. |
| `client/src/cmd/up.rs` | Prints `Zecurity is up. No resources are currently assigned to this device.` when `synced_resources == Some(0)`. |

No new data-plane code. The first assignment goes through the existing Phase 5-C `ResourceDelta` hot-apply: A1 ensures the fwmark rule, nft adds the table/chain, routes are added per IP, and the loop adds the address/listener.

Side effect: **FU-1/H3 resolved in code.** A recovery `down_up` at zero desired resources now brings the tunnel back up with an empty mapping instead of leaving the VPN down.

### Gates

- `cargo test`: **196 passed / 0 failed** (189 → +7). New tests in `daemon.rs::restart_decision_tests`:
  - `up_preflight_accepts_empty_acl_snapshot`
  - `up_preflight_accepts_no_entry_for_this_device`
  - `up_preflight_accepts_no_routable_entry`
  - `up_preflight_routable_entry_yields_flow`
  - `up_preflight_without_acl_snapshot_fails_closed`
  - `up_preflight_without_device_fails_closed`
  - `zero_start_tunnel_hot_applies_first_assigned_resource`: harness started at zero (`harness_zero_start`); the first assignment hot-applies with `down_ups = 0` and the same task and channels.
- `cargo clippy --all-targets`: 23 → 23 diagnostics (no new ones). `cargo fmt --check`: 84 → 84 diff hunks (all pre-existing).
- `cargo build --release`: sha256 `7d2da41544389150…`; `strings` contains the new log line.

### Live (2026-10-09, client `7d2da415`)

- Z0: old build `0f3adbbd` refuses (`ACL snapshot has no entries`, rc=1, no TUN). This is the baseline.
- Z1: new build, `up` at zero gives rc=0, the CLI message, the log line, and `zecurity0` ifindex 12.
- Z2: first assignment hot-applied at the next tick, with 0 restarts and the same ifindex/PID. fwmark and table 105 were created, and the probe went through the tunnel (12/12 × 200, 1:1 `new TCP connection`).
- Z3: induced route-delete failure on removal to zero, then `recovering with full restart`, then `zecurity0 up routes=0` and the new log line (ifindex 12→13). A re-assign then hot-applied again.
- 0 session errors and 0 non-200 probes. Details are in the live record.

### Open

- Not committed. To ship as a separate PR after #114 merges (user decision).
- Unchanged: the controller, the connector and FU-2 (2-IP smoltcp capacity).
- Not live: UDP-only assignment at startup (unit-tested only) and boot-time auto-start at zero.
