---
type: fix-phase
sprint: 20
fix: 1
phase: 2
title: Full Hot-Apply
status: partial
depends_on: [1]
blocked_by: [Q3]
component: client
tags:
  - client
  - tunnel
  - net-stack
  - fix-01
---

# Fix 01 · Phase 2 — Full Hot-Apply

> Parent: [[Fix01-Client-Tunnel-Restart-On-Snapshot-Change]].
> Status (reconciled 2026-10-10): **PARTIAL.**
> - Delivered: transport-map swap, resource/route/listener hot-apply, socket lifecycle, stale-pool
>   eviction, zero-resource start and fallback. They were delivered in slices (§ Delivery status below),
>   each with its own live record.
> - **Outstanding:** connector ownership of flows, the per-connector selective close, the Q3 lifecycle
>   decision that governs that close, and a live U5/G9 run after the slices.
>
> Original status (until 2026-10-10): *NOT IMPLEMENTED — future stage after Phase 1 validation and the
> Q3 decision.*

Phase 2 is the larger architectural change. It must **not** be implemented as part of Phase 1.

> **Split (2026-10-05):** Phase 2 runs in slices. **2-A — connector topology hot-apply
> (transport-map swap only)** → [[Phase2A-Connector-Topology-Hot-Apply]] (status: live-accepted 2026-10-05, [[Phase2A-Live-Acceptance-2026-10-05]]). Resource/route/listener hot-apply, flow ownership/selective close and the P1-A
> handshake-timeout fix stay in this umbrella doc for later slices.
>
> **Update 2026-10-10:** resource/route/listener hot-apply (Fix 05 Phase 5-C) and the P1-A stale-pool fix
> (Fix 05-B) have since been delivered as Fix 05 slices. Flow ownership/selective close is still open.

Goal: keep unaffected long-lived flows alive even when the configuration genuinely changes.

## Delivery status (reconciled 2026-10-10)

"Merged" means merged into `fixed-pendings` via the PR shown. Live status is taken from each slice's own
record; the caveats are theirs and are not restated as fixed here.

| Slice | Phase 2 scope it covers | Code | Live status | Record |
|---|---|---|---|---|
| Fix 01 Phase 2-A | update channel (transport `watch`); transport-map swap for new flows; existing flows keep their own `Arc<ClientTransport>`; TransportOnly classification | `b376106`, PR #111 merged | live-accepted 2026-10-05 | [[Phase2A-Live-Acceptance-2026-10-05]] |
| Fix 05 Phase 5-A | socket lifecycle; loop core extracted into `FlowTable` (`net_stack.rs:528`) | `f7af519`, PR #111 merged | live-accepted 2026-10-06 | [[Phase5A-Live-Acceptance-2026-10-06]] |
| Fix 05-B | P1-A: evict a silent connector's stale pooled QUIC connection | `0f7bc41`, PR #113 merged | **live-accepted-with-caveat** (B5 slow flows with two connectors; B4 unexplained code-0 close; day 2 on a rebuilt lab) | [[Fix05-Stale-QUIC-Pool-Live-Acceptance-2026-10-06]] |
| Fix 05 Phase 5-C | resource update channel; listener/interface-address add/remove; incremental routes and nft (`ResourceKernel`), with no global flush on hot-apply; per-key reset of a removed or changed resource's flows; `MutationFailed` → full restart | `167ffa5`, PR #114 merged | **live-accepted-with-caveat**: L1–L5 and L7–L15 on 2026-10-08; L6 passed on the 2026-10-10 re-run (single flow, substituted rig) | [[Phase5C-Live-Acceptance-2026-10-08]], [[Phase5C-L6-Rerun-Live-Acceptance-2026-10-10]] |
| Zero-resource startup | tunnel starts and recovers at zero resources (5-C FU-1/H3) | `e027a3a`, PR #115 merged | live-accepted Z0–Z3 2026-10-09 (Z3 = recovery full restart at zero) | [[ZeroResource-Startup-Live-Acceptance-2026-10-09]] |
| F-3 (connector) | connector revokes sessions when a resource's address, port or protocol changes, so revocation is not weakened by hot-apply | `2a44ab2`, PR #116 merged | live-accepted 2026-10-09 (L1–L8) | `../fix-connector/F3-Live-Acceptance-2026-10-09.md` |

Also related, not a Phase 2 slice: the Fix 05 empty-network transport delivery (`4781bc8`, controller)
is live-accepted-with-caveat ([[Fix05-Empty-Network-Live-Acceptance-2026-10-06]]).

## Remaining work (verified against code 2026-10-10)

1. **`ClientTransport` has no `connector_id`.** `transport.rs:81-87` holds `direct`, `direct_addr`,
   `relay: Option<RelayContext>` and health counters. Only `RelayContext` carries `connector_id`
   (`transport.rs:77`).
2. **Flows do not record connector ownership.** `ActiveRelay` (`net_stack.rs:232`) has no connector
   field. Its only close handle is `close_tag` (`net_stack.rs:251`, added by 5-C), which serves the
   per-resource-key reset, not a per-connector close.
