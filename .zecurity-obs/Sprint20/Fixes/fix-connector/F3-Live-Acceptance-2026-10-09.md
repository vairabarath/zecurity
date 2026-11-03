---
type: live-acceptance
sprint: 20
fix: connector-session-tuple-revocation
title: "F-3 connector tuple revocation — live acceptance run"
date: 2026-10-09
status: pass
spec: "[[Fix-Connector-Session-Tuple-Revocation]]"
---

# F-3 connector tuple revocation: live acceptance, 2026-10-09

Spec: [[Fix-Connector-Session-Tuple-Revocation]] §7 (L1–L8). Run window: 16:58–17:30 IST.
Path under test: **Client → Zecurity tunnel → Connector → Shield → Internal application**.

**Result: L1–L8 all pass.** The F-3 connector cancelled every old-tuple session within ~1 s of the ACL
push, including while the client was cut off from the controller (L3). Name-only edits and same-content
saves cancelled nothing. Unrelated sessions survived. 0 client restarts across the whole run.

## 1. Lab inventory

| Item | Value |
|---|---|
| Branch / HEAD | `feat/sprint20-m1-phase2` @ `eac891a`; F-3 **uncommitted** in `connector/src/{policy/mod.rs,control_stream.rs}` |
| Connector | `s20z-conn-75` `2d7f1b16`, host .75 (archlinux), RN `s20f5-rn2`. Binary swapped in place to the F-3 release build sha `19e36cee64c4a4fc` (prior build kept at `/usr/local/bin/zecurity-connector.pre-f3`, sha `970bfa02`). Update timer disabled. |
| Shield | **new** `s20f3-shield-75` `4c920665`, host .75, bound to `s20z-conn-75` (JWT `connector_id` checked), sha `30ba3e39` (branch build) |
| Resource under test | `s20f3-web` `7736aba1` 192.168.1.75, **protected (shield route)**; port 51712 → 51714 (Row 1) → 51712 (L3); renamed `s20f3-web-renamed` (L7) |
| Unrelated resource | `s20f3-unrel` `261a549d` 192.168.1.75:51713, unprotected (**connector route**); unassigned in L8 |
| Apps | journal-app vite on .75 :51712 (existing), :51713 and :51714 (started for this run, `/tmp/f3-app-*.log`) |
| Client | controller host .164, `pv638418@gmail.com`, device `03cb5cf7`, PID 188850 the whole run, client sha `7d2da415` (zero-resource build); runs as user `inkyank-01` in `system.slice/zecurity-client.service` |
| Controller | `~/s20-run/controller-f5c` (unit `s20-controller`) |
| Admin UI | `sathiyaseelank326@gmail.com` (≠ client user) |
| Access | group `s20-summa-users` (2 members) |

Lab rebuild this run: the 10-08 lab was cold (manoj/udaya/inkyank connectors and the .34 shield
expired). `s20f5-rn2-web` had no shield binding (UI Protect disabled), so it was deleted and re-created
as `s20f3-web` (auto-matched the new shield), protected, and assigned.

## 2. Instruments (`~/s20-run`, logs in `logs/f3/`)

| Unit / script | Log | What |
|---|---|---|
| `f3-tunsamp` (`f5c5-tunsamp.sh`) | `f3-tunsamp.log` | change-only: zecurity0 ifindex, client PID, fwmark rules, table 105 |
| `f3-nc-old/new/unrel` (`f5c5-newconn.sh`) | `f3-newconn.log` | new connection every 2 s to :51712 / :51714 / :51713, `curl -m 8`; tunnel ≈ 15–20 ms, direct ≈ 2–4 ms |
| `f3-hold-<L>` (`f5c5_hold.py`) | `f3-hold-<L>.log` | one keep-alive socket, GET every 2 s, exits on the first failure. `RemoteDisconnected` = FIN (connector ended the relay), `ConnectionResetError` = RST (client per-key reset) |
| `f3-client-block.sh` (root) | `f3-block.log` | L3: self-reverting nft drop of the client's traffic to 192.168.1.164 tcp 8080/9090, matched by `socket cgroupv2 level 2 "system.slice/zecurity-client.service"` |
| `f3-conn.sh`, `f3-snap.sh` | — | connector/shield journal on .75; client tallies |
| event markers | `f3-events.log` | "SAVE-CLICKED" markers are written after the UI call returned; the authoritative event time is the controller `acl push` line |

Client tick: `hh:mm:19.18` until L3; `hh:mm:49.99` after the L3 recovery (the hung sync moved the phase).

## 3. Rows

### Row 1: port 51712 → 51714 (L1, L2, L4, L5, L6)

