//! Shield-routed tunnels (RDE): device sessions relayed through a Shield.
//!
//! FU-9 (PENDING-21): every tunnel has its own gRPC stream
//! (`ShieldService.Tunnel`). The Control stream only coordinates: the
//! Connector sends `TunnelOpen` on it, the Shield answers by opening a
//! `Tunnel` stream whose first frame names the tunnel (`TunnelStart`), or
//! reports a failure with `TunnelOpened{ok: false}`; an abort from the
//! Connector also goes out on Control (`TunnelClose`), so it reaches the
//! Shield even while the tunnel's own stream is stalled. Data never rides
//! Control, so HTTP/2's per-stream flow control isolates the tunnels: a slow
//! reader holds only its own stream's window, which pushes back to its
//! sender. There is no stall cutoff and no queue that grows without bound.

use parking_lot::Mutex;
use std::collections::{HashMap, HashSet};
use std::future::Future;
use std::pin::Pin;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;
use std::task::{Context, Poll};
use std::time::Duration;

use anyhow::Result;
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::sync::{mpsc, oneshot};
use tokio_stream::{Stream, StreamExt};
use tokio_util::sync::{CancellationToken, WaitForCancellationFutureOwned};
use tonic::Status;
use tracing::{info, warn};
use uuid::Uuid;

use crate::shield_proto::shield_control_message::Body;
use crate::shield_proto::tunnel_frame::Frame;
use crate::shield_proto::{ShieldControlMessage, TunnelClose, TunnelEnd, TunnelFrame, TunnelOpen};

/// Largest TCP data frame (the device read buffer).
const MAX_CHUNK: usize = 16 * 1024;
/// D-3: tunnels (active plus pending) one Shield may have. The Shield
/// enforces the same limit itself (`shield/src/tunnel.rs`).
pub const MAX_TUNNELS_PER_SHIELD: usize = 256;
/// Connector-wide default for `shield_tunnel_limit` (PENDING-21 §7, resource
/// budget): tunnels (active plus pending) across all Shields.
pub const DEFAULT_SHIELD_TUNNEL_LIMIT: usize = 512;
/// How long an open waits for the Shield's `Tunnel` stream: the Shield's 5 s
/// resource connect plus 5 s to open the stream.
pub const TUNNEL_OPEN_TIMEOUT: Duration = Duration::from_secs(10);
/// How long a new `Tunnel` stream has to send its `TunnelStart`.
pub const TUNNEL_START_TIMEOUT: Duration = Duration::from_secs(5);
/// Frames queued toward the Shield beyond HTTP/2's own window. TCP awaits
/// room (backpressure); UDP drops the newest datagram when full (D-4).
const TCP_TO_SHIELD_QUEUE: usize = 4;
const UDP_TO_SHIELD_QUEUE: usize = 32;
/// The device-side UDP framing caps a datagram here (as `relay_udp`).
const MAX_DATAGRAM: usize = 65_535;
/// F-7: upper bound for shutting the device stream down after a clean end
/// (inner TLS close_notify, then FIN). The close_notify queues behind all data
/// still in flight, so a slow reader needs as long as it takes to drain.
/// Cancellation ends it sooner.
const DEVICE_SHUTDOWN_GRACE: Duration = Duration::from_secs(120);

/// Device-facing error code when a tunnel limit refuses a session.
pub const ERR_SHIELD_TUNNEL_LIMIT: &str = "SHIELD_TUNNEL_LIMIT";

/// Why `open_relay_session` failed.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum OpenError {
    Revoked,
    NotConnected,
    /// The Shield already has MAX_TUNNELS_PER_SHIELD tunnels.
    ShieldLimit,
    /// The Connector already has `shield_tunnel_limit` Shield tunnels.
    ConnectorLimit(usize),
    /// The Shield reported it could not open the tunnel.
    Refused(String),
    /// No `Tunnel` stream within TUNNEL_OPEN_TIMEOUT.
    Timeout,
    /// The Shield's Control stream or the session ended first.
    Closed,
    /// The device session's access was revoked (FU-15) before the tunnel
    /// was open.
    SessionRevoked,
}

impl OpenError {
    /// Whether a tunnel limit refused the session.
    pub fn is_limit(&self) -> bool {
        matches!(self, OpenError::ShieldLimit | OpenError::ConnectorLimit(_))
    }
}

