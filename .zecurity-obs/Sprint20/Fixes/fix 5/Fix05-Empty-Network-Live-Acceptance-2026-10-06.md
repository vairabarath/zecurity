---
type: fix-phase-acceptance
sprint: 20
fix: 5
title: Empty-network transport delivery — Live Acceptance
status: live-accepted-with-caveat (D1, D2, D4 pass; D3 recorded; D5 at-rest pass, partial-loss row not run)
date: 2026-10-06
component: controller (transport plane)
---

# Fix 05 · Empty-network transport delivery — Live Acceptance (2026-10-06)

> Phase doc: [[Fix05-Empty-Network-Transport-Delivery]] · Investigation:
> [[Fix05-Connector-State-Delivery-Latency-Investigation]].

**Result:** after a remote network's last connector stops, the client hot-applies "no connector"
**5.0–6.0 s** later, every time. That is at the first early resync, with no wait for the 60 s ACL poll,
and at all three poll offsets (+4.6 / +24.6 / +44.6 s). Before the fix the same event took 48.9 s and
ended at the poll. The other remote network served 803/803 new connections with no failure. 0 tunnel
restarts.

## Lab

| Item | Value |
|---|---|
| Controller | `s20-controller` restarted 15:11:48 on `~/s20-run/controller-f5c` (sha `36892b48`, built from the working tree with the fix; no TEMP relay patch). Same env: `CONNECTOR_CERT_TTL=15m`, `CONNECTOR_RENEWAL_WINDOW=10m`, `SHIELD_CERT_TTL=168h`, `RELAY_CERT_TTL=1h`. Previous binary `controller-fixconn` (`3e52e994`) kept |
| RN `palace` | **one** connector `s20f5-conn-manoj` `162319a6` (omarchy .49); shield `s20f5-shield-nika` on it; resource `s20f5-nika-web` 192.168.1.39:51711 (summa, shield-protected) |
| RN `s20f5-rn2` (new, `cf520c20`) | connector `s20f5-conn-rn2` `56de96a1` (archlinux .75, the same host/bundle, re-enrolled from `s20f5-conn-inkyank`); resource `s20f5-rn2-web` 192.168.1.75:51712, connector-routed (no shield) = React/Vite app `~/Projects/journal-management-main/client`; access rule `s20-summa-users`; ACL v4 `entries=2` |
| Client | build `e2e36c45` (Fix 5-A), PID 2379 throughout. Re-logged in 15:41:07 (new device `841fa6e5`) → one expected structural restart (`zecurity0` ifindex 23 → 24). ACL poll phase afterwards `hh:mm:58.4` |
| Instruments | `f5c-probe.sh` (1 s, a new connection to **each** resource, `curl -m 8`) → `logs/f5c-probe.log`; `f5-tunsamp.sh` → `logs/f5c-tunsamp.log`; stop/start schedule `f5c-dsched.sh` run as root on omarchy (stop at second :03/:23/:43, 60 s down, start, 150 s gap); analysis `f5c-analyze.sh`, `f5c-summary.sh`; markers `logs/f5-events.log` |

## D1 + D2 + D4 — last-connector removal at three poll offsets

Lag = client `hot-applied … reachable=1` (2 resources, palace now unreachable) − controller
`connector … disconnected`. The controller log has 1 s resolution.

| Iter | Stop (controller `disconnected`) | Offset after poll (`hh:mm:58.4`) | `relay failure signalled` → `transport snapshot stored` → `transport-only change` → **`hot-applied reachable=1`** | **Lag** | Next poll (would have been the pre-fix apply) | Poll result | `effective config unchanged` between resync and apply |
|---|---|---|---|---|---|---|---|
| A | 15:46:03 | +4.6 s | 15:46:09.088 → 15:46:09.107 (v11) | **6.1 s** | 15:46:58 | ACL v14 → `effective config unchanged` (already applied) | 0 |
| B | 15:50:23 | +24.6 s | 15:50:28.231 → 15:50:28.250 (v13) | **5.3 s** | 15:50:58 | same | 0 |
| C | 15:54:43 | +44.6 s | 15:54:48.384 → 15:54:48.405 (v15) | **5.4 s** | 15:54:58 | same | 0 |
| pilot | 15:22:03 | +5 s (old session, poll :57.97) | 15:22:08 (v5) | **~5 s** | 15:22:57 | same | 0 |

Pre-fix reference (5-A L3, same mechanism): 13:48:09 disconnect → resync at 13:48:14 logged `effective
config unchanged` → applied only at the 13:48:57.97 poll = **48.9 s**.

