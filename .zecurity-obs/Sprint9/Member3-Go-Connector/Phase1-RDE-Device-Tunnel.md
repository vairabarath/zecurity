---
type: phase
status: pending
sprint: 9
member: M3
phase: Phase1-RDE-Device-Tunnel
depends_on:
  - Sprint 8 Policy Engine complete
  - M2-D1-A (shield.proto TunnelOpen/Opened/Data/Close — Sprint 9 Day 1)
  - buf generate
tags:
  - rust
  - connector
  - rde
  - device-tunnel
  - quic
---

# M3 Phase 1 — RDE: Remote Device Extension (Device Tunnel)

---

## What You're Building

The RDE is the connector's device-facing access layer. End-user devices connect to the connector's TLS port (`:9092`) to access resources. The connector verifies access against the Sprint 8 local ACL snapshot and routes the connection either:

- **Protected path** (resource has nftables rules): relay through `AgentTunnelHub` → Shield Control stream → Shield proxies TCP locally via `zecurity0`
- **Direct path** (unprotected resource): `tokio::io::copy_bidirectional` directly to the resource IP:port

UDP is also supported via length-prefixed datagrams on the TLS stream.

QUIC runs on the same port number over UDP as a faster alternative — TLS responses advertise `quic_addr` so clients can upgrade.

---

## Files to Create / Modify

### 1. `connector/src/device_tunnel.rs` (NEW)

