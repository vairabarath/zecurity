---
type: phase
status: done
sprint: 7
member: M4
phase: Phase1-CLI-Scaffold
depends_on: []
tags:
  - rust
  - cli
  - client
---

# M4 Phase 1 — Rust CLI Scaffold + setup / status / logout

> **No dependencies — can start on Day 1.**

---

## Storage Architecture

**Only one file ever touches disk:**

```
/etc/zecurity/client.conf          (system install)
~/.config/zecurity-client/client.conf  (user install fallback)
```

Contents (TOML):
```toml
workspace = "myworkspace"

# Only present in dev/non-prod builds.
# In production, controller_address and connector_address are compiled-in constants.
controller_address = "controller.example.com:9090"
connector_address  = "connector.example.com:9092"
```

**Everything else lives in memory only.** Session tokens, device certificate, private key — none of it is ever written to disk. When the process exits, it's gone.

---

## Compile-Time Controller Defaults (Production)

Follow the existing `appmeta` pattern used by the controller and connector. Add a new file:

### `client/src/appmeta.rs`

```rust
// These are overridden at build time for production releases.
// Dev builds fall back to client.conf values.
pub const DEFAULT_CONTROLLER_ADDRESS: &str =
    option_env!("ZECURITY_CONTROLLER_ADDRESS").unwrap_or("");
pub const DEFAULT_CONNECTOR_ADDRESS: &str =
    option_env!("ZECURITY_CONNECTOR_ADDRESS").unwrap_or("");
pub const SCHEMA_VERSION: u32 = 1;
```

Production build sets these via env at compile time:
```bash
ZECURITY_CONTROLLER_ADDRESS=controller.prod.example.com:9090 \
ZECURITY_CONNECTOR_ADDRESS=connector.prod.example.com:9092 \
cargo build --release
```

---

## Directory Structure

```
client/
  Cargo.toml
  build.rs              (tonic proto codegen — Phase 2)
  src/
    main.rs             (clap CLI entry point)
    appmeta.rs          (compile-time constants)
    config.rs           (read /etc/zecurity/client.conf — disk only)
    runtime.rs          (in-memory state struct — never serialized)
    error.rs
    ipc.rs              (Unix socket server/client for status queries — Phase 4)
    cmd/
      setup.rs
      status.rs
      logout.rs
      login.rs          (stub)
      invite.rs         (stub)
      connect.rs        (stub)
```

---

## `client/Cargo.toml`

```toml
[package]
name = "zecurity-client"
version = "0.1.0"
edition = "2021"

[[bin]]
name = "zecurity-client"
path = "src/main.rs"

[dependencies]
clap       = { version = "4", features = ["derive"] }
serde      = { version = "1", features = ["derive"] }
toml       = "0.8"
dirs       = "5"
tokio      = { version = "1", features = ["full"] }
anyhow     = "1"
thiserror  = "1"

# Phase 2 — add now so they compile clean:
tonic      = { version = "0.12", features = ["tls"] }
prost      = "0.13"
open       = "5"
rcgen      = "0.13"
sha2       = "0.10"
base64     = "0.22"
rand       = "0.8"
axum       = "0.7"
reqwest    = { version = "0.12", features = ["json", "rustls-tls"], default-features = false }
hostname   = "0.4"
urlencoding = "2"

# Phase 5 — add now so they compile clean:
tun         = { version = "0.6", features = ["async"] }
tokio-rustls = "0.26"
rustls       = "0.23"
rustls-pemfile = "2"

[build-dependencies]
tonic-build = "0.12"
```

---

## `client/src/error.rs`

```rust
use thiserror::Error;

#[derive(Debug, Error)]
pub enum ClientError {
    #[error("Not configured. Run `zecurity-client setup --workspace <name>` first.")]
    NotConfigured,
    #[error("Not connected. Run `zecurity-client connect` first.")]
    NotConnected,
    #[error("IO error: {0}")]
    Io(#[from] std::io::Error),
    #[error("Config parse error: {0}")]
    Toml(#[from] toml::de::Error),
    #[error("{0}")]
    Other(String),
}
```

---

## `client/src/config.rs`

**This is the only module that reads/writes disk.** Minimal data only.

