---
title: Connector Revocation — Controlled Live Verification of Existing CRL Enforcement
date: 2026-10-12
type: live-verification-evidence
status: evidence-recorded-awaiting-review
branch: feat/sprint20-m1-phase2
baseline: 9844a00
related:
  - Live-Acceptance-Run-Sheet.md (Finding 34, Finding 33 — NOT updated by this record)
  - Fixes/fix 1/Phase2-Full-Hot-Apply.md (Q3 — open, not decided here)
  - pending/PENDING-18-Established-Connection-Recovery-Connector-Failover.md (not expanded here)
---

# Connector Revocation — Controlled Live Verification (2026-10-12)

**Purpose:** observe the current runtime behaviour of connector revocation on the
existing implementation, before any decision on required changes.

This is evidence only:
- No product code, protocol, configuration, schema or existing documentation was changed.
- Nothing was committed.
- Results use the classifications **OBSERVED_STOPPED / OBSERVED_CONTINUES / OBSERVED_FAILOVER / BLOCKED / INCONCLUSIVE**.
- None of them is a PASS/FAIL on a security requirement.

Times are **IST (UTC+05:30)** unless marked UTC. Raw evidence is in the run directory
`~/s20-run/logs/rev/` on the controller host (192.168.1.164). It is outside the repository and is listed in §6.

---

## 1. Environment and safety verification

| Item | Value |
|---|---|
| Repo / branch / HEAD | `~/zecurity`, `feat/sprint20-m1-phase2`, `9844a00`. 25 pre-existing working-tree status lines, unchanged by this run (this file is the only addition) |
| Controller | lab controller on 192.168.1.164: user unit `s20-controller`, relaunched 16:14:11 by `~/s20-run/f5c5-ctl.sh`. Env: `CONNECTOR_CERT_TTL=15m`, `CONNECTOR_RENEWAL_WINDOW=10m`, `SHIELD_CERT_TTL=168h`. Listens on gRPC :9090 and HTTP :8080 (`/ca.crl`). Admin UI on localhost:5173 (`s20-admin`) |
| Deployment identity | local lab DB `ztna_platform` (docker `ztna_postgres`). Not a production deployment |
| Workspace | `xiyo`, `0d3b8f09-0338-4d03-8729-c4e12b052a4e` (lab workspace; trust domain `ws-xiyo.zecurity.in`) |
| Client | controller host, `zecurity-client` build sha `032a0cf4` (F-1 build). MainPID 2426, TUN `zecurity0` ifindex 12. Device SPIFFE `spiffe://ws-xiyo.zecurity.in/client/03cb5cf7-…`. Client user pv638418 (admin UI used a different Google user, so there was no session eviction; `sync failed`/`session expired`/`restarting VPN` count over the run = 0) |
| **Disposable connector** | **`s20rev-conn-75`, id `bde51440-910b-42a3-9823-99e116b2a6c4`**. Created for this test at 16:37, RN `s20f5-rn2` (`cf520c20-…`), host .75 (archlinux, 192.168.1.75), binary sha `19e36cee64c4a4fc` (connector source unchanged since `2a44ab2`, which is in HEAD). Update timer disabled |
| Shield | **`s20rev-shield-75`, id `31ceacd3-9a89-4945-8067-b1c72ebf0e3d`**. Created for this test, bound to `bde51440` (JWT `connector_id` checked), `lan_ip` 192.168.1.75, `interface_addr` 100.64.0.4/32, shield binary sha `30ba3e39785720d6` |
| Relay | **none active.** Every relay row is `inactive`/`revoked`/`deleted`/`pending`. Connector logs `LabelledRelayList is empty` |
| Test resources | direct: **`s20f3-unrel`** `261a549d-b987-4a87-be02-0d88a51cae0a`, 192.168.1.75:51713, `unprotected` (connector route). Shield: **`s20rev-web`** `ab4d488e-38a2-4e55-8a25-b5c2da6bb79a`, 192.168.1.75:51714, `protected` by `s20rev-shield-75`. Both are in group `s20-summa-users` (`f0a4603e-…`). Backend: vite (journal-management client) on .75:51713 and :51714, started for this run |

