use std::collections::{HashMap, VecDeque};
use std::net::{IpAddr, Ipv4Addr};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, Result};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use smoltcp::iface::{Config, Interface, SocketHandle, SocketSet};
use smoltcp::phy::{Device, DeviceCapabilities, Medium, RxToken, TxToken};
use smoltcp::socket::tcp;
use smoltcp::time::Instant as SmolInstant;
use smoltcp::wire::{HardwareAddress, IpAddress, IpCidr, Ipv4Address};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::sync::{mpsc, Notify};
use tokio::time::timeout;
use tun::AsyncDevice;

use crate::grpc::client_v1::AclEntry;
use crate::transport::ClientTransport;
use crate::tunnel_pool::TunnelOpenError;

/// True when a stream-open error was an authentication/identity failure
/// (revoked or mismatched cert/SPIFFE) rather than a network/transport failure.
/// Auth failures fail closed — re-polling the transport plane can't fix a bad
/// credential — so they must NOT trigger an accelerated transport resync
/// (finding #8). Recovers the typed error from the anyhow chain.
fn is_auth_failure(err: &anyhow::Error) -> bool {
    err.chain()
        .find_map(|c| c.downcast_ref::<TunnelOpenError>())
        .map(|e| matches!(e, TunnelOpenError::Authenticate(_)))
        .unwrap_or(false)
}

const TUNNEL_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(5);
const MAX_TCP_PAYLOAD: usize = 64 * 1024;
const MAX_TUNNEL_HANDSHAKE_SIZE: usize = 16 * 1024;
const SMOL_TICK_MS: u64 = 5;
const TUN_TX_QUEUE_CAP: usize = 1024;
const FLOW_QUEUE_CAP: usize = 64;
const FLOW_WRITE_BUF_CAP: usize = 1024 * 1024;

// --- TunDevice: bridges tun::AsyncDevice to smoltcp's Device trait ---

struct TunRxToken(Vec<u8>);
struct TunTxToken(mpsc::Sender<Vec<u8>>);

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
        let _ = self.0.try_send(buf);
        result
    }
}

struct TunDevice {
    rx: std::sync::mpsc::Receiver<Vec<u8>>,
    tx: mpsc::Sender<Vec<u8>>,
}

impl Device for TunDevice {
    type RxToken<'a>
        = TunRxToken
    where
        Self: 'a;
    type TxToken<'a>
        = TunTxToken
    where
        Self: 'a;

