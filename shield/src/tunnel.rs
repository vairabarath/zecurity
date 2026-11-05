//! Shield side of Shield-routed tunnels.
//!
//! FU-9 (PENDING-21): the Connector asks for a tunnel with `TunnelOpen` on
//! Control. The Shield connects to the resource, then opens a dedicated
//! `ShieldService.Tunnel` stream (first frame `TunnelStart`) and relays the
//! tunnel on it. HTTP/2 flow control is per stream, so a slow resource or a
//! slow device holds up only its own tunnel: there is no stall cutoff and no
//! unbounded queue. A tunnel that could not be opened is reported on Control
//! (`TunnelOpened{ok: false}`); the Connector aborts a tunnel on Control
//! (`TunnelClose`) or by ending its stream with an error.

use std::collections::HashMap;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpStream, UdpSocket};
use tokio::sync::{mpsc, watch};
use tokio::time::{timeout, Instant};
use tokio_stream::wrappers::ReceiverStream;
use tonic::transport::Channel;
use tonic::{Request, Streaming};
use tracing::{debug, warn};

use crate::proto::shield_control_message::Body;
use crate::proto::shield_service_client::ShieldServiceClient;
use crate::proto::tunnel_frame::Frame;
use crate::proto::{
    ShieldControlMessage, TunnelEnd, TunnelFrame, TunnelOpen, TunnelOpened, TunnelStart,
};

/// D-3: tunnels (active plus pending) this Shield runs at once. Must match the
/// Connector's `MAX_TUNNELS_PER_SHIELD`; the HTTP/2 windows in `tls.rs` are
/// sized for it.
pub const MAX_TUNNELS: usize = 256;
const MAX_CHUNK: usize = 16 * 1024;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
/// How long opening the tunnel's stream may take (an HTTP/2 stream limit
/// would queue it silently; this bounds it).
const STREAM_OPEN_TIMEOUT: Duration = Duration::from_secs(5);
const UDP_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
/// Frames queued toward the Connector beyond HTTP/2's own window. TCP awaits
/// room (backpressure); UDP drops the newest datagram when full (D-4).
const TCP_TO_CONNECTOR_QUEUE: usize = 4;
const UDP_TO_CONNECTOR_QUEUE: usize = 32;
/// Fix 14 handover: how long a refusal waits for an upstream (a Control
/// stream) while the Shield moves from one stream to the next; also how long
/// the old stream may take to drain.
pub const HANDOVER_GRACE: Duration = Duration::from_secs(15);

type Client = ShieldServiceClient<Channel>;

/// The Shield's tunnels. Cloning is cheap (Arc reference counts).
#[derive(Clone)]
pub struct TunnelHub {
    /// connection_id → the tunnel's abort switch.
    tunnels: Arc<Mutex<HashMap<String, watch::Sender<bool>>>>,
    /// Tunnels admitted and not yet ended (pending ones included).
    count: Arc<AtomicUsize>,
    /// The current Control stream, for reporting tunnels that could not be
    /// opened. None (paused) during a renewal handover.
    upstream: Arc<watch::Sender<Option<mpsc::Sender<ShieldControlMessage>>>>,
    /// The client new tunnel streams are opened on: the newest connection.
    /// A tunnel takes a clone only to open its stream and drops it at once,
    /// so an old connection lives only as long as its streams (PENDING-21 §5
    /// Experiment 2).
    client: Arc<watch::Sender<Option<Client>>>,
}

pub fn new_hub() -> TunnelHub {
    TunnelHub {
        tunnels: Arc::new(Mutex::new(HashMap::new())),
        count: Arc::new(AtomicUsize::new(0)),
        upstream: Arc::new(watch::channel(None).0),
        client: Arc::new(watch::channel(None).0),
    }
}

/// A place in the tunnel budget, freed when dropped.
struct Permit(Arc<AtomicUsize>);

impl Drop for Permit {
    fn drop(&mut self) {
        self.0.fetch_sub(1, Ordering::SeqCst);
    }
}

impl TunnelHub {
    /// Point refusals at `upstream`, or pause them (None).
    pub fn set_upstream(&self, upstream: Option<mpsc::Sender<ShieldControlMessage>>) {
        self.upstream.send_replace(upstream);
    }

    /// Open new tunnel streams on `client` (None: no new tunnels).
    pub fn set_client(&self, client: Option<Client>) {
        self.client.send_replace(client);
    }

    /// Tunnels admitted and not yet ended.
    pub fn len(&self) -> usize {
        self.count.load(Ordering::SeqCst)
    }

