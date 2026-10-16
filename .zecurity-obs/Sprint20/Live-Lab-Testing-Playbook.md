---
type: playbook
sprint: 20
title: Live lab testing on real machines — procedure for agents and new sessions
status: current
updated: 2026-10-02
tags: [live-acceptance, lab, procedure, connector, client, shield]
---

# Live Lab Testing Playbook (real machines)

This is how the Sprint 20 fixes were tested live (Fix 01 client restart decision, and Fix-connector
make-before-break renewal), written so another agent or a fresh session can repeat it.
The scripts it refers to are in `.zecurity-obs/Sprint20/live-lab-scripts/` (copied from `~/s20-run/`).
None of them contain secrets.

Related records:
- `docs/sprint20-live-acceptance-runbook.md`: the sprint runbook rows (S/C/U/G/E/A/D/F/B/R).
- `.zecurity-obs/Sprint20/Live-Acceptance-Run-Sheet.md`: evidence and Findings 1–21.
- `.zecurity-obs/Sprint20/Fixes/fix-connector/Fix-Connector-Control-Stream-Certificate-Renewal.md` → §Live Acceptance.
- `.zecurity-obs/Sprint20/Fixes/fix 1/Phase1-AppliedConfig-Restart-Decision.md` → live sections.
- Hermes skill `zecurity-live-acceptance-run`: the same rules, auto-loaded by Hermes.

---

## 0. Rules (from the user; non-negotiable)

1. **One row or group at a time.** After each one, post a short evidence table, propose the next step,
   and ask before going on. Exception: the user may explicitly say "run it yourself, I approve".
2. **Never type sudo or SSH passwords.** Send the command to the host's Orca terminal, then ask the
   user to type the password there ("Done — I typed it"). Don't put passwords in commands, logs or
   docs, even if one appears in chat.
3. **Tokens** (enrollment JWTs) live only in `0600` files and are deleted at clean-up. They are
   never echoed. To ship one to a host, use `shipenv.sh` and verify the sha prefix.
4. **Admin UI and Google sign-in:** use the Hermes preview (`desktop_preview` + `drive_preview`).
   Headless `browser_exec` does not work for this app.
5. **Don't commit** `.localdev/` (it holds the relay lab private key), the run sheet, or unrelated docs.
   Commit only when told to; the format is Conventional Commits (`fix(scope): …`, `docs(sprint20): …`)
   with a body covering problem, changes, tests and docs. Push only commits that are live-tested, and only
   when asked.
6. Don't tick run-sheet checkboxes until close-out.
7. **Report honestly.** Separate "observed" from "inferred". Record caveats such as "the UI click landed 8 s after
   the renewal, not inside the 250 ms window". A row that wasn't exercised is "not run", never PASS.

---

## 1. Lab topology

| Role | Host | IP | User | Notes |
|---|---|---|---|---|
| Controller + Postgres/Valkey (docker) + admin UI + **client** | this machine (`inkyank-01-computer`) | 192.168.1.164 | inkyank-01 | client binary `/usr/local/bin/zecurity-client`, unit `zecurity-client` |
| Connector `s20fc-conn-inkyank` | archlinux | 192.168.1.75 | inkyank | Orca `T_INK` |
| Connector `s20fc-conn-udaya` | archlinux | 192.168.1.42 | udaya | Orca `T_UDAYA` |
| Connector `s20fc-conn-manoj` | omarchy | 192.168.1.44 | manojinkyank | Orca `T_MANOJ` |
| Shield `s20fc-shield-nika` + test resource (summa, TCP 5173) | nika | 192.168.1.38 | nika | Orca `T_NIKA` |

- **IPs change after reboots** (DHCP). On every new session, re-check with `ip -br a` in each terminal and
  update the table, the resource host, and the connector install env.
- Workspace (tenant) `xiyo` = `0d3b8f09-0338-4d03-8729-c4e12b052a4e`; network `palace`.
- Ports:
  - Controller: gRPC `:9090`, HTTP `:8080`.
  - Connector: shield-facing `:9091`, device tunnel `:9092` (TLS+QUIC).
  - Admin UI: `127.0.0.1:5173`.
