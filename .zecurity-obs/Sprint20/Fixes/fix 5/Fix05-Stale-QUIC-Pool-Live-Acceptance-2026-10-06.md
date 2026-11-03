---
type: live-acceptance
sprint: 20
fix: 5
phase: 5-B
title: Stale pooled QUIC connection — live acceptance
status: live-accepted-with-caveat
date: 2026-10-06 / 2026-10-07
spec: "[[Fix05-Stale-QUIC-Pool-Investigation]] §18 (rows), §20 (implementation)"
---

# Fix 05-B live acceptance — 2026-10-06 (B2, B1) and 2026-10-07 (B3–B7)

> **Status: `live-accepted-with-caveat`.** All rows B1–B7 ran, plus the B2 old-build baseline. Every
> §13 acceptance gate of the run brief holds (stale pool evicted after the silent gate; no repeated 5 s
> penalty; healthy connector carries the flows; existing flow survives eviction; slow resource on a live
> connector is not evicted; no tunnel restart). Caveats are listed in §15: B5 had 10 slow flows against
> §18's "≤ 5" (two stale connections, one per connector — §18 was written for a single connector), one
> unexplained code-0 connection close on the connector side during B4, and day 2 ran on a rebuilt lab.
> No production code changed. Nothing committed.
>
> Day 1 sections (§1–§7) describe the 2026-10-06 lab. Day 2 (§8 onwards) used a rebuilt lab after a
> whole-lab reboot; see §8.

## 1. Lab inventory

