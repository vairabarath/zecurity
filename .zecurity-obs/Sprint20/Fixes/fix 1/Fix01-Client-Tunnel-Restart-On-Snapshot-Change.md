---
type: fix
sprint: 20
fix: 1
title: Client restarts the whole tunnel on every snapshot change
status: partial
source_rows: [U5, G9]
source_findings: [15]
severity: high
component: client (+ controller notify churn)
tags:
  - client
  - transport
  - tunnel
  - live-acceptance-fail
---

# Fix 01 — Client restarts the whole tunnel on every snapshot change

> Source: live acceptance rows **U5 (long-lived hold)** and **G9 / AT-G.6**, both FAIL. Run-sheet Finding 15.
> Status (reconciled 2026-10-10): **PARTIAL.**
> - Phase 1 decision logic is accepted (`done` per its doc).
> - Original G9 / AT-G.6 re-graded **PASS** (2026-10-06 hold, with limitations). This is renewal survival,
>   not Fix 01 regression proof.
> - Strict U5 **PASS** on 2026-10-10 (connector-route hold, [[U5-Connector-Route-Long-Hold-Live-Acceptance-2026-10-10]]). This is not
>   Fix 01 hot-apply proof; no configuration version changed during the hold.
> - Phase 2 is partially delivered through slices (2-A, Fix 05 5-A / 5-B / 5-C, zero-resource, F-3).
> - Outstanding:
>   - connector ownership of flows and the per-connector selective close;
>   - Q3 (open);
>   - a separate post-slice Fix 01 regression run (criterion still to be agreed).
>
> See [[Phase2-Full-Hot-Apply]] § Delivery status.
>
> Original status: *verified, not fixed. No code changed.*

## Failure (as observed)

- AT-G.6: *"the long-lived tunnel from U5 is still open after several connector renewals"*.
- Hold #2 (2026-09-26): one TCP connection to summa on local port 48394 got 93 × 200, then a
  `ConnectionResetError` at 17:37:22. That was the same second the client logged
  `transport snapshot stored version=13` → `snapshot changed, restarting VPN`. The connector had renewed
  its cert about a minute earlier.

## Verification (2026-09-28, this re-check)

| Check | Evidence |
|---|---|
| Restart path is a full TUN down/up | `client/src/daemon.rs:827-855` `perform_tunnel_restart`: `info!("snapshot changed, restarting VPN")` → `handle_down` → `handle_up`. `handle_down` (`daemon.rs:~772-777`) takes and **aborts** `tun_handle` and drops the TUN manager. Every smoltcp flow and QUIC stream dies |
| Triggered by *any* version change | `daemon.rs:2295-2338` `sync_and_restart_if_changed`: `if acl_changed \|\| transport_changed { restart_tunnel_if_running }`. Also `daemon.rs:2270-2273` (early transport resync) and `daemon.rs:285-290` (IPC `Sync`) |
| Frequency in the live run | Client journal 11:00→16:00 on 2026-09-28: **43 × `snapshot changed, restarting VPN`**. Background-sync causes: **33 × `acl_changed=true transport_changed=true`**, **9 × `acl_changed=false transport_changed=true`**, plus 1 early resync |
| Why versions move so often | A connector renewal reconnects its control stream: the controller logs `disconnected` → `connected` (e.g. 14:11:50/51). The close defer (`controller/internal/connector/control_stream.go:432-444`) and the activation path (`control_stream.go:380-392`) each call **both** `NotifyPolicyChange` and `NotifyTopologyChange`, but **only on a real transition** (`becameDisconnected` / `becameActive`). The notifiers themselves bump without comparing content (`internal/policy/notifier.go:57-79`, `internal/transport/notifier.go:57-72`), but each transition *is* a real content change: both compilers select only `c.status = 'active'` connectors (`internal/policy/store.go:449`, `internal/transport/store.go:61`), so the disconnected snapshot (e.g. v11) omits the connector and the reconnected one (v12) restores it. The churn is a **transient** real change: v12 ≈ v10 in content (not proven byte-identical; ACL also carries `GeneratedAt`). The client polls every 60 s, so it usually jumps v10 → v12 and restarts on a new number with the same effective connector/route content (client compares versions only: `daemon.rs:2574-2581`). With the 15 m test cert TTL, this occurred roughly every 5 min during the live run. The same mechanism can also occur whenever a connector or relay state transition causes the relevant snapshot revision to change |
| Reproducible cause | `restarting VPN` lines line up to the second with tunnel drops: 12:48–12:54 (5×), 13:13:49, 14:05:06, 14:46:06, 14:47:06 |

