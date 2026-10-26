use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, Mutex};
use tokio::task::AbortHandle;
use tokio::time::timeout;
use tracing::warn;

use crate::proto::{
    shield_control_message::Body, ShieldControlMessage, TunnelClose, TunnelData, TunnelOpened,
};

const MAX_CHUNK: usize = 16 * 1024;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const UDP_IDLE_TIMEOUT: Duration = Duration::from_secs(30);
/// How long a chunk waits for room in a tunnel's queue to the resource
/// before the tunnel is ended (Fix 14, F48).
const RESOURCE_STALL_LIMIT: Duration = Duration::from_secs(2);

struct TunnelSession {
    inbound_tx: mpsc::Sender<Bytes>,
    /// The tunnel's read and write tasks, set once they are spawned. Aborting
    /// them drops the resource socket (Fix 14).
    tasks: Vec<AbortHandle>,
}

impl TunnelSession {
    fn abort(self) {
        for task in self.tasks {
            task.abort();
        }
    }
}

/// Shared tunnel session registry. Cloning is cheap (Arc reference count).
#[derive(Clone)]
pub struct TunnelHub(Arc<Mutex<HashMap<String, TunnelSession>>>);

pub fn new_hub() -> TunnelHub {
    TunnelHub(Arc::new(Mutex::new(HashMap::new())))
}

impl TunnelHub {
    /// Record a tunnel's tasks; if the tunnel was already ended, stop them.
    async fn set_tasks(&self, connection_id: &str, tasks: Vec<AbortHandle>) {
        let mut guard = self.0.lock().await;
        match guard.get_mut(connection_id) {
            Some(session) => session.tasks = tasks,
            None => {
                for task in tasks {
                    task.abort();
                }
            }
        }
    }

    /// End every tunnel and close its resource socket. Fix 14 (F44): called
    /// when the Control stream these tunnels ran on ends — their queued data
    /// died with it, so they must not carry on, nor leak their sockets.
    pub async fn close_all(&self) -> usize {
        let sessions: Vec<TunnelSession> = self.0.lock().await.drain().map(|(_, s)| s).collect();
        let n = sessions.len();
        for session in sessions {
            session.abort();
        }
        n
    }
}

/// Dispatch to TCP or UDP handler based on protocol field.
pub async fn handle_tunnel_open(
    hub: TunnelHub,
    connection_id: String,
    destination: String,
    port: u32,
    protocol: String,
    upstream_tx: mpsc::Sender<ShieldControlMessage>,
) {
    if protocol == "udp" {
        handle_tunnel_open_udp(hub, connection_id, destination, port, upstream_tx).await;
    } else {
        handle_tunnel_open_tcp(hub, connection_id, destination, port, upstream_tx).await;
    }
}

async fn handle_tunnel_open_tcp(
    hub: TunnelHub,
    connection_id: String,
    destination: String,
    port: u32,
    upstream_tx: mpsc::Sender<ShieldControlMessage>,
) {
    let addr = format!("{destination}:{port}");
    let conn_id = connection_id.clone();

    tokio::spawn(async move {
        let stream = match timeout(CONNECT_TIMEOUT, TcpStream::connect(&addr)).await {
            Ok(Ok(s)) => s,
            Ok(Err(e)) => {
                let _ = upstream_tx
                    .send(tunnel_opened_msg(&conn_id, false, &e.to_string()))
                    .await;
                return;
            }
            Err(_) => {
                let _ = upstream_tx
                    .send(tunnel_opened_msg(&conn_id, false, "connect timeout"))
                    .await;
                return;
            }
        };

        let (inbound_tx, mut inbound_rx) = mpsc::channel::<Bytes>(64);
        hub.0.lock()
            .await
            .insert(
                conn_id.clone(),
                TunnelSession {
                    inbound_tx,
                    tasks: Vec::new(),
                },
            );

        if upstream_tx
            .send(tunnel_opened_msg(&conn_id, true, ""))
            .await
            .is_err()
        {
            hub.0.lock().await.remove(&conn_id);
            return;
        }

        let (mut reader, mut writer) = stream.into_split();
        let hub_clone = hub.clone();
        let tx_clone = upstream_tx.clone();
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
                        if tx_clone.send(msg).await.is_err() {
                            break;
                        }
                    }
                }
            }
            let _ = tx_clone
                .send(ShieldControlMessage {
                    body: Some(Body::TunnelClose(TunnelClose {
                        connection_id: conn_id_read.clone(),
                        error: String::new(),
                    })),
                })
                .await;
            hub_clone.0.lock().await.remove(&conn_id_read);
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
    upstream_tx: mpsc::Sender<ShieldControlMessage>,
) {
    let addr = format!("{destination}:{port}");
    let conn_id = connection_id.clone();

    tokio::spawn(async move {
        let socket = match tokio::net::UdpSocket::bind("0.0.0.0:0").await {
            Ok(s) => s,
            Err(e) => {
                let _ = upstream_tx
                    .send(tunnel_opened_msg(&conn_id, false, &e.to_string()))
                    .await;
                return;
            }
        };
        if let Err(e) = socket.connect(&addr).await {
            let _ = upstream_tx
                .send(tunnel_opened_msg(&conn_id, false, &e.to_string()))
                .await;
            return;
        }

        let (inbound_tx, mut inbound_rx) = mpsc::channel::<Bytes>(64);
        hub.0.lock()
            .await
            .insert(
                conn_id.clone(),
                TunnelSession {
                    inbound_tx,
                    tasks: Vec::new(),
                },
            );

        if upstream_tx
            .send(tunnel_opened_msg(&conn_id, true, ""))
            .await
            .is_err()
        {
            hub.0.lock().await.remove(&conn_id);
            return;
        }

        let socket = Arc::new(socket);
        let hub_clone = hub.clone();
        let tx_clone = upstream_tx.clone();
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
                        if tx_clone.send(msg).await.is_err() {
                            break;
                        }
                    }
                    _ => break, // idle timeout or socket error
                }
            }
            let _ = tx_clone
                .send(ShieldControlMessage {
                    body: Some(Body::TunnelClose(TunnelClose {
                        connection_id: conn_id_read.clone(),
                        error: String::new(),
                    })),
                })
                .await;
            hub_clone.0.lock().await.remove(&conn_id_read);
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

