---
title: Sprint 20 Fix 11 — the update timer must honour AUTO_UPDATE_ENABLED=false (Findings 7 / 18 / 23 / 31)
date: 2026-10-24
type: fix (investigation + design + acceptance list)
status: FIXED and verified live (2026-10-24) — unit + script tests, 10/10 mutants, live systemd check PASS. Commit 905989a2 (local).
owner: Sathiya (M1)
findings: Live-Acceptance-Run-Sheet Findings 7, 18, 23, 31; reproduced 2026-10-23 (Connector-Revocation-Systemd-Live-2026-10-23.md, Finding A)
---

# Fix 11 — installing must not silently replace the build

## Problem

On every install path, the connector and shield binaries are replaced with the latest GitHub release a few seconds after install, even though auto-update is documented as **off by default** and the local installers write `AUTO_UPDATE_ENABLED=false`.

- **Seen 2026-10-23:** sd-a was replaced by release `983a4ac2` 5 s after install; sd-shield by release v1.0.11 4 s after start. So a revocation-capable build silently became a release without that work.
- **Same in Sprint 20** (Findings 7, 18, 31): branch binaries under test were replaced during the live run, which invalidated evidence.
- **Finding 23:** the units can't be masked, because the installers copy them into `/etc/systemd/system`.

## Root cause (from source)

| # | Defect | Where |
|---|---|---|
| R1 | `--check-update` → `updater::run_single_check()` **never reads `AUTO_UPDATE_ENABLED`**; it goes straight to `check_and_update`. The background loop (`run_update_loop`) does check it | `connector/src/updater.rs:123`, `shield/src/updater.rs:87`; called from `connector/src/main.rs:39`, `shield/src/main.rs:86` |
| R2 | The timers have **`Requires=zecurity-*-update.service`**, so `systemctl enable --now <timer>` also **starts the update service at once**. That is why it fires seconds after install | `connector/systemd/zecurity-connector-update.timer`, `shield/systemd/zecurity-shield-update.timer` |
| R3 | **All four installers always `enable --now` the update timer**, even with `AUTO_UPDATE_ENABLED=false` (the default in the release installers; hard-coded `false` in the local ones) | `connector/scripts/connector-install.sh:263`, `shield/scripts/shield-install.sh:312`, `scripts/connector-local-install.sh:133`, `scripts/shield-local-install.sh:127` |
| R4 | The update service units hard-code **`Environment=AUTO_UPDATE_ENABLED=true`** | `connector/systemd/zecurity-connector-update.service`, `shield/systemd/zecurity-shield-update.service` |

**Contributing factor (not changed here):** branch builds use the release version numbers (branch 1.0.17 < release 1.0.18), so the updater treats a newer branch build as "older". That is why auto-update must stay off on branch and test installs.

## Proposed fix (no proto, no schema, no architecture change: it makes the documented default true)

1. **Binaries (R1).** `--check-update` reads `AUTO_UPDATE_ENABLED` from the environment, which systemd fills from `/etc/zecurity/{connector,shield}.conf` via the unit's `EnvironmentFile=`.
   - If it isn't truthy, log `auto-update is disabled; skipping update check` and exit 0 without any network call.
   - Parsing is fail-safe: only `true`/`1`/`yes` (case-insensitive, trimmed) enable it; unset, empty or anything else means **off**.
   - Same in the connector and the shield.
2. **Timer units (R2).** Remove `Requires=` from both timers. A timer activates its same-named service on schedule without it, so enabling or starting the timer no longer runs an update immediately.
3. **Service units (R4).** Remove `Environment=AUTO_UPDATE_ENABLED=true`. The value comes only from the `.conf`.
4. **Installers (R3), all four.**
   - Install the unit files as today.
   - Then: if `AUTO_UPDATE_ENABLED=true` → `systemctl enable --now <timer>`, which no longer runs an update at install because of item 2.
   - Otherwise → `systemctl disable --now <timer>` and `systemctl stop <service>`, so a re-install also turns a previously enabled timer off.
   - Log which branch was taken.
5. **Existing installs** keep their old unit files until re-installed. The lab re-install is part of the live check.

## Acceptance

