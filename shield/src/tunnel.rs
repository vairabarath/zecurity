use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use bytes::Bytes;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::TcpStream;
use tokio::sync::{mpsc, Mutex};
use tokio::time::timeout;

use tracing::{debug, warn};

use crate::proto::{
    shield_control_message::Body, ShieldControlMessage, TunnelClose, TunnelData, TunnelOpened,
};
use crate::resources::SharedResourceState;

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
///
/// Sprint 16 Phase 8.2: the address dialed comes from this shield's own applied
/// state, keyed by the `resource_id` the connector asserts — never from the
/// `destination` field of this per-connection message. Resolution happens once,
/// here, so the TCP and UDP handlers below receive an already-decided address and
/// cannot disagree about it.
pub async fn handle_tunnel_open(
    hub: TunnelHub,
    connection_id: String,
    destination: String,
    port: u32,
    protocol: String,
    resource_id: String,
    resource_state: Arc<SharedResourceState>,
    upstream_tx: mpsc::Sender<ShieldControlMessage>,
) {
    let target = resolve_dial_target(&resource_id, &destination, &resource_state, &connection_id);

    debug!(
        connection_id = %connection_id,
        resource_id = %resource_id,
        dial_target = %target,
        port,
        protocol = %protocol,
        "opening tunnel to resource"
    );

    if protocol == "udp" {
        handle_tunnel_open_udp(hub, connection_id, target, port, upstream_tx).await;
    } else {
        handle_tunnel_open_tcp(hub, connection_id, target, port, upstream_tx).await;
    }
}

/// Decide the address to dial for one tunnel-open. Extracted as a pure function so
/// the decision is testable — the surrounding handler spawns tasks and opens real
/// sockets, so the choice itself would otherwise have no coverage at all.
///
/// The rule: identity comes from the message, the ADDRESS comes from applied state.
fn resolve_dial_target(
    resource_id: &str,
    destination: &str,
    state: &SharedResourceState,
    connection_id: &str,
) -> String {
    if resource_id.is_empty() {
        // Un-upgraded connector: no identity asserted, so there is nothing to look
        // up. Unchanged pre-Phase-8 behaviour, and the reason TunnelOpen.resource_id
        // is tolerant rather than required — mirroring the Phase 1 → Phase 3
        // sequence one layer up. Tightening this to a denial is a follow-up.
        return destination.to_string();
    }
    match state.dial_target_for(resource_id) {
        Some(t) => t,
        None => {
            // The connector asserted a resource this shield has not applied — a
            // snapshot still in flight, or genuine divergence. Falling back is not a
            // new hole (it is exactly what the shield did before this phase), but it
            // IS the residual case worth tightening once every connector is known to
            // send resource_id.
            warn!(
                connection_id = %connection_id,
                resource_id = %resource_id,
                destination = %destination,
                "tunnel open for a resource this shield has not applied — \
                 falling back to the connector's destination"
            );
            destination.to_string()
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::resources::ActiveResource;

    fn state_with(id: &str, dial_target: &str) -> Arc<SharedResourceState> {
        let state = Arc::new(SharedResourceState::new());
        *state.active.lock().unwrap() = vec![ActiveResource {
            resource_id: id.to_string(),
            host: "10.0.0.1".to_string(),
            dial_target: dial_target.to_string(),
            protocol: "tcp".to_string(),
            port_from: 8080,
            port_to: 8080,
        }];
        state
    }

    /// The whole point of Phase 8.2: the stored target wins over the address the
    /// connector put in the message. If this ever returns `destination`, the
    /// connector has free-form dialing inside the shield.
    #[test]
    fn applied_resource_dials_its_stored_target_not_the_message() {
        let state = state_with("r1", "127.0.0.1");
        assert_eq!(
            resolve_dial_target("r1", "10.0.0.1", &state, "c1"),
            "127.0.0.1"
        );
    }

    /// Backward compatibility: an un-upgraded connector sends no resource_id, so
    /// there is nothing to look up and behaviour must be exactly as before.
    #[test]
    fn empty_resource_id_falls_back_to_the_message_destination() {
        let state = state_with("r1", "127.0.0.1");
        assert_eq!(
            resolve_dial_target("", "10.0.0.1", &state, "c1"),
            "10.0.0.1"
        );
    }

    /// Deliberately tolerant, not fail-closed: a snapshot may still be in flight.
    /// Documented as the residual case to tighten once every connector sends an id.
    #[test]
    fn unapplied_resource_falls_back_to_the_message_destination() {
        let state = state_with("r1", "127.0.0.1");
        assert_eq!(
            resolve_dial_target("r-other", "10.0.0.1", &state, "c1"),
            "10.0.0.1"
        );
    }

    /// A resource with no local_target stores host as its dial_target, so the
    /// lookup path and the fallback path agree — the common case must not change
    /// behaviour just because the connector started asserting an identity.
    #[test]
    fn resource_without_local_target_dials_its_host() {
        let state = state_with("r1", "10.0.0.1");
        assert_eq!(
            resolve_dial_target("r1", "10.0.0.1", &state, "c1"),
            "10.0.0.1"
        );
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
        hub.0
            .lock()
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
        hub.0
            .lock()
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
