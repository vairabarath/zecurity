# Handoff — Fix 05 Phase 5-C live acceptance (Resource Hot-Apply), for a new chat

Written 2026-10-08 ~12:25 IST. **Re-verify every lab fact below before acting.** Nothing here is
guaranteed to still be true.

## Read first (in order)

1. Skills:
   - `zecurity-live-acceptance-run`: lab procedure, Orca terminals, sudo/Google etiquette, pitfalls, `references/admin-ui-recipes.md`.
   - `zecurity-client-dataplane`: 5-C facts and the resource hot-apply section.
2. `.zecurity-obs/Sprint20/Live-Lab-Testing-Playbook.md`: rules §0 and the unit recipes in §3. **Its host table is stale**; use the one below.
3. **The spec for this run:** `.zecurity-obs/Sprint20/Fixes/fix 5/Phase5C-Resource-Hot-Apply.md`.
   - §C mutation order, §D failure boundary, §E zero-resource behaviour, §F capacity (H1).
   - **§H live criteria L1–L15.**
   - The Implementation section at the end: files changed, gates, mutation check, deviations.
4. Design background: `Fix05-Resource-Hot-Apply-Investigation.md` (§15 Q5/Q6/Q7, §17 zero-K, §18 H1–H4).
5. Format reference for the acceptance record: `Fix05-Stale-QUIC-Pool-Live-Acceptance-2026-10-06.md`.

## Rules (the user's, unchanged)

- Run **one row at a time**: evidence table, then ask before the next row.
- The user types every sudo and every Google step. Ask with a single `done` choice and never type a password.
- **Commit nothing and push nothing until told.** Leave `Live-Acceptance-Run-Sheet.md` (an older pre-existing edit), `.localdev/` and the other untracked docs alone.
- **No admin-UI sign-in after the client's last login and before a client row.** The same Google user evicts the client's refresh session. Do all UI work first, then `zecurity-client login`, then `sync`, then the rows. Grep `sync failed|session expired|refresh session dead` over every row window.
- Resource edits only through the admin UI or GraphQL, never SQL: SQL doesn't bump the notifier, so the client never sees the change.
- Frozen: Phase 2-A, 5-A, 5-B, the controller empty-network fix, the connector. FU-1 to FU-4 stay out of scope.
- Never echo enrollment tokens. Write them to 0600 env files and ship them with `live-lab-scripts/shipenv.sh`.

## Code state

| Item | State |
|---|---|
| Branch / HEAD | `feat/sprint20-m1-phase2` @ `86cda74` |
| 5-C changes | **uncommitted**: `client/src/{daemon,net_stack,runtime,tun}.rs` |
| Unit gates | `cargo test` 157 → **189 (+32)**; release ok; clippy 0 in new code; rustfmt clean on new code; mutation check 15/15 killed |
| 5-C release binary | `client/target/release/zecurity-client`, sha `0f3adbbd1ec7be3a…`. Rebuild if `git status` shows client changes since then |
| **Installed client** | `/usr/local/bin/zecurity-client` sha `1f960fe23a7bb98b…` = **5-B build (pre-5-C)**. Back it up to `~/s20-run/` before installing 5-C |
| Client daemon | `active`, but **`zecurity0` does not exist** (cold after the reboot). The device is stale; a new `zecurity-client login` is needed after UI work |
| Live-lab monitors | `p1-mon.sh` / `fc-mon.sh` (repo + `~/s20-run`) print a `phase5c:` counter line for the new log strings |

New 5-C log strings to grep:
- `resource change, hot-applying`
- `resources hot-applied`
- `resource additions pending: address capacity`
- `no routable resources; tunnel kept up, nothing captured`
- `resource apply failed before mutation, retaining current state`
- `resource apply failed after mutation, recovering with full restart`

Full restart (should be 0 on hot-apply rows): `structural configuration change, restarting VPN`.

## Lab state at handoff (re-verify)

Every host rebooted this morning (~09:43–10:12). Every connector cert expired long ago (15m TTL), so every agent needs re-enrolling.

