use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, Mutex};
use tokio::time::timeout;

use crate::proto::{
    shield_control_message::Body, ShieldControlMessage, TunnelClose, TunnelData, TunnelOpened,
};

const MAX_CHUNK: usize = 16 * 1024;
const CONNECT_TIMEOUT: Duration = Duration::from_secs(5);
const UDP_IDLE_TIMEOUT: Duration = Duration::from_secs(30);

struct TunnelSession {
    inbound_tx: mpsc::Sender<Bytes>,
}

/// Shared tunnel session registry. Cloning is cheap (Arc reference count).
#[derive(Clone)]
pub struct TunnelHub(Arc<Mutex<HashMap<String, TunnelSession>>>);

pub fn new_hub() -> TunnelHub {
    TunnelHub(Arc::new(Mutex::new(HashMap::new())))
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
            .insert(conn_id.clone(), TunnelSession { inbound_tx });

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
            .insert(conn_id.clone(), TunnelSession { inbound_tx });

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

        let _ = tokio::join!(read_task, write_task);
    });
}

pub async fn handle_tunnel_data(hub: TunnelHub, connection_id: &str, data: Vec<u8>) {
    let guard = hub.0.lock().await;
    if let Some(session) = guard.get(connection_id) {
        let _ = session.inbound_tx.try_send(Bytes::from(data));
    }
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
