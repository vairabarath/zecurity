use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, watch, Mutex};
use tokio::task::AbortHandle;
use tokio::time::{timeout, Instant};
use tracing::warn;

use crate::proto::{
    shield_control_message::Body, ShieldControlMessage, TunnelClose, TunnelData, TunnelOpened,
};

const MAX_CHUNK: usize = 16 * 1024;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const UDP_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
/// Fix 14 (F48): a tunnel may hold up the Control stream (waiting for room in
/// its full queue to the resource) for at most RESOURCE_STALL_LIMIT in total
/// per RESOURCE_STALL_WINDOW; then it is ended. Counted across chunks, so a
/// resource reading just under a per-chunk limit cannot stall the stream (and
/// every other tunnel on it) indefinitely.
const RESOURCE_STALL_LIMIT: Duration = Duration::from_secs(2);
const RESOURCE_STALL_WINDOW: Duration = Duration::from_secs(10);
/// Fix 14 handover: how long a tunnel message waits for an upstream (a
/// Control stream) while the Shield moves from one stream to the next.
pub const HANDOVER_GRACE: Duration = Duration::from_secs(15);

struct TunnelSession {
    inbound_tx: mpsc::Sender<Bytes>,
    /// The tunnel's read and write tasks, set once they are spawned. Aborting
    /// them drops the resource socket (Fix 14).
    tasks: Vec<AbortHandle>,
    /// F48 stall budget: when the current window started, and how long this
    /// tunnel has held up the Control stream in it.
    stall_window: Instant,
    stalled: Duration,
}

impl TunnelSession {
    fn new(inbound_tx: mpsc::Sender<Bytes>) -> Self {
        Self {
            inbound_tx,
            tasks: Vec::new(),
            stall_window: Instant::now(),
            stalled: Duration::ZERO,
        }
    }

    fn abort(self) {
        for task in self.tasks {
            task.abort();
        }
    }
}

/// Shared tunnel session registry. Cloning is cheap (Arc reference count).
///
/// Fix 14 (D-1 = B): every message a tunnel sends to the Connector goes
/// through the hub's one `upstream`, the current Control stream. The Shield
/// pauses it (None) while it hands its tunnels over to a new stream, then
/// points it at the new stream; tunnels never hold a stream of their own.
#[derive(Clone)]
pub struct TunnelHub {
    sessions: Arc<Mutex<HashMap<String, TunnelSession>>>,
    upstream: Arc<watch::Sender<Option<mpsc::Sender<ShieldControlMessage>>>>,
}

pub fn new_hub() -> TunnelHub {
    TunnelHub {
        sessions: Arc::new(Mutex::new(HashMap::new())),
        upstream: Arc::new(watch::channel(None).0),
    }
}

impl TunnelHub {
    /// Point tunnel traffic at `upstream`, or pause it (None).
    pub fn set_upstream(&self, upstream: Option<mpsc::Sender<ShieldControlMessage>>) {
        self.upstream.send_replace(upstream);
    }

    /// Send `msg` on the current upstream. While paused, or if the stream
    /// behind it closed, wait for the next one; the failed message is retried,
    /// so none is lost or reordered. Gives up after HANDOVER_GRACE.
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

    /// Record a tunnel's tasks; if the tunnel was already ended, stop them.
    async fn set_tasks(&self, connection_id: &str, tasks: Vec<AbortHandle>) {
        let mut guard = self.sessions.lock().await;
        match guard.get_mut(connection_id) {
            Some(session) => session.tasks = tasks,
            None => {
                for task in tasks {
                    task.abort();
                }
            }
        }
    }

    /// The number of open tunnels.
    pub async fn len(&self) -> usize {
        self.sessions.lock().await.len()
    }

    /// End every tunnel and close its resource socket. Fix 14 (F44): called
    /// when the Control stream these tunnels ran on ends other than by a
    /// handover — their queued data died with it, so they must not carry on,
    /// nor leak their sockets.
    pub async fn close_all(&self) -> usize {
        let sessions: Vec<TunnelSession> = self
            .sessions
            .lock()
            .await
            .drain()
            .map(|(_, s)| s)
            .collect();
        let n = sessions.len();
        for session in sessions {
            session.abort();
        }
        n
    }