**Isolation and safety gate:**
- **Lab-only.** The controller, DB, client and both agents are lab instances on the LAN (192.168.1.x), and the controller is the local build, not `controller.zecurity.in`.
- **Disposable, unshared connector.** The connector was enrolled for this test only. It was the only `active` connector in the workspace: the other 5 connector rows were `disconnected` with certificates expired 10-08…10-10.
- **No other users.** The only client of either test resource was this host's client (one device).
- **No other acceptance work was running.** Prior Sprint 20 rigs on .75 (`s20l6-conn-75`, `s20l6-shield-75`) were dead (expired certs) and were replaced by the uninstall/install of the new agents.

The user approved the setup (clarify, 16:12) with "Go: set up as proposed; relay path BLOCKED", and chose to keep the 15-minute TTL and report the "revoked, cert valid" and "revoked, cert expired" windows separately.

**Revocation method:** the existing admin UI Revoke button on the connector detail page:
- It calls the GraphQL `revokeConnector` mutation (`admin/src/pages/ConnectorDetail.tsx:214-217`; resolver `controller/graph/resolvers/connector.resolvers.go:113-155`).
- There was no DB UPDATE and no confirm dialog (the connector revoke has none).

## 2. Exact tested paths

| Path | Actual traffic path (verified) | Tested? |
|---|---|---|
| **A. Direct** | Client → Zecurity tunnel (QUIC to connector :9092) → Connector → app .75:51713. Evidence: client `new TCP connection` / `tunnel opened … port=51713`; connector `session registered … resource_id=261a549d … transport="quic"` and `tunnel_opened ok resource_id=261a549d dest=192.168.1.75:51713` | yes |
| **B. Relay-routed** | No active relay exists; the connector's relay list is empty. No session traversed a relay | **BLOCKED** |
| **C. Shield** | Client → Zecurity tunnel (QUIC to connector) → Connector → Shield (`31ceacd3`) → app .75:51714. Evidence: connector `tunnel_opened ok shield=31ceacd3-… resource_id=ab4d488e`; session label `transport="relay"`, which is the connector's label for the **shield-relay** child path, *not* a Relay server (`connector/src/device_tunnel.rs:241-248`, `session_registry.rs:25-33`). Direct LAN to :51714 was blocked by the shield (curl `000 8.00` with the tunnel down at 16:39) | yes |
| **D. New-connection probes** | `f5c5-newconn.sh`: a NEW curl every 2 s to each port (`-m 8`), logging code, `time_total`, rc. Tunnel 200 ≈ 15–20 ms; client fail-closed RST = `000`, ~6 ms, rc 56 | yes |

## 3. Test procedures and actual outcomes

**Instruments** (local units are `systemd-run --user`):

| Unit / file | What it samples |
|---|---|
| `rev-hold-D` / `rev-hold-S` → `rev-hold-{D,S}.log` | `rev_hold.py`: one keep-alive socket; `GET /` every 2 s on the SAME socket; logs every request with local port. It never reconnects; on error it logs `FAIL <type>` and exits |
| `rev-nc-D` / `rev-nc-S` → `rev-newconn.log` | new-connection probes (above) |
| `rev-tunsamp` → `rev-tunsamp.log` | change-only: `zecurity0` ifindex, client PID, policy-routing hash |
| `.75:/tmp/rev-connsamp.sh` → copy at `.75:~/rev-connsamp-2026-10-12.log` | change-only, 1 s: connector PID / NRestarts / state, shield PID / state, UDP :9092 listener, TCP :9091 listener, established TCP to controller :9090, established on :9091 (shield link) |
| `rev-crl.sh` | fetches `/ca.crl?workspace_id=…` (existing endpoint), verifies the signature against the workspace CA public cert (`workspace_ca_keys.certificate_pem`) with `openssl crl -CAfile`, lists serials |

