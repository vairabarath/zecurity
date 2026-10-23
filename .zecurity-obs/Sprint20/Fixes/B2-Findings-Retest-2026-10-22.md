---
type: live retest
sprint: 20
date: 2026-10-22
build: 40e25f3 (branch feat/sprint20-m1-phase2)
scope: B2 live findings 1–6 (Connector-Revocation-PhaseB2-Implementation-2026-10-21.md, "Observed, outside B2")
result: 6 / 6 PASS
status: Committed locally, not pushed.
---

# B2 findings: full live retest on the final build (2026-10-22, 10:55–11:09 UTC)

Every result below comes from a log line, DB row or HTTP response captured during this run. Nothing is inferred.

## Preflight: what was actually running

- `git diff <fix commit> HEAD -- <component>` is empty for every component:
  - controller since `071c0de`;
  - connector since `3f0d604`;
  - shield since `1d806de`;
  - client since `3f8f9f7`;
  - admin since `40e25f3`.

  The working tree was clean.
- Each component was rebuilt from HEAD and compared with what was deployed:

| Component | HEAD build | Deployed | Where |
|---|---|---|---|
| controller | `controller-head` | `/proc/<pid>/exe` sha `7553b501…` = `controller-head` | this PC :8080/:9090 |
| client (release, platform root) | `7a76a99b…` | `/usr/local/bin/zecurity-client` `7a76a99b…` | this PC, systemd |
| shield | `0490e96f…` | `shield-fix` `0490e96f…` | `s3-shield`, this PC (user-level) |
| connector | `8dadeaec…` | **was `d0be6f28…` (not provably HEAD)** → redeployed: luffy `~/s3-conn/zecurity-connector` `8dadeaec…` (pid 6582), local `connector-fix` `8dadeaec…` | `s3-luffy` on luffy (.40); `s3-relaylist` here |
| admin | working tree = HEAD | `vite` dev server :5173 | Orca built-in browser, local test login |

- The connector's `build.rs` reads `.env` and environment variables, so a build from different inputs can differ. The connector was therefore redeployed from the HEAD build before testing.
- Relay: `s3-relay-manoj` on .49 (B2 relay binary `8a47a3e8…`; relay code unchanged).

## Results

