---
title: Zero-resource tunnel startup — live acceptance run
date: 2026-10-09
status: live-accepted (Z0–Z3); code uncommitted
spec: "[[Phase5C-Resource-Hot-Apply]] § Zero-resource startup"
---

# Zero-resource tunnel startup: live acceptance, 2026-10-09

Requirement: the VPN/TUN starts when the device has no assigned resources; zero resources is a valid
tunnel state (empty mapping), logged clearly, and later assignments are applied without a restart.
Run window: 11:50–12:54 IST. All rows pass. The code is **uncommitted** on `feat/sprint20-m1-phase2`
(`client/src/daemon.rs`, `client/src/cmd/up.rs`).

## 1. Lab inventory

| Item | Value |
|---|---|
| Branch / HEAD | `feat/sprint20-m1-phase2` @ `5fe4e1c` (5-C committed); zero-resource change uncommitted |
| Client OLD (Z0) | `/usr/local/bin/zecurity-client` sha `0f3adbbd1ec7be3a…` (5-C build); backup `~/s20-run/backup/zecurity-client.5C.0f3adbbd` |
| Client NEW (Z1–Z3) | sha `7d2da41544389150…` (release build of the change); `strings` contains `no resources currently assigned to this device`. Installed 11:54:19, MainPID **188850** for every row (NRestarts 0) |
| Client user / device | `pv638418@gmail.com` (member; the admin UI used `sathiyaseelank326`, so no session eviction), device `03cb5cf7` |
| Controller | `~/s20-run/controller-f5c` sha `36892b48` (same as the 5-C run), restarted 11:49:39 after the 09:49 lab reboot; connector TTL 15m |
| Connector | `s20z-conn-75` `2d7f1b16` on .75 (archlinux), RN `s20f5-rn2`, sha `970bfa02`, update timer disabled/inactive. Enrolled 11:57 for this run (the old `inkyank` cert had expired) |
| Resource | `s20f5-rn2-web` `0d9132b3` 192.168.1.75:51712, connector-routed (journal Vite app) |
| Access | group `s20-summa-users` (members: pv + admin). At start: **0 access rules** → ACL `entries=0` |
| Client poll tick | `hh:mm:19.18`; every UI click fired at `hh:mm:22` |

## 2. Instruments

| Unit | Log | Samples |
|---|---|---|
| `fz-tunsamp` (user) | `~/s20-run/logs/fz-tunsamp.log` | change-only 500 ms: `zecurity0` ifindex, client MainPID, fwmark-0x5a rule count, table-105 route count, rules+table hash |
| `fz-nc-B` (user) | `~/s20-run/logs/fz-newconn.log` | new connection every 2 s to 192.168.1.75:51712: code, `time_total`, rc. Direct LAN ≈ 1–7 ms; tunnel ≈ 15–20 ms |
| event log | `~/s20-run/logs/fz-events.log` | row markers |
| tally | `~/s20-run/fz-snap.sh <since> [until]` | window counts + client key lines |

## 3. Rows

### Z0: OLD build, zero resources → `up` (baseline)

```
11:50:47 zecurity-client up
Error: ACL snapshot has no entries — no resources to route      (rc=1)
Device "zecurity0" does not exist.
```

| Assertion | Observed | |
|---|---|---|
| Before-fix behaviour reproduced | refusal, no TUN | ✅ (baseline) |

### Z1: NEW build, zero resources → `up`

```
11:54:36 zecurity-client up
Zecurity is up. No resources are currently assigned to this device.   (rc=0)
06:24:36.033609Z INFO zecurity0 up routes=0
06:24:36.033621Z INFO no resources currently assigned to this device; tunnel up, waiting for resource assignment acl_entries=0
06:24:36.033656Z INFO net_stack: smoltcp loop started resources=0
```

| Assertion | Observed | |
|---|---|---|
| tunnel starts at zero | rc=0; `zecurity0` ifindex 12, `100.64.0.1/32` | ✅ |
| clear log | the line above | ✅ |
| CLI says so | `Zecurity is up. No resources are currently assigned to this device.` | ✅ |
| empty mapping | 0 fwmark rules, table 105 absent (startup only clears stale policy) | ✅ |
| session | `sync` OK; 0 `session expired`/`refresh session dead` | ✅ |

### Z2: first assignment to a tunnel that started empty

Assign rn2-web clicked ~12:00:22 → controller `acl push version=2 entries=1` 12:00:24.

