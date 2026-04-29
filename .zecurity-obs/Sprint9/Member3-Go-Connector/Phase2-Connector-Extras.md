---
type: phase
status: pending
sprint: 9
member: M3
phase: Phase2-Connector-Extras
depends_on:
  - Phase1-RDE-Device-Tunnel
tags:
  - rust
  - connector
  - watchdog
  - crl
  - reliability
---

# M3 Phase 2 — Connector Reliability Extras

---

## What You're Building

Three small but production-critical modules:

1. **`watchdog.rs`** — systemd `sd_notify` integration (READY + WATCHDOG keepalives)
2. **`crl.rs`** — Certificate Revocation List manager (fetch, parse, background refresh, revocation check)
3. **`net_util.rs`** — already done in Phase 1. Wire it into `main.rs` here.

These are wired into `main.rs` and used by `device_tunnel.rs`.

---

## Files to Touch

### 1. `connector/src/watchdog.rs` (NEW)

```rust
use std::env;
use std::time::Duration;
use tokio::time::interval;

fn sd_notify(msg: &str) {
    let Ok(sock_path) = env::var("NOTIFY_SOCKET") else { return };
    let _ = std::os::unix::net::UnixDatagram::unbound()
        .and_then(|s| s.send_to(msg.as_bytes(), &sock_path));
}

/// Call once after all listeners are bound and ready to serve traffic.
pub fn notify_ready() {
    sd_notify("READY=1\n");
}

/// Spawns a background task that sends WATCHDOG=1 at half the WatchdogUSec interval.
/// Safe to call even when WATCHDOG_USEC is not set.
pub fn spawn_watchdog() {
    let Some(usec_str) = env::var("WATCHDOG_USEC").ok() else { return };
    let Ok(usec) = usec_str.parse::<u64>() else { return };
    let interval_ms = usec / 2 / 1000;
    tokio::spawn(async move {
        let mut tick = interval(Duration::from_millis(interval_ms));
        loop {
            tick.tick().await;
            sd_notify("WATCHDOG=1\n");
        }
    });
}
```

**Wire into `main.rs`:**

```rust
mod watchdog;

// After all listeners are bound (device_tunnel, quic_listener, agent_server):
watchdog::notify_ready();
watchdog::spawn_watchdog();
```

---

### 2. `connector/src/crl.rs` (NEW)

```rust
use std::collections::HashSet;
use std::sync::{Arc, RwLock};
use std::time::Duration;

use tokio::time::interval;
use x509_parser::prelude::*;

#[derive(Clone, Default)]
pub struct CrlManager {
    revoked: Arc<RwLock<HashSet<Vec<u8>>>>,
}

impl CrlManager {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn is_revoked(&self, serial: &[u8]) -> bool {
        self.revoked.read().unwrap().contains(serial)
    }

    pub async fn refresh(&self, url: &str) -> anyhow::Result<()> {
        let bytes = reqwest::get(url).await?.bytes().await?;
        let (_, crl) = CertificateRevocationList::from_der(&bytes)
            .map_err(|e| anyhow::anyhow!("CRL parse error: {:?}", e))?;
        let serials: HashSet<Vec<u8>> = crl
            .iter_revoked_certificates()
            .map(|e| e.user_certificate.to_bytes_be())
            .collect();
        *self.revoked.write().unwrap() = serials;
        Ok(())
    }

    pub fn spawn_refresh(self, url: String, interval_secs: u64) {
        tokio::spawn(async move {
            let mut tick = interval(Duration::from_secs(interval_secs));
            loop {
                tick.tick().await;
                if let Err(e) = self.refresh(&url).await {
                    tracing::warn!("CRL refresh failed: {e}");
                }
            }
        });
    }
}
```

**`Cargo.toml` additions:**

```toml
x509-parser = "0.16"
```

**Wire into `main.rs`:**

```rust
mod crl;

let crl_manager = crl::CrlManager::new();
if let Err(e) = crl_manager.refresh(&format!("{}/ca.crl", controller_base_url)).await {
    tracing::warn!("Initial CRL fetch failed: {e}");
}
crl_manager.clone().spawn_refresh(
    format!("{}/ca.crl", controller_base_url),
    300, // 5 minutes
);
// Pass crl_manager into device_tunnel::listen()
```

**Use in `device_tunnel.rs`:**

After reading the client cert from the TLS stream:

```rust
if crl_manager.is_revoked(cert.serial()) {
    send_response(&mut writer, false, "certificate revoked", None).await?;
    return Ok(());
}
```

---

### 3. `connector/src/main.rs` — Full wiring order

All modules must be initialized in this order:

```
1. Load config (controller URL, TLS certs)
2. net_util::lan_ip()                          → lan_ip
3. crl::CrlManager::new() + initial refresh
4. crl::spawn_refresh(...)
5. quic_listener::set_quic_advertise_addr(...)
6. device_tunnel::set_quic_advertise_addr(...)
7. Spawn agent_server (Shield-facing :9091)
8. Spawn device_tunnel::listen(:9092, ...)     → TLS
9. Spawn quic_listener::listen(:9092, ...)     → QUIC/UDP
10. watchdog::notify_ready()
11. watchdog::spawn_watchdog()
12. tokio::signal::ctrl_c().await              → graceful shutdown
```

---

## Cargo.toml Summary of New Dependencies

```toml
[dependencies]
tokio      = { version = "1", features = ["full"] }
quinn      = "0.11"
reqwest    = { version = "0.12", features = ["json"] }
anyhow     = "1"
tracing    = "0.1"
ipnet      = "2"
x509-parser = "0.16"
```

---

## Build Check

```bash
cd connector && cargo build
```

Warnings OK. Zero errors required before proceeding.