**Sequence:**
1. Baseline from 16:41:26.
2. Holds started 16:41:32.
3. Revoke at 16:44:04.
4. Observed through cert expiry (16:57:17.951) to 17:03:32.
5. Holds stopped 17:03:32; probes stopped 17:03:55.

### REV-DIRECT-EXISTING: **OBSERVED_CONTINUES**

| Fact | Evidence |
|---|---|
| Session identity | the hold's local port stayed **41390** for all 657 requests (`lport=` on every line; one distinct value). Connector session **`c2aa0d99`** (resource `261a549d`, transport `quic`) registered 16:41:32.376, **unregistered 17:03:32.381**, i.e. 0.3 s after `HOLDS-STOPPED 17:03:32.407` |
| Requests | 76 × 200 before revoke; **15 × 200** revoke → client sync; **380 × 200** client sync → cert expiry; **186 × 200** after cert expiry. **0** FAIL, **0** SOCKET CHANGED, 0 non-200; max RTT after revoke 16 ms |
| Closed / reset / stalled? | no: no error, no reset, no stall (every 2 s request answered) |
| Client route change / reconnect | none for this flow. The client's 16:44:33.999 transport hot-apply (`reachable=0`) did not touch the established flow. Tunsamp: ifindex 12 / PID 2426 / policy hash unchanged for the whole run (only its start line) |
| Connector session-termination event | none until the hold was stopped by the tester |
| Through the client sync | yes: sync at 16:44:33.986, and the flow continued for 19 min afterwards |
| Final state of the original session | ended **only** when the tester stopped the hold (17:03:32). No replacement session was created by the hold (it can't reconnect) |

Proves: an established direct client → connector flow, opened before revocation, kept carrying application traffic:
- for ≥ 19 min 28 s after the persisted revocation (16:44:04.481 → 17:03:32);
- across the client's snapshot sync that removed the connector;
- for ≥ 6 min 14 s after the revoked certificate's `notAfter`.

Does not prove: behaviour on longer horizons, or on other client builds.

### REV-RELAY-EXISTING: **BLOCKED**
No active relay exists in the lab (all relay rows `inactive`/`revoked`/`deleted`/`pending`; connector `LabelledRelayList is empty`). No session traversed a relay, so the relay's (non-)recheck of an established session was **not observed**. No other path was substituted.

### REV-NEW-CONNECTION: per path and window

| Window | Direct probes (51713) | Shield probes (51714) | Rejecting component |
|---|---|---|---|
| Pre-revoke (→ 16:44:04.481) | 78 × 200, rc 0 | 78 × 200, rc 0 | — |
| **A. Revoked, before client sync** (16:44:04.481 → 16:44:33.986) | **15 × 200** (tunnel ~15–20 ms) | **15 × 200** | **none: new connections succeeded through the revoked connector.** The connector logged `tunnel_opened ok` for **7 direct + 7 shield** new sessions before its control stream closed (16:44:17.92), and **8 direct + 8 shield** after it closed (last 16:44:33.545). The client's last known config was ACL v5 / transport v1, both naming the connector. The connector's CRL cache state could not be inspected (§12) |
| **B. After client sync** (16:44:33.986 → 16:57:17.951) | 0 × 200; 371 rc 56, 4 rc 55, 2 rc 7, all `000` in ~6 ms | 0 × 200; 375 rc 56, 1 rc 55, 1 rc 7 | **client**: `connector offline — failing closed for managed resource` (`net_stack`) on every probe from 16:44:35.588. The first fail-closed probe was at 16:44:35.595, 1.6 s after the client's `transport map hot-applied … reachable=0` (16:44:33.999). The resource stays inside the TUN (no direct-LAN fallback for unprotected 51713) |
| B'. After cert expiry (16:57:17.951 →) | 0 × 200; 192 rc 56, 4 rc 55 | 0 × 200; 193 rc 56, 3 rc 55 | client (same mechanism) |
| C. After relay CRL refresh | — | — | **BLOCKED** (no relay) |

Client sync trigger: the regular 60 s ACL tick (tick phase `hh:mm:33.98`; no early resync). Versions:
- **ACL v6** (`acl push … version=6 entries=2 connectors=1` at 16:44:04; entries unchanged, version bumped);
- **transport v2**;
- client logs `ACL snapshot synced version=6 entries=2` → `transport snapshot stored version=2` → `transport-only change, hot-applying connector map` → `transport map hot-applied successfully resources=2 reachable=0` (16:44:33.986–.999).

The connector component's rejection of new sessions after the client sync could **not** be exercised, because no supported client path selects the revoked connector after sync (see §15). **INCONCLUSIVE for the connector-side post-sync rejection.**

### REV-CONNECTOR-RECONNECT
**Control-plane outcome: control stream closed; reconnects rejected.**
- 16:44:17: controller logs `revoked/deleted — closing control stream` then `disconnected`. This was at the first health report after revocation, 12.6 s after `revoked_at`.
- 16:44:17.917: connector logs `ERROR … control stream recv error: code: 'The caller does not have permission …', message: "connector revoked"`.
- Reconnect loop continued:
  - preflight `controller SPIFFE identity verified`, then `failed to open Control stream`;
  - backoff 4 → 8 → 16 → 32 → 60 s, then every 60 s;
  - 24 failures by 17:03:20, before and after cert expiry.
- No "certificate expired" message appeared on the connector side; the error text stayed `failed to open Control stream`.
- The connector never re-activated: DB stays `revoked`.

**Data-plane outcome: continues serving.**
- Process: PID 49648, NRestarts 0, `active` throughout.
- Listeners: UDP :9092 and TCP :9091 listening throughout.
- Shield link: established :9091 = 1 throughout.
- Control connection: established TCP to :9090 went 2 → 1 at 16:44:18.834 and stayed 1.
- New sessions were accepted in window A, including **after** the control stream was rejected (16:44:17.92–16:44:33.55, 16 new sessions).
- Existing sessions were served until 17:03:32.
- At 16:58:50 (after `notAfter`) the connector was still presenting the revoked and expired cert `D404C944…` (notAfter 11:27:17 GMT) on :9091.

**Classification: control plane STOPPED; data plane OBSERVED_CONTINUES.** It didn't exit or shut down, and wasn't restarted.

### REV-SHIELD: **OBSERVED_CONTINUES** (data plane). Shield control-plane status changed indirectly

| Item | Observation |
|---|---|
| Shield ↔ connector link | stayed established (:9091 established = 1) the whole run. Shield process PID 50629 unchanged. The shield journal shows **no** connector-related event after revocation (no disconnect, no peer change, no CRL) |
| Peer list | no `PeerConnectorList` change logged by the shield (only the filtered `unchanged` lines) |
| Application connectivity | existing shield-routed hold `a6623d99` (resource `ab4d488e`): **657 × 200**, same local port 37338, 0 FAIL, through revoke, client sync and cert expiry. Ended only at the tester's stop (connector `session unregistered` 17:03:32.381). New shield-routed connections: 200 in window A (15); client fail-closed after sync |
| Shield control-plane status | DB `shields.status` went `active` → **`disconnected`** at 16:46:11 (`shield disconnect watcher: marked 1 shield(s) disconnected`). Its last heartbeat was 16:43:52, because shield status reaches the controller only via its connector's control stream, which was closed at 16:44:17. **Indirect effect; the shield did not detect anything itself** |
| Failover | no alternative eligible connector existed in RN `s20f5-rn2`, so failover was not testable. Not OBSERVED_FAILOVER |
| Does the shield detect revocation itself? | no evidence of any shield-side detection. Consistent with source: no CRL code in `shield/src` |

## 4. Connector and certificate identifiers

| Item | Value |
|---|---|
| Connector | `bde51440-910b-42a3-9823-99e116b2a6c4` (`s20rev-conn-75`) |
| Cert at enrollment | serial `9c32beb15202d8e719746e0888ef3937`, notAfter 2026-10-12 11:22:17 UTC (**16:52:17 IST**) |
| Cert at revocation (after renewal at 16:42:17) | serial **`d404c9447a8a51345cc2693d0da57120`**, notAfter 2026-10-12 11:27:17.951 UTC (**16:57:17.951 IST**). Confirmed the same on the wire (`openssl s_client :9091` 16:43:20 and 16:58:50) |
| Revocation persisted | `revoked_at` 2026-10-12 11:14:04.481568 UTC (**16:44:04.481 IST**), `revocation_reason='revoked via admin API'`, `status='revoked'` |
| Workspace / tenant | `0d3b8f09-0338-4d03-8729-c4e12b052a4e` |
| Shield | `31ceacd3-9a89-4945-8067-b1c72ebf0e3d` |
| Resources | direct `261a549d-b987-4a87-be02-0d88a51cae0a`; shield `ab4d488e-38a2-4e55-8a25-b5c2da6bb79a` |
| Held connector sessions | direct `c2aa0d99-882a-4e63-8899-296a1c9ee443` (quic); shield `a6623d99-96ce-469b-98e8-cd9d655a6fad` (shield-relay path) |
| Client device | `spiffe://ws-xiyo.zecurity.in/client/03cb5cf7-ab64-4726-9167-033f679fe3a4` |

No private keys, tokens or passwords appear in this record. The enrollment tokens are in 0600 run-dir files (`rev-conn-75.env`, `rev-shield-75.env`; spent), see §13.

## 5. Chronological event timeline (IST)

| Time | Component | Event | Source |
|---|---|---|---|
| 16:14:11 | controller | relaunched (`:8080`, `:9090`) | controller.log |
| 16:37:17 | connector | enrolled, `Control stream established`; controller `connected` | .75 journal, controller.log |
| 16:38:22 | shield | `enrollment complete` → `Shield Control stream established` | .75 shield journal |
| 16:39:13 | controller | `s20rev-web` protected; ACL v3→v5 (16:39:05–:41, `entries=2`) | DB, controller.log |
| 16:40:24 | client | `zecurity-client up`; both resources 200 through the tunnel | client journal |
| 16:41:26.222 | run | instruments start (tunsamp ifindex 12 / PID 2426) | rev-events.log |
| 16:41:32.376/.377 | connector | hold sessions `c2aa0d99` (direct) / `a6623d99` (shield) registered | .75 journal |
| 16:41:32.383 | hold | HOLD-D lport 41390, HOLD-S lport 37338 START | rev-hold-*.log |
| 16:42:17–18 | controller | renewal: `ReEnroll sent` → `connected` → `superseded stream closed` (serial → `d404…`) | controller.log |
| 16:42:23.869 | CRL | pre-revoke CRL: signature OK, **0 serials**, target ABSENT | crl-pre-164223.{der,txt} |
| 16:44:00.107 | tester | Revoke click sent (pointer travel ≈ 4 s) | rev-events.log |
| **16:44:04.481** | controller/DB | **`revoked_at` persisted** (`status=revoked`) | DB |
| 16:44:04 | controller | `acl push … version=6 entries=2 connectors=1` | controller.log |
| 16:44:04.608 | connector | `ACL snapshot stored version=6` (still on its control stream) | .75 journal |
| 16:44:10.426 | UI | page shows Delete Connector (Revoke gone): the mutation had returned by this time (upper bound) | rev-events.log |
| **16:44:10.467** | CRL | fresh CRL: signature **verify OK**, 1 serial, **`D404C944…` PRESENT**, Revocation Date 11:14:04 GMT, thisUpdate 11:14:10, nextUpdate +24 h | crl-post1-164410.{der,txt} |
| 16:44:17 | controller | health report → `revoked/deleted — closing control stream` → `disconnected` | controller.log |
| 16:44:17.917 | connector | `PermissionDenied … "connector revoked"`; reconnect loop begins | .75 journal |
| 16:44:18.834 | connector host | established TCP to :9090 2 → 1; PID / listeners unchanged | connsamp |
| 16:44:19.9 → 16:45:20 | connector | `failed to open Control stream`, backoff 4/8/16/32/60 s | .75 journal |
| 16:44:17.92–16:44:33.55 | connector | 16 new sessions accepted (`tunnel_opened ok`) while revoked with the control stream closed | .75 journal |
| 16:44:33.567 | probes | last new-connection 200 (both ports) | rev-newconn.log |
| **16:44:33.986** | client | **ACL v6 synced; transport v2 stored; hot-applied `reachable=0`** (60 s tick) | client journal |
| 16:44:35.588 / .595 | client / probes | `connector offline — failing closed`; first `000 rc=56` | client journal, rev-newconn.log |
| 16:46:11 | controller | `shield disconnect watcher: marked 1 shield(s) disconnected` | controller.log |
| 16:52:17 | — | previous cert `9c32…` notAfter (this serial was **never** on the CRL) | DB / §8 |
| **16:57:17.951** | — | **revoked cert `d404…` notAfter** | DB, wire |
| 16:58:50 | connector | still presents `D404C944…` (expired) on :9091 | openssl |
| 17:03:32.407 | tester | holds stopped (both 657 × 200, 0 FAIL) | rev-events.log |
| 17:03:32.381 | connector | `session unregistered` for `c2aa0d99` and `a6623d99` (from the client closing) | .75 journal |
| 17:03:44.993 | connector | `WARN crl: CRL refresh failed: CRL thisUpdate is in the future` (single occurrence; both hosts NTP-synchronized) | .75 journal |
| 17:03:55.268 | tester | probes stopped | rev-events.log |

## 6. Evidence references

**Controller host** `~/s20-run/logs/rev/`:
- `rev-events.log`: markers.
- `rev-hold-D.log`, `rev-hold-S.log`: every request.
- `rev-newconn.log`.
- `rev-tunsamp.log`.
- `baseline-1.txt`, `baseline-1-remote.txt`, `pre-revoke.txt`, `post-1.txt`, `remote-post-1.txt`, `remote-newconn-window.txt`, `remote-post-expiry.txt`, `remote-final.txt`.
- `snap-main.txt`: periodic snapshots 16:45:47, 16:52:04, 16:58:41, 17:03:2x.
- `analysis-final.txt`: window tallies (`rev-analyze.sh`).
- `crl-pre-164223.{der,txt}`, `crl-post1-164410.{der,txt}`, `ws-ca.pem` (public cert).
- `controller-excerpt.log`, `client-journal-excerpt.log`.

Also `~/s20-run/logs/controller.log` (full controller log).

**Connector host .75** `~/`:
- `rev-connsamp-2026-10-12.log`;
- `rev-connector-journal-filtered-2026-10-12.log` (connector journal since 16:37 without per-session lines; the per-session lines remain in the system journal);
- `rev-shield-journal-2026-10-12.log`.

**Scripts** (run dir, not repo): `rev_hold.py`, `rev-instr.sh`, `rev-holds.sh`, `rev-connsamp.sh`, `rev-crl.sh`, `rev-base.sh`, `rev-remote.sh`, `rev-snap.sh`, `rev-analyze.sh`, plus the existing `f5c5-newconn.sh` and `f5c5-tunsamp.sh`.

## 7. Existing vs new connections

| | Existing (opened before revoke) | New |
|---|---|---|
| Direct | **continued** through revoke, client sync and cert expiry (19 min 28 s, 581 × 200 after revoke) | **succeeded** until client sync (29.5 s window); **client fail-closed** after |
| Shield | **continued** (same) | same as direct |
| Relay | BLOCKED | BLOCKED |

## 8. Before vs after client synchronization

- **Before sync** (29.5 s; 16:44:04.481 → 16:44:33.986):
  - The client still held ACL v5 and transport v1.
  - New flows were opened through the revoked connector and accepted by it, including after its control stream was rejected.
- **After sync:**
  - The client stopped selecting the connector (`reachable=0`) and resets new flows itself.
  - The client did **not** close the two established flows (Phase 2-A hot-apply; flows pinned to their stream).
- **The client does not use the workspace CRL for connector identity:**
  - no CRL activity in the client journal;
  - consistent with source (`PrefixSpiffeVerifier`, no connector CRL check).
- **Mechanism of the post-sync rejection:** snapshot absence, not CRL.

## 9. Before vs after relay CRL refresh
**BLOCKED.** No active relay, so no relay CRL fetch/load could be observed, and no relay-routed session existed.

## 10. Connector reconnection behaviour
See REV-CONNECTOR-RECONNECT in §3:
- the control stream was closed within one health interval;
- every reconnect was rejected (`failed to open Control stream`);
- the reconnect loop runs indefinitely at a 60 s backoff;
- the data plane (process, listeners, ACL cache, sessions) was unaffected;
- the connector did not exit, and the DB stays `revoked`.

The connector logs `PermissionDenied … "connector revoked"` once (stream recv). Later attempts log only the generic `failed to open Control stream`.

## 11. Shield behaviour and failover
See REV-SHIELD in §3:
- **data plane continued**;
- **link to the revoked connector stayed up**;
- **no shield-side detection**;
- control-plane status went `disconnected` indirectly (watcher, 16:46:11);
- **failover not testable** (no alternative connector).

## 12. CRL freshness and cache observations

| Question | Observation |
|---|---|
| When the controller's CRL includes the serial | in the first fetch after revocation: 16:44:10.467, 6.0 s after `revoked_at` (signature verified with the workspace CA; Revocation Date = `revoked_at`). The CRL is generated per request (thisUpdate = fetch time) |
| Which serial | only the current serial `d404…`. The pre-renewal serial `9c32…` (valid until 16:52:17) was **not** listed. Consistent with `cert_serial` being overwritten on renewal |
| Connector's last workspace-CRL fetch | **not observable**: the connector logs CRL refresh only on failure. One failure was logged at 17:03:44.993 (`thisUpdate is in the future`; both hosts NTP-synchronized, so it's likely sub-second skew). This shows the refresh loop runs, but successful fetch times and cache contents cannot be read |
| Connector's local CRL contains its own serial? | **cannot be inspected** (in-memory). Whatever it contains, the connector served its own revoked identity for 19+ min, consistent with source: it only checks *client device* serials |
| Relay's CRL fetch/load | BLOCKED (no relay) |
| Client's use of the workspace CRL for the connector | none observed (no CRL lines in the client journal over the run) |
| Shield CRL path | none observed (no CRL lines; consistent with source: no CRL code in `shield/src`) |
| Existing sessions rechecked after a CRL refresh | no termination of either held session by any component |
| Explicit action terminating established sessions | **none** observed. Both sessions ended only when the tester stopped the holds |
| Stale CRL/ACL state in use | the connector kept serving with its cached ACL v6 (received 16:44:04.608, after the revocation was persisted) |

