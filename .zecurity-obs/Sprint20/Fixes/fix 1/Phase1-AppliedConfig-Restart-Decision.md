---
type: fix-phase
sprint: 20
fix: 1
phase: 1
title: AppliedConfig / Restart Decision
status: done
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
> Status: **done**: implemented (`6d26479`) and live-validated 2026-10-01/02 (see end of file). Phase 2 may begin;
> read Finding P1-A first.

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

## Acceptance — **Phase 1 decision logic accepted** (live validation 2026-10-01/02). Original G9 / AT-G.6 re-graded **PASS** from the 2026-10-06 hold (see § G9 / AT-G.6 re-grade). Strict U5 **PASS** from the 2026-10-10 connector-route hold. The post-slice Fix 01 regression validation remains outstanding.

> Reconciled 2026-10-10. The heading previously read: *Not yet satisfied (code + unit tests done
> 2026-09-30; live run pending)*. Evidence is cited by section of this document. The historical U5
> long-hold and G9 results on the run sheet remain **FAIL** (hold #2 on 2026-09-26, Finding 15;
> re-verified 2026-09-28). G9 / AT-G.6 was re-graded on 2026-10-10 from existing evidence; see
> § G9 / AT-G.6 re-grade. Strict U5 passed in a new live run on 2026-10-10 ([[U5-Connector-Route-Long-Hold-Live-Acceptance-2026-10-10]]);
> the 2026-09-26 U5 failure is kept as history.

- [x] Phase 1 unit tests pass (`cd client && cargo build && cargo test`): 110 passed (baseline 90;
      +9 `effective_config` in `daemon_tests.rs`, +11 `restart_decision_tests` in `daemon.rs`).
- [x] **Live: strict U5 PASS (2026-10-10).** The U5 row reads *"tunnel open to a connector-served TCP resource
      (not shield-routed RDE)"*. Evidence: [[U5-Connector-Route-Long-Hold-Live-Acceptance-2026-10-10]].
      - Resource `s20f3-unrel` `261a549d`, 192.168.1.75:51713. It was `unprotected`, so it used the
        connector route (no shield), which satisfies the strict routing requirement.
      - Connector `s20l6-conn-75` `7523dc08`.
      - Hold: 2,401 s on one TCP connection (local port 39492), 1,194 × HTTP 200, no `SOCKET CHANGED`.
      - 8 connector renewals during the hold.
      - No client restart reported. ACL stayed at v16 throughout.
      **Does not validate:**
      - Fix 01 hot-apply behaviour: no effective configuration version changed during the hold.
      - Established-flow recovery after connector loss: still deferred as
        [[PENDING-18-Established-Connection-Recovery-Connector-Failover]].
      **History kept:** the 2026-09-26 U5 result, "tunnel open PASS; long-lived hold FAIL" (Finding 15),
      stands as the historical record. The 2026-10-06 hold (§ G9 / AT-G.6 re-grade) used a protected
      (shield-routed) resource and did not demonstrate this scope; the later 2026-10-10 run did, and passed.
- [x] **Live: G9 / AT-G.6 (original criterion) PASS, with limitations.** Evidence is the 2026-10-06
      hold: 91 min 17 s on one socket, through 17 renewals of connector inkyank (§ G9 / AT-G.6 re-grade).
      Historical result: FAIL on 2026-09-26 (Finding 15), before `6362d1f`. **This is not Fix 01
      regression proof.** Since `6362d1f`, a renewal no longer bumps the ACL/transport version, so
      renewal survival does not exercise the restart/hot-apply decision. A separate post-slice Fix 01
      regression criterion still needs agreement and remains **OUTSTANDING**.
- [x] A renewal or effective no-op doesn't cause an unnecessary full tunnel restart. Evidence:
      § Live evidence so far, 2026-10-01 hold #2 (`effective config unchanged, keeping tunnel` on
      each version bump across 2 renewals); § Closing hold, 12:41:17 (forced version bump →
      `keeping tunnel`, hold unaffected). Validated on the Phase 1 build only.
- [ ] A long-lived TCP connection stays alive when the effective configuration is unchanged
      (0 resets caused by snapshot synchronization). **Not revalidated for the current
      implementation.** Historical result: § Closing hold, 2026-10-02 (746 × 200 over 1500 s,
      0 FAIL). Limitation: during that hold renewals no longer changed the version, so only the
      single forced bump at 12:41:17 exercised the Phase 1 decision. It is not a formal U5/G9 pass.
