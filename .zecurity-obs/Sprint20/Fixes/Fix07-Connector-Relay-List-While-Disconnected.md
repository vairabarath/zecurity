---
type: fix
sprint: 20
fix: 7
title: A disconnected connector never re-reads the relay list (stuck in backoff until restart)
status: fixed and verified live (2026-10-22). Committed locally, not pushed.
source: Phase B2 live run 2026-10-21, finding 3; also Sprint 20 live acceptance F4/F5/F22 (second half)
severity: medium
component: connector (src/relay_selector.rs)
branch: feat/sprint20-m1-phase2
date: 2026-10-22
tags:
  - rust
  - connector
  - relay
  - selector
---

# Fix 07: the relay selector re-reads pushed relay lists while disconnected

## Symptom

During B2 (2026-10-21), the connector's `relay_selector::run` stayed in backoff after relays appeared, until the process was restarted. The connector had started while the relay list was empty.

## Root cause

`connector/src/relay_selector.rs` `run()` keeps a local `current_list`. It was refreshed from the `relay_list_rx` watch channel in only two places:
- before the first list arrives (`current_list.is_none()`);
- inside `connected_step` (the `relay_list_rx.changed()` arm).

The `Disconnected → bootstrap → Backoff → sleep → Disconnected` loop never read the channel. So any list pushed while the selector wasn't attached was ignored for the life of the process. Two cases:
- an empty first list stayed "empty" forever, even after relays came up;
- a stale non-empty list was retried forever, even after the controller had replaced its relays.

The control stream did receive and publish the list (`control_stream.rs:912`, `relay_list_tx.send`); the selector just never looked.

## Live reproduction (2026-10-22, baseline connector = `bda9244`, unchanged selector)

**Setup:**
- controller `bda9244` (`~/.cache/zecurity-s3/controller-head`), run with `RELAY_DEV_ALLOW_PRIVATE_ADDR=true` (`ENV=development`);
- a disposable **user-level connector** `s3-relaylist` on this PC, enrolled into `k2-net` via the admin API (`generateConnectorToken` + `POST /api/connectors/{id}/token`), state in `~/.cache/zecurity-s3/conn/`, runner `run-conn.sh`.

**Simulating "a relay becomes available":** the lab relay `pm-relay-manoj` (192.168.1.49) is offline, so the relay row in the disposable local DB was set `status='active', last_heartbeat_at=now()`. The controller was then restarted. The connector's control stream reconnects and the controller sends the current list on every connect (`internal/connector/control_stream.go:597-607`). About 90 s later the controller's relay sweep marks the row `inactive` again and broadcasts an empty list. This tests exactly the selector path at fault. A real relay attach is not exercised (host offline); the unit tests cover the logic.

Times are UTC (IST = UTC+5:30).

| Time | Step | Baseline connector |
|---|---|---|
| 08:36:24 | first push | `relay_count=0` → `LabelledRelayList is empty; entering backoff` |
| 08:36:29 / :39 / :59 | backoff 5 → 10 → 20 s | `LabelledRelayList is empty` |
| 08:37:24 | relay row active + controller restart | |
| 08:37:34 | control stream reconnects | **`received LabelledRelayList push … relay_count=1`** |
| 08:37:39 | selector wakes | **`LabelledRelayList is empty; entering backoff`** (stale list) |
| 08:38:59 | next backoff (80 s) | **`LabelledRelayList is empty`**: never dials the relay |
| 08:39:24 | sweep marks relay inactive | `relay_count=0` push |

## Fix

`relay_selector.rs`:
- The `Backoff` state now calls a new `wait_backoff(delay, &mut relay_list_rx, &mut current_list)`. It `select!`s between `sleep(delay)` and `relay_list_rx.changed()`.
- A pushed list replaces `current_list` and ends the backoff early (`BackoffWake::ListChanged`, logged `New LabelledRelayList during backoff; retrying now`). The next `bootstrap` dials the new list.
- **No tight loop:** the backoff still grows after an early wake (`next_backoff_delay`). The connector's control stream re-sends the list on every reconnect, so a controller pushing lists of unreachable relays can't make the selector dial non-stop.
- A push that landed while `bootstrap` was dialing is seen immediately at the start of the backoff (the watch keeps it unseen).
- A closed channel (shutdown) waits out the full delay, with no busy loop.
- No change to `connected_step`, migration or failover.

## Tests

