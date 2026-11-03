---
title: Connector Revocation — live check with installed (systemd) builds and a root shield
date: 2026-10-23
type: live test record
branch: feat/sprint20-m1-phase2 (connector `e174ee78`, shield `82442a7e`, code at 0d83b776)
status: PASS for revocation on installed services; 4 findings (1 pre-existing reproduced, 3 new), see "Findings"
---

# Live check: installed systemd services + root shield

**Goal:** the part the earlier runs didn't cover. Install the current connector and shield builds with the product's own installers (`scripts/*-local-install.sh`, systemd units, the `zecurity` service user). Then revoke a connector while a **root** shield enforces nftables protection.

## Setup

| Component | Host | How | Build |
|---|---|---|---|
| controller | this PC (`k2-controller`) | user unit | `6d729a7` |
| relay | manojinkyank .49 | installed (systemd) | Phase D |
| **sd-a** (the one revoked) | inkyank .75 | `connector-local-install.sh` → `zecurity-connector.service` | `e174ee78` |
| **sd-b** (peer) | manojinkyank .49 | same | `e174ee78` |
| **sd-shield** | udaya .41 | `shield-local-install.sh` → `zecurity-shield.service` (root, nftables) | `82442a7e` |
| resource `sd-web` | udaya 192.168.1.41:18091, `python3 -m http.server --protocol HTTP/1.1` | **Protected** by sd-shield; group `g9-users` | — |
| client | this PC, installed daemon | — | Phase C build |

- **Bundle:** `sd-bundle.tgz`: installers + units + binaries + `sd-run.sh` (install wrapper) + `sd-pin.sh` (re-pin after Finding A).
- **Tokens:** sent through hidden prompts into 0600 `~/sd.env` files, shredded by the wrapper.
- **sudo:** typed by the user in the Orca tabs.
- **Replaced on the way in:**
  - on inkyank, an old installed connector (`6c442e1b`) that was in a reconnect loop (`NRestarts=149` since boot);
  - on udaya, an old installed shield (`2910b0e2`) and an old `zecurity-connector` unit stuck in `activating` (a restart loop).

**Instruments:**
- client probes on this PC through the tunnel: a **true long-lived HTTP/1.1 keep-alive connection** (GET every 0.5 s) and a new connection every 0.5 s;
- a **direct LAN probe from inkyank** to 192.168.1.41:18091 every 0.5 s, which must stay blocked by the shield's nftables;
- the journals of every unit.

## Results

**Baseline:**
- sd-shield attached to sd-a; `Protect` → `rebuilt nftables resource_protect chain … resource_count=1`.
- Through the tunnel: 3 × 200. Direct from inkyank: **000 (blocked)**.
- All traffic on sd-a: 44 tunnel/access lines on sd-a, 0 on sd-b.

**Revoke sd-a at 11:15:36.298 UTC** (its systemd process keeps running):

| Event | Time | After revoke |
|---|---|---|
| shield: `connector is revoked or unknown; trying the next peer connector` (Phase F fix B) | 11:15:36.30 | ~0 |
| `deleteConnector(sd-a)` → **refused** `1 shield(s) still attached` | 11:15:36.305 | 7 ms |
| sd-a: `connector identity REVOKED — data plane closed`, `fail-closed: every session cancelled` | 11:15:36.314 | 16 ms |
| **long-lived HTTP/1.1 flow cut** (`held: FAIL ConnectionResetError`) | 11:15:36.735 | 437 ms |
| shield: Control stream established on **sd-b**; sd-a pruned (Phase E) | 11:15:38.644 | 2.35 s |
| client traffic back (held 11:15:38.83, fresh 11:15:39.07) | | **2.5–2.8 s** |
| shield row re-pointed to sd-b | by 11:15:49 | |
| relay: `revoked connector: Relay connection closed`, removed | 11:15:58.84 | 22.5 s |
| `deleteConnector(sd-a)` → true; tombstone at 11:15:36; reuse of the id → refused | 11:16:17 | |
| **direct LAN access** | whole run | **BLOCKED throughout** (one log line, no change) |

**systemd behaviour:**
- **Revoked sd-a** stays `active running`, `NRestarts=0`. The process stays up in its terminal state, so there's no restart loop.
- **`systemctl restart` of the revoked and deleted sd-a:** it starts and goes terminal on `connector identity UNKNOWN` (11:16:52.38). It stays up and doesn't restart-loop.
- **`systemctl restart` of the shield** (11:16:52.63):
  - it resumed **directly on sd-b** from the saved list (`resuming connector_id=258d3869…`), so the pruned/rotated list survived the restart;
  - re-attached at 11:16:53.05;
  - client disruption ~3.5 s;
  - **direct access stayed blocked** (no nftables gap seen at 0.5 s granularity).

