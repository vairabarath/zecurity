---
type: fix-phase-acceptance
sprint: 20
fix: 5
phase: 5-A
title: Phase 5-A Live Acceptance — net_stack socket lifecycle
status: partial — completed in Phase5A-Live-Acceptance-2026-10-06
date: 2026-10-05
component: client
---

# Fix 05 · Phase 5-A — Live Acceptance (partial, 2026-10-05)

> Phase doc: [[Phase5A-NetStack-Socket-Lifecycle]] · Investigation: [[Fix05-Client-Dataplane-Reliability]]

**Stopped at 17:40:04 because nika (shield + test resource) went offline.** L2 and L5 pass. L1 has 30 of
60 min, all clean. L3 and the L4 connector part have not run.

## Lab

| Item | Value |
|---|---|
| Client | Fix 5-A build `e2e36c45` installed 17:09; PID 1764234; `zecurity0` ifindex 15; device `f0e30d8f`; ACL v29. Old build backed up at `~/s20-run/zecurity-client.p2a-30749eb9.bak` |
| Connectors | `s20p2a-conn-manoj2` (omarchy .49, systemd); `s20p2a-conn-inkyank` (archlinux .75, **hand-started**, PID 6305). The shield was on manoj2 |
| Shield / resource | `s20p2a-shield-nika` @ .39; summa `192.168.1.39:51711` (protected) |
| Instruments (`~/s20-run/`) | `f5_hold.py` (keep-alive, 2 s, cap 2 h), `f5-newconn.sh` (new conn every 2 s, `curl -m 8`), `f5-tunsamp.sh`, `f5-sampler.sh` (RSS + kernel socket states / 30 s), `f5-fastprobe.sh` (for L3, 1 s); logs `logs/f5-*.log`, markers `logs/f5-events.log` |

## Results

| Id | Result | Evidence |
|---|---|---|
| **L2** source-port reuse after 70 s | **PASS 5/5** | `f5-l2.log`: ports 47123/47555/47201/47202/47203 reused → all `200`, 0.012–0.020 s. Every reuse has a client `new TCP connection` + `tunnel opened` (4/4 in the last burst). The pre-fix build gave `000 8.00x` on the same probe (2/2) |
| **L1** 60 min, no failure trend | **PARTIAL (30 min, clean)** | 17:10:02–17:40:03: **886 × 200, 0 × 000**; max `time_total` 0.058 s. The pre-fix build's comparable window: 22 × `000 8.00x` in 34 min. RSS 16.9 → 18.6 MB (pre-fix: 188 MB after 3 h). 0 `restarting VPN`, 0 hot-applies, 0 `handshake timed out`. `tunsamp` printed only its start line |
| **L5** no socket accumulation | **PASS (for those 30 min)** | Stats line every minute: `active_relays=1 sockets=2` (the hold + the listener); `fin_on_relay_end == sockets_removed` (30 → 893); `backstop_aborts=0` |
| **L4** no regression | **PARTIAL** | The hold kept one socket (57520) for 897 requests (30 min), then `FAIL RemoteDisconnected` at 17:40:04 because of the shield loss, not the client. Connector loss/return hot-apply was **not run** |
| **L3** connector-offline fail-closed → RST < 1 s | **NOT RUN** | — |

**Side result (not L3):** after nika dropped, the connectors returned `SHIELD_NOT_ATTACHED` and the client
logged `no connector accepted tunnel (auth/denial only) — failing closed`. 86 new connections all failed
**fast** (median 0.011 s, max 0.030 s), not after 8 s. Stats after the loss: `rst_on_relay_failure=23`,
`active_relays=0 sockets=1`, so the relay-failure RST path works live and nothing leaked.

## To finish (2026-10-06)

See [[../../Handoff-Fix05-5A-2026-10-06]]. **Completed:** [[Phase5A-Live-Acceptance-2026-10-06]]
(L1, L3, L4, L5 pass; L2 from this run).
