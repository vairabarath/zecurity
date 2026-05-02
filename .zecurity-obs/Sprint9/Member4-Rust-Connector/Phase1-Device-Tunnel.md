---
type: phase
status: done
sprint: 9
member: M4
phase: Phase1-Device-Tunnel
depends_on:
  - Sprint 8 Policy Engine complete (PolicyCache in connector)
  - M2-D1-A (shield.proto buf generate)
  - M3-B2 (AgentTunnelHub API defined in agent_tunnel.rs)
tags:
  - rust
  - connector
  - rde
  - device-tunnel
  - acl
---

# M4 Phase 1 — Device Tunnel (ACL Enforcement + Routing)

---

## What You're Building

The core of the RDE: the connection handler that every device tunnel passes through. You own `device_tunnel.rs` — it is the ACL gatekeeper and the router. Every device connection hits this code, gets checked against the local Sprint 8 snapshot, and gets routed either direct or via Shield.

M3 owns the listeners (`quic_listener.rs`) and the hub infrastructure (`agent_tunnel.rs`). You consume `AgentTunnelHub` from M3 — do not implement it yourself.

---

## Dependency Note

**Wait for M3 to define `AgentTunnelHub` in `agent_tunnel.rs` before writing the protected path.** You can scaffold the file, write the handshake, and implement the direct path without M3. The protected path (relay session) needs `AgentTunnelHub::open_relay_session()` to be available.

Coordinate with M3 to agree on this signature early:
```rust
// In connector/src/agent_tunnel.rs (M3 defines this)
pub async fn open_relay_session(
    hub: AgentTunnelHub,
    agent_id: &str,
    destination: &str,
    port: u16,
    protocol: &str,
) -> Result<RelaySession>;
```

---

## File to Create

### `connector/src/device_tunnel.rs` (NEW)

Full spec is the same as originally written — pulled here for M4 reference.

#### Structs

```rust
/// JSON handshake sent by the device on connect.
#[derive(Deserialize)]
struct TunnelRequest {
    token:       String,
    destination: String,
    port:        u16,
    #[serde(default = "default_tcp")]
    protocol:    String,
}

/// JSON response sent back after verifying access.
/// quic_addr is ALWAYS included, even on rejection — lets client pre-warm QUIC.
#[derive(Serialize)]
struct TunnelResponse {
    ok:        bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    error:     Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    quic_addr: Option<String>,
}
```

#### Core handler logic

```rust
pub async fn handle_stream<S: AsyncRead + AsyncWrite + Unpin + Send + 'static>(
    mut stream: S,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    agent_registry: Arc<AgentRegistry>,
    crl_manager: Arc<CrlManager>,  // from M3's crl.rs
    connector_id: &str,
    control_tx: &tokio::sync::mpsc::Sender<ControlMessage>,
) -> Result<()>
```

Steps inside `handle_stream`:
1. Read JSON handshake line (max 4 KB, newline-terminated)
2. Parse `TunnelRequest`
3. Extract client SPIFFE ID from TLS peer certificate
4. **CRL check** — `crl_manager.is_revoked(cert_serial)` → deny with "certificate revoked"
5. **ACL check** — `acl.authorize(destination, port, protocol, spiffe_id)` → deny with "access denied" if None
6. **Route decision** — `decision.protected` flag:
   - `true` → protected path via `AgentTunnelHub`
   - `false` → direct path

#### Direct path

```rust
// TCP
let mut resource_conn = TcpStream::connect(format!("{}:{}", destination, port)).await?;
send_response(&mut stream, true, None).await?;
emit_access_log(..., "path=direct").await;
tokio::io::copy_bidirectional(&mut stream, &mut resource_conn).await?;

// UDP
send_response(&mut stream, true, None).await?;
relay_udp(&mut stream, &dest).await?;
```

#### Protected path

```rust
let relay = agent_tunnel::open_relay_session(
    tunnel_hub, &agent_id, destination, port, protocol
).await?;
send_response(&mut stream, true, None).await?;
emit_access_log(..., "path=shield_relay").await;
relay.relay_stream(stream).await?;
```

