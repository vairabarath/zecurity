---
type: phase
status: done
sprint: 7
member: M4
phase: Phase4-Login-OneShot
depends_on:
  - M4-Phase2 (login flow done)
tags:
  - rust
  - cli
  - login
---

# M4 Phase 4 — Login (One-Shot Auth)

> **Architecture Change (Option B):** `login` is a one-shot command — authenticate, print result, exit. No long-running daemon, no IPC socket, no systemd unit. `status`, `logout`, and `invite` are redesigned as standalone commands that do not query a running daemon.

---

## What You're Building

1. **`connect` command** — loads config, calls `login::run()`, prints "Connected as {email}", exits.
2. **`status` command** — reads config file only; prints workspace + "Not connected (run `connect` to authenticate)".
3. **`logout` command** — no-op (nothing persisted to clear); prints acknowledgement.
4. **`invite` command** — runs its own login flow to get a token, then calls `POST /api/invitations`.

---

## Removed from Plan

- `client/src/ipc.rs` — **removed**. No Unix socket, no daemon query protocol.
- `client/zecurity-client.service` — **removed**. No systemd daemon.
- `RuntimeState` / `SharedState` / `new_shared()` from `runtime.rs` — still exists but is local to `connect`, not shared across processes.
- All `libc` dependency usage from `ipc.rs` — `libc` can be removed from `Cargo.toml` if unused elsewhere.

---

## `client/src/cmd/connect.rs`

```rust
use anyhow::Result;
use crate::{config::load, login};

pub async fn run() -> Result<()> {
    let conf = load()?;
    let result = login::run(&conf, None).await?;
    println!("Connected as {}", result.user.email);
    println!("Workspace:  {}", result.workspace.slug);
    println!("Device ID:  {}", result.device.device_id);
    println!("SPIFFE ID:  {}", result.device.spiffe_id);
    Ok(())
}
```

---

## `client/src/cmd/status.rs`

No daemon to query — reads config file only.

```rust
use anyhow::Result;

pub async fn run() -> Result<()> {
    match crate::config::load() {
        Ok(conf) => {
            println!("Workspace:  {}", conf.workspace);
            println!("Status:     Not connected (run `zecurity-client connect` to authenticate)");
        }
        Err(_) => println!("Status: Not configured (run `zecurity-client setup` first)"),
    }
    Ok(())
}
```

---

## `client/src/cmd/logout.rs`

Nothing persisted to clear — print acknowledgement and exit.

```rust
use anyhow::Result;

pub async fn run() -> Result<()> {
    println!("No active session to clear (sessions are not persisted).");
    Ok(())
}
```

---

## `client/src/cmd/invite.rs`

Must run its own login flow to obtain an access token (no daemon to query).

```rust
use anyhow::Result;
use reqwest::Client;
use serde::{Deserialize, Serialize};

use crate::config::load;
use crate::login;

#[derive(Serialize)]
struct InviteRequest<'a> {
    email: &'a str,
}

#[derive(Deserialize)]
struct InviteResponse {
    email:      String,
    expires_at: String,
}

pub async fn run(email: String) -> Result<()> {
    let conf = load()?;

    println!("Authenticating to get access token...");
    let result = login::run(&conf, None).await?;
    let access_token = result.session.access_token;

    let url = format!("{}/api/invitations", conf.http_base());

    let resp = Client::new()
        .post(&url)
        .bearer_auth(&access_token)
        .json(&InviteRequest { email: &email })
        .send()
        .await?;

    match resp.status().as_u16() {
        201 => {
            let inv: InviteResponse = resp.json().await?;
            println!("Invitation sent to {} (expires: {})", inv.email, inv.expires_at);
        }
        401 => return Err(anyhow::anyhow!("Authentication failed.")),
        403 => return Err(anyhow::anyhow!("Permission denied. Only admins can invite users.")),
        409 => return Err(anyhow::anyhow!("{} is already invited or a workspace member.", email)),
        s   => {
            let body = resp.text().await.unwrap_or_default();
            return Err(anyhow::anyhow!("Error {}: {}", s, body));
        }
    }
    Ok(())
}
```

---

## Files to Delete / Not Create

| File | Action |
|------|--------|
| `client/src/ipc.rs` | Delete — daemon IPC removed |
| `client/zecurity-client.service` | Delete — no systemd daemon |

---

## Build Check

```bash
cd client && cargo build
```

Manual test:
```bash
./target/debug/zecurity-client connect     # authenticates + prints result + exits
./target/debug/zecurity-client status      # prints workspace from config
./target/debug/zecurity-client logout      # prints acknowledgement
./target/debug/zecurity-client invite --email user@example.com   # re-auths + sends invite
```

---

## Post-Phase Fixes

_None yet._
