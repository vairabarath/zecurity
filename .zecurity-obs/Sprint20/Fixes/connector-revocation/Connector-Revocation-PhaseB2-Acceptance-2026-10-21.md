---
title: Connector Revocation — Phase B2 acceptance list (deadline schedule + fail-closed)
date: 2026-10-21
type: acceptance list (pre-written, design §17)
status: SIGNED OFF 2026-10-21 — implementation authorized
branch: feat/sprint20-m1-phase2
revision: 39c4257 + D17/D18 docs
design: Connector-Revocation-Implementation-Design-2026-10-14.md (rev 5)
inputs: Phase B1 (da5b12f), Phase M results, decisions D13–D18
---

# Phase B2 acceptance list

B2 makes the connector **live-safe**. Authorization requests go out every P, so a healthy connector never misses D. When it does lose authorization, it fails closed completely: existing sessions are cancelled too, not only new ones refused.

**Connector-only. The controller and protos are untouched** (D16: the full health report every P needs no protocol change). If a controller change turns out to be needed, stop and ask.

## 1. Scope

1. **Request schedule (§6.3, D14, D16).**
   - The authorization supervisor owns a schedule of period **P = `authz_timing::AUTHZ_REQUEST_PERIOD_MS`** (4.5 s), independent of streams, reconnects and renewal switches.
   - Each tick enqueues one **full health report** carrying a new seq on the current stream. `MissedTickBehavior::Delay` is used.
   - A new stream gets its request immediately: the initial report, reusing the outstanding seq and its `sent` time during a renewal switch, as today.
   - The per-session 15 s ticker keeps only its non-authorization work (shield status, state and discovery flushes, resource acks).
   - **Removes the B1-Q1 blocker:** a B2 build can run in the lab.
2. **Fail-closed steps 3–6 (§7), on entry to `Disabled`, `Revoked` or `UnknownIdentity`.**
   - **Step 3:** `SessionRegistry::cancel_every_session()` cancels every connector-routed and shield-routed session. B1's post-registration re-check covers the registration race.
   - **Step 4, shields:**
     - *Terminal:* close every shield Control stream and refuse `control()`.
     - *`Disabled` (D15):* keep the streams, carrying nothing (new tunnels refused, sessions cancelled by step 3), and close them after **`DISABLED_SHIELD_CLOSE_SECS`** (30 s) still in `Disabled`. Recovery before that cancels the close.
   - **Step 5:** `PolicyCache::clear()` in the terminal states. ACL snapshots received while closed are still applied but never admit anything (admission is keyed on state + D, B1).
   - **Step 6:** unchanged from B1 (only the supervisor transitions; terminal states park the loop).
   - **Client QUIC connections and relay attachment (D15):**
     - `Disabled`: kept, with new streams reset (B1);
     - terminal states: client QUIC connections closed (`Endpoint`/connection close), relay attachment unchanged (it carries nothing, B1 refuses its streams).
3. **Reconnect backoff (D13).**
   - The Control-stream backoff starts at `CONTROL_RECONNECT_BASE_MS` (1 s), doubles up to `CONTROL_RECONNECT_MAX_MS` (5 s), with ±`CONTROL_RECONNECT_JITTER_PCT` (20 %).
   - It **resets once a session receives an accepted ack**, not only on a clean close.
   - It replaces the 2 s → 60 s schedule.
4. **Renewal head-of-line fix (D18).** A `ReEnroll` renewal runs as an owned future polled by the session loop, like `opening` today. The loop keeps reading the current stream while the renewal runs, so `AuthorizationAck` and `ConnectorRevoked` are processed during a renewal. Ordering stays the same: one renewal at a time, make-before-break switch afterwards.
5. **Test support.**
   - tokio `test-util` dev-dependency (paused clock).
   - The B1 `ReconnectTiming` / `Attempt` seams are reused for the backoff tests.

**Out of scope:**
- the lightweight authorization request (D16(b), later);
- client, relay and shield phases C/D/E;
- recovery after revocation (O10);
- the client hot-apply capacity issue (lab workaround: one resource or `down`/`up`);
- WAN emulation (R4).

## 2. Acceptance items — unit level (paused clock where timing matters)