impl std::fmt::Display for OpenError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            OpenError::Revoked => write!(f, "shield is revoked"),
            OpenError::NotConnected => write!(f, "shield not connected"),
            OpenError::ShieldLimit => write!(
                f,
                "shield tunnel limit reached ({MAX_TUNNELS_PER_SHIELD} per shield): close a tunnel and retry"
            ),
            OpenError::ConnectorLimit(n) => write!(
                f,
                "shield tunnel limit reached ({n} per connector, shield_tunnel_limit): close a tunnel or raise the limit"
            ),
            OpenError::Refused(e) => write!(f, "shield rejected tunnel: {e}"),
            OpenError::Timeout => write!(
                f,
                "shield did not open the tunnel stream within {}s",
                TUNNEL_OPEN_TIMEOUT.as_secs()
            ),
            OpenError::Closed => write!(f, "tunnel session closed before the shield opened it"),
            OpenError::SessionRevoked => write!(f, "session revoked while the tunnel was opening"),
        }
    }
}

impl std::error::Error for OpenError {}

/// Frames from the Shield on a tunnel's stream (a tonic `Streaming` in
/// production).
pub type InboundFrames = Pin<Box<dyn Stream<Item = Result<TunnelFrame, Status>> + Send>>;

/// A bound tunnel stream, handed from the `Tunnel` handler to the session.
pub struct TunnelIo {
    inbound: InboundFrames,
    outbound: mpsc::Sender<Result<TunnelFrame, Status>>,
    /// When the certificate of the connection the stream runs on expires.
    expires_at: tokio::time::Instant,
}

/// The `Tunnel` response stream: the session's frames to the Shield. When the
/// session is aborted it ends with an error status at once, skipping what is
/// still queued.
pub struct ToShield {
    rx: mpsc::Receiver<Result<TunnelFrame, Status>>,
    /// The session's abort. For a device session it is a child of the
    /// session's revocation token, so a revocation sets it inside `cancel()`.
    abort: CancellationToken,
    /// Wakes this stream when the abort is set.
    aborted: Pin<Box<WaitForCancellationFutureOwned>>,
    done: bool,
}

impl Stream for ToShield {
    type Item = Result<TunnelFrame, Status>;

    fn poll_next(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Option<Self::Item>> {
        let this = &mut *self;
        if this.done {
            return Poll::Ready(None);
        }
        // FU-9 gate-4: the gate every frame to the Shield passes. The abort
        // is read (one atomic load) before each frame is handed to HTTP/2: a
        // frame is sent only if the abort was not yet set at that read, so no
        // frame — whenever the device→shield task read or queued it — leaves
        // once `cancel()` on the session's revocation token has returned.
        // Queued frames are discarded.
        let _ = this.aborted.as_mut().poll(cx); // registers the wake-up
        if this.abort.is_cancelled() {
            this.done = true;
            return Poll::Ready(Some(Err(Status::aborted("tunnel ended by the connector"))));
        }
        this.rx.poll_recv(cx)
    }
}

/// Most aborts a Shield's mailbox holds before its Control stream is ended
/// instead (fail closed). Only tunnels with a stream at the Shield are
/// posted, and while Control is not drained no new tunnel can get one, so
/// in practice it holds at most about MAX_TUNNELS_PER_SHIELD.
pub const ABORT_MAILBOX_CAP: usize = 2 * MAX_TUNNELS_PER_SHIELD;

/// Aborts (`TunnelClose`) waiting to go to one Shield over Control.
///
/// FU-9 (reliable abort): an abort must never be dropped because Control is
/// congested — the tunnel's own stream cannot carry it then (hyper sends
/// nothing on a stream without window, not even its end). Posting never
/// blocks and never fails: one entry per tunnel, bounded by
/// ABORT_MAILBOX_CAP; past that the mailbox is marked overflowed and the
/// Control stream draining it ends, which closes every tunnel of the Shield
/// (F44). The Shield's current Control stream drains it with priority.
#[derive(Debug, Default)]
pub struct AbortMailbox {
    pending: Mutex<Vec<TunnelClose>>,
    overflowed: AtomicBool,
    notify: tokio::sync::Notify,
}

impl AbortMailbox {
    pub(crate) fn post(&self, connection_id: &str, error: &str) {
        {
            let mut pending = self.pending.lock();
            if pending.iter().any(|c| c.connection_id == connection_id) {
                return;
            }
            if pending.len() >= ABORT_MAILBOX_CAP {
                self.overflowed.store(true, Ordering::SeqCst);
            } else {
                pending.push(TunnelClose {
                    connection_id: connection_id.to_string(),
                    error: error.to_string(),
                });
            }
        }
        self.notify.notify_one();
    }

    /// Wait until something was posted.
    pub async fn notified(&self) {
        self.notify.notified().await
    }

