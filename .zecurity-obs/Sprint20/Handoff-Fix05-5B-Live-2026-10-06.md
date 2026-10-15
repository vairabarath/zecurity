# Handoff — Fix 05-B live acceptance (stale pooled QUIC connection), for a new chat

Written 2026-10-06 ~17:10 IST. **Start by re-verifying every lab fact below**; nothing here is guaranteed
to still be true.

## Read first (in this order)

1. Skills: `zecurity-live-acceptance-run` (lab procedure, terminals, sudo/Google etiquette, pitfalls) and
   `zecurity-client-dataplane` (5-B facts and test harness).
2. `.zecurity-obs/Sprint20/Live-Lab-Testing-Playbook.md`: rules §0, recipes §7.4.
3. **The fix doc, which is the spec for this run:**
   `.zecurity-obs/Sprint20/Fixes/fix 5/Fix05-Stale-QUIC-Pool-Investigation.md`
   - §5 failure path
   - §9 safety
   - **§18 live acceptance rows B1–B7**
   - **§20 implementation + unit gates**
4. Most recent live record, for format and lab quirks:
   `.zecurity-obs/Sprint20/Fixes/fix 5/Fix05-Empty-Network-Live-Acceptance-2026-10-06.md`.

## Rules (the user's, unchanged)

- Run **one row at a time**. After each row, give an evidence table, then ask before the next.
- The user types every sudo and every Google step. Ask with a single `done` choice; never type a
  password.
- **Commit nothing unless told to.** No push.
- **Never sign in to the admin UI after the client's last login and before a client-dependent row.**
  The same Google user evicts the client's session a few minutes later (that voided half of the
  empty-network run). Do all UI work first, then `zecurity-client login`, then `sync`, then the rows.
  Grep `sync failed|session expired|refresh session dead` over every row window.
- Frozen, so don't touch: Fix 5-A, Phase 2-A, the controller empty-network fix, connector code,
  `relay_pool.rs` (its close-on-age finding is separate). Don't touch `.localdev/` or the other
  untracked files either.
- Never echo enrollment tokens. Write them to 0600 env files and ship them with
  `.zecurity-obs/Sprint20/live-lab-scripts/shipenv.sh`.

## Where the code stands

- Branch `feat/sprint20-m1-phase2`, HEAD `091837d`, pushed. PR #111 → `fixed-pendings` is open.
- **5-B is uncommitted** in the working tree. It touches only `client/src/{tunnel_pool,transport,net_stack}.rs`.
  `cargo test` gives 157 passed (149 → +8). Clippy and fmt match the baseline. The mutation check
  passed. Details in §20.
- The 5-B release binary is already built: `client/target/release/zecurity-client`, sha `1f960fe23a7bb98b…`
  (built 17:02). Rebuild if `git status` shows changes since then.
- Installed client is still the **pre-5-B** build `e2e36c45…` (Fix 5-A). That's what B2 (the before-fix
  comparison) needs, so run B2 first. A backup of an older build is at
  `~/s20-run/zecurity-client.p2a-30749eb9.bak`. Before installing 5-B, also back up `e2e36c45` to `~/s20-run/`.

## Lab state at handoff (re-verify)

| Item | State |
|---|---|
| Controller 192.168.1.164 (this box) | user unit `s20-controller`, binary `~/s20-run/controller-f5c` (empty-network fix). Env: `CONNECTOR_CERT_TTL=15m`, renewal 10m. Restart script `~/s20-run/f5c-ctl-restart.sh`. Log `~/s20-run/logs/controller.log` |
| Client (this box) | `zecurity-client` active, device `841fa6e5…` (re-logged 15:41), ACL poll phase ≈ `hh:mm:58.4`, `zecurity0` up |
| RN `palace` | `s20f5-conn-manoj` `162319a6…` on **omarchy .49** (active). Shield `s20f5-shield-nika` on nika .39 via manoj. Resource `s20f5-nika-web` 192.168.1.39:51711 (**shield-routed**: no use for fallback rows) |
| RN `s20f5-rn2` `cf520c20…` | `s20f5-conn-rn2` `56de96a1…` on **archlinux .75** (active). Resource `s20f5-rn2-web` 192.168.1.75:51712, **connector-routed**, = Vite app `~/Projects/journal-management-main/client` (`npm run dev -- --host 0.0.0.0 --port 51712 --strictPort`, log `~/app-rn2.log`) |
| Group | `s20-summa-users` has both resources |
| Disconnected leftovers | `s20f5-conn-inkyank`, `s20p2a-*` (can be revoked at clean-up) |
| Postgres | container `ztna_postgres`, db `ztna_platform`, user `ztna`. Get the password from `docker inspect` inside scripts so it never prints |
| Orca terminals | `~/s20-run/terms.env` (`T_LOCAL`, `T_MANOJ`=omarchy, `T_INK`=archlinux, `T_NIKA`). Re-derive with `orca-ide terminal list --json` if stale. Remote commands go through `live-lab-scripts/osend.sh` |

## What B1–B7 need that the lab doesn't have yet