| # | Requirement | Evidence |
|---|---|---|
| AB2-1 (T1) | Healthy operation never reaches `Disabled`: with acks arriving within B ≤ 1 s, the state stays `Serving` over ≥ 1 simulated hour | paused-clock test over the real schedule |
| AB2-2 (T2, T27) | Missing acks → new traffic refused **exactly at D = sent[k] + H**, and every session cancelled at the expiry instant (assert the instant), under transport failure and under unmarked `Unavailable` | paused clock |
| AB2-3 (§6.3) | The schedule sends every P regardless of stream churn. A stall catches up with `Delay`, never as a burst. A send never moves D. A late catch-up send carries its true send time | paused clock |
| AB2-4 (T9) | On `Disabled`/terminal, every connector-routed and shield-routed session ends (`cancel_every_session`). New streams are refused on TLS, QUIC and relay paths (B1). The client sees a transport failure, not `ok:false` | `handle_stream` harness, as `acl_diff_session_tests` |
| AB2-5 (D15, T13) | Terminal: shield streams closed at once and `control()` refused. `Disabled`: shield streams stay open carrying nothing, are closed at exactly `DISABLED_SHIELD_CLOSE_SECS`, and the close is cancelled if serving resumes first | paused clock + `ShieldRegistry` |
| AB2-6 (step 5) | Terminal: `PolicyCache` cleared. An ACL snapshot received while closed is applied but admits nothing | unit |
| AB2-7 (D15) | Client QUIC connections are kept in `Disabled` (new streams reset) and closed in terminal states | QUIC listener test (as `serving_gate_tests`) |
| AB2-8 (D13) | Backoff: 1 s → ×2 → 5 s cap, each delay within ±20 %. It resets after an accepted ack, and a ratchet like Phase M R3 (16 → 60 s) can't recur | `reconnect_loop` test with a scripted `Attempt` |
| AB2-9 (D18, T10, T28) | An `AuthorizationAck` (and `ConnectorRevoked`) arriving while a renewal is in flight is processed before the renewal finishes. Renewal still switches make-before-break, reuses the outstanding seq and extends nothing | renewal-switch test with a slow fake `renew()` |
| AB2-10 | The build-time values (`crate::authz_timing`) are what the schedule, backoff and shield close actually use, not literals | unit/grep review |
| AB2-11 | All B1 acceptance tests still pass unchanged, except where B2 strengthens them (named in the record) | full suite |

## 3. Acceptance items — live (lab per design §15)

**Setup:**
- one coordinated build from one commit, binaries' shas recorded;
- controller on this PC (private path); connector on inkyank;
- **shield on udaya** (install + enroll; sudo on udaya);
- **relay on manojinkyank**;
- client on this PC with one resource, or `down`/`up` after changes;
- update timers off;
- fresh disposable connector identities for revoke and delete rows.

| # | Live check | Pass criterion |
|---|---|---|
| L-1 (T1) | ≥ 1 h normal operation incl. ≥ 3 renewals and client traffic | 0 `Disabled`; every request acked; B logged (Phase M instrumentation kept or equivalent) |
| L-2 (T10, D18) | ≥ 3 renewals under a long-held client flow | flow uninterrupted; no ack tail from renewal (B p99 ≪ 61 ms) |
| L-3 (T2) | Block the control plane (stop the controller) with an active client flow | new streams refused at D; the active flow cut at D (± scheduling); recovery after the controller returns within about one backoff step (≤ 5 s + reconnect) |
| L-4 (D13) | 5 controller restarts, as in Phase M R3 | each recovers in ≤ about 5 s; no ratchet |
| L-5 (T5) | Revoke the connected connector during a client flow | `Revoked` immediately; flow cut; shield streams closed; no reconnect afterwards; process stays up (D12) |
| L-6 (T19/T25) | Delete a disposable connector; restart it | `UnknownIdentity`; never serves; same after restart |
| L-7 (T9, relay) | Client reaches the resource via the manojinkyank relay; then revoke | relay-routed stream refused or cut; nothing served after revocation |
| L-8 (D15, T13) | Disable via control-plane block with the udaya shield connected | shield stream stays open carrying nothing; closed at 30 s in `Disabled`; reconnects normally after recovery |

## 4. Gates

- `cargo build` / `cargo test` green in `connector/`; the lib count rises from 219; DB-backed controller tests unaffected (controller untouched).
- Cross-language test `scripts/xlang-authz-markers.sh` passes.
- **Mutation checks**, each with a named test that fails:
  - the schedule tick;
  - `Delay` vs `Burst`;
  - `cancel_every_session`;
  - the terminal shield close;
  - the 30 s `Disabled` shield close and its cancel-on-recovery;
  - `PolicyCache::clear`;
  - the terminal QUIC close;
  - the backoff reset;
  - the backoff cap;
  - the jitter bound;
  - the concurrent renewal.
- Record: `Connector-Revocation-PhaseB2-Implementation-<date>.md` (counts, mutations, commits, live results).
- **Nothing is pushed** without an explicit request (user rule, 2026-10-21).