    /// Take everything posted, in order, and whether the cap was passed since
    /// the last take (the stream that sees it ends; the next one starts clean).
    pub fn take(&self) -> (Vec<TunnelClose>, bool) {
        let list = std::mem::take(&mut *self.pending.lock());
        (list, self.overflowed.swap(false, Ordering::SeqCst))
    }

    /// Put back aborts that could not be sent (their stream ended); another
    /// Control stream of the Shield delivers them.
    pub fn restore(&self, mut unsent: Vec<TunnelClose>) {
        {
            let mut pending = self.pending.lock();
            unsent.append(&mut pending);
            *pending = unsent;
        }
        self.notify.notify_one();
    }

    /// Aborts waiting (tests).
    #[cfg(test)]
    pub fn pending_len(&self) -> usize {
        self.pending.lock().len()
    }
}

/// What `deliver_aborts` did.
#[derive(Debug, PartialEq, Eq)]
pub enum Drained {
    /// Everything posted went out.
    Delivered,
    /// The stream's queue closed: what was not sent is back in the mailbox.
    StreamClosed,
    /// Everything held went out, but the cap was passed: end the stream.
    Overflowed,
}

/// Send a Shield's posted aborts on one Control stream's queue, in order.
/// Awaits room (Control is the Shield's own stream, which it always reads);
/// never drops one: if the queue closes, the rest go back to the mailbox for
/// the Shield's next stream.
pub async fn deliver_aborts(
    mailbox: &AbortMailbox,
    out: &mpsc::Sender<Result<ShieldControlMessage, Status>>,
) -> Drained {
    let (list, overflowed) = mailbox.take();
    let mut list = list.into_iter();
    while let Some(close) = list.next() {
        let msg = ShieldControlMessage {
            body: Some(Body::TunnelClose(close)),
        };
        if let Err(mpsc::error::SendError(Ok(ShieldControlMessage {
            body: Some(Body::TunnelClose(close)),
        }))) = out.send(Ok(msg)).await
        {
            let mut unsent = vec![close];
            unsent.extend(list);
            mailbox.restore(unsent);
            return Drained::StreamClosed;
        }
    }
    if overflowed {
        Drained::Overflowed
    } else {
        Drained::Delivered
    }
}

// ---------------------------------------------------------------------------
// AgentTunnelHub
// ---------------------------------------------------------------------------

/// Tunnel sessions between device connections and Shields.
///
/// agent_server.rs registers Shields' Control streams (`register_shield`,
/// `end_stream`), binds `Tunnel` streams (`attach`) and reports failures
/// (`dispatch_opened`); device_tunnel.rs opens sessions
/// (`open_relay_session`).
#[derive(Clone, Debug)]
pub struct AgentTunnelHub {
    /// One lock for registrations, sessions and revocations, so a shield
    /// revocation and a session being opened through that shield are
    /// serialized (certificate audit bug 3).
    state: Arc<Mutex<HubState>>,
    /// `shield_tunnel_limit`: Shield tunnels across all Shields.
    limit: Arc<AtomicUsize>,
}

impl Default for AgentTunnelHub {
    fn default() -> Self {
        Self {
            state: Default::default(),
            limit: Arc::new(AtomicUsize::new(DEFAULT_SHIELD_TUNNEL_LIMIT)),
        }
    }
}

#[derive(Debug, Default)]
struct HubState {
    /// shield_id → the Control stream currently registered for that Shield:
    /// its stream id and the channel for sending ShieldControlMessages TO it.
    /// Fix 14 (F45): keyed by shield, owned by one stream — an older stream's
    /// cleanup must not remove a newer stream's registration.
    shield_txs: HashMap<String, (u64, mpsc::Sender<ShieldControlMessage>)>,
    /// shield_id → aborts to deliver to that Shield over Control.
    abort_mailboxes: HashMap<String, Arc<AbortMailbox>>,
    /// connection_id → the session, pending or bound.
    relay_sessions: HashMap<String, RelayEntry>,
    /// Revoked shields (terminal): never registered again, no new sessions.
    revoked: HashSet<String>,
}

#[derive(Debug)]
struct RelayEntry {
    shield_id: String,
    /// The Control stream the tunnel belongs to (F44): its unclean end ends
    /// the tunnel; a clean handover moves the tunnel to the newer stream.
    stream_id: u64,
    udp: bool,
    /// Set while pending: the waiting `open_relay_session` gets the bound
    /// stream (or the Shield's refusal) through it. Taken once (single use).
    pending: Option<oneshot::Sender<Result<TunnelIo, String>>>,
    /// Cancelled to end the session abruptly (revocations, stream ends).
    abort: CancellationToken,
}

impl std::fmt::Debug for TunnelIo {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("TunnelIo").finish_non_exhaustive()
    }
}

