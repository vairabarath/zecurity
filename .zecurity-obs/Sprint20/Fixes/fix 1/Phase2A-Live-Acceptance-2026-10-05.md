---
type: fix-phase-acceptance
sprint: 20
fix: 1
phase: 2-A
title: Phase 2-A Live Acceptance — connector topology hot-apply
status: live-accepted
date: 2026-10-05
component: client
depends_on: [2-A]
tags:
  - client
  - tunnel
  - live-acceptance
  - fix-01
---

# Fix 01 · Phase 2-A — Live Acceptance Record (2026-10-05)

> Phase doc: [[Phase2A-Connector-Topology-Hot-Apply]] · Parent: [[Fix01-Client-Tunnel-Restart-On-Snapshot-Change]] ·
> Playbook: [[../../Live-Lab-Testing-Playbook]] · Run sheet: [[../../Live-Acceptance-Run-Sheet]]

**Verdict: all four rows pass.** Rows A, B and C are TransportOnly: the client hot-applied the connector
map and the VPN never restarted. Row D is Structural (a full restart), so the negative control holds.
Row C's flow-loss expectation was **not observed** in the first session (the hold had expired). It
was **re-run and observed** in a second session the same afternoon (§4b), with a fresh hold, a 4-min
no-event baseline and a fresh client device.

Nothing is committed. Open, non-blocking items are in §9.

---

## 1. Lab as tested

### Session 1 (13:51 – 15:12)

| Component | Identity | Status |
|---|---|---|
| Client | Phase 2-A binary `30749eb9…`, device `204e0e06…`, workspace `xiyo` | running, `zecurity0` ifindex **12** |
| Connector | `s20p2a-conn-inkyank` `d7de9603` @ `192.168.1.75` (archlinux) | hand-started (no systemd unit); see rows |
| Connector | `s20p2a-conn-manoj` `337dd26e` @ `192.168.1.49` (omarchy) | hand-started; active until Row C |
| Shield | `s20p2a-shield-nika` `345079fd` @ `192.168.1.39` | active, bound to **manoj** |
| Resource | `s20p2a-nika-web` `f478542d` = summa `192.168.1.39:51711` | **protected**, assigned to `s20-summa-users` |
| ACL | 18 at start → 19 (A) → 20 (B) → 22 (C) → 24 (D) | 1 resource |

Direct LAN access to the resource is blocked (curl → `000`), so every success below went through the
tunnel.

### Session 1 instruments (started 13:51:56)

| Unit | Script | Log | What it samples |
|---|---|---|---|
| `p2a-hold` | `p2a_hold.py` (cap 60 min, 1 req / 2 s) | `p2a-hold.log` | one keep-alive TCP connection; any error → `FAIL` and exit |
| `p2a-hold2` | `p2a_hold.py` | `p2a-hold2.log` | replacement hold, started 14:56:26 (after Row C) |
| `p2a-newconn` | `p2a-newconn.sh` (`curl -m 8`) | `p2a-newconn.log` | a **new** TCP connection every 2 s |
| `p2a-tunsamp` | `p2a-tunsamp.sh` | `p2a-tunsamp.log` | change-only: `zecurity0` ifindex, client `MainPID`, `ip rule 0x5a` + `table 105` hash |
| `p2a-shieldsamp` | `p2a-shieldsamp.sh` | `p2a-shieldsamp.log` | change-only: connector statuses + the shield's connector |
| — | manual | `p2a-events.log` | row markers |

All under `~/s20-run/` (logs in `~/s20-run/logs/`). `tunsamp` is the anti-reconnect proof: any change
to ifindex, PID, policy routing or table 105 prints a new line. Over session 1 it printed exactly two
lines: `13:51:56 ifindex=12 pid=285512 …=aea051572c` and `15:07:34 ifindex=13 pid=285512 …=aea051572c`
(Row D).

### Pre-row failure baseline (session 1)

From 13:51:56 to the first kill at 14:25:24 the new-connection sampler logged **879 × 200 and 22 × 000**,
with no topology event. Every in-row `000` count below has to be read against that background rate
(~2.4 %). See §7.2.

---

## 2. Row A — connector removed while NOT used by any active flow (PASS)

