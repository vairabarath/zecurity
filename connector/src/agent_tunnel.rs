use std::collections::HashMap;
use std::sync::{Arc, Mutex};

use anyhow::{anyhow, Result};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;
use tracing::warn;
use uuid::Uuid;

use crate::shield_proto::shield_control_message::Body;
use crate::shield_proto::{ShieldControlMessage, TunnelClose, TunnelData, TunnelOpen};

const MAX_CHUNK: usize = 16 * 1024;

// ---------------------------------------------------------------------------
// Events dispatched from Shield → relay session
// ---------------------------------------------------------------------------

enum RelayEvent {
    Opened { ok: bool, error: String },
    Data(Vec<u8>),
    Close(String),
}

// ---------------------------------------------------------------------------
// AgentTunnelHub
// ---------------------------------------------------------------------------

/// Manages tunnel sessions between device connections and Shield instances.
///
/// M3 creates this; M4 calls `open_relay_session` from device_tunnel.rs.
/// agent_server.rs calls `register_shield`, `dispatch_*`, and `unregister_shield`
/// as Shields connect / send messages / disconnect.
#[derive(Clone, Debug, Default)]
pub struct AgentTunnelHub {
    /// shield_id → channel for sending ShieldControlMessages TO that Shield.
    shield_txs: Arc<Mutex<HashMap<String, mpsc::Sender<ShieldControlMessage>>>>,
    /// connection_id → channel for routing events FROM Shield back to the relay session.
    relay_sessions: Arc<Mutex<HashMap<String, mpsc::Sender<RelayEvent>>>>,
}

impl AgentTunnelHub {
    pub fn new() -> Self {
        Self::default()
    }

    // -----------------------------------------------------------------------
    // Called by agent_server.rs when a Shield connects / disconnects
    // -----------------------------------------------------------------------

    /// Register a channel that delivers ShieldControlMessages to `shield_id`.
    /// Called by agent_server's `control()` after the Shield connects.
    pub fn register_shield(&self, shield_id: String, tx: mpsc::Sender<ShieldControlMessage>) {
        self.shield_txs
            .lock()
            .unwrap()
            .insert(shield_id, tx);
    }

    /// Remove the Shield's send channel when it disconnects.
    pub fn unregister_shield(&self, shield_id: &str) {
        self.shield_txs.lock().unwrap().remove(shield_id);
    }

    // -----------------------------------------------------------------------
    // Called by agent_server.rs to dispatch incoming Shield tunnel messages
    // -----------------------------------------------------------------------

    pub fn dispatch_opened(&self, connection_id: &str, ok: bool, error: String) {
        let tx = self
            .relay_sessions
            .lock()
            .unwrap()
            .get(connection_id)
            .cloned();
        if let Some(tx) = tx {
            let _ = tx.try_send(RelayEvent::Opened { ok, error });
        }
    }

    pub fn dispatch_data(&self, connection_id: &str, data: Vec<u8>) {
        let tx = self
            .relay_sessions
            .lock()
            .unwrap()
            .get(connection_id)
            .cloned();
        if let Some(tx) = tx {
            let _ = tx.try_send(RelayEvent::Data(data));
        }
    }

    pub fn dispatch_close(&self, connection_id: &str, error: String) {
        let tx = self
            .relay_sessions
            .lock()
            .unwrap()
            .get(connection_id)
            .cloned();
        if let Some(tx) = tx {
            let _ = tx.try_send(RelayEvent::Close(error));
        }
        // Remove session — Shield closed its side.
        self.relay_sessions.lock().unwrap().remove(connection_id);
    }

    // -----------------------------------------------------------------------
    // Called by device_tunnel.rs to open a relay through a Shield
    // -----------------------------------------------------------------------

