---
title: Sprint 20 Fix 11 — the update timer must honour AUTO_UPDATE_ENABLED=false (Findings 7 / 18 / 23 / 31)
date: 2026-10-24
type: fix (investigation + design + acceptance list)
status: PROPOSED — awaiting the user's sign-off before any code change
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

Nothing is pushed until the user asks.
