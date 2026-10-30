use parking_lot::Mutex;
use std::collections::{HashMap, HashSet, VecDeque};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, Result};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::mpsc;
use tokio_util::sync::CancellationToken;
use tracing::{info, warn};
use uuid::Uuid;

use crate::shield_proto::shield_control_message::Body;
use crate::shield_proto::{ShieldControlMessage, TunnelClose, TunnelData, TunnelOpen};

const MAX_CHUNK: usize = 16 * 1024;
/// Fix 14 (F48): how long a Shield chunk waits for room in a device's full
/// tunnel queue (no progress at all) before the session is ended. Accepted
/// limit (user, 2026-10-26): while it waits, the Shield's whole Control
/// stream waits, so a device reading just under this pace delays the other
/// sessions through that Shield; per-tunnel flow control is the follow-up.
/// A total stall budget was tried and ended every steadily slower download.
const DEVICE_STALL_LIMIT: Duration = Duration::from_secs(2);
/// Fix 14 handover: how many Shield events a session may hold while they
/// arrive on the Shield's newer stream before its older stream has ended.
/// More ends the session (never drop and carry on).
const EARLY_EVENT_LIMIT: usize = 256;
/// ... and how many bytes, and for how long (the handover grace): a Shield
/// that opens a second stream and never ends the first cannot make the
/// Connector hold its data without bound.
const EARLY_BYTES_LIMIT: usize = 1024 * 1024;
/// Fix 14 handover: how long device data waits for the session's stream to
/// be replaced after its send channel closed.
const HANDOVER_SEND_GRACE: Duration = Duration::from_secs(15);
/// F-7: upper bound for shutting the device stream down after a clean end
/// (inner TLS close_notify, then FIN). The close_notify queues behind all data
/// still in flight, so a slow reader needs as long as it takes to drain: 2 s
/// cut every slow download's end. Cancellation ends it sooner.
const DEVICE_SHUTDOWN_GRACE: Duration = Duration::from_secs(120);

// ---------------------------------------------------------------------------
// Events dispatched from Shield → relay session
// ---------------------------------------------------------------------------

#[derive(Debug)]
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
    /// stream_id → the channel for sending to that live Control stream. A
    /// session sends on its own stream (`RelayEntry::stream_id`), which may be
    /// older than the Shield's current one until it is handed over.
    stream_txs: HashMap<u64, mpsc::Sender<ShieldControlMessage>>,
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
    /// Fix 14 handover: Shield events that must wait their turn — they came
    /// on a newer stream before this session's stream ended, or while the
    /// held ones are still being delivered (`flushing`).
    early: VecDeque<RelayEvent>,
    flushing: bool,
    /// Bytes held in `early`, and since when it has been non-empty.
    early_bytes: usize,
    early_since: Option<tokio::time::Instant>,
}

impl RelayEntry {
    /// Hold `event` until the handover delivers it. False — holding nothing —
    /// once the held events would pass their count, byte or time limit.
    fn hold(&mut self, event: RelayEvent) -> bool {
        let bytes = match &event {
            RelayEvent::Data(d) => d.len(),
            _ => 0,
        };
        let now = tokio::time::Instant::now();
        let since = *self.early_since.get_or_insert(now);
        if self.early.len() >= EARLY_EVENT_LIMIT
            || self.early_bytes + bytes > EARLY_BYTES_LIMIT
            || now.duration_since(since) > HANDOVER_SEND_GRACE
        {
            return false;
        }
        self.early_bytes += bytes;
        self.early.push_back(event);
        true
    }

    /// The next held event, in order.
    fn next_held(&mut self) -> Option<RelayEvent> {
        let event = self.early.pop_front();
        if let Some(RelayEvent::Data(d)) = &event {
            self.early_bytes -= d.len();
        }
        if self.early.is_empty() {
            self.early_since = None;
        }
        event
    }