#### UDP relay (direct path only — protected UDP goes through Shield's handle_tunnel_open_udp)

```rust
async fn relay_udp<S: AsyncRead + AsyncWrite + Unpin>(stream: &mut S, dest: &str) -> Result<()> {
    let udp = UdpSocket::bind("0.0.0.0:0").await?;
    udp.connect(dest).await?;
    let mut udp_buf = [0u8; 65535];
    let mut len_buf = [0u8; 4];
    loop {
        tokio::select! {
            // Client → resource: read 4-byte length prefix then payload
            result = stream.read_exact(&mut len_buf) => {
                if result.is_err() { break; }
                let len = u32::from_be_bytes(len_buf) as usize;
                if len > 65535 { break; }
                let mut buf = vec![0u8; len];
                if stream.read_exact(&mut buf).await.is_err() { break; }
                if udp.send(&buf).await.is_err() { break; }
            }
            // Resource → client: recv datagram, send with 4-byte prefix
            result = udp.recv(&mut udp_buf) => {
                let n = match result { Ok(n) => n, Err(_) => break };
                let prefix = (n as u32).to_be_bytes();
                if stream.write_all(&prefix).await.is_err() { break; }
                if stream.write_all(&udp_buf[..n]).await.is_err() { break; }
                if stream.flush().await.is_err() { break; }
            }
        }
    }
    Ok(())
}
```

> Note: the 4-byte length prefix is only on the **Client ↔ Connector raw TLS stream** because there's no message framing there. The **Shield ↔ Connector** UDP path uses protobuf `TunnelData` messages — those are already framed, no prefix needed.

#### Access log

```rust
async fn emit_access_log(
    control_tx: &Sender<ControlMessage>,
    connector_id: &str,
    message: &str,
) {
    let payload = serde_json::json!({ "connector_id": connector_id, "message": message });
    let _ = control_tx.send(ControlMessage { /* connector_log type */ }).await;
}
```

#### TLS listener entry point

```rust
pub async fn listen(
    addr: &str,
    store: CertStore,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    agent_registry: Arc<AgentRegistry>,
    crl_manager: Arc<CrlManager>,
    connector_id: String,
    control_tx: tokio::sync::mpsc::Sender<ControlMessage>,
) -> Result<()>
```

`listen()` accepts TLS connections and spawns `handle_stream()` per connection. QUIC connections are accepted by M3's `quic_listener.rs` which calls the same `handle_stream()`.

---

## QUIC Advertise Address

M3's `quic_listener.rs` calls `device_tunnel::set_quic_advertise_addr(addr)` after binding. `handle_stream` reads this static and includes it in every `TunnelResponse`.

```rust
static QUIC_ADVERTISE_ADDR: OnceLock<String> = OnceLock::new();

pub fn set_quic_advertise_addr(addr: String) {
    let _ = QUIC_ADVERTISE_ADDR.set(addr);
}
```

---

## Build Check

```bash
cd connector && cargo build
```

---

## Post-Phase Fixes

### Fix: `handle_stream` required peer_addr parameter
**File:** `connector/src/device_tunnel.rs`
**Issue:** The `handle_stream` signature in the spec didn't include `peer_addr: SocketAddr`, but `quic_listener.rs` called it with a peer address. Build failed with "this function takes 8 arguments but 7 arguments were supplied".
**Fix:** Added `peer_addr: SocketAddr` parameter to `handle_stream` function signature.

### Fix: TlsAcceptor cannot be moved in loop
**File:** `connector/src/device_tunnel.rs`
**Issue:** The `TlsAcceptor` was moved into each iteration of the accept loop, but it doesn't implement `Copy`. Build failed with "use of moved value: acceptor".
**Fix:** Clone the acceptor before each spawn: `let acceptor_clone = acceptor.clone();` in the loop.

### Fix: QUIC listener passed wrong peer_addr type
**File:** `connector/src/quic_listener.rs`
**Issue:** QUIC listener called `device_tunnel::handle_stream` but passed string slice `&conn_id` where a `SocketAddr` was expected.
**Fix:** Created a dummy SocketAddr: `let peer_addr = "0.0.0.0:0".parse().unwrap();` before calling handle_stream.