**Conclusion:** confirmed. The client's reaction (full TUN teardown on any version bump) is the root
cause. The transient connector disconnect/reconnect sequence causes the revision to change (a real but
transient topology change: the connector is briefly absent, then restored). What turns that transient
state transition into an interruption of healthy long-lived flows is that the client treats any
observed revision change as a reason for a full tunnel restart.
*(Corrected 2026-09-29 after tracing the controller version lifecycle.)*

### Renewal / version lifecycle (clarified 2026-09-29, client architecture investigation)

A connector cert renewal legitimately moves the revision twice: **vN → vN+1 → vN+2**.

- **vN+1**: the connector is `disconnected`, so it is absent from the active connector set (both
  compilers select `c.status = 'active'`).
- **vN+2**: the connector is `active` again and restored.
- The notifier revision is an **event/revision counter, not a snapshot-content hash**. Each bump
  reflects a real transition.
- Byte-identical snapshot comparison is not a reliable definition of effective state: ACL
  `GeneratedAt` always changes, and connector order within a remote network follows
  `ORDER BY c.last_heartbeat_at DESC` (`controller/internal/policy/store.go:450`).
- The controller is **not** producing incorrect "identical" snapshots, and content-hash dedupe is
  **not** the fix.

**The bug is client-side:** the client treats *any* ACL/transport version change as sufficient reason
to destroy and recreate the whole tunnel.

### Client architecture facts (verified in code, 2026-09-29)

- **No decision state.** `sync_acl_now_with` (`daemon.rs:2692-2704`) and
  `fetch_and_store_transport_with` (`daemon.rs:2576-2578`) overwrite `state.acl_snapshot` /
  `state.transport_snapshot` **before** the restart decision. Only two booleans (`version != old`)
  survive. Nothing records what the running tunnel was actually built from.
- **Five restart callers**, all ending in `restart_tunnel_if_running` → `perform_tunnel_restart`
  (`daemon.rs:827`) → `handle_down` + `handle_up`:
  1. IPC `Sync` (`:282-297`)
  2. IPC `Resources` (`:323-325`)
  3. `PostLoginState` with the tunnel running (`:521-522`)
  4. early transport resync `run_transport_recovery` (`:2270-2273`)
  5. background tick `sync_and_restart_if_changed` (`:2329-2334`)
- **Data plane is immutable once spawned.** `handle_up` (`daemon.rs:559-769`) moves the TUN device,
  the SPIFFE-filtered entries, and an immutable `Arc<HashMap<(ip,port), Option<Vec<Arc<ClientTransport>>>>>`
  by value into `net_stack::run` (`net_stack.rs:210-215`). There is no inbound update channel.
  smoltcp listeners and iface addresses are built once (`net_stack.rs:254-284`).
- **`handle_down`** (`daemon.rs:771-790`) aborts the net_stack task, which drops the smoltcp
  `Interface`, `SocketSet`, and every `ActiveRelay`. `TunManager::cleanup` (`tun.rs:149-155`) flushes
  nft/ip-rule/table 105 and deletes the `zecurity0` link. Every flow dies.
- **Routes are all-or-nothing.** `configure_allowed_flows` starts with `cleanup_policy_routes()`
  (`tun.rs:63`). It is not a delta API.
- **The flow → connector relationship is lost.** `relay_tcp_to_quic` keeps only the winning stream
  (`net_stack.rs:480-555`). `ActiveRelay` has no connector field (`:199-206`). `ClientTransport`
  has no `connector_id` (only the optional `RelayContext` does, `transport.rs:44-57`). There is no
  per-flow close handle.
- **QUIC connections already outlive the map.** A flow's task holds its own
  `Arc<ClientTransport>` → `Arc<TunnelPool>`, which owns its endpoint and connection cache
  (`tunnel_pool.rs:320-372`). `tunnel_pool.rs` needs no change.
- **TUN address/CIDR never changes at runtime.** `100.64.0.1/32` is hardcoded (`tun.rs:35`,
  `net_stack.rs:272`), so there is no "TUN address change" trigger today.

## Goal

A long-lived TCP flow through the tunnel survives connector cert renewals and any snapshot change that
doesn't remove its route or its access.

## Design (proposal; confirm before implementing)

Fix 01 is primarily a **client / data-plane lifecycle** problem. It is delivered in two stages.
Phase 1 must be implemented **and live-validated** before Phase 2 starts. Neither stage is
implemented yet. *(Original text. 2026-10-10: see the table below.)*