- DB access: `docker exec -i ztna_postgres psql -U ztna -d ztna_platform -Atc "…"`.

---

## 2. How the agent reaches the remote machines (Orca terminals)

The user opens one SSH session per host inside the Orca IDE. The agent drives them with `orca-ide`:

```bash
export XDG_CONFIG_HOME=$HOME/.config          # required, or you get runtime_unavailable
orca-ide terminal list --json                 # map handles to hosts by title
# save handles to ~/s20-run/terms.env:  T_NIKA=term_… T_MANOJ=term_… T_UDAYA=term_… T_INK=term_…
```

- **Run a command and capture its output:** `osend.sh <handle> <UNIQUE_MARKER> '<cmd>'`.
  - It wraps the command in `==M==`/`==ME==` markers and polls until the end marker appears.
  - Use `WAIT=<s>` for slow commands, and a fresh marker on every call.
  - `terminal_handle_stale` means Orca restarted: re-list and remap.
- **sudo commands:** use `orca-ide terminal send --terminal $T --text '<sudo …>' --enter`, then ask the user
  to type the password, then read the result with `orca-ide terminal read` or the next `osend`.
- **Shipping a secret file:** `shipenv.sh <handle> <local 0600 file> <remote path>`.
  - It runs `umask 077; cat > path`, pastes the content, then sends Ctrl-D. Nothing is echoed.
  - Verify with `sha256sum` on both sides (compare the first 12 chars).
- **Shipping binaries and scripts:** serve `~/s20-run/serve/` on the LAN with `serve-unit.sh`, which runs
  http.server `192.168.1.164:8765` for 30 min. On the host, `curl -sO http://192.168.1.164:8765/<file>`
  and check the sha against `SHA256SUMS`. **The server expires silently**; run
  `systemctl --user is-active s20-serve` before every pull.

---

## 3. Keep lab processes alive (important)

Hermes kills its own background processes when a turn is interrupted. This killed the controller twice
during the run. Run every long-lived process as a **user systemd unit**:

```bash
# controller (branch build) with short cert TTLs so renewals happen every ~5 min
systemd-run --user --unit=s20-controller --working-directory=$HOME/zecurity/controller \
  -p StandardOutput=append:$HOME/s20-run/logs/controller.log -p StandardError=append:$HOME/s20-run/logs/controller.log \
  -E CONNECTOR_CERT_TTL=15m -E CONNECTOR_RENEWAL_WINDOW=10m -E SHIELD_CERT_TTL=168h -E RELAY_CERT_TTL=1h \
  $HOME/s20-run/controller-fixconn
bash admin-unit.sh                     # admin UI (vite) → unit s20-admin
bash serve-unit.sh                     # LAN file server → unit s20-serve (RuntimeMaxSec=1800)
systemd-run --user --unit=s20-<name> -p StandardOutput=append:$HOME/s20-run/logs/<name>.log <cmd>   # samplers/holds
```

- Remote samplers: `nohup bash /tmp/sockdump.sh 2400 '( dport = :9090 or sport = :9091 )' > /tmp/fc-sock.log 2>&1 &`
  sent via `osend`. These survive.
- Check before trusting any run: `systemctl --user is-active s20-controller s20-admin`.

---

## 4. Build and deploy the code under test

```bash
# controller
cd ~/zecurity/controller && go build -o ~/s20-run/controller-fixconn ./cmd/server
# connector (release) → bundle
cd ~/zecurity/connector && cargo build --release
#   bundle = zecurity-connector + scripts/connector-local-install.sh + scripts/connector-uninstall.sh + SHA256SUMS
#   tar into ~/s20-run/serve/cbundle.tgz, record its sha
# client — embeds THIS controller's platform root CA (certificate audit bug 1);
# a plain `cargo build --release` now fails without one. On the controller host:
#   scripts/export-platform-root.sh ~/s20-run/platform-root.pem
# then on the client host (copy the file over first):
cd ~/zecurity/client && ROOT_PEM=~/s20-run/platform-root.pem scripts/dev-build.sh --release \
  && sudo install target/release/zecurity-client /usr/local/bin/   # user types sudo
# After a controller DB reset (new root): rebuild the client, then log in again.
```