    fn try_admit(&self) -> Option<Permit> {
        self.count
            .fetch_update(Ordering::SeqCst, Ordering::SeqCst, |n| {
                (n < MAX_TUNNELS).then_some(n + 1)
            })
            .ok()
            .map(|_| Permit(self.count.clone()))
    }

    /// Send `msg` on the current upstream. While paused, or if the stream
    /// behind it closed, wait for the next one; gives up after HANDOVER_GRACE.
    async fn send_upstream(&self, mut msg: ShieldControlMessage) -> bool {
        let deadline = Instant::now() + HANDOVER_GRACE;
        let mut rx = self.upstream.subscribe();
        loop {
            let upstream = rx.borrow_and_update().clone();
            if let Some(tx) = upstream {
                match tx.send(msg).await {
                    Ok(()) => return true,
                    Err(mpsc::error::SendError(m)) => msg = m,
                }
            }
            match tokio::time::timeout_at(deadline, rx.changed()).await {
                Ok(Ok(())) => {}
                Ok(Err(_)) | Err(_) => return false,
            }
        }
    }

    /// Abort every tunnel (Fix 14, F44): the Control session they belong to
    /// ended other than by a handover. Returns how many were aborted.
    pub fn close_all(&self) -> usize {
        let tunnels: Vec<_> = self.tunnels.lock().unwrap().drain().collect();
        for (_, abort) in &tunnels {
            abort.send_replace(true);
        }
        tunnels.len()
    }
}

/// Report a tunnel this Shield won't or couldn't open (D21-R D, limits,
/// connect failures).
pub async fn refuse_tunnel_open(hub: &TunnelHub, connection_id: &str, reason: &str) {
    hub.send_upstream(ShieldControlMessage {
        body: Some(Body::TunnelOpened(TunnelOpened {
            connection_id: connection_id.to_string(),
            ok: false,
            error: reason.to_string(),
        })),
    })
    .await;
}

/// The Connector aborted `connection_id` (FU-15 revocation, its session
/// ended): close the resource socket at once.
pub fn abort_tunnel(hub: &TunnelHub, connection_id: &str) {
    if let Some(abort) = hub.tunnels.lock().unwrap().remove(connection_id) {
        abort.send_replace(true);
    }
}

/// Start a tunnel for an admitted `TunnelOpen` (the caller has checked the
/// CRL and the resource allowlist). Refused at once, on Control, when the
/// Shield already runs MAX_TUNNELS tunnels or the id is in use.
pub async fn handle_tunnel_open(hub: TunnelHub, open: TunnelOpen) {
    let Some(permit) = hub.try_admit() else {
        warn!(connection_id = %open.connection_id, "refused tunnel: shield tunnel limit reached");
        refuse_tunnel_open(
            &hub,
            &open.connection_id,
            &format!("shield tunnel limit reached ({MAX_TUNNELS} per shield)"),
        )
        .await;
        return;
    };
    let (abort_tx, abort_rx) = watch::channel(false);
    let in_use = {
        let mut tunnels = hub.tunnels.lock().unwrap();
        let in_use = tunnels.contains_key(&open.connection_id);
        if !in_use {
            tunnels.insert(open.connection_id.clone(), abort_tx.clone());
        }
        in_use
    };
    if in_use {
        drop(permit);
        refuse_tunnel_open(&hub, &open.connection_id, "tunnel id already in use").await;
        return;
    }
    tokio::spawn(async move {
        let _permit = permit;
        let connection_id = open.connection_id.clone();
        run_tunnel(&hub, open, abort_rx).await;
        let mut tunnels = hub.tunnels.lock().unwrap();
        if tunnels
            .get(&connection_id)
            .is_some_and(|tx| tx.same_channel(&abort_tx))
        {
            tunnels.remove(&connection_id);
        }
    });
}

enum Resource {
    Tcp(TcpStream),
    Udp(UdpSocket),
}

async fn connect_resource(open: &TunnelOpen) -> Result<Resource, String> {
    let addr = format!("{}:{}", open.destination, open.port);
    if open.protocol.eq_ignore_ascii_case("udp") {
        let socket = UdpSocket::bind("0.0.0.0:0")
            .await
            .map_err(|e| e.to_string())?;
        socket.connect(&addr).await.map_err(|e| e.to_string())?;
        Ok(Resource::Udp(socket))
    } else {
        match timeout(CONNECT_TIMEOUT, TcpStream::connect(&addr)).await {
            Ok(Ok(s)) => Ok(Resource::Tcp(s)),
            Ok(Err(e)) => Err(e.to_string()),
            Err(_) => Err("connect timeout".to_string()),
        }
    }
}

