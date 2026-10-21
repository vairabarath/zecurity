---
title: Connector Revocation — Phase B2 Implementation Record
date: 2026-10-21
type: implementation record
design: Connector-Revocation-Implementation-Design-2026-10-14.md (revision 5; D13–D18)
acceptance: Connector-Revocation-PhaseB2-Acceptance-2026-10-21.md (signed off 2026-10-21)
branch: feat/sprint20-m1-phase2
commits: 76a0130 (implementation + tests), cd03540 (dropped-stream deadline test, since_send_us)
status: UNIT LEVEL DONE — live acceptance (L-1 … L-8) pending lab setup (udaya shield, manojinkyank relay). Committed locally, NOT pushed.
---

# Phase B2 — request schedule and full fail-closed

**The connector only changes.** The controller and protos are untouched (D16).

## 1. What changed

| Area | Change |
|---|---|
| `control_stream.rs`: schedule | Authorization requests (full health report, D16) every **P = `authz_timing::AUTHZ_REQUEST_PERIOD_MS`** (4.5 s), on an `Interval` owned by the outer loop (`LiveAttempt.schedule`), so it survives reconnects and renewal switches. `MissedTickBehavior::Delay`. Reset when a new stream opens (its initial report is the current request). The 15 s tick keeps only the upstream flushes (resource acks, shield status, state batch). |
| `control_stream.rs`: renewal (D18) | `StreamOps::renew` returns an owned future (`RenewFuture`), and `StreamOps::renewed` applies the result. The session loop polls the renewal next to the stream, so `AuthorizationAck` and `ConnectorRevoked` are processed while it runs. A second `ReEnroll` during a renewal is ignored (logged). After the renewal the make-before-break switch is unchanged. |
| `control_stream.rs`: backoff (D13) | `reconnect_loop`: delay = `jittered(backoff, 20 %)`; backoff 1 s → ×2 → 5 s (`CONTROL_RECONNECT_*`). It **resets when the attempt received an accepted ack** (`highest_accepted` advanced). |
| `authorization.rs` | `AuthzSnapshot.closures`: a count of open → closed transitions, so a reader never misses a brief lapse. `AuthzGate::subscribe` (crate-internal). Every `ack_accepted`/`ack_rejected` log line now carries `since_send_us` (transport send → ack processed, ≈ B). |
| `fail_closed.rs` (new) | One task per process (`main.rs`) follows the published state. **Every closure:** `SessionRegistry::cancel_every_session`. **Terminal:** `ShieldRegistry::close_all_streams` + `PolicyCache::clear`, once. **`Disabled`/`Recovering` (D15):** shield streams closed after `DISABLED_SHIELD_CLOSE_SECS` (30 s), once per episode, unless serving resumes first. |
| `agent_server.rs` | `close_all_streams`: a registry-wide signal. Open shield Control streams end with **`Unavailable`** "connector is not authorized to serve" (the shield fails over). A shield revocation still ends its stream with `PermissionDenied`. |
| `quic_listener.rs` | `spawn_terminal_close`: in a terminal state the endpoint and every client QUIC connection are closed. `Disabled` keeps them (B1 resets their new streams). |
| `session_registry.rs`, `policy/mod.rs` | `cancel_every_session`, `PolicyCache::clear`. |
| `Cargo.toml` | dev-dependency: tokio `test-util` (paused clock). |

## 2. Behaviour changes to note

1. **A clean session end without an accepted ack no longer resets the backoff.** B1 reset it on any clean close; D13 says "reset after an accepted ack", and B2 follows D13 exactly.
2. **The renewal-replacement open retry (`PendingSwitch`) still uses 2 s → 60 s.** D13 covers the reconnect loop only. The replacement retry runs while the old stream is still authoritative and serving, so it doesn't affect D.
3. **The connector now sends a health report every 4.5 s** instead of every 15 s. The controller logs "received health report" about 3× as often.
4. **`quic_listener_follows_the_serving_state` was strengthened, not weakened.** It now checks both halves:
   - `Disabled`: the connection is kept, its new streams are reset, and new connections are refused;
   - terminal: the connection itself is closed.

   B1 asserted only the stream reset after `UnknownIdentity`.

## 3. Tests

- Lib tests: **219 → 233**, all green 5 runs in a row.
- `tests/scenario4_probe_security.rs`: 4/4.
- Cross-language marker test (`scripts/xlang-authz-markers.sh`): passes.