3. **No per-connector selective close.** Today a flow whose connector is removed from the map keeps
   running on its own transport. This is pinned by `established_flow_survives_map_swap_that_removes_its_connector`
   (`net_stack.rs:1485`). The flow ends only when its QUIC path fails: 2-A row C (§4b), hold died with a
   `TimeoutError`, not a reset.
4. **Q3 lifecycle decision is open.** Items 1–3 depend on it. See § Q3 below.
5. **Tests still missing from § Tests:** "a connector disappears permanently → only dependent flows
   close" (blocked by items 1–4), and a renewal-sequence test with the decided Q3 behaviour.
6. **Live U5/G9 after the slices: not run.** The 5-C L6 re-run is not a U5/G9 run.

> **2026-10-10: deferred.** Established-flow recovery and connector failover (what items 1–3 would
> enable) is **deferred out of Sprint 20 and NOT IMPLEMENTED**; it is tracked as
> [[PENDING-18-Established-Connection-Recovery-Connector-Failover]]. Items 1–3 stay open, and so does Q3 below.

### Q3 (open)

Q3 is **not finalized** (parent: Decisions → Q3). The connector lifecycle policy is still under
discussion. It must keep distinct:
- transient control-plane interruption (e.g. certificate renewal);
- a configured connector that is temporarily unreachable;
- a connector intentionally removed from configuration;
- a connector whose certificate or identity is revoked.

No grace period, timeout or close policy is approved.

## Known limitations (carried, not fixed)

- **D3 remote-network move is still Structural** (full restart). `classify_applied` treats a changed
  `remote_network_id` as Structural (`daemon.rs:866-912`). Hot-applying it is 5-C FU-4 / investigation Q6.
- **smoltcp `IFACE_MAX_ADDR_COUNT = 2`.** One slot is 100.64.0.1, so one resource IP applies and further
  new IPs go pending (5-C H1, FU-2).
- **Listeners bind by port only** (`socket.listen(port)`, `net_stack.rs:1051`). Two resource IPs on the
  same port can resolve to the wrong key (5-C FU-3).
- **F-3 FU-1:** a `route_type`/`shield_id` change (protect toggle) does not revoke connector sessions
  (see the F-3 fix doc).
- **5-B caveats** as listed in its record.

## Preconditions

- Phase 1 is implemented, tested, and live-validated (U5/G9).
- Q3 (how the client handles vN+1 during connector renewal) is resolved using Phase 1 live evidence.
  See the parent doc, "⚠ Unresolved design decision / risk: vN+1".

> 2026-10-10: Phase 1 is `done` per [[Phase1-AppliedConfig-Restart-Decision]] (implemented `6d26479`,
> live evidence 2026-10-01/02). Its own acceptance checklist still shows the U5/G9 boxes unticked; that
> ambiguity is left as is. Q3 is still open and now gates only the remaining connector-flow work.

## Scope

Original scope. Each item is annotated with where it was delivered (2026-10-10).

- **Update channel** from the daemon into `net_stack::run`, polled without blocking in the smoltcp
  loop, so the running data plane accepts updates without destroying the TUN/interface/`SocketSet`.
  → *delivered: 2-A transport `watch`, 5-C resource channel.*
- **Device-agnostic, testable net-stack update handling.** `net_stack::run` currently requires a
  real `tun::AsyncDevice`, so the loop core needs a harness. → *delivered: 5-A `FlowTable`.*
- **Listener / interface-address add/remove** for added/removed resources. → *delivered: 5-C.*
- **Incremental TUN route add/remove** (nft rules, `ip route replace`/`del`) without the global
  `cleanup_policy_routes()` flush (`tun.rs:63`). → *delivered: 5-C `ResourceKernel`; the flush is used only at `up`.*
- **Transport configuration updates for new flows**: swap the transport map. Existing flows keep
  their own `Arc<ClientTransport>` / QUIC connection. → *delivered: 2-A.*
- **`transport.rs` connector ownership:** `ClientTransport` gains `connector_id` (today only the
  optional `RelayContext` has it, `transport.rs:44-57`). → ***outstanding** (now `transport.rs:77`, `:81-87`).*
- **Flow → connector tracking:** `ActiveRelay` records the chosen connector plus a per-flow close
  handle (today it has neither, `net_stack.rs:199-206`, `:480-555`). → ***outstanding**; 5-C added
  `close_tag` for per-resource-key reset only.*
- **Close only flows affected** by a connector/resource that is removed *permanently*, subject to the
  Q3 decision for transient vN+1 removal. → *resource: delivered (5-C, L6/L9). Connector: **outstanding**, Q3 open.*
- **Keep unaffected existing flows alive.** → *delivered for TransportOnly (2-A) and ResourceDelta (5-C).*
- **New flows use the new transport configuration.** A resource with no valid connector keeps failing
  closed for new flows (`Some(None)`, `net_stack.rs:360-364`). → *delivered: 2-A.*
- **Fallback:** if an incremental route change or apply fails, do a full restart (fail-closed).
  → *delivered: 2-A `Fallback`, 5-C `MutationFailed`; live in zero-resource Z3.*