    fn close_msg(connection_id: &str, error: &str) -> ShieldControlMessage {
        ShieldControlMessage {
            body: Some(Body::TunnelClose(TunnelClose {
                connection_id: connection_id.to_string(),
                error: error.to_string(),
            })),
        }
    }
}

/// Dispatch to TCP or UDP handler based on protocol field.
pub async fn handle_tunnel_open(
    hub: TunnelHub,
    connection_id: String,
    destination: String,
    port: u32,
    protocol: String,
) {
    if protocol == "udp" {
        handle_tunnel_open_udp(hub, connection_id, destination, port).await;
    } else {
        handle_tunnel_open_tcp(hub, connection_id, destination, port).await;
    }
}

async fn handle_tunnel_open_tcp(
    hub: TunnelHub,
    connection_id: String,
    destination: String,
    port: u32,
) {
    let addr = format!("{destination}:{port}");
    let conn_id = connection_id.clone();

    tokio::spawn(async move {
        let stream = match timeout(CONNECT_TIMEOUT, TcpStream::connect(&addr)).await {
            Ok(Ok(s)) => s,
            Ok(Err(e)) => {
                hub.send_upstream(tunnel_opened_msg(&conn_id, false, &e.to_string()))
                    .await;
                return;
            }
            Err(_) => {
                hub.send_upstream(tunnel_opened_msg(&conn_id, false, "connect timeout"))
                    .await;
                return;
            }
        };

        let (inbound_tx, mut inbound_rx) = mpsc::channel::<Bytes>(64);
        hub.sessions
            .lock()
            .await
            .insert(conn_id.clone(), TunnelSession::new(inbound_tx));

        if !hub.send_upstream(tunnel_opened_msg(&conn_id, true, "")).await {
            hub.sessions.lock().await.remove(&conn_id);
            return;
        }

        let (mut reader, mut writer) = stream.into_split();
        let hub_clone = hub.clone();
        let conn_id_read = conn_id.clone();

        let read_task = tokio::spawn(async move {
            let mut buf = vec![0u8; MAX_CHUNK];
            loop {
                match reader.read(&mut buf).await {
                    Ok(0) | Err(_) => break,
                    Ok(n) => {
                        let msg = ShieldControlMessage {
                            body: Some(Body::TunnelData(TunnelData {
                                connection_id: conn_id_read.clone(),
                                data: buf[..n].to_vec(),
                            })),
                        };
                        if !hub_clone.send_upstream(msg).await {
                            break;
                        }
                    }
                }
            }
            hub_clone
                .send_upstream(TunnelHub::close_msg(&conn_id_read, ""))
                .await;
            hub_clone.sessions.lock().await.remove(&conn_id_read);
        });

        let write_task = tokio::spawn(async move {
            while let Some(data) = inbound_rx.recv().await {
                if writer.write_all(&data).await.is_err() {
                    break;
                }
            }
        });

        hub.set_tasks(&conn_id, vec![read_task.abort_handle(), write_task.abort_handle()])
            .await;
        let _ = tokio::join!(read_task, write_task);
    });
}