5-B only matters when a network keeps another working connector, so **B1/B3/B4/B6 need one RN
with TWO active connectors and a connector-routed resource** (shield resources work only through the
shield's connector).

**Recommended setup:**
1. Add a second connector `s20f5b-conn-rn2b` to `s20f5-rn2` on **omarchy .49**, re-enrolling that host's
   connector (the same recipe as `s20f5-conn-rn2`: `connector-uninstall.sh -y`, then `connector-local-install.sh`
   with `CONTROLLER_ADDR=192.168.1.164:9090 CONTROLLER_HTTP_ADDR=192.168.1.164:8080`, then disable the
   update timer).
   - Consequence: `palace` loses its only connector, so the shield and `s20f5-nika-web` go dark for
     the run.
   - Ask the user before doing this; the alternative is a connector on another host if one is available.
2. **B3 resource:** a connector-routed resource that silently drops SYNs, so the connector's
   `TcpStream::connect` hangs past 5 s. Candidate: `10.255.255.1:80` in `s20f5-rn2`, assigned to
   `s20-summa-users`.
   - Verify first from .75: `timeout 8 bash -c '</dev/tcp/10.255.255.1/80'; echo $?` must be 124 (a hang).
   - An `EHOSTUNREACH` or RST in under 5 s won't do; pick another address.
3. **B4 download:** a file of ≥ 5 MB served by the rn2 app (drop it into the Vite `public/`), fetched with
   `curl --limit-rate 50k` so it runs ≥ 90 s.
4. Do all admin-UI work (connector token, resources, assignments) **before** re-logging the client.
5. Find which rn2 connector the client tries **first**: the connector journal `access allowed` per
   probe, or the client's candidate order. B1 stops *that* one.

## Monitor and grep changes for this run

- New 5-B log line (warn, target `zecurity_client::transport`):
  `connector silent since stream open: stale pooled QUIC connection evicted (not closed), direct path cooling down`.
  Count it per row.
- Existing lines to count: `tunnel handshake timed out after 5s`, `direct path is in cooldown`,
  `relay failure signalled`, `transport-only change, hot-applying`, `restarting VPN`,
  `structural configuration change`, `failed to reach connector (transport)`, `tunnel opened`.
- Reuse and adapt: `~/s20-run/f5c-probe.sh` (1 s new connections; point it at the rn2 resources),
  `f5-tunsamp.sh` (ifindex/PID/policy hash), `f5c-dsched.sh` (a root, self-timed stop/start schedule
  on a connector host), and `f5c-analyze.sh` / `f5c-summary.sh` (per-row timelines). Use a new prefix
  `f5b-*` and a new event log `logs/f5b-events.log`.

## Row order and pass criteria (summary; full text is §18 of the fix doc)

1. **B2 baseline, current client `e2e36c45`:** stop the first-tried rn2 connector with probes running.
   Expect about 25 × 5 s-slow `200`s over ~30 s (stale reuse until the quinn idle timeout), then one 2 s
   penalty, then fast. This is the "before".
   - Restart it and wait until both connectors are active and the client shows `reachable=2`.
2. **Install 5-B:** back up, then send `sudo install -m 0755 client/target/release/zecurity-client
   /usr/local/bin/zecurity-client && sudo systemctl restart zecurity-client` (user types sudo). Then
   `zecurity-client up` (re-login only if `sync` fails), check the sha is `1f960fe2…`, check
   `strings` contains `connector silent since stream open`, and get 3 × 200 on both rn2 resources.
3. **B1 partial loss (main):** stop the same connector.
   - Pass: only the flows already in flight pay 5 s, there is exactly one eviction line, then **0
     further 5 s timeouts** on it until it returns.
   - All probes return `200` via the other connector, with 0 `restarting VPN`, and ifindex and PID
     unchanged.
4. **B3 slow resource:** probe `10.255.255.1:80` on a live connector.
   - `handshake timed out` lines appear but there are **0 eviction lines** and no cooldown.
   - The rn2 web probes stay fast throughout.
5. **B4 shared-connection survival:** start the slow download through connector 1. Then
   `sudo kill -STOP <connector pid>` for about 8 s while one new probe stalls, then `kill -CONT`
   (well under the 30 s idle timeout).
   - Expect one eviction line, and the download completes with no reset.
6. **B5 total loss (regression):** stop both rn2 connectors. Expect the same result as the
   empty-network D1: hot-apply in ≤ 6 s, `reachable` drops for rn2, ≤ 5 slow flows, then fast `000`.
   Restart both within ~3 min, because of the cert TTL.
7. **B6 freeze:** `kill -STOP` connector 1 for 60 s. Expect one 5 s timeout, then fast fallthrough with
   no repeats while frozen. `kill -CONT`, then recovery after the cooldown or the next hot-apply.
8. **B7 soak:** 30 min at rest with both connectors. Expect 0 evictions, 0 cooldowns, 0 slow probes.
9. **Record:** write `Fixes/fix 5/Fix05-Stale-QUIC-Pool-Live-Acceptance-<date>.md` and update the §1/§20
   status and live-acceptance line in the fix doc. Mark it `live-accepted` only if B1–B7 pass, or
   `…-with-caveat` and list what wasn't run. Then **stop and wait** for commit approval.
   - Suggested commit split: `fix(client): Fix 05-B …` (the 3 files), then `docs(sprint20): …`.

## Clean-up / restore after the run (ask first)

- Put omarchy back into `palace` if the user wants the shield path back.
- Revoke the disconnected leftovers.
- Stop the f5b instruments.
- Leave the rn2 app running unless told otherwise.

## Suggested skills

`zecurity-live-acceptance-run`, `zecurity-client-dataplane`, `sprint-phase-tracking` (status/doc
discipline), and `caveman` (the user prefers terse replies).