Holds: WEB on :51712 (shield route, `tunnel_opened ok shield=4c920665…`), UNREL on :51713 (connector route).

**Attempt 1 VOID (17:04).** The save went out with the port unchanged: `EditResourceModal` re-initialises
its form from `resource` in a `useEffect([resource])`, and the detail page refetches on every shield ack
(~15 s), which overwrote the typed value (Observation O-1). The save still produced a **same-content**
push `version=19` (17:04:27). The connector stored it with **no ACL diff**, and hold WEB kept running, so
this is an incidental negative control.

**Attempt 2** (port typed and saved within ~2 s):

```
controller  17:06:21  acl push … version=21 entries=2 connectors=1
connector   11:36:21.434769Z ACL diff: session cancelled spiffe_id=…/client/03cb5cf7… (1 live session: the hold)
connector   11:36:21.434832Z ACL diff: revoked sessions torn down count=16
connector   11:36:21.434849Z session cancelled — authorization revoked mid-session
connector   11:36:21.479681Z WARN access denied … (new :51712, every 2 s from here)
hold-WEB    17:06:22.367 req=127 FAIL RemoteDisconnected: Remote end closed connection without response
client      11:37:19.189500Z resource change, hot-applying removed=1 added=1 pending=0
client      11:37:19.239229Z resources hot-applied resources=2 removed=1 added=1 pending=0
```

`count=16` is the number of revoked `(spiffe, resource_id)` keys: every device SPIFFE of the group
members that the old snapshot allowed for `s20f3-web`. Only one of them had a live session (the hold),
so there is exactly one `session cancelled` line. The same 1-line / `count=16` pattern repeats for v23
and v26.

| # | Assertion | Observed | |
|---|---|---|---|
| L1 | connector cancel ≤ 2 s after the push, before the client tick | cancel at push +0.4 s (same ms as `ACL snapshot stored version=21`); hold FAIL at 17:06:22.367, its first request after the push; the client tick was 57 s later | ✅ |
| L2 | closed by the connector, not the client | `RemoteDisconnected` (FIN); the first client `hot-applying` is at 17:07:19.19 | ✅ |
| L4 | new connections to the old port refused | connector `access denied` from 17:06:21.479; client `tunnel denied` → newconn `000 rc=56` in ~10 ms until the tick. After the tick :51712 isn't captured; it went direct and the shield dropped it (`000 8.00 rc=28`) | ✅ |
| L5 | new port works once applied | :51714 direct ~3 ms until 17:07:18, tunnelled 15–20 ms from 17:07:20 | ✅ |
| L6 | unrelated session unaffected | hold UNREL req=180 at 17:08:11, 0 FAIL; UNREL newconn 76/76 × 200 | ✅ |
| — | no restart | tunsamp: no change (ifindex 13, PID 188850); `structural configuration change, restarting VPN` = 0 | ✅ |

### Row L3: port 51714 → 51712 with the client cut off from the controller

1. Hold WEBN on :51714 (tunnelled) from 17:19:32.
2. Block: `17:20:39.635 BLOCK rc=0 dur=300s` → `17:25:39.667 UNBLOCK rc=0`; the rule counted
   **60 packets dropped**. Confirmed in the client: `Relay CRL refresh failed … 192.168.1.164:8080`
   (11:51:27Z), the 17:21:19 sync hung, then `background ACL sync failed — keeping cached snapshot
   error=transport error` (11:53:34Z).
3. Edit while blocked:

```
controller  17:21:52  acl push … version=23 entries=2
connector   11:51:52.579710Z ACL diff: session cancelled spiffe_id=…/client/03cb5cf7…
connector   11:51:52.579767Z ACL diff: revoked sessions torn down count=16
connector   11:51:52.579794Z session cancelled — authorization revoked mid-session
hold-WEBN   17:21:53.296 req=71 FAIL RemoteDisconnected: Remote end closed connection without response
client      (no sync success, no apply until the block lifted)
client      11:55:49.989051Z ACL snapshot synced version=23 entries=2
client      11:55:49.997052Z resource change, hot-applying removed=1 added=1 pending=0
client      11:55:50.029176Z resources hot-applied resources=2 removed=1 added=1 pending=0
```