- A full down/up stays for genuinely structural changes. → *kept (D3, D4).*
- Revocation semantics must not be weakened: a resource or access removal must still close its
  flows. → *client per-key reset (5-C) plus connector ACL diff, extended by F-3.*
- Live U5/G9 validation after Phase 2. → ***outstanding.***

## Files (expected)

Original plan. The "Actual" column was added 2026-10-10.

| File | Change (planned) | Actual |
|---|---|---|
| `client/src/daemon.rs` | hot-apply dispatch from the Phase 1 decision point | changed by 2-A, 5-C, zero-resource |
| `client/src/runtime.rs` | `TunHandle` carries the data-plane update sender | changed by 2-A, 5-C |
| `client/src/net_stack.rs` | update receiver in `run`; listener/address add/remove; transport-map swap; per-flow connector tracking + selective close; device-agnostic loop core | changed by 2-A, 5-A, 5-B, 5-C; **per-flow connector tracking + selective close not done** |
| `client/src/tun.rs` | incremental flow/route add/remove without the global flush | changed by 5-C |
| `client/src/transport.rs` | `ClientTransport` carries `connector_id` | changed by 5-B (pool eviction); **`connector_id` not added** |
| net-stack / transport tests + test harness | Phase 2 tests | per slice (see each slice doc) |

Not expected to change: `tunnel_pool.rs` (QUIC connections already outlive the transport map,
`tunnel_pool.rs:320-372`; list it only if repo evidence later proves it's needed), `relay_pool.rs`,
`crl.rs`, `proto/`, all controller code (including the notifier files).
*2026-10-10: `tunnel_pool.rs` (and `transport.rs`) were changed by Fix 05-B (stale-pool eviction, the P1-A item below).*

## Tests

These go in the `client/src/net_stack.rs` tests and need the device-agnostic loop harness.
`MockDirect`/`MockRelay` in `transport.rs` can be reused.

Status per item (2026-10-10). Delivered items are covered by the slice docs' test lists.

- An ACL change that keeps an existing flow's resource → the flow stays alive. *(5-C)*
- A transport-only change → the existing flow stays alive. *(2-A)*
- A connector disappears permanently → only dependent flows close. ***Missing**: today the existing flow
  survives by design (`established_flow_survives_map_swap_that_removes_its_connector`).*
- Unaffected flows stay alive across any of the above. *(2-A, 5-C, for the delivered cases)*
- New flows use the newly applied transport configuration. *(2-A)*
- TUN routes are added/removed incrementally (`tun.rs`), with no global flush. *(5-C)*
- A failed hot-apply falls back to a full restart. *(2-A, 5-C, zero-resource)*
- A renewal sequence (with the Q3 vN+1 behaviour) doesn't reset a long-lived TCP connection.
  ***Missing**; depends on Q3.*

## Acceptance — **Partially satisfied** (reconciled 2026-10-10)

- [x] Existing unaffected long-lived flows survive applicable configuration changes, for TransportOnly
      (2-A rows A/B/C) and ResourceDelta (5-C L5; L6 "other flows survive", 10-08 row 3).
- [x] Removing a **resource** closes only the flows that depend on it: 5-C L6 (re-run 2026-10-10, client
      RST on a pure removal) and L9; connector side extended by F-3.
- [ ] Removing a **connector** closes only the flows that depend on it. Not implemented (Remaining
      work 1–3); gated by Q3.
- [x] New flows use the updated transport configuration (2-A).
- [x] Incremental route changes work (5-C L4, L7, L8).
- [x] A failed hot-apply falls back safely to a full restart (5-C `MutationFailed`; live zero-resource Z3).
- [ ] Phase 2 unit tests pass. Each slice's gates passed, but the two tests marked Missing above don't
      exist yet.
- [ ] **Live, PENDING / NOT YET RUN:** U5 and G9 pass after Phase 2.

Caveats that carry: 5-B and 5-C are `live-accepted-with-caveat` (see their records); the known
limitations above.

## Input from Phase 1 live run (2026-10-02)

Read `Phase1-AppliedConfig-Restart-Decision.md` → **Finding P1-A** first. When a connector is lost without a clean shutdown, its pooled QUIC connection stays "open" for up to the quinn idle timeout, and every new tunnel pays the 5 s `TUNNEL_HANDSHAKE_TIMEOUT` before falling through to the next connector. Today the full VPN restart flushes that dead transport as a side effect. Hot-apply removes the restart, so Phase 2 must explicitly drop the transports for removed connectors when it applies a change, and should evict or mark-failed a pooled connection after a handshake timeout. Fix-connector (`6362d1f`) means renewals no longer bump versions; connector loss/return (v+1 on disconnect, v+1 on shield move, v+1 on return) is now the main restart trigger Phase 2 must handle.

> 2026-10-10: the evict-on-silent part of P1-A was delivered by Fix 05-B (`0f7bc41`, live-accepted-with-caveat,
> [[Fix05-Stale-QUIC-Pool-Live-Acceptance-2026-10-06]]).
