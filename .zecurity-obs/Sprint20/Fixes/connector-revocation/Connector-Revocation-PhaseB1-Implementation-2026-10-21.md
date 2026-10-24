---
title: Connector Revocation — Phase B1 Implementation Record
date: 2026-10-21
type: implementation record
design: Connector-Revocation-Implementation-Design-2026-10-14.md (revision 4)
acceptance: Connector-Revocation-PhaseB1-Acceptance-2026-10-20.md (signed off 2026-10-20, B1-Q1 = a)
branch: feat/sprint20-m1-phase2
base: 6a6b52a
commit: da5b12f (implementation + tests)
status: PHASE B1 IMPLEMENTED — was a unit-tested intermediate; superseded by B2 (2026-10-21, live L-1…L-8 PASS), which made it deployable. See the B2 record.
---

# Phase B1 — Connector serving states and data-plane admission gate

> **Do not deploy a B1 build to the lab.** Health reports still go out every H (15 s), which is also the freshness bound. So with `D = sent[k] + H`, every next ack arrives just after D (design §6.2), and a B1 connector would drop to `Disabled` every cycle. The lab stays on the Phase A build (`4d10fb9`) until B2 lands the request period P < H − B, which needs the Phase M measurements.

## 1. What B1 does

**Connector only** (`connector/src`). The controller, protos and migrations are untouched.

| Area | Change |
|---|---|
| `authorization.rs` | `AckTracker` is now the serving-state machine. It has the states `Starting`/`Serving`/`ServingDisconnected`/`Disabled`/`Recovering`/`Revoked`/`UnknownIdentity`, with `deadline` (D) and `lost_at`. Events: `on_ack`, `on_terminal`, `stream_up`, `stream_down`, `expire`. One private `transition()` is the only place the state changes; it publishes `{state, D}` as one `AuthzSnapshot` over a `watch` channel and logs `authz_event = "state"`. `AuthzGate` is the read-only reader (`admits_now()`). `spawn_expiry` moves the state to `Disabled` at D. New ack rejection: `BeforeLoss`. |
| `main.rs` | Creates the tracker (in `Starting`), its gate and the expiry task **before** any listener. It passes the gate to the TLS and QUIC listeners, `RelayHandler` and `ShieldRegistry`, and the tracker to `run_control_stream`. |
| `control_stream.rs` | The outer loop moved into `reconnect_loop<A: Attempt>` (production `LiveAttempt`; tests script it). Every attempt end is `stream_down`. In a terminal state the loop calls `park_terminal`, which logs every 300 s and never returns (`-> Infallible`). `run_session` returns as soon as a terminal signal is recorded: after a controller message, or after a replacement open refused with a marker. |
| `device_tunnel.rs` | TLS: the socket is dropped before the TLS handshake. `handle_stream` (the funnel for every transport) checks the gate **first**, before the CRL check, so a refusal writes nothing (never `ok:false`). It checks again after `registry.register`. |
| `quic_listener.rs` | New connections are refused with `Incoming::refuse()`. New streams on an established connection are reset (`send.reset(0)` + `recv.stop(0)`). |
| `relay_handler.rs` | Relay-opened streams are reset (`reject_stream`) before the inner handshake. |
| `agent_server.rs` | `control()` returns `Unavailable("connector is not authorized to serve")`, in the same critical section as the `revocations_synced` check. |

Admission rule: the state is `Serving` or `ServingDisconnected`, **and** `Instant::now() < D`, read from one snapshot. It doesn't depend on the expiry task, so a starved timer can't keep the data plane open past D.

## 2. Design choices and deviations (for review)

1. **§5.4 realisation.** The design describes a supervisor task fed by one `mpsc`. B1 implements it as the existing process-global `AckTracker` behind its `parking_lot::Mutex`:
   - every event is serialized by the lock;
   - `transition()` is the single writer;
   - the snapshot is published from inside the lock.

   The guarantees are the same (serialized events, single writer, atomic `{state, D}`), and timing-independent tests are simpler. Entry-path modules import only `AuthzGate`:

   ```
   grep -n "AckTracker\|SharedAckTracker" connector/src/{device_tunnel,quic_listener,relay_handler}.rs
   (none)
   ```

   `agent_server.rs` names `AckTracker` only inside its test module.