    /// Events for this session must queue behind `early` rather than go
    /// straight to the device: they came on another stream than the
    /// session's, or held events are still being delivered.
    fn must_queue(&self, from_stream: u64) -> bool {
        self.stream_id != from_stream || self.flushing
    }
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
        state.stream_txs.insert(stream_id, tx.clone());
        state.shield_txs.insert(shield_id, (stream_id, tx));
        true
    }

    /// Stream `stream_id` of `shield_id` ended. Fix 14:
    /// - F45: the Shield's registration is removed only if it is still this
    ///   stream's; a newer stream that registered first stays.
    /// - Handover (D-1 = B): if the Shield ended this stream cleanly (`clean`:
    ///   it half-closed it after sending everything) and a newer stream of the
    ///   same Shield is already registered here, this stream's sessions move
    ///   to the newer stream and keep running. The Shield does this on a
    ///   certificate renewal (make before break).
    /// - Otherwise (F44) every session on this stream ends now: its queued
    ///   tunnel data died with the stream, so it must not carry on (a hole in
    ///   its byte stream) nor hang.
    ///
    /// Returns the sessions handed over. Each must then get its held events
    /// with `deliver_held_events`, after this stream's last message.
    pub fn end_stream(&self, shield_id: &str, stream_id: u64, clean: bool) -> Vec<String> {
        let mut state = self.state.lock();
        state.stream_txs.remove(&stream_id);
        if state
            .shield_txs
            .get(shield_id)
            .is_some_and(|(id, _)| *id == stream_id)
        {
            state.shield_txs.remove(shield_id);
        }
        let newer = state
            .shield_txs
            .get(shield_id)
            .map(|(id, _)| *id)
            .filter(|id| clean && *id > stream_id);
        let on_stream: Vec<String> = state
            .relay_sessions
            .iter()
            .filter(|(_, e)| e.shield_id == shield_id && e.stream_id == stream_id)
            .map(|(cid, _)| cid.clone())
            .collect();
        let mut handed_over = Vec::new();
        for cid in on_stream {
            match newer {
                Some(newer) => {
                    let entry = state.relay_sessions.get_mut(&cid).expect("listed above");
                    entry.stream_id = newer;
                    entry.flushing = !entry.early.is_empty();
                    handed_over.push(cid);
                }
                None => {
                    if let Some(entry) = state.relay_sessions.remove(&cid) {
                        // Best effort: dropping the only sender ends the session anyway.
                        let _ = entry
                            .tx
                            .try_send(RelayEvent::Close("shield stream ended".to_string()));
                    }
                }
            }
        }
        if !handed_over.is_empty() {
            info!(
                shield_id,
                sessions = handed_over.len(),
                "shield Control stream handed over: sessions kept"
            );
        }
        handed_over
    }

    /// Deliver, in order, the Shield events a handed-over session held while
    /// its old stream was ending. Later events keep queueing behind them
    /// (`flushing`) until all are delivered.
    pub async fn deliver_held_events(&self, connection_id: &str) {
        loop {
            let (tx, event) = {
                let mut state = self.state.lock();
                let Some(entry) = state.relay_sessions.get_mut(connection_id) else {
                    return;
                };
                match entry.next_held() {
                    None => {
                        entry.flushing = false;
                        return;
                    }
                    Some(RelayEvent::Close(err)) => {
                        let entry = state.relay_sessions.remove(connection_id).expect("present");
                        let _ = entry.tx.try_send(RelayEvent::Close(err));
                        return;
                    }
                    Some(event) => (entry.tx.clone(), event),
                }
            };
            if !self.deliver(connection_id, &tx, event).await {
                return;
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

    pub async fn dispatch_data(&self, connection_id: &str, data: Vec<u8>, from_stream: u64) {
        let tx = {
            let mut state = self.state.lock();
            let Some(entry) = state.relay_sessions.get_mut(connection_id) else {
                return;
            };
            if entry.must_queue(from_stream) {
                // Fix 14 handover: wait behind the session's older stream.
                if entry.hold(RelayEvent::Data(data)) {
                    return;
                }
                drop(state);
                self.end_stuck_session(connection_id, None, "handover backlog over its limit");
                return;
            }
            entry.tx.clone()
        };
        self.deliver(connection_id, &tx, RelayEvent::Data(data)).await;
    }

    /// Hand `event` to the session's device side. Fix 14 (F48): never drop a
    /// chunk and carry on (a hole in the byte stream). If the device queue is
    /// full, wait for room: that pauses this Shield's Control stream, which
    /// pushes back to the resource (flow control). A device that makes no
    /// progress for DEVICE_STALL_LIMIT ends its session. Returns whether the
    /// session goes on.
    async fn deliver(
        &self,
        connection_id: &str,
        tx: &mpsc::Sender<RelayEvent>,
        event: RelayEvent,
    ) -> bool {
        match tx.send_timeout(event, DEVICE_STALL_LIMIT).await {
            Ok(()) => true,
            Err(mpsc::error::SendTimeoutError::Closed(_)) => false,
            Err(mpsc::error::SendTimeoutError::Timeout(_)) => {
                self.end_stuck_session(
                    connection_id,
                    Some(tx),
                    "device stalled with its tunnel queue full",
                );
                false
            }
        }
    }

    /// End a session that cannot take more Shield data, and tell the Shield.
    /// With `only_if`, only if it is still the session behind that channel.
    /// Removing the entry drops the only sender: the device gets the chunks
    /// already queued, then the end.
    fn end_stuck_session(
        &self,
        connection_id: &str,
        only_if: Option<&mpsc::Sender<RelayEvent>>,
        reason: &'static str,
    ) {
        let shield_tx = {
            let mut state = self.state.lock();
            if !state
                .relay_sessions
                .get(connection_id)
                .is_some_and(|e| only_if.is_none_or(|tx| e.tx.same_channel(tx)))
            {
                return;
            }
            let entry = state
                .relay_sessions
                .remove(connection_id)
                .expect("checked above");
            state.stream_txs.get(&entry.stream_id).cloned()
        };
        warn!(connection_id, reason, "ending the session");
        if let Some(tx) = shield_tx {
            let _ = tx.try_send(ShieldControlMessage {
                body: Some(Body::TunnelClose(TunnelClose {
                    connection_id: connection_id.to_string(),
                    error: "device too slow".to_string(),
                })),
            });
        }
    }

    pub fn dispatch_close(&self, connection_id: &str, error: String, from_stream: u64) {
        let mut state = self.state.lock();
        let Some(entry) = state.relay_sessions.get_mut(connection_id) else {
            return;
        };
        if entry.must_queue(from_stream) {
            // Fix 14 handover: the close waits behind the held data. Past the
            // limits, the session simply ends now.
            if !entry.hold(RelayEvent::Close(error.clone())) {
                let entry = state.relay_sessions.remove(connection_id).expect("present");
                let _ = entry.tx.try_send(RelayEvent::Close(error));
            }
            return;
        }
        // Remove session — Shield closed its side.
        let entry = state.relay_sessions.remove(connection_id).expect("present");
        let _ = entry.tx.try_send(RelayEvent::Close(error));
    }

    /// The channel to the Control stream `connection_id`'s tunnel runs on.
    fn stream_sender(&self, connection_id: &str) -> Option<mpsc::Sender<ShieldControlMessage>> {
        let state = self.state.lock();
        let entry = state.relay_sessions.get(connection_id)?;
        state.stream_txs.get(&entry.stream_id).cloned()
    }

    /// Send a device-side message for `connection_id` to its Shield. If the
    /// session's stream closed under it (a handover in progress), retry on the
    /// stream it moves to; give up when the session is gone or the grace
    /// passes. The message that failed is retried, so none is lost or
    /// reordered: everything sent before it went out on the old stream, which
    /// the Shield reads to its end first.
    async fn send_to_shield(&self, connection_id: &str, mut msg: ShieldControlMessage) -> bool {
        let deadline = tokio::time::Instant::now() + HANDOVER_SEND_GRACE;
        let mut failed: Option<mpsc::Sender<ShieldControlMessage>> = None;
        loop {
            let tx = self.stream_sender(connection_id);
            match tx {
                Some(tx) if !failed.as_ref().is_some_and(|f| f.same_channel(&tx)) => {
                    match tx.send(msg).await {
                        Ok(()) => return true,
                        Err(mpsc::error::SendError(m)) => {
                            msg = m;
                            failed = Some(tx);
                        }
                    }
                }
                None if !self.state.lock().relay_sessions.contains_key(connection_id) => {
                    return false;
                }
                _ => {}
            }
            if tokio::time::Instant::now() >= deadline {
                return false;
            }
            tokio::time::sleep(Duration::from_millis(10)).await;
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
                    early: VecDeque::new(),
                    flushing: false,
                    early_bytes: 0,
                    early_since: None,
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
            hub: self.clone(),
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
    /// Device data goes to the session's current stream, looked up per send
    /// (Fix 14 handover), never a sender captured at open time.
    hub: AgentTunnelHub,
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
        let hub = self.hub.clone();
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
                                let msg = ShieldControlMessage {
                                    body: Some(Body::TunnelData(TunnelData {
                                        connection_id: conn_id_d2s.clone(),
                                        data: buf[..n].to_vec(),
                                    })),
                                };
                                if !hub.send_to_shield(&conn_id_d2s, msg).await {
                                    break;
                                }
                            }
                        }
                    }
                }
            }
            // Notify Shield that the device closed its side.
            let close = ShieldControlMessage {
                body: Some(Body::TunnelClose(TunnelClose {
                    connection_id: conn_id_d2s.clone(),
                    error: String::new(),
                })),
            };
            hub.send_to_shield(&conn_id_d2s, close).await;
        });

        // Shield → Device: relay events to the device stream.
        let mut clean_close = false;
        while let Some(event) = self.event_rx.recv().await {
            match event {
                RelayEvent::Data(data) => {
                    if writer.write_all(&data).await.is_err() {
                        break;
                    }
                }
                RelayEvent::Close(err) => {
                    if err.is_empty() {
                        clean_close = true;
                    } else {
                        warn!(connection_id = %self.connection_id, error = %err, "shield closed tunnel with error");
                    }
                    break;
                }
                RelayEvent::Opened { .. } => {} // unexpected after session start
            }
        }

        d2s.abort();
        if clean_close {
            // The resource closed normally: end the device stream properly. On
            // a relayed stream this sends the inner TLS close_notify before the
            // FIN; dropping the stream instead looked like a truncation, so the
            // Client reset the app connection and the tail of every response
            // was lost (live E2E run 2026-10-30, F-7). A failed or lost session
            // still ends abruptly, so the Client can tell it from a normal end.
            tokio::select! {
                _ = tokio::time::timeout(DEVICE_SHUTDOWN_GRACE, writer.shutdown()) => {}
                _ = self.cancel_token.cancelled() => {}
            }
        }
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

    /// A device stream that records whether it was shut down (as opposed to
    /// dropped): a relayed stream needs the shutdown to send close_notify.
    struct ShutdownProbe {
        inner: tokio::io::DuplexStream,
        shut: Arc<std::sync::atomic::AtomicBool>,
    }

    impl tokio::io::AsyncRead for ShutdownProbe {
        fn poll_read(
            mut self: std::pin::Pin<&mut Self>,
            cx: &mut std::task::Context<'_>,
            buf: &mut tokio::io::ReadBuf<'_>,
        ) -> std::task::Poll<std::io::Result<()>> {
            std::pin::Pin::new(&mut self.inner).poll_read(cx, buf)
        }
    }

    impl tokio::io::AsyncWrite for ShutdownProbe {
        fn poll_write(
            mut self: std::pin::Pin<&mut Self>,
            cx: &mut std::task::Context<'_>,
            buf: &[u8],
        ) -> std::task::Poll<std::io::Result<usize>> {
            std::pin::Pin::new(&mut self.inner).poll_write(cx, buf)
        }
        fn poll_flush(
            mut self: std::pin::Pin<&mut Self>,
            cx: &mut std::task::Context<'_>,
        ) -> std::task::Poll<std::io::Result<()>> {
            std::pin::Pin::new(&mut self.inner).poll_flush(cx)
        }
        fn poll_shutdown(
            mut self: std::pin::Pin<&mut Self>,
            cx: &mut std::task::Context<'_>,
        ) -> std::task::Poll<std::io::Result<()>> {
            self.shut.store(true, std::sync::atomic::Ordering::SeqCst);
            std::pin::Pin::new(&mut self.inner).poll_shutdown(cx)
        }
    }

    /// Runs one session that receives `data` and then a Close with `error`;
    /// returns what the device read and whether its stream was shut down.
    async fn session_end(error: &str) -> (Vec<u8>, bool) {
        let hub = AgentTunnelHub::new();
        let (tx, rx) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        accept_all(hub.clone(), rx);
        let session = hub
            .open_relay_session(SHIELD, "10.0.0.9", 80, "tcp")
            .await
            .expect("opened");
        let connection_id = session.connection_id.clone();
        let (mut device, relay_end) = tokio::io::duplex(64 * 1024);
        let shut = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let relay = tokio::spawn(session.relay_stream(ShutdownProbe {
            inner: relay_end,
            shut: shut.clone(),
        }));
        hub.dispatch_data(&connection_id, b"HTTP/1.0 200 OK\r\n\r\nbody".to_vec(), 1)
            .await;
        hub.dispatch_close(&connection_id, error.to_string(), 1);
        tokio::time::timeout(WAIT, relay)
            .await
            .expect("session ends")
            .unwrap()
            .unwrap();
        let mut got = Vec::new();
        tokio::time::timeout(WAIT, device.read_to_end(&mut got))
            .await
            .expect("device sees the end")
            .unwrap();
        (got, shut.load(std::sync::atomic::Ordering::SeqCst))
    }

    /// F-7 (live E2E run 2026-10-30): a resource that closes normally ends the
    /// device stream with a shutdown (close_notify on a relayed stream), after
    /// all its data; a Shield-reported error still ends it abruptly.
    #[tokio::test]
    async fn a_clean_close_shuts_the_device_stream_down_an_error_does_not() {
        let (got, shut) = session_end("").await;
        assert_eq!(got, b"HTTP/1.0 200 OK\r\n\r\nbody");
        assert!(shut, "a clean close must shut the device stream down");

        let (_, shut) = session_end("resource reset").await;
        assert!(!shut, "an error close must not look like a clean end");
    }

    /// A device stream whose shutdown completes only once the reader has
    /// drained it, as a relayed stream's close_notify queues behind the data
    /// still in flight. Records whether the shutdown completed.
    struct DrainGatedShutdown {
        inner: tokio::io::DuplexStream,
        drained: Arc<std::sync::atomic::AtomicBool>,
        waker: Arc<parking_lot::Mutex<Option<std::task::Waker>>>,
        completed: Arc<std::sync::atomic::AtomicBool>,
    }

    impl tokio::io::AsyncRead for DrainGatedShutdown {
        fn poll_read(
            mut self: std::pin::Pin<&mut Self>,
            cx: &mut std::task::Context<'_>,
            buf: &mut tokio::io::ReadBuf<'_>,
        ) -> std::task::Poll<std::io::Result<()>> {
            std::pin::Pin::new(&mut self.inner).poll_read(cx, buf)
        }
    }

    impl tokio::io::AsyncWrite for DrainGatedShutdown {
        fn poll_write(
            mut self: std::pin::Pin<&mut Self>,
            cx: &mut std::task::Context<'_>,
            buf: &[u8],
        ) -> std::task::Poll<std::io::Result<usize>> {
            std::pin::Pin::new(&mut self.inner).poll_write(cx, buf)
        }
        fn poll_flush(
            mut self: std::pin::Pin<&mut Self>,
            cx: &mut std::task::Context<'_>,
        ) -> std::task::Poll<std::io::Result<()>> {
            std::pin::Pin::new(&mut self.inner).poll_flush(cx)
        }
        fn poll_shutdown(
            mut self: std::pin::Pin<&mut Self>,
            cx: &mut std::task::Context<'_>,
        ) -> std::task::Poll<std::io::Result<()>> {
            if !self.drained.load(std::sync::atomic::Ordering::SeqCst) {
                *self.waker.lock() = Some(cx.waker().clone());
                return std::task::Poll::Pending;
            }
            self.completed
                .store(true, std::sync::atomic::Ordering::SeqCst);
            std::pin::Pin::new(&mut self.inner).poll_shutdown(cx)
        }
    }

    /// F-7: the close_notify queues behind data still in flight, so a reader
    /// slower than the old 2 s grace must still get the clean end.
    #[tokio::test(start_paused = true)]
    async fn a_slow_reader_still_gets_the_clean_end() {
        let hub = AgentTunnelHub::new();
        let (tx, rx) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        accept_all(hub.clone(), rx);
        let session = hub
            .open_relay_session(SHIELD, "10.0.0.9", 80, "tcp")
            .await
            .expect("opened");
        let connection_id = session.connection_id.clone();
        let (_device, relay_end) = tokio::io::duplex(1024);
        let drained = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let waker = Arc::new(parking_lot::Mutex::new(None::<std::task::Waker>));
        let completed = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let relay = tokio::spawn(session.relay_stream(DrainGatedShutdown {
            inner: relay_end,
            drained: drained.clone(),
            waker: waker.clone(),
            completed: completed.clone(),
        }));
        hub.dispatch_data(&connection_id, b"body".to_vec(), 1).await;
        hub.dispatch_close(&connection_id, String::new(), 1);
        // The slow reader takes 10 s to drain what is in flight.
        tokio::time::sleep(Duration::from_secs(10)).await;
        drained.store(true, std::sync::atomic::Ordering::SeqCst);
        if let Some(w) = waker.lock().take() {
            w.wake();
        }
        tokio::time::timeout(WAIT, relay)
            .await
            .expect("session ends")
            .unwrap()
            .unwrap();
        assert!(
            completed.load(std::sync::atomic::Ordering::SeqCst),
            "the shutdown must still complete after a slow drain"
        );
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

        hub.end_stream(SHIELD, 1, false);

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

        hub.end_stream(SHIELD, 1, false);

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

        hub.end_stream(SHIELD, 1, false);

        assert!(hub
            .state
            .lock()
            .relay_sessions
            .contains_key(&other.connection_id));
    }
}