**Connector install on each host** (the user types sudo):

```bash
cd ~/s20fc/cbundle && sudo bash -c 'bash scripts/connector-uninstall.sh -y; set -a; . ./conn.env; set +a; \
  CONTROLLER_ADDR=192.168.1.164:9090 CONTROLLER_HTTP_ADDR=192.168.1.164:8080 \
  bash scripts/connector-local-install.sh ./zecurity-connector; \
  systemctl disable --now zecurity-connector-update.timer; systemctl stop zecurity-connector-update.service'
```

Shield install is the same shape: `shield-uninstall.sh -y`, `shield-local-install.sh ./zecurity-shield`, and disable
`zecurity-shield-update.timer`.

**A running connector may not be under systemd at all.** If someone started it by hand from
`~/s20fc/cbundle`, then all of these lie to you and the connector stays alive:

```bash
pkill -f cbundle                 # process is named zecurity-connector, not cbundle — no match
pgrep zecurity-connector         # pgrep (no -f) matches the 15-char comm name — matches nothing
systemctl stop zecurity-connector    # reports inactive, kills nothing (no unit)
systemctl stop connector.service     # a DIFFERENT pre-existing service on 192.168.1.164
```

Verify with what actually holds the port, and kill by PID (the user types sudo):

```bash
ss -lnt | grep 9091             # must print nothing
ps -eo pid,args | grep -i zecurity-connector | grep -v grep
sudo kill <pid>
```

Restarting it needs `conn.env` and root — the bare binary gives `missing field 'controller_addr'`
and running it as the user gives `Permission denied (os error 13)`:

```bash
sudo bash -c 'cd /home/<user>/s20fc/cbundle; set -a; . ./conn.env; set +a; \
  CONTROLLER_ADDR=192.168.1.164:9090 CONTROLLER_HTTP_ADDR=192.168.1.164:8080 \
  nohup /usr/local/bin/zecurity-connector >/tmp/conn.log 2>&1 &'
```

`pkill -f "bin/zecurity-connector"` was also tried on manoj and did **not** kill it — only
`sudo kill <pid>` worked. Get the pid from `sudo ss -lntp | grep 9091`.

**Editing a resource in Postgres does not reach the client.** The ACL/transport version comes from
`policy.Notifier.Version()` (`controller/internal/policy/compiler.go:294`), which only advances on a
controller-side event. A direct `update resources set port_from=...` leaves the client logging
`ACL up_to_date — kept cached snapshot version=N` and nothing happens. Any acceptance row that needs
the client to *see* a resource or ACL change must go through the admin UI or its GraphQL mutation.

**Editing a resource's port unprotects it.** After saving a port change the resource showed
`UNPROTECTED` / status `Failed` with `port not listening`, and re-Protect did not clear it, yet the
tunnel still served 200. Check the resource status before and after any edit.

**Time-bounded monitors expire silently.** The `fc_hold.py` hold had a 60-minute cap and finished on
its own (`DONE ... elapsed=3601s`) two minutes before the row that needed it. For a long session either
raise `DURATION` or start the hold immediately before the row.

**Always verify the binary afterwards.** The auto-updater replaces branch binaries within seconds (Finding 7):

```bash
sha256sum /usr/local/bin/zecurity-connector    # must equal the branch build sha
systemctl is-enabled zecurity-connector-update.timer   # disabled (or masked)
```

Do this on hosts you didn't touch too. Nika had been running an updater shield binary for days.

---

## 5. Admin UI work (Hermes preview)

- Sign in: open `http://localhost:5173` in `desktop_preview`, then `drive_preview` → Google → account → Allow
  (the user may need to click Allow). If the preview is reopened, the session can be lost; sign in again.
