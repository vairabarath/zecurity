---
type: fix-investigation
sprint: 20
fix: 5
title: Connector-state delivery latency — why the client learns "no connector reachable" ~43 s late
status: investigated — fix implemented in Fix05-Empty-Network-Transport-Delivery (pending live)
date: 2026-10-06
component: client + controller (transport plane)
related:
  - Phase5A-Live-Acceptance-2026-10-06 (Finding 1)
  - 5-B stale QUIC-pool handling (separate)
---

# Connector-state delivery latency — investigation

> Source finding: [[Phase5A-Live-Acceptance-2026-10-06]] §Findings #1.
> Scope: investigation only. Fix 5-A is frozen, Phase 2-A is frozen, 5-B not started. No production
> code was changed and no new live test was run. All evidence comes from the 2026-10-06 run logs
> (`~/s20-run/logs/controller.log`, `journalctl -u zecurity-client`, `logs/f5-fastprobe.log`) and the
> code at HEAD `f1ce4df` (+ uncommitted 5-A `net_stack.rs`, which this path does not touch).

## A. Executive conclusion

The controller learned of the last connector's loss **at once** (13:48:09) and bumped both versions
right then. The client even fetched the new transport snapshot **5 s later** (13:48:14). It still kept
the dead connector for another **43 s** because of a client-side fallback:

1. When a remote network has **zero active connectors**, the controller's transport snapshot leaves
   that remote network **out** of the snapshot entirely (`controller/internal/transport/compiler.go:34-71`,
   query `store.go:44-62` selects only `c.status = 'active'`).
2. The client reads "remote network absent from the transport snapshot" as "transport plane doesn't
   cover it" and **falls back to the connector list inside the cached ACL snapshot**
   (`client/src/daemon.rs:3924-3942` `resolve_entry_coords`, `None => rn_by_id…`; pinned by the test
   `resolve_falls_back_to_acl_when_transport_lacks_rn`, `daemon_tests.rs:308`).
3. The early relay-failure resync fetches **only the transport plane**
   (`daemon.rs:3358-3392` `run_transport_recovery` → `fetch_and_store_transport`). The cached ACL
   (v14) still listed manoj, so the recomputed effective config equalled the applied one: `effective
   config unchanged, keeping tunnel`. The recovery loop then **stops**, because the version did change
   (`Ok(true)` → `break`, `daemon.rs:3373-3378`).
4. Nothing else re-fetches the ACL until the fixed **60 s ACL tick** (`ACL_REFRESH_TTL_SECS = 60`,
   `daemon.rs:38`, `run_acl_sync_scheduler` `daemon.rs:3076`). The tick fell at 13:48:57. ACL v15 no
   longer listed manoj, the fallback resolved to nothing, and the client hot-applied `reachable=0`.

