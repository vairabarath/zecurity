---
type: live-acceptance
sprint: 20
fix: 5
phase: 5-C
row: L6 (re-run per §6a.4)
date: 2026-10-10
verdict: PASS
source: "[[Phase5C-Live-Acceptance-2026-10-08]] §6a.4"
---

# Phase 5-C L6 re-run: client resets the flow on a pure resource removal (2026-10-10)

Re-run of [[Phase5C-Live-Acceptance-2026-10-08]] §6a.4. That record and [[Phase5C-Resource-Hot-Apply]]
are not modified here. Times are IST (UTC+5:30); journal lines are UTC.

Path: Client → Zecurity tunnel → Connector → Shield → Internal application.

## A. Environment

| Item | Value |
|---|---|
| Repo | `feat/sprint20-m1-phase2` @ `7a37b5f`. Uncommitted F-1 (log wording only) in `client/src/daemon.rs` and `daemon_tests.rs`, plus pre-existing doc changes |
| Client binary | `/usr/local/bin/zecurity-client` sha `032a0cf43277cff7`: 5-C + zero-resource + F-1 wording. The 5-C removal path (`net_stack.rs` close-by-key) is identical to the committed code |
| Client process | MainPID **2501** for the whole run; `zecurity0` ifindex **12**; user `pv638418@gmail.com`, device `03cb5cf7` |
| Controller | `~/s20-run/controller-f5c` via `f5c5-ctl.sh` (TTLs: connector 15m, shield 168h), relaunched 11:07:29 after the 10:28 host reboot |
| Connector | `s20l6-conn-75` `7523dc08` on .75 (re-enrolled 11:10; old `s20z-conn-75` cert had expired). Binary sha `19e36cee64c4a4fc` (F-3 build; pair diff unchanged for pure removals). Update timer disabled |
| Shield | `s20l6-shield-75` `0c858d2d` on .75, bound to `7523dc08` (re-enrolled 11:11). Update timer disabled |
| Resource | `s20l6-web` `aca1779e` 192.168.1.75:51714, protected (shield route), RN `s20f5-rn2`, group `s20-summa-users`. Backend: journal vite on .75:51714 |
| Relays | none (`LabelledRelayList relay_count=0`), so every flow is direct QUIC to the connector |
| Logs | `~/s20-run/logs/l6/` (`l6-events.log`, `l6-hold.log`, `l6-tunsamp.log`); .75 `/tmp/l6block.log` |

### Deviations from §6a.4 (approved 2026-10-10)

- Rig on .75 (`s20l6-conn-75` + `s20l6-shield-75` + `s20l6-web`) instead of .49 `manoj` + A2. The
  .49/.34 agents' certificates had expired, so the original rig is gone. The mechanism is the same.
- Block lasted 120 s instead of about 90 s, so the client tick fell well inside it.
- The unassign landed at `11:17:59`, 2 s before the client tick (`hh:mm:01.1`), not at `hh:mm:06`. Exactly one
  tick (11:18:01) followed it inside the block, which is the intent of step 3.
- The old resource `s20f3-web-renamed` (bound to the dead `s20f3` shield) was deleted (status
  `deleting`, excluded from ACL compile) before `s20l6-web` was created. Its group rule row remains.

## B. Procedure

1. Baseline 11:13:16.793 (`l6-tunsamp`): ifindex 12, pid 2501, fwmark rules 1, table-105 routes 1.
2. Hold `l6-hold` (`f5c5_hold.py`, one keep-alive socket, GET every 2 s) on 192.168.1.75:51714 from
   11:13:16.816, local port 46416. Client: `new TCP connection` / `tunnel opened` at 05:43:16.8Z;
   connector: `session registered` at 05:43:16.815Z.
3. Confounder check: connector cert renewal fired 11:15:34 with no ACL bump; the next one is due ≈11:20:34,
   after the block window.
4. On .75 (root, self-reverting): `connector-control-block.sh 11:17:50 120` drops TCP 9090 to and from
   the controller both ways; QUIC 9092 untouched.
5. 11:17:59 admin UI: group `s20-summa-users` → Unassign `s20l6-web` (controller `acl push version=9
   entries=0`; `access_rules` for `aca1779e` = 0).
6. Waited until 11:20:05, then collected client, connector and shield journals, the hold, tunsamp and the block log.

Window: 11:13:16 – 11:21:32 IST.

## C. Timeline

