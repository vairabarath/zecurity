use std::collections::{HashMap, VecDeque};
use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, Result};
use serde::{Deserialize, Serialize};
use smoltcp::iface::{Config, Interface, SocketHandle, SocketSet};
use smoltcp::phy::{Device, DeviceCapabilities, Medium, RxToken, TxToken};
use smoltcp::socket::tcp;
use smoltcp::time::Instant as SmolInstant;
use smoltcp::wire::{HardwareAddress, IpAddress, IpCidr, Ipv4Address};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;
use tun::AsyncDevice;

use crate::grpc::client_v1::AclSnapshot;
use crate::tunnel_pool::TunnelPool;

const MAX_TCP_PAYLOAD: usize = 64 * 1024;
const SMOL_TICK_MS: u64 = 5;

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

// --- Per-connection relay state (lives in the smoltcp loop) ---

struct ActiveRelay {
    dest_ip: Ipv4Addr,
    dest_port: u16,
    // smoltcp loop → relay task: client payload going to the resource
    tcp_to_quic_tx: mpsc::UnboundedSender<Vec<u8>>,
    // relay task → smoltcp loop: resource payload coming back to the client
    quic_to_tcp_rx: mpsc::UnboundedReceiver<Vec<u8>>,
    // overflow buffer when the smoltcp send window is temporarily full
    write_buf: VecDeque<u8>,
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

    let (mut tun_read, mut tun_write) = tokio::io::split(dev);

    // TUN → smoltcp: read raw IP packets from the kernel and forward to the
    // sync channel that TunDevice::receive() drains each poll cycle.
    tokio::spawn(async move {
        let mut buf = vec![0u8; 4096];
        loop {
            match tun_read.read(&mut buf).await {
                Ok(0) | Err(_) => break,
                Ok(n) => { let _ = rx_sync_tx.send(buf[..n].to_vec()); }
            }
        }
    });

    // smoltcp → TUN: write IP packets that smoltcp emits back to the kernel.
    tokio::spawn(async move {
        while let Some(pkt) = tx_async_rx.recv().await {
            if tun_write.write_all(&pkt).await.is_err() { break; }
        }
    });

    let mut tun_dev = TunDevice { rx: rx_sync_rx, tx: tx_async_tx };

    let mut config = Config::new(HardwareAddress::Ip);
    config.random_seed = rand::random();
    let mut iface = Interface::new(config, &mut tun_dev, smoltcp_now());

    // Collect TCP resources from the ACL snapshot.
    let resource_entries: Vec<(Ipv4Addr, u16)> = acl
        .entries
        .iter()
        .filter(|e| e.protocol.to_lowercase() == "tcp" || e.protocol.is_empty())
        .filter_map(|e| {
            let ip = e.address.parse::<IpAddr>().ok()?;
            match ip { IpAddr::V4(v4) => Some((v4, e.port as u16)), _ => None }
        })
        .collect();

    // Assign one /32 address per resource so smoltcp accepts inbound packets.
    iface.update_ip_addrs(|addrs| {
        for (ip, _) in &resource_entries {
            let cidr = IpCidr::new(IpAddress::Ipv4(Ipv4Address::from(*ip)), 32);
            let _ = addrs.push(cidr);
        }
        let _ = addrs.push(IpCidr::new(IpAddress::v4(100, 64, 0, 1), 32));
    });

    let mut sockets = SocketSet::new(vec![]);

    // Create ONE listening TCP socket per resource BEFORE the loop.
    // Re-created each time a connection is accepted (smoltcp promotes the
    // listening socket to established, so a fresh one is needed immediately).
    let mut listen_handles: HashMap<(Ipv4Addr, u16), SocketHandle> = HashMap::new();
    for (ip, port) in &resource_entries {
        let handle = new_listen_socket(&mut sockets, *port);
        listen_handles.insert((*ip, *port), handle);
    }

    let mut active_relays: HashMap<SocketHandle, ActiveRelay> = HashMap::new();

    tracing::info!(
        resources = resource_entries.len(),
        connector = %connector_addr,
        "net_stack: smoltcp loop started"
    );

