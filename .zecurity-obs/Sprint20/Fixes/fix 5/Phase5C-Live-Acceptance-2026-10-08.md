---
type: live-acceptance
sprint: 20
fix: 5
phase: 5-C
title: Phase 5-C Resource Hot-Apply — live acceptance run
date: 2026-10-08
status: L6-reopened
spec: "[[Phase5C-Resource-Hot-Apply]]"
---

# Phase 5-C Resource Hot-Apply: live acceptance, 2026-10-08

Spec: [[Phase5C-Resource-Hot-Apply]] §H (L1–L15). Run window: 15:20–16:48 IST.

**Result.** 14 of 15 criteria pass. **L6 is REOPENED (§6a)**: two of its
assertions were satisfied by other components (connector ACL diff, shield) before the client acted.
None of the findings is a 5-C client defect. Across the whole run:

- 14 `resource change, hot-applying` → 14 `resources hot-applied`;
- 0 apply failures (pre- or post-mutation);
- exactly 2 `structural configuration change, restarting VPN`, both from the controls (row 9a RN move,
  row 9b identity);
- 0 `session expired` / `refresh session dead`.

The phase stays `implemented-pending-live` until the §6a.4 re-run passes.

## 1. Lab inventory

| Item | Value |
|---|---|
| Branch / HEAD | `feat/sprint20-m1-phase2` @ `86cda74`; 5-C **uncommitted** in `client/src/{daemon,net_stack,runtime,tun}.rs` |
| Client binary | `/usr/local/bin/zecurity-client` sha `0f3adbbd1ec7be3a…` (5-C release build); MainPID 473963 for the whole run; 5-B backup `~/s20-run/backup/zecurity-client.5B.1f960fe2` |
| Client user / device | `pv638418@gmail.com` (member, invited for this run so admin UI ≠ client user); device `810b8c9e` → `03cb5cf7` after row 9b |
| Controller | `~/s20-run/controller-f5c` sha `36892b48` (empty-network build from 10-06). HEAD rebuild `controller-f5c2` fails at start with `PROVIDER_JWT_SECRET is not set` (provider code from the fixed-pendings merge); 5-C is client-only so the controller build is not under test. TTLs: connector 15m, shield 168h |
| Workspace | `xiyo`; admin UI as `sathiyaseelank326@gmail.com` |
| RNs | `palace` `6ebeb898`, `s20f5-rn2` `cf520c20` |
| Connectors (sha `970bfa02`) | `manoj` `72eb3056` (.49, palace), `udaya` `dfb5290f` (.37, palace), `inkyank` `e92fb862` (.75, s20f5-rn2) |
| Shield | `s20f5c-shield-34` `c0c8f822`, host `seechan5` .34, bound to `manoj` |
| Resource A | `s20f5c-A` `9fc95e47` 192.168.1.34:51721, protected (shield), palace — admin vite |
| Resource A2 | `s20f5c-A2` `f440bceb` 192.168.1.34:51722 → **51723** (row 2), protected, palace |
| Resource B | `s20f5-rn2-web` `0d9132b3` 192.168.1.75:51712, unprotected, s20f5-rn2 (journal app) |
| Access | group `s20-summa-users` (members: pv + admin) |
| Capacity | smoltcp `IFACE_MAX_ADDR_COUNT=2` → one resource IP (one slot is 100.64.0.1). A second IP must go pending (H1). |

## 2. Instruments (`~/s20-run`, prefix `f5c5-*`)