| Item | Value |
|---|---|
| Branch / HEAD | `feat/sprint20-m1-phase2` @ `091837d`. 5-B uncommitted in `client/src/{tunnel_pool,transport,net_stack}.rs` only |
| Controller | user unit `s20-controller`, `~/s20-run/controller-f5c` sha `36892b48…` (accepted empty-network build), PID 637969, up since 15:11:58. Not touched |
| Client OLD (B2) | `e2e36c45…` (Fix 5-A build). Backup `~/s20-run/zecurity-client.f5a-e2e36c45.bak` |
| Client NEW (B1+) | `1f960fe23a7bb98b…`. Rebuilt in preflight because two source files had an mtime after the 17:02 build; `cargo build --release` (no source edit) gave the **identical** sha, so the binary matches the tree. Backup `~/s20-run/zecurity-client.5b-1f960fe2.bak`. Installed 18:01:43, PID 1038726, `strings` contains the 5-B line ×1 |
| Client device | `bbe8063c-4660-42ec-9eed-b95e08530a95` (re-logged 17:30 after the admin-UI work). 0 `sync failed / session expired / refresh session dead` since |
| Client ACL poll phase | OLD daemon: `hh:mm:58.43`. NEW daemon (since 18:01:43): **`hh:mm:43.86`** |
| Remote network | `s20f5-rn2` `cf520c20-a4e5-415d-938b-6bb932c06748` |
| Connector `.75` | `s20f5-conn-rn2` `56de96a1-63f6-4bdb-8e25-84559368f131`, archlinux 192.168.1.75, sha `970bfa02` |
| Connector `.47` | `s20f5b-conn-rn2b` `bd4a4f6a-e349-4995-8e14-c0abcbe765c4`, **udaya 192.168.1.47** (new for this run; the host's stale connector was re-enrolled), sha `970bfa02`, update timer `disabled`/`inactive` |
| Resource (B1/B2) | `s20f5-rn2-web` `0d9132b3-1375-4f3a-a214-29a3c92e2bfa` 192.168.1.75:51712, connector-routed (no shield) |
| Resource (B3) | `s20f5b-blackhole` `9d638364-be29-470b-ba30-940e97573fec` 10.255.255.1:80, assigned to `s20-summa-users` (ACL v22 `entries=3`). From .75, `timeout 8 bash -c '</dev/tcp/10.255.255.1/80'` → rc 124 (hangs) |
| B4 file | `~/Projects/journal-management-main/client/public/f5b-blob.bin` on .75, 8 MiB (sha `00fa11d4…`). Delete at clean-up |
| Palace (control) | `s20f5-conn-manoj` .49 + shield nika, untouched; probed as a control resource |

## 2. Instruments

| Unit / script | Log | Purpose |
|---|---|---|
| `f5b-probe` (`f5b-probe.sh`) | `logs/f5b-probe.log` | 1 s new connection to rn2-web and palace, `curl -m 8`, `time_total` (time stamp = completion) |
| `f5b-tunsamp` (`f5b-tunsamp.sh`) | `logs/f5b-tunsamp.log` | change-only zecurity0 ifindex / client PID / policy-routing hash |
| `f5b-instr.sh <s> [label]` | `logs/f5b-events.log` | (re)starts both units and writes a marker |
| `f5b-dsched.sh <stop|stoponly|freeze> <down_s> [HH:MM:SS]` | `/tmp/f5b-d.log` on the host | root self-timed connector stop/freeze; shipped to .75 and .47 (sha `a09702d7`) |
| `f5b-analyze.sh <since> <until>` | stdout / `logs/f5b-<row>-analyze.txt` | controller connect/disconnect, client counts (incl. the 5-B line), key lines, probe tallies, tunsamp |
| `f5b-conn.sh <since> <until>` | stdout / `logs/f5b-<row>-conn.txt` | per-connector `tunnel_opened ok` / blackhole attempts + schedule log, both hosts |
| `f5b-db.sh` | stdout | read-only connectors/resources/RNs |

Both units were **stopped at the pause** (18:11:48). Restart with `bash ~/s20-run/f5b-instr.sh 7200 resume`.

## 3. Findings that shape the remaining rows

1. **The first-tried connector is re-rolled at every snapshot version bump.** Connector-routed resources have
   no `preferred_connector_id`, so the client uses the transport snapshot order
   (`client/src/daemon.rs` `ordered_transport_connectors_for_entry`), and the controller orders connectors by
   `last_heartbeat_at DESC` at compile time (`controller/internal/transport/store.go:62`). Any connector
   connect/disconnect re-rolls it. **Before every row, read which connector served the last ~20 s**
   (`f5b-conn.sh`) and stop *that* one. B2 attempt 1 was void because of this (see §4).
2. **The ACL poll can mask the stale pool.** With the empty-network controller, a stop is seen by the
   controller at once, and the client's next 60 s poll hot-applies a map without the dead connector (fresh
   pools). In the topology check the poll landed 5.7 s after the stop and the old build showed only 7 slow
   probes. So every stop row is armed **3 s after the poll** (`hh:mm:46` for the NEW daemon) to leave the full
   window exposed.
3. **Cooldown expiry costs 2 × 2 s (pre-existing, both builds).** 30 s after the direct path enters
   cooldown (`DIRECT_RETRY_INITIAL_COOLDOWN`, `client/src/transport.rs:16`) the client re-dials the dead
   connector, pays `direct stream establishment exceeded 2s` on the flows started during that dial (2 probes),
   then cools down again until a poll removes the connector. Not a stale-pool effect; not a 5-B regression.

## 4. Setup and topology check (old build)

- 17:26 admin UI: connector `s20f5b-conn-rn2b` token (0600 `~/s20-run/f5b-conn-rn2b.env`, shipped to
  udaya `~/s20fc/cbundle/conn.env`, sha match `f801e0e0b43d`), `uninstall -y` + `local-install` + timer
  disabled; `enrollment complete` 17:28:22, `Control stream established`. Resource `s20f5b-blackhole`
  created and assigned. Admin preview closed, then client re-login 17:30.
- A-stop (.75) 17:33:52.088–17:34:52.150: .47 served 60 tunnels, .75 0; 112/112 rn2 200 (7 × 5 s, cut short
  by the 17:33:57.8 poll, finding 2).
- B-stop (.47) 17:38:52.630–17:39:22.678: .75 served 29 tunnels, .47 0; 28/28 rn2 200 fast.
- 0 VPN restarts; ifindex 36 / PID 2379 / hash `c8e19c5985` unchanged.
- **B2 attempt 1 (17:53:01, stop .75) — VOID.** .47 had become first-tried at the 17:39:58 poll (656 tunnels
  on .47, 0 on .75); the stop hit the unused connector: 154/154 fast, 0 timeouts. Not counted.