Full scope, files, tests, and acceptance for each phase live in this folder (`fix 1/`):

| Phase | Doc | Status |
|---|---|---|
| 1: AppliedConfig / Restart Decision | [[Phase1-AppliedConfig-Restart-Decision]] | `done`: decision logic accepted (`6d26479`, live 2026-10-01/02). G9 / AT-G.6 PASS (2026-10-06 evidence) and strict U5 PASS (2026-10-10). Post-slice Fix 01 regression validation outstanding |
| 2: Full Hot-Apply | [[Phase2-Full-Hot-Apply]] | **PARTIAL** (2026-10-10). Delivered by slices: 2-A, Fix 05 5-A / 5-B / 5-C, zero-resource, F-3, with live records. Outstanding: connector ownership + per-connector selective close (gated by Q3), live U5/G9 after the slices |

*Before 2026-10-10 the rows read: Phase 1 "planned, not implemented"; Phase 2 "NOT IMPLEMENTED;
future stage after Phase 1 validation and the Q3 decision".*

1. **Phase 1: AppliedConfig / Restart Decision** (first stage; `daemon.rs`, `runtime.rs`,
   `daemon_tests.rs`). Record the effective config the running tunnel was built from
   (`AppliedConfig`). Compare each new snapshot's effective config against it, ignoring
   version/`GeneratedAt` and other non-effective metadata. With no effective change, don't call
   `handle_down()`/`handle_up()`. Any real change keeps today's full restart. All five restart callers
   go through one decision point.
   Phase 1 is intentionally not a full hot-apply implementation. It only prevents unnecessary
   full tunnel restarts when the effective configuration has not changed. It must not silently change
   controller behaviour or revocation semantics.

2. **Phase 2: Full Hot-Apply** (larger architectural change; `daemon.rs`, `runtime.rs`,
   `net_stack.rs`, `tun.rs`, `transport.rs`). Daemon → `net_stack` update channel, incremental routes,
   transport swap for new flows, flow → connector tracking, and closing only dependent flows, with a
   full-restart fallback. It must **not** be implemented as part of Phase 1.

Sequence:

```text
Phase 1 implementation
        ↓
Phase 1 tests
        ↓
Live U5/G9
        ↓
Observe vN+1 behavior
        ↓
Resolve Q3
        ↓
Phase 2 design/implementation
        ↓
Phase 2 tests
        ↓
Live U5/G9
```

3. **Controller: out of scope for Fix 01, NOT content-hash dedupe.**
   - ~~Compare a content hash of the compiled snapshot before bumping.~~ This would not help: v10→v11 and
     v11→v12 each really differ in content (the connector is removed, then restored), so a hash
     still bumps both times. To make it effective you would have to suppress a real disconnect.
   - The only controller lever that actually works is a short grace window before the stream-close
     path marks the connector `disconnected` (the watcher and `Goodbye` stay definitive). That
     trades against ADR-017 fail-fast. It is a **separate design decision** and must not be changed
     silently or as part of Fix 01.
   - Keep ADR-017 fail-fast for real disconnects (watcher path, Phase 2 / D1–D2).

### ⚠ Unresolved design decision / risk: the intermediate vN+1 snapshot (NOT solved)

The client can observe and apply the intermediate **vN+1** (connector absent).
`run_transport_recovery` polls at 2 s, 4 s, 8 s … (`daemon.rs:2261-2289`), far more often than the
60 s background tick. It fires whenever a flow hits a transport error during the disconnect window.

- If vN+1 is applied immediately:
  - Phase 1: vN+1 is a real effective change → full restart, and then vN+2 → a second restart.
  - Phase 2 with a naive "connector left → close its flows": the long-lived flow is closed.
- ACL and transport are fetched sequentially with independent versions (`daemon.rs:2308`, `:2321`),
  so the client can briefly hold mixed generations.

Proposed direction (to be decided explicitly and tested before Phase 2 is considered complete):

- For **existing** flows, stay tolerant of *temporary* connector removal during the renewal
  transition: defer closing for a bounded grace period, and/or close only when the flow's own QUIC
  connection actually fails.
- Do **not** weaken permanent connector failure or revocation semantics. A resource or access
  removal must still close its flows.
- **New** flows fail closed or follow the available valid connector policy (today:
  `Some(None)` → fail closed, `net_stack.rs:360-364`).
