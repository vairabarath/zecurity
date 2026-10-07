use std::collections::{HashMap, VecDeque};
use std::net::{IpAddr, Ipv4Addr};
use std::sync::atomic::{AtomicU8, Ordering as AtomicOrdering};
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
use tokio::sync::{mpsc, watch, Notify};
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

/// Fix 05-A back-stop: a relay socket that sits in a closing state
/// (FIN-WAIT-1/2, CLOSE-WAIT, CLOSING, LAST-ACK) with no byte or state
/// activity for this long is aborted, so a missed lifecycle path can never
/// leak a socket (and its 4-tuple) for the life of the net_stack.
/// ESTABLISHED flows are never subject to it: idle long-lived flows are fine.
const CLOSING_IDLE_LIMIT: smoltcp::time::Duration = smoltcp::time::Duration::from_secs(60);

/// Fix 05-A observability: emit the lifecycle stats line at most this often,
/// and only when the counts changed. The daemon's log filter is fixed at
/// `info`, so a `debug` metric would never be visible.
const LIFECYCLE_STATS_INTERVAL: Duration = Duration::from_secs(60);
/// Warn (once per crossing) when this many relay sockets are alive at once.
const ACTIVE_RELAYS_WARN: usize = 1024;

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
    /// True only for connectivity failures that may be repaired by fetching
    /// updated transport coordinates.
    ///
    /// Protocol errors, malformed responses, oversized frames, encoding
    /// failures, and unrelated local I/O errors must not trigger recovery.
    fn is_transport_failure(&self) -> bool {
        matches!(
            self,
            Self::Io(error)
                if matches!(
                    error.kind(),
                    std::io::ErrorKind::ConnectionReset
                        | std::io::ErrorKind::ConnectionAborted
                        | std::io::ErrorKind::BrokenPipe
                        | std::io::ErrorKind::UnexpectedEof
                        | std::io::ErrorKind::TimedOut
                        | std::io::ErrorKind::NotConnected
                )
        )
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

/// How a relay task ended (Fix 05-A). The task stores this **before** it drops
/// its `quic_to_tcp` sender, so once the poll loop sees the channel
/// disconnected it knows whether to FIN (normal end) or RST (failure).
/// A task that dies without storing an outcome (panic) leaves `RUNNING`,
/// which the loop treats as a failure.
const RELAY_RUNNING: u8 = 0;
const RELAY_ENDED_OK: u8 = 1;
const RELAY_FAILED: u8 = 2;

/// Result of a relay task that got as far as running its data loop.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum RelayEnd {
    /// Normal close (QUIC EOF, client closed, or the TCP side went away).
    Normal,
    /// Mid-session transport failure (QUIC read/write error).
    Failed,
}

struct ActiveRelay {
    // smoltcp loop → relay task: client payload going to the resource.
    // `None` once the client side has closed (CLOSE-WAIT) — dropping the
    // sender tells the relay task the client is done.
    tcp_to_quic_tx: Option<mpsc::Sender<Vec<u8>>>,
    // relay task → smoltcp loop: resource payload coming back to the client.
    // `None` once the relay task has ended (channel disconnected).
    quic_to_tcp_rx: Option<mpsc::Receiver<Vec<u8>>>,
    // overflow buffer when the smoltcp send window is temporarily full
    write_buf: VecDeque<u8>,
    // set by the relay task before it ends (RELAY_*)
    outcome: Arc<AtomicU8>,
    // last time bytes moved or the TCP state changed (back-stop clock)
    last_activity: SmolInstant,
    last_state: tcp::State,
    // close()/abort() already issued by the lifecycle logic
    terminated: bool,
}

impl ActiveRelay {
    fn new(
        tcp_to_quic_tx: mpsc::Sender<Vec<u8>>,
        quic_to_tcp_rx: mpsc::Receiver<Vec<u8>>,
        outcome: Arc<AtomicU8>,
        now: SmolInstant,
    ) -> Self {
        Self {
            tcp_to_quic_tx: Some(tcp_to_quic_tx),
            quic_to_tcp_rx: Some(quic_to_tcp_rx),
            write_buf: VecDeque::new(),
            outcome,
            last_activity: now,
            last_state: tcp::State::SynReceived,
            terminated: false,
        }
    }

    /// A flow refused at accept time (fail closed). The caller has already
    /// aborted the socket; the entry only exists so the socket is removed
    /// after its RST has been dispatched.
    fn fail_closed(now: SmolInstant) -> Self {
        Self {
            tcp_to_quic_tx: None,
            quic_to_tcp_rx: None,
            write_buf: VecDeque::new(),
            outcome: Arc::new(AtomicU8::new(RELAY_FAILED)),
            last_activity: now,
            last_state: tcp::State::Closed,
            terminated: true,
        }
    }
}

/// Fix 05-A lifecycle counters, reported by the periodic stats line.
#[derive(Default, Debug, Clone, Copy, PartialEq, Eq)]
struct LifecycleStats {
    fin_on_relay_end: u64,
    rst_on_relay_failure: u64,
    rst_fail_closed: u64,
    backstop_aborts: u64,
    sockets_removed: u64,
}

fn is_closing_state(state: tcp::State) -> bool {
    matches!(
        state,
        tcp::State::FinWait1
            | tcp::State::FinWait2
            | tcp::State::CloseWait
            | tcp::State::Closing
            | tcp::State::LastAck
    )
}

/// The client has closed its transmit half and every byte it sent has been
/// drained from the socket.
fn client_finished_sending(socket: &tcp::Socket) -> bool {
    matches!(
        socket.state(),
        tcp::State::CloseWait
            | tcp::State::LastAck
            | tcp::State::Closing
            | tcp::State::TimeWait
            | tcp::State::Closed
    ) && !socket.can_recv()
}

/// A relay socket can leave the SocketSet once smoltcp no longer needs it:
/// TIME-WAIT (both FINs exchanged), CLOSED with the 4-tuple already
/// forgotten (any pending RST has been dispatched — removing an aborted socket
/// earlier would drop its RST), or LISTEN (a promoted socket that was reset in
/// SYN-RECEIVED reverts to LISTEN; it must not linger as a second listener).
fn relay_socket_removable(socket: &tcp::Socket) -> bool {
    match socket.state() {
        tcp::State::TimeWait | tcp::State::Listen => true,
        tcp::State::Closed => socket.remote_endpoint().is_none(),
        _ => false,
    }
}