| Unit | Log | Samples |
|---|---|---|
| `f5c5-tunsamp` (user) | `logs/f5c5-tunsamp.log` | change-only 500 ms: `zecurity0` ifindex, client MainPID, `fwmark 0x5a` rule count, table-105 route count, hash of rules+table 105 |
| `f5c5-nftsamp` (root) | `logs/f5c5-nftsamp.log` | change-only 500 ms: hash + rule count of `nft list table inet zecurity_client`, rule text |
| `f5c5-nc-{A,A2,A2n,B}` | `logs/f5c5-newconn.log` | new connection every 2 s, `curl -m 8`: code, `time_total`, rc. Tunnel ≈ 15–25 ms, direct ≈ 3–6 ms |
| `f5c5-hold-*` | `logs/f5c5-hold-*.log` (dead ones in `logs/dead/`) | `f5c5_hold.py`: one keep-alive socket, GET every 2 s, FAIL + exit on error |
| `f5c5-snap.sh <since>` | `logs/f5c5-row*.txt` | per-row tally: 5-C log strings, restarts, newconn per target, tunsamp/nft windows, holds |
| `f5c5-q5.sh` (root, one-shot) | `logs/f5c5-q5.log` | L13/L14 |
| event markers | `logs/f5c5-events.log` | every click/stop/start with ms timestamp |

Client poll tick: `hh:mm:02.6`. Every UI action was fired at `:06` so exactly one tick follows it.

**Baseline** (15:20:16): ifindex 12, pid 473963, fwmark rules 1, table-105 1 route, nft 1 rule
`ip daddr 192.168.1.34 tcp dport 51721` (hash `8a2f155b2ab3`).

## 3. Rows

Row times are IST, journal lines UTC (IST − 5:30).

### Row 1: add A2 (same IP, new port). L2, L5

Assign A2 at 15:22:25.9 → push `version=38 entries=2` → tick:

```
09:53:02.646657Z resource change, hot-applying removed=0 added=1 pending=0
09:53:02.666526Z resources hot-applied resources=2 removed=0 added=1 pending=0
```

| Assertion | Observed | |
|---|---|---|
| hot-apply, 0 restarts | as above; restarts 0 | ✅ |
| ifindex constant (L2) | tunsamp no change (12) | ✅ |
| A2 reachable (L5) | A2 `000 8.00` (shield drops direct) until the tick, then `200 7.19` (in-flight SYN), then 21× 200 med 19 ms; 13 client `new TCP connection … 51722` | ✅ |
| A hold 0 FAIL (L5) | `hold-A` req=60 at 15:23:03, same socket | ✅ |
| nft (L4) | 15:23:02.984 → 2 rules (51721, 51722) | ✅ |
| L12 | `entries=2` ↔ `resources=2 pending=0` | ✅ |

### Row 2: A2 port 51722 → 51723. L9

Hold on A2:51722 from 15:24:30. Saved 15:26:28.6 → push `version=40 entries=2` → tick:

```
09:57:02.653233Z resource change, hot-applying removed=1 added=1 pending=0
09:57:02.689756Z resources hot-applied resources=2 removed=1 added=1 pending=0
```

| Assertion | Observed | |
|---|---|---|
| old-port hold reset by the client | `req=77 FAIL ConnectionResetError` at 15:27:03.319, the first request after the apply. The connector did **not** cut it (sessions keyed on resource_id, unchanged by a port edit). The reset is the client's per-key remove (`net_stack.rs:812-814`; ack only after reset + reap) | ✅ |
| new port OK | A2new ~5 ms direct before the tick → 15–22 ms after; `tunnel opened … port=51723` from 09:57:03.33Z | ✅ |
| 0 restarts, TUN untouched | 0; tunsamp no change | ✅ |
| A unaffected | hold-A req=180 at 15:27:04; A 42/42 200 | ✅ |
| nft | `5614a9117ebd` → `3cf26ea0ed90`, 2 rules (51721, 51723) | ✅ |

Delivery lag (expected): between the save and the tick, new :51722 connections still entered the tunnel and
were denied by the connector (`tunnel denied: access denied`), and the client reset them in ~10 ms
(rc 56). That is denial, not a leak.

### Row 3: remove A2. L6 — ⚠ partial → REOPENED (§6a)

Hold on A2:51723 from 15:28:46. Unassigned 15:30:04.2 → push `version=41 entries=1` 15:30:03 → tick:

