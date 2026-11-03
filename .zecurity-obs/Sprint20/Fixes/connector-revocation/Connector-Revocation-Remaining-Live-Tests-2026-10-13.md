---
title: Connector Revocation — Remaining Live Tests (R1–R4)
date: 2026-10-13
type: live-verification-evidence
status: historical evidence (pre-design) — reviewed; superseded by the implementation records (phases A–F) and the end-to-end live run 2026-10-23
branch: feat/sprint20-m1-phase2
baseline: 9844a00
previous_record: Connector-Revocation-Live-Verification-2026-10-12.md (historical, unchanged)
---

# Connector Revocation — Remaining Live Tests (2026-10-13)

This record covers **only** what was observed in the 2026-10-13 session.

Findings A–H from the 2026-10-12 record are historical and are not restated as new results.

All times are **IST (UTC+05:30)** unless marked UTC. Result classes are the ones defined in the task.

None of the results below says that a security requirement is satisfied.

## 1. Environment and safety verification

| Item | Value / check |
|---|---|
| Repo | `~/zecurity`, branch `feat/sprint20-m1-phase2`, HEAD `9844a00`. The pre-existing working tree was unchanged before and after the run; the only addition is this file |
| Hosts | All hosts had rebooted overnight: .164 (controller and client) up since 10:02, .75 since 10:23, .49 since 10:40. Before the run, the controller and admin units were **inactive** and no agent was active in the workspace |
| Controller | **Fresh build of HEAD `9844a00`**: `~/s20-run/build-r/controller-9844a00`, sha `9d19d90c`, started 14:32:38 as user unit `s20-controller` via `~/s20-run/rev2-ctl.sh` |
| Controller env | `CONNECTOR_CERT_TTL=15m`, `CONNECTOR_RENEWAL_WINDOW=10m`, `SHIELD_CERT_TTL=168h` (unchanged lab parameters). The HEAD build refuses to start without `PROVIDER_JWT_SECRET` (`controller/internal/provider/identity.go:134`). A random per-run secret was supplied as process env from a 0600 file (deleted at cleanup); no config file was edited |
| Note on 10-12 | The 10-12 run used the older lab binary `controller-f5c`, built 10-06 15:07, i.e. before commit `4781bc8`. That run's controller was therefore not exactly HEAD. The 10-12 record is unchanged; this is noted as an open question (§13) |
| Admin UI | `s20-admin` (vite :5173); signed in as the admin Google user. The client uses a different user, so there is no session eviction |
| Workspace | `xiyo` `0d3b8f09-0338-4d03-8729-c4e12b052a4e` (lab) |
| Client | .164, `zecurity-client` sha `032a0cf4`, PID 2418, `zecurity0` ifindex 12 (unchanged over the whole run, per tunsamp) |
| Previously revoked connector | `bde51440` (`s20rev-conn-75`): DB `revoked`, `revoked_at` 2026-10-12 11:14:04 UTC, serial `d404…`. **Not touched**, and its serial is still on every CRL fetched today. Its stopped install on .75 was replaced by connector A's install, because one connector unit per host is the only supported layout; its DB row was not touched |
| Isolation | Lab LAN only. All agents are new, disposable identities created for this run. No other active connectors, shields or relays were in the workspace. The only client is this host's |

## 2. Tests attempted, completed, blocked, inconclusive

| Test | Status |
|---|---|
| R1 existing relay-routed connection | **BLOCKED** (no relay can be offered to connectors in this LAN lab; see §7) |
| R2 shield failover | **Completed**: OBSERVED_CONTINUES (no failover) |
| R3 connector enforcement after client sync | As written: **INCONCLUSIVE** (no supported probe can bypass client routing). **Variant R3-V** (approved): **completed**, OBSERVED_CONTINUES |
| R4 previous serial in CRL | **Completed**: CONFIRMED_SERIAL_HISTORY_GAP (absence while valid confirmed live; usability inferred from source, not exercised) |
| CRL cache observation | partial (§8) |

## 3. Topology and path verification