No prolonged outage or CRL-expiry (24 h) test was attempted.

## 13. Blocked tests and cleanup limitations

| Test | Reason |
|---|---|
| REV-RELAY-EXISTING | no active relay in the lab; the connector's relay list is empty |
| REV-NEW-CONNECTION part C (after relay CRL refresh), relay part of A/B | same |
| Shield failover to another connector | no second eligible connector in RN `s20f5-rn2` |

**Cleanup status (safe, disposable):**
- **Done:** holds and probes stopped (17:03:32 / 17:03:55); `rev-tunsamp` stopped; `.75` connsamp stopped.
- **Left in place:**
  - the revoked connector `s20rev-conn-75` is still running on .75 (reconnect loop). It is not un-revoked, and stopping it needs sudo;
  - shield `s20rev-shield-75` (DB `disconnected`);
  - resource `s20rev-web` (protected) and its group assignment;
  - vite on .75:51713 / :51714;
  - controller and admin units;
  - spent enrollment-token env files (`~/s20-run/rev-{conn,shield}-75.env`, `.75:~/s20fc/cbundle/conn.env`, `.75:~/s20f3/f3bundle/sbundle/shield.env`).

  These await the user's decision. No shared resource was modified.

## 14. Inconclusive results

1. **Connector-side rejection of a new session after the client sync and after cert expiry.**
   - After sync, no supported client selects the revoked connector, so whether the connector would still accept a new QUIC session from a valid client after `notAfter` was not exercised.
   - The connector does keep serving TLS with its expired cert on :9091.