    /// Open a relay session through `shield_id` for the given destination.
    ///
    /// Sends `TunnelOpen` to the Shield and waits for `TunnelOpened`.
    /// Returns a `RelaySession` ready for bidirectional data relay.
    ///
    /// **M4 API contract** — do not change the signature without coordinating with M4:
    /// ```rust
    /// let relay = agent_tunnel::open_relay_session(
    ///     &hub, shield_id, destination, port, protocol
    /// ).await?;
    /// relay.relay_stream(stream).await?;
    /// ```
    pub async fn open_relay_session(
        &self,
        shield_id: &str,
        destination: &str,
        port: u16,
        protocol: &str,
    ) -> Result<RelaySession> {
        let shield_tx = self
            .shield_txs
            .lock()
            .unwrap()
            .get(shield_id)
            .cloned()
            .ok_or_else(|| anyhow!("shield {} not connected", shield_id))?;

        let connection_id = Uuid::new_v4().to_string();

        // Register session channel before sending TunnelOpen — avoid race where
        // Shield replies before we're listening.
        let (event_tx, mut event_rx) = mpsc::channel::<RelayEvent>(64);
        self.relay_sessions
            .lock()
            .unwrap()
            .insert(connection_id.clone(), event_tx);

        shield_tx
            .send(ShieldControlMessage {
                body: Some(Body::TunnelOpen(TunnelOpen {
                    connection_id: connection_id.clone(),
                    destination: destination.to_string(),
                    port: port as u32,
                    protocol: protocol.to_string(),
                })),
            })
            .await
            .map_err(|_| anyhow!("shield {} send channel closed", shield_id))?;

        // Wait for TunnelOpened (first event on this session).
        match event_rx.recv().await {
            Some(RelayEvent::Opened { ok: true, .. }) => {}
            Some(RelayEvent::Opened { ok: false, error }) => {
                self.relay_sessions.lock().unwrap().remove(&connection_id);
                return Err(anyhow!("shield rejected tunnel: {}", error));
            }
            _ => {
                self.relay_sessions.lock().unwrap().remove(&connection_id);
                return Err(anyhow!("tunnel session closed before TunnelOpened"));
            }
        }

        Ok(RelaySession {
            connection_id,
            event_rx,
            shield_tx,
        })
    }
}

// ---------------------------------------------------------------------------
// RelaySession — returned by open_relay_session, used by device_tunnel
// ---------------------------------------------------------------------------

pub struct RelaySession {
    connection_id: String,
    event_rx: mpsc::Receiver<RelayEvent>,
    shield_tx: mpsc::Sender<ShieldControlMessage>,
}

impl RelaySession {
    /// Bidirectionally relay between `stream` (device) and the Shield.
    ///
    /// - Reads data from `stream` → sends `TunnelData` to Shield (max 16 KB chunks).
    /// - Receives `TunnelData` events from Shield → writes to `stream`.
    /// - Terminates on `TunnelClose` from either side or any IO error.
    pub async fn relay_stream<S>(mut self, stream: S) -> Result<()>
    where
        S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin + Send + 'static,
    {
        let (mut reader, mut writer) = tokio::io::split(stream);

        // Device → Shield in a separate task.
        let shield_tx = self.shield_tx.clone();
        let conn_id_d2s = self.connection_id.clone();
        let d2s = tokio::spawn(async move {
            let mut buf = vec![0u8; MAX_CHUNK];
            loop {
                match reader.read(&mut buf).await {
                    Ok(0) | Err(_) => break,
                    Ok(n) => {
                        if shield_tx
                            .send(ShieldControlMessage {
                                body: Some(Body::TunnelData(TunnelData {
                                    connection_id: conn_id_d2s.clone(),
                                    data: buf[..n].to_vec(),
                                })),
                            })
                            .await
                            .is_err()
                        {
                            break;
                        }
                    }
                }
            }
            // Notify Shield that the device closed its side.
            let _ = shield_tx
                .send(ShieldControlMessage {
                    body: Some(Body::TunnelClose(TunnelClose {
                        connection_id: conn_id_d2s,
                        error: String::new(),
                    })),
                })
                .await;
        });

        // Shield → Device: relay events to the device stream.
        while let Some(event) = self.event_rx.recv().await {
            match event {
                RelayEvent::Data(data) => {
                    if writer.write_all(&data).await.is_err() {
                        break;
                    }
                }
                RelayEvent::Close(err) => {
                    if !err.is_empty() {
                        warn!(connection_id = %self.connection_id, error = %err, "shield closed tunnel with error");
                    }
                    break;
                }
                RelayEvent::Opened { .. } => {} // unexpected after session start
            }
        }

        d2s.abort();
        Ok(())
    }
}
