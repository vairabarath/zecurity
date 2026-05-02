---
type: phase
status: pending
sprint: 9
member: M3
phase: Phase1-Connector-Infrastructure
depends_on:
  - M2-D1-A (shield.proto TunnelOpen/Opened/Data/Close — Sprint 9 Day 1)
  - buf generate
tags:
  - rust
  - connector
  - quic
  - infrastructure
---

# M3 Phase 1 — Connector Infrastructure

---

## What You're Building

The infrastructure layer of the RDE Connector: QUIC listener, AgentTunnelHub API, and the `net_util` LAN IP helper. M4 builds `device_tunnel.rs` on top of the `AgentTunnelHub` you define here — so the most important deliverable is the **public API of `agent_tunnel.rs`**. Define the struct and method signatures first so M4 can work in parallel.

**M4 depends on your `AgentTunnelHub` API.** Define `open_relay_session()` signature and the hub struct early, even before the full implementation is done.

> `device_tunnel.rs` is owned by M4. See [[Sprint9/Member4-Rust-Connector/Phase1-Device-Tunnel]].

---

## Files to Create / Modify

### 1. `connector/src/device_tunnel.rs` — OWNED BY M4

> Do not create this file. See [[Sprint9/Member4-Rust-Connector/Phase1-Device-Tunnel]].
> Your `agent_tunnel.rs` must export `AgentTunnelHub` and `open_relay_session()` for M4 to import.

---

### 2. `connector/src/quic_listener.rs` (NEW)

QUIC/UDP on the same port number as TLS/TCP. Each QUIC bidirectional stream is handled by the same `handle_stream` function.

> **Interface contract with M4:** `device_tunnel::handle_stream` signature is:
> `handle_stream(stream, acl, tunnel_hub, agent_registry, crl_manager, connector_id, control_tx)`
> No `controller_http_url` — CRL fetching is handled by `CrlManager` (M3 Phase 2).

```rust
use anyhow::Result;
use std::sync::Arc;
use tracing::{info, warn};

use crate::agent_tunnel::AgentTunnelHub;
use crate::crl::CrlManager;
use crate::device_tunnel;
use crate::policy::PolicyCache;
use crate::tls::cert_store::CertStore;
use crate::tls::server_cfg::build_device_tunnel_tls;
use crate::AgentRegistry;
use crate::ControlMessage;

pub async fn listen(
    addr: &str,
    advertise_addr: &str,
    store: CertStore,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    agent_registry: Arc<AgentRegistry>,
    crl_manager: CrlManager,
    connector_id: String,
    control_tx: tokio::sync::mpsc::Sender<ControlMessage>,
) -> Result<()> {
    let mut tls_config = build_device_tunnel_tls(&store)?;
    tls_config.alpn_protocols = vec![b"ztna-tunnel-v1".to_vec()];

    let quic_server_cfg = quinn::crypto::rustls::QuicServerConfig::try_from(tls_config)
        .map_err(|e| anyhow::anyhow!("QUIC server config: {}", e))?;
    let server_config = quinn::ServerConfig::with_crypto(Arc::new(quic_server_cfg));

    let socket_addr: std::net::SocketAddr = addr.parse()
        .map_err(|e| anyhow::anyhow!("bad QUIC addr '{}': {}", addr, e))?;
    let endpoint = quinn::Endpoint::server(server_config, socket_addr)?;

    device_tunnel::set_quic_advertise_addr(advertise_addr.to_string());
    info!("device tunnel (QUIC) listening on {} advertise={}", addr, advertise_addr);

    loop {
        match endpoint.accept().await {
            Some(incoming) => {
                let acl          = acl.clone();
                let tunnel_hub   = tunnel_hub.clone();
                let agent_reg    = agent_registry.clone();
                let crl          = crl_manager.clone();
                let connector_id = connector_id.clone();
                let control_tx   = control_tx.clone();
                tokio::spawn(async move {
                    match incoming.await {
                        Ok(conn) => loop {
                            match conn.accept_bi().await {
                                Ok((send, recv)) => {
                                    let combined = tokio::io::join(recv, send);
                                    let acl          = acl.clone();
                                    let tunnel_hub   = tunnel_hub.clone();
                                    let agent_reg    = agent_reg.clone();
                                    let crl          = crl.clone();
                                    let connector_id = connector_id.clone();
                                    let control_tx   = control_tx.clone();
                                    tokio::spawn(async move {
                                        if let Err(e) = device_tunnel::handle_stream(
                                            combined, acl, tunnel_hub, agent_reg, crl, &connector_id, &control_tx,
                                        ).await {
                                            warn!("QUIC stream error: {}", e);
                                        }
                                    });
                                }
                                Err(e) => { warn!("QUIC accept_bi: {}", e); break; }
                            }
                        },
                        Err(e) => warn!("QUIC connection error: {}", e),
                    }
                });
            }
            None => break,
        }
    }
    Ok(())
}
```