2. **The source of the 16 window-A sessions.** The client journal can't tell whether they rode the pooled QUIC connection or a fresh handshake.
3. **Connector CRL cache contents**, and the connector's successful CRL fetch times (not logged).

## 15. Final evidence summary

| Test | Result | Proves | Does not prove |
|---|---|---|---|
| REV-DIRECT-EXISTING | **OBSERVED_CONTINUES** | an established direct flow survives connector revocation, client sync and revoked-cert expiry (≥ 19 min 28 s, 0 errors) on the current build | behaviour beyond 19 min, or with other client builds |
| REV-RELAY-EXISTING | **BLOCKED** | — | relay recheck behaviour |
| REV-NEW-CONNECTION: before sync | **OBSERVED_CONTINUES** (new flows accepted; 15 per port in 29.5 s) | the connector accepts new sessions while revoked, including after its control stream is rejected | the window length on other tick phases (bounded by the 60 s client tick) |
| REV-NEW-CONNECTION: after sync | **OBSERVED_STOPPED by the client** (fail-closed, snapshot-based) | the client stops selecting a connector absent from its transport snapshot | any CRL-based client or connector rejection |
| REV-NEW-CONNECTION: relay | **BLOCKED** | — | — |
| REV-CONNECTOR-RECONNECT | control plane **STOPPED** (stream closed 12.6 s after revoke; all reconnects rejected); data plane **OBSERVED_CONTINUES** | the controller enforces revocation on the control plane; the connector doesn't react on the data plane | connector new-session rejection after sync (INCONCLUSIVE) |
| REV-SHIELD | data plane **OBSERVED_CONTINUES**; shield control status → `disconnected` (indirect); failover **BLOCKED** | the shield neither detects nor reacts to its connector's revocation; shield-routed existing flows continue | shield behaviour with an alternative connector |
| CRL generation | serial present in the first post-revoke CRL (6 s), signature valid | controller CRL generation works for connector revocation (live) | distribution to relays (BLOCKED) |

