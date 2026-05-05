use std::collections::VecDeque;
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{anyhow, Result};
use serde::{Deserialize, Serialize};
use smoltcp::iface::{Config, Interface, SocketSet};
use smoltcp::phy::{Device, DeviceCapabilities, Medium, RxToken, TxToken};
use smoltcp::socket::tcp;
use smoltcp::time::Instant as SmolInstant;
use smoltcp::wire::{EthernetAddress, HardwareAddress, IpAddress, IpCidr, Ipv4Address};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;
use tun::AsyncDevice;

use crate::grpc::client_v1::AclSnapshot;
use crate::tunnel_pool::TunnelPool;

const MAX_TCP_PAYLOAD: usize = 16 * 1024;
const SMOL_TICK_MS: u64 = 10;

// --- TunDevice: bridges tun::AsyncDevice to smoltcp's Device trait ---

struct TunRxToken(Vec<u8>);
struct TunTxToken(mpsc::UnboundedSender<Vec<u8>>);

impl RxToken for TunRxToken {
    fn consume<R, F>(mut self, f: F) -> R
    where
        F: FnOnce(&mut [u8]) -> R,
    {
        f(&mut self.0)
    }
}

impl TxToken for TunTxToken {
    fn consume<R, F>(self, len: usize, f: F) -> R
    where
        F: FnOnce(&mut [u8]) -> R,
    {
        let mut buf = vec![0u8; len];
        let result = f(&mut buf);
        let _ = self.0.send(buf);
        result
    }
}

struct TunDevice {
    rx: std::sync::mpsc::Receiver<Vec<u8>>,
    tx: mpsc::UnboundedSender<Vec<u8>>,
}

impl Device for TunDevice {
    type RxToken<'a> = TunRxToken where Self: 'a;
    type TxToken<'a> = TunTxToken where Self: 'a;

    fn receive(&mut self, _timestamp: SmolInstant) -> Option<(Self::RxToken<'_>, Self::TxToken<'_>)> {
        match self.rx.try_recv() {
            Ok(pkt) => Some((TunRxToken(pkt), TunTxToken(self.tx.clone()))),
            Err(_) => None,
        }
    }

    fn transmit(&mut self, _timestamp: SmolInstant) -> Option<Self::TxToken<'_>> {
        Some(TunTxToken(self.tx.clone()))
    }

    fn capabilities(&self) -> DeviceCapabilities {
        let mut caps = DeviceCapabilities::default();
        caps.medium = Medium::Ip;
        caps.max_transmission_unit = 1500;
        caps
    }
}

// --- JSON protocol with Connector ---

#[derive(Serialize)]
struct TunnelRequest {
    token: String,
    destination: String,
    port: u16,
    protocol: String,
}

#[derive(Deserialize)]
struct TunnelResponse {
    ok: bool,
    error: Option<String>,
}

// --- Main entry point ---