```
06:31:19.180502Z ACL snapshot synced version=2 entries=1
06:31:19.192298Z resource change, hot-applying removed=0 added=1 pending=0
06:31:19.209274Z resources hot-applied resources=1 removed=0 added=1 pending=0
06:31:20.651344Z new TCP connection dest=192.168.1.75 port=51712 → tunnel opened
tunsamp 12:01:19.466 ifindex=12 pid=188850 fwmark_rules=1 t105_routes=1
```

| Assertion | Observed | |
|---|---|---|
| applied at the next tick, no restart | 1 hot-apply, 0 `restarting VPN`, 0 `recovering with full restart` | ✅ |
| same TUN / daemon | ifindex 12, pid 188850 unchanged | ✅ |
| policy routing created by the apply | fwmark 0→1, table 105 0→1 route | ✅ |
| traffic through the tunnel | probe 12:00:00–12:01:19: 39/39 × 200 direct (~6 ms). From 12:01:20: 12/12 × 200 at 15–20 ms, 12 `new TCP connection` + 12 `tunnel opened` (1:1) | ✅ |

### Z3: recovery restart while zero resources are desired (FU-1 / H3)

Fault injection: `sudo ip route del 192.168.1.75/32 table 105` at 12:49:07.166 (tunsamp
`t105_routes=0` 12:49:07.222; probes go direct). Unassign rn2-web clicked ~12:50:23 → `acl push version=3
entries=0` 12:50:33. At the next tick the removal side fails at the route delete, so the client takes the
full-restart recovery with zero desired resources: the exact path that used to leave the VPN down.

```
07:21:19.183584Z ACL snapshot synced version=3 entries=0
07:21:19.193182Z resource change, hot-applying removed=1 added=0 pending=0
RTNETLINK answers: No such process
07:21:19.212838Z WARN resource apply failed after mutation, recovering with full restart error=delete resource routes
07:21:19.263500Z zecurity0 down
07:21:19.286674Z zecurity0 up routes=0
07:21:19.286698Z no resources currently assigned to this device; tunnel up, waiting for resource assignment acl_entries=0
07:21:19.286739Z net_stack: smoltcp loop started resources=0
tunsamp 12:51:19.302 ifindex=ABSENT … 12:51:19.849 ifindex=13 pid=188850 fwmark_rules=0 t105_routes=0
```

| Assertion | Observed | |
|---|---|---|
| recovery path exercised | `resource apply failed after mutation, recovering with full restart` | ✅ |
| VPN comes back with zero resources | `zecurity0 down` → `up routes=0` in 23 ms (74 ms after the failure); ifindex 12 → 13; pid unchanged | ✅ |
| clear log | `no resources currently assigned …` | ✅ |
| no stray traffic | probes 12:49:07–12:53:19: all 200 direct 1–3 ms (only the 4 probes before 12:49:07 were tunnel 16–19 ms) | ✅ |

**Z3 tail: still ready after recovery.** Re-assign clicked ~12:52:23 → `acl push version=4 entries=1`
12:52:25.

```
07:23:19.185904Z ACL snapshot synced version=4 entries=1
07:23:19.197163Z resource change, hot-applying removed=0 added=1 pending=0
07:23:19.215079Z resources hot-applied resources=1 removed=0 added=1 pending=0
tunsamp 12:53:19.381 ifindex=13 pid=188850 fwmark_rules=1 t105_routes=1
```

Probes from 12:53:20: 200 at 14–18 ms (tunnel); 0 restarts.

## 4. Counts over the whole run (11:54:19–12:53:45, NEW build)

`restarting VPN` 0 · `recovering with full restart` 1 (Z3, induced) · `resource change, hot-applying` 3 ·
`resources hot-applied` 2 (the Z3 one failed by design) · `no resources currently assigned` 2 (Z1, Z3) ·
`session expired|refresh session dead|sync failed` 0 · non-200 probes 0.

## 5. Observations (not graded)

- O-1: `zecurity-client status` prints `ACL: loaded (no policies configured for this workspace)` at zero.
  The workspace has resources; none are assigned to this device. Pre-existing status wording, cosmetic.
- O-2: controller JWKS fetch timed out once on the first admin sign-in (`fetch jwks … context deadline
  exceeded`); the retry worked. Environment, not product.

## 6. Not covered

- `allowed > 0, routable = 0` startup (e.g. UDP-only assignment) is unit-tested
  (`up_preflight_accepts_no_routable_entry`) but not run live: there is no UDP resource in the lab.
- Daemon auto-start at boot with zero resources was not exercised; `up` was invoked by hand, as in every
  earlier run.