/// UDP relay: each TunnelData proto message = one datagram.
/// Idle timeout: 30s with no received datagram closes the session.
async fn handle_tunnel_open_udp(
    hub: TunnelHub,
    connection_id: String,
    destination: String,
    port: u32,
) {
    let addr = format!("{destination}:{port}");
    let conn_id = connection_id.clone();

    tokio::spawn(async move {
        let socket = match tokio::net::UdpSocket::bind("0.0.0.0:0").await {
            Ok(s) => s,
            Err(e) => {
                hub.send_upstream(tunnel_opened_msg(&conn_id, false, &e.to_string()))
                    .await;
                return;
            }
        };
        if let Err(e) = socket.connect(&addr).await {
            hub.send_upstream(tunnel_opened_msg(&conn_id, false, &e.to_string()))
                .await;
            return;
        }

        let (inbound_tx, mut inbound_rx) = mpsc::channel::<Bytes>(64);
        hub.sessions
            .lock()
            .await
            .insert(conn_id.clone(), TunnelSession::new(inbound_tx));

        if !hub.send_upstream(tunnel_opened_msg(&conn_id, true, "")).await {
            hub.sessions.lock().await.remove(&conn_id);
            return;
        }

        let socket = Arc::new(socket);
        let hub_clone = hub.clone();
        let conn_id_read = conn_id.clone();
        let socket_read = socket.clone();

        // Resource → Connector: recv datagram → TunnelData
        let read_task = tokio::spawn(async move {
            let mut buf = vec![0u8; MAX_CHUNK];
            loop {
                match timeout(UDP_IDLE_TIMEOUT, socket_read.recv(&mut buf)).await {
                    Ok(Ok(n)) => {
                        let msg = ShieldControlMessage {
                            body: Some(Body::TunnelData(TunnelData {
                                connection_id: conn_id_read.clone(),
                                data: buf[..n].to_vec(),
                            })),
                        };
                        if !hub_clone.send_upstream(msg).await {
                            break;
                        }
                    }
                    _ => break, // idle timeout or socket error
                }
            }
            hub_clone
                .send_upstream(TunnelHub::close_msg(&conn_id_read, ""))
                .await;
            hub_clone.sessions.lock().await.remove(&conn_id_read);
        });

        // Connector → Resource: TunnelData → send datagram
        let write_task = tokio::spawn(async move {
            while let Some(data) = inbound_rx.recv().await {
                if socket.send(&data).await.is_err() {
                    break;
                }
            }
        });

        hub.set_tasks(&conn_id, vec![read_task.abort_handle(), write_task.abort_handle()])
            .await;
        let _ = tokio::join!(read_task, write_task);
    });
}

pub async fn handle_tunnel_data(hub: TunnelHub, connection_id: &str, data: Vec<u8>) {
    let (tx, budget) = {
        let mut guard = hub.sessions.lock().await;
        let Some(session) = guard.get_mut(connection_id) else {
            return;
        };
        let now = Instant::now();
        if now.duration_since(session.stall_window) >= RESOURCE_STALL_WINDOW {
            session.stall_window = now;
            session.stalled = Duration::ZERO;
        }
        (
            session.inbound_tx.clone(),
            RESOURCE_STALL_LIMIT.saturating_sub(session.stalled),
        )
    };
    // Fix 14 (F48): never drop a chunk and carry on (a hole in the byte
    // stream). If the queue is full, wait for room: that pauses this Control
    // stream, which pushes back to the device (flow control). The wait comes
    // out of the tunnel's stall budget; a tunnel that uses it up is ended, so
    // it cannot keep stalling every other tunnel.
    let data = match tx.try_send(Bytes::from(data)) {
        Ok(()) | Err(mpsc::error::TrySendError::Closed(_)) => return,
        Err(mpsc::error::TrySendError::Full(data)) => data,
    };
    let started = Instant::now();
    let sent = if budget.is_zero() {
        Err(mpsc::error::SendTimeoutError::Timeout(data))
    } else {
        tx.send_timeout(data, budget).await
    };
    let mut guard = hub.sessions.lock().await;
    if let Some(session) = guard.get_mut(connection_id) {
        session.stalled += started.elapsed();
    }
    if !matches!(sent, Err(mpsc::error::SendTimeoutError::Timeout(_))) {
        return;
    }
    if !guard
        .get(connection_id)
        .is_some_and(|s| s.inbound_tx.same_channel(&tx))
    {
        return;
    }
    if let Some(session) = guard.remove(connection_id) {
        session.abort();
    }
    drop(guard);
    warn!(connection_id, "resource used up its stall budget with its tunnel queue full: ending the tunnel");
    // Not awaited here: the upstream may be paused for a handover, and this
    // runs on the Control stream's reader.
    let close = TunnelHub::close_msg(connection_id, "resource too slow");
    tokio::spawn(async move { hub.send_upstream(close).await });
}

pub async fn handle_tunnel_close(hub: TunnelHub, connection_id: &str) {
    hub.sessions.lock().await.remove(connection_id);
}

fn tunnel_opened_msg(connection_id: &str, ok: bool, error: &str) -> ShieldControlMessage {
    ShieldControlMessage {
        body: Some(Body::TunnelOpened(TunnelOpened {
            connection_id: connection_id.to_string(),
            ok,
            error: error.to_string(),
        })),
    }
}