```
10:01:02.648205Z resource change, hot-applying removed=1 added=0 pending=0
10:01:02.663527Z resources hot-applied resources=1 removed=1 added=0 pending=0
```

| Assertion | Observed | |
|---|---|---|
| hot-apply, 0 restarts, TUN untouched | as above; tunsamp no change | ✅ |
| nft back to the expected set | 1 rule, hash `8a2f155b2ab3` = baseline, byte-identical | ✅ |
| removed resource's hold reset | Hold died 15:30:04.558 `RemoteDisconnected`, caused by the **connector**'s `ACL diff: session cancelled — authorization revoked mid-session` (15 sessions, 10:00:03.921Z), 58 s **before** the client applied. The client's remove-reset had no live flow to act on in this row. Row 2 already shows the client per-key reset working. | ⚠ |
| new connects refused | Before the tick: connector-denied, ~10 ms RST. After the apply the key is no longer captured, so :51723 goes direct. It answered **200 direct** until 15:31:29, when the shield applied the row-2 port edit late (F-2); after that it was `000 8.00` (shield drop). | ⚠ (F-2) |
| other flows survive | hold-A req=300, 0 FAIL; A 40/40 200 | ✅ |
| L12 | `entries=1` ↔ `resources=1 pending=0` | ✅ |

### Row 4: add B (second IP .75). L11

Assigned 15:55:16.9 → push `version=42 entries=2` → tick:

```
10:26:02.629893Z WARN resource additions pending: address capacity pending=["192.168.1.75:51712"]
10:27:02.647579Z background sync: … resource_pending=true      (retried every tick)
```

| Assertion | Observed | |
|---|---|---|
| pending warning, logged once per distinct set | 1 WARN, then `resource_pending=true` retries every tick, no repeat WARN (rate limit, `daemon.rs` `log_capacity_pending`) | ✅ |
| no silent loss | B not captured (nft unchanged, 1 rule), stays reachable direct ~3 ms; never half-captured | ✅ |
| other state applied / unchanged | A unaffected, hold-A req=1050 at 15:56:15 | ✅ |
| 0 restarts | 0; tunsamp no change | ✅ |
| no publish when nothing applicable | no `hot-applying` / `hot-applied` line on this row | ✅ |
| ascending-IP admission | see row 6 (B admitted when the slot freed) | ✅ |

### Row 5: resource + connector change in one snapshot. L10

**Attempt 1 (16:00) is VOID.** User clicks in the admin pane produced v43 (15:59:37, assign A2) and v44
(16:00:05) before the planned actions, so the resource delta landed a tick early. Confirmed with the
user; recorded in the event log.

**Attempt 2.** Udaya stop sent 16:15:06.3 → `version=54 connectors=2`; Unassign A2 16:15:07.0 →
`version=55 entries=2`; both before the 16:16:02 tick:

```
10:46:02.635599Z ACL snapshot synced version=55 entries=2
10:46:02.643997Z transport snapshot stored version=33
10:46:02.644040Z background sync: … acl_changed=true transport_changed=true
10:46:02.646827Z resource change, hot-applying removed=1 added=0 pending=1
10:46:02.664758Z resources hot-applied resources=1 removed=1 added=0 pending=1
```

| Assertion | Observed | |
|---|---|---|
| one sync carries both deltas | ACL v55 + transport v33 in the same tick | ✅ |
| one apply, one publication; no separate transport-only apply | single hot-apply (18 ms); 0 `transport-only change` lines | ✅ |
| order in the journal | synced → transport stored → classify → hot-applying → hot-applied (matches §C) | ✅ |
| 0 restarts | 0; tunsamp no change | ✅ |
| nft | 1 rule, `8a2f155b2ab3` | ✅ |
| L12 | snapshot entries=2 (A, B) ↔ `resources=1 pending=1` | ✅ |
| other flows survive | newconn A 34/34 200. The A hold was not running (died in ENV-1). | ⚠ env, not graded |