```rust
use serde::{Deserialize, Serialize};
use std::path::PathBuf;
use anyhow::Result;

#[derive(Debug, Serialize, Deserialize, Default)]
pub struct ClientConf {
    pub workspace: String,
    /// Only set in dev. Empty = use compiled-in constant from appmeta.
    #[serde(default)]
    pub controller_address: String,
    /// Only set in dev. Empty = use compiled-in constant from appmeta.
    #[serde(default)]
    pub connector_address: String,
}

impl ClientConf {
    pub fn controller(&self) -> &str {
        if self.controller_address.is_empty() {
            crate::appmeta::DEFAULT_CONTROLLER_ADDRESS
        } else {
            &self.controller_address
        }
    }

    pub fn connector(&self) -> &str {
        if self.connector_address.is_empty() {
            crate::appmeta::DEFAULT_CONNECTOR_ADDRESS
        } else {
            &self.connector_address
        }
    }
}

pub fn conf_paths() -> Vec<PathBuf> {
    let mut paths = vec![PathBuf::from("/etc/zecurity/client.conf")];
    if let Some(d) = dirs::config_dir() {
        paths.push(d.join("zecurity-client").join("client.conf"));
    }
    paths
}

pub fn load() -> Result<ClientConf> {
    for path in conf_paths() {
        if path.exists() {
            let raw = std::fs::read_to_string(&path)?;
            return Ok(toml::from_str(&raw)?);
        }
    }
    Err(crate::error::ClientError::NotConfigured.into())
}

pub fn save(conf: &ClientConf) -> Result<PathBuf> {
    // Prefer /etc/zecurity/ if writable, otherwise fall back to user config dir.
    let system_path = PathBuf::from("/etc/zecurity/client.conf");
    let path = if system_path.parent().map(|p| p.exists()).unwrap_or(false) {
        system_path
    } else {
        let user = dirs::config_dir()
            .unwrap_or_else(|| PathBuf::from("."))
            .join("zecurity-client")
            .join("client.conf");
        std::fs::create_dir_all(user.parent().unwrap())?;
        user
    };
    std::fs::write(&path, toml::to_string_pretty(conf)?)?;
    Ok(path)
}
```

---

## `client/src/runtime.rs`

**In-memory only — never serialized or written to disk.**

```rust
use std::sync::Arc;
use tokio::sync::RwLock;

/// All runtime state. Lives only in process memory.
#[derive(Debug, Default, Clone)]
pub struct RuntimeState {
    pub schema_version: u32,
    pub workspace:  Option<WorkspaceInfo>,
    pub user:       Option<UserInfo>,
    pub device:     Option<DeviceInfo>,
    pub session:    Option<SessionInfo>,
    pub resources:  Vec<Resource>,
    pub last_sync_at: Option<i64>,  // Unix timestamp
}

#[derive(Debug, Clone)]
pub struct WorkspaceInfo {
    pub id:           String,
    pub name:         String,
    pub slug:         String,
    pub trust_domain: String,
}

#[derive(Debug, Clone)]
pub struct UserInfo {
    pub id:    String,
    pub email: String,
    pub role:  String,
}

#[derive(Debug, Clone)]
pub struct DeviceInfo {
    pub id:              String,
    pub spiffe_id:       String,
    pub certificate_pem: String,
    pub private_key_pem: String,  // plaintext in memory — never written to disk
    pub ca_cert_pem:     String,  // workspace CA + intermediate (concatenated)
    pub cert_expires_at: i64,     // Unix timestamp
    pub hostname:        String,
    pub os:              String,
}

#[derive(Debug, Clone)]
pub struct SessionInfo {
    pub access_token:  String,
    pub refresh_token: String,
    pub expires_at:    i64,  // Unix timestamp
}

#[derive(Debug, Clone, Default)]
pub struct Resource {
    pub id:       String,
    pub name:     String,
    pub host:     String,
    pub port:     u16,
    pub protocol: String,
}

/// Shared handle used across async tasks.
pub type SharedState = Arc<RwLock<RuntimeState>>;

pub fn new_shared() -> SharedState {
    Arc::new(RwLock::new(RuntimeState {
        schema_version: crate::appmeta::SCHEMA_VERSION,
        ..Default::default()
    }))
}
```

---

## `client/src/cmd/setup.rs`