impl AgentTunnelHub {
    pub fn new() -> Self {
        Self::default()
    }

    /// Set `shield_tunnel_limit` (connector.conf). At least 1.
    pub fn set_tunnel_limit(&self, limit: usize) {
        self.limit.store(limit.max(1), Ordering::Relaxed);
    }

    /// Shield tunnels (active plus pending) right now.
    pub fn tunnel_count(&self) -> usize {
        self.state.lock().relay_sessions.len()
    }

    // -----------------------------------------------------------------------
    // Control streams
    // -----------------------------------------------------------------------

    /// Register stream `stream_id`'s channel that delivers ShieldControlMessages
    /// to `shield_id`; the newest stream becomes the Shield's current one.
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

    /// Stream `stream_id` of `shield_id` ended.
    /// - F45: the Shield's registration is removed only if it is still this
    ///   stream's; a newer stream that registered first stays.
    /// - Renewal handover: if the Shield ended this stream cleanly (`clean`:
    ///   it half-closed it) and a newer stream of the same Shield is
    ///   registered, this stream's tunnels move to the newer stream. Their
    ///   data runs on their own `Tunnel` streams, which are unaffected.
    /// - Otherwise (F44) every tunnel of this stream ends now, pending opens
    ///   included.
    ///
    /// Returns how many tunnels were handed over.
    pub fn end_stream(&self, shield_id: &str, stream_id: u64, clean: bool) -> usize {
        let mut state = self.state.lock();
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
        let mut handed_over = 0;
        for cid in on_stream {
            match newer {
                Some(newer) => {
                    state
                        .relay_sessions
                        .get_mut(&cid)
                        .expect("listed above")
                        .stream_id = newer;
                    handed_over += 1;
                }
                None => {
                    if let Some(entry) = state.relay_sessions.remove(&cid) {
                        entry.abort.cancel();
                    }
                }
            }
        }
        if !state.shield_txs.contains_key(shield_id) {
            // No Control stream left: the Shield closes all its tunnels when
            // its stream ends (F44), so no pending abort is still needed.
            state.abort_mailboxes.remove(shield_id);
        }
        if handed_over > 0 {
            info!(
                shield_id,
                tunnels = handed_over,
                "shield Control stream handed over: tunnels kept"
            );
        }
        handed_over
    }

    /// Revoke `shield_id` (certificate audit bug 3): unregister it, refuse it
    /// from now on, and end every tunnel through it — including pending
    /// opens. Idempotent.
    pub fn revoke_shield(&self, shield_id: &str) {
        let mut state = self.state.lock();
        state.revoked.insert(shield_id.to_string());
        state.shield_txs.remove(shield_id);
        // Its Control streams are closed with the revocation; the Shield
        // closes every tunnel when they end.
        state.abort_mailboxes.remove(shield_id);
        state.relay_sessions.retain(|_, e| {
            if e.shield_id == shield_id {
                e.abort.cancel();
                false
            } else {
                true
            }
        });
    }

    // -----------------------------------------------------------------------
    // Shield answers
    // -----------------------------------------------------------------------

    /// The Shield (`from_shield`, its verified identity) reports on a tunnel
    /// open over Control. FU-9: only a failure is reported this way; an
    /// `ok = true` is the legacy, pre-FU-9 answer and fails the open too.
    pub fn dispatch_opened(&self, from_shield: &str, connection_id: &str, ok: bool, error: String) {
        let mut state = self.state.lock();
        let Some(entry) = state.relay_sessions.get_mut(connection_id) else {
            return;
        };
        if entry.shield_id != from_shield {
            warn!(
                connection_id,
                from_shield, "ignored TunnelOpened for another shield's tunnel"
            );
            return;
        }
        let Some(pending) = entry.pending.take() else {
            return; // already bound: a stray answer
        };
        state.relay_sessions.remove(connection_id);
        let error = if ok {
            "shield does not support dedicated tunnel streams; upgrade the shield".to_string()
        } else {
            error
        };
        let _ = pending.send(Err(error));
    }