Udaya restarted 16:16:11 (cert valid to 16:12:55 + renewals; DB `active`).

### Row 6: remove A → B admitted on the same tick (H1)

Unassigned A 16:19:07.6 → `version=57 entries=1`:

```
10:50:02.631302Z resource change, hot-applying removed=1 added=1 pending=0
10:50:02.663055Z resources hot-applied resources=1 removed=1 added=1 pending=0
```

| Assertion | Observed | |
|---|---|---|
| removal frees capacity; B admitted same tick | `removed=1 added=1 pending=0` | ✅ |
| B through the tunnel | B ~3 ms direct → 15–25 ms; `tunnel opened dest=192.168.1.75` from 10:50:04.63Z | ✅ |
| A hold reset | `req=68 FAIL RemoteDisconnected` 16:19:09 (connector ACL diff again, 53 s before the client) | ✅ (expected) |
| 0 restarts, ifindex 12 | yes; table-105 hash changed (route .34 → .75), as expected | ✅ |
| nft | 16:20:02.662 0 rules → 16:20:03.194 1 rule `.75:51712` (F-4: by design) | ✅ |

### Row 7: remove B → zero resources. L3, L7

Hold on B (tunnelled, rtt 14 ms) from 16:20:49. Unassigned 16:22:07.6 → `version=58 entries=0`:

```
10:53:02.656457Z resource change, hot-applying removed=1 added=0 pending=0
10:53:02.675135Z resources hot-applied resources=0 removed=1 added=0 pending=0
10:53:02.675154Z no routable resources; tunnel kept up, nothing captured
```

| Assertion | Observed | |
|---|---|---|
| 0 restarts | 0 | ✅ |
| nft rule count 0 | 16:23:02.969 `nft_rules=0` (table + chain kept, hash `8e72d3e8b101`) | ✅ |
| table 105 empty | tunsamp `t105_routes=0`; `ip route show table 105` empty | ✅ |
| tunnel up | `zecurity0 … UP,LOWER_UP`, ifindex 12 | ✅ |
| fwmark count 1 (L3) | `fwmark_rules=1` | ✅ |
| B hold | `FAIL RemoteDisconnected` 16:22:07.652 (connector ACL diff); new B connects ~10 ms RST until the tick, then 200 direct ~3 ms (B is unprotected) | ✅ |

### Row 8: re-add A from zero. L8

Assigned 16:25:07.7 → `version=59 entries=1`:

```
10:56:02.655781Z resource change, hot-applying removed=0 added=1 pending=0
10:56:02.684092Z resources hot-applied resources=1 removed=0 added=1 pending=0
```

| Assertion | Observed | |
|---|---|---|
| reachable | A `000 8.00` (shield drop) → `200` 15–20 ms from 16:26:03.9 | ✅ |
| 0 restarts, same ifindex | 0; ifindex 12; tunsamp hash back to baseline `6ec33c5c92` | ✅ |
| nft | `8a2f155b2ab3` = baseline | ✅ |

### Row 9: structural controls. L1

**9a, attempt 1 (16:28) is INVALID.** It moved the RN of B while B was **pending** (capacity). A pending
entry is not in `AppliedConfig`, so the classifier correctly saw no D3 move (`daemon.rs:906-933`): no
restart and no apply. That is correct behaviour, but it isn't the control.

**9a, corrected.** Unassign A (16:31:06.8, v63) → B admitted via palace (`removed=1 added=1
pending=0`, ifindex 12). Hold on B from 16:32:11. Move B palace → s20f5-rn2 at 16:33:07.4 (v65):

```
11:04:02.640217Z structural configuration change, restarting VPN
11:04:02.681886Z zecurity0 down
11:04:02.719206Z zecurity0 up routes=1
11:04:02.719369Z net_stack: smoltcp loop started resources=1
```