#[cfg(test)]
mod handover_tests {
    //! Fix 14, D-1 = B: on a clean end of a Shield's stream, while a newer
    //! stream of the same Shield is registered, the sessions move to the newer
    //! stream with no byte lost or reordered in either direction. Any other
    //! end still closes them.

    use super::*;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    const SHIELD: &str = "5d1c7f0e-2b8a-4e6d-9c3f-1a7b2e4d6f80";
    const WAIT: Duration = Duration::from_secs(5);

    async fn open_on(
        hub: &AgentTunnelHub,
        shield_rx: &mut mpsc::Receiver<ShieldControlMessage>,
    ) -> (String, RelaySession) {
        let opener = hub.clone();
        let opening = tokio::spawn(async move {
            opener
                .open_relay_session(SHIELD, "10.0.0.9", 22, "tcp")
                .await
        });
        let Some(Body::TunnelOpen(open)) = shield_rx.recv().await.unwrap().body else {
            panic!("expected TunnelOpen");
        };
        hub.dispatch_opened(&open.connection_id, true, String::new());
        (open.connection_id, opening.await.unwrap().expect("opened"))
    }

    async fn next_data(rx: &mut mpsc::Receiver<ShieldControlMessage>) -> Vec<u8> {
        match tokio::time::timeout(WAIT, rx.recv()).await.expect("data in time") {
            Some(ShieldControlMessage {
                body: Some(Body::TunnelData(d)),
            }) => d.data,
            other => panic!("expected TunnelData, got {other:?}"),
        }
    }