## 5. B2 — before-fix baseline (OLD client `e2e36c45`)

```text
Test ID:          B2 (attempt 2)
Start / End:      17:57:50 / 18:00:20
Client build:     e2e36c45 (pre-5-B), PID 2379
Controller build: controller-f5c 36892b48
Stopped:          s20f5b-conn-rn2b bd4a4f6a (.47) — first-tried (20/20 tunnels in the 20 s before arming)
Fallback:         s20f5-conn-rn2 56de96a1 (.75)
Remote Network:   s20f5-rn2 cf520c20
Resource:         s20f5-rn2-web 0d9132b3 192.168.1.75:51712
```

| Event | Time |
|---|---|
| Last fast probe before stop | 17:58:00.847 |
| `D-STOP` (.47) | 17:58:01.091 (controller `disconnected` 17:58:01, ACL v30) |
| 25 × `tunnel handshake timed out after 5s` | 17:58:06.842 → 17:58:31.0, one per 1 s probe, same stale connection |
| 5 × `tunnel handshake failed … connection lost` | 17:58:31.842 (the 5 in-flight flows, at quinn idle timeout) |
| Stale connection lifetime | **30.75 s** (= quinn 30 s idle timeout) |
| 2 × `direct stream establishment exceeded 2s` | 17:58:34.039, 17:58:35.056 (finding 3) |
| `direct path is in cooldown` (fast fall-through) | 25 from 17:58:34.05 |
| First of the consistently fast probes | 17:58:35.077 (+34.0 s) |
| Poll hot-apply | 17:58:58.44 (v30 / transport v24), 17:59:58.44 (v31 / v25) |
| `D-START` (.47) | 17:59:31.150 (controller `connected`, v31) |

| Counter | Value |
|---|---|
| rn2 probes | 137 × 200 (106 fast, **31 slow**: 25 × 5.0 s, 4 in-flight partials 1.8–4.8 s, 2 × 2.0 s), 0 failed |
| palace probes | 137 × 200 fast |
| .75 tunnels during the stop / .47 | 89 / 0 |
| 5-B eviction line | 0 (not in this build) |
| VPN restarts / structural / sync failures | 0 / 0 / 0 |
| tunsamp | no change (ifindex 36, PID 2379, `c8e19c5985`) |

Matches the §18 expectation ("about 25 × 5 s-slow 200s over ~30 s, then one 2 s penalty"). Evidence:
`logs/f5b-B2-analyze.txt`, `f5b-B2-conn.txt`, `f5b-B2-client-journal.txt`, `f5b-B2-probe-snapshot.log`.

## 6. B1 — partial loss, main fix row (NEW client `1f960fe2`)

```text
Test ID:          B1
Start / End:      18:05:35 / 18:08:00
Client build:     1f960fe2 (5-B), PID 1038726
Controller build: controller-f5c 36892b48
Stopped:          s20f5b-conn-rn2b bd4a4f6a (.47) — first-tried (15/15 tunnels right before arming)
Fallback:         s20f5-conn-rn2 56de96a1 (.75)
Remote Network:   s20f5-rn2 cf520c20
Resource:         s20f5-rn2-web 0d9132b3 192.168.1.75:51712
```

