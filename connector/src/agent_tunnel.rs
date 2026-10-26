use parking_lot::Mutex;
use std::collections::{HashMap, HashSet};
use std::sync::Arc;

use anyhow::{anyhow, Result};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;
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
    /// One lock for registrations, sessions and revocations, so a shield
    /// revocation and a session being opened through that shield are
    /// serialized (certificate audit bug 3).
    state: Arc<Mutex<HubState>>,
}

#[derive(Debug, Default)]
struct HubState {
    /// shield_id → the Control stream currently registered for that Shield:
    /// its stream id and the channel for sending ShieldControlMessages TO it.
    /// Fix 14 (F45): keyed by shield, owned by one stream — an older stream's
    /// cleanup must not remove a newer stream's registration.
    shield_txs: HashMap<String, (u64, mpsc::Sender<ShieldControlMessage>)>,
    /// connection_id → the session's Shield and stream, and the channel
    /// routing events FROM that Shield back to the relay session. The hub
    /// holds the only sender, so removing an entry ends the session.
    relay_sessions: HashMap<String, RelayEntry>,
    /// Revoked shields (terminal): never registered again, no new sessions.
    revoked: HashSet<String>,
}

#[derive(Debug)]
struct RelayEntry {
    shield_id: String,
    /// The Control stream the session's tunnel runs on (Fix 14, F44).
    stream_id: u64,
    tx: mpsc::Sender<RelayEvent>,
}

impl AgentTunnelHub {
    pub fn new() -> Self {
        Self::default()
    }

    // -----------------------------------------------------------------------
    // Called by agent_server.rs when a Shield connects / disconnects
    // -----------------------------------------------------------------------

    /// Register stream `stream_id`'s channel that delivers ShieldControlMessages
    /// to `shield_id`. Called by agent_server's `control()` after the Shield
    /// connects; the newest stream becomes the Shield's current one.
    /// Returns false, registering nothing, if the shield is revoked.
    #[must_use]
    pub fn register_shield(
        &self,
        shield_id: String,
        stream_id: u64,
        tx: mpsc::Sender<ShieldControlMessage>,
    ) -> bool {
        let mut state = self.state.lock();
        if state.revoked.contains(&shield_id) {
            return false;
        }
        state.shield_txs.insert(shield_id, (stream_id, tx));
        true
    }

    /// Stream `stream_id` of `shield_id` ended. Fix 14:
    /// - F45: the Shield's registration is removed only if it is still this
    ///   stream's; a newer stream that registered first stays.
    /// - F44: every relay session whose tunnel ran on this stream ends now.
    ///   Its queued tunnel data died with the stream, so it must not carry on
    ///   (it would have a hole in its byte stream) nor hang.
    pub fn unregister_shield(&self, shield_id: &str, stream_id: u64) {
        let mut state = self.state.lock();
        if state
            .shield_txs
            .get(shield_id)
            .is_some_and(|(id, _)| *id == stream_id)
        {
            state.shield_txs.remove(shield_id);
        }
        let ended: Vec<String> = state
            .relay_sessions
            .iter()
            .filter(|(_, e)| e.shield_id == shield_id && e.stream_id == stream_id)
            .map(|(cid, _)| cid.clone())
            .collect();
        for cid in ended {
            if let Some(entry) = state.relay_sessions.remove(&cid) {
                // Best effort: dropping the only sender ends the session anyway.
                let _ = entry
                    .tx
                    .try_send(RelayEvent::Close("shield stream ended".to_string()));
            }
        }
    }

    /// Revoke `shield_id` (certificate audit bug 3): unregister it, refuse it
    /// from now on, and end every relay session through it — including one
    /// still waiting for TunnelOpened. Idempotent.
    pub fn revoke_shield(&self, shield_id: &str) {
        let mut state = self.state.lock();
        state.revoked.insert(shield_id.to_string());
        state.shield_txs.remove(shield_id);
        let ended: Vec<String> = state
            .relay_sessions
            .iter()
            .filter(|(_, e)| e.shield_id == shield_id)
            .map(|(cid, _)| cid.clone())
            .collect();
        for cid in ended {
            if let Some(entry) = state.relay_sessions.remove(&cid) {
                // Best effort: dropping the only sender ends the session anyway.
                let _ = entry
                    .tx
                    .try_send(RelayEvent::Close("shield revoked".to_string()));
            }
        }
    }

