---
title: Connector Revocation — Phase B2 Implementation Record
date: 2026-10-21
type: implementation record
design: Connector-Revocation-Implementation-Design-2026-10-14.md (revision 5; D13–D18)
acceptance: Connector-Revocation-PhaseB2-Acceptance-2026-10-21.md (signed off 2026-10-21)
branch: feat/sprint20-m1-phase2
commits: 63e9e90 (implementation + tests), 4f00448 (dropped-stream deadline test, since_send_us)
status: DONE — unit level + live acceptance L-1…L-8 PASS (2026-10-21; L-1 shortened to 15 min / 3 renewals by user decision). Pushed 2026-10-23 on `feat/sprint20-m1-phase2`.
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

Each check disabled one enforcement line against `63e9e90`, ran the full `cargo test --lib`, then restored the tree. **15 mutants, 15 killed; none skipped, none failing to compile.**

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

## 5. Live acceptance (2026-10-21): L-1 … L-8 PASS

**Lab:**
- controller on this PC (user unit `k2-controller`; `CONNECTOR_CERT_TTL=15m`, `CONNECTOR_RENEWAL_WINDOW=10m`, `RELAY_CERT_TTL=1h`);
- connector `pm-conn` on inkyank (192.168.1.75);
- shield `pm-udaya-shield` on udaya (192.168.1.37), protecting `pm-udaya-web` (tcp/8090);
- relay `pm-relay-manoj` on manojinkyank (192.168.1.49:9093);
- client on this PC.

**Binaries:**
- controller `d32cecc0b692` (`150fb66`) for L-1…L-4/L-8, then `ff4772b994fe` (`c76c783`, plus the dev relay setting) for L-7/L-5/L-6;
- connector `6c442e1b40b9`, shield `2910b0e297aa`, relay `8a47a3e8a65e` (all `150fb66`);
- `c76c783` touches the controller only (`git diff 150fb66 c76c783 -- connector shield relay proto` is empty), so this is still one coordinated build;
- update timers off.