- The exact grace/defer mechanism and its bound are **open**. No grace period is approved.
- Whatever is chosen must preserve fail-closed and revocation semantics.

## Decisions

- **Q1: Where should Fix 01 be implemented? → Decision: client-only.** Fix 01 changes the
  client/data-plane lifecycle. No controller notifier changes, no controller content-hash dedupe, and no
  controller grace-window change. Any controller grace window belongs to the separate ADR-017
  decision.
- **Q2: What happens when a connector genuinely disappears? → Decision: eventually close only the
  flows that depend on that connector/resource.** Unaffected flows stay alive. This belongs to
  **Phase 2**. The current data plane has neither flow → connector tracking nor a hot-update
  mechanism, so this behaviour does **not** exist yet.
  *2026-10-10: hot-update now exists (2-A, 5-C), and resource-dependent flows are closed (5-C,
  F-3). Flow → connector tracking and the per-connector close still do **not** exist
  ([[Phase2-Full-Hot-Apply]] § Remaining work).*
- **Q3: How should the client handle vN+1 during connector renewal? → NOT FINALIZED YET.**
  Proposed direction: tolerate/defer transient connector removal for existing flows during the
  renewal transition, with a bounded grace/defer mechanism if the evidence supports it. Permanent
  removal must still be handled correctly, revocation must not be weakened, and new flows must not use
  an unavailable connector. The exact grace period, timeout, and mechanism are **not** decided. The
  Phase 1 live run supplies the evidence: whether the client actually observes/applies vN+1 during
  renewal, and what that does to the long-lived flow. **Q3 must be resolved before Phase 2 is
  implemented.**
  *2026-10-10: still open. Slices that didn't depend on it have shipped. Q3 now gates only the
  remaining connector-flow work. The lifecycle policy is under discussion and must keep
  distinct:*
  - *transient control-plane interruption (e.g. renewal);*
  - *a temporarily unreachable configured connector;*
  - *intentional removal from configuration;*
  - *certificate or identity revocation.*
  *No grace period or close policy is approved.*
  *2026-10-10: established-flow recovery and connector failover is **deferred** out of Sprint 20 /
  Fix 01 scope and **not implemented**. It is tracked as
  [[PENDING-18-Established-Connection-Recovery-Connector-Failover]]. Q3 is **not** resolved by this
  deferral: the connector-loss classification above is still open.*
- **Q4: Ship and validate Phase 1 on its own? → Decision: yes.** Phase 1 is implemented, tested,
  and run through U5/G9 before Phase 2 starts (sequence above).

## Files (expected)

Details are in the phase docs. *(Original plan. 2026-10-10: all listed files now exist and were changed by the
slices. `transport.rs` did not get `connector_id`, and `net_stack.rs` has no per-flow connector
tracking. Fix 05-B also changed `tunnel_pool.rs`. Per-slice file lists are in
[[Phase2-Full-Hot-Apply]] § Files.)*

| File | Phase |
|---|---|
| `client/src/daemon.rs` | 1, 2 |
| `client/src/runtime.rs` | 1, 2 |
| `client/src/daemon_tests.rs` (+ `daemon.rs` tests) | 1 |
| `client/src/net_stack.rs` | 2 |
| `client/src/tun.rs` | 2 |
| `client/src/transport.rs` | 2 |
| net-stack / transport tests + test harness | 2 |

**Outside the Fix 01 implementation scope:**
- `client/src/tunnel_pool.rs`: QUIC connections already outlive the transport map
  (`tunnel_pool.rs:320-372`). Only add it if repo evidence later proves it's needed.
- `relay_pool.rs`, `crl.rs`, `proto/`.
- Controller notifier files (`controller/internal/policy/notifier.go`,
  `controller/internal/transport/notifier.go`) and all other controller code. Content-hash dedupe was
  dropped because it wouldn't dedupe real connector add/remove.

## Tests

