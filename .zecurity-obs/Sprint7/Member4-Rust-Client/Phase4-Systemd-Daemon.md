---
type: phase
status: planned
sprint: 7
member: M4
phase: Phase4-Systemd-Daemon
depends_on:
  - M4-Phase2 (login flow done)
tags:
  - rust
  - cli
  - systemd
  - daemon
  - ipc
---

# M4 Phase 4 — Systemd Service + Connect Daemon + IPC

---

## What You're Building

1. **`connect` command** — the long-running entrypoint: reads `client.conf`, calls `login::run()`, stores `RuntimeState` in memory, reconnect loop, sd_notify integration.
2. **Unix socket IPC** — other CLI commands (`status`, `invite`, `logout`) query the running daemon for in-memory state without any disk writes.
3. **Systemd unit file** — `zecurity-client.service` for running as a managed service.

---

## IPC Design

Unix domain socket at `/run/zecurity-client.sock` (system) or `/tmp/zecurity-client-{uid}.sock` (user).

Simple newline-delimited JSON protocol:
```
→ {"cmd":"status"}\n
← {"email":"user@example.com","workspace":"myworkspace","spiffe_id":"...","session_expires_at":1234567890}\n

→ {"cmd":"token"}\n
← {"access_token":"eyJ..."}\n

→ {"cmd":"logout"}\n
← {"ok":true}\n
```

---

## `client/src/ipc.rs`

```rust
use anyhow::{anyhow, Result};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::net::UnixListener;
use tokio::net::UnixStream;

use crate::runtime::SharedState;

const SOCK_PATH: &str = "/run/zecurity-client.sock";

pub fn sock_path() -> String {
    // Use user-scoped socket if /run is not writable
    if std::path::Path::new("/run").exists()
        && std::fs::metadata("/run").map(|m| !m.permissions().readonly()).unwrap_or(false)
    {
        SOCK_PATH.to_string()
    } else {
        let uid = unsafe { libc::getuid() };
        format!("/tmp/zecurity-client-{}.sock", uid)
    }
}

// ── Server (runs inside connect daemon) ───────────────────────────────────

pub async fn serve_ipc(state: SharedState) {
    let path = sock_path();
    let _ = std::fs::remove_file(&path);  // clean up stale socket
    let listener = match UnixListener::bind(&path) {
        Ok(l) => l,
        Err(e) => { eprintln!("IPC socket error: {}", e); return; }
    };

    loop {
        let Ok((stream, _)) = listener.accept().await else { continue };
        let state = state.clone();
        tokio::spawn(handle_ipc_conn(stream, state));
    }
}

#[derive(Deserialize)]
struct IpcRequest { cmd: String }

#[derive(Serialize)]
#[serde(untagged)]
enum IpcResponse {
    Status {
        email:              String,
        workspace:          String,
        spiffe_id:          String,
        session_expires_at: i64,
        connected:          bool,
    },
    Token  { access_token: String },
    Ok     { ok: bool },
    Error  { error: String },
}

async fn handle_ipc_conn(stream: UnixStream, state: SharedState) {
    let (reader, mut writer) = stream.into_split();
    let mut lines = BufReader::new(reader).lines();

    while let Ok(Some(line)) = lines.next_line().await {
        let resp = match serde_json::from_str::<IpcRequest>(&line) {
            Err(_) => IpcResponse::Error { error: "invalid request".into() },
            Ok(req) => {
                let st = state.read().await;
                match req.cmd.as_str() {
                    "status" => IpcResponse::Status {
                        email:              st.user.as_ref().map(|u| u.email.clone()).unwrap_or_default(),
                        workspace:          st.workspace.as_ref().map(|w| w.slug.clone()).unwrap_or_default(),
                        spiffe_id:          st.device.as_ref().map(|d| d.spiffe_id.clone()).unwrap_or_default(),
                        session_expires_at: st.session.as_ref().map(|s| s.expires_at).unwrap_or(0),
                        connected:          st.device.is_some(),
                    },
                    "token" => match st.session.as_ref() {
                        Some(s) => IpcResponse::Token { access_token: s.access_token.clone() },
                        None    => IpcResponse::Error { error: "not authenticated".into() },
                    },
                    "logout" => {
                        drop(st);
                        let mut st = state.write().await;
                        *st = Default::default();
                        // Signal the connect loop to exit by setting a flag (see connect.rs)
                        IpcResponse::Ok { ok: true }
                    }
                    _ => IpcResponse::Error { error: "unknown command".into() },
                }
            }
        };
        let mut out = serde_json::to_string(&resp).unwrap_or_default();
        out.push('\n');
        if writer.write_all(out.as_bytes()).await.is_err() { break; }
    }
}

// ── Client (runs in status/invite/logout subcommands) ─────────────────────

async fn ipc_call(cmd: &str) -> Result<String> {
    let path = sock_path();
    let mut stream = UnixStream::connect(&path).await
        .map_err(|_| anyhow!("Daemon not running. Start with `zecurity-client connect`."))?;
    let req = format!("{{\"cmd\":\"{}\"}}\n", cmd);
    stream.write_all(req.as_bytes()).await?;
    let mut lines = BufReader::new(stream).lines();
    lines.next_line().await?.ok_or_else(|| anyhow!("no response from daemon"))
}

pub async fn query_daemon_status() -> Result<serde_json::Value> {
    let raw = ipc_call("status").await?;
    Ok(serde_json::from_str(&raw)?)
}

pub async fn query_daemon_token() -> Result<String> {
    let raw = ipc_call("token").await?;
    let v: serde_json::Value = serde_json::from_str(&raw)?;
    v["access_token"].as_str().map(|s| s.to_string())
        .ok_or_else(|| anyhow!("no token in response"))
}

pub async fn signal_daemon_logout() -> Result<()> {
    ipc_call("logout").await?;
    Ok(())
}
```