**Relation to Finding 34** (run sheet, 09-28, pre-2-A): the *existing-flow* part of the behaviour recorded there is **observed again on the current build** (post-2-A). New-flow behaviour differs: after sync the client fails closed instead of reusing the connector. Finding 34 itself was **not** edited (per instruction).

## 16. Follow-up questions before deciding on an implementation

1. Which component(s) must terminate **established** flows through a revoked connector: the connector itself, the client, the shield, the relay (when present)? Within what bound (immediate, next sync, cert expiry)?
2. Is the **pre-sync window** acceptable? Today a revoked connector accepts new sessions for up to one client tick (≤ 60 s; 29.5 s here), and does so even after its control stream has been rejected.
3. Should the connector treat control-plane `PermissionDenied "connector revoked"` as authoritative (stop listeners / sessions), given that it currently logs it once and then just retries?
4. Should the **previous serial** after a renewal be revocable? A pre-renewal certificate valid for up to the TTL is not on the CRL.
5. Should the shield react to its connector's revocation, and should failover to another connector be tested once a second connector exists in the RN?
6. The relay path is untested: should a relay be provisioned for a follow-up run of REV-RELAY-EXISTING and the relay CRL timing?
7. Should a longer-horizon run (beyond 19 min, and beyond the 24 h CRL `nextUpdate` under a controller outage) be planned separately with its own safety plan?
8. Should the CRL refresh failure `thisUpdate is in the future` (sub-second skew) be tracked as a minor robustness issue?
9. Cleanup decisions: stop/uninstall the revoked connector on .75 (sudo), revoke the shield, delete `s20rev-web`, remove the spent token env files, stop vite.