- Phase 1: see [[Phase1-AppliedConfig-Restart-Decision#Tests]].
- Phase 2: see [[Phase2-Full-Hot-Apply#Tests]].

Unit tests do not prove the fix. The live U5/G9 run after each phase is the final validation.

## Acceptance Criteria

Target: a long-lived TCP connection must survive connector certificate renewal and snapshot revision
changes when the effective access/transport configuration required by that connection has not
actually changed.

### Phase 1: **Decision logic accepted; G9 / AT-G.6 and strict U5 PASS; post-slice Fix 01 regression outstanding** (reconciled 2026-10-10; mirrors [[Phase1-AppliedConfig-Restart-Decision]] § Acceptance)

*Previously headed "Not yet satisfied". Historical: U5 long hold and G9 FAIL on 2026-09-26 (Finding 15,
re-verified 2026-09-28); these entries are kept. G9 / AT-G.6 was re-graded 2026-10-10. Details and limitations:
[[Phase1-AppliedConfig-Restart-Decision]] § G9 / AT-G.6 re-grade. Strict U5 passed in a later live run on
2026-10-10 ([[U5-Connector-Route-Long-Hold-Live-Acceptance-2026-10-10]]).*

- [x] Phase 1 unit tests pass: 110 passed, +20 (Phase 1 doc § Acceptance).
- [x] **U5 (strict): PASS on 2026-10-10** ([[U5-Connector-Route-Long-Hold-Live-Acceptance-2026-10-10]]).
      - Unprotected, connector-served resource `s20f3-unrel` `261a549d` at 192.168.1.75:51713, via
        connector `s20l6-conn-75` `7523dc08`.
      - One TCP connection (port 39492) held for 2,401 s: 1,194 × 200, no `SOCKET CHANGED`, 8 renewals.
      - No client restart; ACL stayed at v16.
      **Not validated:** Fix 01 hot-apply (no version change during the hold), and established-flow
      recovery after connector loss (deferred, [[PENDING-18-Established-Connection-Recovery-Connector-Failover]]).
      **History kept:** U5 long hold FAILED on 2026-09-26 (Finding 15). The 2026-10-06 hold used a
      protected resource and did not cover this scope.
- [x] **G9 / AT-G.6 (original criterion): PASS, with limitations.** 2026-10-06 12:10:16–13:41:33 IST
      (91 min 17 s): one socket (port 37546), 2,717 × 200, 17 renewals of connector inkyank `5aa40833`,
      PID 2379 / ifindex 14 stable ([[Phase5A-Live-Acceptance-2026-10-06]]).
      Limitations:
      - shield-routed target;
      - connector ownership inferred;
      - the client journal is no longer available.
      Since `6362d1f`, renewals no longer bump the version, so this proves renewal survival only. A
      separate post-slice Fix 01 regression criterion needs agreement and remains **outstanding**.
- [x] A renewal or effective no-op doesn't cause an unnecessary full tunnel restart (live 2026-10-01
      hold #2; 2026-10-02 12:41:17; Phase 1 build).
- [ ] A long-lived TCP connection stays alive when the effective configuration is unchanged. Not
      revalidated for the current implementation. The historical 2026-10-02 closing hold had 0 FAIL,
      but only one forced bump exercised the decision.
- [x] No `snapshot changed, restarting VPN` for renewal-only effective no-op changes (0
      renewal-caused versions across 38 renewals).
- [x] Record whether vN+1 was actually observed/applied (2026-10-01 hold #1; Q3 input). Q3 remains open.

### Phase 2: **Partially satisfied** (reconciled 2026-10-10; mirrors [[Phase2-Full-Hot-Apply]] § Acceptance)

- [x] Existing unaffected long-lived flows survive applicable configuration changes (2-A; 5-C).
- [x] Removing a **resource** closes only the flows that depend on it (5-C L6 re-run 2026-10-10, L9; F-3).
- [ ] Removing a **connector** closes only the flows that depend on it. Not implemented; gated by Q3.
- [x] New flows use the updated transport configuration (2-A).
- [x] Incremental route changes work (5-C).
- [x] A failed hot-apply falls back safely to a full restart (2-A `Fallback`, 5-C `MutationFailed`; live zero-resource Z3).
- [ ] U5/G9 pass after Phase 2. **Not run.**

## Out of scope

- Finding 28 (server FIN not forwarded): separate issue in `net_stack.rs`. It isn't a failed
  run-sheet row, but it affects browsers.
- Findings 33/34 (revocation doesn't cut established paths). Hot-apply must still not weaken
  today's behaviour: removed resources/access must close their flows.
- Any controller change: notifier changes, content-hash dedupe (ineffective), and the stream-close
  grace window (separate ADR-017 decision).

## Recommended implementation order

1. Implement Phase 1 `AppliedConfig` and the effective-delta decision.
2. Add Phase 1 tests.
3. Route the restart callers through the centralized decision point.
4. Run live U5/G9.
5. Observe whether/how vN+1 is seen during renewal.
6. Resolve Q3.
7. Only then begin Phase 2.
8. Implement the Phase 2 hot-apply architecture.
9. Add Phase 2 tests.
10. Run live U5/G9 again.
