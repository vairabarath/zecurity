---
type: phase
status: planned
sprint: 7
member: M4
phase: Phase5-TUN-Mode
depends_on:
  - M4-Phase4 (connect daemon + SharedState wired)
  - Sprint8-M3-B (Connector device_tunnel.rs `:9092` listener live)
tags:
  - rust
  - cli
  - tun
  - tunnel
  - networking
---

# M4 Phase 5 — TUN Mode (Layer 3 Tunnel to Connector)

---

## What You're Building

Replace `tunnel_placeholder()` in `connect.rs` with a real TUN tunnel. The device cert and private key are read directly from `RuntimeState` (in memory) — **never from disk**.

Flow:
1. Read `DeviceInfo` from `SharedState` → cert PEM + private key PEM (both in memory)
2. Build `rustls::ClientConfig` with mTLS using in-memory cert + key
3. Create TUN network interface (`tun0` / `utunN`)
4. Packet loop: intercept outbound IP packets → open TLS stream to Connector `:9092` → JSON handshake → forward payload bytes

---

## `client/src/tun_mode.rs`

```rust
use anyhow::{anyhow, Result};
use rustls::{ClientConfig, RootCertStore};
use rustls_pemfile::{certs, private_key};
use std::sync::Arc;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio_rustls::TlsConnector;

use crate::runtime::DeviceInfo;

pub struct TunTunnel {
    device:           DeviceInfo,   // cloned from SharedState — all in memory
    connector_address: String,
    access_token:     String,
}

impl TunTunnel {
    pub fn new(device: DeviceInfo, connector_address: String, access_token: String) -> Self {
        Self { device, connector_address, access_token }
    }

    pub async fn run(&self) -> Result<()> {
        // 1. Build TLS client config from in-memory cert + key
        let tls_config = self.build_tls_config()?;
        let connector = TlsConnector::from(Arc::new(tls_config));

        // 2. Create TUN interface
        let mut tun_config = tun::Configuration::default();
        tun_config
            .address("100.64.0.2")
            .netmask("255.255.255.0")
            .destination("100.64.0.1")
            .mtu(1500)
            .up();
        let dev = tun::create_as_async(&tun_config)?;
        println!("TUN interface up: 100.64.0.2/24");

        // 3. Packet forwarding loop
        let (mut tun_reader, mut tun_writer) = tokio::io::split(dev);
        let mut buf = vec![0u8; 65535];

        loop {
            let n = tun_reader.read(&mut buf).await?;
            if n == 0 { break; }

            let packet = &buf[..n];
            if let Some((dst_ip, dst_port)) = parse_ipv4_tcp_dest(packet) {
                let payload = extract_tcp_payload(packet);
                if payload.is_empty() { continue; }

                match self.open_tunnel_stream(&connector, dst_ip, dst_port).await {
                    Ok(mut stream) => {
                        stream.write_all(payload).await?;
                        let mut resp = vec![0u8; 65535];
                        let m = stream.read(&mut resp).await?;
                        if m > 0 {
                            tun_writer.write_all(&resp[..m]).await?;
                        }
                    }
                    Err(e) => eprintln!("Tunnel error to {}:{}: {}", dst_ip, dst_port, e),
                }
            }
        }
        Ok(())
    }

    async fn open_tunnel_stream(
        &self,
        connector: &TlsConnector,
        dst_ip:    std::net::Ipv4Addr,
        dst_port:  u16,
    ) -> Result<tokio_rustls::client::TlsStream<tokio::net::TcpStream>> {
        let host = self.connector_address.split(':').next().unwrap_or(&self.connector_address);
        let tcp = tokio::net::TcpStream::connect(&self.connector_address).await?;
        let server_name = rustls::pki_types::ServerName::try_from(host.to_string())?;
        let mut stream = connector.connect(server_name, tcp).await?;

        // JSON handshake
        let req = format!(
            "{{\"token\":\"{}\",\"destination\":\"{}\",\"port\":{},\"protocol\":\"tcp\"}}\n",
            self.access_token, dst_ip, dst_port
        );
        stream.write_all(req.as_bytes()).await?;

        // Read response line
        let mut line = String::new();
        let mut byte = [0u8; 1];
        loop {
            stream.read_exact(&mut byte).await?;
            if byte[0] == b'\n' { break; }
            line.push(byte[0] as char);
        }
        if line.contains("\"ok\":false") {
            return Err(anyhow!("Connector rejected: {}", line));
        }
        Ok(stream)
    }

    fn build_tls_config(&self) -> Result<ClientConfig> {
        // Parse cert from in-memory PEM — no disk reads
        let mut cert_reader = std::io::BufReader::new(self.device.certificate_pem.as_bytes());
        let cert_chain: Vec<_> = certs(&mut cert_reader).collect::<Result<_, _>>()?;

        // Parse private key from in-memory PEM — no disk reads
        let mut key_reader = std::io::BufReader::new(self.device.private_key_pem.as_bytes());
        let key = private_key(&mut key_reader)?
            .ok_or_else(|| anyhow!("no private key in runtime state"))?;

        // Trust workspace CA + intermediate from in-memory CA chain
        let mut roots = RootCertStore::empty();
        let mut ca_reader = std::io::BufReader::new(self.device.ca_cert_pem.as_bytes());
        for cert in certs(&mut ca_reader) {
            roots.add(cert?)?;
        }

        Ok(ClientConfig::builder()
            .with_root_certificates(roots)
            .with_client_auth_cert(cert_chain, key)?)
    }
}

fn parse_ipv4_tcp_dest(packet: &[u8]) -> Option<(std::net::Ipv4Addr, u16)> {
    if packet.len() < 20 { return None; }
    if (packet[0] >> 4) != 4 { return None; }  // IPv4
    if packet[9] != 6 { return None; }           // TCP
    let ihl = (packet[0] & 0xF) as usize * 4;
    if packet.len() < ihl + 4 { return None; }
    let dst_ip = std::net::Ipv4Addr::new(packet[16], packet[17], packet[18], packet[19]);
    let dst_port = u16::from_be_bytes([packet[ihl + 2], packet[ihl + 3]]);
    Some((dst_ip, dst_port))
}

fn extract_tcp_payload(packet: &[u8]) -> &[u8] {
    let ihl = (packet[0] & 0xF) as usize * 4;
    let data_offset = ((packet[ihl + 12] >> 4) & 0xF) as usize * 4;
    &packet[ihl + data_offset..]
}
```