- **Create a connector:** Connectors → Add Connector. The dialog's button is off-screen: scroll `btn-linux` ~600 px
  inside the dialog, then click `btn-add-connector-1`. Copy the token into a 0600 file
  (`~/s20-run/fc-conn-<host>.env` as `ENROLLMENT_TOKEN=…`) using `execute_code`. Never print it.
- **Shield token:** decode the JWT payload and check `connector_id` **before** installing. The shield page's
  "CONNECTED VIA" label was wrong once. A mismatched token fails with `shield connector mismatch` and burns its JTI.
  Reload the page to mint a matching one.
- **Resource:** create it on the shield, Protect it, then assign it to a group (Team → Groups → Manage → resources →
  Assign Resource; for the native `<select>`, `press` a letter key on it).
- Revoke/delete: open the item's detail page, `reload`, call `elements` (refs go stale), then click `btn-revoke`.
  **Order: shield first, then connector.** Deleting a connector also deletes its shield.
- Confirm dialogs: some need the user to click; ask with clarify.

**Client login:** run `zecurity-client login` as a background process, read the Google URL from its log, open it in
`desktop_preview`, and have the user click Allow. Then run `zecurity-client sync` and `zecurity-client status`, and
`curl` the resource through `zecurity0`.

> ⚠ **Close the admin UI preview during client tests.** The admin UI and the client, signed in as the
> same Google user, share one refresh-session key (`refresh:<user_id>` in Valkey, rotated on use) and evict
> each other. The client then logs `session expired; re-login required` on every sync and makes no
> decisions, which silently invalidates the test. Gate: `journalctl -u zecurity-client --since <start> | grep -c 'sync failed'` must be 0.

---

## 6. Evidence instruments

All samplers log **only changes**, with millisecond timestamps. All are read-only.

| Script | Where | What it records |
|---|---|---|
| `fc_hold.py` (edit `DURATION`) | controller host | **One keep-alive TCP socket** to the resource through the tunnel, GET every 2 s. Never reconnects; any error → `FAIL` and exit. Proves "the long-lived flow survived" |
| `p1-newconn.sh <s>` | controller host | A **new** connection every 2 s (`curl -m 4`). Note: `-m 4` < the client's 5 s handshake timeout (see §8) |
| `fc-dbsamp.sh <s>` | controller host | Connector status + cert serial and shield status/connector every 100 ms from Postgres |
| `p1-shieldsamp.sh <s>` | controller host | Connector statuses + which connector the shield is on |
| `fc-probe9092.sh <s>` | controller host | The `:9092` cert serial served by each connector (rotation proof). It floods connector logs with `peer sent no certificates`; filter that out |
| `sockdump.sh <s> '<ss filter>'` | each connector / shield | Established sockets + agent PID every 200 ms. Proves "new stream before old closed", "no process restart", "shield socket unchanged" |
| `t7-block.sh <HH:MM:SS> <secs>` | connector (sudo) | **Fault injection:** lets exactly 1 new SYN to controller `:9090` through, then resets the rest for N s. Self-reverting (`trap`). Simulates "renewal OK, replacement stream fails" |
| `fc-mon.sh <start>` / `p1-mon.sh <start>` | controller host | One-shot tallies: ReEnroll / superseded / disconnected per connector, ACL pushes, client `keeping tunnel` vs `restarting VPN`, sync failures |

Useful log lines:

| Component | Line | Meaning |
|---|---|---|
| controller | `<id> ReEnroll sent` | renewal triggered |
| controller | `superseded stream closed (newer stream is authoritative)` | old stream closed after the switch (good) |
| controller | `connector <id> disconnected` | real disconnect; must be 0 during renewals |
| controller | `acl push: … version=N connectors=K` | ACL version bump; match each one to a cause |
| connector | `opening replacement Control stream (old stream stays active)` → `switched to renewed Control stream — old stream dropped` | make-before-break |
| connector | `replacement Control stream failed — keeping current stream, retrying with backoff` | failure path (test 7) |
| client | `effective config unchanged, keeping tunnel` / `structural configuration change, restarting VPN` (pre-Phase-2-A builds: `snapshot changed, restarting VPN`) | Fix 01 decision |
| client | `transport-only change, hot-applying connector map` → `transport map hot-applied successfully` / `transport map build failed, retaining existing map` / `hot-apply not possible …, falling back to full restart` | Fix 01 Phase 2-A connector hot-apply |
| client | `tunnel handshake timed out after 5s` | stale transport (§8) |
| shield | `Shield failed over to peer Connector` / `ignoring stale resource snapshot generation=` | failover / replay dedup |