pub async fn handle_tunnel_data(
    hub: TunnelHub,
    connection_id: &str,
    data: Vec<u8>,
    upstream_tx: &mpsc::Sender<ShieldControlMessage>,
) {
    let Some(tx) = hub
        .0
        .lock()
        .await
        .get(connection_id)
        .map(|s| s.inbound_tx.clone())
    else {
        return;
    };
    // Fix 14 (F48): never drop a chunk and carry on (a hole in the byte
    // stream). Wait for room: that pauses this Control stream, which pushes
    // back to the device (flow control). A resource stuck for longer than
    // RESOURCE_STALL_LIMIT ends its tunnel instead, so it cannot stall every
    // other tunnel for long.
    match tx
        .send_timeout(Bytes::from(data), RESOURCE_STALL_LIMIT)
        .await
    {
        Err(mpsc::error::SendTimeoutError::Timeout(_)) => {}
        Ok(()) | Err(mpsc::error::SendTimeoutError::Closed(_)) => return,
    }
    {
        let mut guard = hub.0.lock().await;
        if !guard
            .get(connection_id)
            .is_some_and(|s| s.inbound_tx.same_channel(&tx))
        {
            return;
        }
        if let Some(session) = guard.remove(connection_id) {
            session.abort();
        }
    }
    warn!(connection_id, "resource stalled with its tunnel queue full: ending the tunnel");
    let _ = upstream_tx.try_send(ShieldControlMessage {
        body: Some(Body::TunnelClose(TunnelClose {
            connection_id: connection_id.to_string(),
            error: "resource too slow".to_string(),
        })),
    });
}

pub async fn handle_tunnel_close(hub: TunnelHub, connection_id: &str) {
    hub.0.lock().await.remove(connection_id);
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
    //! sockets (F44); a full resource queue ends the tunnel rather than
    //! dropping a chunk (F48).

    use super::*;
    use tokio::net::TcpListener;

    const WAIT: Duration = Duration::from_secs(5);

    #[tokio::test]
    async fn close_all_closes_the_resource_socket() {
        let resource = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = resource.local_addr().unwrap().port();
        let hub = new_hub();
        let (up_tx, mut up_rx) = mpsc::channel(8);
        handle_tunnel_open(
            hub.clone(),
            "c1".into(),
            "127.0.0.1".into(),
            port as u32,
            "tcp".into(),
            up_tx,
        )
        .await;
        let (mut socket, _) = timeout(WAIT, resource.accept()).await.unwrap().unwrap();
        let opened = timeout(WAIT, up_rx.recv()).await.unwrap().unwrap();
        assert!(matches!(opened.body, Some(Body::TunnelOpened(ref o)) if o.ok));
        // Wait until the tunnel's tasks are recorded.
        timeout(WAIT, async {
            while hub.0.lock().await.get("c1").is_none_or(|s| s.tasks.is_empty()) {
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
        assert!(hub.0.lock().await.is_empty());
        // Both directions are gone: what the resource sends now goes nowhere,
        // and the tunnel's upstream sender is dropped.
        let _ = socket.write_all(b"late").await;
        match timeout(WAIT, up_rx.recv()).await.expect("upstream must close") {
            None => {}
            Some(m) => panic!("a closed tunnel still relayed {m:?}"),
        }
    }

    fn tunnel_without_reader(hub: &TunnelHub) -> impl std::future::Future<Output = mpsc::Receiver<Bytes>> + '_ {
        async move {
            let (inbound_tx, inbound_rx) = mpsc::channel::<Bytes>(64);
            hub.0.lock().await.insert(
                "c1".into(),
                TunnelSession {
                    inbound_tx,
                    tasks: Vec::new(),
                },
            );
            inbound_rx
        }
    }

    /// A resource that is merely slower than the device gets every byte.
    #[tokio::test]
    async fn a_slow_resource_gets_every_chunk_in_order() {
        let hub = new_hub();
        let mut inbound_rx = tunnel_without_reader(&hub).await;
        let (up_tx, _up_rx) = mpsc::channel(8);
        let feeder = {
            let hub = hub.clone();
            tokio::spawn(async move {
                for i in 0..200u8 {
                    handle_tunnel_data(hub.clone(), "c1", vec![i], &up_tx).await;
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
        assert!(hub.0.lock().await.contains_key("c1"));
    }

    /// A resource that stops reading ends its tunnel after the stall limit;
    /// no chunk is dropped while the tunnel carries on.
    #[tokio::test(start_paused = true)]
    async fn a_stalled_resource_ends_the_tunnel_instead_of_dropping_data() {
        let hub = new_hub();
        let mut inbound_rx = tunnel_without_reader(&hub).await;
        let (up_tx, mut up_rx) = mpsc::channel(8);
        for i in 0..65u8 {
            handle_tunnel_data(hub.clone(), "c1", vec![i], &up_tx).await;
        }

        assert!(
            hub.0.lock().await.get("c1").is_none(),
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
}