- **New** (`relay_selector::tests`, `tokio::test(start_paused = true)`):
  - `backoff_ends_early_and_adopts_a_pushed_list`: 120 s backoff, push at 1 s → wakes at exactly 1 s, adopts v2 with 1 relay;
  - `list_pushed_before_backoff_is_seen_immediately`;
  - `backoff_without_push_runs_full_delay_and_keeps_list`;
  - `closed_list_channel_still_waits_out_the_delay`.
- **Mutation check:** with `wait_backoff` reduced to the old plain `sleep(delay)`, the first two **FAIL**; restored, all pass.
- **Gate:** `cd connector && cargo build && cargo test` gives 237 lib + 4 integration PASS, 0 FAIL. 2 ignored: `xlang_authz_markers`, which needs the Go/Postgres harness, was ignored before this change, and doesn't touch the selector. `cargo clippy --all-targets`: no warnings in `relay_selector.rs`.
- **Note:** `cargo fmt` rewrites 11 unrelated files (the crate isn't fmt-clean). Those changes were reverted; only `relay_selector.rs` changed.

## Live verification (fixed connector, same enrolment, same scenario)

| Time | Step | Fixed connector |
|---|---|---|
| 08:40:38 | first push | `relay_count=0` → `LabelledRelayList is empty; entering backoff` (5 → 10 s) |
| 08:40:59 | relay row active + controller restart | |
| 08:41:09.469 | control stream reconnects | `received LabelledRelayList push … relay_count=1` |
| 08:41:09.469 | **same millisecond** | **`New LabelledRelayList during backoff; retrying now`** |
| 08:41:09.473 | bootstrap dials the new list | **`Relay session failed relay_addr=192.168.1.49:9093`** (the pushed relay; host offline) → backoff |
| 08:41:49 | next backoff (40 s, still growing) | retry of the same relay, fails |
| 08:42:59.700 | sweep pushes `relay_count=0` | `New LabelledRelayList during backoff; retrying now` → `LabelledRelayList is empty` (follows the controller) |
| 08:44:59 | next backoff (120 s cap) | `LabelledRelayList is empty`: no tight loop |

Compared with the baseline, the connector now acts on each relay list the controller pushes, within milliseconds, without a restart.

**Successful attach: verified 2026-10-22 (addendum below).**

## Addendum: attach to a real relay without a restart (2026-10-22, 10:51 UTC)

- **Connector:** `s3-luffy` (Fix 07 build `6a4970c`), a user-level process on luffy (192.168.1.40, pid 6321), started 10:36 with an empty relay list. It logged 29 × `LabelledRelayList is empty; entering backoff`.
- **Relay:** the old `pm-relay-manoj` couldn't heartbeat: its certificate expired 2026-10-21 13:17:33 UTC, because B2 ran with `RELAY_CERT_TTL=1h`. The controller refused every Heartbeat RPC. So:
  - a new relay `s3-relay-manoj` (`ad5966f0-…`) was registered with the provider API (provider password typed by the user in an Orca tab);
  - it was reinstalled on manojinkyank (.49) with sudo typed by the user. The expired state was kept as `/var/lib/zecurity-relay/pki.expired-*`.

| Time (UTC) | Event |
|---|---|
| 10:51:12 | relay provisioned, cert until 2026-11-21, `active` in the controller |
| **10:51:12.750** | connector: `received LabelledRelayList push … relay_count=1` |
| **10:51:12.750** | connector: `New LabelledRelayList during backoff; retrying now` |
| **10:51:12.790** | connector: **`Connector registered with Relay`** (40 ms after the push) |
| 10:51:12 | controller: `relay state connector=65303ae3… relay=ad5966f0… reason=connected`; placement `s3-luffy → s3-relay-manoj` |

Same process throughout (pid 6321, `Relay selector starting` logged once): **no restart**. Before Fix 07, this needed a connector restart (B2 finding 3).

## Lab state after the run

- The connector `s3-relaylist` (k2-net) is **enrolled and active, process stopped**. Restart it with `~/.cache/zecurity-s3/run-conn.sh ~/.cache/zecurity-s3/connector-fix`. Revoke it in the admin UI when it's no longer needed.
- `pm-relay-manoj` is back to `inactive` (sweep).
- The controller `controller-head` (`bda9244`) is running with `RELAY_DEV_ALLOW_PRIVATE_ADDR=true`, log `~/.cache/zecurity-s3/controller-head.log`.