    async fn read_exact_n(device: &mut tokio::io::DuplexStream, n: usize) -> Vec<u8> {
        let mut buf = vec![0u8; n];
        tokio::time::timeout(WAIT, device.read_exact(&mut buf))
            .await
            .expect("bytes in time")
            .expect("device open");
        buf
    }

    #[tokio::test]
    async fn a_clean_handover_keeps_the_session_in_order_both_ways() {
        let hub = AgentTunnelHub::new();
        let (tx1, mut rx1) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx1));
        let (cid, session) = open_on(&hub, &mut rx1).await;
        let (mut device, relay_end) = tokio::io::duplex(1024);
        let relay = tokio::spawn(session.relay_stream(relay_end));

        // Shield → device on stream 1, and device → shield on stream 1.
        hub.dispatch_data(&cid, vec![1], 1).await;
        hub.dispatch_data(&cid, vec![2], 1).await;
        device.write_all(b"a").await.unwrap();
        assert_eq!(next_data(&mut rx1).await, b"a");

        // The Shield opens stream 2 (renewal), switches to it and sends 3;
        // that overtakes stream 1's last chunk, 4.
        let (tx2, mut rx2) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 2, tx2));
        hub.dispatch_data(&cid, vec![3], 2).await;
        hub.dispatch_data(&cid, vec![4], 1).await;
        // Stream 1 ends cleanly: the session moves to stream 2.
        assert_eq!(hub.end_stream(SHIELD, 1, true), vec![cid.clone()]);
        hub.deliver_held_events(&cid).await;
        hub.dispatch_data(&cid, vec![5], 2).await;

        assert_eq!(read_exact_n(&mut device, 5).await, vec![1, 2, 4, 3, 5]);
        // Device → shield now goes out on stream 2.
        device.write_all(b"b").await.unwrap();
        assert_eq!(next_data(&mut rx2).await, b"b");
        assert!(!relay.is_finished(), "the session must survive the handover");
    }

    #[tokio::test]
    async fn device_data_whose_stream_closed_is_retried_on_the_new_stream() {
        let hub = AgentTunnelHub::new();
        let (tx1, mut rx1) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx1));
        let (_cid, session) = open_on(&hub, &mut rx1).await;
        let (mut device, relay_end) = tokio::io::duplex(1024);
        let _relay = tokio::spawn(session.relay_stream(relay_end));
        let (tx2, mut rx2) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 2, tx2));

        // Stream 1's queue is already closed (its task is draining) when the
        // device sends: the send fails and must be retried, not lost.
        rx1.close();
        device.write_all(b"c").await.unwrap();
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert_eq!(hub.end_stream(SHIELD, 1, true).len(), 1);
        assert_eq!(next_data(&mut rx2).await, b"c");
    }

    #[tokio::test]
    async fn a_clean_end_without_a_newer_stream_closes_the_session() {
        let hub = AgentTunnelHub::new();
        let (tx1, mut rx1) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx1));
        let (cid, _session) = open_on(&hub, &mut rx1).await;
        assert!(hub.end_stream(SHIELD, 1, true).is_empty());
        assert!(!hub.state.lock().relay_sessions.contains_key(&cid));
    }

    #[tokio::test]
    async fn an_unclean_end_closes_the_session_even_with_a_newer_stream() {
        let hub = AgentTunnelHub::new();
        let (tx1, mut rx1) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx1));
        let (cid, _session) = open_on(&hub, &mut rx1).await;
        let (tx2, _rx2) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 2, tx2));
        assert!(hub.end_stream(SHIELD, 1, false).is_empty());
        assert!(!hub.state.lock().relay_sessions.contains_key(&cid));
    }

    #[tokio::test]
    async fn an_early_close_comes_after_the_held_data() {
        let hub = AgentTunnelHub::new();
        let (tx1, mut rx1) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx1));
        let (cid, mut session) = open_on(&hub, &mut rx1).await;
        let (tx2, _rx2) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 2, tx2));
        hub.dispatch_data(&cid, vec![7], 2).await;
        hub.dispatch_close(&cid, String::new(), 2);
        hub.dispatch_data(&cid, vec![6], 1).await;
        assert_eq!(hub.end_stream(SHIELD, 1, true).len(), 1);
        hub.deliver_held_events(&cid).await;

        assert!(matches!(session.event_rx.recv().await, Some(RelayEvent::Data(d)) if d == vec![6]));
        assert!(matches!(session.event_rx.recv().await, Some(RelayEvent::Data(d)) if d == vec![7]));
        assert!(matches!(session.event_rx.recv().await, Some(RelayEvent::Close(_))));
        assert!(!hub.state.lock().relay_sessions.contains_key(&cid));
    }

    #[tokio::test]
    async fn a_handover_backlog_over_its_byte_limit_ends_the_session() {
        let hub = AgentTunnelHub::new();
        let (tx1, mut rx1) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx1));
        let (cid, _session) = open_on(&hub, &mut rx1).await;
        let (tx2, _rx2) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 2, tx2));
        hub.dispatch_data(&cid, vec![0; EARLY_BYTES_LIMIT], 2).await;
        assert!(hub.state.lock().relay_sessions.contains_key(&cid));
        hub.dispatch_data(&cid, vec![0], 2).await;
        assert!(!hub.state.lock().relay_sessions.contains_key(&cid));
    }

    #[tokio::test(start_paused = true)]
    async fn a_handover_backlog_held_past_the_grace_ends_the_session() {
        let hub = AgentTunnelHub::new();
        let (tx1, mut rx1) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx1));
        let (cid, _session) = open_on(&hub, &mut rx1).await;
        let (tx2, _rx2) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 2, tx2));
        hub.dispatch_data(&cid, vec![0], 2).await;
        tokio::time::advance(HANDOVER_SEND_GRACE + Duration::from_secs(1)).await;
        hub.dispatch_data(&cid, vec![0], 2).await;
        assert!(!hub.state.lock().relay_sessions.contains_key(&cid));
    }

    #[tokio::test]
    async fn an_overlong_handover_backlog_ends_the_session() {
        let hub = AgentTunnelHub::new();
        let (tx1, mut rx1) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 1, tx1));
        let (cid, _session) = open_on(&hub, &mut rx1).await;
        let (tx2, _rx2) = mpsc::channel(64);
        assert!(hub.register_shield(SHIELD.into(), 2, tx2));
        for _ in 0..=EARLY_EVENT_LIMIT {
            hub.dispatch_data(&cid, vec![0], 2).await;
        }
        assert!(!hub.state.lock().relay_sessions.contains_key(&cid));
    }
}