/// Drive one relay socket for one poll-loop pass: move bytes both ways and
/// apply the Fix 05-A lifecycle. Returns true when the socket should be removed
/// from the SocketSet.
///
/// Lifecycle:
/// 1. relay ended normally → FIN to the client once pending bytes are queued;
/// 2. relay failed (or died without an outcome) → RST to the client;
/// 3. client reached CLOSE-WAIT → drop the client→relay sender (relay sees EOF);
/// 4. fail-closed flows are aborted at accept (see `run`) and only removed here;
/// 5. back-stop: a closing-state socket idle for `CLOSING_IDLE_LIMIT` is aborted.
fn drive_relay(
    socket: &mut tcp::Socket,
    relay: &mut ActiveRelay,
    now: SmolInstant,
    stats: &mut LifecycleStats,
) -> bool {
    let state = socket.state();
    if state != relay.last_state {
        relay.last_state = state;
        relay.last_activity = now;
    }

    // Client → resource: drain bytes from the TCP socket into the channel;
    // the relay task reads them and writes to the QUIC stream.
    if let Some(tx) = relay.tcp_to_quic_tx.as_ref() {
        while socket.can_recv() {
            let mut buf = vec![0u8; 4096];
            match socket.recv_slice(&mut buf) {
                Ok(0) | Err(_) => break,
                Ok(n) => {
                    relay.last_activity = now;
                    match tx.try_send(buf[..n].to_vec()) {
                        Ok(()) => {}
                        Err(mpsc::error::TrySendError::Full(_)) => {
                            tracing::warn!("flow queue full; closing TCP flow");
                            socket.close();
                            break;
                        }
                        // The relay task is gone; its outcome decides FIN vs
                        // RST below. Nothing more can be delivered.
                        Err(mpsc::error::TrySendError::Closed(_)) => break,
                    }
                }
            }
        }
    }

    // (3) Client closed its side and everything it sent has been handed to the
    // relay: drop the sender so the relay task sees EOF and can finish.
    if relay.tcp_to_quic_tx.is_some() && client_finished_sending(socket) {
        relay.tcp_to_quic_tx = None;
    }
    // With no relay to hand bytes to, discard what the client still sends so a
    // full receive window can never stop smoltcp from accepting the client's
    // FIN. Deliberately NOT counted as activity: a client that keeps sending
    // into a finished flow must not hold the back-stop off forever.
    if relay.tcp_to_quic_tx.is_none() {
        let mut sink = [0u8; 4096];
        while socket.can_recv() {
            match socket.recv_slice(&mut sink) {
                Ok(0) | Err(_) => break,
                Ok(_) => {}
            }
        }
    }

    // Resource → client: flush any previously buffered bytes first,
    // then pull fresh bytes from the relay channel.
    while !relay.write_buf.is_empty() && socket.can_send() {
        let chunk: Vec<u8> = relay.write_buf.drain(..).collect();
        if let Ok(n) = socket.send_slice(&chunk) {
            if n > 0 {
                relay.last_activity = now;
            }
            if n < chunk.len() {
                let pending = chunk.len() - n;
                if relay.write_buf.len() + pending > FLOW_WRITE_BUF_CAP {
                    tracing::warn!("write buffer cap exceeded; closing TCP flow");
                    socket.close();
                    break;
                }
                relay.write_buf.extend(&chunk[n..]);
                break;
            }
        }
    }
    if relay.write_buf.is_empty() {
        if let Some(rx) = relay.quic_to_tcp_rx.as_mut() {
            if socket.may_send() {
                while socket.can_send() {
                    match rx.try_recv() {
                        Ok(data) => {
                            relay.last_activity = now;
                            match socket.send_slice(&data) {
                                Ok(n) if n < data.len() => {
                                    let pending = data.len() - n;
                                    if relay.write_buf.len() + pending > FLOW_WRITE_BUF_CAP {
                                        tracing::warn!(
                                            "write buffer cap exceeded; closing TCP flow"
                                        );
                                        socket.close();
                                        break;
                                    }
                                    relay.write_buf.extend(&data[n..]);
                                    break;
                                }
                                _ => {}
                            }
                        }
                        Err(mpsc::error::TryRecvError::Empty) => break,
                        Err(mpsc::error::TryRecvError::Disconnected) => {
                            relay.quic_to_tcp_rx = None;
                            break;
                        }
                    }
                }
            } else {
                // Our transmit half is already closed (or the socket is gone):
                // nothing more can reach the client. Discard, but keep watching
                // for the relay's end so the lifecycle can complete.
                loop {
                    match rx.try_recv() {
                        Ok(_) => continue,
                        Err(mpsc::error::TryRecvError::Empty) => break,
                        Err(mpsc::error::TryRecvError::Disconnected) => {
                            relay.quic_to_tcp_rx = None;
                            break;
                        }
                    }
                }
            }
        }
    }

    // (1)/(2) The relay task has ended: FIN after a normal end (once every
    // pending byte is in the socket), RST after a failure.
    if !relay.terminated && relay.quic_to_tcp_rx.is_none() {
        if relay.outcome.load(AtomicOrdering::Acquire) == RELAY_ENDED_OK {
            if relay.write_buf.is_empty() {
                socket.close();
                relay.terminated = true;
                stats.fin_on_relay_end += 1;
            }
        } else {
            socket.abort();
            relay.terminated = true;
            stats.rst_on_relay_failure += 1;
        }
        // The client can no longer reach the resource either way.
        relay.tcp_to_quic_tx = None;
    }

    // (5) Back-stop.
    let state = socket.state();
    if is_closing_state(state) && now - relay.last_activity >= CLOSING_IDLE_LIMIT {
        tracing::warn!(
            state = %state,
            idle_secs = (now - relay.last_activity).secs(),
            "net_stack: relay socket stuck in closing state; aborting"
        );
        socket.abort();
        relay.terminated = true;
        relay.tcp_to_quic_tx = None;
        relay.quic_to_tcp_rx = None;
        relay.write_buf.clear();
        stats.backstop_aborts += 1;
    }

    let removable = relay_socket_removable(socket);
    if removable {
        stats.sockets_removed += 1;
    }
    removable
}

/// Everything a relay task needs; handed to the spawner for each tunnelled flow.
struct RelaySpawn {
    transports: Vec<Arc<ClientTransport>>,
    dest: String,
    port: u16,
    tcp_to_quic_rx: mpsc::Receiver<Vec<u8>>,
    quic_to_tcp_tx: mpsc::Sender<Vec<u8>>,
    /// The spawner must store RELAY_ENDED_OK / RELAY_FAILED here BEFORE the last
    /// `quic_to_tcp` sender is dropped.
    outcome: Arc<AtomicU8>,
}

/// Listener + relay-socket bookkeeping for the smoltcp loop (Fix 05-A).
///
/// Shared by `run` and the unit tests so the tests exercise the production
/// accept/lifecycle code against a real smoltcp interface.
struct FlowTable {
    listen_handles: HashMap<(Ipv4Addr, u16), SocketHandle>,
    active_relays: HashMap<SocketHandle, ActiveRelay>,
    stats: LifecycleStats,
}

impl FlowTable {
    fn new(sockets: &mut SocketSet<'_>, resources: &[(Ipv4Addr, u16)]) -> Self {
        let mut listen_handles = HashMap::new();
        for (ip, port) in resources {
            listen_handles.insert((*ip, *port), new_listen_socket(sockets, *port));
        }
        Self {
            listen_handles,
            active_relays: HashMap::new(),
            stats: LifecycleStats::default(),
        }
    }