| Event | Time |
|---|---|
| Last fast probe before stop | 18:05:45.260 |
| `D-STOP` (.47) | 18:05:46.042 (controller `disconnected` 18:05:46, ACL v32) |
| First stall `tunnel handshake timed out after 5s` | 18:05:51.234128 |
| **Eviction warning** (1) | 18:05:51.234128 `WARN zecurity_client::transport: connector silent since stream open: stale pooled QUIC connection evicted (not closed), direct path cooling down direct_addr=192.168.1.47:9092` |
| Stale connection in the pool | **5.19 s** (stop → eviction) |
| First successful probe via .75 after eviction | 18:05:51.282 (0.017 s; a new flow 32 ms after the eviction) |
| Other 4 timeouts | 18:05:52.24, 53.26, 54.26, 55.28 — flows opened 18:05:47–50, before the eviction (in flight); their stall reports were no-ops (stable_id gate), so no second eviction line |
| `direct path is in cooldown` | 26 from 18:05:51.27, then 20 more — each falls straight through to .75 in ms |
| Cooldown expiry | 2 × `direct stream establishment exceeded 2s` 18:06:23.502, 18:06:24.515 (finding 3) |
| Poll hot-apply | 18:06:43.88 (v32 / transport v26, removes .47), 18:07:43.88 (v33 / v27) |
| `D-START` (.47) | 18:07:16.097 (controller `connected`, v33) |

| Counter | Value |
|---|---|
| rn2 probes | 143 × 200 (136 fast, **7 slow**: 5 × 5.0 s in flight, 2 × 2.0 s cooldown expiry), 0 failed |
| palace probes | 143 × 200 fast |
| 5 s timeouts on flows opened after the eviction | **0** |
| Eviction warnings | **1** |
| `connection lost` | 0 (B2: 5) |
| .75 tunnels during the stop / .47 | 89 / 0 |
| VPN restarts / structural / sync failures | 0 / 0 / 0 |
| tunsamp | no change (ifindex 37, PID 1038726, `c8e19c5985`) |

| §18 B1 assertion | Result |
|---|---|
| ≤ the flows already in flight pay 5 s | ✅ 5, all opened before the first stall completed (1 s probe × 5 s timeout) |
| Evidence-gated eviction logged after the first `handshake timed out` | ✅ 1 line, same µs as the first stall |
| 0 further 5 s timeouts on that connector until it returns | ✅ 0 (only the pre-existing 2 × 2 s cooldown re-dial, finding 3) |
| All probes `200` via connector 2 | ✅ 143/143; .75 served 89 tunnels, .47 0 |
| 0 tunnel restarts | ✅ PID / ifindex / policy hash unchanged |
| Eviction does not `close()` the connection | ✅ not observable in B1 itself; proven on day 2 by B4 (§10) |

**B1: PASS** on its own assertions. Evidence: `logs/f5b-B1-analyze.txt`, `f5b-B1-conn.txt`,
`f5b-B1-client-journal.txt`.

## 7. B2 vs B1

| Metric | B2 old client | B1 new client |
|---|---:|---:|
| Connector loss time | 17:58:01.091 | 18:05:46.042 |
| 5 s handshake failures | 25 | 5 (all in flight) |
| Slow probes (≥ 1 s) | 31 | 7 |
| First fast probe via the other connector | 17:58:35.077 (+34.0 s) | 18:05:51.282 (+5.24 s) |
| Stale connection lifetime in the pool | 30.75 s (quinn idle) | 5.19 s (evicted) |
| Eviction warning | N/A | 1 |
| `connection lost` | 5 | 0 |
| Tunnel restarts | 0 | 0 |
| Successful probes | 137/137 | 143/143 |

Day-1 probe/tunsamp logs were moved to `logs/day1/` before day 2 so counts don't mix.

## 8. Day 2 (2026-10-07): lab rebuild

The whole lab rebooted overnight (controller host `uptime -s` 10:02; connector hosts 10:21–10:38). On resume
`zecurity-client sync` returned `transport error`: `s20-controller`/`s20-admin` units were gone and both 15 m
connector certs had expired. Rebuild (no production code touched):