    /// Bind a new `Tunnel` stream from `shield_id` (its verified identity) to
    /// the pending tunnel `connection_id`. The id must be pending, opened to
    /// this Shield, and is accepted once. Returns the stream's response.
    pub fn attach(
        &self,
        shield_id: &str,
        connection_id: &str,
        inbound: InboundFrames,
        expires_at: tokio::time::Instant,
    ) -> Result<ToShield, Status> {
        let mut state = self.state.lock();
        let entry = state
            .relay_sessions
            .get_mut(connection_id)
            .ok_or_else(|| Status::not_found("unknown or expired tunnel"))?;
        if entry.shield_id != shield_id {
            // The other Shield's pending open is left alone.
            warn!(
                connection_id,
                shield_id, "refused a tunnel stream for another shield's tunnel"
            );
            return Err(Status::permission_denied(
                "tunnel belongs to another shield",
            ));
        }
        let pending = entry
            .pending
            .take()
            .ok_or_else(|| Status::already_exists("tunnel already has a stream"))?;
        let cap = if entry.udp {
            UDP_TO_SHIELD_QUEUE
        } else {
            TCP_TO_SHIELD_QUEUE
        };
        let (outbound, rx) = mpsc::channel(cap);
        let to_shield = ToShield {
            rx,
            abort: entry.abort.clone(),
            aborted: Box::pin(entry.abort.clone().cancelled_owned()),
            done: false,
        };
        let io = TunnelIo {
            inbound,
            outbound,
            expires_at,
        };
        if pending.send(Ok(io)).is_err() {
            // The open gave up in the same instant.
            state.relay_sessions.remove(connection_id);
            return Err(Status::not_found("unknown or expired tunnel"));
        }
        Ok(to_shield)
    }

    /// Tell the Shield over Control to abort `connection_id` (best effort; the
    /// tunnel's own stream also ends with an error).
    fn abort_on_shield(&self, connection_id: &str, error: &str) {
        let mut state = self.state.lock();
        let Some(entry) = state.relay_sessions.get(connection_id) else {
            return;
        };
        // A tunnel still pending has no stream at the Shield: its late
        // stream is refused (NotFound) and the Shield closes its side.
        if entry.pending.is_some() {
            return;
        }
        let shield_id = entry.shield_id.clone();
        state
            .abort_mailboxes
            .entry(shield_id)
            .or_default()
            .post(connection_id, error);
    }

    /// The abort mailbox of `shield_id`, drained by its Control stream.
    pub fn abort_mailbox(&self, shield_id: &str) -> Arc<AbortMailbox> {
        self.state
            .lock()
            .abort_mailboxes
            .entry(shield_id.to_string())
            .or_default()
            .clone()
    }

    // -----------------------------------------------------------------------
    // Called by device_tunnel.rs to open a relay through a Shield
    // -----------------------------------------------------------------------

    /// Open a relay session through `shield_id` for the given destination.
    ///
    /// Admission is immediate: a revoked or absent Shield, or a full per-Shield
    /// or per-Connector tunnel budget, refuses at once (`OpenError`). Then
    /// `TunnelOpen` goes out on the Shield's Control stream and the session
    /// waits, at most TUNNEL_OPEN_TIMEOUT, for its `Tunnel` stream.
    /// Tests only: production device sessions must use
    /// `open_relay_session_revocable`, so every tunnel is bound to its
    /// session's revocation (FU-9 gate-4).
    #[cfg(test)]
    pub(crate) async fn open_relay_session(
        &self,
        shield_id: &str,
        destination: &str,
        port: u16,
        protocol: &str,
    ) -> Result<RelaySession> {
        self.open(shield_id, destination, port, protocol, None)
            .await
    }

    /// `open_relay_session` for a device session whose access can be revoked
    /// (FU-15) through `revoked`. FU-9 gate-4: the tunnel's abort is a child
    /// of `revoked`, so revoking the session cancels the tunnel inside the
    /// same `cancel()` call — while it is still opening, before its first
    /// frame, or while it relays — and nothing more goes to the Shield (see
    /// `ToShield`). A revocation during the open fails it at once.
    pub async fn open_relay_session_revocable(
        &self,
        shield_id: &str,
        destination: &str,
        port: u16,
        protocol: &str,
        revoked: &CancellationToken,
    ) -> Result<RelaySession> {
        self.open(shield_id, destination, port, protocol, Some(revoked))
            .await
    }