| Role | Host | IP | Orca handle (`~/s20-run/terms.env`) | State |
|---|---|---|---|---|
| Controller + client + admin UI | this box | 192.168.1.164 | `T_LOCAL` | `s20-controller` **active** (12:12:46, binary `~/s20-run/controller-f5c` sha `36892b48`, TTLs `CONNECTOR_CERT_TTL=15m`, renewal 10m, `SHIELD_CERT_TTL=168h`, `RELAY_CERT_TTL=1h`), started via `~/s20-run/f5c5-ctl.sh`; `s20-admin` active on 127.0.0.1:5173 |
| Connector (RN palace) | omarchy (`manojinkyank`) | .49 | `T_MANOJ` | connector `970bfa02` installed, crash-looping on an expired cert → re-enroll |
| Connector (RN s20f5-rn2) | archlinux (`inkyank`) | .75 | `T_INK` | same → re-enroll. Resource app: journal Vite on :51712 (`~/Projects/journal-management-main/client`, `npm run dev -- --host 0.0.0.0 --port 51712 --strictPort`), **not running** |
| Connector (RN s20f5-rn2) | udaya (archlinux) | **.37** (was .47) | `T_UDAYA` | same → re-enroll. Old relay binary present, inactive |
| **Shield (replaces nika)** | `seechan5` Arch, kernel 7.0.14 (modules present) | **.34** | `T_H34` (switched to plain `bash --norc`; its zsh garbled osend commands) | old **non-branch connector `983a4ac2` installed + enabled** → user approved uninstalling it. No shield yet. Repo `~/Desktop/zecurity` (branch `integration/relay-merge`), node/npm present; :5173 is taken by another vite app |
| nika | — | — | — | **unavailable; don't use** |

DB (workspace slug `xiyo`):
- RNs: `palace` `6ebeb898`, `s20f5-rn2` `cf520c20`.
- Old connectors: all `disconnected`; revoke them at clean-up.
- Old shield `s20f5-shield-nika` `453ba26b`: disconnected.
- Old resources:
  - `s20f5-nika-web` (192.168.1.39:51711, protected, nika shield): dead. Unprotect/Force-delete it, or it can block the ACL compile.
  - `s20f5-rn2-web` (192.168.1.75:51712)
  - `s20f5b-blackhole` (10.255.255.1:80)
- Group `s20-summa-users` has `rn2-web` and `blackhole`.
- Read-only snapshot script: `bash ~/s20-run/f5c5-db.sh`.

Prepared artefacts in `~/s20-run/`:
- `serve/sbundle.tgz`: shield release `30ba3e39…` + install/uninstall scripts + systemd units. Bundle sha `c4bee646…`, in `sbundle.sha`.
- `serve/cbundle.tgz`: connector `970bfa02` (already on the hosts).
- `f5c5-probe.sh`: read-only probe of all hosts.
- `f5c5-ctl.sh`: controller + admin restart.
- `f5c5-db.sh`: read-only DB snapshot.
- `osend.sh`: remote command wrapper.
- Event log: `logs/f5c5-events.log`.

Controller note: the HEAD rebuild (`controller-f5c2`) fails at start with `PROVIDER_JWT_SECRET is not set` (provider code arrived via the fixed-pendings merge), so the run uses `controller-f5c`, the empty-network build from 10-06 (lab-proven). Record this in the acceptance record. 5-C is client-only.

## Blocker at handoff: the preview pane won't take actions

`drive_preview` fails on every call with "The in-app browser only takes actions in the session the user is looking at", even after `desktop_preview` close+open, switching chats, and a fresh turn. `desktop_preview read` works (it shows `/login`).

Cause, from source (`~/.hermes/hermes-agent/apps/desktop/src/app/session/hooks/use-message-stream/gateway-event/server-requests.ts`): `previewAct` refuses unless `requestNamesActiveSession` passes (`:130`, `:534`). The old chat was context-compressed, and the comment at `:179–181` says the window keeps the pre-rotation id until an explicit session resume. **A new chat should not have this problem.** If it does, ask the user to create the 4 tokens by hand and leave each detail page open for `desktop_preview read`.

## Approved setup (user said "Yes, go with this setup")

1. **Admin UI** (sign in at slug `xiyo`):
   - Unprotect/Force-delete the dead `s20f5-nika-web`.
   - Create connector tokens:
     - `s20f5c-conn-manoj` → palace (.49)
     - `s20f5c-conn-rn2a` → s20f5-rn2 (.75)
     - `s20f5c-conn-rn2b` → s20f5-rn2 (.37)
   - Decode each JWT `connector_id`; write the env files 0600.
2. **Connectors:** on each host, one sudo line:
   `uninstall -y && set -a && . ./<env> && set +a && connector-local-install.sh` with `CONTROLLER_ADDR=192.168.1.164:9090 CONTROLLER_HTTP_ADDR=192.168.1.164:8080`.
   Then `systemctl disable --now zecurity-connector-update.timer`. Verify DB `active`, sha `970bfa02`.