| Element | Identity | Host |
|---|---|---|
| Connector **A** (revocation subject) | `s20rev2-a-75`, `fad7c06b-e66b-42fa-8f5d-88fb0e065d2f`, RN `s20f5-rn2` | .75, binary sha `19e36cee64c4a4fc` (connector source unchanged since `2a44ab2`), update timer disabled |
| Connector **B** (alternative) | `s20rev2-b-49`, `84c49845-e7bb-4885-b997-4843dc0ea72e`, same RN | .49, same binary sha, update timer disabled. This replaced the stale `manoj` connector install on .49 |
| Shield | `s20rev2-shield-75`, `909d233e-ddf9-4116-b421-9257eff5d675`, `interface_addr` 100.64.0.5/32 | .75, binary sha `30ba3e39785720d6` |
| Shield binding | **A**: DB `shields.connector_id = fad7c06b` and enrollment-token JWT `connector_id = fad7c06b`. The shield detail page label showed `s20rev2-b-49`; this is the known stale-label issue, and DB and JWT agree on A | — |
| Protected resource | `s20rev2-web`, `f6ae2ea0-bc51-47c2-b693-b9f8d08b5e93`, .75:51714 (test vite app), assigned to group `s20-summa-users` | — |
| Unprotected resource (control) | `s20f3-unrel`, `261a549d-…`, .75:51713 (pre-existing assignment) | — |

**Path evidence (pre-revoke):**
- Shield journal at 14:40:42:
  - `Shield Control stream established … connector_id=fad7c06b`;
  - then `peer connector list refreshed peers=2` (A and B).
- Connector sampler on .75: established :9091 = 1 (the shield link). On .49: established :9091 = 0, so B had no shield link.
- **HOLD-S** (51714, local port **34554**) = connector **A** session `9ebabfa2`:
  - registered 14:43:44.208, label `transport="relay"`, which is the connector's label for the **shield** path (`connector/src/device_tunnel.rs:241-248`), not a Relay server;
  - unregistered **15:07:24.427**, when the tester stopped the hold.
- **HOLD-D** (51713, local port **57482**) = connector **B** session `efce4f7d` (`quic`): registered 14:43:44.187, unregistered 15:07:24.370.
  - B was first in the client's transport order for the unprotected resource.
  - This hold therefore runs through the **non-revoked** connector and serves as a control. It is not a revoked-path test.