| Item | Tests |
|---|---|
| AB2-1 (T1) | `healthy_hour_never_disables`: 1 h of paused clock, acks at 250 ms → `Serving` throughout, 0 closures, 800 requests (one every P) |
| AB2-2 (T2, T27) | `missing_acks_close_exactly_at_the_deadline` (controller silent); `dropped_stream_closes_exactly_at_the_deadline` (transport failure, no reconnect). Both: open at D − 1 ms; at exactly D the gate refuses, the state is `Disabled`, `lost_at = D`, and the session is cancelled |
| AB2-3 (§6.3) | `schedule_never_bursts_after_a_stall` |
| AB2-4 (T9) | `losing_authorization_cancels_every_session` (connector- and shield-routed); `a_brief_closure_is_not_missed` |
| AB2-5 (D15, T13) | `terminal_closes_shields_and_clears_the_acl_immediately`; `disabled_closes_shields_after_exactly_the_grace`; `recovery_before_the_grace_keeps_shield_streams`; `closing_all_streams_ends_them_with_unavailable` (real shield gRPC stream) |
| AB2-6 (step 5) | `terminal_closes_shields_and_clears_the_acl_immediately` (cleared; a later snapshot is applied but the gate stays closed) |
| AB2-7 (D15) | `quic_listener_follows_the_serving_state` (real QUIC) |
| AB2-8 (D13) | `backoff_doubles_to_the_cap_and_resets_after_an_ack` (gaps 1, 2, 4, 5, 5, 5, 5 s → reset 1, 2, 4 s, each within ±20 %); `jitter_stays_within_its_bound_and_varies` |
| AB2-9 (D18) | `acks_are_processed_while_a_renewal_runs` (ack < 500 ms during a 1.5 s renewal; the switch still happens); `revocation_during_a_renewal_is_immediate` |
| AB2-10 | grep: `authz_timing::` is used by `control_stream.rs` (P, backoff base, max, jitter) and `fail_closed.rs` (grace). No literals |
| AB2-11 | all B1 tests pass; only the QUIC test changed (item 2.4 above) |

## 4. Mutation checks

Each check disabled one enforcement line against `76a0130`, ran the full `cargo test --lib`, then restored the tree. **15 mutants, 15 killed; none skipped, none failing to compile.**

| Mutant | Killed by |
|---|---|
| B1 schedule sends the request | `healthy_hour_never_disables`, `missing_acks_close_exactly_at_the_deadline` |
| B2 `Delay` → `Burst` | `schedule_never_bursts_after_a_stall` |
| B3 cancel every session on closure | `losing_authorization_cancels_every_session`, `a_brief_closure_is_not_missed`, `missing_acks_close_exactly_at_the_deadline` |
| B4 closure counter | the same three |
| B5 terminal closes shields | `terminal_closes_shields_and_clears_the_acl_immediately` |
| B6 `Disabled` grace closes shields | `disabled_closes_shields_after_exactly_the_grace` |
| B7 recovery cancels the grace close | `recovery_before_the_grace_keeps_shield_streams` |
| B8 terminal clears the ACL | `terminal_closes_shields_and_clears_the_acl_immediately` |
| B9 terminal closes client QUIC | `quic_listener_follows_the_serving_state` |
| B10 backoff reset after an ack | `backoff_doubles_to_the_cap_and_resets_after_an_ack` |
| B11 backoff cap | `backoff_doubles_to_the_cap_and_resets_after_an_ack` |
| B12 jitter bound | `backoff_doubles…`, `jitter_stays_within_its_bound_and_varies` |
| B13 concurrent renewal (awaited inline) | `acks_are_processed_while_a_renewal_runs`, `revocation_during_a_renewal_is_immediate` |
| B14 shield stream obeys close-all | `closing_all_streams_ends_them_with_unavailable` |
| B15 close-all says `Unavailable` | `closing_all_streams_ends_them_with_unavailable` |

## 5. Live acceptance (L-1 … L-8): pending

**Lab per design §15:**
- controller (this PC) and connector (inkyank), both built from **one B2 commit**;
- controller with `CONNECTOR_CERT_TTL=15m` and `CONNECTOR_RENEWAL_WINDOW=10m`, so the runs include renewals;
- **shield on udaya**, **relay on manojinkyank**;
- client with one resource, or `down`/`up` after each change.

**For L-1/L-2:** B is read from `since_send_us` on `ack_accepted` (permanent in B2). The B2 connector has no `health_enqueued` log; Phase M showed that transport-send → ack and enqueue → ack differ by about 0.1 ms.