**Action:** kill the inkyank connector process on `192.168.1.75`.
**Timeline:** `ROW-A-KILL 14:25:24` did nothing (see §7.1). The kill took effect later, and the
controller flipped inkyank to `disconnected` at 14:32:21 (`ROW-A-CONFIRMED 14:32:57`).

Controller / shield sampler:

```
14:32:21.640 s20p2a-conn-inkyank=disconnected s20p2a-conn-manoj=active | shield_via=s20p2a-conn-manoj
```

Client decision (journal, colours stripped):

```
09:02:33.990424Z  INFO daemon: ACL snapshot synced version=19 entries=1
09:02:34.000370Z  INFO daemon: transport snapshot stored version=7
09:02:34.000401Z  INFO daemon: background sync: version changed, restarting tunnel acl_changed=true transport_changed=true transport_pending=false
09:02:34.000454Z  INFO daemon: transport-only change, hot-applying connector map
09:02:34.002008Z  INFO daemon: transport map hot-applied successfully resources=1 reachable=1
```

| Assertion | Expected | Observed | |
|---|---|---|---|
| Classifier verdict | TransportOnly | `transport-only change, hot-applying connector map` | ✅ |
| Hot-apply succeeded | yes | `hot-applied successfully resources=1 reachable=1` (~1.6 ms) | ✅ |
| Full VPN restarts | 0 | **0**: no `restarting VPN`, no `zecurity0 up` | ✅ |
| TUN ifindex / PID / routing | unchanged | 12 / 285512 / `aea051572c` (`tunsamp` silent) | ✅ |
| Existing flow | survives | hold on port 53104 never failed (0 FAIL, same source port) | ✅ |
| New flows | succeed | 14:32:21–14:35:21: **62 × 200, 6 × 000** | ✅ |
| Shield | unaffected | stayed on manoj | ✅ |

The six `000`s in that window are about 9 %, above the 2.4 % baseline. They are not attributed: the
stale-pool cost (§7.2) can affect the first connections after a connector loss. New connections
recovered without a restart.

The ACL version also bumped (18 → 19) in the same sync. The controller re-issues the resource entry
when the connector set changes. The entry's routing fields (host, port, protocol) were unchanged, so
TransportOnly is the correct classification.

---

## 3. Row B — connector returns (PASS)

**Action:** restart the inkyank connector on `192.168.1.75` (hand-start recipe, §7.1).
**Timeline:** `ROW-B-START 14:34:23`; connector `active` again at 14:41:20 (`ROW-B-CONFIRMED 14:48:50`).

```
14:41:20.001 s20p2a-conn-inkyank=active s20p2a-conn-manoj=active | shield_via=s20p2a-conn-manoj
```

Client decision:

```
09:11:33.982935Z  INFO daemon: ACL snapshot synced version=20 entries=1
09:11:33.991939Z  INFO daemon: transport snapshot stored version=8
09:11:33.991980Z  INFO daemon: background sync: version changed, restarting tunnel acl_changed=true transport_changed=true transport_pending=false
09:11:33.992051Z  INFO daemon: transport-only change, hot-applying connector map
09:11:33.994395Z  INFO daemon: transport map hot-applied successfully resources=1 reachable=1
```

| Assertion | Expected | Observed | |
|---|---|---|---|
| Classifier verdict | TransportOnly | `transport-only change, hot-applying connector map` | ✅ |
| Hot-apply succeeded | yes | `hot-applied successfully` (~2.3 ms) | ✅ |
| Full VPN restarts | 0 | **0** | ✅ |
| TUN ifindex / PID / routing | unchanged | 12 / 285512 / `aea051572c` (`tunsamp` silent) | ✅ |
| Existing flow | survives | hold still on 53104, 0 FAIL, `req=1680` at 14:48 | ✅ |
| New flows | succeed | 14:41:20–14:45:20: **85 × 200, 7 × 000** | ✅ |

---

## 4. Row C — remove the connector the shield is bound to

### 4a. Session 1 (topology part PASS; flow-loss NOT OBSERVED)