| Item | Day 2 value |
|---|---|
| Controller | same binary `controller-f5c` `36892b48` relaunched 10:43:32 via `f5c-ctl-restart.sh` (same TTL env). `git log -- controller` newest is the `091837d` merge; its later provider changes are not in the binary — the run keeps the accepted empty-network build, as the brief requires |
| Connector `.75` | **`s20f5b-rn2-a75` `5f926570-0fbe-46ba-bafa-b511e41668aa`** (re-enrolled, sha `970bfa02`, update timer disabled) |
| Connector `.47` | **`s20f5b-rn2-b47` `263e4241-22ab-445e-9ebe-d06efb2773b1`** (re-enrolled, sha `970bfa02`, update timer disabled) |
| Old day-1 connectors | `s20f5-conn-rn2`, `s20f5b-conn-rn2b`: `disconnected` (expired), not used |
| rn2 app | restarted on .75 (`npm run dev … --port 51712`), `f5b-blob.bin` still present |
| Client | same NEW binary `1f960fe2`, PID 2535 (daemon started at boot), NRestarts 0. Re-login 10:48 (device `0da4c730`), again 11:08 (device `49d36040`, after the ACL change below). ACL poll phase **`hh:mm:20.41`** |
| Palace | not rebuilt; dropped from the probe (rn2-only) |
| Orca handles | all stale after the restart; re-derived (`f5b-terms.py`), `terms.env.old` kept |
| ACL change | `s20f5-nika-web` **unassigned** from `s20-summa-users` at 11:08 (ACL v5 `entries=2`) to unblock B3 — see finding F-1 |

### Finding F-1 (pre-existing, not 5-B): the client accepts at most 2 resource IPs

B3 attempt 1 (10:50:58) was void: the 6 blackhole curls timed out (`rc 28`, 12 s) with **no** client
`new TCP connection` and **no** connector attempt. Cause, read in source:

1. The controller sorts ACL entries by resource UUID (`controller/internal/policy/compiler.go:206-208`):
   `0d9132b3…` rn2-web, `8d65d5f5…` nika-web, `9d638364…` blackhole.
2. `net_stack::run` pushes one `/32` per entry in that order, then `100.64.0.1`, ignoring each result
   (`client/src/net_stack.rs:713-719`, `let _ = addrs.push(...)`).
3. smoltcp 0.11's address list is a `heapless::Vec<IpCidr, IFACE_MAX_ADDR_COUNT>`
   (`smoltcp-0.11.0/src/iface/interface/mod.rs:109,337`). This build compiles it as **2**
   (`client/target/release/build/smoltcp-*/out/config.rs:10`; no `iface-max-addr-count-*` feature in
   `client/Cargo.toml:59`; the `8` in smoltcp `lib.rs:140` is its own `cfg(test)` config).
4. So the 3rd IP (and `100.64.0.1`) is silently dropped. nft + table 105 still route the SYN into
   `zecurity0`, smoltcp has no matching address and drops it: no SYN-ACK, no log.

Impact: a device entitled to more than 2 distinct resource IPs can reach only the 2 with the lowest resource
UUIDs, silently. Not touched by 5-B; out of scope for this run. Needs its own fix doc.

## 9. B3 — slow resource behind a healthy connector (safety gate)

```text
Test ID:          B3 (attempt 2)
Start / End:      11:10:07 / 11:10:52
Client build:     1f960fe2, PID 2535
Controller build: controller-f5c 36892b48
Connectors:       first-tried s20f5b-rn2-b47 263e4241 (.47); second s20f5b-rn2-a75 5f926570 (.75) — both kept running
Remote Network:   s20f5-rn2 cf520c20
Resource:         s20f5b-blackhole 9d638364 10.255.255.1:80 (connector's TcpStream::connect hangs)
Control:          s20f5-rn2-web 0d9132b3 (1 s probes)
```

| Event / counter | Value |
|---|---|
| Blackhole flows | 6, opened 11:10:07.311 → 11:10:22.328 (3 s apart, `f5b-b3.sh`) |
| Each flow | 5 s on .47, 5 s on .75, then `QUIC relay ended … no connector accepted tunnel`; curl `000` 10.0 s rc 56 (expected for an unreachable resource) |
| Connector side | .47: 6 `access allowed` + 6 blackhole attempts; .75: 6 + 6 |
| `tunnel handshake timed out` | 12 (6 flows × 2 connectors) |
| **5-B eviction lines** | **0** |
| **`direct path is in cooldown`** | **0** |
| rn2-web during the row | 45/45 × 200 fast, all through .47 (same pool); after the row 13/13 fast |
| `relay failure signalled` | 1 (11:10:17.33, all candidates failed) → resync `ACL up_to_date`, no hot-apply, no restart |
| VPN restarts / tunsamp | 0 / unchanged (ifindex 15, PID 2535) |