    loop {
        let smol_now = smoltcp_now();
        iface.poll(smol_now, &mut tun_dev, &mut sockets);

        // --- Promote listening sockets that have accepted a connection ---
        //
        // When smoltcp completes the TCP handshake, the socket transitions
        // Listen → Established (is_active).  We immediately replace it with a
        // new listener so further connections to the same resource still work.
        let listen_snapshot: Vec<_> = listen_handles.iter().map(|(k, v)| (*k, *v)).collect();
        for ((ip, port), handle) in listen_snapshot {
            let established = sockets.get_mut::<tcp::Socket>(handle).is_active();
            if established {
                // Fresh listener for the next connection.
                let new_handle = new_listen_socket(&mut sockets, port);
                listen_handles.insert((ip, port), new_handle);

                // Channel pair that bridges the synchronous smoltcp poll loop
                // and the async QUIC relay task.
                let (tcp_to_quic_tx, tcp_to_quic_rx) = mpsc::unbounded_channel::<Vec<u8>>();
                let (quic_to_tcp_tx, quic_to_tcp_rx) = mpsc::unbounded_channel::<Vec<u8>>();

                active_relays.insert(handle, ActiveRelay {
                    dest_ip: ip,
                    dest_port: port,
                    tcp_to_quic_tx,
                    quic_to_tcp_rx,
                    write_buf: VecDeque::new(),
                });

                let pool_c = pool.clone();
                let dest = ip.to_string();
                tracing::info!(dest = %dest, port, "new TCP connection — spawning QUIC relay");
                tokio::spawn(async move {
                    if let Err(e) = relay_tcp_to_quic(
                        pool_c, connector_addr, dest, port,
                        tcp_to_quic_rx, quic_to_tcp_tx,
                    ).await {
                        tracing::warn!(error = %e, "QUIC relay ended");
                    }
                });
            }
        }

        // --- Drive active relay sockets ---
        let active_handles: Vec<_> = active_relays.keys().cloned().collect();
        for handle in active_handles {
            let relay = active_relays.get_mut(&handle).unwrap();
            let socket = sockets.get_mut::<tcp::Socket>(handle);

            // Client → resource: drain bytes from the TCP socket into the
            // channel; the relay task reads them and writes to the QUIC stream.
            while socket.can_recv() {
                let mut buf = vec![0u8; 4096];
                match socket.recv_slice(&mut buf) {
                    Ok(0) | Err(_) => break,
                    Ok(n) => { let _ = relay.tcp_to_quic_tx.send(buf[..n].to_vec()); }
                }
            }

            // Resource → client: flush any previously buffered bytes first,
            // then pull fresh bytes from the relay channel.
            while !relay.write_buf.is_empty() && socket.can_send() {
                let chunk: Vec<u8> = relay.write_buf.drain(..).collect();
                match socket.send_slice(&chunk) {
                    Ok(n) if n < chunk.len() => {
                        relay.write_buf.extend(&chunk[n..]);
                        break;
                    }
                    _ => {}
                }
            }
            if relay.write_buf.is_empty() {
                while socket.can_send() {
                    match relay.quic_to_tcp_rx.try_recv() {
                        Ok(data) => {
                            match socket.send_slice(&data) {
                                Ok(n) if n < data.len() => {
                                    relay.write_buf.extend(&data[n..]);
                                    break;
                                }
                                _ => {}
                            }
                        }
                        Err(_) => break,
                    }
                }
            }

            // Clean up sockets whose TCP connection has closed.
            if !socket.is_active() && !socket.is_open() {
                active_relays.remove(&handle);
                sockets.remove(handle);
            }
        }

        let poll_delay = iface
            .poll_delay(smol_now, &sockets)
            .map(|d| Duration::from_micros(d.micros()))
            .unwrap_or(Duration::from_millis(SMOL_TICK_MS));

        tokio::time::sleep(poll_delay.min(Duration::from_millis(SMOL_TICK_MS))).await;
    }
}

fn new_listen_socket(sockets: &mut SocketSet<'_>, port: u16) -> SocketHandle {
    let rx_buf = tcp::SocketBuffer::new(vec![0u8; MAX_TCP_PAYLOAD]);
    let tx_buf = tcp::SocketBuffer::new(vec![0u8; MAX_TCP_PAYLOAD]);
    let mut socket = tcp::Socket::new(rx_buf, tx_buf);
    let _ = socket.listen(port);
    sockets.add(socket)
}

/// Bidirectional relay between the smoltcp TCP socket and the QUIC stream.
///
/// `tcp_to_quic_rx` carries bytes read from the TCP socket (client → resource).
/// `quic_to_tcp_tx` carries bytes read from the QUIC stream (resource → client).
async fn relay_tcp_to_quic(
    pool: Arc<TunnelPool>,
    connector_addr: SocketAddr,
    destination: String,
    port: u16,
    mut tcp_to_quic_rx: mpsc::UnboundedReceiver<Vec<u8>>,
    quic_to_tcp_tx: mpsc::UnboundedSender<Vec<u8>>,
) -> Result<()> {
    let (mut send, mut recv) = pool.open_stream(connector_addr).await?;

    // Send the tunnel handshake to the connector.
    let req = TunnelRequest {
        token: String::new(),
        destination: destination.clone(),
        port,
        protocol: "tcp".to_string(),
    };
    send.write_all(&serde_json::to_vec(&req)?).await?;

    // Read the connector's JSON response.
    let mut resp_buf = vec![0u8; 1024];
    let n = recv.read(&mut resp_buf).await?.unwrap_or(0);
    if n == 0 {
        return Err(anyhow!("connector closed stream before sending response"));
    }
    let resp: TunnelResponse = serde_json::from_slice(&resp_buf[..n])
        .map_err(|e| anyhow!("invalid tunnel response: {}", e))?;
    if !resp.ok {
        return Err(anyhow!(
            "tunnel denied for {}:{}: {}",
            destination, port,
            resp.error.unwrap_or_default()
        ));
    }

    tracing::info!(dest = %destination, port, "tunnel open — relaying");

    // Bidirectional relay loop.
    let mut quic_buf = vec![0u8; 65536];
    loop {
        tokio::select! {
            // Client → resource: bytes from the TCP socket go to the QUIC send stream.
            data = tcp_to_quic_rx.recv() => {
                match data {
                    Some(buf) => {
                        if send.write_all(&buf).await.is_err() { break; }
                    }
                    None => break, // TCP socket closed
                }
            }
            // Resource → client: bytes from the QUIC recv stream go to the TCP socket.
            result = recv.read(&mut quic_buf) => {
                match result {
                    Ok(Some(n)) if n > 0 => {
                        if quic_to_tcp_tx.send(quic_buf[..n].to_vec()).is_err() { break; }
                    }
                    _ => break, // QUIC stream finished or error
                }
            }
        }
    }

    let _ = send.finish();
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