| # | Assertion | Observed | |
|---|---|---|---|
| L3 | connector cancel lands while the client can't sync | cancel at push +0.6 s; hold FIN at 17:21:53.296; the client's only sync results in the window were failures; it applied 3 m 57 s later, after the unblock | ✅ |
| L4 (blocked) | old port refused while the client still captures it | 17:21:53–17:25:49: :51714 116 × `000`, avg 11 ms (connector deny); client `tunnel denied` ×117 in the block window | ✅ |
| L5 (after unblock) | new port tunnelled once applied | :51712 direct ~2–3 ms until 17:25:48, tunnelled 17–31 ms from 17:25:50.49 | ✅ |
| L6 | unrelated session | UNREL hold alive through the block (req=750 at 17:27:17); UNREL newconn 116/116 × 200 in the window | ✅ |

Not graded (F-2 / by design): during the block, :51712 (the *new* port, not yet captured by the client)
answered **200 direct** (~3 ms, 117/117). The shield's protection had moved to the row-1 port (:51714)
and it hadn't yet received the move back. This is the F-2 shield-delivery lag and the uncaptured direct
path, not a connector issue.

### Row L7: name-only edit with a hold open

Hold WEB2 on :51712 from 17:27:34. Rename `s20f3-web` → `s20f3-web-renamed`:

```
controller  17:27:55  acl push … version=25 entries=2
connector   11:57:55.629927Z ACL snapshot stored version=25   (no ACL diff line)
client      11:58:49.998661Z effective config unchanged, keeping tunnel
```

| # | Assertion | Observed | |
|---|---|---|---|
| L7 | no connector cancel; hold 0 FAIL | no `ACL diff` line; hold WEB2 req=30 at 17:28:33, req=60 at 17:29:33, stopped cleanly at 17:30:30 | ✅ |

### Row L8: regression, access removal

Unassign `s20f3-unrel` from the group with hold UNREL open (WEB2 still open):

```
controller  17:29:35  acl push … version=26 entries=1
connector   11:59:35.498731Z ACL diff: session cancelled spiffe_id=…/client/03cb5cf7…
connector   11:59:35.498785Z ACL diff: revoked sessions torn down count=16
connector   11:59:35.498819Z session cancelled — authorization revoked mid-session
hold-UNREL  17:29:36.563 req=819 FAIL RemoteDisconnected
hold-WEB2   17:29:33 req=60 200 … still active until stopped
```

| # | Assertion | Observed | |
|---|---|---|---|
| L8 | access removal still cancels (pair diff unchanged) | cancel at push +0.5 s; UNREL FIN at 17:29:36.563; WEB2 (other resource) unaffected | ✅ |

## 4. Whole-run counts (17:02–17:30)

- `ACL diff: revoked sessions torn down`: 3 (v21 port edit, v23 port edit while blocked, v26 unassign).
  0 on v19 (same-content save) and v25 (name-only).
- Client: `resource change, hot-applying` 3 → `resources hot-applied` 3; `structural configuration change,
  restarting VPN` **0**; `session expired|refresh session dead` **0**; `sync failed` 2 (both inside the L3
  block, by design).
- tunsamp: one line (baseline), no change: no data-plane restart.
- Every hold death was a FIN from the connector; the client RST path never fired, because the
  connector always acted first.

## 5. Findings and observations

| # | Component | Severity | Item | Follow-up |
|---|---|---|---|---|
| O-1 | admin UI | low (UX) | `EditResourceModal` `useEffect([resource])` resets the edit form on every resource refetch (shield acks ~15 s), so a slow edit silently saves the old values (Row 1 attempt 1) | own ticket; not F-3 |
| O-2 | client | cosmetic (F-1) | `background sync: version changed, restarting tunnel` printed before the L3 recovery hot-apply | F-1 |
| O-3 | shield/controller | known (F-2) | the shield follows port edits late, so the uncaptured new/old port's direct reachability depends on shield timing | F-2 |
| ENV-1 | lab | — | the first L3 `sudo … &` suspended at the password prompt (tty input); re-sent as `sudo -v; sudo setsid nohup …&` | playbook note |

## 6. Lab state left behind

- .75: F-3 connector build running (rollback: `sudo install -m 0755 /usr/local/bin/zecurity-connector.pre-f3
  /usr/local/bin/zecurity-connector && sudo systemctl restart zecurity-connector`); shield `s20f3-shield-75`
  running; vite on :51713/:51714 still running; bundle at `~/s20f3/` (shield.env token consumed at
  enrollment and removed from `/etc/zecurity/shield.conf` by the installer; the bundle copy remains).
- Resources: `s20f3-web-renamed` (:51712, protected, assigned); `s20f3-unrel` (unassigned).
- Local: `~/s20-run/f3-shield-75.env` (0600, token already used); `s20-serve` expires on its own
  (RuntimeMaxSec); the block table is removed (UNBLOCK rc=0). The instruments are stopped.
- Nothing committed.