| # | Check | Level |
|---|---|---|
| AF11-1 | Connector: `--check-update` with `AUTO_UPDATE_ENABLED` unset, empty, `false`, `0` or `garbage` → returns Ok **without** building an HTTP client or making any request; `true`/`1`/`yes` (any case, surrounding spaces) → proceeds | connector unit |
| AF11-2 | Shield: the same | shield unit |
| AF11-3 | Units: no `Requires=` in either timer; no `Environment=AUTO_UPDATE_ENABLED` in either service; `systemd-analyze verify` passes (if available) | static check |
| AF11-4 | Installers (4): with `AUTO_UPDATE_ENABLED=false` the timer is disabled and the update service is not started; with `true` the timer is enabled and started, and the update service does **not** run at install | `bash -n` + review; live (AF11-L) |
| AF11-5 | Existing suites green (connector, shield, controller, client, relay) | suites |
| AF11-6 | Mutation checks: the gate in each binary, the parser's fail-safe default, the installer branch | mutants |
| AF11-L | **Live (systemd, lab):** re-install a connector and a shield with the local installers → **no** `running single update check` in the journal, binary sha unchanged after ≥ 2 min, timer `disabled`. Then `systemctl start zecurity-<x>-update.service` by hand → logs `auto-update is disabled`, exits 0, binary unchanged | live |

**Not covered live:** the release installers (they download release binaries from GitHub). They get the same code change and a review, not a live run.

## Implementation (2026-10-24, approved by the user)

| Area | Change |
|---|---|
| `connector/src/updater.rs`, `shield/src/updater.rs` | `run_single_check()` → `run_single_check_gated(auto_update_enabled_from_env(), …)`. The check (HTTP client + `check_and_update`) runs only when enabled; otherwise it logs `auto-update is disabled; skipping update check` and returns Ok. `parse_auto_update` is fail-safe: `true`/`1`/`yes`, trimmed, case-insensitive |
| update timers | `Requires=` removed (comment explains why) |
| update services | `Environment=AUTO_UPDATE_ENABLED=true` removed; the value comes from the `.conf` only |
| `connector/scripts/connector-install.sh`, `shield/scripts/shield-install.sh` | `configure_update_timer`: enable + start the timer only for `true|1|yes`; otherwise `disable --now` the timer and stop the service (re-installs too) |
| `scripts/connector-local-install.sh`, `scripts/shield-local-install.sh` | always `disable --now` the timer and stop the service (branch builds; `.conf` says false) |
| `scripts/test-update-timer-gate.sh` (new) | no-root test: extracts `configure_update_timer` from both release installers and runs it against a fake `systemctl` for 11 values; checks the local installers and the units |

## Tests

- Connector **242 → 244**, shield **42 → 44**: `auto_update_is_off_unless_explicitly_true`, `check_update_does_nothing_when_disabled`.
- `scripts/test-update-timer-gate.sh`: 28/28 PASS.
- Controller, client and relay build.
- **Mutation checks: 10/10 killed.**
  - M1/M2: the gate in the connector/shield.
  - M3/M5: the parser defaulting to on.
  - M4: trim.
  - M6: the release installer accepting `false`.
  - M7: the disable path removed.
  - M8: the local installer enabling the timer.
  - M9: `Requires=` back.
  - M10: the service env back.
- **Known gap:** `run_single_check` wiring the env value through is not unit-tested. Such a test would run a real update check that could replace the test binary. The live check covers it.

## Live check AF11-L (2026-10-24): PASS

**Setup:** controller `6d729a7` restarted; network `sd-net`. Installed with the **fixed local installers only**: the wrapper `f11-run.sh` runs uninstall + installer and does no timer handling of its own. sudo was typed by the user.

| Host | Component | Installed (UTC) | Installer log | After install | ≥ 2 min later | Manual `systemctl start zecurity-*-update.service` |
|---|---|---|---|---|---|---|
| inkyank .75 | connector `f11-a` (`335d3cd0…`), replaced the revoked sd-a | 05:32:36 | `auto-update off: zecurity-connector-update.timer disabled` | timer `disabled`/`inactive`; disk = running = `bcf87647`; 0 update runs | 05:36:43: unchanged, 0 update runs, 0 restarts | `running single update check` → **`auto-update is disabled; skipping update check`**, exit 0, binary unchanged |
| udaya .41 | shield `f11-shield` (`551396ef…`), replaced sd-shield; attached to f11-a | 05:34:06 | `auto-update off: zecurity-shield-update.timer disabled` | timer `disabled`/`inactive`; disk = running = `22dfc611`; 0 update runs | 05:36:45: unchanged, 0 update runs, 0 restarts | same: **skipped**, exit 0, binary unchanged |

Compare 2026-10-23, same hosts, old installers: both binaries were replaced by GitHub releases 4–5 s after install.

**Not covered live:**
- the release installers (`connector-install.sh`, `shield-install.sh`), which download release binaries; covered by the script test and review;
- `AUTO_UPDATE_ENABLED=true` end to end (it would really update); covered by the script test.

**Existing installs keep their old unit files until re-installed.** sd-b on manojinkyank still has the old units (timer disabled by hand on 2026-10-23).

Nothing is pushed until the user asks.