Renewal timing: with TTL 15m and window 10m, each connector renews about every **5 min 15 s**. Predict the next one
from the last `ReEnroll sent` and arm timed tests ~15 s before it.

---

## 7. Test procedures that were run

### 7.1 Fix-connector — repeated renewal (core)
1. Start the hold, `fc-dbsamp`, `fc-probe9092`, and `sockdump` on every connector and on the shield.
2. Let it run ≥ 30 min (≥ 5 renewals per connector).
3. **Pass:**
   - every `ReEnroll` → `switched` + `superseded`;
   - **0 `disconnected`**;
   - 0 renewal-caused ACL versions;
   - hold has no FAIL;
   - shield socket/PID unchanged;
   - `:9092` never FAIL;
   - client 0 `restarting VPN`.
4. **O-1 (overlap):** in `sockdump`, the new `:9090` socket appears before the old one disappears.

### 7.2 O-2 / O-3 — admin change during a renewal
- Poll the controller log for the next `ReEnroll sent` at 50 ms, then click immediately:
  - Protect/Unprotect for O-2;
  - unassign the grant for O-3.
- The terminal tool caps a foreground wait at ~180–280 s, so start the watcher within ~3 min of the predicted time.
- **Pass:**
  - shield applies the change (nft rule removed/restored);
  - for a revoke: connector `revoked sessions torn down`, the hold is cut, and new connections fail.
- **Caveat to record:** a human click lands seconds after the renewal, never inside the ~20–250 ms overlap.

### 7.3 Test 7 — new stream fails, old stream must survive
- Ship `t7-block.sh`, then:

  ```bash
  sudo bash /tmp/t7-block.sh <renewal-15s> 50
  ```

  The user types sudo.
- **Pass:**
  - `replacement Control stream failed — keeping current stream`;
  - retries at +0/2/4/8/16 s (existing BACKOFF);
  - no controller `disconnected`;
  - switch completes after the block ends;
  - nft table `s20t7` is gone afterwards.
- `/tmp/t7.log` may be empty. Prove it ran with `journalctl _COMM=sudo | grep t7-block` and the connector journal.