#[cfg(test)]
mod tests {
    //! Fix 14: a Control stream's end closes its tunnels and their resource
    //! sockets (F44); a full resource queue waits, within a stall budget,
    //! rather than dropping a chunk (F48); tunnel traffic follows the hub's
    //! upstream across a handover (D-1 = B).

    use super::*;
    use tokio::net::TcpListener;

    const WAIT: Duration = Duration::from_secs(5);

    #[tokio::test]
    async fn close_all_closes_the_resource_socket() {
        let resource = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = resource.local_addr().unwrap().port();
        let hub = new_hub();
        let (up_tx, mut up_rx) = mpsc::channel(8);
        hub.set_upstream(Some(up_tx));
        handle_tunnel_open(
            hub.clone(),
            "c1".into(),
            "127.0.0.1".into(),
            port as u32,
            "tcp".into(),
        )
        .await;
        let (mut socket, _) = timeout(WAIT, resource.accept()).await.unwrap().unwrap();
        let opened = timeout(WAIT, up_rx.recv()).await.unwrap().unwrap();
        assert!(matches!(opened.body, Some(Body::TunnelOpened(ref o)) if o.ok));
        // Wait until the tunnel's tasks are recorded.
        timeout(WAIT, async {
            while hub
                .sessions
                .lock()
                .await
                .get("c1")
                .is_none_or(|s| s.tasks.is_empty())
            {
                tokio::task::yield_now().await;
            }
        })
        .await
        .unwrap();

        assert_eq!(hub.close_all().await, 1);

        let mut buf = [0u8; 8];
        let n = timeout(WAIT, socket.read(&mut buf))
            .await
            .expect("the resource socket must close, not stay open")
            .unwrap_or(0);
        assert_eq!(n, 0);
        assert!(hub.sessions.lock().await.is_empty());
        // Both directions are gone: what the resource sends now goes nowhere.
        let _ = socket.write_all(b"late").await;
        if let Ok(Some(m)) = timeout(Duration::from_millis(300), up_rx.recv()).await {
            panic!("a closed tunnel still relayed {m:?}");
        }
    }

    async fn tunnel_without_reader(hub: &TunnelHub) -> mpsc::Receiver<Bytes> {
        let (inbound_tx, inbound_rx) = mpsc::channel::<Bytes>(64);
        hub.sessions
            .lock()
            .await
            .insert("c1".into(), TunnelSession::new(inbound_tx));
        inbound_rx
    }

    /// A resource that is merely slower than the device gets every byte.
    #[tokio::test]
    async fn a_slow_resource_gets_every_chunk_in_order() {
        let hub = new_hub();
        let mut inbound_rx = tunnel_without_reader(&hub).await;
        let feeder = {
            let hub = hub.clone();
            tokio::spawn(async move {
                for i in 0..200u8 {
                    handle_tunnel_data(hub.clone(), "c1", vec![i]).await;
                }
            })
        };
        for i in 0..200u8 {
            if i % 50 == 0 {
                tokio::time::sleep(Duration::from_millis(100)).await;
            }
            assert_eq!(inbound_rx.recv().await.unwrap(), Bytes::from(vec![i]), "chunk {i}");
        }
        feeder.await.unwrap();
        assert!(hub.sessions.lock().await.contains_key("c1"));
    }

    /// A resource that stops reading ends its tunnel when its stall budget
    /// runs out; no chunk is dropped while the tunnel carries on.
    #[tokio::test(start_paused = true)]
    async fn a_stalled_resource_ends_the_tunnel_instead_of_dropping_data() {
        let hub = new_hub();
        let mut inbound_rx = tunnel_without_reader(&hub).await;
        let (up_tx, mut up_rx) = mpsc::channel(8);
        hub.set_upstream(Some(up_tx));
        for i in 0..65u8 {
            handle_tunnel_data(hub.clone(), "c1", vec![i]).await;
        }

        assert!(
            hub.sessions.lock().await.get("c1").is_none(),
            "the tunnel carried on after a chunk could not be queued"
        );
        let close = up_rx.recv().await.unwrap();
        assert!(
            matches!(close.body, Some(Body::TunnelClose(ref c)) if c.connection_id == "c1" && !c.error.is_empty()),
            "the connector was not told: {close:?}"
        );
        for i in 0..64u8 {
            assert_eq!(inbound_rx.recv().await.unwrap(), Bytes::from(vec![i]));
        }
        assert!(inbound_rx.recv().await.is_none());
    }