    async fn open(
        &self,
        shield_id: &str,
        destination: &str,
        port: u16,
        protocol: &str,
        revoked: Option<&CancellationToken>,
    ) -> Result<RelaySession> {
        let connection_id = Uuid::new_v4().to_string();
        let udp = protocol.eq_ignore_ascii_case("udp");
        let (pending_tx, pending_rx) = oneshot::channel();
        let abort = match revoked {
            Some(revoked) => revoked.child_token(),
            None => CancellationToken::new(),
        };
        if abort.is_cancelled() {
            // Revoked before anything was registered or sent.
            return Err(OpenError::SessionRevoked.into());
        }
        // Admission and registration in one critical section, so neither a
        // concurrent revocation nor concurrent opens get past the checks.
        let shield_tx = {
            let mut state = self.state.lock();
            if state.revoked.contains(shield_id) {
                return Err(OpenError::Revoked.into());
            }
            let (stream_id, shield_tx) = state
                .shield_txs
                .get(shield_id)
                .cloned()
                .ok_or(OpenError::NotConnected)?;
            let limit = self.limit.load(Ordering::Relaxed);
            if state.relay_sessions.len() >= limit {
                return Err(OpenError::ConnectorLimit(limit).into());
            }
            let per_shield = state
                .relay_sessions
                .values()
                .filter(|e| e.shield_id == shield_id)
                .count();
            if per_shield >= MAX_TUNNELS_PER_SHIELD {
                return Err(OpenError::ShieldLimit.into());
            }
            state.relay_sessions.insert(
                connection_id.clone(),
                RelayEntry {
                    shield_id: shield_id.to_string(),
                    stream_id,
                    udp,
                    pending: Some(pending_tx),
                    abort: abort.clone(),
                },
            );
            shield_tx
        };
        let guard = SessionGuard {
            hub: self.clone(),
            connection_id: connection_id.clone(),
            abort: abort.clone(),
            settled: false,
        };

        let revocation = async {
            match revoked {
                Some(revoked) => revoked.cancelled().await,
                None => std::future::pending().await,
            }
        };
        let opening = tokio::time::timeout(TUNNEL_OPEN_TIMEOUT, async {
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
                .map_err(|_| OpenError::Closed)?;
            match pending_rx.await {
                Ok(Ok(io)) => Ok(io),
                Ok(Err(error)) => Err(OpenError::Refused(error)),
                Err(_) => Err(OpenError::Closed),
            }
        });
        let opened = tokio::select! {
            biased;
            // The guard removes the pending tunnel; a stream the Shield opens
            // for it later is refused (NotFound) and the Shield closes it.
            _ = revocation => return Err(OpenError::SessionRevoked.into()),
            opened = opening => opened,
        };
        let io = match opened {
            Ok(Ok(io)) => io,
            Ok(Err(e)) => return Err(e.into()),
            Err(_) => return Err(OpenError::Timeout.into()),
        };

        Ok(RelaySession {
            connection_id,
            udp,
            io,
            abort,
            cancel_token: revoked.cloned().unwrap_or_default(),
            guard,
        })
    }
}

/// Removes the session from the hub when it ends, freeing its place in the
/// tunnel budget. A session that did not end cleanly is aborted toward the
/// Shield too.
struct SessionGuard {
    hub: AgentTunnelHub,
    connection_id: String,
    abort: CancellationToken,
    /// The session ended cleanly, or its abort was already sent: dropping
    /// only frees its place.
    settled: bool,
}

impl Drop for SessionGuard {
    fn drop(&mut self) {
        if !self.settled {
            self.hub
                .abort_on_shield(&self.connection_id, "session ended");
            self.abort.cancel();
        }
        self.hub
            .state
            .lock()
            .relay_sessions
            .remove(&self.connection_id);
    }
}

// ---------------------------------------------------------------------------
// RelaySession — returned by open_relay_session, used by device_tunnel
// ---------------------------------------------------------------------------

pub struct RelaySession {
    connection_id: String,
    udp: bool,
    io: TunnelIo,
    /// The hub's abort for this session (revocation, Control stream end).
    abort: CancellationToken,
    /// FU-15: the device session's authorization was revoked.
    cancel_token: CancellationToken, // defaults to a token that's never cancelled
    guard: SessionGuard,
}

/// How the Shield → device direction ended.
#[derive(Debug, PartialEq)]
enum End {
    /// `TunnelEnd{}` from the Shield: the resource closed normally.
    Clean,
    /// FU-15 cancellation, hub abort, or certificate expiry.
    Aborted(&'static str),
    /// An error end from the Shield, a broken stream or device.
    Failed,
}

/// Send `frame`, unless the session is stopped first. False when it was not
/// sent.
async fn send_frame(
    out: &mpsc::Sender<Result<TunnelFrame, Status>>,
    frame: Frame,
    stop: &CancellationToken,
) -> bool {
    tokio::select! {
        biased;
        _ = stop.cancelled() => false,
        sent = out.send(Ok(TunnelFrame { frame: Some(frame) })) => sent.is_ok(),
    }
}

impl RelaySession {
    /// Attaches an external cancellation token (FU-15: the session's
    /// authorization was revoked). Optional — a caller that never calls this
    /// keeps a token that is never cancelled.
    pub fn with_cancel_token(mut self, token: CancellationToken) -> Self {
        self.cancel_token = token;
        self
    }