/// Open the tunnel's stream on the current client. The client clone is
/// dropped before this returns: a tunnel never keeps a client handle.
async fn open_stream(
    hub: &TunnelHub,
    connection_id: &str,
    queue: usize,
) -> Result<(mpsc::Sender<TunnelFrame>, Streaming<TunnelFrame>), String> {
    let Some(mut client) = hub.client.borrow().clone() else {
        return Err("shield has no connector connection".to_string());
    };
    let (tx, rx) = mpsc::channel(queue);
    tx.send(TunnelFrame {
        frame: Some(Frame::Start(TunnelStart {
            connection_id: connection_id.to_string(),
        })),
    })
    .await
    .expect("the receiver is held here");
    let opened = timeout(
        STREAM_OPEN_TIMEOUT,
        client.tunnel(Request::new(ReceiverStream::new(rx))),
    )
    .await;
    drop(client);
    match opened {
        Ok(Ok(response)) => Ok((tx, response.into_inner())),
        Ok(Err(status)) if status.code() == tonic::Code::Unimplemented => {
            Err("connector does not support dedicated tunnel streams; upgrade the connector".into())
        }
        Ok(Err(status)) => Err(format!("tunnel stream refused: {}", status.message())),
        Err(_) => Err("tunnel stream not opened in time".to_string()),
    }
}

async fn run_tunnel(hub: &TunnelHub, open: TunnelOpen, mut abort: watch::Receiver<bool>) {
    let connection_id = open.connection_id.clone();
    let resource = tokio::select! {
        _ = aborted(&mut abort) => return,
        resource = connect_resource(&open) => resource,
    };
    let resource = match resource {
        Ok(resource) => resource,
        Err(e) => {
            debug!(connection_id = %connection_id, error = %e, "tunnel not opened");
            refuse_tunnel_open(hub, &connection_id, &e).await;
            return;
        }
    };
    let queue = match resource {
        Resource::Tcp(_) => TCP_TO_CONNECTOR_QUEUE,
        Resource::Udp(_) => UDP_TO_CONNECTOR_QUEUE,
    };
    let opened = tokio::select! {
        _ = aborted(&mut abort) => {
            // Aborted before the stream was up: reset the resource connection.
            if let Resource::Tcp(stream) = &resource {
                let _ = stream.set_zero_linger();
            }
            return;
        }
        opened = open_stream(hub, &connection_id, queue) => opened,
    };
    let (tx, inbound) = match opened {
        Ok(opened) => opened,
        Err(e) => {
            debug!(connection_id = %connection_id, error = %e, "tunnel not opened");
            if let Resource::Tcp(stream) = &resource {
                let _ = stream.set_zero_linger();
            }
            refuse_tunnel_open(hub, &connection_id, &e).await;
            return;
        }
    };
    match resource {
        Resource::Tcp(stream) => relay_tcp(&connection_id, stream, tx, inbound, abort).await,
        Resource::Udp(socket) => relay_udp(&connection_id, socket, tx, inbound, abort).await,
    }
}

/// Resolves once the tunnel is aborted (never, if its switch is gone).
async fn aborted(abort: &mut watch::Receiver<bool>) {
    if abort.wait_for(|a| *a).await.is_err() {
        std::future::pending::<()>().await;
    }
}

fn data_frame(d: Vec<u8>) -> TunnelFrame {
    TunnelFrame {
        frame: Some(Frame::Data(d)),
    }
}

fn end_frame(error: String) -> TunnelFrame {
    TunnelFrame {
        frame: Some(Frame::End(TunnelEnd { error })),
    }
}

