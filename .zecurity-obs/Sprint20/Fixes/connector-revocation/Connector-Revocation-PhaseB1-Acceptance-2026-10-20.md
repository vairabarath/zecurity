---
title: Connector Revocation — Phase B1 acceptance list (connector states + serving gate)
date: 2026-10-20
type: acceptance list (pre-written, design §17)
status: SIGNED OFF 2026-10-20 — B1-Q1 = option (a) ACCEPTED; implementation authorized
branch: feat/sprint20-m1-phase2
revision: 4d10fb9 (in sync with origin)
design: Connector-Revocation-Implementation-Design-2026-10-14.md (rev 4)
handoff: ../../Handoff-Connector-Revocation-PhaseA-2026-10-15.md
---

# Phase B1 acceptance list — connector states + serving gate

Signed off 2026-10-20; B1-Q1 answered: **option (a) ACCEPTED**. O9 is resolved (D12, 2026-10-20).

## 0. Decision needed before B1: B1-Q1 — the deadline vs today's schedule

**Problem (CONFIRMED BY SOURCE, design §6.2).** Health reports still go out every H on the per-session ticker (`control_stream.rs` `run_session`). With `D = sent[k] + H`, the ack for k+1 arrives at about `sent[k] + H + RTT`, which is always after D. So a B1 build can't run in the live lab on the current schedule. What happens depends on where the expiry transition lives, and the documents disagree:
- **Design §13** gives B1 all of §5, including `Serving → Disabled at now ≥ D`. The connector then drops to `Disabled` every cycle. Re-entry depends on jitter between `sent[k+1]` and `lost_at = D`, so it can stay closed for several cycles.
- **The handoff** puts "deadline expiry" in B2. Then a B1 connector stays in `Serving` with an expired D, the admission check (`now < D`) refuses everything, and every later ack is rejected by the §4.4 row "Serving: `now < D`". It locks closed after the first heartbeat.

**Options:**

| Option | What B1 does | Cost |
|---|---|---|
| **(a) ACCEPTED 2026-10-20.** B1 is a unit-tested intermediate, like Phase A | full §4.4 rules, including D and `lost_at`; the full §5 state machine, including the expiry transition; the admission check `now < D`. All tested by injecting `now` (`AckTracker::on_ack` already takes `now`), so no tokio `test-util` is needed. **Not deployed to the live lab.** The lab stays on the Phase A build (`61ec3ac`/`4d10fb9` include `4cb5a6a`), where Phase M can run now and produce B and P for B2 | live rows wait for B2 |
| (b) State-only gate | B1 closes the data plane only in `Starting`, `Revoked` and `UnknownIdentity`; the `now < D` check and expiry move to B2 | changes the §13 scope; a B1 lab build would serve without freshness enforcement |
| (c) Interim P for development | pick a temporary P < H | **advised against**: P is BLOCKED ON MEASUREMENT (O7), and choosing it in code is an architectural decision |

The rest of this list assumes **(a)**.

## 1. Scope

**In scope (design §13 B1 row):**
1. **Authorization supervisor** (new module, e.g. `connector/src/authz_supervisor.rs`): the single owner of state, `sent`, `highest`, `D`, `lost_at`, built on `authorization.rs` (`AckTracker`, `observe_controller_msg`, `observe_status`, `TerminalSignal`). Inputs arrive over one `mpsc` (§5.4): `ReportEnqueued`, `Ack`, `Revoked`, `UnknownIdentity`, `StreamUp`/`StreamDown`, `Capability`. It publishes `{state, D}` as one `watch` snapshot.
2. **States** `Starting` / `Serving` / `ServingDisconnected` / `Disabled` / `Recovering` / `Revoked` / `UnknownIdentity`, with the §5.2 transitions.
3. **The remaining §4.4 rows:** "Serving/ServingDisconnected: `now < D`" and "Starting/Disabled/Recovering: `sent[k] ≥ lost_at`". On acceptance, `D = sent[k] + H` (monotonic).
4. **Admission gate (§7 steps 1–2).** Every entry path reads the snapshot and admits only when the state is `Serving`/`ServingDisconnected` **and** `Instant::now() < D`:

   | Entry path | Location (HEAD `4d10fb9`) | Refusal |
   |---|---|---|
   | TLS device tunnel | `device_tunnel.rs:84` accept → `:119` `handle_stream` | drop the socket |
   | QUIC device tunnel | `quic_listener.rs:85` `endpoint.accept()` → `:144` | `Incoming::refuse()` for new connections; stream reset for new streams on an existing connection |
   | Relay-routed | `relay_handler.rs:187` → `device_tunnel::handle_stream` | stream reset |
   | Funnel | `device_tunnel::handle_stream:151` | stream reset, **no `TunnelResponse`**, never `ok:false` (§7 step 2); re-check after session registration (mirror `device_tunnel.rs:252-258`) |
   | Shield `control()` | `agent_server.rs:836` | refuse new shield streams while not serving, next to the existing `revocations_synced` check (T15) |

   (`control_stream.rs:1901` is test code, not an entry path.)