---

## Wire into `connect.rs` (replace `tunnel_placeholder`)

```rust
// In connect.rs — replace tunnel_placeholder() call:
async fn run_tunnel(state: &SharedState, conf: &ClientConf) -> Result<()> {
    let st = state.read().await;
    let device  = st.device.clone().ok_or_else(|| anyhow::anyhow!("no device in state"))?;
    let token   = st.session.as_ref().map(|s| s.access_token.clone()).unwrap_or_default();
    drop(st);

    let tunnel = crate::tun_mode::TunTunnel::new(device, conf.connector().to_string(), token);
    tunnel.run().await
}

// In the connect loop, replace:
//   tunnel_placeholder() => {}
// with:
//   run_tunnel(&state, &conf).await => { ... }
```

Add `mod tun_mode;` to `main.rs`.

---

## Systemd Unit Update

Add `CAP_NET_ADMIN` for TUN device access (add to `zecurity-client.service`):

```ini
[Service]
AmbientCapabilities=CAP_NET_ADMIN
CapabilityBoundingSet=CAP_NET_ADMIN
```

---

## Platform Notes

| Platform | TUN device | Notes |
|----------|-----------|-------|
| Linux | `/dev/net/tun` | Requires `CAP_NET_ADMIN`. The `tun` crate handles this. |
| macOS | `utunN` (auto-numbered) | `tun` crate handles `utun` automatically. |
| Windows | WinTun | Out of scope — Linux/macOS only for this sprint. |

---

## Build Check

```bash
cd client && cargo build
```

Integration test (requires running Connector with Sprint 8 `device_tunnel.rs`):
```bash
sudo ./target/debug/zecurity-client connect
ip addr show tun0          # → 100.64.0.2/24
curl http://<resource-ip>  # → proxied through Connector
```

---

## Post-Phase Fixes

_None yet._