**Action:** kill the manoj connector (the shield's own connector) on `192.168.1.49`.
**Timeline:** `ROW-C-START 14:52:39`. Connector `disconnected` by 14:54:54; **shield failed over to
inkyank at 14:55:05**; `ROW-C-CONFIRMED 14:56:00`.

```
14:54:54.266 s20p2a-conn-inkyank=active s20p2a-conn-manoj=disconnected | shield_via=s20p2a-conn-manoj
14:55:05.093 s20p2a-conn-inkyank=active s20p2a-conn-manoj=disconnected | shield_via=s20p2a-conn-inkyank
```

The client hit the 5-second stale-pool cost on the dead connector, then resynced and hot-applied:

```
09:24:59.356899Z  WARN net_stack: tunnel handshake timed out after 5s dest=192.168.1.39 port=51711
09:24:59.375599Z  WARN net_stack: shield not attached, trying next connector dest=192.168.1.39 port=51711
09:24:59.375662Z  WARN net_stack: QUIC relay ended error=no connector accepted tunnel for 192.168.1.39: 51711
09:24:59.375688Z  INFO daemon: relay failure signalled — early transport resync (background)
09:24:59.387926Z  INFO daemon: transport snapshot stored version=9
09:24:59.387967Z  INFO daemon: early transport resync: version changed, restarting tunnel
09:24:59.388033Z  INFO daemon: transport-only change, hot-applying connector map
09:24:59.390060Z  INFO daemon: transport map hot-applied successfully resources=1 reachable=1
09:25:33.982059Z  INFO daemon: ACL snapshot synced version=22 entries=1
09:25:33.984369Z  INFO daemon: background sync: version changed, restarting tunnel acl_changed=true transport_changed=false
09:25:33.984395Z  INFO daemon: transport-only change, hot-applying connector map
09:25:33.984840Z  INFO daemon: transport map hot-applied successfully resources=1 reachable=1
```

The second decision has `transport_changed=false` but is still TransportOnly. Only the ACL entry's
connector set changed (its routing fields did not), so a TransportOnly verdict is consistent with
D1–D5. Note "restarting tunnel" in the `background sync` line is the pre-existing generic wording. The
decision itself is on the following line.

| Assertion | Expected | Observed | |
|---|---|---|---|
| Shield failover | manoj → inkyank | `shield_via=s20p2a-conn-inkyank` at 14:55:05 | ✅ |
| Classifier verdict | TransportOnly | 2 × `transport-only change, hot-applying connector map` | ✅ |
| Hot-apply succeeded | yes | 2 × `hot-applied successfully` | ✅ |
| Full VPN restarts | 0 | **0** | ✅ |
| TUN ifindex / PID / routing | unchanged | 12 / 285512 / `aea051572c` (`tunsamp` silent) | ✅ |
| New flows around failover | succeed | 14:54:40–14:56:00: 23 × 200, 3 × 000 (two are 8 s timeouts, at 14:54:46 and 14:55:02); continuous 200 from 14:55:04 | ✅ |
| Existing flow on the removed connector | dies | **NOT OBSERVED.** The hold hit its 60-min cap at 14:51:58 (`DONE req=1790 … elapsed=3601s`), before the kill | ⚠️ → §4b |

### 4b. Session 2 re-run (PASS, flow-loss observed)

Re-run because §4a could not see the flow-loss case.

**Lab changes before the re-run** (all logged in `p2c-events.log`):

- The manoj connector's cert had expired at 15:05 (15-min TTL), so it could not rejoin. A new
  connector **`s20p2a-conn-manoj2` `64f825ab`** was enrolled on omarchy (`192.168.1.49`). It was
  created in the admin UI, the token was decoded and `connector_id` checked, and it was installed
  **under systemd** with `connector-local-install.sh`. `zecurity-connector-update.timer` is disabled,
  and its binary sha `970bfa02…` matches the bundle. The shield logged `peer connector list refreshed
  peers=2` at 15:28:59.
- The client's refresh session had died at 15:15:19 (`refresh session dead`, the same Google user as
  the admin UI). The admin preview was closed and the client re-logged-in as new device
  **`f0e30d8f`**. ACL v26, `zecurity0` ifindex **14**, client PID still **285512**, binary `30749eb9`.
  The re-login itself did a structural restart at 15:30:02 and a hot-apply at 15:30:19. Both happened
  **before** the instruments started.