| Assertion | A | B | C |
|---|---|---|---|
| Controller detects at once (`disconnected` at the stop second) | ✅ | ✅ | ✅ |
| Client receives the updated list in its next connector resync | ✅ v11 | ✅ v13 | ✅ v15 |
| Network represented as zero connectors (`reachable` 2 → 1, palace slot `None`) | ✅ | ✅ | ✅ |
| Applied without waiting for the 60 s poll (lag ≤ 10 s, poll had no new effect) | ✅ 6.1 s | ✅ 5.3 s | ✅ 5.4 s |
| Fail closed after apply: palace `000` in < 1 s | ✅ first fast `000` 15:46:09.150 | ✅ 15:50:28.264 | ✅ 15:54:48.436 |
| **D4** RN2 unaffected | ✅ | ✅ | ✅ RN2 **803 × 200, 0 × 000, 0 slow** over 15:45:00–15:58:30 |
| 0 `restarting VPN`, ifindex/PID unchanged | ✅ ifindex 24, PID 2379, policy hash `ab6fe7c3ab` constant from 15:41:08 | | |

**Slow probes before the apply (5-B evidence, not counted against this fix):** exactly 5 per iteration.
These are new connections already in flight between the stop and the resync, each paying the 5 s
`TUNNEL_HANDSHAKE_TIMEOUT` on the stale pooled QUIC connection (`handshake timed out` × 5 per iteration).
The resync itself is triggered by the first of them failing. That is why the lag is ≈ 5 s plus a few ms:
it equals the handshake timeout.

## D3 — connector return

| Iter | Connector started (controller `connected`) | Client hot-applied `reachable=2` | Lag | Shield `Control stream established` | First palace `200` after return |
|---|---|---|---|---|---|
| A | 15:47:03 | 15:47:58.449 | 55 s | 15:48:03 | 15:48:04.105 |
| B | 15:51:23 | 15:51:58.449 | 35 s | 15:52:23 | 15:52:24.240 |
| C | 15:55:43 | 15:55:58.426 | 15 s | 15:56:43 | 15:56:44.455 |

- The client recognized each return at its **next 60 s poll**, not at a connector resync. That is
  expected and unchanged by this fix: there is no client trigger for "connector returned" (no push, and
  the early resync fires only on failure). This was documented in the phase doc §6 and is not in scope.
- New connections used the restored connector as soon as the shield re-attached. The shield retries
  every 60 s (`backoff_secs=60`), so it re-attached exactly 60 s after each return, about 1 s
  before the first `200`. Until then the client failed closed fast (`shield not attached` /
  `auth/denial only`). Recovery time is set by the shield's back-off (5-A Finding 2), not by this fix.

## D5 — no regression

| Check | Result |
|---|---|
| At rest, both networks, 15:58:38–16:08:15 (9.6 min) | **palace 575 × 200, rn2 575 × 200, 0 × 000**, max 0.023 s; 1150 client `new TCP connection`; 0 `restarting VPN`, 0 hot-applies, 0 sync failures; `backstop_aborts=0` |
| Baseline before the D rows (15:19:49–15:22:03) | palace 66 × 200, rn2 66 × 200 |
| Single-connector removal inside an RN that keeps another connector (L4-style) | **NOT RUN live.** This topology has one connector per RN. The code path is unchanged by this fix: RNs with active connectors are built from the same rows in the same order (pinned by `TestAssemble_RNWithConnectorsUnchanged` and the existing DB subtests), and the 5-A L4 row (same client) already showed it hot-applies. Re-run when an RN with two connectors is available |

## Invalidated attempt (kept for the record)

The first schedule (15:22–15:34) lost iterations 2 and 3: the client's refresh session died at
15:24:57 (`refresh session dead — user must sign in again`). The cause was the admin-UI sign-in used to
set up RN2, made as the same Google user (known lab hazard). The client could not fetch anything, so no
resync applied. The pilot iteration at 15:22:03 had finished before that and is valid. The whole
schedule was re-run on a fresh client session (15:45–15:58); the table above uses only that run plus
the pilot.

## Findings

| # | Observation | Class |
|---|---|---|
| 1 | Loss is applied at the first resync (≈ the 5 s handshake timeout); the 60 s poll no longer decides it | fix works as designed |
| 2 | Exactly 5 slow (5 s) new connections per loss, before the resync | 5-B (stale QUIC pool); unchanged by this fix |
| 3 | Connector return is poll-bound (15–55 s) and new flows also wait for the shield's 60 s back-off | expected / out of scope (no push channel; shield back-off = 5-A Finding 2) |
| 4 | Admin-UI sign-in evicts the client session (same Google user) | environment / known lab hazard |

## Not touched

`client/src/net_stack.rs` (Fix 5-A), client `daemon.rs`/`runtime.rs` (Phase 2-A), QUIC pools (5-B),
connector code, routes/TUN/nft. Nothing committed.