```rust
use anyhow::Result;
use serde::{Deserialize, Serialize};
use std::sync::{Arc, OnceLock};
use std::time::Duration;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::{TcpListener, TcpStream, UdpSocket};
use tokio_rustls::TlsAcceptor;
use tracing::{info, warn};

use crate::tls::cert_store::CertStore;
use crate::tls::server_cfg::build_device_tunnel_tls;
use crate::ControlMessage;
use crate::{agent_tunnel::AgentTunnelHub, policy::PolicyCache, AgentRegistry};

static HTTP_CLIENT: OnceLock<reqwest::Client> = OnceLock::new();

fn shared_http_client() -> &'static reqwest::Client {
    HTTP_CLIENT.get_or_init(|| {
        reqwest::Client::builder()
            .timeout(Duration::from_secs(5))
            .pool_max_idle_per_host(4)
            .build()
            .expect("failed to build HTTP client")
    })
}

/// QUIC address advertised to clients in TLS responses.
/// Set by quic_listener after bind succeeds.
static QUIC_ADVERTISE_ADDR: OnceLock<String> = OnceLock::new();

pub fn set_quic_advertise_addr(addr: String) {
    let _ = QUIC_ADVERTISE_ADDR.set(addr);
}

fn default_tcp() -> String { "tcp".to_string() }

/// JSON handshake sent by the device on connect.
#[derive(Deserialize)]
struct TunnelRequest {
    token:       String,
    destination: String,
    port:        u16,
    #[serde(default = "default_tcp")]
    protocol:    String,
}

/// JSON response sent back by the connector after verifying access.
#[derive(Serialize)]
struct TunnelResponse {
    ok:        bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    error:     Option<String>,
    /// QUIC address for client to upgrade to.
    /// Included even in rejection responses so clients can pre-warm QUIC.
    #[serde(skip_serializing_if = "Option::is_none")]
    quic_addr: Option<String>,
}

#[derive(Deserialize)]
struct CheckAccessResponse {
    allowed:     bool,
    resource_id: String,
}

/// Start the TLS/TCP device tunnel listener.
pub async fn listen(
    addr: &str,
    store: CertStore,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    agent_registry: Arc<AgentRegistry>,
    connector_id: String,
    control_tx: tokio::sync::mpsc::Sender<ControlMessage>,
) -> Result<()> {
    let tls_config = build_device_tunnel_tls(&store)?;
    let acceptor   = TlsAcceptor::from(Arc::new(tls_config));
    let listener   = TcpListener::bind(addr).await?;
    info!("device tunnel (TLS) listening on {}", addr);

    loop {
        match listener.accept().await {
            Ok((stream, peer)) => {
                let ctrl         = controller_http_url.clone();
                let acl          = acl.clone();
                let tunnel_hub   = tunnel_hub.clone();
                let agent_reg    = agent_registry.clone();
                let connector_id = connector_id.clone();
                let control_tx   = control_tx.clone();
                let acc          = acceptor.clone();
                tokio::spawn(async move {
                    match acc.accept(stream).await {
                        Ok(tls) => {
                            if let Err(e) = handle_stream(tls, &ctrl, acl, tunnel_hub, agent_reg, &connector_id, &control_tx).await {
                                warn!("device tunnel error from {}: {}", peer, e);
                            }
                        }
                        Err(e) => warn!("device tunnel TLS error from {}: {}", peer, e),
                    }
                });
            }
            Err(e) => warn!("device tunnel accept error: {}", e),
        }
    }
}

/// Read a newline-terminated line from an async stream (max 4 KB).
async fn read_line<S: AsyncRead + Unpin>(stream: &mut S) -> Result<String> {
    let mut buf  = Vec::with_capacity(256);
    let mut byte = [0u8; 1];
    loop {
        let n = stream.read(&mut byte).await?;
        if n == 0 { anyhow::bail!("EOF before handshake newline"); }
        if byte[0] == b'\n' { break; }
        buf.push(byte[0]);
        if buf.len() > 4096 { anyhow::bail!("handshake line too long"); }
    }
    Ok(String::from_utf8(buf)?)
}

async fn send_response<S: AsyncWrite + Unpin>(stream: &mut S, ok: bool, error: Option<&str>) -> Result<()> {
    let resp = TunnelResponse {
        ok,
        error:     error.map(|s| s.to_string()),
        quic_addr: QUIC_ADVERTISE_ADDR.get().cloned(),
    };
    let mut line = serde_json::to_string(&resp)?;
    line.push('\n');
    stream.write_all(line.as_bytes()).await?;
    stream.flush().await?;
    Ok(())
}

/// Core connection handler — shared between TLS/TCP and QUIC paths.
pub async fn handle_stream<S: AsyncRead + AsyncWrite + Unpin + Send + 'static>(
    mut stream: S,
    controller_http_url: &str,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    agent_registry: Arc<AgentRegistry>,
    connector_id: &str,
    control_tx: &tokio::sync::mpsc::Sender<ControlMessage>,
) -> Result<()> {
    // Step 1: Read JSON handshake line from device.
    let line = read_line(&mut stream).await?;
    let req: TunnelRequest = serde_json::from_str(line.trim())
        .map_err(|e| anyhow::anyhow!("bad handshake: {}", e))?;

    // Step 2: Resolve and authorize from the local Sprint 8 ACL snapshot.
    // No per-request controller check is allowed in the tunnel hot path.
    let Some(decision) = acl.authorize(&req.destination, req.port, &req.protocol, client_spiffe_id) else {
        send_response(&mut stream, false, Some("access denied")).await?;
        return Ok(());
    };
    let (resource_id, protected) = (decision.resource_id, decision.protected);

    let resource = acl.resource_by_id(&resource_id);

    // Step 3: Route the connection.
    if protected {
        // Protected path: relay through shield via AgentTunnelHub.
        let agent_id = resource.as_ref()
            .and_then(|res| {
                if res.agent_ids.is_empty() { None }
                else { tunnel_hub.select_agent_id(&res.agent_ids) }
            })
            .or_else(|| resolve_resource_owner(&req.destination, &agent_registry))
            .ok_or_else(|| anyhow::anyhow!("no connected shield for resource {} ({})", req.destination, resource_id))?;

        let relay = match crate::agent_tunnel::open_relay_session(
            tunnel_hub, &agent_id, &req.destination, req.port, &req.protocol,
        ).await {
            Ok(s) => s,
            Err(e) => {
                let msg = format!("shield relay failed for {}:{} via {}: {}", req.destination, req.port, agent_id, e);
                let _ = send_response(&mut stream, false, Some(&msg)).await;
                return Err(anyhow::anyhow!("{}", msg));
            }
        };

        send_response(&mut stream, true, None).await?;
        emit_access_log(control_tx, connector_id,
            &format!("rde: destination={} port={} protocol={} path=shield_relay agent={}", req.destination, req.port, req.protocol, agent_id)).await;
        info!("rde: routing protected {}:{} via shield {}", req.destination, req.port, agent_id);
        return relay.relay_stream(stream).await;
    }

    // Direct path: connector connects to resource directly.
    send_response(&mut stream, true, None).await?;
    let dest = format!("{}:{}", req.destination, req.port);

    if req.protocol == "udp" {
        return relay_udp(&mut stream, &dest).await;
    }

    let mut resource_conn = match TcpStream::connect(&dest).await {
        Ok(s) => s,
        Err(e) => return Err(anyhow::anyhow!("connect to {} failed: {}", dest, e)),
    };

    emit_access_log(control_tx, connector_id,
        &format!("rde: destination={} port={} protocol={} path=direct", req.destination, req.port, req.protocol)).await;

    match tokio::io::copy_bidirectional(&mut stream, &mut resource_conn).await {
        Ok((sent, recv)) => info!("rde: closed {} sent={} recv={}", dest, sent, recv),
        Err(e) => warn!("rde: I/O error {}: {}", dest, e),
    }
    Ok(())
}

/// UDP relay using length-prefixed (4-byte big-endian) datagrams over TLS stream.
async fn relay_udp<S: AsyncRead + AsyncWrite + Unpin>(stream: &mut S, dest: &str) -> Result<()> {
    let udp = UdpSocket::bind("0.0.0.0:0").await?;
    udp.connect(dest).await?;
    let mut udp_buf = [0u8; 65535];
    let mut len_buf = [0u8; 4];
    loop {
        tokio::select! {
            result = stream.read_exact(&mut len_buf) => {
                if result.is_err() { break; }
                let len = u32::from_be_bytes(len_buf) as usize;
                if len > 65535 { break; }
                let mut buf = vec![0u8; len];
                if stream.read_exact(&mut buf).await.is_err() { break; }
                if udp.send(&buf).await.is_err() { break; }
            }
            result = udp.recv(&mut udp_buf) => {
                let n = match result { Ok(n) => n, Err(_) => break };
                let len = (n as u32).to_be_bytes();
                if stream.write_all(&len).await.is_err() { break; }
                if stream.write_all(&udp_buf[..n]).await.is_err() { break; }
                if stream.flush().await.is_err() { break; }
            }
        }
    }
    info!("rde: UDP relay closed {}", dest);
    Ok(())
}

/// Fallback: find the shield that owns a destination IP when resource has no explicit agent_ids.
/// Returns None if zero or more than one agent matches (avoids ambiguity).
fn resolve_resource_owner(destination: &str, registry: &AgentRegistry) -> Option<String> {
    let dest = destination.trim();
    if dest.is_empty() { return None; }
    let mut matches = registry.snapshot()
        .into_iter()
        .filter(|a| a.ip.trim() == dest)
        .map(|a| a.agent_id);
    let owner = matches.next()?;
    if matches.next().is_some() { return None; }  // ambiguous — more than one
    Some(owner)
}

async fn emit_access_log(
    control_tx: &tokio::sync::mpsc::Sender<ControlMessage>,
    connector_id: &str,
    message: &str,
) {
    let payload = serde_json::json!({ "connector_id": connector_id, "message": message });
    let _ = control_tx.send(ControlMessage {
        r#type:       "connector_log".to_string(),
        connector_id: connector_id.to_string(),
        payload:      serde_json::to_vec(&payload).unwrap_or_default(),
        ..Default::default()
    }).await;
}
```