- New-connection probes every 2 s:
  - 51714 probes went to A (preferred connector = the shield's connector);
  - 51713 probes went to B.

## 4. Identifiers and certificate lifecycle (connector A)

| # | Serial | notBefore (UTC) | notAfter (UTC / IST) | Source |
|---|---|---|---|---|
| 1 | `7663EED229533F1D91202A24F41376FC` | 08:09:07 | 09:24:07 / **14:54:07** | enrollment 14:39:08; wire sample (`openssl s_client 192.168.1.75:9091`) 14:43:15 |
| 2 | `8F1380225AB86B2A1047269336DC6528` | 08:14:08 | 09:29:08 / **14:59:08** | renewal 14:44:08 (controller `ReEnroll sent` → `superseded stream closed`); wire sample 14:45:04 |
| 3 | `8891EE053E2AF72E0F9C8288DC79F68B` | 08:19:23 | 09:34:23.858 / **15:04:23.858** | renewal 14:49:23; connector `certificate renewed, persisted and published serial=8891…`; wire sample 14:49:26 |

Notes:
- notBefore is backdated about 1 h from issuance (observed, not investigated).
- DB `connectors.cert_serial` always held only the latest serial: `7663…` → `8f13…` → `8891…`.

## 5. Revocation and CRL events (IST)

| Time | Event | Evidence |
|---|---|---|
| 14:49:35.862 | Pre-revoke CRL: signature verify OK; 1 serial (`D404C944…`, from 10-12); `8F13…` absent | `crl-pre-144935.{der,txt}` |
| 14:50:00.162 | R3-V client-isolation block starts (240 s) | `rev2-client-block.log` |
| 14:50:05.014 | Revoke click sent (admin UI connector page; GraphQL `revokeConnector`; no confirm dialog) | `rev2-events.log` |
| **14:50:05.461** | **`revoked_at` persisted**, `status=revoked`, reason "revoked via admin API", `cert_serial=8891…` | DB (`revoke-db.txt`) |
| 14:50:05 | `acl push … version=7 entries=2 connectors=2`. Both connectors log `ACL snapshot stored version=7` (A at 14:50:05.480) | controller.log, connector journals |
| ≤ 14:50:10.4 | UI shows Delete Connector (the mutation had returned) | `rev2-events.log` |
| **14:50:10.811** | CRL: signature OK; 2 serials. **`8891…` PRESENT** (Revocation Date 09:20:05 GMT). **`8F13…` ABSENT. `7663…` ABSENT.** thisUpdate 09:20:10, nextUpdate +24 h | `crl-post-*-145010.{der,txt}` |
| 14:50:08 | Controller: `revoked/deleted — closing control stream` → `disconnected`. A (14:50:08.846): `PermissionDenied … "connector revoked"`, then `failed to open Control stream` with backoff 4/8/16/32/60 s, continuing | controller.log, A journal |
| 14:52:38 | `shield disconnect watcher: marked 1 shield(s) disconnected` | controller.log |
| 14:54:00.205 | Block ends (`UNBLOCK`, 54 client packets dropped) | `rev2-client-block.log` |
| **14:54:01.094** | **Client sync:** `transport snapshot stored version=3` → `transport-only change, hot-applying connector map` → `hot-applied … resources=2 reachable=2`; `ACL snapshot synced version=7 entries=2` → `effective config unchanged` | client journal |
| 14:54:01.745 | First 51714 probe fails (client-side RST, rc 56) | `rev2-newconn.log` |
| 14:54:07 | Serial #1 `7663…` expires (never on the CRL) | §4 |
| 14:59:08 | Serial #2 `8F13…` expires (never on the CRL) | §4 |
| 15:04:23.858 | Serial #3 `8891…` (revoked) expires | §4 |
| 15:07:24 | Holds stopped; probes stopped at 15:07:27 | `rev2-events.log` |
| post-cleanup | CRL: 2 serials (`D404…`, `8891…`), signature OK | `crl-final-*.txt` |

## 6. Results

### R3-V: connector enforcement on new sessions, client sync delayed (variant): **OBSERVED_CONTINUES**

**Variant definition (labelled; approved by the user):**
- The existing lab harness `f3-client-block.sh` (copied as `rev2-client-block.sh`) dropped **only** the client daemon's TCP traffic to the controller (:8080/:9090), matched by its cgroup, from 14:50:00 to 14:54:00.
- The client therefore kept its pre-revoke connector map and kept selecting A.
- This isolates the connector's own behaviour, but it is **not** "after client sync" as the original R3 is worded.

**Observed, server-side (A's journal):**
- **115** `tunnel_opened ok` new sessions between `revoked_at` (14:50:05.461) and the client sync (14:54:01.094).
- **114** of these came **after** A's control stream was rejected with PermissionDenied (14:50:08.846). The last was at 14:53:59.700.
- Client side: 115 × 200 on 51714 new-connection probes in the same window.
- No `denied`, revocation or CRL-rejection line appears for any of them.
- The connector's certificate (`8891…`) was valid throughout (until 15:04:23).

**What this proves:**
- For ~3 min 56 s after revocation, the running connector accepted and served new client sessions, with a still-valid certificate and a rejected control plane.
- It had ≥ 3 periodic CRL refresh opportunities in that window. Its refresh interval is 60 s + jitter (`connector/src/main.rs:178-191`); no refresh *success* is logged, so loads cannot be confirmed (§8).

**What it does not prove:**
- That a *new QUIC handshake* was accepted. The client did not rebuild its pool before 14:54:01, so these sessions most likely rode the QUIC connection established before revocation (inference). The connector does not log QUIC connection accepts.
- Connector behaviour after its certificate expired: no client selected A after 14:54:01. Behaviour on a fresh handshake after expiry remains **INCONCLUSIVE**.

**Original R3 (after sync, normal routing): INCONCLUSIVE.**
- After 14:54:01, the client sent no 51714 session to A: 0 `tunnel_opened` on A.
- The client CLI has no diagnostic connect command (`zecurity-client --help`: setup, login, status, resources, sync, logout, up, down).
- Missing capability: a supported probe that dials a specific connector with a valid device identity, independently of the snapshot.

### R2: shield failover to an alternative connector: **OBSERVED_CONTINUES** (no failover)

| Question | Observation |
|---|---|
| **A. Does the shield switch to the alternative connector?** | **No.** On .75 the shield's link to A stayed established (:9091 established = 1) at every sample until 15:06:39, including after A's cert expiry (15:04:23). Shield PID 17690, NRestarts 0. The shield journal has **no** line after 14:49:57 except periodic discovery: no disconnect, no reconnect, no peer change, no CRL. The 15 s `PeerConnectorList` messages from A **stopped** after revocation (last 14:49:57); the shield did not react to their absence. On .49, B never had a shield link (:9091 established = 0) |
| **B. Do new application connections work through B?** | **No.** After the client sync (14:54:01) the client dropped A and tried B for 51714. B logged **379** `tunnel_opened error shield=909d233e…` / `QUIC stream error: shield 909d233e… not connected`. The client logged `shield not attached, trying next connector` → `no connector accepted tunnel (auth/denial only) — failing closed`. Probes: 307 × `000` rc 56 (sync → A expiry) + 84 × `000` rc 56 (after expiry), 0 × 200 |
| **C. Does the original established session survive?** | **Yes.** HOLD-S (local port 34554, A session `9ebabfa2`): **702 requests, all 200, 0 FAIL / 0 SOCKET CHANGED**. Of these, 117 fell between revoke and client sync, 310 between client sync and A's cert expiry, and **85 after A's certificate expired**. It ended only when the tester stopped it (A `session unregistered` 15:07:24.427) |
| **D. Migrated or replaced?** | **Neither.** Same socket and same connector session throughout; no session for the shield resource ever appeared on B |
| Shield control-plane status | DB `active` → `disconnected` at 14:52:38 (watcher), because its status reached the controller via A's (closed) control stream. This is an indirect effect, not detection by the shield |
| Alternative connector eligibility | B was in the shield's peer list (`peers=2` at 14:40:42) and is in the same RN. B's runtime evidence shows it can't serve the shield resource without a shield attachment |

**Proves:**
- With a healthy alternative connector in the RN, revoking the shield's connector does **not** move the shield.
- The protected resource then becomes unreachable for **new** connections, once the client stops selecting A.
- The existing protected session keeps flowing through the revoked connector, even past that connector's certificate expiry.

**Does not prove:** what the shield would do if A's process stopped (connection loss rather than revocation). That was not part of this test.

### R4: previous certificate serial in the CRL: **CONFIRMED_SERIAL_HISTORY_GAP**

- Normal renewal issued two new serials (§4), each with the **previous** certificate still inside its validity period.
- After revocation, the freshly generated, signature-verified CRL (14:50:10.8) listed only the **latest** serial `8891…`.
  - `8F13…` was **absent** while still valid for ~8 min 57 s more (until 14:59:08).
  - `7663…` was **absent** while still valid for ~3 min 57 s more (until 14:54:07).
- Source: renewal builds the CSR from the **existing private key** (`connector/src/renewal.rs:94-97`: "Same key: the CSR is proof of possession of the EXISTING private key").
  - So the previous certificate and the key the connector still holds form a cryptographically valid credential until the previous `notAfter`.
  - This **usable-certificate risk is inferred from source and lifecycle evidence; it was not exercised.** No attempt was made to present the old certificate anywhere.
- Note: the previous certs were listed on no CRL at any point. The CRL query only selects `connectors.cert_serial` (`controller/internal/pki/workspace.go:583-614`).

### R1: existing relay-routed connection: **BLOCKED**

- A relay is offered to connectors only if `public_addr IS NOT NULL OR address_scope = 'public'` (`controller/internal/policy/store.go:488`, `controller/internal/relay/store.go:623`).
- The controller sets `public_addr` only from a relay's observed **global unicast** address. Private LAN addresses get scope `private` and no public address (`controller/internal/relay/heartbeat.go:332-345`).
- There is no relay-side setting to advertise an address (`relay/src/config.rs` has none).
- Lab relay hosts are LAN-only (.62 up, .46 down; every relay row `inactive`/`revoked`/`deleted`/`pending`). Both connectors logged `relay_count=0` / `LabelledRelayList is empty`.
- Running the relay path would need a source patch (the earlier `S20_TEMP_RELAY` approach) or a relay with a public IP. Both are outside this task's rules.
- Not tested:
  - relay recheck of an established session;
  - relay CRL fetch/load timing;
  - relay rejection of a new handshake.

## 7. Existing vs new connections (this session)

| Path | Existing (pre-revoke) | New |
|---|---|---|
| Client → A → Shield (protected) | continues through revoke, sync and A's cert expiry (HOLD-S) | **accepted by A** until the client sync (R3-V, 115). After sync the client selects B; B refuses (`SHIELD_NOT_ATTACHED`); the client fails closed |
| Client → B (unprotected, control) | continues (HOLD-D 702 × 200) | 490 × 200 after revoke (unaffected by A's revocation) |
| Relay | BLOCKED | BLOCKED |

## 8. CRL and cache observations

| Item | Observation |
|---|---|
| Controller CRL generation | generated per request. thisUpdate = fetch time; nextUpdate = +24 h; signature verified against the workspace CA public cert. Revoked serial present 5.35 s after `revoked_at` (first fetch) |
| Connector CRL refresh (A) | no CRL lines at all in A's journal for the run (14:38 →). Success isn't logged, so **whether A fetched or loaded a CRL containing its own serial cannot be established**. The source only checks *client* serials, which is consistent with A serving after revocation |
| Connector CRL refresh (B) | 2 refresh failures at 14:49:40 and 14:49:45 IST (`CRL thisUpdate is in the future`, likely sub-second skew). Otherwise not observable |
| Relay CRL | BLOCKED (no relay) |
| Client | `Relay CRL refresh failed` during the block (expected). No connector-CRL use (as on 10-12) |
| Shield | no CRL activity |
| Existing-session recheck after refresh | no component terminated HOLD-S |
| Stale state in use | A kept serving with ACL v7 (received 14:50:05.480, after `revoked_at`). The shield kept its link and peer list from before the revocation |

## 9. Was direct connector enforcement isolated?

Partly:
- **R3-V** isolates the connector's behaviour for new sessions while its certificate is valid and its control plane is rejected. Result: it keeps accepting.
- It cannot separate "existing QUIC connection" from "new handshake", and could not exercise post-expiry handshakes.
- The original R3 stays INCONCLUSIVE.

## 10. Raw log locations

**Controller host** `~/s20-run/logs/rev2/`:
- `rev2-events.log`;
- `rev2-hold-{D,S}.log` (every request);
- `rev2-newconn.log`, `rev2-tunsamp.log`;
- `rev2-wire-{a,b}.log` (served serial / validity, change-only);
- `rev2-client-block.log`;
- `pre-revoke-db.txt`, `revoke-db.txt`;
- `crl-pre-144935.*`, `crl-post-*-145010.*`, `crl-final-*.*`, `ws-ca.pem` (public);
- `snap-main.txt` (14:51:10, 14:55:06, 15:06:38);
- `analysis-final.txt`;
- `controller-excerpt.log`, `client-journal-excerpt.log`.

Also `~/s20-run/logs/controller.log`.

**Remote hosts:**
- .75: `~/rev2-connector-journal-2026-10-13-a75.log` (full A journal since 14:38), `~/rev2-shield-journal-2026-10-13-a75.log`, `~/rev2-connsamp-2026-10-13-a75.log`.
- .49: `~/rev2-connector-journal-2026-10-13-b49.log`, `~/rev2-connsamp-2026-10-13-b49.log`.

**Scripts** (run dir, not repo):
- `rev2-ctl.sh`, `rev2-instr.sh`, `rev2-holds.sh`, `rev2-client-block.sh`, `rev2-wiresamp.sh`, `rev2-remote.sh`, `rev2-snap.sh`, `rev2-crl.sh`, `rev2-analyze.sh`;
- reused: `rev_hold.py`, `rev-connsamp.sh`, `f5c5-newconn.sh`, `f5c5-tunsamp.sh`.

**Key excerpts:**
```
14:50:08 control stream: connector fad7c06b-… revoked/deleted — closing control stream
09:20:08.845806Z ERROR … control stream recv error: … message: "connector revoked"        (A)
09:23:59.699755Z INFO … tunnel_opened ok shield=909d233e-… resource_id=f6ae2ea0-…           (A, last before sync)
09:24:01.096444Z INFO … transport map hot-applied successfully resources=2 reachable=2      (client)
… WARN … QUIC stream error: shield 909d233e-… not connected                                 (B, 379×)
… WARN … shield not attached, trying next connector dest=192.168.1.75 port=51714            (client)
14:52:38 shield disconnect watcher: marked 1 shield(s) disconnected
CRL 09:20:10Z: D404C944… (Oct 12), 8891EE05… (Oct 13 09:20:05) — 8F138022…, 7663EED2… absent
```

## 11. Cleanup

| Action | Verified |
|---|---|
| Holds, probes, tunsamp and wire samplers stopped (15:07:24–:27); connsamp stopped on .75 and .49 | units inactive |
| `s20rev2-web` unassigned (Team → Groups), Delete (user clicked OK in the confirm), stuck at `deleting` (shield behind a revoked connector) → **Force delete** | no resource row; 0 access rules |
| Test vite apps on .75 (PIDs 17359/17361, started 14:40 by this run, logs `/tmp/rev2-app-*.log`) killed | 0 listeners on :51713/:51714 |
| Connector A: stopped and disabled on .75; **DB row left `revoked`** (serial `8891…` still on the CRL) | `inactive`/`disabled`, no process |
| Connector B: stopped and disabled on .49; DB row now `disconnected`, **not revoked** (not requested) | `inactive`/`disabled` |
| Shield runtime on .75 uninstalled (`shield-uninstall.sh -y`: binary, config, key/cert/state) | unit `not-found` |
| Token files removed: local `rev2-conn-a.env`, `rev2-conn-b.env`, `rev2-shield.env`, the per-run `rev2-provider-jwt.secret`; remote `~/s20rev2/conn.env` (.75, .49), `~/s20rev2/shield.env` (.75) | absent |
| LAN serve directory `serve-r2` removed; `s20-serve-r2` unit stopped | absent |
| Controller and admin UI stopped (started by this run; nothing else depends on them) | `inactive` |

**Left in place:**
- Revoked DB rows `bde51440` and `fad7c06b` (required for CRL coverage).
- B's DB row (`disconnected`) and shield DB rows `909d233e` / `31ceacd3` (`disconnected`): no DB change was requested.
- Installed connector binaries and configs on .75 / .49 (stopped).
- `.49:~/s20rev2/zecurity-connector.f3`: binary copy, not a credential.
- The fresh controller build in `~/s20-run/build-r/`.
- Pre-existing stale resources and assignments (`s20f3-web-renamed`, `s20l6-web`, `s20f3-unrel`).
- The stale `manoj` connector DB row: its install on .49 was replaced, but the row was not touched.

## 12. Evidence vs inference

- **Evidence:**
  - R2 no-failover, existing-session survival and new-connection refusal;
  - R3-V acceptance of 115 new sessions after revocation;
  - R4 CRL contents vs serial validity.
- **Inference:**
  - R3-V sessions likely reused the pre-revoke QUIC connection;
  - the previous certificate plus the reused key is a usable credential (source: `renewal.rs`).

## 13. Unresolved questions

1. Would connector A accept a **new QUIC handshake** from a valid client after revocation, and after its own certificate expiry? This needs a supported per-connector probe (missing capability).
2. Relay behaviour (R1) needs a relay with a public address or an approved test build.
3. Shield failover on connector *process loss* (as opposed to revocation) was not tested.
4. The 10-12 run used controller binary `controller-f5c`, built before `4781bc8`. Its relevance to the 10-12 results is unassessed. The 10-12 record is unchanged.
5. Why the revoked connector stops forwarding `PeerConnectorList` to the shield (last 14:49:57) yet keeps the data link: mechanism not traced.
6. Connector certs are backdated about 1 h in `notBefore`: observed, purpose not investigated.