    fn receive(
        &mut self,
        _timestamp: SmolInstant,
    ) -> Option<(Self::RxToken<'_>, Self::TxToken<'_>)> {
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
    destination: String,
    port: u16,
    protocol: String,
}

#[derive(Deserialize, Debug)]
struct TunnelResponse {
    ok: bool,
    error: Option<String>,
}

/// Error from the framed-JSON handshake helpers, split so the caller can tell a
/// genuine transport failure (network I/O) from a protocol failure (bad or
/// oversized payload). Only the former warrants an early transport resync.
#[derive(Debug, thiserror::Error)]
enum FramedJsonError {
    #[error("transport I/O failed: {0}")]
    Io(#[from] std::io::Error),

    #[error("JSON encoding failed: {0}")]
    Encode(serde_json::Error),

    #[error("JSON decoding failed: {0}")]
    Decode(serde_json::Error),

    #[error("frame too large: {0} bytes")]
    FrameTooLarge(usize),
}

impl FramedJsonError {
    /// True only for genuine transport I/O failures. Protocol failures — malformed
    /// JSON (`Decode`), oversized frames (`FrameTooLarge`), encode errors — are NOT
    /// transport failures: the path reached the peer fine, so re-polling the
    /// transport plane cannot help.
    ///
    /// Deliberately broad (`Io(_)`, not an `ErrorKind` allowlist): every `Io` here
    /// originates from the QUIC stream read/write, because the JSON/size failures
    /// are separate variants. Verified against quinn 0.11.9 — a lost connection
    /// maps to `ConnectionReset`/`NotConnected` and a clean close to `UnexpectedEof`,
    /// all `Io`. Broad also survives future quinn error-mapping changes, and an
    /// unnecessary resync is cheap whereas a missed one strands the client on a
    /// dead relay until the next 60s tick.
    fn is_transport_failure(&self) -> bool {
        matches!(self, Self::Io(_))
    }
}

async fn write_framed_json<W, T>(
    writer: &mut W,
    value: &T,
) -> std::result::Result<(), FramedJsonError>
where
    W: AsyncWrite + Unpin,
    T: Serialize,
{
    let body = serde_json::to_vec(value).map_err(FramedJsonError::Encode)?;
    if body.len() > MAX_TUNNEL_HANDSHAKE_SIZE {
        return Err(FramedJsonError::FrameTooLarge(body.len()));
    }

    writer.write_all(&(body.len() as u32).to_be_bytes()).await?;
    writer.write_all(&body).await?;
    writer.flush().await?;
    Ok(())
}

async fn read_framed_json<R, T>(reader: &mut R) -> std::result::Result<T, FramedJsonError>
where
    R: AsyncRead + Unpin,
    T: DeserializeOwned,
{
    let mut length = [0u8; 4];
    reader.read_exact(&mut length).await?;
    let length = u32::from_be_bytes(length) as usize;
    if length > MAX_TUNNEL_HANDSHAKE_SIZE {
        return Err(FramedJsonError::FrameTooLarge(length));
    }

    let mut body = vec![0u8; length];
    reader.read_exact(&mut body).await?;
    serde_json::from_slice(&body).map_err(FramedJsonError::Decode)
}

// --- Per-connection relay state (lives in the smoltcp loop) ---

struct ActiveRelay {
    // smoltcp loop → relay task: client payload going to the resource
    tcp_to_quic_tx: mpsc::Sender<Vec<u8>>,
    // relay task → smoltcp loop: resource payload coming back to the client
    quic_to_tcp_rx: mpsc::Receiver<Vec<u8>>,
    // overflow buffer when the smoltcp send window is temporarily full
    write_buf: VecDeque<u8>,
}

// --- Main entry point ---

pub async fn run(
    dev: AsyncDevice,
    allowed_entries: Vec<AclEntry>,
    transports: Arc<HashMap<(Ipv4Addr, u16), Option<Vec<Arc<ClientTransport>>>>>,
    relay_resync: Arc<Notify>,
) -> Result<()> {
    let (rx_sync_tx, rx_sync_rx) = std::sync::mpsc::sync_channel::<Vec<u8>>(TUN_TX_QUEUE_CAP);
    let (tx_async_tx, mut tx_async_rx) = mpsc::channel::<Vec<u8>>(TUN_TX_QUEUE_CAP);

    let (mut tun_read, mut tun_write) = tokio::io::split(dev);

    // TUN → smoltcp: read raw IP packets from the kernel and forward to the
    // sync channel that TunDevice::receive() drains each poll cycle.
    tokio::spawn(async move {
        let mut buf = vec![0u8; 4096];
        loop {
            match tun_read.read(&mut buf).await {
                Ok(0) | Err(_) => break,
                Ok(n) => {
                    let _ = rx_sync_tx.try_send(buf[..n].to_vec());
                }
            }
        }
    });

    // smoltcp → TUN: write IP packets that smoltcp emits back to the kernel.
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

    let mut config = Config::new(HardwareAddress::Ip);
    config.random_seed = rand::random();
    let mut iface = Interface::new(config, &mut tun_dev, smoltcp_now());

    // Collect TCP resources from the allowed (SPIFFE-filtered) entries.
    let resource_entries: Vec<(Ipv4Addr, u16)> = allowed_entries
        .iter()
        .filter(|e| e.protocol.to_lowercase() == "tcp" || e.protocol.is_empty())
        .filter_map(|e| {
            let ip = e.address.parse::<IpAddr>().ok()?;
            match ip {
                IpAddr::V4(v4) => Some((v4, e.port as u16)),
                _ => None,
            }
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
                let (tcp_to_quic_tx, tcp_to_quic_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
                let (quic_to_tcp_tx, quic_to_tcp_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);

                active_relays.insert(
                    handle,
                    ActiveRelay {
                        tcp_to_quic_tx,
                        quic_to_tcp_rx,
                        write_buf: VecDeque::new(),
                    },
                );

                let dest = ip.to_string();
                tracing::info!(dest = %dest, port, "new TCP connection");
                match transports.get(&(ip, port)) {
                    Some(Some(transports)) => {
                        // Managed resource, connector online → tunnel via QUIC.
                        if !transports.is_empty() {
                            let transports = transports.clone();
                            let resync = relay_resync.clone();
                            tokio::spawn(async move {
                                // relay_tcp_to_quic fires `resync` itself at the
                                // transport-failure points (open failure and
                                // mid-session drop). A normal close does not, so we
                                // don't resync on every finished connection.
                                if let Err(e) = relay_tcp_to_quic(
                                    transports,
                                    dest,
                                    port,
                                    tcp_to_quic_rx,
                                    quic_to_tcp_tx,
                                    resync,
                                )
                                .await
                                {
                                    tracing::warn!(error = %e, "QUIC relay ended");
                                }
                            });
                        } else {
                            tracing::warn!(
                                dest = %dest,
                                port,
                                "transport list unexpectedly empty"
                            );
                            drop(tcp_to_quic_rx);
                            drop(quic_to_tcp_tx);
                        }
                    }
                    Some(None) => {
                        // Managed resource, connector offline → fail closed.
                        // Dropping the channels causes smoltcp to RST the connection.
                        tracing::warn!(dest = %dest, port, "connector offline — failing closed for managed resource");
                    }
                    None => {
                        // The listener set is already built from allowed ACL entries.
                        // Missing transport here means malformed snapshot/RN data, so
                        // fail closed instead of bypassing a managed destination.
                        tracing::warn!(dest = %dest, port, "no transport for managed resource — failing closed");
                        drop(tcp_to_quic_rx);
                        drop(quic_to_tcp_tx);
                    }
                }
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
                    Ok(n) => {
                        if relay.tcp_to_quic_tx.try_send(buf[..n].to_vec()).is_err() {
                            tracing::warn!("flow queue full; closing TCP flow");
                            socket.close();
                            break;
                        }
                    }
                }
            }

            // Resource → client: flush any previously buffered bytes first,
            // then pull fresh bytes from the relay channel.
            while !relay.write_buf.is_empty() && socket.can_send() {
                let chunk: Vec<u8> = relay.write_buf.drain(..).collect();
                match socket.send_slice(&chunk) {
                    Ok(n) if n < chunk.len() => {
                        let pending = chunk.len() - n;
                        if relay.write_buf.len() + pending > FLOW_WRITE_BUF_CAP {
                            tracing::warn!("write buffer cap exceeded; closing TCP flow");
                            socket.close();
                            break;
                        }
                        relay.write_buf.extend(&chunk[n..]);
                        break;
                    }
                    _ => {}
                }
            }
            if relay.write_buf.is_empty() {
                while socket.can_send() {
                    match relay.quic_to_tcp_rx.try_recv() {
                        Ok(data) => match socket.send_slice(&data) {
                            Ok(n) if n < data.len() => {
                                let pending = data.len() - n;
                                if relay.write_buf.len() + pending > FLOW_WRITE_BUF_CAP {
                                    tracing::warn!("write buffer cap exceeded; closing TCP flow");
                                    socket.close();
                                    break;
                                }
                                relay.write_buf.extend(&data[n..]);
                                break;
                            }
                            _ => {}
                        },
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
    transports: Vec<Arc<ClientTransport>>,
    destination: String,
    port: u16,
    mut tcp_to_quic_rx: mpsc::Receiver<Vec<u8>>,
    quic_to_tcp_tx: mpsc::Sender<Vec<u8>>,
    resync: Arc<Notify>,
) -> Result<()> {
    let mut selected_stream = None;
    // Whether any failure was a network/transport failure (relay/connector
    // unreachable). Only these warrant an early transport resync; auth failures
    // and connector denials fail closed (finding #8).
    let mut saw_transport_failure = false;

    for transport in transports {
        let candidate = match transport.open_authenticated_stream().await {
            Ok(stream) => stream,

            Err(e) => {
                if is_auth_failure(&e) {
                    // Revoked/mismatched cert or SPIFFE — re-polling transport
                    // won't help. Fail closed; do not signal a resync.
                    tracing::warn!(
                        destination = %destination,
                        port,
                        error = %e,
                        "connector rejected authentication — failing closed (no transport resync)"
                    );
                } else {
                    tracing::warn!(
                        destination = %destination,
                        port,
                        error = %e,
                        "failed to reach connector (transport), trying next"
                    );
                    saw_transport_failure = true;
                }
                continue;
            }
        };

        let mut stream = candidate;

        // Send the tunnel handshake to the connector.
        let req = TunnelRequest {
            destination: destination.clone(),
            port,
            protocol: "tcp".to_string(),
        };

        // Send handshake + read response, bounded so a stalled peer can't wedge us.
        let handshake = timeout(TUNNEL_HANDSHAKE_TIMEOUT, async {
            write_framed_json(&mut stream, &req).await?;
            read_framed_json::<_, TunnelResponse>(&mut stream).await
        })
        .await;

        let resp: TunnelResponse = match handshake {
            Ok(Ok(resp)) => resp,
            Ok(Err(error)) => {
                // Only a transport I/O failure warrants a resync; a malformed or
                // oversized reply means the path is fine but the peer is confused.
                let transport_failure = error.is_transport_failure();
                tracing::warn!(
                    error = %error,
                    transport_failure,
                    "tunnel handshake failed"
                );
                saw_transport_failure |= transport_failure;
                continue;
            }
            Err(_) => {
                tracing::warn!(
                    dest = %destination, port,
                    "tunnel handshake timed out after {:?}", TUNNEL_HANDSHAKE_TIMEOUT
                );
                // Timeout means the selected relay/connector path is unusable.
                saw_transport_failure = true;
                continue;
            }
        };

        if resp.ok {
            tracing::info!(
                dest = %destination,
                port,
                "tunnel opened"
            );
            selected_stream = Some(stream);
            break;
        }
        match resp.error.as_deref() {
            Some("SHIELD_NOT_ATTACHED") => {
                tracing::warn!(
                    dest = %destination,
                    port,
                    "shield not attached, trying next connector"
                );
                continue;
            }
            _ => {
                return Err(anyhow!("tunnel denied: {}", resp.error.unwrap_or_default()));
            }
        }
    }
    let stream = match selected_stream {
        Some(stream) => stream,
        None => {
            // No transport accepted the tunnel. Signal a transport resync ONLY
            // if the failures were network/transport (relay down, or connector
            // re-homed to a relay the transport plane hasn't propagated yet).
            // If every failure was an auth rejection or a connector denial,
            // re-polling won't help — fail closed without signalling (finding #8).
            if saw_transport_failure {
                resync.notify_one();
            } else {
                tracing::warn!(
                    dest = %destination, port,
                    "no connector accepted tunnel (auth/denial only) — failing closed, no resync"
                );
            }
            return Err(anyhow!(
                "no connector accepted tunnel for {}: {}",
                destination,
                port
            ));
        }
    };
    let (mut recv, mut send) = tokio::io::split(stream);
    // Bidirectional relay loop. `relay_failed` distinguishes a mid-session
    // transport error (relay/connector dropped — worth an early resync) from a
    // normal close (client or resource closed the stream — no resync).
    let mut quic_buf = vec![0u8; 65536];
    let mut relay_failed = false;
    loop {
        tokio::select! {
            // Client → resource: bytes from the TCP socket go to the QUIC send stream.
            data = tcp_to_quic_rx.recv() => {
                match data {
                    Some(buf) => {
                        if send.write_all(&buf).await.is_err() {
                            relay_failed = true; // QUIC send failed mid-session
                            break;
                        }
                    }
                    None => break, // TCP socket closed (normal client-side close)
                }
            }
            // Resource → client: bytes from the QUIC recv stream go to the TCP socket.
            result = recv.read(&mut quic_buf) => {
                match result {
                    Ok(n) if n > 0 => {
                        if quic_to_tcp_tx.send(quic_buf[..n].to_vec()).await.is_err() {
                            break; // client-side channel closed (socket gone) — normal
                        }
                    }
                    Ok(_) => break, // n == 0: QUIC stream finished (normal EOF)
                    Err(_) => {
                        relay_failed = true; // mid-session QUIC read error
                        break;
                    }
                }
            }
        }
    }

    let _ = send.shutdown().await;
    if relay_failed {
        resync.notify_one();
    }
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

#[cfg(test)]
mod tests {
    use super::*;
    use tokio::io::{duplex, AsyncWriteExt};

    // A malformed JSON body is a protocol failure, not a transport failure:
    // we received a well-framed reply, it just didn't parse. No resync.
    #[tokio::test]
    async fn malformed_handshake_json_is_not_transport_failure() {
        let (mut writer, mut reader) = duplex(64);

        let body = b"{invalid-json";
        writer
            .write_all(&(body.len() as u32).to_be_bytes())
            .await
            .unwrap();
        writer.write_all(body).await.unwrap();
        drop(writer);

        let error = read_framed_json::<_, TunnelResponse>(&mut reader)
            .await
            .unwrap_err();

        assert!(matches!(error, FramedJsonError::Decode(_)));
        assert!(!error.is_transport_failure());
    }

    // A dropped connection surfaces as an I/O error (UnexpectedEof here): the
    // path is dead, so this IS a transport failure and should resync.
    #[tokio::test]
    async fn disconnected_handshake_is_transport_failure() {
        let (writer, mut reader) = duplex(64);
        drop(writer);

        let error = read_framed_json::<_, TunnelResponse>(&mut reader)
            .await
            .unwrap_err();

        assert!(matches!(error, FramedJsonError::Io(_)));
        assert!(error.is_transport_failure());
    }

    // An oversized frame length is rejected before reading the body — a protocol
    // failure, not transport. No resync.
    #[tokio::test]
    async fn oversized_handshake_is_not_transport_failure() {
        let (mut writer, mut reader) = duplex(64);
        let size = MAX_TUNNEL_HANDSHAKE_SIZE + 1;

        writer
            .write_all(&(size as u32).to_be_bytes())
            .await
            .unwrap();
        drop(writer);

        let error = read_framed_json::<_, TunnelResponse>(&mut reader)
            .await
            .unwrap_err();

        assert!(matches!(
            error,
            FramedJsonError::FrameTooLarge(value) if value == size
        ));
        assert!(!error.is_transport_failure());
    }
}