/// TCP: each direction awaits room (HTTP/2 backpressure, then the resource
/// socket's own). A clean end half-closes; an error or abort resets the
/// resource connection.
async fn relay_tcp(
    connection_id: &str,
    stream: TcpStream,
    tx: mpsc::Sender<TunnelFrame>,
    mut inbound: Streaming<TunnelFrame>,
    mut abort: watch::Receiver<bool>,
) {
    let (mut rr, mut rw) = stream.into_split();
    // Resource → Connector; ends by sending TunnelEnd (and dropping `tx`).
    let r2c = async move {
        let mut buf = vec![0u8; MAX_CHUNK];
        loop {
            match rr.read(&mut buf).await {
                Ok(0) => {
                    let _ = tx.send(end_frame(String::new())).await;
                    break;
                }
                Ok(n) => {
                    if tx.send(data_frame(buf[..n].to_vec())).await.is_err() {
                        break;
                    }
                }
                Err(e) => {
                    let _ = tx.send(end_frame(e.to_string())).await;
                    break;
                }
            }
        }
        rr
    };
    // Connector → resource; Ok when the Connector ended cleanly.
    let c2r = async {
        loop {
            match inbound.message().await {
                Ok(Some(TunnelFrame {
                    frame: Some(Frame::Data(d)),
                })) => {
                    if rw.write_all(&d).await.is_err() {
                        return Err(());
                    }
                }
                Ok(Some(TunnelFrame {
                    frame: Some(Frame::End(TunnelEnd { error })),
                })) if error.is_empty() => break,
                Ok(None) => break,
                Ok(Some(other)) => {
                    warn!(connection_id, frame = ?other.frame, "tunnel ended by the connector");
                    return Err(());
                }
                Err(status) => {
                    debug!(connection_id, error = %status, "tunnel stream ended with an error");
                    return Err(());
                }
            }
        }
        let _ = rw.shutdown().await;
        Ok(())
    };
    let mut r2c = Box::pin(r2c);
    let mut c2r = Box::pin(c2r);
    let mut read_done = false;
    let mut write_done = false;
    let failed = loop {
        tokio::select! {
            _ = aborted(&mut abort) => break true,
            _ = &mut r2c, if !read_done => {
                read_done = true;
                if write_done {
                    break false;
                }
            }
            res = &mut c2r, if !write_done => match res {
                Ok(()) => {
                    write_done = true;
                    if read_done {
                        break false;
                    }
                }
                Err(()) => break true,
            },
        }
    };
    drop(c2r);
    if failed {
        // Reset the resource connection rather than end it normally: an
        // aborted tunnel must not look complete to the resource. The write
        // half must not send its FIN on drop (`forget`); the socket closes,
        // with an RST, when the read half goes too.
        let _ = rw.as_ref().set_zero_linger();
        rw.forget();
    }
    drop(r2c);
}

/// UDP: one frame per datagram. Toward the Connector the queue is bounded
/// and the newest datagram is dropped when it is full (D-4); 30 s without a
/// datagram from the resource ends the tunnel.
async fn relay_udp(
    connection_id: &str,
    socket: UdpSocket,
    tx: mpsc::Sender<TunnelFrame>,
    mut inbound: Streaming<TunnelFrame>,
    mut abort: watch::Receiver<bool>,
) {
    let socket = Arc::new(socket);
    let r2c = |tx: mpsc::Sender<TunnelFrame>| {
        let socket = socket.clone();
        async move {
            let mut buf = vec![0u8; 65_535];
            let mut dropped: u64 = 0;
            let mut last_log: Option<Instant> = None;
            loop {
                match timeout(UDP_IDLE_TIMEOUT, socket.recv(&mut buf)).await {
                    Ok(Ok(n)) => {
                        match tx.try_send(data_frame(buf[..n].to_vec())) {
                            Ok(()) => {}
                            Err(mpsc::error::TrySendError::Full(_)) => {
                                dropped += 1;
                                let now = Instant::now();
                                if last_log
                                    .is_none_or(|t| now.duration_since(t) >= Duration::from_secs(1))
                                {
                                    last_log = Some(now);
                                    warn!(connection_id, dropped, "udp tunnel queue to the connector full: dropping datagrams");
                                }
                            }
                            Err(mpsc::error::TrySendError::Closed(_)) => return,
                        }
                    }
                    // Idle or a socket error: end the tunnel.
                    _ => {
                        let _ = tx.send(end_frame(String::new())).await;
                        return;
                    }
                }
            }
        }
    };
    let end_tx = tx.clone();
    let c2r = async {
        loop {
            match inbound.message().await {
                Ok(Some(TunnelFrame {
                    frame: Some(Frame::Data(d)),
                })) => {
                    let _ = socket.send(&d).await;
                }
                // The device side ended: end ours too.
                Ok(Some(TunnelFrame {
                    frame: Some(Frame::End(TunnelEnd { error })),
                })) if error.is_empty() => {
                    let _ = end_tx.send(end_frame(String::new())).await;
                    return;
                }
                Ok(None) => return,
                Ok(Some(_)) | Err(_) => return,
            }
        }
    };
    let r2c = r2c(tx);
    tokio::pin!(r2c);
    tokio::pin!(c2r);
    let mut r2c_done = false;
    loop {
        tokio::select! {
            _ = aborted(&mut abort) => return,
            _ = &mut c2r => return,
            _ = &mut r2c, if !r2c_done => r2c_done = true,
        }
    }
}

#[cfg(test)]
#[path = "tunnel_tests.rs"]
mod tests;
