---
type: live-acceptance
sprint: 20
fix: 1
row: U5 (strict scope)
date: 2026-10-10
verdict: PASS
---

# U5 (strict) — long-lived tunnel to a connector-served TCP resource: live acceptance (2026-10-10)

U5 run-sheet definition: *"Client up; long-lived tunnel open"*. Expected: *"tunnel open to a
connector-served TCP resource (not shield-routed RDE)"* (`Live-Acceptance-Run-Sheet.md`, U5 row).

Earlier evidence used shield-routed resources (see [[Phase1-AppliedConfig-Restart-Decision]]
§ G9 / AT-G.6 re-grade), so strict U5 had stayed outstanding. This run closes that gap.

Path: Client → Zecurity tunnel → Connector → Internal application. The resource is unprotected, so
it uses connector route and no shield.

Times are IST; journal lines are UTC (IST − 5:30). Lab logs are in `~/s20-run/logs/u5/` on the client
host (not in the repo).

## A. Environment

| Item | Value |
|---|---|
| Repo | `feat/sprint20-m1-phase2` @ `7a37b5f`; uncommitted F-1 log wording in `client/src/daemon.rs` |
| Client | `/usr/local/bin/zecurity-client` sha `032a0cf4…` (committed slices through F-3 + F-1 wording). MainPID **2501**, `zecurity0` ifindex **12**, user `pv638418@gmail.com`, device `03cb5cf7` |
| Controller | `~/s20-run/controller-f5c` (`s20-controller` user unit), `CONNECTOR_CERT_TTL=15m`, `CONNECTOR_RENEWAL_WINDOW=10m` |
| Connector | `s20l6-conn-75` `7523dc08` on .75, binary sha `19e36cee…` (F-3 build). It is the **only active connector** in RN `s20f5-rn2` (`cf520c20`) |
| Resource | `s20f3-unrel` `261a549d`, 192.168.1.75:51713 TCP, **`unprotected`**, so `routeTypeForResource` → `"connector"` (`controller/internal/policy/compiler.go`). Backend: journal vite app on .75:51713, started 17:08 IST |
| Access | assigned to group `s20-summa-users` at about 17:09:53 (ACL **v16**, `entries=1`). The client hot-applied it at 17:10:01 (`resource change, hot-applying removed=0 added=1`), with no restart |
| Pre-run note | .75 rebooted at 15:03 IST (before the run, not during it). Connector and shield restarted automatically; the vite app was restarted by hand |

## B. Procedure

1. Probes at 17:11:12: 3 × `curl` → 200 (≈17–28 ms).
   - Client: `new TCP connection` / `tunnel opened dest=192.168.1.75 port=51713`.
   - Connector: `session registered … resource_id=261a549d…`. This confirms the tunnel path.
2. `u5-tunsamp` (`f5c5-tunsamp.sh`, change-only, 500 ms: ifindex, PID, fwmark rules, table-105 routes, policy hash) started at 17:11:30.118.
3. `u5-hold` (`f5c5_hold.py 192.168.1.75 51713 2400`) started at 17:11:30.150.
   - It holds one `http.client` connection and sends a keep-alive GET every 2 s.
   - On every request it checks the local port; it exits on `SOCKET CHANGED` or on any error, with no reconnect.
   - It logs every 30th request and any failure.
4. No intervention during the hold.

## C. Evidence

| Assertion | Result | Evidence |
|---|---|---|
| One long-lived connection | ✅ | `u5-hold.log`: `START … local_port=39492 duration=2400`, then `DONE req=1194 local_port=39492 elapsed=2401s` at 17:51:31.978. 0 `FAIL`, 0 `SOCKET CHANGED`; all 39 progress lines `status=200` |
| Carried by the tunnel, not the LAN | ✅ | Client: exactly 1 `new TCP connection dest=192.168.1.75 port=51713` and 1 `tunnel opened` in 17:11:25–17:51:35, both at 11:41:30.15Z. Connector: `session registered … resource_id=261a549d…` at 11:41:30.157615Z and `session unregistered` at 12:21:32.001339Z, matching hold START and DONE. No other session lines in the window |
| Connector-served, not shield-routed | ✅ | Resource `unprotected` → route type `connector`; single active connector in the RN; connector session lines above |
| Connector renewed during the hold | ✅ 8× | Controller `ReEnroll sent` for `7523dc08` at 17:12:17, 17:17:32, 17:22:47, 17:28:02, 17:33:17, 17:38:32, 17:43:47, 17:49:02. 8 `superseded stream closed (newer stream is authoritative)`; 0 `disconnected`. Connector side: 8 × `cert renewed successfully`, expiry advancing 11:57:17Z → 12:34:02Z; 8 × `Control stream established`, 0 `control stream error` |
| No tunnel restart / no client action | ✅ | `u5-tunsamp.log` has only its start line: ifindex 12, PID 2501, hash `c7a9f16913` throughout. Client journal for the window: 0 `restarting VPN`, 0 `full restart`, 0 `zecurity0 up/down`, 0 hot-applies, 0 `change detected`, 0 `sync failed`, 0 `session expired`, 0 `QUIC relay ended`, 0 `handshake timed out`; 40 × `ACL up_to_date — kept cached snapshot version=16` |
| No config version changes | (context) | No `acl push` / transport push in the controller log for the window. Renewals no longer bump versions (`6362d1f`) |

## D. Verdict

**PASS for U5 as defined.** A long-lived tunnel to a connector-served (non-shield-routed) TCP resource
stayed open on one socket for 40 min 1 s (1194 × 200). It ran through 8 successful renewals of the
connector that carried it. The client made no restarts and no decisions.

The renewal part of this also matches G9 / AT-G.6 on the connector route, which complements the
shield-routed 2026-10-06 evidence.

## E. Limitations and scope

- **Not a Fix 01 regression test.** The ACL stayed at v16 and no snapshot changed during the hold, so the
  restart/hot-apply decision was not exercised. The post-slice Fix 01 regression criterion is still
  outstanding and still needs agreement.
- **No connector loss.** Recovery of an established flow is out of scope; it is deferred as
  [[PENDING-18-Established-Connection-Recovery-Connector-Failover]].
- **Duration.** U5 states no explicit duration. 40 min, 8 renewals was chosen to exceed the "≥ 3 renewals
  at 15 m TTL" refinement and the fix-connector "≥ 30 min".
- **One run, one flow, one connector.** The client binary includes uncommitted F-1, which changes log
  wording only.
- **Q3 is unaffected** and remains open.

## F. Lab state after the run

- The `u5-hold` and `u5-tunsamp` units have finished or stopped.
- `s20f3-unrel` is still assigned to `s20-summa-users`, and the vite app is still running on .75:51713.
- Nothing is committed.