2. **§4.4 row "Serving/ServingDisconnected: `now < D`".** It is folded into "expire first". `on_ack` applies `expire(now)` before anything else, so an ack arriving at or after D finds the state already `Disabled` with `lost_at = D`. A request sent before D is then rejected by the `lost_at` row (`BeforeLoss`). An explicit `AfterDeadline` branch would have been unreachable, so it was removed.
3. **`lost_at` follows §5.2, not §4.4 — CONFIRMED by the user 2026-10-21 (do not reset it on reconnect).** §4.4 lists "entry to `Recovering`" as a `lost_at` instant; §5.2 says `Disabled → Recovering` leaves `lost_at` unchanged. B1 follows §5.2. Under the §4.4 reading, the reconnect's initial report (sent at stream open, before the first controller message) could never be accepted, which delays every recovery by one tick. `Recovering → Disabled` (the stream ends) sets `lost_at = now`, which is stricter and therefore safe.
4. **Shield admission is gated at `control()` only** (as listed). Other shield RPCs, and the shield TCP accept, keep their existing `revocations_synced` checks. Closing existing shield streams is B2 (§7 step 4, O8).
5. **Terminal idle (D12).** No state is persisted. `run_control_stream` can't return in a terminal state; this is guaranteed by the `Infallible` return type of `reconnect_loop`/`park_terminal`, and a mutant that returns doesn't compile. The test also asserts that the parked task is still pending.

## 3. Observation for B2 / Phase M

When a reconnect's initial report is sent **before** D and acknowledged **after** D, the ack is rejected (`BeforeLoss`, or `Stale`). Serving then resumes only with the next request, which today is the next 15 s health tick (`reconnect_after_expiry_serves_only_after_a_fresh_ack` shows this). B2's schedule (P, and an immediate request when a stream opens in `Recovering`) should take this into account.

## 4. Tests