**So the 43 s is not deterministic.** It is the time from the resync to the next 60 s tick (0–60 s,
depending on the tick's phase). The root cause is an ambiguity in the transport snapshot ("RN with no
connectors" and "RN not covered" look the same) combined with the client's ACL fallback. It is a
**real product bug** in the transport plane / client resolution path. It predates Fix 5 and Phase 2-A:
the fallback came in with Sprint 13 Phase C (`73ab176`), and transport-only early resync with Track B.

## B. Timeline (L3 row, 2026-10-06; IST, client lines are journal time)

| Time | Component | Event | Source |
|---|---|---|---|
| 13:47:50 | controller | inkyank `stream error … Canceled` → `connector … disconnected` → `acl push … version=14 entries=1 connectors=1` (manoj only). Transport version bumped in the same defer (`control_stream.go:425-437`) | controller.log |
| 13:47:57.97 | client | 60 s tick: `ACL snapshot synced version=14` → `transport snapshot stored version=5` → hot-apply `reachable=1` (manoj) | journal |
| **13:48:09** | controller | manoj `stream error … Canceled` → `connector … disconnected`. `retireStream` marks it `disconnected`; `NotifyPolicyChange` (ACL → v15) and `NotifyTopologyChange` (transport → v6) fire at once. No `acl push` line, because no connector is left to push to | controller.log, `control_stream.go:417-438` |
| 13:48:09–14 | client | new connections already in flight: the first `tunnel handshake timed out after 5s` lands at 13:48:14.77 | journal |
| **13:48:14.77** | client | `QUIC relay ended … no connector accepted tunnel` → `relay failure signalled — early transport resync` | journal, `net_stack.rs:952-958` |
| 13:48:14.78 | client | `transport snapshot stored version=6` (RN `palace` **absent**, since zero active connectors) | journal |
| 13:48:14.78 | client | `early transport resync: version changed, restarting tunnel` → **`effective config unchanged, keeping tunnel`** (the ACL v14 fallback still gives manoj). Recovery loop ends | journal, `daemon.rs:3373-3378`, `1073-1076` |
| 13:48:15–13:48:42 | client | 5 × 5.0 s + 1 × 2.0 s probe failures: dials to manoj from the stale map + stale QUIC pool (→ 5-B) | `f5-fastprobe.log` |
| 13:48:42–13:48:57 | client | `failed to reach connector (transport), trying next` → fast reset (< 1 s) | journal |
| 13:48:45.31 | client | 2nd `relay failure signalled` (30 s cooldown since 13:48:14 had passed). `fetch_and_store_transport` returns up-to-date (v6) → `Ok(false)` → back-off 2/4/8/16 s. The ACL is never fetched on this path | journal, `daemon.rs:3380-3384` |
| **13:48:57.97** | client | 60 s tick: `ACL snapshot synced version=15` → effective config now empty → `transport-only change, hot-applying` → **`hot-applied successfully resources=1 reachable=0`** | journal |
| 13:48:58 → | client | `connector offline — failing closed` (L3 steady state, ≤ 8 ms resets) | journal, probes |

The delay is 13:48:57.97 − 13:48:14.78 = **43.2 s** after the client's own resync, or **48.9 s** after
the controller saw the disconnect.

### Same mechanism elsewhere in the run (checks determinism)

| Event | Controller saw it | Client applied it | Lag | Why |
|---|---|---|---|---|
| L3: last connector gone | 13:48:09 | 13:48:57.97 | 48.9 s | this bug: transport resync at +5 s was masked by the ACL fallback; waited for the tick |
| L3: connectors back | 13:49:28–29 | 13:49:57.97 | ~29 s | no client trigger exists for "connector returned"; waited for the tick (by design) |
| L4: inkyank gone (manoj stays) | 13:41:23 | 13:41:53.30 (resync v3 hot-applied) | 30 s | the RN stayed in the transport snapshot (manoj), so the transport-only resync **worked**. Resync only fired after a whole relay failed (the hold), because single handshake timeouts that fell through to manoj don't signal (`net_stack.rs:952-958`) |
| L4: inkyank back | 13:43:06 | 13:43:57.98 | 52 s | tick only (by design) |

The client's ACL tick ran at `hh:mm:57.97` every minute all afternoon (one `interval` since the 12:09:33
stack start + login). Every lag above lines up with that phase, not with a fixed ~43 s.

## C. Component responsible

**Proven:** the *zero-connector* lag comes from the client's transport→ACL fallback in
`resolve_entry_coords`, triggered by the controller's transport compiler leaving out RNs with no
active connectors. The ACL-only update that finally cleared it was delivered by the 60 s client poll.

**Not responsible:**
- **Controller detection:** the stream-close path fired in < 1 s; the disconnect watcher was not
  involved.
- **Controller notification:** both notifiers bumped synchronously in the same defer.
- **Connector behaviour:** not a factor.
- **Shield:** it only affects the *recovery* side (back-off), see the 5-A record, Finding 2.
- **Test instrumentation:** the probes only exposed the lag.

## D. Evidence

- Client journal 13:47:57–13:48:58: `transport snapshot stored version=6` immediately followed by
  `effective config unchanged, keeping tunnel`; `reachable=0` only after `ACL snapshot synced version=15`.
- Controller log: `disconnected` lines at 13:47:50 / 13:48:09. `acl push version=14 connectors=1` at 13:47:50
  shows the ACL snapshot the client held (v14) still contained manoj.
- Code:
  - `transport/store.go:44-62`: active connectors only.
  - `transport/compiler.go:34-71`: RNs are created only from connector rows, so zero rows means no RN.
  - `client daemon.rs:3924-3942`: an RN missing from transport falls back to the ACL's `remote_networks`.
  - `daemon.rs:3358-3392`: the recovery path fetches transport only and breaks on `Ok(true)`.
  - `daemon.rs:1052-1076`: the NoChange branch.
  - `daemon.rs:38`, `3076-3101`: the 60 s tick.
  - `transport/notifier.go:15-20`: "There is no proactive push … the client's next poll observes the new
    version".
- Unit test `resolve_falls_back_to_acl_when_transport_lacks_rn` (`daemon_tests.rs:308`) shows the
  fallback is intentional (transitional, PENDING-03 Option A).

## E. Unknowns

| Item | Status | How to close (without a destructive test) |
|---|---|---|
| Exact contents of client ACL v14 / transport v6 at 13:48:14 | **inferred** (high confidence). The controller pushed v14 with `connectors=1` and the compiler cannot emit an RN with zero rows, but the client doesn't log snapshot bodies | Unit test reproducing transport-without-RN + ACL-with-connector → NoChange (section H) |
| Controller-side request timestamps for the client's fetches | not logged (the controller doesn't log `GetTransportSnapshot`/`GetACLSnapshot` calls) | Client journal times are enough for the timeline |
| Why probes switched from 5 s timeouts to fast transport failures at 13:48:42 | inferred: the stale QUIC connection to manoj died (idle timeout) and new dials got refused quickly | **5-B scope**; not needed for this finding |
| Whether the ACL fallback is still needed by any deployed path | open design question | Check whether any controller still serves an ACL-only topology (pre-Track-B) |