- [x] No `snapshot changed, restarting VPN` for renewal-only effective no-op changes. Evidence:
      § Live evidence so far → **Q3 input** (0 renewal-caused ACL/transport versions across 38
      renewals), and § Closing hold (5 renewals, 0 renewal-caused versions).
- [x] Record whether vN+1 was actually observed/applied during renewal, and what it did to the
      long-lived flow. Recorded: § Live evidence so far, 2026-10-01 hold #1 (sync landed in the
      renewal's connector-absent gap → real change → restart, reset at 11:22:35) and **Q3 input**.
      Q3 itself remains open.

## G9 / AT-G.6 re-grade from existing evidence (2026-10-10)

**Original criteria.**
- G9 (run sheet): *"the long-lived tunnel from U5 is still open after several connector renewals"*.
- AT-G.6 (`Acceptance-Test-Plan.md`, Phase G): *"Established tunnels during swap → not dropped"*.
- The later "≥ 3 renewals at 15 m TTL" refinement is from this fix's docs.

**Result: PASS for the original G9 / AT-G.6 criterion, with the limitations below.** No new test was run.

**Evidence.** Source record: [[Phase5A-Live-Acceptance-2026-10-06]] (`.zecurity-obs/Sprint20/Fixes/fix 5/`; L1, L4,
"long-lived flow survives the run"). Artifacts are in `~/s20-run/logs/` (lab host, not in the repo).

| Item | Value | Source |
|---|---|---|
| Window | 2026-10-06 12:10:16 – 13:41:33 IST, **91 min 17 s** | `f5-hold.log` (`START local_port=37546` … `req=2718 FAIL TimeoutError`) |
| Client | build `e2e36c45` (Phase 1 + Fix 01 Phase 2-A + Fix 05 5-A), PID 2379, `zecurity0` ifindex 14 | 5-A record, lab table; `f5-tunsamp.log` |
| Connection | one TCP connection, local port 37546, to protected resource `s20f5-nika-web` 192.168.1.39:51711 | `f5-hold.log`; 5-A record |
| Hold method | `~/s20-run/f5_hold.py`: one `http.client` connection, keep-alive GET every 2 s. It checks the local port on every request and exits on `SOCKET CHANGED` or any error, with no reconnect | script source |
| Traffic | **2,717 × HTTP 200** (req 1–2717). The script prints any non-200; none was printed. No `SOCKET CHANGED` | `f5-hold.log` |
| Renewals | **17 `ReEnroll sent`** for connector inkyank `5aa40833`, 12:14:31 – 13:37:32 IST. Each is followed within about 1 s by the renewed stream `connected` and the old stream `superseded stream closed (newer stream is authoritative)`; the reported cert expiry advances each cycle | `controller.log` (2026/10/06) |
| Config versions | the controller logged `ACL already current … version=10` throughout; no `acl push` until the deliberate stop at 13:41:23 | `controller.log` |
| Tunnel stability | PID 2379 / ifindex 14 at 12:10:13 and 13:39:50 (`f5-tunsamp.log`, change-only); 154 `f5-sampler.log` samples 12:10–13:41, all `pid=2379`. L1 recorded 0 `restarting VPN` for 12:10–13:10 | samplers; 5-A record L1 |
| End of hold | inkyank deliberately stopped (≈13:41:18; controller `disconnected` 13:41:23). The hold timed out at 13:41:33 (`TimeoutError`, 10 s socket timeout). This was the **intended end of the test** (5-A row L4), not a renewal-triggered restart. Connector manoj stayed up | `f5-events.log` `L4-STOP-SENT` / `L4-END`; 5-A record L4 |

**Limitations.**
1. **Shield-routed target.** The resource was protected, so the flow ran Client → Zecurity tunnel → Connector →
   Shield → application. It is not a demonstrated unprotected connector-served TCP resource, so strict U5
   scope is not claimed.
2. **Connector ownership is inferred, not logged.** The basis:
   - the lab mapping: the shield is bound to inkyank `5aa40833` (5-A lab table; `f5-events.log` `F5-REBUILD`);
   - stopping only inkyank ended the hold while manoj stayed available.
   The per-flow client `tunnel opened` line was not retained.
3. **The 2026-10-06 client journal is no longer available** (the host journal starts 2026-10-09). The
   conclusion rests on the retained controller log, hold output, sampler output and the 5-A acceptance
   record. For 13:10–13:39 only the endpoints of the change-only tunnel sampler and the PID sampler
   cover the client; a VPN restart recreates `zecurity0` with a new ifindex.