ifindex 12 → 13; B hold `FAIL ConnectionResetError` at 16:34:04.6. Exactly 1 restart. ✅

**9b, identity.** Fresh `zecurity-client login` as pv at 16:34:58 → new device `03cb5cf7`:

```
11:05:01.367233Z structural configuration change, restarting VPN
11:05:01.411047Z zecurity0 down
11:05:01.447320Z zecurity0 up routes=1
```

tunsamp: 16:35:01.442 `ifindex=ABSENT fwmark_rules=0` → 16:35:01.996 `ifindex=14 fwmark_rules=1`. The B
hold was reset. Exactly 1 restart. ✅ (The momentary fwmark 0 is inside the restart, not on a hot-apply row.)

**L1 overall:** every resource row 0; RN control 1; identity control 1. Run total 2. ✅

### Row 10: Q5/Q7 kernel checks. L13, L14, L15

**L15 (Q7).** With B unassigned (v67, nft 0 rules), a direct keep-alive hold to .75:51712 was opened at
16:44:27 (local port 50956, rtt 3 ms). B was re-assigned at 16:46:03.4 (v68); tick 16:47:02.65
`hot-applying removed=0 added=1`. The hold's first request after the rule landed failed:
`req=79 FAIL ConnectionResetError` at 16:47:03.419. The `new TCP connection` lines after the apply
(11:17:04.05, 06.08, 08.11, …) match the newconn sampler 1:1 (16:47:04.069, 06.098, 08.123, …), so no
connection was opened for the existing socket. It was reset, not adopted (`smoltcp` non-SYN → `rst_reply`,
investigation §15 P5/P6). ✅

**L13 (Q5 abort).** `sudo f5c5-q5.sh` on the live table, which held 1 rule, `.75:51712`. A desired-state
batch (`add table`/`add chain`/`flush chain`/re-add rule) whose last line references a nonexistent set:

```
16:47:48.571 L13 nft_rc=1 hash_before=b52adfa1deee hash_after=b52adfa1deee PASS
    err: /tmp/f5c5-l13.nft:5:47-56: Error: No such file or directory
```

The flush in the same batch was not committed. Atomicity is confirmed on kernel 7.0.0-34. ✅

**L14 (Q5 no-gap).** 150 valid flush + refill batches of the identical rule set, 0.1 s apart
(16:47:51.6–16:48:09.2), under a 0.2 s new-connection probe to B:

```
16:48:09.181 L14 end batch_failures=0 hash_end=b52adfa1deee
16:48:14.224 L14 probes=108 non200=0 slow>0.5s=0 PASS
```

Probes went through the tunnel: 13–21 ms, med 17 ms, 94 client `new TCP connection` lines in the window.
A tunnelled B keep-alive hold opened at 16:47:26 survived the whole test (req=30 200 at 16:48:25).
nftsamp saw no intermediate state. The final hash equals the start hash. ✅

## 4. Criteria summary

| ID | Result | Evidence |
|---|---|---|
| L1 | ✅ | rows 1–8, 10: 0 restarts; 9a 1; 9b 1; run total 2 |
| L2 | ✅ | ifindex 12 through rows 1–8 and the start of 9a; 14 after 9b through row 10 |
| L3 | ✅ | `fwmark_rules=1` on every hot-apply row, including zero (row 7, row 10 L15 start) |
| L4 | ✅ | nftsamp/tunsamp after every row equal the expected set (row tables) |
| L5 | ✅ | row 1, row 8 |
| L6 | **OPEN** | row 3: apply/survival ✅; "its hold reset" and "new connects refused" were satisfied by connector/shield, not observed from the client (row 2 shows the client reset). Re-run: §6a.4 |
| L7 | ✅ | row 7 |
| L8 | ✅ | row 8 |
| L9 | ✅ | row 2 |
| L10 | ✅ | row 5 attempt 2 |
| L11 | ✅ | row 4 + row 6 |
| L12 | ✅ | every apply line matched its snapshot entries (applied + pending) |
| L13 | ✅ | row 10 |
| L14 | ✅ | row 10 |
| L15 | ✅ | row 10 |