**`connector/Cargo.toml`** — add if not present:
```toml
tokio-rustls = "0.26"
reqwest      = { version = "0.12", features = ["json"] }
```

---

### 2. `connector/src/quic_listener.rs` (NEW)

QUIC/UDP on the same port number as TLS/TCP. Each QUIC bidirectional stream is handled by the same `handle_stream` function.

```rust
use anyhow::Result;
use std::sync::Arc;
use tracing::{info, warn};

use crate::agent_tunnel::AgentTunnelHub;
use crate::device_tunnel;
use crate::policy::PolicyCache;
use crate::tls::cert_store::CertStore;
use crate::tls::server_cfg::build_device_tunnel_tls;
use crate::AgentRegistry;
use crate::ControlMessage;

pub async fn listen(
    addr: &str,
    advertise_addr: &str,
    controller_http_url: String,
    store: CertStore,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    agent_registry: Arc<AgentRegistry>,
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
                let ctrl         = controller_http_url.clone();
                let acl          = acl.clone();
                let tunnel_hub   = tunnel_hub.clone();
                let agent_reg    = agent_registry.clone();
                let connector_id = connector_id.clone();
                let control_tx   = control_tx.clone();
                tokio::spawn(async move {
                    match incoming.await {
                        Ok(conn) => loop {
                            match conn.accept_bi().await {
                                Ok((send, recv)) => {
                                    let combined = tokio::io::join(recv, send);
                                    let ctrl         = ctrl.clone();
                                    let acl          = acl.clone();
                                    let tunnel_hub   = tunnel_hub.clone();
                                    let agent_reg    = agent_reg.clone();
                                    let connector_id = connector_id.clone();
                                    let control_tx   = control_tx.clone();
                                    tokio::spawn(async move {
                                        if let Err(e) = device_tunnel::handle_stream(
                                            combined, &ctrl, acl, tunnel_hub, agent_reg, &connector_id, &control_tx,
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

### 3. `connector/src/agent_tunnel.rs` (MODIFY)

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

### 4. `connector/src/net_util.rs` (NEW)

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

Start both device tunnel listeners after all agent-facing listeners:

```rust
// TLS/TCP on :9092
tokio::spawn(device_tunnel::listen(
    "0.0.0.0:9092",
    controller_http_url.clone(),
    cert_store.clone(),
    acl.clone(),
    tunnel_hub.clone(),
    agent_registry.clone(),
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
    connector_id.clone(),
    control_tx.clone(),
));
```

Add `mod device_tunnel;`, `mod quic_listener;`, `mod net_util;`.

---

## Build Check

```bash
cd connector && cargo build
```
