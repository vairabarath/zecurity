---
type: fix-phase-acceptance
sprint: 20
fix: 5
phase: 5-A
title: Phase 5-A Live Acceptance — net_stack socket lifecycle (completion run)
status: live-accepted
date: 2026-10-06
component: client
---

# Fix 05 · Phase 5-A — Live Acceptance (2026-10-06)

> Phase doc: [[Phase5A-NetStack-Socket-Lifecycle]] · Investigation: [[Fix05-Client-Dataplane-Reliability]]
> Previous partial run: [[Phase5A-Live-Acceptance-2026-10-05]] (L2 PASS there; L1/L4/L5 partial, L3 not run).

**Result: L1–L5 all pass.** L1 ran the full 60 min on one net_stack instance with 0 failures. L3
fail-closed answers with a reset in ≤ 8 ms. L4 connector loss and return hot-apply with no tunnel restart.
L2 is carried over from the 2026-10-05 run on the same build (`e2e36c45`). Two open observations
are outside 5-A's scope; they are recorded below and do not block acceptance.

## Lab rebuild (before the rows)

The 10-05 agents had expired (15-min connector certs), so the lab was re-enrolled from scratch:

| Item | Value |
|---|---|
| Controller | `s20-controller` user unit on 192.168.1.164, `CONNECTOR_CERT_TTL=15m`, `SHIELD_CERT_TTL=168h` |
| Client | Fix 5-A build sha `e2e36c45…` (`/usr/local/bin/zecurity-client`), PID 2379, `zecurity0` ifindex 14, device `29e5d6ec`. Re-logged in 12:06:57 (the admin UI had evicted the refresh session); 0 `sync failed` afterwards |
| Connectors | `s20f5-conn-manoj` `162319a6` (omarchy .49) and `s20f5-conn-inkyank` `5aa40833` (archlinux .75). Both systemd, binary `970bfa02…` (bundle SHA256SUMS OK), update timers `disabled`/`inactive`. Enrolled 11:53 |
| Shield | `s20f5-shield-nika` `453ba26b` (nika .39), bundle sha OK, update timer disabled. Token `connector_id` = `5aa40833` (inkyank) = DB `shields.connector_id`; the UI's "Connected via manoj" label was wrong (known quirk). Enrolled 11:59:58 |
| Resource | `s20f5-nika-web` `8d65d5f5` = summa `192.168.1.39:51711` TCP, `protected` 12:04:53; access rule `s20-summa-users`; ACL push v9 `entries=1 connectors=2` |
| Old objects | `s20p2a-shield-nika` revoked; `s20p2a-nika-web` deleted (see Finding 3). Old `s20p2a-conn-*` rows are still `disconnected` (clean-up) |
| nika | `sleep.target`/`suspend.target` masked during the run (the screen had blanked; the journal shows no suspend after 11:00, and shield status reports had no gap > 20 s) |
| Instruments (`~/s20-run/`) | `f5-newconn.sh` (new conn / 2 s, `curl -m 8`), `f5-sampler.sh` (RSS + kernel sockets / 30 s), `f5-tunsamp.sh` (change-only ifindex/PID/policy hash, 500 ms), `f5_hold.py` (keep-alive, 2 s), `f5-fastprobe.sh` (L3, 1 s). Snapshot: `f5-snap.sh`; L3 analysis: `f5-l3-analyze.sh`. Logs `logs/f5-*.log` (10-05 logs moved to `logs/f5-2026-10-05/`), markers `logs/f5-events.log` |

## Results

| Id | Result | Summary |
|---|---|---|
| **L1** | **PASS** | 60 min, 1768 × 200, **0 × 000**, no trend; RSS +1.9 MB |
| **L2** | **PASS** (2026-10-05, same build) | 5/5 source-port reuse → 200 via the tunnel |
| **L3** | **PASS** | both connectors down: 58/58 probes `000` in < 1 s (max 8 ms), `rst_fail_closed=88`, 0 restarts |
| **L4** | **PASS** | hold survived 90 min; connector loss + return each hot-applied, 0 `restarting VPN`, ifindex/PID unchanged, shield failed over |
| **L5** | **PASS** | `active_relays` = 1 (hold) then 0; `backstop_aborts=0`; every socket removed |