**Verdict:** PASS. Revocation enforcement works on installed systemd services with a root shield enforcing nftables, including the restarts.

## Findings

> Filed in `Sprint20/path.md` → Post-Sprint Fixes: **A = F7** (updated, still open), **B = F37**, **C = F38**, **D = F39**; also F40–F43.

| # | Finding | Evidence | Severity | Status |
|---|---|---|---|---|
| **A** | **Installing silently replaces the local build with an older GitHub release.** The installers enable `zecurity-{connector,shield}-update.timer`; it is `Persistent=true`, so it fires immediately. `--check-update` then ignores `AUTO_UPDATE_ENABLED=false` (written by the installer) and replaces the binary | sd-a replaced by release `983a4ac2` at 16:39:16 local, 5 s after start; shield replaced by release v1.0.11 (`0131b3a8`) at 16:40:30, 4 s after start. The updater also spawned a detached "restart + health-check" process | **High**: a revocation-capable build is replaced by a release without the revocation work; any local or test install is silently changed | **Pre-existing = Sprint 20 Finding 7, still unfixed.** Lab workaround: `sd-pin.sh` (disable timer + service, reinstall, restart). Fix: installers must not enable the timer when auto-update is off, `--check-update` must honour the setting, and the unit's `Environment=AUTO_UPDATE_ENABLED=true` override should go |
| **B** | **The client refuses concurrent new connections to the same resource.** The userspace stack keeps **one smoltcp listening socket per (ip, port)** (`client/src/net_stack.rs` `new_listen_socket`, re-armed only on a later tick, ~:697). SYNs arriving before the re-arm get RST and never reach the client's flow table | Same resource, same minute: **24/24 sequential OK** vs **5/24 OK in parallel bursts of 8** (19 refused). Refused attempts have no `new TCP connection` line in the client journal. Also the first-probe refusal at 11:15:13.560 (two threads connecting at once) | **High (data plane)**: browsers open ~6 parallel connections per host | **New** (not revocation-related). It **explains the "isolated single refusals"** listed as unexplained in the E2E record. Fix: keep a small pool of listening sockets per key (backlog) and top it up every poll |
| **C** | **A revoked (terminal) connector keeps retrying relay registration.** The control loop stops, but the relay selector keeps reconnecting with backoff (11:16:03, :13, :33, …) and the relay refuses each attempt | sd-a journal after the revoke | Low (noise / wasted connections) | New. Fix: stop the relay selector on entering a terminal state |
| **D** | **The shield's shutdown goodbye uses the connector list loaded at startup**, not the current one, so after a revocation/failover it contacts the pruned or stale head first | shutdown at 16:46:52 tried revoked sd-a (`peer Connector unreachable, trying next connector_id=ec894f0b…`) | Low (non-fatal, delays shutdown slightly) | New. `shield/src/main.rs:211` passes the startup `state`. Fix: share the live state with the shutdown path |

**Known, unchanged:** a deleted revoked connector is told `UNKNOWN` (not `revoked`); both are terminal (seen again at the sd-a restart).

## Unexplained behaviour from earlier live runs — status after this run

| Item (from the E2E record) | Status |
|---|---|
| Isolated single connection refusals around client ACL hot-apply / probe start | **Explained by Finding B** (concurrent SYNs to the same resource). Hot-apply rebuilds listeners, which widens the window |
| Shield failover once took 12.9 s (stalled ~11 s connecting to manojinkyank .49:9091) | **Not reproduced.** Here the failover to sd-b on the same host .49 took 2.35 s, and the restart re-attach 0.34 s. Still unexplained; keep watching |
| Repeated `ignored empty PeerConnectorList` shield warning | Not exercised (this network has a resource, so peer lists are non-empty) |

## Lab state after the run

- **Installed and running** (systemd, our builds, update timers disabled):
  - sd-b on manojinkyank (active);
  - sd-shield on udaya (active, attached to sd-b, protecting `sd-web`);
  - sd-a on inkyank (**revoked + deleted**, still running in terminal state, retrying the relay; Finding C).
- **Old installs removed:** old connectors on inkyank and udaya, and the old shield on udaya.
- Web server `sd-web` on udaya :18091 still running. Probes stopped.
- DB: `sd-net`, `sd-web` (protected), `sd-b`, `sd-shield`; sd-a tombstoned.

Committed locally. Not pushed (push only on request).