### 7.4 Fix 01 — client restart decision
- **Unchanged config:** force a version bump without a content change, then expect
  `effective config unchanged, keeping tunnel` and a hold with no FAIL.
  - A controller restart works (`systemctl --user restart s20-controller` at second :35, so the
    client's :17 sync sees it). It resets versions to a low number (Finding P1-B).
  - After fix-connector, **renewals no longer bump versions**, so they no longer exercise this path.
- **Real change:** stop the shield's connector for 90 s, then start it:

  ```bash
  sudo bash -c "systemctl stop zecurity-connector; sleep 90; systemctl start zecurity-connector"
  ```

  - Expect v+1 on disconnect, v+1 on the shield moving to another connector, and v+1 on return.
  - Pre-Phase-2-A builds log `snapshot changed, restarting VPN` for each. With Phase 2-A each is a connector-only
    change: expect `transport map hot-applied successfully` and **0** `restarting VPN` (see
    `Fixes/fix 1/Phase2A-Connector-Topology-Hot-Apply.md` §Step 11). New connections should recover via the
    other connector.
  - Run `p1-newconn.sh`, `p1-shieldsamp.sh` and a hold together.
- **Phase 2-A Row C (the shield's own connector removed): the flow on it must die, nothing else.**
  1. You need **two** live connectors, and the one *not* bound to the shield must be `active` (check
     the shield journal for `peer connector list refreshed peers=2`). A hand-started connector that
     was down past its cert TTL cannot rejoin, so enroll a new one instead.
  2. Run `zecurity-client sync` and check it succeeds. Close the admin preview first, because the same
     Google user evicts the client's refresh session.
  3. Start the change-only samplers, take a ≥4-min no-event baseline (count 200/000), then start the hold
     **last** (`p2c_hold.py`, 3 h cap) and wait for `req=30 status=200`.
  4. Find the PID with `ps -eo pid,user,args | grep zecurity-connector` on the shield's connector, run
     `sudo kill <pid>`, and check that `ss -lnt | grep -c 9091` prints `0`.
  5. Expected (observed 2026-10-05): hold `FAIL TimeoutError` within ~10 s; shield `shield_via` moves
     ~25 s later; client `handshake timed out after 5s` → `early transport resync` →
     `transport-only change, hot-applying` → `hot-applied successfully`; 0 `restarting VPN`; ifindex unchanged.
  6. Restart the killed connector before its cert expires. Its return must also hot-apply.
- **Phase 2-A Row D (negative control): edit the resource port in the admin UI, not SQL.** Expect
  `structural configuration change, restarting VPN`, an ifindex change, and the hold reset.

---

## 8. How to read results (lessons)

- **"New connections failing" for ~20 s after a connector loss was really a 5 s delay per connection.**
  - The client's pooled QUIC connection to the dead connector stays "open" until the QUIC idle timeout. Each
    tunnel waits `TUNNEL_HANDSHAKE_TIMEOUT` = 5 s, then succeeds on the next connector. A `curl -m 4` probe
    reports these as 000.
  - Always compare the connector's `tunnel_opened ok` with the client's `handshake timed out` before calling it an
    outage. (Finding P1-A; input to Fix 01 Phase 2.)
- **A dropped hold isn't automatically a failure.** If the hold's path went through the connector you stopped,
  its death is expected. The question is whether new connections recover.
- **Find the cause of every ACL version.** Match each `acl push version=N` to an admin action, an enrollment,
  a disconnect, a shield move, or a controller restart. Anything left unexplained is a finding.
- **Attribute disconnects correctly.** Controller restarts produce `disconnected` lines for every connector;
  exclude them from renewal tallies and say so.
- **Report the delta.** A row the change didn't exercise is "not run", not a pass.

---

## 9. Clean-up checklist

1. Stop the holds and samplers: `systemctl --user stop s20-<name>`, and `pkill -f sockdump` on the hosts.
2. Delete the token files:
   - locally: `~/s20-run/*.env` token files and the client auth URL file;
   - on the hosts: `conn.env`, `shield*.env`, relay env files.
   - Verify: `find ~ -maxdepth 3 -name '*.env' -path '*s20*'` returns nothing.
3. Revoke stale connectors and shields in the admin UI, shield first. Leave live lab agents up unless asked.
4. Make sure no `inet s20t7` nft table is left: `sudo nft list tables`.
5. Re-enable updater timers only if the user wants the lab returned to normal.
6. Write results into the fix doc (tables + caveats + findings), and update the Hermes skill with any new gotcha.
7. Commit only when told, with docs separate from code.

---

## 10. Starting a new session quickly

1. Read this file, then the relevant fix doc's live section and the newest `Handoff-*.md`.
2. `orca-ide terminal list --json` → write `terms.env`. On every host, run `ip -br a` and `hostname`.
3. `systemctl --user is-active s20-controller s20-admin` → start them if they're down (§3).
4. DB check:

   ```sql
   select name,status,cert_not_after from connectors where status<>'revoked';
   ```

   Cert expired while a host was off? → it can't recover; create a new connector/shield and reinstall (Finding 17).
5. Binary sha check on every host (§4). Client: `zecurity-client status`, then `curl` the resource.
6. Present the plan (rows, hosts, timing) to the user and get a go-ahead before running anything.