### L1 + L5 — 60 min on one net_stack instance

Stack restarted (`zecurity-client down && up`) at 12:09:33 (`smoltcp loop started resources=1`); instruments
started 12:10:13, hold 12:10:16 (local port 37546). Window **12:10:13–13:10:13**.

| Assertion | Expected | Observed |
|---|---|---|
| new-connection failures | ≤ 1/70 `000 8.00x`, no trend with stack age | **1768 × 200, 0 × 000.** Per 10 min: 288 / 295 / 294 / 295 / 295 / 295 × 200, 0 × 000 each. Max `time_total` 0.107 s (once, 12:33:29); typical ≈ 0.02 s |
| traffic went through the tunnel | client `new TCP connection` per sample | 1778 lines over the window (sampler + hold start + probes) |
| RSS flat (± 5 MB) | | 18 104 → 19 984 kB (+1.9 MB); 19.3–20.0 MB from minute 10 on |
| data plane untouched | | `tunsamp` printed only its start line: ifindex 14, PID 2379, policy hash `aea051572c` |
| client decisions | none needed | 0 `restarting VPN`, 0 hot-applies, 0 `handshake timed out`, 0 `sync failed` |
| L5 stats | `active_relays` ~1 (the hold), no accumulation | every minute `active_relays=1 sockets=2`, `fin_on_relay_end == sockets_removed` (1752 = 1752 at 13:09:33), `rst_on_relay_failure=0`, `backstop_aborts=0` |
| hold | same socket, no FAIL | port 37546, req 1770 all 200 at 13:09:36 |

About 22 connector cert renewals (`ReEnroll sent`, both connectors every ~5 min) happened inside the
window; none caused a client action.

Before the fix (for comparison): 22 × `000 8.00x` in 34 min, RSS 188 MB after 3 h. The 10-05 run had 30
clean minutes.

### L4 — connector loss and return (shield's connector)

The shield was on `s20f5-conn-inkyank`, so that connector was stopped (not manoj2 as the handoff
assumed). Instruments restarted 13:39:50; baseline 35 × 200 in 60 s.

| Time | Event |
|---|---|
| ~13:41:18 | `sudo systemctl stop zecurity-connector` on .75 (sudo journal) |
| 13:41:29–13:41:50 | 4 new connections take 5.0 s (`tunnel handshake timed out after 5s` on the stale pooled connection), then succeed: all `200` |
| 13:41:33 | hold `FAIL TimeoutError` at req 2718: its flow was on inkyank |
| 13:41:53 | `relay failure signalled — early transport resync` → `transport-only change, hot-applying connector map` → `hot-applied successfully resources=1 reachable=1` (v3); second hot-apply 13:41:57 |
| — | shield failed over to `s20f5-conn-manoj` (DB) |
| 13:43:05 | inkyank restarted, new cert, `Control stream established` |
| 13:43:57 | client hot-applies v4 |

| Assertion | Result |
|---|---|
| connector loss hot-applies, 0 `restarting VPN` | ✅ |
| connector return hot-applies, 0 `restarting VPN` | ✅ |
| ifindex / PID / routing unchanged | ✅ ifindex 14, PID 2379, hash `aea051572c` throughout |
| new connections recover | ✅ 136 × 200, **0 × 000** (13:39:50–13:44:35); 4 × 5.0 s during the loss |
| long-lived flow survives the run | ✅ 90 min / 2700+ requests on one socket, through L1 and every renewal. It ended only when its own connector was stopped in this row, which is the expected loss for that flow (relay-failure path), not a regression |
| L5 after the row | `active_relays=0 sockets=1`, `rst_on_relay_failure=1` (the hold), `backstop_aborts=0`, removed 2343 = 2342 FIN + 1 RST |

### L3 — fail-closed with both connectors offline

`f5-fastprobe.sh 300` started 13:47:34. inkyank stopped 13:47:50, manoj 13:48:09 (controller
`disconnected`).