**`connector/Cargo.toml`** — add:
```toml
quinn = "0.11"
```

---

### 3. `connector/src/agent_tunnel.rs` (MODIFY) — Define AgentTunnelHub API first

Dispatch tunnel messages coming from the Shield via the Control stream into the hub:

```rust
// In agent_server.rs, inside the Shield stream receive loop:
ShieldControlMessage::TunnelOpened(payload) => {
    hub.dispatch_opened(&payload.connection_id, payload.ok, payload.error.clone());
}
ShieldControlMessage::TunnelData(payload) => {
    hub.dispatch_data(&payload.connection_id, payload.data.clone());
}
ShieldControlMessage::TunnelClose(payload) => {
    hub.dispatch_close(&payload.connection_id, payload.error.clone());
}
```

When connector wants to open a tunnel to the shield:

```rust
shield_control_tx.send(ShieldControlMessage {
    body: Some(Body::TunnelOpen(proto::TunnelOpen {
        connection_id: connection_id.clone(),
        destination: destination.to_string(),
        port: port as u32,
        protocol: protocol.to_string(),
    })),
}).await?;
```

---

### 4. `connector/src/net_util.rs` (NEW) — small, do this first

```rust
use std::net::{IpAddr, UdpSocket};

/// Returns the connector's outbound LAN IP by asking the OS which source IP
/// it would use to reach 8.8.8.8. No packet is sent.
pub fn lan_ip() -> anyhow::Result<IpAddr> {
    let socket = UdpSocket::bind("0.0.0.0:0")?;
    socket.connect("8.8.8.8:53")?;
    let addr = socket.local_addr()?;
    Ok(addr.ip())
}
```

---

### 5. `connector/src/main.rs` (MODIFY)

Start both device tunnel listeners after all agent-facing listeners. `crl_manager` is created in Phase 2 — wire it in here:

```rust
// TLS/TCP on :9092  (M4 owns device_tunnel::listen — see Phase2-Connector-Extras for crl_manager init)
tokio::spawn(device_tunnel::listen(
    "0.0.0.0:9092",
    cert_store.clone(),
    acl.clone(),
    tunnel_hub.clone(),
    agent_registry.clone(),
    crl_manager.clone(),
    connector_id.clone(),
    control_tx.clone(),
));

// QUIC/UDP on :9092 (same port, different transport)
tokio::spawn(quic_listener::listen(
    "0.0.0.0:9092",
    &format!("{}:9092", lan_ip),
    cert_store.clone(),
    acl.clone(),
    tunnel_hub.clone(),
    agent_registry.clone(),
    crl_manager.clone(),
    connector_id.clone(),
    control_tx.clone(),
));
```

Add `mod device_tunnel;`, `mod quic_listener;`, `mod net_util;`.

> Full wiring order (including `crl_manager` init) is in [[Sprint9/Member3-Go-Connector/Phase2-Connector-Extras]].

---

## Build Check

```bash
cd connector && cargo build
```
