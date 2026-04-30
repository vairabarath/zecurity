---
type: phase
status: done
sprint: 8.5
member: M4
phase: Phase1-Daemon-Scaffold-IPC
depends_on: []
tags:
  - rust
  - client
  - daemon
  - ipc
  - systemd
---

# M4 Phase 1 — Daemon Scaffold + IPC

---

## What You're Building

Create the daemon foundation required for active client runtime state.

The daemon owns:

- decrypted private key during active use
- active access token
- active ACL snapshot
- future TUN/routes/tunnel state

The CLI owns:

- argument parsing
- starting daemon if needed
- running the existing PKCE/browser login flow
- sending IPC requests
- printing results

---

## Files To Create / Modify

Likely files:

- `client/src/daemon.rs`
- `client/src/ipc.rs`
- `client/src/main.rs`
- `client/src/runtime.rs`
- `client/src/cmd/login.rs`
- `client/src/cmd/status.rs`
- `client/src/cmd/logout.rs`
- `client/src/cmd/invite.rs`
- `client/zecurity-client.service`

---

## IPC Requirements

Use a Unix socket under a user runtime directory, for example:

```text
$XDG_RUNTIME_DIR/zecurity-client/daemon.sock
```

Messages:

- `Status`
- `LoadState`
- `GetToken`
- `PostLoginState`
- `Shutdown`
- `Up` — stub only in Sprint 8.5; implemented in Sprint 9 Phase F. Return `{"ok":false,"error":"not implemented"}` for now.
- `Down` — stub only in Sprint 8.5; implemented in Sprint 9 Phase F. Return `{"ok":false,"error":"not implemented"}` for now.

Wire format:

- newline-delimited JSON
- one request or response object per line
- `\n` is the frame delimiter
- unknown message types return an error response
- malformed JSON returns an error response and closes the connection

Example request:

```json
{"type":"Status"}
```

Example response:

```json
{"ok":true,"type":"Status","state":"running"}
```

`PostLoginState` intent:

- CLI runs the existing OAuth/PKCE/browser callback flow in `login.rs`.
- CLI receives tokens, device info, cert, private key, and workspace info.
- CLI sends that result to daemon as `PostLoginState`.
- Daemon writes encrypted durable state through `state_store.rs`.
- Daemon stores decrypted private key/access token/ACL snapshot in memory.

Do not make the daemon run the browser OAuth flow in Sprint 8.5.

Security:

- Same-user only.
- Remove stale socket on daemon startup.
- Commands fail closed if IPC fails after daemon start retry.

---

## Systemd User Unit

Install target:

```text
/etc/systemd/system/zecurity-client.service
```

This is a system-level unit, not a `systemctl --user` unit. The installer sets `User=<enrolling_user>` so the daemon runs as the user while still receiving `CAP_NET_ADMIN` from systemd.

The service file lives at `client/zecurity-client.service` and is installed to `/etc/systemd/system/zecurity-client.service` by the enrollment script, which also sets `User=` to the enrolling user.

Full file contents (reference copy — do not change `AmbientCapabilities`):

```ini
[Unit]
Description=Zecurity Client Daemon
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
# User= is set by the installation script to the enrolling user.
# This is a system-level unit (/etc/systemd/system/) — not a user unit.
# AmbientCapabilities requires system-level placement to take effect.
User=
ExecStart=/usr/local/bin/zecurity-client daemon
Restart=on-failure
RestartSec=5
WatchdogSec=90

# CAP_NET_ADMIN is required for TUN interface creation and route management.
# The daemon does not run as root — this is the minimum required capability.
# Works here because this is a system unit. Would not work in --user unit.
AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN
NoNewPrivileges=yes

StandardOutput=journal
StandardError=journal

RuntimeDirectory=zecurity-client
RuntimeDirectoryMode=0700

[Install]
WantedBy=multi-user.target
```

Sprint 8.5 daemon does **not** use `CAP_NET_ADMIN` — it is user-only. The capability is required by Sprint 9 Phase F when the daemon creates the TUN interface. Do not remove it from the service file.

CLI startup flow:

```text
try IPC
  -> success: use daemon
  -> fail: systemctl start zecurity-client
  -> retry IPC with short timeout
```

---

## sd_notify — Required for Type=notify

The service file uses `Type=notify`. If the daemon does not call `sd_notify("READY=1\n")` after binding the IPC socket, `systemctl start zecurity-client` will hang until the start timeout expires.

Add this to `daemon.rs` immediately after the IPC socket is bound and ready to accept connections:

```rust
fn sd_notify_ready() {
    let Ok(path) = std::env::var("NOTIFY_SOCKET") else { return };
    let _ = std::os::unix::net::UnixDatagram::unbound()
        .and_then(|s| s.send_to(b"READY=1\n", &path));
}
```

Call `sd_notify_ready()` once, after the socket is bound, before entering the accept loop.

---

## Build Check

```bash
cd client && cargo build
```

Manual checks:

```bash
systemctl start zecurity-client    # must return immediately (not hang)
systemctl status zecurity-client   # must show active (running)
zecurity-client status
zecurity-client logout
```

---

## Files Touched

### Created
| File | What |
|------|------|
| `client/src/ipc.rs` | `IpcRequest` / `IpcResponse` types, `ipc_socket_path()`, `check_same_user()` via `SO_PEERCRED`, `send_ipc()`, `ensure_daemon_and_send()` |
| `client/src/daemon.rs` | `run()` entry point, Unix socket accept loop, `handle_request()` for all 7 message types, `sd_notify_ready()`, `populate_runtime()` |

### Modified
| File | What |
|------|------|
| `client/src/main.rs` | Added `mod daemon`, `mod ipc`; added hidden `Daemon` subcommand; wired dispatch |
| `client/zecurity-client.service` | Updated from old `connect` subcommand to `daemon`; added `User=`, `AmbientCapabilities=CAP_NET_ADMIN`, `CapabilityBoundingSet`, `NoNewPrivileges=yes`, changed `WantedBy` to `multi-user.target` |
| `client/Cargo.toml` | Added `tracing = "0.1"` and `tracing-subscriber = { version = "0.3", features = ["env-filter"] }` |

---

## Post-Phase Fixes

### Fix: `ClientConf` missing `Clone`

**Issue:** `daemon.rs` clones `ClientConf` per accepted connection to move into the spawned task. Build failed with `no method named clone found for struct ClientConf`.

**Root Cause:** `ClientConf` in `config.rs` only derived `Debug, Serialize, Deserialize, Default` — `Clone` was missing.

**Fix Applied (`client/src/config.rs`):**
```rust
// BEFORE:
#[derive(Debug, Serialize, Deserialize, Default)]
pub struct ClientConf {

// AFTER:
#[derive(Debug, Clone, Serialize, Deserialize, Default)]
pub struct ClientConf {
```