4. **Not a Fix 01 regression test.** After `6362d1f` (make-before-break control-stream renewal), renewal
   no longer produces the `disconnected` → `connected` snapshot transitions and renewal-related version
   bumps that caused the 2026-09-26 restart. During this hold the ACL stayed at v10, so the client had
   no configuration change to decide on. The hold proves **renewal survival**. It does not prove that
   Fix 01 hot-applies meaningful configuration changes without a restart.

**Historical result kept.** G9 FAILED on 2026-09-26 (hold #2, Finding 15: reset by `snapshot changed,
restarting VPN` after a connector renewal; re-verified 2026-09-28). That run predates both `6362d1f`
and Phase 1.

**Still open.**
- ~~Strict U5~~: PASS on 2026-10-10 ([[U5-Connector-Route-Long-Hold-Live-Acceptance-2026-10-10]]; see § Acceptance).
- A separate post-slice Fix 01 regression criterion (needs agreement).
- Q3.

## Live evidence so far (2026-10-01 / 2026-10-02) — acceptance still open

> 2026-10-10: the Phase 1 **decision logic** is accepted; see the verdict at the end of
> § Live run 2026-10-02 (afternoon) ("Phase 1 verdict: correct … Phase 1 is done"). `status: done`
> refers to the decision logic only. It does not mean every Phase 1 acceptance criterion passed (see
> § Acceptance). Since then, G9 / AT-G.6 (2026-10-06 evidence) and strict U5 (2026-10-10 run) have
> passed, while the post-slice Fix 01 regression validation remains **outstanding**.

| Run | What it shows | Counts for Phase 1? |
|---|---|---|
| 2026-10-01 hold #1 (port 41098) | 589 requests, then reset at 11:22:35: the 60 s sync landed inside the renewal's connector-absent gap (vN+1), so the effective config really changed and Phase 1 restarted as designed | Expected Phase 1 limit (Q3), not a Phase 1 bug |
| 2026-10-01 hold #2 (port 56056) | 270 × 200 across 2 renewals; the journal shows `effective config unchanged, keeping tunnel` on each version bump | Yes, but only **2** renewals (G9 asks for ≥ 3) |
| 2026-10-02 fix-connector hold (port 48394) | 1194 × 200 / 40 min, 0 resets | **No.** After 11:19 every client sync failed with `session expired; re-login required` (39×), so the client made only 1 restart decision (11:03, `keeping tunnel`). The hold mostly proves the connector fix, not this phase |

**Session-expiry cause (likely, not proven):** the refresh session is one Valkey key per user
(`refresh:<user_id>`, `controller/internal/auth/valkey.go:131`), and it rotates on every use
(`refresh.go`). The admin UI (browser) and the client daemon were signed in as the same Google user, so
they share and overwrite one refresh slot. For the re-run, keep the admin UI closed, or use a
different user, while the client hold runs.

**Q3 input:** with the connector fix (`6362d1f`), a renewal no longer produces the connector-absent
vN+1 at all (0 renewal-caused ACL/transport versions across 38 renewals). So Q3's renewal case
mostly goes away. Q3 still applies to real connector disconnects and revocations.

**To close Phase 1:** one hold of ≥ 3 shield-connector renewals with client syncs succeeding:
≥ 3 `keeping tunnel`, 0 `snapshot changed, restarting VPN`, 0 resets. Then tick the boxes above and
set `status: done`.

## Live run 2026-10-02 (afternoon): closing hold + connector offline/return

### Closing hold, 12:30:43–12:55:43
- A single socket (local port 33430) served 746 × 200 for 1500 s with 0 FAIL. Client syncs ran every 60 s and never
  failed.
- Admin UI was kept closed and the client logged in fresh, so the refresh-slot collision did not occur.
- Shield connector inkyank renewed 5×, all `superseded`, 0 renewal-caused versions. **With fix-connector, renewals
  no longer change the ACL/transport version, so they no longer exercise the Phase 1 decision.** The closing
  criterion written above ("≥ 3 `keeping tunnel` across renewals") is unreachable and is replaced by the two cases below.
- I forced version bumps by restarting the controller (12:40:35, 12:44:35, 12:48:35). At 12:41:17 the client logged
  `version changed` → **`effective config unchanged, keeping tunnel`**, and the hold was unaffected. After the
  12:44 and 12:48 restarts the version came back unchanged (v3), so the client made no decision.

### Connector offline → return (user-requested), 12:57:05–13:02
inkyank (the shield's connector) was stopped at 12:58:57.850 and started at 13:00:27.862.

| Time | Controller | Shield | Client |
|---|---|---|---|
| 12:58:57 | inkyank `disconnected`, ACL **v4** (`connectors=2`) | stream error | hold socket (via inkyank) dies (FAIL at 12:59:08, expected) |
| 12:59:02–03 | | fails over to manoj in 5 s, `Control stream established` | new connections: each takes **5 s** (`tunnel handshake timed out after 5s` → next connector → `tunnel opened` via manoj) |
| 12:59:08 | `shield moved to connector c3c88536`, ACL **v5** | | |
| 12:59:17 | | | sync v5 → **`snapshot changed, restarting VPN`** (real change: connector set) |
| 12:59:23 | | | new connections immediate again (200) |
| 13:00:28 | inkyank `connected`, ACL **v6** (`connectors=3`) | stays on manoj (no fail-back) | |
| 13:01:17 | | | sync v6 → **`snapshot changed, restarting VPN`** (real change) |

**Phase 1 verdict:** correct. It kept the tunnel when the effective config was unchanged (12:41:17, plus 10-01 hold #2)
and restarted on real connector-set changes (12:59:17, 13:01:17). Both restarts on connector loss/return are the
expected Phase 1 limit and are Phase 2's (hot-apply) job. **Phase 1 is done.**

### Finding P1-A: the ~20 s window of "failed" new connections is a 5 s per-connection delay, not an outage (client)
Evidence:
- manoj logged `tunnel_opened ok` for every attempt. The client logged `tunnel handshake timed out after 5s`,
  then `tunnel opened` ~4 ms later on the next transport.
- The sampler (`curl -m 4`) gave up before the 5 s timeout, so it recorded 000 from 12:59:03 to 12:59:23.

Root cause, from the code (`client/src/net_stack.rs:480-546`, `transport.rs:112-138`, `tunnel_pool.rs:361-390`):
1. The stopped connector did not send a QUIC CONNECTION_CLOSE; the process just exited and connector/src/main.rs
   has no signal-handled endpoint close. So the client's pooled quinn connection to .75:9092 stayed
   `close_reason() == None` until quinn's idle timeout (default 30 s; only `keep_alive_interval(10s)` is set).
2. `open_authenticated_stream()` on that stale connection succeeds locally (opening a bi stream needs no round trip),
   so `mark_direct_success()` runs and the direct-path cooldown never engages.
3. The tunnel handshake then waits the full `TUNNEL_HANDSHAKE_TIMEOUT` = 5 s. The handshake timeout neither evicts the
   pooled connection nor marks the direct path failed, and because the next connector accepts, no `resync` fires.
   So **every** new connection pays 5 s until the connection idles out, or the VPN restart at 12:59:17 rebuilds the
   transport list without inkyank (that is what ended it here).

Impact: after an ungraceful connector loss, new connections take an extra 5 s each, for up to ~30 s or until the
next sync. Existing flows through the dead connector die (unavoidable).

**Relevance to Phase 2:** once hot-apply removes the VPN restart, the restart will no longer accidentally flush the
dead transport, so the only bound left is the quinn idle timeout. Phase 2 must drop transports for removed connectors
on apply, and should evict or mark-failed a pooled connection when the tunnel handshake times out. Not fixed here
(no approval for a code change); recorded for Phase 2 planning.

### Finding P1-B: ACL/transport version restarts from a low number after a controller restart (low, informational)
- v20 before the controller restart, v3 after. The client accepted v3 because it compares with `!=`
  (`daemon.rs:2941`, `daemon.rs:3058`), not `>`, so it converges.
- The connector also converged (`connector ACL already current`/pushes after reconnect).
- Theoretical edge: a client holding vN from the previous controller process could coincidentally match a new vN
  with different content and skip the update until the next bump. That is single-instance (D-02) process-local
  state. **Not observed**; not investigated further.

### Other observations
- Shield failover took 5 s (stream error, then backoff, then next peer). It did not fail back when inkyank returned,
  and the DB `shields.connector_id` followed (manoj). That is the existing design.
- At 12:48:37 there was one `posture submission failed … transport error`, during my controller restart.