- Shield bound to **inkyank** (since §4a). inkyank PID 6187 (root, hand-started).

**Instruments** (`p2c-*` units, all logs in `~/s20-run/logs/`): `p2c-newconn` (same script as session 1),
`p2c-tunsamp`, `p2c-shieldsamp` started 15:30:49. `p2c-hold` (`p2c_hold.py` = `p2a_hold.py` with a 3 h
cap) started 15:35:17 on local port **48410**: ESTAB, settled with `req=30 status=200` at 15:36:16.

**Baseline** 15:30:49–15:36:03 (no event): **145 × 200, 2 × 000**. No client decision lines.

**Action:** `ROW-C-KILL-SENT 15:36:03`; `sudo kill 6187` on inkyank completed at 15:36:20.397. `:9091`
was gone (`ss -lnt | grep -c 9091` → 0). inkyank was restarted at 15:37:44 (`ROW-C-RESTORE-SENT`,
listening again at 15:37:51), before its cert expiry at 15:43:35.

Shield sampler:

```
15:30:49.421 inkyank=active       manoj=disconnected manoj2=active | shield_via=s20p2a-conn-inkyank
15:36:19.581 inkyank=disconnected manoj=disconnected manoj2=active | shield_via=s20p2a-conn-inkyank
15:36:45.124 inkyank=disconnected manoj=disconnected manoj2=active | shield_via=s20p2a-conn-manoj2
15:37:45.990 inkyank=active       manoj=disconnected manoj2=active | shield_via=s20p2a-conn-manoj2
```

Hold:

```
15:35:17 START local_port=48410
15:36:16 req=30 status=200 bytes=612 port=48410
15:36:30 req=32 FAIL TimeoutError: timed out
```

Client (journal, colours stripped, `new TCP connection`/`tunnel opened` lines omitted):

```
10:06:18.747614Z  INFO net_stack: tunnel opened dest=192.168.1.39 port=51711        ← last open before the kill
10:06:25.782102Z  WARN net_stack: tunnel handshake timed out after 5s dest=192.168.1.39 port=51711
10:06:25.786133Z  WARN net_stack: shield not attached, trying next connector dest=192.168.1.39 port=51711
10:06:25.786150Z  WARN net_stack: QUIC relay ended error=no connector accepted tunnel for 192.168.1.39: 51711
10:06:25.786156Z  INFO daemon: relay failure signalled — early transport resync (background)
10:06:25.789016Z  INFO daemon: transport snapshot stored version=11
10:06:25.789025Z  INFO daemon: early transport resync: version changed, restarting tunnel
10:06:25.789045Z  INFO daemon: transport-only change, hot-applying connector map
10:06:25.789497Z  INFO daemon: transport map hot-applied successfully resources=1 reachable=1
10:06:30.814379Z  INFO net_stack: tunnel opened dest=192.168.1.39 port=51711        ← new flows back
10:07:19.290612Z  INFO daemon: ACL snapshot synced version=28 entries=1
10:07:19.300698Z  INFO daemon: transport-only change, hot-applying connector map
10:07:19.304702Z  INFO daemon: transport map hot-applied successfully resources=1 reachable=1
10:08:19.292094Z  INFO daemon: ACL snapshot synced version=29 entries=1                ← inkyank returned
10:08:19.305540Z  INFO daemon: transport snapshot stored version=12
10:08:19.305616Z  INFO daemon: transport-only change, hot-applying connector map
10:08:19.309485Z  INFO daemon: transport map hot-applied successfully resources=1 reachable=1
```