    // -----------------------------------------------------------------------
    // Called by agent_server.rs to dispatch incoming Shield tunnel messages
    // -----------------------------------------------------------------------

    pub fn dispatch_opened(&self, connection_id: &str, ok: bool, error: String) {
        if let Some(tx) = self.session_tx(connection_id) {
            let _ = tx.try_send(RelayEvent::Opened { ok, error });
        }
    }

    pub fn dispatch_data(&self, connection_id: &str, data: Vec<u8>) {
        let mut state = self.state.lock();
        let Some(entry) = state.relay_sessions.get(connection_id) else {
            return;
        };
        if !matches!(
            entry.tx.try_send(RelayEvent::Data(data)),
            Err(mpsc::error::TrySendError::Full(_))
        ) {
            return;
        }
        // Fix 14 (F48): the device side is not keeping up. Never drop a chunk
        // and carry on (a hole in the byte stream); end this session instead.
        // Waiting for room would stall the Shield's whole Control stream, i.e.
        // every other session through it. Removing the entry drops the only
        // sender: the device gets the chunks already queued, then the end.
        let Some(entry) = state.relay_sessions.remove(connection_id) else {
            return;
        };
        let shield_tx = state
            .shield_txs
            .get(&entry.shield_id)
            .filter(|(id, _)| *id == entry.stream_id)
            .map(|(_, tx)| tx.clone());
        drop(state);
        warn!(connection_id, "tunnel queue to the device is full: ending the session");
        if let Some(tx) = shield_tx {
            let _ = tx.try_send(ShieldControlMessage {
                body: Some(Body::TunnelClose(TunnelClose {
                    connection_id: connection_id.to_string(),
                    error: "device too slow".to_string(),
                })),
            });
        }
    }

    pub fn dispatch_close(&self, connection_id: &str, error: String) {
        // Remove session — Shield closed its side.
        if let Some(entry) = self.state.lock().relay_sessions.remove(connection_id) {
            let _ = entry.tx.try_send(RelayEvent::Close(error));
        }
    }