Add to `Cargo.toml`:
```toml
libc = "0.2"
```

---

## `client/src/cmd/connect.rs`

```rust
use anyhow::Result;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use tokio::signal;
use tokio::time::{sleep, Duration};

use crate::{config::load, ipc::serve_ipc, login, runtime::new_shared};

pub async fn run() -> Result<()> {
    let conf = load()?;
    let state = new_shared();
    let shutdown = Arc::new(AtomicBool::new(false));

    // Spawn IPC socket server
    tokio::spawn(serve_ipc(state.clone()));

    sd_notify("READY=1");

    loop {
        if shutdown.load(Ordering::Relaxed) {
            println!("Shutting down.");
            break;
        }

        println!("Authenticating...");
        match login::run(&conf, None).await {
            Ok(result) => {
                // Populate in-memory state
                {
                    let mut st = state.write().await;
                    st.workspace = Some(result.workspace);
                    st.user      = Some(result.user);
                    st.device    = Some(result.device);
                    st.session   = Some(result.session);
                }
                println!("Connected as {}", state.read().await.user.as_ref().map(|u| u.email.as_str()).unwrap_or("?"));

                // Run tunnel (Phase 5 replaces this placeholder)
                tokio::select! {
                    _ = tunnel_placeholder() => {
                        eprintln!("Tunnel ended. Reconnecting in 5s...");
                    }
                    _ = signal::ctrl_c() => {
                        println!("Shutting down.");
                        return Ok(());
                    }
                }
            }
            Err(e) => {
                eprintln!("Login failed: {}. Retrying in 10s...", e);
            }
        }

        // Clear session on disconnect
        *state.write().await = Default::default();

        tokio::select! {
            _ = sleep(Duration::from_secs(5)) => {}
            _ = signal::ctrl_c() => return Ok(()),
        }

        sd_notify("WATCHDOG=1");
    }
    Ok(())
}

async fn tunnel_placeholder() {
    // Phase 5 replaces this with TunTunnel::run()
    sleep(Duration::from_secs(u64::MAX)).await;
}

fn sd_notify(msg: &str) {
    if let Ok(addr) = std::env::var("NOTIFY_SOCKET") {
        use std::os::unix::net::UnixDatagram;
        if let Ok(sock) = UnixDatagram::unbound() {
            sock.send_to(msg.as_bytes(), addr.trim_start_matches('@')).ok();
        }
    }
}
```

---

## Updated `cmd/status.rs` and `cmd/logout.rs`

**`status.rs`** — now uses IPC:
```rust
use anyhow::Result;
use std::time::{SystemTime, UNIX_EPOCH};

pub async fn run() -> Result<()> {
    match crate::ipc::query_daemon_status().await {
        Ok(v) => {
            let now = SystemTime::now().duration_since(UNIX_EPOCH).unwrap().as_secs() as i64;
            let expires = v["session_expires_at"].as_i64().unwrap_or(0);
            let remaining = expires - now;
            let session_str = if remaining > 0 {
                format!("Active (expires in {}h)", remaining / 3600)
            } else {
                "Expired".into()
            };
            println!("Email:      {}", v["email"].as_str().unwrap_or("—"));
            println!("Workspace:  {}", v["workspace"].as_str().unwrap_or("—"));
            println!("Status:     Connected");
            println!("Session:    {}", session_str);
            println!("Device:     Verified");
            println!("SPIFFE ID:  {}", v["spiffe_id"].as_str().unwrap_or("—"));
        }
        Err(_) => {
            match crate::config::load() {
                Ok(conf) => {
                    println!("Workspace:  {}", conf.workspace);
                    println!("Status:     Not connected");
                }
                Err(_) => println!("Status: Not configured"),
            }
        }
    }
    Ok(())
}
```

**`logout.rs`** — now uses IPC:
```rust
use anyhow::Result;

pub async fn run() -> Result<()> {
    crate::ipc::signal_daemon_logout().await
        .map_err(|_| anyhow::anyhow!("Daemon not running."))?;
    println!("Session cleared. Daemon will reconnect on next `connect`.");
    Ok(())
}
```

---

## Systemd Unit File: `client/zecurity-client.service`

```ini
[Unit]
Description=Zecurity Client Tunnel
After=network-online.target
Wants=network-online.target

[Service]
Type=notify
ExecStart=/usr/local/bin/zecurity-client connect
Restart=on-failure
RestartSec=5
WatchdogSec=90

StandardOutput=journal
StandardError=journal

RuntimeDirectory=zecurity-client
RuntimeDirectoryMode=0700

[Install]
WantedBy=default.target
```

For system install (runs as root or dedicated user — gets `/run/zecurity-client.sock`):
```bash
sudo cp target/release/zecurity-client /usr/local/bin/
sudo cp zecurity-client.service /etc/systemd/system/
sudo systemctl enable --now zecurity-client
```

For user install:
```bash
cp target/release/zecurity-client ~/.local/bin/
cp zecurity-client.service ~/.config/systemd/user/
systemctl --user enable --now zecurity-client
```

---

## Build Check

```bash
cd client && cargo build
```

Test IPC:
```bash
# Terminal 1:
./target/debug/zecurity-client connect   # starts daemon + IPC socket

# Terminal 2:
./target/debug/zecurity-client status    # queries daemon via Unix socket
./target/debug/zecurity-client logout    # clears in-memory session
```

---

## Post-Phase Fixes

_None yet._