| Assertion | Expected | Observed | |
|---|---|---|---|
| Hold established before the change | yes | ESTAB on 48410 at 15:35:17, `req=30 200` at 15:36:16 | ✅ |
| **Existing flow on the removed connector** | **dies** | **`req=32 FAIL TimeoutError` at 15:36:30**, 10 s after the kill | ✅ |
| Shield failover | inkyank → manoj2 | `shield_via=s20p2a-conn-manoj2` at 15:36:45 | ✅ |
| Classifier verdict | TransportOnly | 3 × `transport-only change, hot-applying connector map` (resync v11, ACL v28, return v29/v12) | ✅ |
| Hot-apply succeeded | yes | 3 × `hot-applied successfully resources=1 reachable=1` | ✅ |
| Full VPN restarts | 0 | **0** `restarting VPN` / `zecurity0 up` after 15:30:49 | ✅ |
| TUN ifindex / PID / routing | unchanged | 14 / 285512 / `aea051572c`; `tunsamp` printed only its start line | ✅ |
| New flows recover | yes | `tunnel opened` from 10:06:30Z; newconn 15:36:03–15:37:30 **33 × 200, 2 × 000**; 15:37:30 onward 58 × 200, 0 × 000 at the time of writing | ✅ |
| Connector return (Row B repeat) | TransportOnly | v29 / transport v12 hot-applied, no restart | ✅ |

The two `000`s in the event window are 15:36:28 (an 8 s timeout, the stale-pool cost hitting a new
connection during the kill) and 15:37:19 (an 8 s timeout at the ACL v28 hot-apply second). With the
baseline at 2 / 147 these are weakly attributable. Neither is a restart.

The hold died with a `TimeoutError` (10 s socket timeout), not a reset. This is consistent with the
expectation: a flow whose connector disappears has no live path, and Phase 2-A does not migrate
in-flight flows (out of scope).

---

## 5. Row D — structural control: resource port change (PASS)

**Action:** edit resource `s20p2a-nika-web` in the admin UI, port `51711` → `51712`.
**Timeline:** `ROW-D-START 14:56:38`, `ROW-D-SAVED 15:07:06`. Hold 2 (port 32884, via inkyank) was
running since 14:56:26.

A first attempt via direct SQL **did not work**: the client logged
`ACL up_to_date — kept cached snapshot version=22`. A SQL write cannot trigger a client change; see
§7.3. **Row D must go through the admin UI or its GraphQL mutation.**

After the UI save:

```
09:37:33.981954Z  INFO daemon: ACL snapshot synced version=24 entries=1
09:37:33.984527Z  INFO daemon: background sync: version changed, restarting tunnel acl_changed=true transport_changed=false
09:37:33.984562Z  INFO daemon: structural configuration change, restarting VPN
09:37:34.027928Z  INFO daemon: zecurity0 down
09:37:34.052805Z  INFO daemon: zecurity0 up routes=1
09:37:34.052885Z  INFO net_stack: net_stack: smoltcp loop started resources=1
```

| Assertion | Expected | Observed | |
|---|---|---|---|
| Classifier verdict | Structural | `structural configuration change, restarting VPN` (the D5 log wording) | ✅ |
| TUN recreated | yes | `zecurity0 down` → `zecurity0 up routes=1` | ✅ |
| TUN ifindex | **changes** | **12 → 13** (`tunsamp` line at 15:07:34); PID unchanged 285512 | ✅ |
| Existing flow | dies | hold 2 on 32884: `req=333 FAIL ConnectionResetError` at 15:07:35 | ✅ |
| New flows | succeed | 15:07:35–15:12:50: **156 × 200, 0 × 000** | ✅ |

Between the save (15:07:06) and the restart (15:07:34) the sampler logged **14 × 000, all fast (<0.02 s)**.
From 09:37:01Z the client logged `QUIC relay ended error=tunnel denied: access denied` and `flow queue
full; closing TCP flow`. The controller had already moved the resource to 51712, so the remote side
denied tunnels to 51711. The client still held the old snapshot until its next sync (09:37:33Z, v24)
triggered the structural restart.

### Session 1 totals

Client journal 13:45–15:15: **4 hot-applies, 1 structural restart**, 0 unintended restarts. Session 2
(15:30:49 onward): **3 hot-applies, 0 restarts**.

---

## 6. Post-Row-D observations (open, not Phase 2-A)

