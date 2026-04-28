---
type: phase
status: planned
sprint: 7
member: M4
phase: Phase3-Invite-Command
depends_on:
  - M4-Phase2 (login flow done — RuntimeState carries access_token)
  - M3-Phase2 (POST /api/invitations HTTP endpoint live)
tags:
  - rust
  - cli
  - invitation
  - http
---

# M4 Phase 3 — Invite Command

---

## What You're Building

`zecurity-client invite --email user@example.com`

The `invite` command is run interactively by an admin while the `connect` daemon is running. It queries the running daemon for the current access token via Unix socket (Phase 4), then calls `POST /api/invitations` on the Controller HTTP API.

Since the session is in-memory only, `invite` cannot run standalone without the daemon. If the daemon is not running, it prints a clear error.

---

## Controller HTTP Base URL

The Controller runs gRPC on port 9090. The HTTP API is on a separate port — check `cmd/server/main.go` for the `LISTEN_ADDR` or `HTTP_PORT` env var. The HTTP base URL is derived from the controller address:

- gRPC address: `controller.example.com:9090`
- HTTP base URL: resolved from the same host with the HTTP port

In `client.conf`, an optional `http_base_url` field can be set for dev:
```toml
workspace = "myworkspace"
controller_address = "localhost:9090"
http_base_url = "http://localhost:8080"   # dev only; prod = embedded constant
```

Add to `config.rs`:
```rust
pub struct ClientConf {
    pub workspace:          String,
    #[serde(default)] pub controller_address: String,
    #[serde(default)] pub connector_address:  String,
    #[serde(default)] pub http_base_url:      String,  // NEW
}

impl ClientConf {
    pub fn http_base(&self) -> &str {
        if self.http_base_url.is_empty() {
            crate::appmeta::DEFAULT_HTTP_BASE_URL
        } else {
            &self.http_base_url
        }
    }
}
```

Add to `appmeta.rs`:
```rust
pub const DEFAULT_HTTP_BASE_URL: &str =
    option_env!("ZECURITY_HTTP_BASE_URL").unwrap_or("");
```

---

## `client/src/cmd/invite.rs`

```rust
use anyhow::{anyhow, Result};
use reqwest::Client;
use serde::{Deserialize, Serialize};

use crate::config::load;
use crate::ipc::query_daemon_token;  // Phase 4 implements this

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

    // Get access token from running daemon's in-memory session
    let access_token = query_daemon_token().await
        .map_err(|_| anyhow!(
            "Not connected. Run `zecurity-client connect` first."
        ))?;

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
        401 => return Err(anyhow!("Session expired. Restart `zecurity-client connect`.")),
        403 => return Err(anyhow!("Permission denied. Only admins can invite users.")),
        409 => return Err(anyhow!("{} is already invited or a workspace member.", email)),
        s   => {
            let body = resp.text().await.unwrap_or_default();
            return Err(anyhow!("Error {}: {}", s, body));
        }
    }
    Ok(())
}
```

> `query_daemon_token()` is a stub until Phase 4 implements the Unix socket IPC. For Phase 3 testing, replace it temporarily with a hardcoded token read from an env var (`ZECURITY_DEV_TOKEN`) to allow end-to-end testing of the HTTP call.

---

## Build Check

```bash
cd client && cargo build
```

---

## Post-Phase Fixes

_None yet._