**B3: PASS.** A healthy connector keeps ACKing, so the RX-datagram gate blocks eviction and cooldown even
though the application handshake times out. Evidence: `logs/f5b-b3.log`, `f5b-B3-*.txt`,
void attempt `logs/f5b-b3-attempt1-void.log`.

## 10. B4 — existing flow survives eviction (connection-lifecycle gate)

**Run 1 (11:15–11:18) — inconclusive, not counted.** `curl --limit-rate` reads from a socket that kernel
autotuning grows up to `tcp_rmem` max 32 MB, so most of the file was already buffered client-side before the
freeze; it also left the evicted connection idle (connector `accept_bi: closed by peer: 0` at 11:17:53.59).
It completed (200, 8,388,608 B, sha match) but proves nothing about post-eviction transfer.

**Run 2 (counted)** used `f5b-b4.py`: `SO_RCVBUF` 16 KiB (kernel reports 32 KiB) set before connect and an
application read rate of 50 KB/s, so back-pressure reaches the connector and the file must keep crossing the
QUIC connection after the eviction.

```text
Test ID:          B4 (run 2)
Start / End:      11:21:00 / 11:24:10
Client build:     1f960fe2, PID 2535
Connector:        s20f5b-rn2-b47 263e4241 (.47) — carried the download (tunnel_opened ok 11:21:06; .75 0)
Other connector:  s20f5b-rn2-a75 5f926570 (.75)
Resource:         s20f5-rn2-web 0d9132b3, GET /f5b-blob.bin (8,388,608 B, sha 00fa11d4…)
```

| Event | Time / value |
|---|---|
| Transfer start | 11:21:06.030 |
| `D-FROZEN` (.47, `kill -STOP`) | 11:21:26.096 |
| **Eviction** (1) | 11:21:31.676783 `…evicted (not closed)… direct_addr=192.168.1.47:9092` |
| `D-CONTINUED` (`kill -CONT`, state `Ssl`, same PID 8457, NRestarts 0) | 11:21:34.111 |
| Bytes read by the app at eviction | 1,279,154 (sample 11:21:31.534) |
| Bytes after eviction | 7,109,454. At most ~1.35 MB could sit in client buffers (quinn stream window 1.25 MB + smoltcp 64 KiB + socket 32 KiB), so **≥ ~5.7 MB crossed the evicted QUIC connection after the eviction** (a stream cannot move to another connection) |
| Completion | 11:23:53.747 `HTTP/1.1 200 OK`, body 8,388,608 = Content-Length, sha `00fa11d4702e5692` = source, `error=None`, 167.7 s |
| Rate | steady ~53 KB per 1.065 s sample through freeze, eviction and to the end — no stall after CONT, no reset |
| Client `connection lost` | 0; `active_relays=1` at 11:22:33 and 11:23:33 |
| Probes | 189 × 200; 5 slow (3 × 5.0 s in flight during the freeze — later stall reports no-ops; 2 × 4.4/3.4 s completed on .47 after CONT) |
| VPN restarts / tunsamp | 0 / unchanged |

**B4: PASS.** Pool eviction did not close or reset the underlying QUIC connection; the long-lived flow on it
completed intact. ⚠ See caveat C-2 (an unrelated code-0 close at 11:22:28).

## 11. B5 — total connector loss (regression)

```text
Test ID:          B5
Start / End:      11:44:10 / 11:47:55
Client build:     1f960fe2, PID 2535
Stopped:          both — s20f5b-rn2-a75 (.75) D-STOP 11:44:23.033, s20f5b-rn2-b47 (.47) D-STOP 11:44:23.039
Restarted:        11:46:23.070 / 11:46:23.098 (NRestarts 0); controller connected 11:46:23 (ACL v10)
```