    fn session_tx(&self, connection_id: &str) -> Option<mpsc::Sender<RelayEvent>> {
        self.state
            .lock()
            .relay_sessions
            .get(connection_id)
            .map(|e| e.tx.clone())
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
    /// ```ignore
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
        let connection_id = Uuid::new_v4().to_string();

        // Register session channel before sending TunnelOpen — avoid race where
        // Shield replies before we're listening. The revocation check and the
        // registration happen under the hub lock, so a concurrent revocation
        // either refuses this session or ends it (certificate audit bug 3).
        let (event_tx, mut event_rx) = mpsc::channel::<RelayEvent>(64);
        let shield_tx = {
            let mut state = self.state.lock();
            if state.revoked.contains(shield_id) {
                return Err(anyhow!("shield {} is revoked", shield_id));
            }
            let (stream_id, shield_tx) = state
                .shield_txs
                .get(shield_id)
                .cloned()
                .ok_or_else(|| anyhow!("shield {} not connected", shield_id))?;
            state.relay_sessions.insert(
                connection_id.clone(),
                RelayEntry {
                    shield_id: shield_id.to_string(),
                    stream_id,
                    tx: event_tx,
                },
            );
            shield_tx
        };

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
                self.state.lock().relay_sessions.remove(&connection_id);
                return Err(anyhow!("shield rejected tunnel: {}", error));
            }
            _ => {
                self.state.lock().relay_sessions.remove(&connection_id);
                return Err(anyhow!("tunnel session closed before TunnelOpened"));
            }
        }

        Ok(RelaySession {
            connection_id,
            event_rx,
            shield_tx,
            cancel_token: CancellationToken::new(),
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
    cancel_token: CancellationToken, // defaults to a token that's never cancelled
}

impl RelaySession {
    // -------------------------------------------------------------------
    // Sprint 15 Phase 2: optional external cancellation, added as a builder
    // method (not a relay_stream/open_relay_session parameter) specifically
    // to respect the M4 API contract documented on open_relay_session above.
    // -------------------------------------------------------------------

    /// Attaches an external cancellation token so relay_stream's d2s child
    /// task can independently notice cancellation, even if the caller's own
    /// future (wrapping relay_stream in a select!) is dropped without ever
    /// reaching relay_stream's own cleanup path. Optional — a caller that
    /// never calls this keeps today's exact behavior.
    pub fn with_cancel_token(mut self, token: CancellationToken) -> Self {
        self.cancel_token = token;
        self
    }

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
        let d2s_token = self.cancel_token.clone();
        let d2s = tokio::spawn(async move {
            let mut buf = vec![0u8; MAX_CHUNK];
            loop {
                tokio::select! {
                    _ = d2s_token.cancelled() => break,
                    result = reader.read(&mut buf) => {
                        match result {
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

#[cfg(test)]
mod stream_ownership_tests {
    //! Fix 14: a Shield's relay sessions belong to the Control stream their
    //! tunnel runs on (F44), and an older stream's cleanup never removes a
    //! newer stream's registration (F45).

    use super::*;
    use std::time::Duration;
    use tokio::io::AsyncReadExt;

    const SHIELD: &str = "5d1c7f0e-2b8a-4e6d-9c3f-1a7b2e4d6f80";
    const WAIT: Duration = Duration::from_secs(5);

    /// A Shield on `rx` that accepts every tunnel.
    fn accept_all(hub: AgentTunnelHub, mut rx: mpsc::Receiver<ShieldControlMessage>) {
        tokio::spawn(async move {
            while let Some(m) = rx.recv().await {
                if let Some(Body::TunnelOpen(o)) = m.body {
                    hub.dispatch_opened(&o.connection_id, true, String::new());
                }
            }
        });
    }

    #[tokio::test]
    async fn a_stream_end_closes_its_sessions_instead_of_hanging() {
        let hub = AgentTunnelHub::new();
        let (tx, rx) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        accept_all(hub.clone(), rx);
        let session = hub
            .open_relay_session(SHIELD, "10.0.0.9", 22, "tcp")
            .await
            .expect("opened");
        let (mut device, relay_end) = tokio::io::duplex(1024);
        let relay = tokio::spawn(session.relay_stream(relay_end));

        hub.unregister_shield(SHIELD, 1);

        tokio::time::timeout(WAIT, relay)
            .await
            .expect("the session must end when its stream ends")
            .unwrap()
            .ok();
        let mut buf = [0u8; 8];
        let n = tokio::time::timeout(WAIT, device.read(&mut buf))
            .await
            .expect("the device must see the end, not hang")
            .unwrap_or(0);
        assert_eq!(n, 0);
        assert!(hub.state.lock().relay_sessions.is_empty());
        assert!(!hub.state.lock().shield_txs.contains_key(SHIELD));
    }

    #[tokio::test]
    async fn an_older_streams_cleanup_keeps_the_newer_stream() {
        let hub = AgentTunnelHub::new();
        let (old_tx, old_rx) = mpsc::channel(64);
        let (new_tx, new_rx) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, old_tx));
        accept_all(hub.clone(), old_rx);
        let on_old = hub
            .open_relay_session(SHIELD, "10.0.0.9", 22, "tcp")
            .await
            .expect("opened on stream 1");
        // Stream 2 registers before stream 1's end is noticed.
        assert!(hub.register_shield(SHIELD.into(), 2, new_tx));
        accept_all(hub.clone(), new_rx);
        let on_new = hub
            .open_relay_session(SHIELD, "10.0.0.9", 22, "tcp")
            .await
            .expect("opened on stream 2");

        hub.unregister_shield(SHIELD, 1);

        {
            let state = hub.state.lock();
            assert_eq!(
                state.shield_txs.get(SHIELD).map(|(id, _)| *id),
                Some(2),
                "stream 1's cleanup removed stream 2"
            );
            assert!(!state.relay_sessions.contains_key(&on_old.connection_id));
            assert!(state.relay_sessions.contains_key(&on_new.connection_id));
        }
        // Stream 2 still opens sessions.
        hub.open_relay_session(SHIELD, "10.0.0.9", 22, "tcp")
            .await
            .expect("stream 2 still serves");
    }

    #[tokio::test]
    async fn a_stream_end_leaves_other_shields_alone() {
        const OTHER: &str = "9e8d7c6b-5a49-4382-a1b0-c9d8e7f6a5b4";
        let hub = AgentTunnelHub::new();
        let (tx, rx) = mpsc::channel(64);
        let (other_tx, other_rx) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        assert!(hub.register_shield(OTHER.into(), 1, other_tx));
        accept_all(hub.clone(), rx);
        accept_all(hub.clone(), other_rx);
        let other = hub
            .open_relay_session(OTHER, "10.0.0.8", 22, "tcp")
            .await
            .expect("opened");

        hub.unregister_shield(SHIELD, 1);

        assert!(hub
            .state
            .lock()
            .relay_sessions
            .contains_key(&other.connection_id));
    }
}

#[cfg(test)]
mod backpressure_tests {
    //! Fix 14 (F48): a full device queue ends the session; no chunk is
    //! dropped while the session carries on.

    use super::*;

    const SHIELD: &str = "5d1c7f0e-2b8a-4e6d-9c3f-1a7b2e4d6f80";

    #[tokio::test]
    async fn a_full_device_queue_ends_the_session_instead_of_dropping_data() {
        let hub = AgentTunnelHub::new();
        let (tx, mut shield_rx) = mpsc::channel(256);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        let opener = hub.clone();
        let opening =
            tokio::spawn(async move { opener.open_relay_session(SHIELD, "10.0.0.9", 22, "tcp").await });
        let Some(Body::TunnelOpen(open)) = shield_rx.recv().await.unwrap().body else {
            panic!("expected TunnelOpen");
        };
        hub.dispatch_opened(&open.connection_id, true, String::new());
        let mut session = opening.await.unwrap().expect("opened");

        // The device reads nothing: the queue (64) fills, then one more chunk.
        for i in 0..65u8 {
            hub.dispatch_data(&open.connection_id, vec![i]);
        }

        assert!(
            !hub.state.lock().relay_sessions.contains_key(&open.connection_id),
            "the session carried on after a chunk could not be queued"
        );
        let Some(Body::TunnelClose(close)) = shield_rx.recv().await.unwrap().body else {
            panic!("the shield was not told to close its side");
        };
        assert_eq!(close.connection_id, open.connection_id);
        // The device still gets every chunk that was queued, in order, then the end.
        for i in 0..64u8 {
            match session.event_rx.recv().await {
                Some(RelayEvent::Data(d)) => assert_eq!(d, vec![i]),
                other => panic!("chunk {i}: got {}", other.is_some()),
            }
        }
        assert!(session.event_rx.recv().await.is_none());
    }
}

#[cfg(test)]
mod revocation_tests {
    //! Certificate audit bug 3: a revoked shield is refused by the hub and
    //! every session through it ends.

    use super::*;
    use std::time::Duration;

    const SHIELD: &str = "5d1c7f0e-2b8a-4e6d-9c3f-1a7b2e4d6f80";
    const WAIT: Duration = Duration::from_secs(5);

    #[test]
    fn a_revoked_shield_cannot_register_again() {
        let hub = AgentTunnelHub::new();
        let (tx, _rx) = mpsc::channel(8);
        assert!(hub.register_shield(SHIELD.into(), 1, tx.clone()));
        hub.revoke_shield(SHIELD);
        assert!(!hub.register_shield(SHIELD.into(), 1, tx));
        assert!(!hub.state.lock().shield_txs.contains_key(SHIELD));
    }

    #[tokio::test]
    async fn no_session_opens_through_a_revoked_shield() {
        let hub = AgentTunnelHub::new();
        let (tx, _rx) = mpsc::channel(8);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        hub.revoke_shield(SHIELD);
        let err = hub
            .open_relay_session(SHIELD, "10.0.0.9", 22, "tcp")
            .await
            .err()
            .expect("refused");
        assert!(err.to_string().contains("revoked"), "{err}");
    }

    #[tokio::test]
    async fn revocation_ends_a_session_waiting_for_tunnel_opened() {
        let hub = AgentTunnelHub::new();
        let (tx, mut rx) = mpsc::channel(8);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        let opener = hub.clone();
        let opening = tokio::spawn(async move {
            opener
                .open_relay_session(SHIELD, "10.0.0.9", 22, "tcp")
                .await
        });
        // The shield received TunnelOpen but never answers.
        let _open = tokio::time::timeout(WAIT, rx.recv())
            .await
            .expect("TunnelOpen sent");
        hub.revoke_shield(SHIELD);
        let result = tokio::time::timeout(WAIT, opening)
            .await
            .expect("open must end when the shield is revoked")
            .unwrap();
        assert!(result.is_err());
        assert!(hub.state.lock().relay_sessions.is_empty());
    }
}