#[cfg(test)]
mod backpressure_tests {
    //! Fix 14 (F48): a full device queue ends the session; no chunk is
    //! dropped while the session carries on.

    use super::*;

    const SHIELD: &str = "5d1c7f0e-2b8a-4e6d-9c3f-1a7b2e4d6f80";

    async fn opened_session(
        hub: &AgentTunnelHub,
        shield_rx: &mut mpsc::Receiver<ShieldControlMessage>,
    ) -> (String, RelaySession) {
        let opener = hub.clone();
        let opening = tokio::spawn(async move {
            opener
                .open_relay_session(SHIELD, "10.0.0.9", 22, "tcp")
                .await
        });
        let Some(Body::TunnelOpen(open)) = shield_rx.recv().await.unwrap().body else {
            panic!("expected TunnelOpen");
        };
        hub.dispatch_opened(&open.connection_id, true, String::new());
        (open.connection_id, opening.await.unwrap().expect("opened"))
    }

    /// A device that is merely slower than the resource gets every byte:
    /// the chunk waits for room instead of being dropped.
    #[tokio::test]
    async fn a_slow_device_gets_every_chunk_in_order() {
        let hub = AgentTunnelHub::new();
        let (tx, mut shield_rx) = mpsc::channel(256);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        let (cid, mut session) = opened_session(&hub, &mut shield_rx).await;

        let feeder = {
            let hub = hub.clone();
            let cid = cid.clone();
            tokio::spawn(async move {
                for i in 0..200u8 {
                    hub.dispatch_data(&cid, vec![i], 1).await;
                }
            })
        };
        for i in 0..200u8 {
            if i % 50 == 0 {
                tokio::time::sleep(std::time::Duration::from_millis(100)).await;
            }
            match session.event_rx.recv().await {
                Some(RelayEvent::Data(d)) => assert_eq!(d, vec![i], "chunk {i}"),
                _ => panic!("chunk {i} missing"),
            }
        }
        feeder.await.unwrap();
        assert!(hub.state.lock().relay_sessions.contains_key(&cid));
    }