3. **Shield on .34**, after the .49 connector is active:
   - Create token `s20f5c-shield-34` → palace. Check the JWT `connector_id` = the .49 connector.
   - Pull `sbundle.tgz` over LAN with `serve-unit.sh`; check `ufw` on this box.
   - **Uninstall the old .34 connector** (`connector-uninstall.sh -y`) in the same sudo line as `shield-local-install.sh`.
   - Disable the shield update timer.
   - Verify `enrollment complete` → `Control stream established`, and that the DB shield is `active` with `lan_ip` 192.168.1.34.
4. **Resources:**
   - **A** = admin dashboard on .34:51721: `cd ~/Desktop/zecurity/admin && nohup npx vite --host 0.0.0.0 --port 51721 --strictPort > ~/adminA.log 2>&1 &`
   - **A2** = a second instance on .34:51722, for the same-IP add and port-change rows. Check `ss -lnt`, and curl both from this box (expect 200).
   - **B** = journal app on .75:51712 (restart it). `s20f5b-blackhole` can stay unassigned or be removed.
   - Create resources in the UI, Protect where shield-routed, and assign them via Team → Groups → `s20-summa-users`.
5. **Client:**
   - Back up the installed 5-B binary.
   - `sudo install -m0755 client/target/release/zecurity-client /usr/local/bin/ && sudo systemctl restart zecurity-client` (user sudo).
   - Close the admin preview, then `zecurity-client login` (detached; user clicks Allow), `sync`, `zecurity-client up`.
   - Confirm the on-disk sha is `0f3adbbd…`, then 3 × 200 through `zecurity0`.
6. **Instruments** (new prefix `f5c5-*`):
   - ifindex/PID/policy change-only sampler (`tun-churn-sampler.sh` from the live-acceptance skill)
   - `nft list table inet zecurity_client | sha256sum` + rule count
   - `ip rule | grep -c 'fwmark 0x5a'`
   - `ip route show table 105`
   - keepalive hold (`fc_hold.py`, retargeted)
   - new-connection sampler
   - client journal counts (`p1-mon.sh` with the `phase5c:` line)

   The nft and route reads need root on this box, so ask the user or run a root sampler unit they start. Start the hold LAST.

## Rows to run (approved order) → criteria in Phase5C §H

Capacity reminder: smoltcp `IFACE_MAX_ADDR_COUNT=2` (one slot is 100.64.0.1), so the client holds **one resource IP**. A second new IP must go **pending** (H1), and that is not a defect.

| # | Row | Expect |
|---|---|---|
| 1 | add A2 (same IP .34, new port) | hot-applied, reachable, A hold 0 FAIL, ifindex same (L2/L5) |
| 2 | A2 port change | old-port flow RST by the client, new port OK, 0 restarts (L9) |
| 3 | remove A2 | its flow reset, new connects refused, A survives (L6) |
| 4 | add B (new IP .75) | `resource additions pending: address capacity`, no silent loss, 0 restarts (L11) |
| 5 | resource + connector change in one snapshot (e.g. port edit on A while stopping/starting the .37 rn2 connector) | one apply, one publish, 0 restarts (L10) |
| 6 | remove A | B admitted on the same tick (removal frees capacity) |
| 7 | remove B → zero resources | 0 restarts, nft rule count 0, table 105 empty, tunnel up, fwmark count 1 (L3/L7) |
| 8 | re-add A | reachable, 0 restarts, same ifindex (L8) |
| 9 | controls: RN move, identity | exactly 1 `structural configuration change, restarting VPN` each (L1) |
| 10 | Q5/Q7 kernel checks | L13 invalid-last-rule batch leaves the hash unchanged; L14 flush/refill under probe 0 failures; L15 existing conn to a new resource → ECONNRESET (all need root/user approval) |

L12: the apply log's resources/pending counts must match the snapshot, on every row.

## Output

- Write `.zecurity-obs/Sprint20/Fixes/fix 5/Phase5C-Live-Acceptance-2026-10-08.md` (format per the live-acceptance skill: inventory, instruments, per-row assertion tables, findings, NOT RUN list) and link it from the Phase5C doc.
- Keep Phase5C at `implemented-pending-live` until every L-row has evidence; mark partials ⚠ per assertion.
- Clean-up at the end:
  - delete the token env files locally and on the hosts
  - stop the vite instances on .34 and .75
  - revoke stale agents (shield before connector)
  - restore or keep the client binary as the user says