| # | Finding | Retest | Evidence (UTC) | Result |
|---|---|---|---|---|
| **4** | Admin and client sessions invalid after restarts (Fix 06) | Fresh client login + two admin logins (A, B); interleaved refreshes; logout A; 3 controller restarts; the client's own scheduled refresh | 10:57:41 client login, admin A + B. Refresh A, B, A, B: **200 ×4**. Logout A 204 → A **401 `refresh token expired`**, B **200**. Controller restarted 10:58:27, 10:59:57, 11:01:47. **11:04:26 client `session refreshed proactively`** (gRPC path). Then B **200** | **PASS** |
| **1** | Client recovery lag: shield re-attach after `Disabled` (Fix 08) | Shield `s3-shield` attached to the local connector; controller down 90 s (`outage.sh`) | 10:58:27.6 stop → 10:58:50.2 `Disabled` → 10:59:20.2 (= Disabled + 30 s) first `connector is not serving yet; retrying`, then 18 retries every ~2.4 s → 11:00:02.892 connector `Serving` → **11:00:04.564 shield `Control stream established` (1.67 s)**. Baseline 37.3 s | **PASS** |
| **6** | Relays on private IPs never advertised (dev flag `c76c783`) | Controller restarted **without**, then **with** `RELAY_DEV_ALLOW_PRIVATE_ADDR` (`ENV=development`); relay `s3-relay-manoj` (192.168.1.49) active both times | Without: no `DEV: relays…` log; luffy 10:59:59.71 `received LabelledRelayList … relay_count=0`. With: 11:01:47 `DEV: relays on private addresses are advertised …`; luffy 11:01:57.97 **`relay_count=1`** | **PASS** |
| **5** | Client hot-apply admits one new resource IP (Fix 09) | Resources `s3-w40` 192.168.1.40:18081, `s3-w49` 192.168.1.49:**18081** (same port, different host), `s3-t75` 192.168.1.75:9092. Test web servers on luffy/manoj return their hostname. A: only w40, `down`/`up`. B: add w49 + t75 **while up**. C: `down`/`up` with all | A: w40 routed, `s3-ok-luffy`, tunnelled. **B (11:03:06): `hot-applying removed=0 added=2` → `hot-applied resources=5` (16 ms); w40 `s3-ok-luffy`, w49 `s3-ok-omarchy`, t75 connect-ok, all routed and tunnelled.** C: 5 routes, all three tunnelled, 0 capacity messages. `s3-ok-omarchy` through the tunnel shows shared-port resources reach the right host | **PASS** |
| **2** | Admin UI can't protect a resource created before its shield (Fix 10) | `s3-shield` stopped (DB `disconnected` after ~120 s) → resource `s3-ui2` 192.168.1.164:18082 created (`shield: null`) → shield started (`active`) → page in Orca's browser → click Protect | Page: "Protect this resource" **enabled** (×3), "…is available on…", **Ready**, "Protecting binds…" (`retest-ui.png`). 11:06:31 click → DB `s3-ui2` **bound to `s3-shield`**, `pending_action=apply`. The shield then logged `nftables apply failed` (unprivileged test shield, no nftables; outside the fix) | **PASS** |
| **3** | Connector never re-reads the relay list while not attached (Fix 07) | Relay on .49 stopped (sudo by the user), then started again; connector `s3-luffy` left running | 11:04:47 relay stopped → luffy 11:06:03 `Active relay session failed; failing over` → 11:06:47 `relay_count=0` → 11:07:11 / 11:07:31 / 11:08:11 `LabelledRelayList is empty; entering backoff` (**not attached, empty list**) → relay started (DB heartbeat 11:08:44.88) → **11:08:44.888 `relay_count=1` → `New LabelledRelayList during backoff; retrying now` → 11:08:44.924 `Connector registered with Relay`** (36 ms). Same pid 6582 throughout. Controller 16:38:44 local `reason=connected` | **PASS** |

## Notes from the run

- **Finding 3 setup:** the first relay command (`stop; sleep 200; start`) exited without starting the relay; the tab showed an error prompt. The start was resent on request at 11:08 (sudo cached) and the relay came up at 11:08:44. The connector had been in empty-list backoff since 11:07:11.
- **Restarting the controller without the flag doesn't detach a connector already on a relay.** The empty list logged "Active relay absent from new list; migrating immediately", and the connector stayed on its working relay. That's why Finding 3 was retested with a real relay stop rather than via the flag.
- **`starts=2` in luffy's connector log** comes from the planned redeploy at 10:56 (pid 6582 since then). It's not a restart during the tests.
- **Not covered (stated, not guessed):**
  - a client flow through a **root** shield protecting a resource (the original L-3 symptom end to end). The fix is on the shield's re-attach, measured directly above. The only lab shields with root are bound to the revoked or disconnected connectors (`pm-conn`, `k2-conn`).
  - `pm-udaya-web` still points at the old udaya IP (.37).

## Cleanup

- Deleted: test resources `s3-w40`, `s3-w49`, `s3-t75`, `s3-ui2`, `s3-r1…r4`.
- Stopped:
  - the test web servers on luffy and manoj;
  - local connector `s3-relaylist` and shield `s3-shield`.
- The client is up with only the stale `.37`/`.39` ACL entries.
- **Still running:** relay `s3-relay-manoj` (.49), connector `s3-luffy` (luffy), controller `controller-head`, admin dev server :5173.
- Evidence log: `~/.cache/zecurity-s3/retest-evidence.txt`; screenshot `retest-ui.png`.