| Window | Probes | Meaning |
|---|---|---|
| 13:48:15–13:48:42 | 6 × `000` slow (5 × 5.0 s, 1 × 2.0 s) | transition: the client's map still had a reachable connector; each probe paid the 5 s QUIC handshake timeout on the stale pool (same as L4), then `QUIC relay ended … no connector accepted tunnel` |
| 13:48:42–13:48:57 | `000` < 1 s | `failed to reach connector (transport), trying next` → RST |
| 13:48:57 | — | background sync hot-applies `reachable=0` |
| **13:48:58–13:49:57** | **58 / 58 `000` < 1 s, max 8 ms, rc 56 (reset)** | `connector offline — failing closed for managed resource` (121 lines in total) — **the L3 criterion** |
| 13:49:28–29 | — | both connectors restarted (`connected`), fresh certs |
| 13:49:57–13:50:19 | 22 × `000` < 1 s | client hot-applies `reachable=1`, but the shield is not attached yet → `no connector accepted tunnel (auth/denial only) — failing closed, no resync`; still a fast reset |
| 13:50:19.8 | — | shield `Control stream established` to manoj |
| **13:50:20** | first `200` | recovered |

One probe (13:48:59) returned rc 55 (send failure on a reset race) in 2 ms; it is still fail-closed.

| Assertion | Result |
|---|---|
| curl `000` in < 1 s (reset), not 8 s | ✅ every probe after the client learned both connectors were gone; max 8 ms |
| client log `connector offline — failing closed` | ✅ |
| `rst_fail_closed > 0` | ✅ `rst_fail_closed=88`, `rst_on_relay_failure=67`, `backstop_aborts=0`, `sockets_removed=2642` |
| no tunnel restart | ✅ 0 `restarting VPN`; ifindex/PID unchanged; 3 hot-applies |
| recovery | ✅ first 200 one second after the shield re-attached |

## Findings / observations

| # | Observation | Class | Evidence | Follow-up |
|---|---|---|---|---|
| 1 | **Detection lag after the last connector drops.** At 13:48:14 an early resync stored transport v6 and logged `effective config unchanged, keeping tunnel`, although manoj had disconnected at 13:48:09. `reachable=0` arrived only with the 13:48:57 background sync (ACL v15). Until then new connections paid a 5 s handshake timeout | insufficient evidence (controller transport snapshot timing; outside 5-A) | client journal 13:48:14 / 13:48:57; controller `disconnected` 13:47:50 / 13:48:09 | Investigate whether the v6 snapshot still listed manoj, and why the 13:48:45 relay-failure resync did not change it. Candidate for 5-B / transport work |
| 2 | **Recovery bounded by the shield's reconnect back-off.** The shield retried its connector at 20 s → 40 s → 60 s; the connectors were back at 13:49:28, but the shield only reconnected at 13:50:19 | expected shield behaviour (not client) | shield journal `backoff_secs=20/40/60`, `Control stream established` 08:20:19Z | Consider capping or resetting the shield back-off when a peer-list refresh arrives. Out of scope for 5-A |
| 3 | **Admin UI cannot Protect a resource whose shield was replaced.** After the old shield is revoked and the resource is unprotected, `shield_id` is NULL; `ResourceDetail.tsx` `canProtect` requires the *bound* shield to be active, but the backend `MarkProtecting` (`controller/internal/resource/store.go:476`) re-matches by `lan_ip` | product (admin UI) | DB `shield_id` NULL after Unprotect; Protect button disabled | Workaround used: delete and re-create the resource, Protect, re-assign the access rule. UI fix later |
| 4 | Connector startup logged `initial CRL fetch failed … CRL thisUpdate is in the future` (13:43:05, inkyank); the clocks agree within 0.3 s | environment / timing edge | connector journal; `timedatectl` synced | Note only; the control stream came up normally |

## Remaining

- Clean-up (at run end): revoke the old `s20p2a-conn-*` connectors; delete `~/s20-run/f5-*.env` and the
  `conn.env`/`shield.env` copies on .49/.75/.39; unmask `sleep.target`/`suspend.target` on nika if wanted.
- Commit only after approval.