| # | What was done | Result |
|---|---|---|
| L-1 / L-2 (T1, T10, D18) | 11:03:07–11:18:14 UTC, **15 min / 3 renewals** (user shortened it from ≥ 1 h; the 1 h schedule is covered by the paused-clock unit test). One HTTP/1.1 connection held through connector + shield, a GET every 2 s | 205 acks accepted, 0 rejected, **0 Disabled**, 0 reconnects, 0 sessions cancelled; 3/3 renewals; 451 requests delivered via the shield, no failure, one socket. **B (since_send_us): p50 3.9 ms, p99 8.2 ms, max 9.3 ms** (Phase M: p99 66–79 ms; the D18 fix confirmed) |
| L-3 (T2) | Held flow; controller stopped 11:19:18 (10 s drain, still acking); down 90 s; restarted | Last ack seq 274, sent 11:19:25.3585 → **D = 11:19:40.3585**. `Disabled` logged 11:19:40.3595 (+1 ms); every session cancelled 11:19:40.3596; the flow saw a reset; new attempts reset, never `ok:false`. Backoff while down: 2.1, 3.4, 5.2, 5.6, … s (capped about 5 s ± 20 %). Controller back 11:20:58.6 → **`Serving` 11:21:02.38 (3.8 s)** |
| L-8 (D15, T13) | Same run | Shield streams kept during `Disabled`, **closed at 11:20:10.3607 = Disabled + 30.00 s** |
| L-4 (D13) | 3 restarts, 60 s apart | Lost → `Serving` in **1.04 s, 1.22 s, 0.93 s**; backoff 990 / 1155 / 850 ms (reset after an ack); 0 Disabled, 0 cancellations (Phase M R3: 16–60 s) |
| L-7 (T9 relay) | Controller with `RELAY_DEV_ALLOW_PRIVATE_ADDR` (`c76c783`); connector restarted; direct UDP :9092 blocked on the client host (nftables) | Client: "direct path failed; used relay fallback relay_addr=192.168.1.49:9093"; relay "bridging Client to Connector through Relay"; 200 OK via the relay |
| L-5 (T5) | Held flow over the relay; `pm-conn` revoked in the admin UI | Connector 12:00:30.811: `Serving → Revoked`; within 0.3 ms every session cancelled, client QUIC connections closed, shield streams closed, ACL cleared; reconnect loop stopped (**0 attempts since**); **process stays up** (same instance, D12). Shield: stream ended with **`Unavailable` "connector is not authorized to serve"**, reconnect refused the same way. Relay: bridge ended 12:00:30.83; new bridge attempts reset; client flow refused from 12:00:31.887 on |
| L-6 (T19, T25) | Throwaway connector `pm-disposable` on udaya (user-level process): enrolled (`Starting → Serving` in 0.4 s), revoked, row deleted (the API deletes only pending or revoked connectors), process restarted twice | Each restart: `Starting → **UnknownIdentity**` (the controller's `unknown` marker), **0 acks**, never served, reconnect loop stopped, process alive |

**Observed, outside B2** (logged as findings):
1. **Client recovery lag.** After L-3 the client's held flow resumed about 21 s after the connector was serving again: client direct-path retry backoff (runbook KI-5).
   - **Corrected and fixed 2026-10-22: [[Sprint20/Fixes/Fix08-Shield-Reattach-After-Connector-Disabled]].** The cause was not the client. The client journal shows `shield not attached` until 11:21:19: the shield's exponential reconnect backoff (5 → 60 s) against the `Disabled` connector's refusals.
   - The shield now retries every 2 s while the connector says "not authorized to serve". Live re-attach lag: 37.3 s → 0.24 s.
2. **Admin UI: a resource created before its shield can't be protected.** The button requires `resource.shield`, though the backend `MarkProtecting` would bind it. Workaround: delete and recreate the resource. **Fixed 2026-10-22: [[Sprint20/Fixes/Fix10-Admin-Protect-Resource-Created-Before-Shield]].** The UI now picks the protect target the way `MarkProtecting` does. Verified live: the old UI had the button disabled; the fixed UI binds the shield.
3. **Connector `relay_selector::run` never refreshes its relay list while not attached.** An initially empty list keeps it in backoff until the process restarts. **Fixed 2026-10-22: [[Sprint20/Fixes/Fix07-Connector-Relay-List-While-Disconnected]].** The backoff now wakes on a pushed list and adopts it. Verified live: the dial starts in the same millisecond as the push, and the backoff still grows.
4. **Admin and client sessions became invalid after controller restarts** (re-login needed, three times today). **Fixed 2026-10-22: [[Sprint20/Fixes/Fix06-Session-Refresh-Per-Login]].**
   - Root cause, reproduced live: one refresh slot per user (`refresh:<userID>`), shared by the admin UI and the client daemon. Each login or rotation of one killed the other with `refresh token mismatch`. The restarts only coincided with a re-login.
   - Now one refresh session per login.
5. **Client hot-apply admits one new resource IP** (smoltcp `IFACE_MAX_ADDR_COUNT = 2`); a `down`/`up` cycle was needed. **Fixed 2026-10-22: [[Sprint20/Fixes/Fix09-Client-AnyIP-Resource-Capacity]].**
   - The client now uses AnyIP plus IP-bound listeners: no per-resource smoltcp addresses, so no limit.
   - This also fixed shared-port misrouting and the silent drop at startup.
   - Verified live: 3 IPs hot-added in one pass, all tunnelled; at startup all tunnelled.
6. **Relays on private IPs were never advertised.** Addressed for dev labs by `c76c783` (`RELAY_DEV_ALLOW_PRIVATE_ADDR`, `ENV=development` only), as the user decided.

**All six findings were fixed and passed a full live retest on 2026-10-22** (build `2f5f411`; report [[Sprint20/Fixes/B2-Findings-Retest-2026-10-22]], `3f12a29`):

| Finding | Fix | Retest result |
|---|---|---|
| 1. Client recovery lag after a connector outage (shield retry wait) | Fix 08 `987147e`: the shield polls a "not serving" connector every 2 s | shield back 1.67 s after the connector served (was 37.3 s) |
| 2. Admin UI couldn't protect a resource created before its shield | Fix 10 `2f5f411`: the Protect gate mirrors `MarkProtecting`'s shield choice | button enabled, resource bound to the shield |
| 3. Relay selector ignored new lists while unattached | Fix 07 `ef7e498` | registered 36 ms after the relay returned, same process, no restart |
| 4. Sessions invalid after controller restarts / logged each other out | Fix 06 `26e7779`: one refresh session per login | interleaved refreshes, logout and 3 restarts all OK |
| 5. Client hot-apply admitted one new resource IP | Fix 09 `7442041`: AnyIP capture interface and IP-bound listeners | 2 resources added in 16 ms; no "capacity" messages |
| 6. Relays on private IPs never advertised | dev setting `c76c783` (`RELAY_DEV_ALLOW_PRIVATE_ADDR`, `ENV=development` only) | relay_count 0 without, 1 with |

**Lab state after the run:**
- `pm-conn` is **revoked** (inkyank connector idles; the lab needs a new connector enrollment for further use);
- `pm-disposable` deleted and stopped;
- shield and relay still running;
- **the nftables block on this PC is still in place** (`sudo bash ~/.cache/zecurity-b2/l7-unblock.sh` removes it).