```rust
use crate::config::{ClientConf, save};
use anyhow::Result;

pub async fn run(workspace: String, controller: Option<String>, connector: Option<String>) -> Result<()> {
    let conf = ClientConf {
        workspace,
        controller_address: controller.unwrap_or_default(),
        connector_address:  connector.unwrap_or_default(),
    };
    let path = save(&conf)?;
    println!("Config written to {}", path.display());
    println!("Run `zecurity-client connect` to authenticate and connect.");
    Ok(())
}
```

---

## `client/src/cmd/status.rs`

Status queries the running daemon via Unix socket (Phase 4 wires the socket). For Phase 1, print a not-connected message if the socket doesn't exist.

```rust
use anyhow::Result;

pub async fn run() -> Result<()> {
    // Phase 4 replaces this with a Unix socket query to the running daemon.
    // For now: indicate not connected.
    match crate::config::load() {
        Ok(conf) => {
            println!("Workspace:    {}", conf.workspace);
            println!("Controller:   {}", conf.controller());
            println!("Status:       Not connected (run `zecurity-client connect`)");
        }
        Err(_) => {
            println!("Status:       Not configured");
            println!("Run `zecurity-client setup --workspace <name>` first.");
        }
    }
    Ok(())
}
```

---

## `client/src/cmd/logout.rs`

```rust
use anyhow::Result;

pub async fn run() -> Result<()> {
    // Phase 4: send SHUTDOWN signal to running daemon via Unix socket.
    // For now: inform user that logout = stopping the connect process.
    println!("To log out, stop the `zecurity-client connect` process (SIGTERM or systemctl stop).");
    println!("All session data is in-memory only — stopping the process clears it.");
    Ok(())
}
```

---

## `client/src/main.rs`

```rust
mod appmeta;
mod config;
mod runtime;
mod error;
mod ipc;    // stub for Phase 4
mod cmd {
    pub mod setup;
    pub mod status;
    pub mod logout;
    pub mod login;    // stub
    pub mod invite;   // stub
    pub mod connect;  // stub
}

use clap::{Parser, Subcommand};

#[derive(Parser)]
#[command(name = "zecurity-client", about = "Zecurity ZTNA client")]
struct Cli {
    #[command(subcommand)]
    command: Commands,
}

#[derive(Subcommand)]
enum Commands {
    /// Write workspace name (and optional dev overrides) to /etc/zecurity/client.conf
    Setup {
        #[arg(long)] workspace:  String,
        /// Dev only: override compiled-in controller address
        #[arg(long)] controller: Option<String>,
        /// Dev only: override compiled-in connector address
        #[arg(long)] connector:  Option<String>,
    },
    /// Authenticate and start the tunnel (long-running)
    Connect,
    /// Show current connection status (queries running daemon)
    Status,
    /// Stop the running daemon and clear the in-memory session
    Logout,
    /// Invite a user to the workspace (admin only)
    Invite {
        #[arg(long)] email: String,
    },
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let cli = Cli::parse();
    match cli.command {
        Commands::Setup { workspace, controller, connector } =>
            cmd::setup::run(workspace, controller, connector).await,
        Commands::Connect =>
            cmd::connect::run().await,
        Commands::Status =>
            cmd::status::run().await,
        Commands::Logout =>
            cmd::logout::run().await,
        Commands::Invite { email } =>
            cmd::invite::run(email).await,
    }
}
```

---

## Stub Files

`client/src/ipc.rs`:
```rust
// Phase 4 implements Unix socket IPC for status queries.
```

`client/src/cmd/connect.rs`:
```rust
pub async fn run() -> anyhow::Result<()> {
    println!("Connect not yet implemented.");
    Ok(())
}
```

`client/src/cmd/login.rs`:
```rust
// Login is part of connect flow — not a standalone command.
// Phase 2 implements the login logic called from connect.rs.
```

`client/src/cmd/invite.rs`:
```rust
pub async fn run(_email: String) -> anyhow::Result<()> {
    println!("Invite not yet implemented.");
    Ok(())
}
```

---

## Build Check

```bash
cd client && cargo build
```

Test manually:
```bash
./target/debug/zecurity-client setup --workspace myworkspace --controller localhost:9090 --connector localhost:9092
cat /etc/zecurity/client.conf   # or ~/.config/zecurity-client/client.conf
./target/debug/zecurity-client status
./target/debug/zecurity-client logout
```

---

## Post-Phase Fixes

_None yet._