**Port edit / revert inconsistency.** After the UI save the resource showed `UNPROTECTED`, status
`Failed` / `port not listening`. The port was then reverted to 51711 **via SQL**, which per §7.3 the
client never sees: the client's last change stayed ACL v24 (the 51712 save), followed only by
`ACL up_to_date — kept cached snapshot version=24`. Even so, new connections to `:51711` through the
tunnel kept returning 200 from 15:07:35 to 15:12:48. So either 51711 was still permitted on that
path, or the client/shield port enforcement does not follow the snapshot as expected. Not concluded.
By 15:14:59 the DB row had changed again (`protected`, `pending_action=apply`). At session 2 (ACL v26,
after re-login) it reads `protected` on 51711 and serves 200.

**Session 1 ended with the client logged out.** From 15:12:50 every new connection timed out
(`newconn=000 8.00x`, 55 in a row), and at 15:15:19 the client logged
`refresh session dead — user must sign in again`. Likely cause: the admin UI and the client were
signed in as the same Google user, so one rotates the shared `refresh:<user_id>` session and the
other loses it. Session 2 closed the admin preview before the client login.

---

## 7. Findings

### 7.1 The lab's session-1 connectors were NOT managed by systemd

This cost about 7 minutes on Row A and produced three misleading "still active" readings.

Both session-1 connectors were started **by hand** from `~/s20fc/cbundle` (inkyank PID 5187, manoj PID
71178, each listening on `:9091`). Consequences:

1. `pkill -f cbundle` matched nothing (exit 1), because the process is `/usr/local/bin/zecurity-connector`.
   `pgrep zecurity-connector` (18 chars) can't match either: `pgrep` without `-f` matches the 15-char
   `comm` name. Even `pkill -f "bin/zecurity-connector"` failed to kill manoj. Only `kill <pid>` worked.
2. `sudo systemctl stop zecurity-connector` reported `inactive` and changed nothing, because there was
   no unit. **`systemctl is-active` returning `inactive` is not proof the process is gone.**
3. `connector.service` on the controller box (`192.168.1.164`) is a *different*, pre-existing
   service ("Zero-Trust Connector"). Stopping it is a no-op for the lab.

**Rule:** confirm the process and its listener: `ps -eo pid,user,args | grep zecurity-connector | grep -v grep`,
`ss -lnt | grep 9091`, and `sudo ss -lntp | grep 9091` (prints the owning PID). Then `sudo kill <pid>`
and re-check `ss -lnt | grep -c 9091` → `0`. Session 2 did exactly this (6187 → 0 listeners in 1 s).

**Restart recipe** (needs `conn.env`; the bare binary gives `missing field 'controller_addr'`, and
running as the user gives `Permission denied (os error 13)`). Session 2 used this as
`/tmp/conn-start.sh` on inkyank, run with `sudo setsid nohup bash /tmp/conn-start.sh > /tmp/conn-restart.log 2>&1 < /dev/null &`:

```bash
#!/bin/bash
cd /home/<user>/s20fc/cbundle
set -a; . ./conn.env; set +a
export CONTROLLER_ADDR=192.168.1.164:9090 CONTROLLER_HTTP_ADDR=192.168.1.164:8080
exec /usr/local/bin/zecurity-connector
```

A hand-started connector must come back **before its cert expires** (15-min TTL in this lab). After
that it cannot rejoin (manoj, session 1), and you need a new enrollment.

### 7.2 The 5-second stale-pool cost, observed live in both Row C runs

```
tunnel handshake timed out after 5s dest=192.168.1.39 port=51711
shield not attached, trying next connector
QUIC relay ended error=no connector accepted tunnel for 192.168.1.39: 51711
relay failure signalled — early transport resync (background)
```

The dead connector's pooled connection was selected and burned 5 s before the client fell back. This
is the behaviour described in the Phase 1 investigation, now observed twice. Phase 2-A does not fix
it (out of scope) and correctly hot-applied rather than restarting.