## F. Classification

**Real product bug (latency, fail-closed preserved).** While it lasts, the client keeps dialling a dead
connector, so new connections pay timeouts instead of an immediate reset. It never fails *open*.
How bad it is depends on the tick phase (0–60 s). The separate "connector came back" latency (≤ 60 s
by polling) is **expected behaviour** under the documented no-push design; a push channel is a
possible future improvement, not a bug.

## G. Recommended fix scope (not implemented)

Preferred, smallest, and keeps the Track B contract:

1. **Controller: make "no connectors" explicit in the transport snapshot.** Emit a
   `TransportRemoteNetwork{remote_network_id, connectors: []}` for every remote network in the
   workspace, including those with zero active connectors (e.g. a `remote_networks LEFT JOIN connectors`
   query in `transport/store.go`, and RN seeding in `compiler.go`). The client then resolves through
   the transport plane and gets an empty list, so `reachable=0` is hot-applied at the first early
   resync (~5 s after the disconnect here instead of 48.9 s). **No client change is needed**, because
   `resolve_entry_coords` already handles a present-but-empty RN, and older clients behave the same way.
   Bump nothing extra: the existing disconnect notify already bumps the version.

Optional defence in depth (client, separate decision):

2. In `run_transport_recovery`, when the transport version changed but the decision is `NoChange`,
   also run one ACL sync (or call `sync_and_restart_if_changed`) before breaking. That covers stale
   ACL-plane fallback coords against any controller that still omits RNs.
3. Longer-term: retire the transport→ACL fallback once every supported controller emits complete
   transport snapshots (PENDING-03 clean-up).

Out of scope: a push channel to clients, the shield back-off (5-A Finding 2), and stale QUIC-pool
reuse (5-B).

## H. Tests and live acceptance for the proposed fix

**Unit/integration:**
- Controller `compiler_integration_test.go`: a workspace with RN `A` (1 active connector) and RN `B`
  (0 active, 1 disconnected) produces a snapshot with both RNs, and `B.connectors` is empty. Also: RN
  with all connectors disconnected after a `NotifyTopologyChange`, so the version increments and `B`
  is present and empty.
- Client `daemon_tests.rs`: `resolve_entry_coords` with the RN present in transport with zero connectors
  and the ACL RN listing a connector returns **empty** (no fallback). The existing fallback test stays
  green (RN absent → fallback).
- Client: a classifier test where applied = {manoj} and the new transport has an empty RN gives
  `TransportOnly` (not `NoChange`).
- Regression: the existing Track B and Phase 2-A classifier tests are unchanged.

**Live acceptance (same lab, `f5-fastprobe` 1 s):**
- D1: stop the last connector of the shield's RN. The client logs `transport snapshot stored` and then
  `hot-applied … reachable=0` **within one early resync** (≤ 10 s after the controller's `disconnected`),
  **independent of the 60 s tick phase**. Repeat 3× at different tick offsets (e.g. +5 s, +25 s, +45 s
  after `hh:mm:57`).
- D2: after D1, every probe is `000` in < 1 s with `connector offline — failing closed`. Count slow
  (≥ 1 s) probes between the disconnect and `reachable=0`. These belong to 5-B; record them, but they
  no longer depend on the tick.
- D3 (no regression): L4-style single-connector loss/return still hot-applies with 0 `restarting VPN`
  and the ifindex unchanged. L1-style 10-min sampler shows 0 × 000.
- D4: connectors return. Recovery is still ≤ 60 s (tick); unchanged, documented.

## I. Relationship to 5-B (stale QUIC-pool handling)

They are separate defects that compound:
- **This finding** decides *when the client's map stops listing the dead connector*. It happened 43 s
  after the resync, because the zero-connector transport snapshot was masked by the ACL fallback.
- **5-B** decides *what a dial to a listed-but-dead connector costs*. That was 5 s per new connection on
  a stale pooled QUIC connection (13:48:15–13:48:39 here, and in L4 13:41:29–50).

Fixing this first shrinks the window in which 5-B hurts after a total connector loss, from up to 60 s
down to about one resync. 5-B is still needed for partial loss (L4 shows 4 × 5 s with no delivery lag,
because the remaining connector kept the RN in the snapshot) and for the first failing dial that
triggers the resync. Do this fix before 5-B, so 5-B's live acceptance isn't skewed by tick-phase noise.
