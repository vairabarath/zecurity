use std::collections::{HashMap, VecDeque};
use std::net::Ipv4Addr;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, Result};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use smoltcp::iface::{Config, Interface, SocketHandle, SocketSet};
use smoltcp::phy::{Device, DeviceCapabilities, Medium, RxToken, TxToken};
use smoltcp::socket::tcp;
use smoltcp::time::Instant as SmolInstant;
use smoltcp::wire::{HardwareAddress, IpAddress, IpCidr, IpListenEndpoint, Ipv4Address};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::sync::{mpsc, Notify};
use tokio::time::timeout;
use tun::AsyncDevice;

use crate::daemon::ResourceTarget;
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
    /// The resource identity this flow is for. The connector authorizes on this
    /// and dials the address from its own ACL — the client no longer picks the target.
    resource_id: String,
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

/// `transports` is the ONLY description of what is managed. It was previously
/// paired with the raw ACL entry list, which let the two disagree about which
/// destinations exist; the map now carries both pinned and synthetic keys, so the
/// second input was redundant and removing it makes divergence unrepresentable.
pub async fn run(
    dev: AsyncDevice,
    transports: Arc<HashMap<(Ipv4Addr, u16), Option<ResourceTarget>>>,
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

    // Every destination we listen on, taken from the TRANSPORTS MAP rather than
    // re-derived from the ACL.
    //
    // The map is the single source of truth for "which (ip, port) is managed": the
    // daemon already resolved pinned addresses and synthetic IPs into its keys
    // (Phase 9.4b). Re-parsing `entry.address` here was a THIRD place that
    // silently dropped name-addressed resources, and — worse — let the listener
    // set diverge from the transport set, which presents as a route into the TUN
    // with nothing behind it.
    let resource_entries: Vec<(Ipv4Addr, u16)> = transports.keys().copied().collect();

    // smoltcp must accept packets addressed to every managed destination.
    //
    // It cannot be done with per-/32 entries: `has_ip_addr` is an EXACT match
    // (no subnet containment) and `ip_addrs` is a fixed-capacity vec —
    // IFACE_MAX_ADDR_COUNT is 2 in this build. The previous code pushed one /32
    // per resource and discarded the `push` failures with `let _ =`, so from the
    // third address onward the entries were silently dropped and those resources
    // hung. A synthetic /22 cannot be expressed that way at all.
    //
    // AnyIP is the mechanism for exactly this: with it enabled, smoltcp accepts a
    // unicast destination that is not one of its own addresses PROVIDED its route
    // table resolves that destination to a router address it *does* own. So we
    // keep 100.64.0.1 as the interface address and add one default route via it.
    // Scope is bounded by the kernel, which only routes our marked flows into this
    // TUN at all.
    iface.update_ip_addrs(|addrs| {
        let _ = addrs.push(IpCidr::new(IpAddress::v4(100, 64, 0, 1), 32));
    });
    iface.set_any_ip(true);
    iface
        .routes_mut()
        .add_default_ipv4_route(Ipv4Address::new(100, 64, 0, 1))
        .map_err(|_| anyhow!("smoltcp route table full — cannot install default route"))?;

    let mut sockets = SocketSet::new(vec![]);

    // Create ONE listening TCP socket per resource BEFORE the loop.
    // Re-created each time a connection is accepted (smoltcp promotes the
    // listening socket to established, so a fresh one is needed immediately).
    let mut listen_handles: HashMap<(Ipv4Addr, u16), SocketHandle> = HashMap::new();
    for (ip, port) in &resource_entries {
        let handle = new_listen_socket(&mut sockets, *ip, *port);
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
                let new_handle = new_listen_socket(&mut sockets, ip, port);
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
                    Some(Some(target)) => {
                        // Managed resource, connector online → tunnel via QUIC.
                        if !target.transports.is_empty() {
                            let transports = target.transports.clone();
                            let resource_id = target.resource_id.clone();
                            // A synthetic IP is CLIENT-LOCAL: the connector has no
                            // pinned address to cross-check it against, and Phase
                            // 7.0 scoped the check accordingly. Sending it would be
                            // denied as `destination_mismatch`. Phase 9.4 and Phase
                            // 7.0 must agree on this — if either ships alone every
                            // name-addressed resource is denied.
                            let dest = if target.synthetic {
                                String::new()
                            } else {
                                dest
                            };
                            let resync = relay_resync.clone();
                            tokio::spawn(async move {
                                // relay_tcp_to_quic fires `resync` itself at the
                                // transport-failure points (open failure and
                                // mid-session drop). A normal close does not, so we
                                // don't resync on every finished connection.
                                if let Err(e) = relay_tcp_to_quic(
                                    transports,
                                    resource_id,
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

/// A listener bound to ONE synthetic address and port.
///
/// ⚠️ The address is not optional. `socket.listen(port)` — a bare `u16` — produces
/// `IpListenEndpoint { addr: None, .. }`, and smoltcp treats `None` as *any*
/// destination address:
///
/// ```text
/// let addr_ok = match self.listen_endpoint.addr {
///     Some(addr) => ip_repr.dst_addr() == addr,
///     None       => true,          // accepts ANY destination
/// };
/// ```
///
/// With `None`, two resources sharing a port are conflated: a connection to
/// resource A's synthetic IP is accepted by resource B's listener, and the client
/// then asserts B's `resource_id` on the wire. Observed live — a request to
/// `100.64.0.2:5443` was served by the listener for `100.64.0.3:5443`, reached the
/// wrong backend, and left one of the two resources unreachable.
///
/// Authorization still held (the connector checks the asserted `resource_id`
/// against the ACL), so this was mis-routing rather than escalation — but the
/// `(ip, port)` key in `listen_handles` was a fiction until this bound the address.
fn new_listen_socket(sockets: &mut SocketSet<'_>, ip: Ipv4Addr, port: u16) -> SocketHandle {
    let rx_buf = tcp::SocketBuffer::new(vec![0u8; MAX_TCP_PAYLOAD]);
    let tx_buf = tcp::SocketBuffer::new(vec![0u8; MAX_TCP_PAYLOAD]);
    let mut socket = tcp::Socket::new(rx_buf, tx_buf);
    let _ = socket.listen(IpListenEndpoint {
        addr: Some(IpAddress::Ipv4(Ipv4Address::from(ip))),
        port,
    });
    sockets.add(socket)
}

/// Bidirectional relay between the smoltcp TCP socket and the QUIC stream.
///
/// `tcp_to_quic_rx` carries bytes read from the TCP socket (client → resource).
/// `quic_to_tcp_tx` carries bytes read from the QUIC stream (resource → client).
async fn relay_tcp_to_quic(
    transports: Vec<Arc<ClientTransport>>,
    resource_id: String,
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
            resource_id: resource_id.clone(),
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
    use smoltcp::iface::{Config, Interface};
    use smoltcp::phy::{Loopback, Medium};
    use smoltcp::wire::{HardwareAddress, IpAddress, IpCidr, Ipv4Address};

    /// The whole synthetic-IP design rests on smoltcp accepting a destination it
    /// does not own. That is a claim about a third-party crate's behaviour, so it
    /// is asserted here rather than inferred from reading its source.
    ///
    /// Two facts together: `ip_addrs` has capacity IFACE_MAX_ADDR_COUNT (2 in this
    /// build) and `has_ip_addr` is an EXACT match with no subnet containment. So a
    /// synthetic /22 can only work via AnyIP + a route to an address we do own.
    #[test]
    fn smoltcp_ip_addrs_cannot_hold_a_per_resource_list() {
        let mut dev = Loopback::new(Medium::Ip);
        let mut config = Config::new(HardwareAddress::Ip);
        config.random_seed = 1;
        let mut iface = Interface::new(config, &mut dev, smoltcp_now());

        let mut pushed = 0;
        iface.update_ip_addrs(|addrs| {
            for i in 1..=8u8 {
                if addrs
                    .push(IpCidr::new(IpAddress::v4(10, 0, 0, i), 32))
                    .is_ok()
                {
                    pushed += 1;
                }
            }
        });
        assert!(
            pushed < 8,
            "if this ever passes, IFACE_MAX_ADDR_COUNT grew and the per-/32 \
             approach became viable — but note push failures were previously \
             discarded with `let _ =`, silently dropping addresses"
        );
    }

    /// A /22 in `ip_addrs` does NOT make smoltcp own the whole range — it owns
    /// exactly the base address. This is why AnyIP is required and why a single
    /// CIDR entry is not a shortcut.
    #[test]
    fn a_cidr_entry_covers_only_its_base_address() {
        let mut dev = Loopback::new(Medium::Ip);
        let mut config = Config::new(HardwareAddress::Ip);
        config.random_seed = 1;
        let mut iface = Interface::new(config, &mut dev, smoltcp_now());
        iface.update_ip_addrs(|addrs| {
            let _ = addrs.push(IpCidr::new(IpAddress::v4(100, 64, 0, 0), 22));
        });

        assert!(iface.has_ip_addr(IpAddress::v4(100, 64, 0, 0)));
        assert!(
            !iface.has_ip_addr(IpAddress::v4(100, 64, 0, 7)),
            "has_ip_addr is an exact match; a /22 entry does not cover the range"
        );
    }

    // ── Live client data-path verification (gated) ───────────────────────────
    //
    // Sprint 16 Phase 9.5. These build a REAL TUN device, install the REAL nft
    // chain and route, and push real packets, so they need root + CAP_NET_ADMIN
    // and must not touch a host firewall. Run inside a throwaway rootless netns,
    // the same way the shield's live_nft tests do:
    //
    //   cargo test --no-run                       # note the test binary path
    //   unshare -rn sh -c '
    //     ip link set lo up
    //     ip link add dummy0 type dummy
    //     ip addr add 10.99.0.1/24 dev dummy0
    //     ip link set dummy0 up
    //     ip route add default via 10.99.0.2 dev dummy0   # REQUIRED — see below
    //     exec <bin> live_synthetic --ignored --nocapture --test-threads=1'
    //
    // ⚠️ THE DEFAULT ROUTE IS NOT TEST SCAFFOLDING — it reflects a real dependency
    // of the whole mark-based steering design, and a bare namespace without it
    // produces a misleading failure.
    //
    // `connect()` performs a main-table route lookup to choose a source address
    // BEFORE any packet exists, and the nft output/mangle hook only runs on a
    // packet. So with no route to the destination in the main table, connect fails
    // immediately with ENETUNREACH and the mark rule never executes — table 105 is
    // never consulted. On a real host the default route always covers CGNAT space,
    // which is why this works in production. It also means: a host with no default
    // route cannot reach name-addressed resources at all. That is a pre-existing
    // property of the steering mechanism (it applies equally to pinned IPs outside
    // any local subnet), not something this phase introduced.
    //
    // What they cover that no unit test can: that the whole-CIDR nft mark plus the
    // CIDR route plus smoltcp's AnyIP actually combine to deliver a packet
    // addressed to a synthetic IP into a smoltcp socket. Every piece of that was
    // verified in isolation; this is the only place the composition is observed.

    /// Connect with a bounded wait and report which of the three outcomes happened.
    /// The distinction is the whole point: a HANG is the failure mode the
    /// whole-CIDR decision had to rule out, and it is invisible to a test that only
    /// asserts "not connected".
    /// The errno distinction is load-bearing, not cosmetic.
    ///
    /// A first version of this returned "refused" for every `Err`, which made the
    /// test pass even if the nft mark never fired: with no mark the packet misses
    /// table 105, finds no route in this namespace, and `connect` fails
    /// IMMEDIATELY with `NetworkUnreachable`. Only `ConnectionRefused` proves a
    /// RST came back — and a RST can only come from smoltcp, which means the
    /// packet actually traversed nft → route → TUN → stack.
    async fn probe(addr: &str) -> &'static str {
        // MUST be off-runtime. `#[tokio::test]` is single-threaded by default, and
        // `connect_timeout` is blocking — calling it inline starves the spawned
        // smoltcp poll loop, so nothing ever answers and the probe reports `hung`.
        // That is a test artefact indistinguishable from the real failure it is
        // meant to detect, so the tests below also use the multi_thread flavor.
        let addr = addr.to_string();
        tokio::task::spawn_blocking(move || probe_blocking(&addr))
            .await
            .unwrap()
    }

    fn probe_blocking(addr: &str) -> &'static str {
        let sa: std::net::SocketAddr = addr.parse().unwrap();
        match std::net::TcpStream::connect_timeout(&sa, Duration::from_millis(1500)) {
            Ok(_) => "connected",
            Err(e) => match e.kind() {
                std::io::ErrorKind::TimedOut => "hung",
                std::io::ErrorKind::ConnectionRefused => "refused-by-stack",
                // No route at all — the packet never left the kernel, so nothing
                // about the TUN path was exercised.
                _ => "unroutable",
            },
        }
    }

    async fn spawn_stack(
        transports: HashMap<(Ipv4Addr, u16), Option<ResourceTarget>>,
        cidr: crate::registry::Net,
    ) -> (crate::tun::TunManager, tokio::task::AbortHandle) {
        spawn_stack_with(transports, cidr, &[]).await
    }

    async fn spawn_stack_with(
        transports: HashMap<(Ipv4Addr, u16), Option<ResourceTarget>>,
        cidr: crate::registry::Net,
        flows: &[crate::tun::AllowedFlow],
    ) -> (crate::tun::TunManager, tokio::task::AbortHandle) {
        let mut mgr = crate::tun::TunManager::create()
            .await
            .expect("create TUN (needs root in a netns)");
        mgr.configure_allowed_flows(flows, Some(cidr))
            .expect("install nft + route");
        let dev = mgr.take_device().expect("take device");
        let task = tokio::spawn(async move {
            let _ = run(dev, Arc::new(transports), Arc::new(Notify::new())).await;
        });
        // Let smoltcp reach its poll loop before any packet arrives.
        tokio::time::sleep(Duration::from_millis(200)).await;
        (mgr, task.abort_handle())
    }

    /// THE Phase 9 composition test: a packet addressed to a synthetic IP — an
    /// address no interface owns — must reach a smoltcp listener.
    ///
    /// The transports slot is `Some(None)` (managed, connector offline), so the
    /// client fails closed after accepting. That is fine for this assertion: what
    /// is being proven is that the SYN arrives at all. If nft, the route, or AnyIP
    /// is wrong the connect HANGS, because nothing answers.
    #[tokio::test(flavor = "multi_thread")]
    #[ignore = "live kernel test; run inside 'unshare -rn' (see module comment)"]
    async fn live_synthetic_ip_reaches_the_smoltcp_stack() {
        let cidr = crate::registry::Net::new("100.64.0.0".parse().unwrap(), 22);
        let synth: Ipv4Addr = "100.64.0.7".parse().unwrap();
        let mut transports = HashMap::new();
        transports.insert((synth, 5432u16), None); // managed, connector offline

        let (mgr, abort) = spawn_stack(transports, cidr).await;
        let outcome = probe("100.64.0.7:5432").await;
        abort.abort();
        let _ = mgr.cleanup().await;

        eprintln!("[phase9] synthetic 100.64.0.7:5432 → {outcome}");
        assert!(
            outcome == "connected" || outcome == "refused-by-stack",
            "got {outcome}: a packet to a synthetic IP must reach smoltcp. \
             `hung` means nft/route/AnyIP delivered it nowhere — the Gate 1 stall \
             shape. `unroutable` means the nft mark never fired, so the packet \
             never even entered table 105."
        );
    }

    /// The consequence the whole-CIDR decision accepted, now verified rather than
    /// assumed (path.md decision #5 says to confirm and record it).
    ///
    /// The mark rule is port-agnostic, so a port that is NOT in the ACL is still
    /// steered into the TUN — where no smoltcp listener exists. That MUST be a
    /// clean refusal. A hang here would mean every mistyped port becomes a
    /// 2-minute stall for the user.
    #[tokio::test(flavor = "multi_thread")]
    #[ignore = "live kernel test; run inside 'unshare -rn' (see module comment)"]
    async fn live_unlistened_port_on_a_synthetic_ip_refuses_rather_than_hangs() {
        let cidr = crate::registry::Net::new("100.64.0.0".parse().unwrap(), 22);
        let synth: Ipv4Addr = "100.64.0.7".parse().unwrap();
        let mut transports = HashMap::new();
        transports.insert((synth, 5432u16), None); // only :5432 is managed

        let (mgr, abort) = spawn_stack(transports, cidr).await;
        let outcome = probe("100.64.0.7:9999").await; // not in the ACL
        abort.abort();
        let _ = mgr.cleanup().await;

        eprintln!("[phase9] unlistened 100.64.0.7:9999 → {outcome}");
        assert_eq!(
            outcome, "refused-by-stack",
            "a non-ACL port on a synthetic IP must be refused BY THE STACK, not \
             hang and not fall off the routing table — this is the accepted cost \
             of the port-agnostic whole-CIDR rule (path.md decision #5)"
        );
    }

    /// Verify-list item 1: a PINNED IP resource behaves identically to before.
    ///
    /// The exact counterpart to `live_unmanaged_destination_is_not_captured`: the
    /// SAME address, the SAME namespace, and the opposite outcome — decided solely
    /// by whether it is in the ACL. Together they prove split tunnelling works in
    /// both directions, and that Phase 9 did not disturb the pinned path while
    /// adding the synthetic one.
    ///
    /// 10.99.0.5 has no host behind it, so an uncaptured connect stalls on
    /// neighbour resolution. Captured, our stack answers immediately.
    #[tokio::test(flavor = "multi_thread")]
    #[ignore = "live kernel test; run inside 'unshare -rn' (see module comment)"]
    async fn live_pinned_resource_is_still_captured() {
        let cidr = crate::registry::Net::new("100.64.0.0".parse().unwrap(), 22);
        let pinned: Ipv4Addr = "10.99.0.5".parse().unwrap();
        let mut transports = HashMap::new();
        transports.insert((pinned, 8080u16), None); // managed, connector offline

        let (mgr, abort) = spawn_stack_with(
            transports,
            cidr,
            &[crate::tun::AllowedFlow {
                ip: pinned,
                port: 8080,
            }],
        )
        .await;
        let outcome = probe("10.99.0.5:8080").await;
        abort.abort();
        let _ = mgr.cleanup().await;

        eprintln!("[phase9] pinned 10.99.0.5:8080 (in ACL) → {outcome}");
        // "Captured" is what is being asserted, and it has two legitimate
        // presentations: `connected` when a listener exists for the port (smoltcp
        // completes the handshake, then the offline connector fails the flow
        // closed), or `refused-by-stack` when it does not (immediate RST). Both
        // prove our stack answered. Only `hung`/`unroutable` mean the packet was
        // never captured — which for an in-ACL flow would be a regression in the
        // pre-existing pinned path.
        assert!(
            outcome == "connected" || outcome == "refused-by-stack",
            "got {outcome}: a pinned resource must still be captured by its \
             per-(ip,port) rule. Note the unmanaged test probes this SAME address \
             and must get `hung` — that contrast is the split-tunnelling proof."
        );
    }

    /// The kernel half of Phase 2's inherited debt: UNMANAGED traffic must not be
    /// captured.
    ///
    /// ADR-009 makes bypass a kernel property — a flow nobody granted is never
    /// marked, so it never enters table 105 and never reaches the TUN. The map's
    /// `absent` state is a backstop, not the mechanism. This asserts the mechanism:
    /// an address outside both the synthetic CIDR and the pinned set must take the
    /// normal route, which in this namespace means it does NOT get the fast
    /// stack-generated RST that a captured flow gets.
    ///
    /// 10.99.0.5 is on dummy0's subnet with no host behind it, so an uncaptured
    /// connect stalls on neighbour resolution. A captured one would be answered by
    /// smoltcp immediately — so `refused-by-stack` here would mean we are
    /// hijacking traffic we were never granted.
    #[tokio::test(flavor = "multi_thread")]
    #[ignore = "live kernel test; run inside 'unshare -rn' (see module comment)"]
    async fn live_unmanaged_destination_is_not_captured() {
        let cidr = crate::registry::Net::new("100.64.0.0".parse().unwrap(), 22);
        let synth: Ipv4Addr = "100.64.0.7".parse().unwrap();
        let mut transports = HashMap::new();
        transports.insert((synth, 5432u16), None);

        let (mgr, abort) = spawn_stack(transports, cidr).await;
        let outcome = probe("10.99.0.5:8080").await; // granted to nobody
        abort.abort();
        let _ = mgr.cleanup().await;

        eprintln!("[phase9] unmanaged 10.99.0.5:8080 → {outcome}");
        assert_ne!(
            outcome, "refused-by-stack",
            "an unmanaged destination was answered by OUR stack — the nft rules are \
             capturing traffic this device was never granted, which is the \
             split-tunnelling violation ADR-009 exists to prevent"
        );
    }

    /// AnyIP + a default route via an address we own is what makes an arbitrary
    /// synthetic destination acceptable. Asserts the configuration this module
    /// installs actually holds, so a future smoltcp bump that changes AnyIP
    /// semantics fails here rather than in a live tunnel.
    #[test]
    fn any_ip_plus_a_default_route_is_installable() {
        let mut dev = Loopback::new(Medium::Ip);
        let mut config = Config::new(HardwareAddress::Ip);
        config.random_seed = 1;
        let mut iface = Interface::new(config, &mut dev, smoltcp_now());

        iface.update_ip_addrs(|addrs| {
            let _ = addrs.push(IpCidr::new(IpAddress::v4(100, 64, 0, 1), 32));
        });
        iface.set_any_ip(true);
        iface
            .routes_mut()
            .add_default_ipv4_route(Ipv4Address::new(100, 64, 0, 1))
            .expect("one default route must fit in IFACE_MAX_ROUTE_COUNT");

        assert!(iface.any_ip());
        // The router address the default route points at must be one we own, or
        // AnyIP's own check rejects the packet.
        assert!(iface.has_ip_addr(IpAddress::v4(100, 64, 0, 1)));
    }

    /// A listening socket MUST be bound to the ONE synthetic address it was
    /// created for. `tcp::Socket::listen(port)` — the `u16` overload — produces
    /// `IpListenEndpoint { addr: None }`, and smoltcp reads `None` as "accept ANY
    /// destination" (`socket/tcp.rs`: `match self.listen_endpoint.addr { .. None
    /// => true }`). With one listener per (synthetic IP, port) that is a
    /// cross-resource leak: two resources sharing a port get whichever socket the
    /// SocketSet happens to visit first, so a connection to resource A is relayed
    /// down resource B's tunnel — wrong backend, wrong authorization.
    ///
    /// The wrongly-addressed socket is added FIRST on purpose. Under the bug it is
    /// the one that wins the SYN, so this test fails; under the fix it must still
    /// be sitting in Listen while the correctly-addressed socket is Established.
    #[test]
    fn a_listener_only_accepts_its_own_synthetic_address() {
        let owned = Ipv4Address::new(100, 64, 0, 1);
        let wrong_ip: Ipv4Addr = "100.64.0.3".parse().unwrap();
        let right_ip: Ipv4Addr = "100.64.0.2".parse().unwrap();
        const PORT: u16 = 5443;

        let mut dev = Loopback::new(Medium::Ip);
        let mut config = Config::new(HardwareAddress::Ip);
        config.random_seed = 1;
        let mut iface = Interface::new(config, &mut dev, smoltcp_now());
        iface.update_ip_addrs(|addrs| {
            let _ = addrs.push(IpCidr::new(IpAddress::Ipv4(owned), 32));
        });
        iface.set_any_ip(true);
        iface
            .routes_mut()
            .add_default_ipv4_route(owned)
            .expect("one default route must fit");

        let mut sockets = SocketSet::new(Vec::new());
        let wrong = new_listen_socket(&mut sockets, wrong_ip, PORT);
        let right = new_listen_socket(&mut sockets, right_ip, PORT);

        let client = sockets.add(tcp::Socket::new(
            tcp::SocketBuffer::new(vec![0u8; 1024]),
            tcp::SocketBuffer::new(vec![0u8; 1024]),
        ));
        sockets
            .get_mut::<tcp::Socket>(client)
            .connect(
                iface.context(),
                (IpAddress::Ipv4(Ipv4Address::from(right_ip)), PORT),
                49152u16,
            )
            .expect("connect to the synthetic address");

        let mut now = smoltcp_now();
        for _ in 0..64 {
            iface.poll(now, &mut dev, &mut sockets);
            if sockets.get::<tcp::Socket>(client).may_send() {
                break;
            }
            now += smoltcp::time::Duration::from_millis(10);
        }

        assert_eq!(
            sockets.get::<tcp::Socket>(right).state(),
            tcp::State::Established,
            "the listener bound to {right_ip} did not take its own connection"
        );
        assert_eq!(
            sockets.get::<tcp::Socket>(wrong).state(),
            tcp::State::Listen,
            "the listener bound to {wrong_ip} answered a SYN addressed to \
             {right_ip} — listen() was given a bare port, so it matches any \
             destination and resources sharing a port cross over"
        );
    }
}