**Background `000` rate, unexplained.** Session 1's sampler logged 121 × `000` in 2003 samples
(13:51:56–15:14:48). Of those, 55 are the post-15:12:50 logged-out tail (§6) and 14 are Row D's
pre-restart denials. The remaining ~52 include **22 before any row fired** (13:51:56–14:25:24,
879 × 200), mostly `000 8.00x` (curl's `-m 8` timeout). Session 2's no-event baseline was much
cleaner (2 / 147). So most of session 1's background timeouts happened with both connectors healthy
and are not explained by connector loss. Open question, §9.

### 7.3 A raw SQL write cannot drive a client-visible change

The snapshot version is `notifier.Version(workspaceID)` (`controller/internal/policy/compiler.go:294`),
which advances only on a controller-side event. Updating `resources` directly in Postgres left the
client logging `ACL up_to_date — kept cached snapshot version=22`. Any live-acceptance row that
needs the client to *see* a resource or ACL change must go through the admin UI or its GraphQL
mutation.

### 7.4 Environment notes (not product bugs)

- nika had to be rebooted before Protect would work: kernel 7.2.7 running with only 7.2.8 modules
  installed → nftables `netlink cache initialization failed`. Known playbook pitfall.
- The shield detail page showed `CONNECTED VIA s20p2a-conn-inkyank` while the token and the DB said
  manoj. Pre-existing label mismatch.
- Hermes preview pane: some admin dialogs freeze after the first `type`, and closing/reopening the
  **pane** recovers. In session 2 `drive_preview` refused all actions ("only takes actions in the
  session the user is looking at") until the user focused this chat. After that, the connector Add
  dialog worked end to end (type-ahead `p` for the network, then the name, then submit).
- Admin UI and client as the same Google user evict each other's refresh session (§6).

---

## 8. Raw evidence pointers

All paths under `~/s20-run/`.

| What | Where |
|---|---|
| Session 1 instruments | `logs/p2a-{hold,hold2,newconn,tunsamp,shieldsamp}.log`, `logs/p2a-events.log` |
| Session 1 markers | `13:55:26 ROW-A-START`, `14:24:34 ROW-A-START`, `14:25:24 ROW-A-KILL`, `14:32:57 ROW-A-CONFIRMED`, `14:34:23 ROW-B-START`, `14:48:50 ROW-B-CONFIRMED`, `14:52:39 ROW-C-START`, `14:56:00 ROW-C-CONFIRMED`, `14:56:38 ROW-D-START`, `15:07:06 ROW-D-SAVED` |
| Session 2 instruments | `logs/p2c-{hold,newconn,tunsamp,shieldsamp}.log`, `logs/p2c-events.log`; tally `./p2c-tally.sh <since> [until]` |
| Session 2 markers | `15:30:49 BASELINE-START`, `15:35:10 BASELINE-END`, `15:35:57 HOLD-SETTLED`, `15:36:03 ROW-C-KILL-SENT`, `15:37:44 ROW-C-RESTORE-SENT` |
| Client decisions | `journalctl -u zecurity-client --since 13:45 -o cat \| sed 's/\x1b\[[0-9;]*m//g' \| grep -E 'hot-applied\|structural\|restarting VPN\|zecurity0 (up\|down)'` |
| Hold 1 (via manoj) | `p2a-hold.log`: 1790 requests, 0 FAIL, `DONE elapsed=3601s` at 14:51:58 |
| Hold 2 (via inkyank) | `p2a-hold2.log`: started 14:56:26, **after** Row C's hot-applies (0 hot-applies in its window), then `req=333 FAIL ConnectionResetError` at the Row D restart |
| Hold 3 (via inkyank, session 2) | `p2c-hold.log`: `req=32 FAIL TimeoutError` at 15:36:30 (Row C re-run) |
| Scripts | `p2a_hold.py`, `p2c_hold.py`, `p2a-newconn.sh`, `p2c-newconn.sh`, `p2a-tunsamp.sh`, `p2a-shieldsamp.sh`, `p2c-shieldsamp.sh`, `p2c-tally.sh` |

---

## 9. Open items (do not block Phase 2-A)

1. **Session 1's background per-connection timeouts** (§7.2) with both connectors healthy.
2. **Port edit/revert inconsistency** (§6): 51711 kept serving while the client held the 51712 snapshot.
   Look at the client's per-resource port matching and the shield's nft state.
3. **Lab state to clean up:** `s20p2a-conn-manoj` (expired, `disconnected`) can be revoked.
   `s20p2a-conn-manoj2` is the live systemd-managed connector on omarchy. inkyank is still hand-started.
   The old client device `204e0e06` is superseded by `f0e30d8f`. The `p2c-*` user units are still running.