    /// Bidirectionally relay between `stream` (device) and the Shield's
    /// tunnel stream.
    ///
    /// - TCP: the byte stream in frames of up to 16 KB, each direction awaiting
    ///   room (HTTP/2 backpressure). A clean end (`TunnelEnd{}`) shuts the
    ///   device stream down (F-7); an error end drops it.
    /// - UDP: the device stream carries `[u32 BE length][datagram]` frames
    ///   (as the direct path); each datagram is one tunnel frame. A full queue
    ///   toward the Shield drops the newest datagram (D-4).
    /// - FU-15 cancel, Shield revocation, Control-stream end or certificate
    ///   expiry abort the device stream and the Shield's side at once.
    pub async fn relay_stream<S>(self, stream: S) -> Result<()>
    where
        S: tokio::io::AsyncRead
            + tokio::io::AsyncWrite
            + crate::abort_stream::AbortStream
            + Unpin
            + Send
            + 'static,
    {
        let RelaySession {
            connection_id,
            udp,
            io,
            abort,
            cancel_token,
            mut guard,
        } = self;
        let TunnelIo {
            mut inbound,
            outbound,
            expires_at,
        } = io;
        // Stops the device → shield task. A child of the session's abort
        // (itself a child of its revocation token): a revocation stops the
        // task inside `cancel()`, and a session revoked before this point
        // never forwards anything (the task checks first).
        let stop = abort.child_token();
        let (mut reader, mut writer) = tokio::io::split(stream);

        let d2s = {
            let stop = stop.clone();
            let out = outbound.clone();
            let connection_id = connection_id.clone();
            tokio::spawn(async move {
                if stop.is_cancelled() {
                    return reader;
                }
                if udp {
                    device_to_shield_udp(&mut reader, &out, &stop, &connection_id).await;
                } else {
                    device_to_shield_tcp(&mut reader, &out, &stop).await;
                }
                reader
            })
        };
        drop(outbound);

        let expiry = tokio::time::sleep_until(expires_at);
        tokio::pin!(expiry);
        let end = loop {
            let next = tokio::select! {
                biased;
                _ = cancel_token.cancelled() => break End::Aborted("session revoked"),
                _ = abort.cancelled() => break End::Aborted("tunnel ended by the connector"),
                _ = &mut expiry => break End::Aborted("shield certificate expired"),
                next = inbound.next() => next,
            };
            match next {
                Some(Ok(TunnelFrame {
                    frame: Some(Frame::Data(data)),
                })) => {
                    let write = async {
                        if udp {
                            let mut framed = Vec::with_capacity(4 + data.len());
                            framed.extend_from_slice(&(data.len() as u32).to_be_bytes());
                            framed.extend_from_slice(&data);
                            writer.write_all(&framed).await?;
                            writer.flush().await
                        } else {
                            writer.write_all(&data).await
                        }
                    };
                    let written = tokio::select! {
                        biased;
                        _ = cancel_token.cancelled() => break End::Aborted("session revoked"),
                        _ = abort.cancelled() => break End::Aborted("tunnel ended by the connector"),
                        written = write => written,
                    };
                    if written.is_err() {
                        break End::Failed;
                    }
                }
                Some(Ok(TunnelFrame {
                    frame: Some(Frame::End(TunnelEnd { error })),
                })) => {
                    if error.is_empty() {
                        break End::Clean;
                    }
                    warn!(connection_id = %connection_id, error = %error, "shield closed tunnel with error");
                    break End::Failed;
                }
                Some(Ok(other)) => {
                    warn!(connection_id = %connection_id, frame = ?other.frame, "unexpected tunnel frame from shield");
                    break End::Failed;
                }
                Some(Err(status)) => {
                    warn!(connection_id = %connection_id, error = %status, "shield tunnel stream failed");
                    break End::Failed;
                }
                None => {
                    warn!(connection_id = %connection_id, "shield tunnel stream ended without TunnelEnd");
                    break End::Failed;
                }
            }
        };
        stop.cancel();

        match end {
            End::Aborted(reason) => {
                // Abort the device stream so neither the connector's queue nor
                // the client's is delivered after the cut (FU-15), and the
                // Shield's side (Control TunnelClose + error on the stream).
                guard.hub.abort_on_shield(&connection_id, reason);
                abort.cancel();
                guard.settled = true;
                match tokio::time::timeout(crate::abort_stream::ABORT_REJOIN_GRACE, d2s).await {
                    Ok(Ok(reader)) => reader.unsplit(writer).abort(),
                    _ => warn!(
                        connection_id = %connection_id,
                        "aborted session: device stream not aborted (device→shield task did not stop)"
                    ),
                }
            }
            End::Failed => {
                d2s.abort();
                // The guard aborts the Shield's side; the device stream is
                // dropped (abrupt, unlike a clean shutdown).
            }
            End::Clean => {
                d2s.abort();
                guard.settled = true;
                // F-7: end the device stream properly (close_notify on a
                // relayed stream), however slowly the reader drains.
                tokio::select! {
                    _ = tokio::time::timeout(DEVICE_SHUTDOWN_GRACE, writer.shutdown()) => {}
                    _ = cancel_token.cancelled() => {}
                }
            }
        }
        Ok(())
    }
}

async fn device_to_shield_tcp<R>(
    reader: &mut R,
    out: &mpsc::Sender<Result<TunnelFrame, Status>>,
    stop: &CancellationToken,
) where
    R: tokio::io::AsyncRead + Unpin,
{
    let mut buf = vec![0u8; MAX_CHUNK];
    loop {
        let read = tokio::select! {
            biased;
            _ = stop.cancelled() => return,
            read = reader.read(&mut buf) => read,
        };
        match read {
            Ok(0) => {
                send_frame(out, Frame::End(TunnelEnd::default()), stop).await;
                return;
            }
            Ok(n) => {
                if !send_frame(out, Frame::Data(buf[..n].to_vec()), stop).await {
                    return;
                }
            }
            Err(e) => {
                send_frame(
                    out,
                    Frame::End(TunnelEnd {
                        error: e.to_string(),
                    }),
                    stop,
                )
                .await;
                return;
            }
        }
    }
}

async fn device_to_shield_udp<R>(
    reader: &mut R,
    out: &mpsc::Sender<Result<TunnelFrame, Status>>,
    stop: &CancellationToken,
    connection_id: &str,
) where
    R: tokio::io::AsyncRead + Unpin,
{
    let mut dropped: u64 = 0;
    let mut last_log: Option<tokio::time::Instant> = None;
    loop {
        let datagram = tokio::select! {
            biased;
            _ = stop.cancelled() => return,
            datagram = read_datagram(reader) => datagram,
        };
        if stop.is_cancelled() {
            return;
        }
        match datagram {
            Ok(Some(d)) => match out.try_send(Ok(TunnelFrame {
                frame: Some(Frame::Data(d)),
            })) {
                Ok(()) => {}
                // D-4: the newest datagram is dropped; the tunnel never blocks.
                Err(mpsc::error::TrySendError::Full(_)) => {
                    dropped += 1;
                    let now = tokio::time::Instant::now();
                    if last_log.is_none_or(|t| now.duration_since(t) >= Duration::from_secs(1)) {
                        last_log = Some(now);
                        warn!(
                            connection_id,
                            dropped, "udp tunnel queue to the shield full: dropping datagrams"
                        );
                    }
                }
                Err(mpsc::error::TrySendError::Closed(_)) => return,
            },
            Ok(None) => {
                send_frame(out, Frame::End(TunnelEnd::default()), stop).await;
                return;
            }
            Err(e) => {
                send_frame(out, Frame::End(TunnelEnd { error: e }), stop).await;
                return;
            }
        }
    }
}

/// One `[u32 BE length][datagram]` frame from the device stream; None at a
/// clean end of the stream.
async fn read_datagram<R>(reader: &mut R) -> std::result::Result<Option<Vec<u8>>, String>
where
    R: tokio::io::AsyncRead + Unpin,
{
    let mut len = [0u8; 4];
    match reader.read_exact(&mut len).await {
        Ok(_) => {}
        Err(e) if e.kind() == std::io::ErrorKind::UnexpectedEof => return Ok(None),
        Err(e) => return Err(e.to_string()),
    }
    let len = u32::from_be_bytes(len) as usize;
    if len > MAX_DATAGRAM {
        return Err(format!("datagram length {len} over {MAX_DATAGRAM}"));
    }
    let mut buf = vec![0u8; len];
    reader
        .read_exact(&mut buf)
        .await
        .map_err(|e| e.to_string())?;
    Ok(Some(buf))
}

#[cfg(test)]
#[path = "agent_tunnel_tests.rs"]
pub(crate) mod tests;