5. **Terminal handling (D12):** in `Revoked`/`UnknownIdentity` the reconnect loop in `run_control_stream` stops, and so do health reports and renewal attempts. **The process stays alive**: `run_control_stream` parks and never returns, because it is `main`'s last call (`main.rs:300`) and returning would exit the process, which Docker's `restart: unless-stopped` would restart. The terminal state is logged once on entry, then periodically at a slow interval.
6. **Capability assertion (§4.6):** `AgentPolicy.authorization_acks` false or absent → stay closed, log the diagnostic, keep retrying (never terminal).
7. **Restart:** every start begins in `Starting`, and nothing is read from disk about a previous terminal state (D12).

**Out of scope (B2 or later):** schedule P and `MissedTickBehavior::Delay` (§6.3); cancelling existing sessions (§7 step 3, `cancel_every_session`); closing shield channels (step 4, O8); `PolicyCache::clear()` (step 5); tokio `test-util` and paused-clock tests (T1, T2, T27); reconnect backoff in `Disabled` (O5); client, relay and shield phases C/D/E.

## 2. Acceptance items

Each item closes at **unit** level in B1 unless it is marked deferred.

| # | Requirement | B1 evidence | Deferred part |
|---|---|---|---|
| AB1-1 (T15, T26) | A new process starts in `Starting`; no entry path in §1.4 admits anything before the first accepted ack; after it, the state is `Serving` and D = `sent[k] + H` | supervisor unit tests + one admission test per entry path (TLS, QUIC connection, QUIC stream, relay, shield `control()`) | live T26 after B2 |
| AB1-2 (T3) | Enqueueing a report never moves D | unit | — |
| AB1-3 (T4) | One rejection test per §4.4 row: terminal, zero, unknown, not newer, stale (`now ≥ sent + H`), Serving with `now ≥ D`, pre-`lost_at` in `Starting`/`Disabled`/`Recovering`, no capability | unit (`now` injected) | — |
| AB1-4 (§5.2) | Every transition in the §5.2 table, including `Serving → ServingDisconnected` on stream end, `→ Disabled` at `now ≥ D` (`lost_at = D`), `Disabled → Recovering` on the first controller message, and `Recovering → Disabled` on stream end | table-driven unit test, injected time | real-time expiry under load (T2) after B2 |
| AB1-5 (T5) | `ConnectorRevoked` and the `revoked` marker → `Revoked` immediately (no wait for D); admission closed at once | unit | session cancellation and live run after B2 |
| AB1-6 (T19) | The `unknown` marker → `UnknownIdentity`, closed at once | unit | live delete after B2 |
| AB1-7 (T16, T20) | `Revoked`, `UnknownIdentity` and `Disabled` are separate states with distinct log events; a marker of the other kind doesn't change a terminal state; the first terminal signal wins over a racing ack | unit | — |
| AB1-8 (T6, T24) | Reconnect after an interruption doesn't admit until an accepted ack for a request sent at or after `lost_at`; in `UnknownIdentity` a transport reconnect never happens | unit (fake `StreamOps`, as the existing `control_stream` tests) | live T6 after B2 |
| AB1-9 (T7, T25) | Terminal + restart: a new supervisor starts in `Starting`; a `revoked`/`unknown` marker on the first open returns it to the same terminal state with no admitted entry in between | unit | live after B2 |
| AB1-10 (D12) | In a terminal state: no further `open()` calls (the reconnect loop has stopped); no health reports and no renewal; `run_control_stream` doesn't return (assert it is still pending after the terminal event plus several backoff periods) | unit with a fake `StreamOps` counting opens | — |
| AB1-11 (T8, T21, T23) | Unmarked `NotFound`/`PermissionDenied`/`Unavailable`/`Unauthenticated` and transport errors never produce a terminal state; they lead only to recoverable states | unit; Phase A cross-language test still green | — |
| AB1-12 (T14, §4.6) | Capability false or absent → no admission, ever, even if an ack-shaped message arrives | unit | full mixed-pairing T14 at T18 |
| AB1-13 (T28, T10) | Renewal switch: the replacement stream reuses the outstanding seq and its `sent` time, an ack on either stream is accepted once, renewal emits no marker and no terminal state, and D is never extended | existing renewal tests extended | live ≥ 3 renewals after B2 |
| AB1-14 | Readers never mutate state; only the supervisor transitions (no write path outside it) | code review + a test that the snapshot type exposes no setter | — |

## 3. Gates

- `cd connector && cargo build` and `cargo test` green; the lib test count rises from the Phase A baseline (161) and no existing test is removed or weakened.
- The Phase A cross-language test passes: `scripts/xlang-authz-markers.sh`.
- Controller untouched by B1. If a controller change turns out to be needed, stop and ask.
- **Mutation checks** (team practice): disable each of the following and confirm a named test fails:
  - the admission check, once per entry path;
  - the `now < D` row;
  - the `lost_at` row;
  - the terminal-wins rule;
  - stopping the reconnect loop;
  - "park, don't return".
- Record: `Connector-Revocation-PhaseB1-Implementation-<date>.md`, with test counts, mutation results and the commit.
- **Live deployment: none under option (a).** The record must state that a B1 build is a development intermediate that must not go into the lab until B2 lands.

## 4. Notes

- `connector/systemd/zecurity-connector.service` sets no `WatchdogSec`, so `spawn_watchdog` is a no-op today. The idle design doesn't depend on it. If a watchdog is added later, it must keep pinging while idle.
- The existing shield `revocations_synced` gate in `agent_server.rs` (refuse with `Unavailable` until synced) is the pattern for the shield-side admission check.
