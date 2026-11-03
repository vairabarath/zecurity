---
title: Connector Revocation — Phase B1 acceptance list (connector states + serving gate)
date: 2026-10-20
type: acceptance list (pre-written, design §17)
status: SIGNED OFF 2026-10-20 — B1-Q1 = option (a) ACCEPTED; IMPLEMENTED 2026-10-21 (`da5b12f`), record: Connector-Revocation-PhaseB1-Implementation-2026-10-21.md
branch: feat/sprint20-m1-phase2
revision: 4d10fb9 (in sync with origin); source surveyed, no source changed
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

## 5. Implementation notes from the pre-start survey (2026-10-20, HEAD `1d7072e`, no code written)

These came from reading the source before the user deferred the start. They are notes for whoever starts B1, not decisions.

**Where the state lives.**
- `authorization.rs` `AckTracker` is already the single process-global, mutex-serialized bookkeeping object (`SharedAckTracker = Arc<Mutex<AckTracker>>`). It holds `sent`, `highest_accepted`, `latest_accepted_sent_at` (the anchor for D) and `terminal`.
- It is written from three places: `stamp_sends` (the tonic encoder, `record_sent`), `observe_controller_msg` and `observe_status`.
- §5.4 describes a supervisor task fed by one `mpsc`. An equivalent, simpler option: extend `AckTracker` with `state`, `deadline` and `lost_at`, and publish a `watch<{state, D}>` snapshot from inside the lock on every mutation. That keeps a single writer and serialized events. Expiry would be a small task that sleeps until D and calls `expire(now)`. Admission compares `now` with D itself, so timer lag never admits. **If this is chosen, record it as the §5.4 realisation in the B1 implementation record.**

**Making the expiry rule independent of timing.** Apply `expire(now)` first in every event (ack, stream up/down, terminal). Then `Serving/ServingDisconnected → Disabled` always gets `lost_at = D` exactly, whenever the event is processed.

**Doc inconsistency to resolve when starting.**
- §4.4 says `lost_at` is also set "on entry to `Recovering`".
- §5.2 says `Disabled → Recovering` leaves `lost_at` unchanged.
- The initial health report is sent when the stream opens, which is before the first controller message (the `Recovering` trigger). So the §4.4 reading would always reject the ack for that initial report, and recovery would be delayed by one tick.
- Recommendation: follow §5.2 (the specific transition row). Ask the user before coding if in doubt.
- `Recovering → Disabled` (stream ends) sets `lost_at = now`. That is stricter, which is safe.

**Hook points in `control_stream.rs`.**
- **Tracker creation.** The tracker is created inside `run_control_stream` (`AckTracker::shared(AUTHORIZATION_FRESHNESS)`). It must move to `main.rs`, so that the listeners and the shield server get the gate before `run_control_stream` (`main.rs:300`) starts.
- **Stream up.** It is the first controller message on a stream. `StreamAuthz.first_seen` in `observe_controller_msg` already detects it.
- **Stream down.** It is every return of `run_once`, whether `Ok` or `Err`, in the outer loop of `run_control_stream`. The renewal switch drops the old stream; that is **not** a stream-down.
- **Terminal handling.**
  - Mid-stream: after `observe_controller_msg` records `ConnectorRevoked`, `run_session` should return at once, which stops health ticks and renewal.
  - After every `run_once` return, the outer loop checks `terminal()`. If it is set, the loop parks forever: a slow periodic log, never `return`.
  - `open_refused` already records the marker on a refused open.

**Entry-path refusal mechanics.**
- **TLS** (`device_tunnel::listen`, accept at `:84`): drop the `TcpStream` before the TLS handshake.
- **QUIC** (`quic_listener::listen`): use `incoming.refuse()` at `:85`. For new streams on an existing connection (the `accept_bi` loop, `:125`), reset them: `send.reset(0)` plus `recv.stop(0)`, as `relay_handler::reject_stream` does. Simply dropping a quinn `SendStream` finishes it gracefully; it is not a reset.
- **Relay** (`RelayHandler::run`): use `reject_stream` per stream when closed.
- **Funnel** (`device_tunnel::handle_stream:151`):
  - Check the gate **first**, before the CRL check. The CRL check sends `ok:false`, and a gate refusal must send no `TunnelResponse`.
  - After `registry.register(...)` (`:211`, next to the existing `is_allowed` re-check), re-check the gate. On refusal, return `Err` without writing anything.
- **Shield `control()`** (`agent_server.rs:836`): refuse with `Status::unavailable` inside the same critical section as the `revocations_synced` check (`:858`).

**Plumbing.** Pass the gate explicitly (a cheap `watch::Receiver` wrapper), not a global, because tests run in parallel. Signatures that change:
- `device_tunnel::listen` and `handle_stream`;
- `quic_listener::listen`;
- `RelayHandler::new`;
- the `ShieldRegistry` constructor or the serve path;
- the test caller `control_stream.rs:1901`, which needs an always-open test gate. A `watch::Receiver` still serves `borrow()` after its sender is dropped.