| IST | Component | Event | Source | Meaning |
|---|---|---|---|---|
| 11:13:16.816 | client / connector | hold opened; `session registered` | hold log, journals | live session on `s20l6-web` |
| 11:15:34 | controller → connector | ReEnroll, cert renewed, ACL stays v8 | controller.log | renewal finished before the test |
| 11:17:50.466 | .75 nft | `BLOCK rc=0` | `/tmp/l6block.log` | connector can't get pushes |
| ~11:18:00.3 | hold | req 142 OK (2 s cadence; log prints every 30th + FAIL) | inferred from req 120 @ 11:17:16.272 | flow alive before tick |
| 11:17:59 | controller | `acl push version=9 entries=0 connectors=1` | controller.log | removal pushed; connector is blocked |
| 11:18:01.136 | client | `ACL snapshot synced version=9 entries=0` | client journal | client received it |
| 11:18:01.148 | client | `background sync: change detected, evaluating tunnel configuration acl_changed=true transport_changed=false` | client journal | pure ACL change |
| 11:18:01.148889 | client | `resource change, hot-applying removed=1 added=0 pending=0` | client journal | §6a.4 step 4 line |
| 11:18:01.150899 | connector | `session unregistered … resource_id=aca1779e…` | .75 journal | stream ended from client side (no ACL in hand) |
| 11:18:01.172 | connector | `QUIC accept_bi: closed by peer: 0` | .75 journal | client closed the idle QUIC conn after the flow |
| 11:18:01.176033 | client | `resources hot-applied resources=0 removed=1 added=0 pending=0` | client journal | apply finished |
| 11:18:01.176 | client | `no routable resources; tunnel kept up, nothing captured` | client journal | no restart |
| 11:18:01.483 | host | table-105 routes 1→0; ifindex 12, pid 2501 unchanged | tunsamp | incremental route removal |
| 11:18:02.530 | hold | `req=143 FAIL ConnectionResetError: [Errno 104]` | hold log | **RST** |
| 11:18:45 | client | lifecycle stats `rst_on_relay_failure=0 rst_fail_closed=0 backstop_aborts=0` | client journal | not a relay-failure reset |
| ~11:18:10–11:19:01 | controller / client | connector marked disconnected; v10 ACL + transport v4 → `effective config unchanged, keeping tunnel` | journals | §6a.4 step 5 side effect, no restart |
| 11:19:50.498 | .75 nft | `UNBLOCK rc=0`; counters out 24 pkts, in 10 pkts dropped | block log | block was effective |
| 11:19:59.265 | connector | reconnect; `ACL diff: revoked sessions torn down count=16`; ACL v11 stored | .75 journal | first ACL the connector saw after the removal, 117 s after the RST. No `session cancelled` line |
| 11:20:01 | client | v11 / transport v5 → `effective config unchanged, keeping tunnel` | client journal | no restart |

Connector `session cancelled` lines from 11:13 onwards: **0**. Session lines in the window: only
`registered` 05:43:16.815Z and `unregistered` 05:48:01.150899Z.

## D. Results

§6a.4 pass condition: "RST on the hold + `hot-applied … removed=1` + 0 restarts + no connector cancel for
that session."

| §6a.4 criterion | Label | Result | Evidence | Caveat |
|---|---|---|---|---|
| (removal performed) | L6-1 | PASS | UI unassign; `acl push version=9 entries=0`; `access_rules`=0 for `aca1779e` | — |
| `hot-applied … removed=1` | L6-2 | PASS | `hot-applying removed=1` 01.148889, `resources hot-applied resources=0 removed=1` 01.176033 | — |
| RST on the hold | L6-3 | PASS | `ConnectionResetError` 11:18:02.530, 1.35 s after the apply. A relay end would give FIN (`RemoteDisconnected`). The removal path `socket.abort()`s the key and drops the relay receiver (`net_stack.rs` ≈575–598), and the connector saw the stream end 2 ms into the apply | The client doesn't log per-flow aborts; attribution is RST + timing + counters + connector side |
| no connector cancel for that session | L6-4 | PASS | Connector blocked 11:17:50–11:19:50 (dropped-packet counters > 0); first ACL after removal stored 11:19:59; `session cancelled` = 0; its `session unregistered` (11:18:01.150) comes after the client's `hot-applying` (01.148889) and before the connector had any new ACL | — |
| (not a relay failure) | L6-5 | PASS | No relays in the workspace (`relay_count=0`); `rst_on_relay_failure=0`; no relay/error/denied lines in the client journal for the window | — |
| 0 restarts | L6-6 | PASS | MainPID 2501 and ifindex 12 throughout; tunsamp only changed table-105 routes; 0 `restarting VPN` / `full restart` / `zecurity0 up` since 11:13 | — |
| (ordering) | L6-7 | PASS | Single clock per host, ms-resolution timestamps; .75 and .164 journals agree to within ms on the session start (connector registered 05:43:16.815682Z vs client opened 05:43:16.818Z) | Cross-host skew bounded only by that match |

## E. Verdict

**PASS.** Every §6a.4 criterion was demonstrated on one live flow. The connector was cut off from ACL pushes for
the whole window, so the reset came from the client's own resource-removal handling, not from F-3 or
the older connector revocation.

## F. Remaining work and limitations

- Single run with a single flow. No unrelated-resource hold this time (that was covered in the 10-08 run and F-3 L8).
- Rig substitution and the 2 s-before-tick timing are documented above.
- The client build carries uncommitted F-1 wording (logging only).
- Updating `Phase5C-Resource-Hot-Apply.md` / the 10-08 record status from this evidence is a separate step;
  it is not done here.
- Lab clean-up pending approval:
  - new enrollment env files `~/s20-run/l6-{conn,shield}-75.env` and the copies on .75 contain spent tokens;
  - vite :51714 on .75;
  - `/tmp/connector-control-block.sh` on .75;
  - the old `s20f3-*` / `s20z-*` agents' DB rows;
  - the `s20f3-web-renamed` row stays `deleting`.
- Nothing committed.