    /// A device that is steadily slower than the resource (a WAN client) keeps
    /// the session's queue full for the whole transfer. It must still get
    /// every chunk, in order, for as long as the transfer lasts.
    #[tokio::test(start_paused = true)]
    async fn a_steadily_slower_device_completes_a_long_transfer() {
        const CHUNKS: u32 = 3000; // 30 s at one chunk per 10 ms
        let hub = AgentTunnelHub::new();
        let (tx, mut shield_rx) = mpsc::channel(256);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        let (cid, mut session) = opened_session(&hub, &mut shield_rx).await;
        let reader = tokio::spawn(async move {
            let mut got = 0u32;
            while let Some(event) = session.event_rx.recv().await {
                match event {
                    RelayEvent::Data(d) => {
                        assert_eq!(d, got.to_be_bytes().to_vec(), "chunk {got} out of order");
                        got += 1;
                    }
                    _ => break,
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            got
        });
        for i in 0..CHUNKS {
            hub.dispatch_data(&cid, i.to_be_bytes().to_vec(), 1).await;
        }
        assert!(
            hub.state.lock().relay_sessions.contains_key(&cid),
            "a merely slow device's session was ended"
        );
        hub.dispatch_close(&cid, String::new(), 1);
        assert_eq!(reader.await.unwrap(), CHUNKS);
    }

    /// A device that stops reading ends its session after the stall limit;
    /// the chunk is never dropped while the session carries on.
    #[tokio::test(start_paused = true)]
    async fn a_stalled_device_ends_the_session_instead_of_dropping_data() {
        let hub = AgentTunnelHub::new();
        let (tx, mut shield_rx) = mpsc::channel(256);
        assert!(hub.register_shield(SHIELD.into(), 1, tx));
        let (cid, mut session) = opened_session(&hub, &mut shield_rx).await;

        for i in 0..64u8 {
            hub.dispatch_data(&cid, vec![i], 1).await;
        }
        // The queue is full and the device reads nothing.
        hub.dispatch_data(&cid, vec![64], 1).await;

        assert!(
            !hub.state.lock().relay_sessions.contains_key(&cid),
            "the session carried on after a chunk could not be queued"
        );
        let Some(Body::TunnelClose(close)) = shield_rx.recv().await.unwrap().body else {
            panic!("the shield was not told to close its side");
        };
        assert_eq!(close.connection_id, cid);
        for i in 0..64u8 {
            match session.event_rx.recv().await {
                Some(RelayEvent::Data(d)) => assert_eq!(d, vec![i]),
                _ => panic!("chunk {i} missing"),
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