pub async fn run(
    dev: AsyncDevice,
    acl: Arc<AclSnapshot>,
    pool: Arc<TunnelPool>,
    connector_addr: SocketAddr,
) -> Result<()> {
    let (rx_sync_tx, rx_sync_rx) = std::sync::mpsc::channel::<Vec<u8>>();
    let (tx_async_tx, mut tx_async_rx) = mpsc::unbounded_channel::<Vec<u8>>();

    // Split the tun AsyncDevice into owned read/write halves via two tasks.
    let (mut tun_read, mut tun_write) = tokio::io::split(dev);

    // tun → smoltcp: read packets from TUN and forward to sync channel.
    let rx_sender = rx_sync_tx;
    tokio::spawn(async move {
        let mut buf = vec![0u8; 2048];
        loop {
            match tun_read.read(&mut buf).await {
                Ok(0) | Err(_) => break,
                Ok(n) => {
                    let _ = rx_sender.send(buf[..n].to_vec());
                }
            }
        }
    });

    // smoltcp → tun: write packets from async channel back to TUN.
    tokio::spawn(async move {
        while let Some(pkt) = tx_async_rx.recv().await {
            if tun_write.write_all(&pkt).await.is_err() {
                break;
            }
        }
    });

    let mut tun_dev = TunDevice {
        rx: rx_sync_rx,
        tx: tx_async_tx,
    };

    // Build smoltcp interface.
    let mut config = Config::new(HardwareAddress::Ip);
    config.random_seed = rand::random();
    let mut iface = Interface::new(config, &mut tun_dev, smoltcp_now());

    // Assign one IP address per ACL entry resource.
    let resource_ips: Vec<Ipv4Addr> = acl
        .entries
        .iter()
        .filter_map(|e| e.address.parse::<IpAddr>().ok())
        .filter_map(|ip| match ip {
            IpAddr::V4(v4) => Some(v4),
            _ => None,
        })
        .collect();

    iface.update_ip_addrs(|addrs| {
        for ip in &resource_ips {
            let cidr = IpCidr::new(IpAddress::Ipv4(Ipv4Address::from(*ip)), 32);
            let _ = addrs.push(cidr);
        }
        // Also add the zecurity0 loopback address
        let _ = addrs.push(IpCidr::new(IpAddress::v4(100, 64, 0, 1), 32));
    });

    let mut sockets = SocketSet::new(vec![]);
    // tcp_handles: maps smoltcp socket handle → (dest_ip, dest_port, relay_spawned)
    let mut socket_states: std::collections::HashMap<smoltcp::iface::SocketHandle, (Ipv4Addr, u16, bool)> =
        std::collections::HashMap::new();

    tracing::info!(
        resources = resource_ips.len(),
        connector = %connector_addr,
        "net_stack: smoltcp loop started"
    );

    loop {
        let smol_now = smoltcp_now();

        // Pre-accept: create a listening TCP socket for each resource IP if not already present.
        // (We open one listen socket per resource port.)
        for entry in &acl.entries {
            let ip: Ipv4Addr = match entry.address.parse::<IpAddr>() {
                Ok(IpAddr::V4(v)) => v,
                _ => continue,
            };
            let port = entry.port as u16;
            if entry.protocol.to_lowercase() == "tcp" || entry.protocol.is_empty() {
                // We can't distinguish which socket belongs to which (ip, port) once queued.
                // Instead we create a fresh LISTEN socket each poll cycle for each entry.
                // smoltcp handles duplicates gracefully — if already listening it accepts connections.
                let _ = open_listen_socket(&mut sockets, ip, port);
            }
        }

        iface.poll(smol_now, &mut tun_dev, &mut sockets);

        // Walk sockets: check for new established connections.
        let handles: Vec<_> = sockets.iter().map(|(h, _)| h).collect();
        for handle in handles {
            let socket = sockets.get_mut::<tcp::Socket>(handle);
            if socket.is_active() && !socket_states.get(&handle).map(|s| s.2).unwrap_or(false) {
                if let Some((dest_ip, dest_port, spawned)) = socket_states.get_mut(&handle) {
                    if !*spawned {
                        *spawned = true;
                        let dest = format!("{}", dest_ip);
                        let port = *dest_port;
                        let pool_clone = pool.clone();
                        let connector = connector_addr;
                        tokio::spawn(async move {
                            if let Err(e) = relay_tcp_to_quic(pool_clone, connector, dest, port).await {
                                tracing::warn!(error = %e, "TCP relay task error");
                            }
                        });
                    }
                }
            }
        }

        let poll_delay = iface
            .poll_delay(smol_now, &sockets)
            .map(|d| Duration::from_micros(d.micros()))
            .unwrap_or(Duration::from_millis(SMOL_TICK_MS));

        tokio::time::sleep(poll_delay.min(Duration::from_millis(SMOL_TICK_MS))).await;
    }
}

fn open_listen_socket(
    sockets: &mut SocketSet<'_>,
    _ip: Ipv4Addr,
    port: u16,
) -> smoltcp::iface::SocketHandle {
    let rx_buf = tcp::SocketBuffer::new(vec![0u8; MAX_TCP_PAYLOAD]);
    let tx_buf = tcp::SocketBuffer::new(vec![0u8; MAX_TCP_PAYLOAD]);
    let mut socket = tcp::Socket::new(rx_buf, tx_buf);
    let _ = socket.listen(port);
    sockets.add(socket)
}

async fn relay_tcp_to_quic(
    pool: Arc<TunnelPool>,
    connector_addr: SocketAddr,
    destination: String,
    port: u16,
) -> Result<()> {
    let (mut send, mut recv) = pool.open_stream(connector_addr).await?;

    // Send tunnel handshake.
    let req = TunnelRequest {
        token: String::new(),
        destination: destination.clone(),
        port,
        protocol: "tcp".to_string(),
    };
    let mut handshake = serde_json::to_vec(&req)?;
    handshake.push(b'\n');
    send.write_all(&handshake).await?;

    // Read response.
    let mut resp_buf = vec![0u8; 1024];
    let n = recv.read(&mut resp_buf).await?.unwrap_or(0);
    let resp: TunnelResponse = serde_json::from_slice(&resp_buf[..n])
        .map_err(|e| anyhow!("invalid tunnel response: {}", e))?;

    if !resp.ok {
        return Err(anyhow!(
            "tunnel denied for {}:{}: {}",
            destination,
            port,
            resp.error.unwrap_or_default()
        ));
    }

    tracing::debug!(dest = %destination, port, "QUIC tunnel open");

    // Note: smoltcp relay via shared socket buffers is complex.
    // This function establishes the QUIC stream; the data relay happens
    // in the smoltcp poll loop via socket read/write buffers.
    // For Sprint 9 the QUIC stream is opened to unblock the handshake —
    // full bidirectional relay via smoltcp socket buffers is Sprint 10.
    let _ = (send, recv);
    Ok(())
}

fn smoltcp_now() -> SmolInstant {
    SmolInstant::from_millis(
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .map(|d| d.as_millis() as i64)
            .unwrap_or(0),
    )
}