    /// One pass after `iface.poll`: promote accepted connections (spawn a
    /// relay or fail closed), then drive and reap every relay socket.
    fn service(
        &mut self,
        sockets: &mut SocketSet<'_>,
        now: SmolInstant,
        transports: &watch::Receiver<Arc<TransportMap>>,
        spawn: &mut dyn FnMut(RelaySpawn),
    ) {
        // --- Promote listening sockets that have accepted a connection ---
        //
        // When smoltcp receives a SYN, the socket leaves Listen (is_active).
        // We immediately replace it with a new listener so further
        // connections to the same resource still work.
        let listen_snapshot: Vec<_> = self.listen_handles.iter().map(|(k, v)| (*k, *v)).collect();
        for ((ip, port), handle) in listen_snapshot {
            if !sockets.get_mut::<tcp::Socket>(handle).is_active() {
                continue;
            }
            // Fresh listener for the next connection.
            self.listen_handles
                .insert((ip, port), new_listen_socket(sockets, port));

            let dest = ip.to_string();
            tracing::info!(dest = %dest, port, "new TCP connection");

            match transports_for(transports, (ip, port)) {
                Some(Some(list)) if !list.is_empty() => {
                    // Managed resource, connector online → tunnel via QUIC.
                    // Channel pair that bridges the synchronous smoltcp poll
                    // loop and the async QUIC relay task.
                    let (tcp_to_quic_tx, tcp_to_quic_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
                    let (quic_to_tcp_tx, quic_to_tcp_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
                    let outcome = Arc::new(AtomicU8::new(RELAY_RUNNING));
                    self.active_relays.insert(
                        handle,
                        ActiveRelay::new(tcp_to_quic_tx, quic_to_tcp_rx, outcome.clone(), now),
                    );
                    spawn(RelaySpawn {
                        transports: list,
                        dest,
                        port,
                        tcp_to_quic_rx,
                        quic_to_tcp_tx,
                        outcome,
                    });
                }
                other => {
                    match other {
                        Some(Some(_)) => tracing::warn!(
                            dest = %dest,
                            port,
                            "transport list unexpectedly empty"
                        ),
                        // Managed resource, connector offline → fail closed.
                        Some(None) => {
                            tracing::warn!(dest = %dest, port, "connector offline — failing closed for managed resource")
                        }
                        // The listener set is already built from allowed ACL
                        // entries. Missing transport here means malformed
                        // snapshot/RN data, so fail closed instead of bypassing
                        // a managed destination.
                        None => {
                            tracing::warn!(dest = %dest, port, "no transport for managed resource — failing closed")
                        }
                    }
                    // Fix 05-A (4): abort so the application gets an immediate
                    // RST. Dropping the channels alone never reset anything —
                    // the socket stayed open until the application's own
                    // timeout, and then leaked.
                    sockets.get_mut::<tcp::Socket>(handle).abort();
                    self.active_relays
                        .insert(handle, ActiveRelay::fail_closed(now));
                    self.stats.rst_fail_closed += 1;
                }
            }
        }

        // --- Drive active relay sockets ---
        let active_handles: Vec<_> = self.active_relays.keys().cloned().collect();
        for handle in active_handles {
            let relay = self.active_relays.get_mut(&handle).unwrap();
            let socket = sockets.get_mut::<tcp::Socket>(handle);
            if drive_relay(socket, relay, now, &mut self.stats) {
                self.active_relays.remove(&handle);
                sockets.remove(handle);
            }
        }
    }
}

// --- Main entry point ---

/// Connector transport map keyed by managed resource (ip, port).
///   Some(Some(t)) — managed resource, connector(s) online → tunnel via QUIC
///   Some(None)    — managed resource, no connector       → fail closed
///   None          — not a managed resource               → fail closed
pub type TransportMap = HashMap<(Ipv4Addr, u16), Option<Vec<Arc<ClientTransport>>>>;

/// Fix 01 Phase 2-A: read the CURRENT transport map for a newly accepted flow.
///
/// The map is published by the daemon through a `watch` channel, so a
/// connector-topology change swaps it without restarting the data plane.
/// Called once per accepted connection: the `borrow()` guard is held only for
/// the HashMap lookup + Arc clones (never across an `.await`). Existing flows
/// are unaffected by later swaps — their relay task owns the cloned Vec and,
/// after selection, only the authenticated stream.
fn transports_for(
    transports: &watch::Receiver<Arc<TransportMap>>,
    key: (Ipv4Addr, u16),
) -> Option<Option<Vec<Arc<ClientTransport>>>> {
    transports.borrow().get(&key).cloned()
}

pub async fn run(
    dev: AsyncDevice,
    allowed_entries: Vec<AclEntry>,
    transports: watch::Receiver<Arc<TransportMap>>,
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

    // ONE listening TCP socket per resource, created BEFORE the loop and
    // re-created each time a connection is accepted (see FlowTable).
    let mut flows = FlowTable::new(&mut sockets, &resource_entries);
    let mut stats_last_logged: (usize, usize, LifecycleStats) = (0, 0, LifecycleStats::default());
    let mut stats_next_at = tokio::time::Instant::now() + LIFECYCLE_STATS_INTERVAL;
    let mut relays_warned = false;

    tracing::info!(
        resources = resource_entries.len(),
        "net_stack: smoltcp loop started"
    );

    // Production relay spawner: one tokio task per accepted flow.
    let mut spawn_relay = |spawn: RelaySpawn| {
        let RelaySpawn {
            transports,
            dest,
            port,
            tcp_to_quic_rx,
            quic_to_tcp_tx,
            outcome,
        } = spawn;
        let resync = relay_resync.clone();
        // Keep one sender alive until the outcome is stored, so the poll loop
        // can't observe the disconnect first.
        let outcome_guard = quic_to_tcp_tx.clone();
        tokio::spawn(async move {
            // relay_tcp_to_quic fires `resync` itself at the transport-failure
            // points (open failure and mid-session drop). A normal close does
            // not, so we don't resync on every finished connection.
            let result = relay_tcp_to_quic(
                transports,
                dest,
                port,
                tcp_to_quic_rx,
                quic_to_tcp_tx,
                resync,
            )
            .await;
            // Fix 05-A: record how the relay ended, THEN drop the last sender.
            // The poll loop acts on the outcome only after it sees the channel
            // disconnected, and Release/Acquire orders the store before that.
            // A panic drops the guard with the outcome still RUNNING, which the
            // loop treats as a failure (RST).
            let end = match result {
                Ok(RelayEnd::Normal) => RELAY_ENDED_OK,
                Ok(RelayEnd::Failed) => RELAY_FAILED,
                Err(e) => {
                    tracing::warn!(error = %e, "QUIC relay ended");
                    RELAY_FAILED
                }
            };
            outcome.store(end, AtomicOrdering::Release);
            drop(outcome_guard);
        });
    };

    loop {
        let smol_now = smoltcp_now();
        iface.poll(smol_now, &mut tun_dev, &mut sockets);

        flows.service(&mut sockets, smol_now, &transports, &mut spawn_relay);

        // --- Fix 05-A observability ---
        let n_relays = flows.active_relays.len();
        if n_relays >= ACTIVE_RELAYS_WARN && !relays_warned {
            tracing::warn!(
                active_relays = n_relays,
                sockets = sockets.iter().count(),
                "net_stack: unusually many live relay sockets"
            );
            relays_warned = true;
        } else if n_relays < ACTIVE_RELAYS_WARN / 2 {
            relays_warned = false;
        }
        if tokio::time::Instant::now() >= stats_next_at {
            stats_next_at = tokio::time::Instant::now() + LIFECYCLE_STATS_INTERVAL;
            let n_sockets = sockets.iter().count();
            let stats = flows.stats;
            let snapshot = (n_relays, n_sockets, stats);
            if snapshot != stats_last_logged {
                tracing::info!(
                    active_relays = n_relays,
                    sockets = n_sockets,
                    fin_on_relay_end = stats.fin_on_relay_end,
                    rst_on_relay_failure = stats.rst_on_relay_failure,
                    rst_fail_closed = stats.rst_fail_closed,
                    backstop_aborts = stats.backstop_aborts,
                    sockets_removed = stats.sockets_removed,
                    "net_stack: socket lifecycle stats"
                );
                stats_last_logged = snapshot;
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
///
/// Returns `Err` when no tunnel could be opened (or it was denied),
/// `Ok(RelayEnd::Failed)` on a mid-session transport failure, and
/// `Ok(RelayEnd::Normal)` on a normal close. The poll loop maps these to RST /
/// RST / FIN towards the client (Fix 05-A).
async fn relay_tcp_to_quic(
    transports: Vec<Arc<ClientTransport>>,
    destination: String,
    port: u16,
    mut tcp_to_quic_rx: mpsc::Receiver<Vec<u8>>,
    quic_to_tcp_tx: mpsc::Sender<Vec<u8>>,
    resync: Arc<Notify>,
) -> Result<RelayEnd> {
    let mut selected_stream = None;
    // Whether any failure was a network/transport failure (relay/connector
    // unreachable). Only these warrant an early transport resync; auth failures
    // and connector denials fail closed (finding #8).
    let mut saw_transport_failure = false;

    for transport in transports {
        let (candidate, probe) = match transport.open_authenticated_stream_probed().await {
            Ok(opened) => opened,

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
                // Fix 05-B: evict the pooled connection if its connector went silent.
                transport.report_handshake_stall(probe.as_ref()).await;
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
    // Fix 05-A: the client closed its transmit half (CLOSE-WAIT). Propagate the
    // half-close to the connector but KEEP reading, so a client that does
    // `shutdown(SHUT_WR)` after its request still gets the whole response. The
    // connector side (`copy_bidirectional`) carries the half-close through to
    // the resource.
    let mut client_done = false;
    loop {
        tokio::select! {
            // Client → resource: bytes from the TCP socket go to the QUIC send stream.
            data = tcp_to_quic_rx.recv(), if !client_done => {
                match data {
                    Some(buf) => {
                        if send.write_all(&buf).await.is_err() {
                            relay_failed = true; // QUIC send failed mid-session
                            break;
                        }
                    }
                    None => {
                        // TCP client closed its side (normal half-close).
                        client_done = true;
                        let _ = send.shutdown().await;
                    }
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
            // Fix 05-A: the net_stack side dropped this flow (socket reset by
            // the client, or reaped by the back-stop). Stop instead of waiting
            // on a QUIC read that may never complete.
            _ = quic_to_tcp_tx.closed() => break,
        }
    }

    let _ = send.shutdown().await;
    if relay_failed {
        resync.notify_one();
        return Ok(RelayEnd::Failed);
    }
    Ok(RelayEnd::Normal)
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

    #[test]
    fn connectivity_io_kinds_are_transport_failures() {
        let kinds = [
            std::io::ErrorKind::ConnectionReset,
            std::io::ErrorKind::ConnectionAborted,
            std::io::ErrorKind::BrokenPipe,
            std::io::ErrorKind::UnexpectedEof,
            std::io::ErrorKind::TimedOut,
            std::io::ErrorKind::NotConnected,
        ];

        for kind in kinds {
            let error = FramedJsonError::Io(std::io::Error::from(kind));
            assert!(
                error.is_transport_failure(),
                "{kind:?} must trigger transport recovery"
            );
        }
    }

    #[test]
    fn unrelated_io_kinds_are_not_transport_failures() {
        let kinds = [
            std::io::ErrorKind::InvalidData,
            std::io::ErrorKind::InvalidInput,
            std::io::ErrorKind::PermissionDenied,
            std::io::ErrorKind::WouldBlock,
        ];

        for kind in kinds {
            let error = FramedJsonError::Io(std::io::Error::from(kind));
            assert!(
                !error.is_transport_failure(),
                "{kind:?} must not trigger transport recovery"
            );
        }
    }

    #[test]
    fn certificate_or_spiffe_failure_is_recognized_as_authentication() {
        let error = anyhow::Error::new(TunnelOpenError::Authenticate(anyhow!(
            "SPIFFE identity mismatch"
        )))
        .context("open authenticated stream");

        assert!(is_auth_failure(&error));
    }

    #[test]
    fn connection_failure_is_not_misclassified_as_authentication() {
        let error = anyhow::Error::new(TunnelOpenError::Connect(anyhow!("connection reset")))
            .context("open authenticated stream");

        assert!(!is_auth_failure(&error));
    }

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

    // ---- Fix 01 Phase 2-A: transport-map swap vs flows ----------------------------

    use crate::transport::{ClientTransport, DirectOpener};
    use crate::tunnel_pool::{AuthenticatedStream, TunnelOpenError};
    use std::net::SocketAddr;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use tokio::io::{AsyncReadExt, DuplexStream};

    /// Opener that hands the far end of each opened stream to the test, so the
    /// test plays the connector.
    struct PipeOpener {
        peers: mpsc::UnboundedSender<DuplexStream>,
        opens: Arc<AtomicUsize>,
    }

    #[async_trait::async_trait]
    impl DirectOpener for PipeOpener {
        async fn open(
            &self,
            _addr: SocketAddr,
        ) -> std::result::Result<AuthenticatedStream, TunnelOpenError> {
            self.opens.fetch_add(1, Ordering::SeqCst);
            let (near, far) = duplex(64 * 1024);
            let _ = self.peers.send(far);
            Ok(Box::new(near))
        }
    }

    fn pipe_transport() -> (
        Arc<ClientTransport>,
        mpsc::UnboundedReceiver<DuplexStream>,
        Arc<AtomicUsize>,
    ) {
        let (tx, rx) = mpsc::unbounded_channel();
        let opens = Arc::new(AtomicUsize::new(0));
        let t = Arc::new(ClientTransport::new(
            Arc::new(PipeOpener {
                peers: tx,
                opens: opens.clone(),
            }),
            "127.0.0.1:9092".parse().unwrap(),
            None,
        ));
        (t, rx, opens)
    }

    fn key() -> (Ipv4Addr, u16) {
        ("10.0.0.1".parse().unwrap(), 80)
    }

    /// Connector side of the tunnel handshake: read the request, answer ok.
    async fn accept_tunnel(peer: &mut DuplexStream) {
        let req: serde_json::Value = read_framed_json(peer).await.unwrap();
        assert_eq!(req["destination"], "10.0.0.1");
        let body = br#"{"ok":true,"error":null}"#;
        peer.write_all(&(body.len() as u32).to_be_bytes())
            .await
            .unwrap();
        peer.write_all(body).await.unwrap();
    }

    /// New flows read the CURRENT map: after a swap, the accept path returns
    /// the new connector list (and a resource whose connectors all vanished
    /// reads as Some(None) → fail closed).
    #[test]
    fn new_flows_read_the_latest_published_map() {
        let (a, _ra, _) = pipe_transport();
        let (b, _rb, _) = pipe_transport();
        let mut m1 = TransportMap::new();
        m1.insert(key(), Some(vec![a.clone()]));
        let (tx, rx) = watch::channel(Arc::new(m1));

        let first = transports_for(&rx, key()).unwrap().unwrap();
        assert!(Arc::ptr_eq(&first[0], &a));

        let mut m2 = TransportMap::new();
        m2.insert(key(), Some(vec![b.clone(), a.clone()]));
        tx.send(Arc::new(m2)).unwrap();
        let second = transports_for(&rx, key()).unwrap().unwrap();
        assert_eq!(second.len(), 2);
        assert!(Arc::ptr_eq(&second[0], &b), "new preferred first");

        let mut m3 = TransportMap::new();
        m3.insert(key(), None);
        tx.send(Arc::new(m3)).unwrap();
        assert!(
            matches!(transports_for(&rx, key()), Some(None)),
            "fail closed"
        );

        // The Vec handed to the first flow is unaffected by later swaps.
        assert!(Arc::ptr_eq(&first[0], &a));
        assert_eq!(first.len(), 1);
    }

    /// An established flow keeps relaying in both directions after the
    /// transport map is swapped to one WITHOUT its connector and every map
    /// reference (old map, sender) is dropped: the relay task owns only its
    /// stream, not the map.
    #[tokio::test]
    async fn established_flow_survives_map_swap_that_removes_its_connector() {
        let (manoj, mut manoj_peers, manoj_opens) = pipe_transport();
        let (other, _other_peers, other_opens) = pipe_transport();
        let mut m1 = TransportMap::new();
        m1.insert(key(), Some(vec![manoj.clone()]));
        let (tx, rx) = watch::channel(Arc::new(m1));
        drop(manoj); // only the map holds it now

        // Flow A accepted under map 1.
        let flow_transports = transports_for(&rx, key()).unwrap().unwrap();
        let (tcp_tx, tcp_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
        let (quic_tx, mut quic_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
        let resync = Arc::new(Notify::new());
        let flow = tokio::spawn(relay_tcp_to_quic(
            flow_transports,
            "10.0.0.1".into(),
            80,
            tcp_rx,
            quic_tx,
            resync,
        ));
        let mut peer = manoj_peers.recv().await.unwrap();
        accept_tunnel(&mut peer).await;

        // Swap: the connector disappears; drop the old map and the sender.
        let mut m2 = TransportMap::new();
        m2.insert(key(), Some(vec![other.clone()]));
        tx.send(Arc::new(m2)).unwrap();
        assert!(Arc::ptr_eq(
            &transports_for(&rx, key()).unwrap().unwrap()[0],
            &other
        ));
        drop(tx);
        drop(rx);

        // Flow A still relays both ways.
        tcp_tx.send(b"GET / HTTP/1.1\r\n".to_vec()).await.unwrap();
        let mut buf = [0u8; 16];
        peer.read_exact(&mut buf).await.unwrap();
        assert_eq!(&buf, b"GET / HTTP/1.1\r\n");
        peer.write_all(b"HTTP/1.1 200 OK").await.unwrap();
        assert_eq!(quic_rx.recv().await.unwrap(), b"HTTP/1.1 200 OK".to_vec());
        assert!(!flow.is_finished());

        assert_eq!(manoj_opens.load(Ordering::SeqCst), 1);
        assert_eq!(
            other_opens.load(Ordering::SeqCst),
            0,
            "existing flow not moved"
        );

        // Normal close still ends the task cleanly. Fix 05-A: the client's
        // close is propagated to the connector as a half-close (EOF) and the
        // relay keeps reading until the connector side finishes too.
        drop(tcp_tx);
        let mut rest = Vec::new();
        peer.read_to_end(&mut rest).await.unwrap();
        assert!(rest.is_empty(), "connector sees the client's EOF");
        drop(peer);
        assert_eq!(flow.await.unwrap().unwrap(), RelayEnd::Normal);
    }

    /// Flows A and B on one connector are undisturbed when a second connector
    /// is added; a NEW flow accepted after the swap tries the new order.
    #[tokio::test]
    async fn connector_added_leaves_existing_flows_and_new_flow_uses_new_map() {
        let (manoj, mut manoj_peers, manoj_opens) = pipe_transport();
        let (inkyank, mut ink_peers, ink_opens) = pipe_transport();
        let mut m1 = TransportMap::new();
        m1.insert(key(), Some(vec![manoj.clone()]));
        let (tx, rx) = watch::channel(Arc::new(m1));

        let mut flows = Vec::new();
        for _ in 0..2 {
            let (tcp_tx, tcp_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
            let (quic_tx, quic_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
            let task = tokio::spawn(relay_tcp_to_quic(
                transports_for(&rx, key()).unwrap().unwrap(),
                "10.0.0.1".into(),
                80,
                tcp_rx,
                quic_tx,
                Arc::new(Notify::new()),
            ));
            let mut peer = manoj_peers.recv().await.unwrap();
            accept_tunnel(&mut peer).await;
            flows.push((tcp_tx, quic_rx, peer, task));
        }

        // inkyank added (and preferred) → new map.
        let mut m2 = TransportMap::new();
        m2.insert(key(), Some(vec![inkyank.clone(), manoj.clone()]));
        tx.send(Arc::new(m2)).unwrap();

        for (tcp_tx, quic_rx, peer, task) in flows.iter_mut() {
            tcp_tx.send(b"ping".to_vec()).await.unwrap();
            let mut buf = [0u8; 4];
            peer.read_exact(&mut buf).await.unwrap();
            assert_eq!(&buf, b"ping");
            peer.write_all(b"pong").await.unwrap();
            assert_eq!(quic_rx.recv().await.unwrap(), b"pong".to_vec());
            assert!(!task.is_finished());
        }

        // New flow C after the swap goes to inkyank first.
        let (_c_tcp_tx, c_tcp_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
        let (c_quic_tx, _c_quic_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
        let _c = tokio::spawn(relay_tcp_to_quic(
            transports_for(&rx, key()).unwrap().unwrap(),
            "10.0.0.1".into(),
            80,
            c_tcp_rx,
            c_quic_tx,
            Arc::new(Notify::new()),
        ));
        let mut c_peer = ink_peers.recv().await.unwrap();
        accept_tunnel(&mut c_peer).await;
        assert_eq!(ink_opens.load(Ordering::SeqCst), 1);
        assert_eq!(manoj_opens.load(Ordering::SeqCst), 2, "A and B only");
    }

    // ---- Fix 05-B: handshake-stall hook ---------------------------------------

    use crate::tunnel_pool::PathProbe;
    use std::sync::Mutex as StdMutex;

    const STALL_PROBE: PathProbe = PathProbe {
        stable_id: 7,
        rx_at_open: 42,
    };

    /// A direct opener whose streams never answer (the connector is silent),
    /// that hands out a `PathProbe` and records every stall report. `evicts`
    /// is what the pool would answer (true = connection was silent).
    struct StallOpener {
        evicts: bool,
        opens: Arc<AtomicUsize>,
        reports: Arc<StdMutex<Vec<(SocketAddr, PathProbe)>>>,
        // Far ends kept alive so reads pend (a timeout, not an EOF), unless
        // `eof` is set, in which case the far end is dropped (an I/O failure).
        held: StdMutex<Vec<DuplexStream>>,
        eof: bool,
    }

    #[async_trait::async_trait]
    impl DirectOpener for StallOpener {
        async fn open(
            &self,
            addr: SocketAddr,
        ) -> std::result::Result<AuthenticatedStream, TunnelOpenError> {
            self.open_probed(addr).await.map(|(s, _)| s)
        }
        async fn open_probed(
            &self,
            _addr: SocketAddr,
        ) -> std::result::Result<(AuthenticatedStream, Option<PathProbe>), TunnelOpenError>
        {
            self.opens.fetch_add(1, Ordering::SeqCst);
            let (near, far) = duplex(64 * 1024);
            if !self.eof {
                self.held.lock().unwrap().push(far);
            }
            Ok((Box::new(near), Some(STALL_PROBE)))
        }
        async fn evict_if_silent(&self, addr: SocketAddr, probe: &PathProbe) -> bool {
            self.reports.lock().unwrap().push((addr, *probe));
            self.evicts
        }
    }

    type Reports = Arc<StdMutex<Vec<(SocketAddr, PathProbe)>>>;

    fn stall_transport(
        evicts: bool,
        eof: bool,
    ) -> (Arc<ClientTransport>, Arc<AtomicUsize>, Reports) {
        let opens = Arc::new(AtomicUsize::new(0));
        let reports: Reports = Arc::new(StdMutex::new(Vec::new()));
        let t = Arc::new(ClientTransport::new(
            Arc::new(StallOpener {
                evicts,
                opens: opens.clone(),
                reports: reports.clone(),
                held: StdMutex::new(Vec::new()),
                eof,
            }),
            "127.0.0.2:9092".parse().unwrap(),
            None,
        ));
        (t, opens, reports)
    }

    type FlowHandles = (
        tokio::task::JoinHandle<Result<RelayEnd>>,
        mpsc::Sender<Vec<u8>>,
        mpsc::Receiver<Vec<u8>>,
    );

    fn spawn_flow(transports: Vec<Arc<ClientTransport>>, resync: Arc<Notify>) -> FlowHandles {
        let (tcp_tx, tcp_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
        let (quic_tx, quic_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
        let task = tokio::spawn(relay_tcp_to_quic(
            transports,
            "10.0.0.1".into(),
            80,
            tcp_rx,
            quic_tx,
            resync,
        ));
        (task, tcp_tx, quic_rx)
    }

    async fn notified(resync: &Notify) -> bool {
        tokio::time::timeout(Duration::from_millis(1), resync.notified())
            .await
            .is_ok()
    }

    /// (6) The handshake-timeout arm reports the stall, once, for the
    /// transport and probe it timed out on; the flow still fails and the
    /// early resync is still signalled exactly as before.
    #[tokio::test]
    async fn handshake_timeout_reports_stall_for_the_stalled_transport() {
        let (a, opens, reports) = stall_transport(true, false);
        let resync = Arc::new(Notify::new());
        let (task, _tcp_tx, _quic_rx) = spawn_flow(vec![a], resync.clone());
        let started = tokio::time::Instant::now();
        assert!(
            task.await.unwrap().is_err(),
            "the flow fails (no candidate left)"
        );
        assert!(
            started.elapsed() >= TUNNEL_HANDSHAKE_TIMEOUT,
            "it was the 5 s timeout"
        );
        assert_eq!(opens.load(Ordering::SeqCst), 1);
        assert_eq!(
            *reports.lock().unwrap(),
            vec![("127.0.0.2:9092".parse().unwrap(), STALL_PROBE)],
            "one report, for this transport's address and this stream's probe"
        );
        assert!(
            notified(&resync).await,
            "transport failure still signals the resync"
        );
    }

    /// (6b) Gate 1: only the TIMEOUT reports. A handshake that fails with an
    /// I/O error (the connection is already known dead) does not.
    #[tokio::test]
    async fn handshake_io_failure_does_not_report_stall() {
        let (a, _opens, reports) = stall_transport(true, true);
        let resync = Arc::new(Notify::new());
        let (task, _tcp_tx, _quic_rx) = spawn_flow(vec![a], resync.clone());
        assert!(task.await.unwrap().is_err());
        assert!(
            reports.lock().unwrap().is_empty(),
            "no stall report on I/O failure"
        );
        assert!(notified(&resync).await);
    }

    /// (7) Partial loss: connector A is silent, B is healthy. The first flow
    /// pays one timeout on A, A is evicted and cooled down, the flow
    /// falls through to B and works (no resync, no restart). The next flow
    /// goes straight to B without touching A or waiting 5 s, and the first
    /// flow's B stream is unaffected.
    #[tokio::test]
    async fn partial_loss_evicts_silent_connector_and_falls_through_to_healthy_one() {
        let (a, a_opens, a_reports) = stall_transport(true, false);
        let (b, mut b_peers, b_opens) = pipe_transport();
        let resync = Arc::new(Notify::new());

        let (flow1, tcp1, mut quic1) = spawn_flow(vec![a.clone(), b.clone()], resync.clone());
        let mut peer1 = b_peers.recv().await.unwrap();
        accept_tunnel(&mut peer1).await;
        assert_eq!(a_opens.load(Ordering::SeqCst), 1);
        assert_eq!(
            a_reports.lock().unwrap().len(),
            1,
            "A's stall reported once"
        );
        assert_eq!(b_opens.load(Ordering::SeqCst), 1);

        let started = tokio::time::Instant::now();
        let (flow2, tcp2, mut quic2) = spawn_flow(vec![a.clone(), b.clone()], resync.clone());
        let mut peer2 = b_peers.recv().await.unwrap();
        accept_tunnel(&mut peer2).await;
        assert!(
            started.elapsed() < TUNNEL_HANDSHAKE_TIMEOUT,
            "the second flow did not wait on the evicted connector"
        );
        assert_eq!(a_opens.load(Ordering::SeqCst), 1, "A skipped (cooldown)");
        assert_eq!(a_reports.lock().unwrap().len(), 1);
        assert_eq!(b_opens.load(Ordering::SeqCst), 2);
        assert!(
            !notified(&resync).await,
            "partial loss: no resync, no restart"
        );

        // Both flows relay through B; flow 1's B stream is intact.
        for (tcp, quic, peer) in [
            (&tcp1, &mut quic1, &mut peer1),
            (&tcp2, &mut quic2, &mut peer2),
        ] {
            tcp.send(b"ping".to_vec()).await.unwrap();
            let mut buf = [0u8; 4];
            peer.read_exact(&mut buf).await.unwrap();
            assert_eq!(&buf, b"ping");
            peer.write_all(b"pong").await.unwrap();
            assert_eq!(quic.recv().await.unwrap(), b"pong".to_vec());
        }
        assert!(!flow1.is_finished() && !flow2.is_finished());
    }

    // ---- Fix 05-A: socket lifecycle -----------------------------------------
    //
    // Relay-task side first (relay_tcp_to_quic against a duplex "connector"),
    // then the smoltcp side below.

    async fn opened_relay() -> (
        tokio::task::JoinHandle<Result<RelayEnd>>,
        mpsc::Sender<Vec<u8>>,
        mpsc::Receiver<Vec<u8>>,
        DuplexStream,
    ) {
        let (t, mut peers, _) = pipe_transport();
        let (tcp_tx, tcp_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
        let (quic_tx, quic_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
        let task = tokio::spawn(relay_tcp_to_quic(
            vec![t],
            "10.0.0.1".into(),
            80,
            tcp_rx,
            quic_tx,
            Arc::new(Notify::new()),
        ));
        let mut peer = peers.recv().await.unwrap();
        accept_tunnel(&mut peer).await;
        (task, tcp_tx, quic_rx, peer)
    }

    /// (3, relay side) Client half-close: the connector sees EOF, but the
    /// response that follows is still delivered, then the relay ends Normal.
    #[tokio::test]
    async fn relay_propagates_client_half_close_and_still_delivers_response() {
        let (task, tcp_tx, mut quic_rx, mut peer) = opened_relay().await;
        tcp_tx.send(b"GET /".to_vec()).await.unwrap();
        drop(tcp_tx); // client FIN
        let mut req = Vec::new();
        peer.read_to_end(&mut req).await.unwrap();
        assert_eq!(req, b"GET /", "request then EOF at the connector");

        peer.write_all(b"HTTP/1.1 200 OK").await.unwrap();
        assert_eq!(quic_rx.recv().await.unwrap(), b"HTTP/1.1 200 OK");
        drop(peer); // resource done
        assert_eq!(task.await.unwrap().unwrap(), RelayEnd::Normal);
    }

    /// The relay stops when the net_stack side drops the flow (socket reset or
    /// reaped by the back-stop), even if the connector never sends again.
    #[tokio::test]
    async fn relay_ends_when_net_stack_drops_the_flow() {
        let (task, _tcp_tx, quic_rx, _peer_silent) = opened_relay().await;
        drop(quic_rx);
        let end = tokio::time::timeout(Duration::from_secs(2), task)
            .await
            .expect("relay must not wait on a silent QUIC read")
            .unwrap()
            .unwrap();
        assert_eq!(end, RelayEnd::Normal);
    }

    /// Mid-session transport failure → RelayEnd::Failed (→ RST to the client)
    /// and an early transport resync, exactly as before Fix 05-A.
    #[tokio::test]
    async fn relay_mid_session_failure_reports_failed_and_resyncs() {
        struct BrokenRead;
        impl AsyncRead for BrokenRead {
            fn poll_read(
                self: std::pin::Pin<&mut Self>,
                _cx: &mut std::task::Context<'_>,
                _buf: &mut tokio::io::ReadBuf<'_>,
            ) -> std::task::Poll<std::io::Result<()>> {
                std::task::Poll::Ready(Err(std::io::ErrorKind::ConnectionReset.into()))
            }
        }
        struct BrokenAfterHandshake;
        #[async_trait::async_trait]
        impl DirectOpener for BrokenAfterHandshake {
            async fn open(
                &self,
                _addr: SocketAddr,
            ) -> std::result::Result<AuthenticatedStream, TunnelOpenError> {
                let body = br#"{"ok":true,"error":null}"#;
                let mut reply = (body.len() as u32).to_be_bytes().to_vec();
                reply.extend_from_slice(body);
                let read = tokio::io::AsyncReadExt::chain(std::io::Cursor::new(reply), BrokenRead);
                Ok(Box::new(tokio::io::join(read, tokio::io::sink())))
            }
        }
        let t = Arc::new(ClientTransport::new(
            Arc::new(BrokenAfterHandshake),
            "127.0.0.1:9092".parse().unwrap(),
            None,
        ));
        let (_tcp_tx, tcp_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
        let (quic_tx, _quic_rx) = mpsc::channel::<Vec<u8>>(FLOW_QUEUE_CAP);
        let resync = Arc::new(Notify::new());
        let end = relay_tcp_to_quic(
            vec![t],
            "10.0.0.1".into(),
            80,
            tcp_rx,
            quic_tx,
            resync.clone(),
        )
        .await
        .unwrap();
        assert_eq!(end, RelayEnd::Failed);
        tokio::time::timeout(Duration::from_millis(100), resync.notified())
            .await
            .expect("mid-session failure still signals a transport resync");
    }

    // smoltcp side: a real smoltcp "client" interface is wired to the
    // production FlowTable ("net_stack" side) through an in-memory packet pipe,
    // driven by a fake clock. The relay task is played by the test through the
    // RelaySpawn channels, so every lifecycle edge is deterministic.

    use smoltcp::iface::{Config as IfaceConfig, Interface as Iface};
    use std::cell::RefCell;
    use std::rc::Rc;

    type PacketQueue = Rc<RefCell<VecDeque<Vec<u8>>>>;

    struct PipeDev {
        rx: PacketQueue,
        tx: PacketQueue,
    }
    struct PipeRx(Vec<u8>);
    struct PipeTx(PacketQueue);

    impl RxToken for PipeRx {
        fn consume<R, F: FnOnce(&mut [u8]) -> R>(mut self, f: F) -> R {
            f(&mut self.0)
        }
    }
    impl TxToken for PipeTx {
        fn consume<R, F: FnOnce(&mut [u8]) -> R>(self, len: usize, f: F) -> R {
            let mut buf = vec![0u8; len];
            let r = f(&mut buf);
            self.0.borrow_mut().push_back(buf);
            r
        }
    }
    impl Device for PipeDev {
        type RxToken<'a>
            = PipeRx
        where
            Self: 'a;
        type TxToken<'a>
            = PipeTx
        where
            Self: 'a;
        fn receive(&mut self, _t: SmolInstant) -> Option<(PipeRx, PipeTx)> {
            let pkt = self.rx.borrow_mut().pop_front()?;
            Some((PipeRx(pkt), PipeTx(self.tx.clone())))
        }
        fn transmit(&mut self, _t: SmolInstant) -> Option<PipeTx> {
            Some(PipeTx(self.tx.clone()))
        }
        fn capabilities(&self) -> DeviceCapabilities {
            let mut caps = DeviceCapabilities::default();
            caps.medium = Medium::Ip;
            caps.max_transmission_unit = 1500;
            caps
        }
    }

    const RES_IP: [u8; 4] = [10, 0, 0, 1];
    const RES_PORT: u16 = 80;

    /// Client app + production FlowTable over a packet pipe.
    struct Lab {
        now_ms: i64,
        // net_stack side (production code under test)
        s_dev: PipeDev,
        s_iface: Iface,
        s_sockets: SocketSet<'static>,
        flows: FlowTable,
        transports: watch::Receiver<Arc<TransportMap>>,
        _transports_tx: watch::Sender<Arc<TransportMap>>,
        spawned: Vec<RelaySpawn>,
        // application side
        c_dev: PipeDev,
        c_iface: Iface,
        c_sockets: SocketSet<'static>,
    }

    impl Lab {
        /// `online`: resource has a connector (tunnel) vs. Some(None) (fail closed).
        fn new(online: bool) -> Self {
            let a: PacketQueue = Rc::new(RefCell::new(VecDeque::new()));
            let b: PacketQueue = Rc::new(RefCell::new(VecDeque::new()));
            let mut s_dev = PipeDev {
                rx: a.clone(),
                tx: b.clone(),
            };
            let mut c_dev = PipeDev { rx: b, tx: a };
            let now = SmolInstant::from_millis(1_000);

            let mut cfg = IfaceConfig::new(HardwareAddress::Ip);
            cfg.random_seed = 1;
            let mut s_iface = Iface::new(cfg, &mut s_dev, now);
            s_iface.update_ip_addrs(|a| {
                a.push(IpCidr::new(IpAddress::v4(10, 0, 0, 1), 24)).unwrap();
            });
            let mut cfg = IfaceConfig::new(HardwareAddress::Ip);
            cfg.random_seed = 2;
            let mut c_iface = Iface::new(cfg, &mut c_dev, now);
            c_iface.update_ip_addrs(|a| {
                a.push(IpCidr::new(IpAddress::v4(10, 0, 0, 2), 24)).unwrap();
            });

            let key = (Ipv4Addr::from(RES_IP), RES_PORT);
            let mut map = TransportMap::new();
            if online {
                map.insert(key, Some(vec![pipe_transport().0]));
            } else {
                map.insert(key, None);
            }
            let (tx, rx) = watch::channel(Arc::new(map));

            let mut s_sockets = SocketSet::new(vec![]);
            let flows = FlowTable::new(&mut s_sockets, &[key]);
            Self {
                now_ms: 1_000,
                s_dev,
                s_iface,
                s_sockets,
                flows,
                transports: rx,
                _transports_tx: tx,
                spawned: Vec::new(),
                c_dev,
                c_iface,
                c_sockets: SocketSet::new(vec![]),
            }
        }

        fn now(&self) -> SmolInstant {
            SmolInstant::from_millis(self.now_ms)
        }

        /// One round: both stacks poll, the FlowTable services its sockets.
        fn step(&mut self, advance_ms: i64) {
            self.now_ms += advance_ms;
            let now = self.now();
            self.c_iface.poll(now, &mut self.c_dev, &mut self.c_sockets);
            self.s_iface.poll(now, &mut self.s_dev, &mut self.s_sockets);
            let spawned = &mut self.spawned;
            self.flows
                .service(&mut self.s_sockets, now, &self.transports, &mut |s| {
                    spawned.push(s)
                });
            self.s_iface.poll(now, &mut self.s_dev, &mut self.s_sockets);
            self.c_iface.poll(now, &mut self.c_dev, &mut self.c_sockets);
        }

        fn pump(&mut self, rounds: usize) {
            for _ in 0..rounds {
                self.step(10);
            }
        }

        /// Open an application connection from `local_port`.
        fn connect(&mut self, local_port: u16) -> SocketHandle {
            let mut s = tcp::Socket::new(
                tcp::SocketBuffer::new(vec![0u8; 256 * 1024]),
                tcp::SocketBuffer::new(vec![0u8; 64 * 1024]),
            );
            s.connect(
                self.c_iface.context(),
                (
                    IpAddress::v4(RES_IP[0], RES_IP[1], RES_IP[2], RES_IP[3]),
                    RES_PORT,
                ),
                local_port,
            )
            .unwrap();
            self.c_sockets.add(s)
        }

        fn app(&mut self, h: SocketHandle) -> &mut tcp::Socket<'static> {
            self.c_sockets.get_mut::<tcp::Socket>(h)
        }

        fn app_read_all(&mut self, h: SocketHandle) -> Vec<u8> {
            let mut out = Vec::new();
            let s = self.app(h);
            while s.can_recv() {
                let mut buf = [0u8; 8192];
                match s.recv_slice(&mut buf) {
                    Ok(0) | Err(_) => break,
                    Ok(n) => out.extend_from_slice(&buf[..n]),
                }
            }
            out
        }

        /// Relay sockets the FlowTable still holds (listeners excluded).
        fn relay_sockets(&self) -> usize {
            self.flows.active_relays.len()
        }

        /// Total smoltcp sockets on the net_stack side (listeners included).
        fn stack_sockets(&self) -> usize {
            self.s_sockets.iter().count()
        }

        fn relay_socket_state(&mut self) -> tcp::State {
            let h = *self
                .flows
                .active_relays
                .keys()
                .next()
                .expect("a relay socket");
            self.s_sockets.get_mut::<tcp::Socket>(h).state()
        }
    }

    /// Play the relay task: end it with `outcome`, then drop its channels
    /// (outcome stored BEFORE the sender is dropped, as the production spawner does).
    fn end_relay(spawn: RelaySpawn, outcome: u8) {
        spawn.outcome.store(outcome, AtomicOrdering::Release);
        drop(spawn);
    }

    /// Establish one flow, exchange a request, return the app handle and the
    /// relay-task side.
    fn establish(lab: &mut Lab, local_port: u16) -> (SocketHandle, RelaySpawn) {
        let h = lab.connect(local_port);
        lab.pump(5);
        assert_eq!(lab.app(h).state(), tcp::State::Established, "handshake");
        let mut spawn = lab.spawned.pop().expect("relay spawned for the flow");
        lab.app(h).send_slice(b"GET / HTTP/1.1\r\n\r\n").unwrap();
        lab.pump(3);
        assert_eq!(
            spawn.tcp_to_quic_rx.try_recv().unwrap(),
            b"GET / HTTP/1.1\r\n\r\n"
        );
        (h, spawn)
    }

    /// (1) Relay ends normally → the app gets every byte, then a FIN; after
    /// the app closes, the net_stack socket is removed (no CLOSE-WAIT leak).
    /// The response is larger than the smoltcp tx buffer, so the FIN must wait
    /// for the overflow buffer to drain — nothing may be truncated.
    #[test]
    fn relay_normal_end_sends_fin_after_all_data_and_socket_is_removed() {
        let mut lab = Lab::new(true);
        let (h, spawn) = establish(&mut lab, 40_001);

        let body: Vec<u8> = (0..200_000u32).map(|i| (i % 251) as u8).collect();
        let mut got = Vec::new();
        for chunk in body.chunks(16 * 1024) {
            // Respect the bounded channel like the real relay (send().await).
            while spawn.quic_to_tcp_tx.try_send(chunk.to_vec()).is_err() {
                lab.step(10);
                got.extend(lab.app_read_all(h));
            }
            lab.step(10);
            got.extend(lab.app_read_all(h));
        }
        end_relay(spawn, RELAY_ENDED_OK);
        for _ in 0..200 {
            lab.step(10);
            got.extend(lab.app_read_all(h));
            if lab.app(h).state() == tcp::State::CloseWait {
                break;
            }
        }
        assert_eq!(got.len(), body.len(), "every byte delivered before FIN");
        assert_eq!(got, body);
        assert_eq!(lab.app(h).state(), tcp::State::CloseWait, "app saw our FIN");
        assert_eq!(lab.flows.stats.fin_on_relay_end, 1);

        lab.app(h).close();
        lab.pump(10);
        assert_eq!(lab.relay_sockets(), 0, "relay socket removed");
        assert_eq!(lab.stack_sockets(), 1, "only the listener is left");
        assert!(matches!(
            lab.app(h).state(),
            tcp::State::Closed | tcp::State::TimeWait
        ));
    }

    /// (2) Relay fails mid-session → the app gets an RST at once (not a FIN,
    /// which would look like a clean, complete response).
    #[test]
    fn relay_failure_resets_the_application_connection() {
        let mut lab = Lab::new(true);
        let (h, spawn) = establish(&mut lab, 40_002);
        end_relay(spawn, RELAY_FAILED);
        lab.pump(3);
        assert_eq!(lab.app(h).state(), tcp::State::Closed, "RST received");
        assert_eq!(lab.flows.stats.rst_on_relay_failure, 1);
        assert_eq!(lab.flows.stats.fin_on_relay_end, 0);
        assert_eq!(lab.relay_sockets(), 0);
        assert_eq!(lab.stack_sockets(), 1);
    }

    /// A relay task that dies without recording an outcome (e.g. a panic)
    /// is treated as a failure: RST, never a clean FIN.
    #[test]
    fn relay_ending_without_outcome_is_treated_as_failure() {
        let mut lab = Lab::new(true);
        let (h, spawn) = establish(&mut lab, 40_003);
        drop(spawn); // outcome still RELAY_RUNNING
        lab.pump(3);
        assert_eq!(lab.app(h).state(), tcp::State::Closed);
        assert_eq!(lab.flows.stats.rst_on_relay_failure, 1);
        assert_eq!(lab.relay_sockets(), 0);
    }

    /// (3) App closes first (curl, browsers) → net_stack socket reaches
    /// CLOSE-WAIT → the client→relay channel is closed so the relay sees EOF
    /// (after every byte the app sent) → relay ends → FIN → socket removed.
    #[test]
    fn app_close_reaches_close_wait_closes_relay_channel_and_cleans_up() {
        let mut lab = Lab::new(true);
        let (h, mut spawn) = establish(&mut lab, 40_004);

        lab.app(h).send_slice(b"tail").unwrap();
        lab.app(h).close();
        lab.pump(3);
        assert_eq!(lab.relay_socket_state(), tcp::State::CloseWait);
        assert_eq!(
            spawn.tcp_to_quic_rx.try_recv().unwrap(),
            b"tail",
            "no byte lost"
        );
        assert!(
            matches!(
                spawn.tcp_to_quic_rx.try_recv(),
                Err(mpsc::error::TryRecvError::Disconnected)
            ),
            "relay sees client EOF"
        );

        end_relay(spawn, RELAY_ENDED_OK);
        lab.pump(5);
        assert_eq!(lab.relay_sockets(), 0, "no CLOSE-WAIT socket left behind");
        assert_eq!(lab.stack_sockets(), 1);
        assert!(matches!(
            lab.app(h).state(),
            tcp::State::TimeWait | tcp::State::Closed
        ));
    }

    /// (4) Fail-closed (connector offline): the app is reset within a few
    /// polls instead of hanging until its own timeout, and nothing leaks.
    #[test]
    fn fail_closed_resets_the_application_promptly() {
        let mut lab = Lab::new(false);
        let h = lab.connect(40_005);
        lab.pump(4);
        assert!(
            lab.spawned.is_empty(),
            "no relay for a fail-closed resource"
        );
        assert_eq!(lab.app(h).state(), tcp::State::Closed, "RST, not a hang");
        assert_eq!(lab.flows.stats.rst_fail_closed, 1);
        assert_eq!(lab.relay_sockets(), 0);
        assert_eq!(lab.stack_sockets(), 1, "only the listener is left");
    }

    /// (5) Back-stop: a socket stuck in a closing state (here CLOSE-WAIT with a
    /// relay task that never finishes) is aborted after CLOSING_IDLE_LIMIT,
    /// and not before.
    #[test]
    fn backstop_aborts_socket_stuck_in_closing_state() {
        let mut lab = Lab::new(true);
        let (h, _spawn_never_ends) = establish(&mut lab, 40_006);
        lab.app(h).close();
        lab.pump(3);
        assert_eq!(lab.relay_socket_state(), tcp::State::CloseWait);

        lab.step(59_000);
        assert_eq!(lab.relay_sockets(), 1, "not before the limit");
        lab.step(1_500);
        lab.pump(2);
        assert_eq!(lab.flows.stats.backstop_aborts, 1);
        assert_eq!(lab.relay_sockets(), 0);
        assert_eq!(lab.stack_sockets(), 1);
    }

    /// The back-stop never touches a healthy idle ESTABLISHED flow.
    #[test]
    fn idle_established_flow_is_not_touched_by_backstop() {
        let mut lab = Lab::new(true);
        let (h, spawn) = establish(&mut lab, 40_007);
        lab.step(10 * 60_000);
        lab.pump(2);
        assert_eq!(lab.relay_socket_state(), tcp::State::Established);
        assert_eq!(lab.flows.stats.backstop_aborts, 0);

        // Still relays both ways.
        spawn.quic_to_tcp_tx.try_send(b"pong".to_vec()).unwrap();
        lab.pump(3);
        assert_eq!(lab.app_read_all(h), b"pong");
        lab.app(h).send_slice(b"ping").unwrap();
        lab.pump(3);
        let mut spawn = spawn;
        assert_eq!(spawn.tcp_to_quic_rx.try_recv().unwrap(), b"ping");
    }

    /// Regression for the live finding (Fix05 §1): after a completed flow, a
    /// NEW connection from the SAME source port must be accepted. Before Fix
    /// 05-A the old socket stayed in CLOSE-WAIT holding the 4-tuple, the new
    /// SYN matched it instead of the listener, and the app hung until its own
    /// timeout (`curl` 000 after 8 s).
    #[test]
    fn source_port_reuse_after_completed_flow_is_accepted() {
        let mut lab = Lab::new(true);
        let port = 47_123;
        for round in 0..5 {
            let (h, spawn) = establish(&mut lab, port);
            lab.app(h).close(); // app closes first, like curl
            lab.pump(3);
            end_relay(spawn, RELAY_ENDED_OK);
            lab.pump(5);
            assert_eq!(lab.relay_sockets(), 0, "round {round}: socket reaped");
            // The OS frees the 4-tuple; the next connection reuses the port.
            lab.c_sockets.remove(h);
            lab.pump(1);
        }
        assert_eq!(lab.flows.stats.fin_on_relay_end, 5);
        assert_eq!(lab.stack_sockets(), 1, "bounded: only the listener");
    }

    /// Control for the regression: while a socket IS stuck holding the
    /// 4-tuple, a reused source port is NOT accepted (the exact pre-fix
    /// mechanism) — and the back-stop clears it so the port works again.
    #[test]
    fn stuck_socket_blocks_source_port_reuse_until_backstop_clears_it() {
        let mut lab = Lab::new(true);
        let port = 47_555;
        let (h, _never_ends) = establish(&mut lab, port);
        lab.app(h).close();
        lab.pump(3);
        lab.c_sockets.remove(h);

        let h2 = lab.connect(port);
        lab.pump(10);
        assert!(lab.spawned.is_empty(), "SYN captured by the stuck socket");
        assert_ne!(lab.app(h2).state(), tcp::State::Established);
        lab.c_sockets.remove(h2);

        lab.step(61_000);
        lab.pump(2);
        assert_eq!(lab.flows.stats.backstop_aborts, 1);
        let (_h3, _s3) = establish(&mut lab, port);
    }
}