## 5. Findings

| # | Component | Severity | Finding | Evidence | Follow-up |
|---|---|---|---|---|---|
| F-1 | client | cosmetic | `background sync: version changed, restarting tunnel` is logged before every hot-apply and every pending retry, so the journal says "restarting" when nothing restarts | every row | rename the line (e.g. `background sync: change detected`); out of 5-C scope |
| F-2 | shield / controller (frozen) | **security-relevant** | A resource port edit reached the shield 5 m 01 s late (save 15:26:28 → shield `resource snapshot applied … generation=4` 15:31:29). For that window the new port :51723 was reachable on the LAN without shield protection. Earlier shield snapshots were rejected as `ignoring stale resource snapshot generation=3 last_applied=3`. | shield journal; newconn A2new 200 direct ~5 ms until 15:31:29 | own Fix doc; investigate the generation bump on resource Update |
| F-3 | connector (frozen) | low | A port edit doesn't revoke existing connector sessions (keyed by resource_id), so only the client's per-key reset (5-C) cuts an old-port flow | row 2 | note for the connector ACL-diff design |
| F-4 | client | none (by design) | On a remove + add apply, nft is committed twice: remove side `flows_after_removal`, then add side last (H2, `daemon.rs:1404-1446`). Row 6 shows a ~0.5 s 0-rule state; only the keys being removed or not yet admitted are affected | nftsamp 16:20:02.662 → 16:20:03.194 | none |
| ENV-1 | lab | — | Controller host `enp2s0` link down 16:06:44 → 16:10:46. All agents disconnected; hold A died (TimeoutError). The client kept TUN, PID and its cached snapshots (`background ACL sync failed — keeping cached snapshot`), and the next sync at 16:12:02 found nothing to apply. 0 restarts. | NetworkManager journal; tunsamp no change | none; incidental robustness evidence |
| N-1 | run | — | Row 5 attempt 1 voided by user pane clicks (v43/v44); row 9a attempt 1 invalid (RN move on a pending resource) | event log | none |

Instrument caveat: `f5c5-snap.sh`'s `sync_bad` also matches `background ACL sync failed`, so ENV-1
inflates it. Session health was clean throughout (0 `session expired`, 0 `refresh session dead`).

## 6. NOT RUN / remaining

- **L6: REOPENED (2026-10-08, after review)** — see §6a. Not passed until the re-run in §6a.4 shows the
  client's own reset on a pure removal.
- FU-1…FU-4: out of scope (spec §J).
- Q6 network-move hot-apply: not part of this phase (spec §H).

## 6a. L6 reopened: function map and race analysis

### 6a.1 Client removal path (the code under test)

| Step | Function | Location |
|---|---|---|
| Poll tick → decision | `run_restart_decision` → `ConfigDelta::ResourceDelta` arm | `client/src/daemon.rs:1487`, `:1557` |
| Classify | `classify_applied` → `resource_delta_allowed` | `daemon.rs:885`, `:906` |
| Plan | `plan_resource_apply`: `removed = applied − candidate`, `flows_after_removal = kept`, `addr_del` | `daemon.rs:1185-1232` |
| Apply (H2 order) | `hot_apply_resources`: `ResourceCmd::Remove` → `apply_nft_desired(flows_after_removal)` → `RemoveAddrs` → `del_routes` → one map publish → add side | `daemon.rs:1325`, mutation `:1402-1450` |
| Bounded ack (H4, 2 s) | `send_resource_cmd` | `daemon.rs:1269` |
| Loop dispatch | `apply_resource_cmd` → `ResourceCmd::Remove` | `client/src/net_stack.rs:803`, `:812` |
| **R1 reset** | `FlowTable::remove_listeners_and_close`: drop the listener; `socket.abort()` (**RST**) on every live socket whose local endpoint is a removed key; tag it | `net_stack.rs:555-602` |
| Ack after reset + reap | `close_pending` / `complete_pending_closes` | `net_stack.rs:605`, `:831` |
| nft | `apply_nft_desired` → `render_nft_batch` (`nft -f -`) | `client/src/tun.rs:303`, `:49` |
| Unit coverage | `remove_resets_only_affected_flows_and_acks_after_reap`, `remove_with_no_live_flows_acks_immediately` | `net_stack.rs:2435`, `:2500` |