    /// A resource reading one chunk every 1.5 s never stalls a single chunk
    /// for 2 s, but holds the stream up most of the time: its budget runs out.
    #[tokio::test(start_paused = true)]
    async fn a_trickling_resource_uses_up_its_stall_budget() {
        let hub = new_hub();
        let mut inbound_rx = tunnel_without_reader(&hub).await;
        for i in 0..64u8 {
            handle_tunnel_data(hub.clone(), "c1", vec![i]).await;
        }
        let reader = tokio::spawn(async move {
            loop {
                tokio::time::sleep(Duration::from_millis(1500)).await;
                if inbound_rx.recv().await.is_none() {
                    break;
                }
            }
        });
        let started = Instant::now();
        let mut sent = 0;
        while hub.sessions.lock().await.contains_key("c1") {
            handle_tunnel_data(hub.clone(), "c1", vec![0]).await;
            sent += 1;
            assert!(sent < 10, "the trickling resource was never ended");
        }
        assert!(started.elapsed() <= RESOURCE_STALL_LIMIT + Duration::from_millis(100));
        reader.abort();
    }

    fn data(n: u8) -> ShieldControlMessage {
        ShieldControlMessage {
            body: Some(Body::TunnelData(TunnelData {
                connection_id: "c1".into(),
                data: vec![n],
            })),
        }
    }

    fn data_of(m: ShieldControlMessage) -> u8 {
        match m.body {
            Some(Body::TunnelData(d)) => d.data[0],
            other => panic!("expected TunnelData, got {other:?}"),
        }
    }

    /// Handover: while paused, tunnel traffic waits; then it goes out on the
    /// new upstream, in order.
    #[tokio::test]
    async fn paused_traffic_goes_out_on_the_next_upstream_in_order() {
        let hub = new_hub();
        let (old_tx, mut old_rx) = mpsc::channel(8);
        hub.set_upstream(Some(old_tx));
        assert!(hub.send_upstream(data(1)).await);
        hub.set_upstream(None);
        let sender = {
            let hub = hub.clone();
            tokio::spawn(async move {
                assert!(hub.send_upstream(data(2)).await);
                assert!(hub.send_upstream(data(3)).await);
            })
        };
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(!sender.is_finished(), "sent while paused");
        let (new_tx, mut new_rx) = mpsc::channel(8);
        hub.set_upstream(Some(new_tx));
        sender.await.unwrap();
        assert_eq!(data_of(old_rx.recv().await.unwrap()), 1);
        assert!(old_rx.try_recv().is_err(), "the old stream got data after the pause");
        assert_eq!(data_of(new_rx.recv().await.unwrap()), 2);
        assert_eq!(data_of(new_rx.recv().await.unwrap()), 3);
    }

    /// A message whose stream closed under it is retried on the next one.
    #[tokio::test]
    async fn a_message_whose_stream_closed_is_retried_on_the_next_one() {
        let hub = new_hub();
        let (old_tx, old_rx) = mpsc::channel(8);
        hub.set_upstream(Some(old_tx));
        drop(old_rx);
        let sender = {
            let hub = hub.clone();
            tokio::spawn(async move { hub.send_upstream(data(9)).await })
        };
        tokio::time::sleep(Duration::from_millis(50)).await;
        let (new_tx, mut new_rx) = mpsc::channel(8);
        hub.set_upstream(Some(new_tx));
        assert!(sender.await.unwrap());
        assert_eq!(data_of(new_rx.recv().await.unwrap()), 9);
    }

    /// No upstream within the grace: the message is given up.
    #[tokio::test(start_paused = true)]
    async fn with_no_upstream_a_message_is_given_up_after_the_grace() {
        let hub = new_hub();
        assert!(!hub.send_upstream(data(1)).await);
    }
}