**Lib tests:** 198 at `6a6b52a` (the real pre-B1 baseline; the acceptance list's "161" was the Phase A count and is stale) → **217**. No existing test was removed.

**Integration:** `tests/scenario4_probe_security.rs` 4/4. The cross-language test `scripts/xlang-authz-markers.sh` (Go grpc controller ↔ tonic connector) passes.

**Stability:** `cargo test --lib` was run 5 times in a row, all green.

**Three Phase A tests were changed to the stricter B1 behaviour** (none weakened):

| Test | Phase A expectation | B1 expectation |
|---|---|---|
| `connector_revoked_on_old_stream_while_replacement_pending` | session keeps running until the controller's marker | session ends on `ConnectorRevoked` itself; state `Revoked`; no further open |
| `connector_revoked_on_new_stream_after_switch` | session keeps running | session ends; state `Revoked`; nothing sent afterwards |
| `replacement_refused_with_revoked_marker` | the old stream keeps being served (Pong) | session ends with the marked refusal; no retry; old stream no longer served |

**New tests, by acceptance item:**

| Item | Tests |
|---|---|
| AB1-1 (T15, T26) | `starts_closed_and_first_accepted_ack_opens_with_send_time_deadline`; `tls_listener_follows_the_serving_state`; `quic_listener_follows_the_serving_state` (connection refused, stream reset); `relay_streams_are_reset_while_not_serving`; `funnel_refuses_without_a_tunnel_response_when_not_serving` (Starting, past D, Disabled, Revoked, Unknown, each also with a no-ACL-match and a CRL-unavailable request); `funnel_rechecks_the_serving_state_after_registration`; `shield_control_follows_the_connector_serving_state` |
| AB1-2 (T3) | `a_send_never_moves_the_deadline` |
| AB1-3 (T4) | existing Phase A rows, plus `late_ack_after_deadline_is_rejected` and `pre_loss_acks_rejected_post_loss_ack_accepted` |
| AB1-4 (§5.2) | `transition_table`; `first_controller_message_moves_disabled_to_recovering`; `expiry_task_disables_at_deadline`; `an_ended_attempt_is_a_stream_end` |
| AB1-5/6/7 (T5, T19, T16, T20) | `terminal_states_close_immediately_and_absorb_everything` |
| AB1-8 (T6, T24) | `reconnect_after_expiry_serves_only_after_a_fresh_ack`; `terminal_marker_stops_reconnecting_and_the_loop_never_returns` (unknown) |
| AB1-9 (T7, T25) | `terminal_marker_stops_reconnecting_and_the_loop_never_returns` (fresh tracker = restart; never opened) |
| AB1-10 (D12) | same test: no attempt after the marker across many backoff periods; task still pending |
| AB1-11 (T8, T21, T23) | `unmarked_failures_keep_reconnecting_and_are_never_terminal`; Phase A `unmarked_errors_never_terminal_and_unknown_is_distinct` |
| AB1-12 (T14, §4.6) | `capability_absent_never_opens`; Phase A `ack_on_stream_without_capability_is_rejected` |
| AB1-13 (T28, T10) | `renewal_reuses_outstanding_seq_and_keeps_its_send_time`, now also asserting D = original send time + H |
| AB1-14 | structural, not a test: `AuthzSnapshot` has private fields and only getters; `AuthzGate` has no mutating method; entry paths import only `AuthzGate` (grep above); `transition()` is private |

**Deferred, as listed:**
- live T26/T6/T5/T19/T25 after B2;
- real-time expiry under load (T2) and T27 paused clock (tokio `test-util`) in B2;
- session cancellation and shield close (§7 steps 3–4) in B2.

## 5. Mutation checks

Each enforcement line was disabled in turn against `da5b12f`, and the full `cargo test --lib` was run. **18 mutants, 18 killed.** The tree was restored after each one.

| Mutant | Killed by |
|---|---|
| M1 funnel entry check | `funnel_refuses_without_a_tunnel_response_when_not_serving` |
| M2 funnel post-registration re-check | `funnel_rechecks_the_serving_state_after_registration` |
| M3 TLS accept check | `tls_listener_follows_the_serving_state` |
| M4 QUIC connection check | `quic_listener_follows_the_serving_state` |
| M5 QUIC stream check | `quic_listener_follows_the_serving_state` |
| M6 relay stream check | `relay_streams_are_reset_while_not_serving` |
| M7 shield control() check | `shield_control_follows_the_connector_serving_state` |
| M8 admission now < D | `starts_closed_and_first_accepted_ack_opens_with_send_time_deadline`, `reconnect_after_expiry_serves_only_after_a_fresh_ack`, `funnel_refuses_without_a_tunnel_response_when_not_serving`, `funnel_rechecks_the_serving_state_after_registration` |
| M9 on_ack expire-first (deadline row) | `late_ack_after_deadline_is_rejected` |
| M10 lost_at row | `late_ack_after_deadline_is_rejected`, `pre_loss_acks_rejected_post_loss_ack_accepted` |
| M11 terminal wins over ack | `connector_revoked_event_is_terminal_and_blocks_later_acks`, `rejects_after_terminal_and_first_terminal_wins`, `terminal_states_close_immediately_and_absorb_everything` |
| M12 reconnect loop stops in terminal state | `terminal_marker_stops_reconnecting_and_the_loop_never_returns` |
| M13 session ends on terminal message | `connector_revoked_on_new_stream_after_switch`, `connector_revoked_on_old_stream_while_replacement_pending` |
| M14 marked replacement refusal ends session | `replacement_refused_with_revoked_marker` |
| M15 lost_at = D (not processing time) | `transition_table`, `expiry_task_disables_at_deadline` |
| M16 Disabled -> Recovering on stream up | `first_controller_message_moves_disabled_to_recovering`, `pre_loss_acks_rejected_post_loss_ack_accepted`, `transition_table`, `reconnect_after_expiry_serves_only_after_a_fresh_ack` |
| M17 Recovering -> Disabled moves lost_at | `transition_table` |
| M18 stream end -> ServingDisconnected | `an_ended_attempt_is_a_stream_end`, `reconnect_after_expiry_serves_only_after_a_fresh_ack` |

"Park, don't return" can't be mutated into a returning version: `reconnect_loop` and `park_terminal` return `Infallible`, so such a mutant doesn't compile. The behaviour is also asserted by `terminal_marker_stops_reconnecting_and_the_loop_never_returns` (the task is still pending).

## 6. Not done / follow-ups

- B2: schedule P, `MissedTickBehavior::Delay`, cancelling sessions at expiry or terminal (`cancel_every_session`), shield `close_all` (O8), `PolicyCache::clear()` in terminal states, and paused-clock tests. Blocked on Phase M (O6/O7) plus O5 and O8.
- Live acceptance rows: only after B2.