### 6a.2 Connector path that pre-empts it

| Step | Function | Location |
|---|---|---|
| Push received | `CBody::AclSnapshot` handler | `connector/src/control_stream.rs:635-648` |
| Diff | `PolicyCache::update_and_revoked` (previous allow set − new, keyed `(spiffe, resource_id)`) / `allow_set` | `connector/src/policy/mod.rs:40-47`, `:139` |
| Cancel | `SessionRegistry::cancel_all` → `token.cancel()` | `connector/src/session_registry.rs:103` |
| Session exit | `cancel_token.cancelled()` arm logs `authorization revoked mid-session`, returns `Ok(())` | `connector/src/device_tunnel.rs:353`, `:481`, `:555` |
| Client side of that exit | relay outcome `RELAY_ENDED_OK` → `socket.close()` → **FIN** (counted `fin_on_relay_end`) | `net_stack.rs:472-486` |

### 6a.3 Why row 3 could not prove L6

1. **Timing.** The connector acts at push time over the control stream (≤ 1 s). The client acts at its next
   60 s poll. For any access removal the connector closes the flow 0–60 s before
   `remove_listeners_and_close` runs; in row 3 the gap was 58 s, so R1 found no socket on the key.
2. **FIN vs RST tells which side closed it.** Connector close → clean FIN → hold sees `RemoteDisconnected`
   (rows 3, 6, 7). Client R1 → RST → hold sees `ConnectionResetError` (row 2, where a port edit keeps the
   resource_id, so the connector doesn't revoke).
3. **"New connects refused" isn't a client function after the apply.** Before the tick the connector denies
   tunnelled connects and the client resets them (`rst_on_relay_failure`, ~10 ms). After the apply the key
   isn't captured, so traffic goes direct. Refusal then depends on the shield: protected A2 was dropped
   (after F-2's delay); unprotected B answered 200 direct (row 7). Treat this assertion as "no longer
   tunnelled" for the client, and as shield-dependent for direct access.

Row 2 already shows R1 live, but on a port change, not a pure removal.

### 6a.4 Re-run to close L6 (not run; needs approval + sudo on .49)

Keep the connector from receiving the push while its data plane stays up:

1. Hold on A2 (shield-routed via `manoj` .49, the shield's connector).
2. On .49, a self-reverting nft rule drops tcp 9090 to the controller for ~90 s. QUIC 9092 stays up.
3. Unassign A2 at `hh:mm:06`.
4. Expected at the next client tick: `hot-applying removed=1`, the hold fails `ConnectionResetError` (RST),
   and the .49 journal has no `ACL diff: session cancelled` for that session in the window.
5. After the rule expires the connector reconnects (the controller marks it disconnected ~20 s in, so a
   transport change rides along; Phase 2-A keeps existing flows across the map swap), diffs, and finds
   nothing left to cancel.

Pass = RST on the hold + `hot-applied … removed=1` + 0 restarts + no connector cancel for that session.

## 7. Lab state left behind

- Group `s20-summa-users` → B only. A, A2 unassigned (A2 on 51723).
- Samplers run until ~19:20; `f5c5-nftsamp` is a root unit. The B tunnel hold is running.
- vites on .34 (51721–51723) and the journal app on .75:51712 are running.
- Nothing committed. Clean-up (tokens, vites, stale agents, client binary) pending approval.