| Event / counter | Value |
|---|---|
| Last 200 before | 11:44:22.343 |
| Controller | `disconnected` 11:44:22 (.75, ~1 s host skew) / 11:44:23 (.47); ACL v7 `connectors=1` |
| Eviction lines | **2**, one per connector: .47 11:44:28.338 (first-tried), .75 11:44:33.339 |
| Resync + fail-closed | `relay failure signalled` 11:44:33.339 → transport v5 (empty RN) → `hot-applied … reachable=0` **11:44:33.344** (10.3 s after the stop) |
| Fail-closed | 166 × `connector offline — failing closed`, probes `000` in 3–8 ms |
| Slow flows | **10**, all opened before the hot-apply: 5 × `000 8.0 s` (5 s on .47 then curl's 8 s limit while stalled on .75), 5 × `000 5.0 s` (straight to .75, .47 in cooldown) |
| `handshake timed out` | 15; no retry storm (each connector paid one stall window, then was evicted) |
| False success | 0 × 200 from 11:44:23 to 11:47:20 |
| Recovery | poll 11:47:20.40 `hot-applied … reachable=2`; first 200 at 11:47:20.644 (return is poll-bound, 0–60 s, known) |
| VPN restarts / tunsamp | 0 (11:45:20 poll `effective config unchanged, keeping tunnel`) / unchanged; sync failures 0 |

**B5: PASS with caveat C-1** (10 slow flows vs §18's "≤ 5"). Fails closed, no storm, no false success,
no restart, recovers.

## 12. B6 — `kill -STOP` freeze of the preferred connector

```text
Test ID:          B6
Start / End:      11:52:10 / 11:55:44
Client build:     1f960fe2, PID 2535
Frozen:           s20f5b-rn2-b47 263e4241 (.47, first-tried) D-FROZEN 11:52:23.068 → D-CONTINUED 11:53:23.082 (state Ssl, PID 30702, NRestarts 0)
Other:            s20f5b-rn2-a75 5f926570 (.75)
```

| Event / counter | Value |
|---|---|
| First stall + **eviction** (1) | 11:52:28.731683 (5.66 s after the freeze) |
| In-flight 5 s timeouts | 5 (11:52:28.73 → 11:52:32.77), all opened before the eviction |
| While frozen | `direct path is in cooldown` → fast fall-through to .75; **0 repeat 5 s stalls** |
| Cooldown expiry (frozen peer re-dial) | 2 × `direct stream establishment exceeded 2s` 11:53:00.997, 11:53:02.006 (pre-existing; finding 3) |
| Back-off | the two re-dial failures raise the cooldown 30 → 60 → 120 s; last cooldown line 11:55:01.94 |
| Recovery after CONT | first `tunnel_opened ok` on .47 at 11:55:02.96 — on cooldown expiry, no hot-apply needed (controller never marked .47 disconnected: 60 s < watcher) |
| Probes | 199 in 11:52:23–11:55:44, 0 failed, 7 slow (5 × 5.0 s, 2 × 2.0 s) |
| VPN restarts / tunsamp | 0 / unchanged |

**B6: PASS.** A frozen (silent, socket still open) connector reaches the same silent-connector eviction as a
clean stop. Note: after CONT the healthy connector waits out the back-off (here ~100 s) before it is used again.

## 13. B7 — 30-minute soak

Window 11:56:23–12:26:23, both connectors up, no actions. Tallies (`f5b-soak-snap.sh`, `logs/f5b-B7-snaps.log`
and a window-bounded recount):

| Counter | Value |
|---|---|
| rn2 probes | **1785**, 0 non-200, **0 slow** (max 0.028 s); per 10 min: 11:50 215/0, 12:00 595/0, 12:10 595/0, 12:20 380/0 |
| `handshake timed out` / eviction / cooldown / relay failure | 0 / **0** / **0** / 0 |
| hot-apply / VPN restart / sync failures / `connection lost` | 0 / 0 / 0 / 0 |
| tunsamp changes | 0 (ifindex 15, PID 2535 throughout) |
| Controller | 0 rn2 `disconnected`; 0 `acl push`. Both connectors re-`connected` every ~5 m 15 s (12:12:38, 12:17:53, 12:23:08 …) — cert renewal make-before-break, no version bump, no client effect |

The user reported an internet outage during the soak. It had no visible effect on the lab: everything is on
the LAN, the probe has no gap (1785 samples in 1800 s, with the 1 s `sleep` plus curl), and no client or
controller error lines appear. Instruments then ran to their 7200 s cap (last probe 12:49:24.473, still 200).

**B7: PASS.**

## 14. Results summary

| Row | Result | Key evidence |
|---|---|---|
| B2 (old) | baseline | 25 × 5 s, stale 30.75 s, 5 `connection lost` |
| B1 | PASS | 5 in-flight 5 s, 1 eviction, stale 5.19 s, 143/143 |
| B3 | PASS | 12 timeouts, **0 evictions, 0 cooldowns**, rn2 45/45 fast |
| B4 | PASS (⚠ C-2) | 1 eviction; ≥ 5.7 MB over the evicted connection after it; 200, full size, sha match |
| B5 | PASS (⚠ C-1) | 2 evictions, hot-apply `reachable=0` 10.3 s, fast `000`, recovered |
| B6 | PASS | 1 eviction, 0 repeats while frozen, recovered on cooldown expiry |
| B7 | PASS | 1785 probes, 0 slow, 0 evictions, 0 cooldowns, 0 restarts |

| Totals (NEW build, B1+B3–B7) | Value |
|---|---|
| Eviction events | 6 (B1 1, B4 1, B5 2, B6 1, B4 run 1 inconclusive 1) — every one on a silent/frozen/stopped connector |
| Healthy-connector false evictions | **0** (B3, B7) |
| Existing-flow resets | **0** (B4) |
| Tunnel restarts | **0** (only the expected structural restart from the 11:08 ACL change, outside all rows) |

## 15. Caveats and follow-ups

- **C-1 (B5).** 10 slow flows vs §18 "≤ 5". §18 B5 says "stop the last connector" (one connector); with two
  stale connections the client pays one stall window per connector before the empty-RN resync. Each connector
  was evicted exactly once; no repeat stalls. Not a 5-B regression.
- **C-2 (B4).** At 11:22:28.17 the .47 connector logged `QUIC accept_bi: closed by peer: 0` + one stream
  `connection lost`. It was **not** the download's connection (the download kept full rate through it). 5-B
  adds no `close()`; quinn closes a connection with code 0 when its last handle drops. **Inferred, not proven:**
  a short-lived second connection to .47 from the cooldown-expiry re-dial (~11:22:01) dropped once unused.
- **C-3.** Day 2 ran on a rebuilt lab (new connector IDs, re-login, nika-web unassigned, palace control
  dropped). Same client and controller binaries as day 1.
- **F-1** (§8): smoltcp 2-address limit silently drops resource IPs beyond the first 2. Pre-existing; needs
  its own fix doc.
- **Finding 3** (§3): cooldown expiry re-dials a dead/frozen connector (2 × 2 s); repeated failures back the
  cooldown off (B6: 120 s), which also delays reuse of a recovered connector. Pre-existing.
- Not touched: `relay_pool.rs` close-on-age finding (still open).

## 16. Lab state and clean-up (not yet done — needs approval)

- Client NEW `1f960fe2` installed (old build backup `~/s20-run/zecurity-client.f5a-e2e36c45.bak`).
- rn2 connectors `s20f5b-rn2-a75`, `s20f5b-rn2-b47` active; controller `controller-f5c` running.
- Instruments stopped (cap reached 12:49). Admin preview closed.
- Owed: re-assign `s20f5-nika-web` (if wanted), decide on `s20f5b-blackhole`, delete `f5b-blob.bin` on .75 and
  `logs/f5b-b4-blob.bin`, delete token env files (`~/s20-run/f5b-*.env`, both hosts' `~/s20fc/cbundle/conn.env`),
  revoke disconnected leftovers (day-1 rn2 connectors, `s20f5-conn-inkyank`, `s20p2a-*`), palace/shield
  rebuild if wanted. Evidence logs stay.
