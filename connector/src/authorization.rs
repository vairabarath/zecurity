//! Controller ↔ connector authorization contract — connector side
//! (connector-revocation design §4.4–§4.6, §5; Phases A and B1).
//!
//! Phase A: assigns heartbeat sequence numbers, timestamps each
//! authorization request at the transport-send boundary, validates
//! `AuthorizationAck`s, and recognises the terminal signals
//! (`ConnectorRevoked`, the `revoked` / `unknown` trailer markers).
//!
//! Phase B1: [`AckTracker`] is also the serving-state machine (§5): it owns
//! the state, the deadline D and `lost_at`, and publishes `{state, D}` as one
//! [`AuthzSnapshot`] through a `watch` channel. It is the only writer; every
//! event is serialized by its mutex (the §5.4 single supervisor). Data-plane
//! entry paths hold an [`AuthzGate`] and admit only while
//! [`AuthzSnapshot::admits`] holds. Nothing here treats a missing
//! acknowledgement, a missing capability or an unsupported controller as
//! authorization.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use parking_lot::Mutex;
use tokio::sync::watch;
use tokio::task::JoinHandle;
use tokio::time::{sleep_until, Instant};
use tokio_stream::{Stream, StreamExt};
use tracing::{error, info, warn};

use crate::proto::connector_control_message::Body as CBody;
use crate::proto::ConnectorControlMessage;

/// gRPC trailer key carrying the controller's authoritative connector state
/// (design D10). Must match the controller's `ConnectorStateMarkerKey`.
pub const CONNECTOR_STATE_MARKER_KEY: &str = "zecurity-connector-state";
/// Marker value: administrative revocation (REVOKED).
pub const MARKER_REVOKED: &str = "revoked";
/// Marker value: the controller has no record of this identity (UNKNOWN_CONNECTOR).
pub const MARKER_UNKNOWN: &str = "unknown";

/// An authoritative terminal signal from the controller.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum TerminalSignal {
    /// `ConnectorRevoked` or the `revoked` marker.
    Revoked,
    /// The `unknown` marker. Distinct from `Revoked`.
    UnknownIdentity,
}

/// Classification of a gRPC status received from the controller. Only the
/// structured trailer marker is consulted — never the message text. Any
/// status without a recognised marker is non-terminal, whatever its code.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum StatusClass {
    Revoked,
    UnknownConnector,
    /// Recoverable: transport failure, Unavailable, wrong role, an
    /// unmarked PermissionDenied/NotFound, FailedPrecondition, ...
    Unmarked(tonic::Code),
}

pub fn classify_status(status: &tonic::Status) -> StatusClass {
    match status
        .metadata()
        .get(CONNECTOR_STATE_MARKER_KEY)
        .and_then(|v| v.to_str().ok())
    {
        Some(MARKER_REVOKED) => StatusClass::Revoked,
        Some(MARKER_UNKNOWN) => StatusClass::UnknownConnector,
        _ => StatusClass::Unmarked(status.code()),
    }
}

/// Connector serving state (design §5.1). Every process starts in
/// `Starting`; only an accepted acknowledgement opens the data plane.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ServingState {
    /// No accepted ack since process start. Closed.
    Starting,
    /// `now < D`, control stream up. Open.
    Serving,
    /// Stream down or reconnecting, `now < D`. Open only until D.
    ServingDisconnected,
    /// D passed without an accepted ack, or the stream was lost while
    /// recovering. Closed, recoverable.
    Disabled,
    /// `Disabled` with a (re)established stream, awaiting an accepted ack
    /// for a request sent at or after `lost_at`. Closed, recoverable.
    Recovering,
    /// Administrative revocation. Closed, terminal for the process.
    Revoked,
    /// The controller has no record of this identity. Closed, terminal for
    /// the process, distinct from `Revoked`.
    UnknownIdentity,
}

impl ServingState {
    pub fn is_terminal(self) -> bool {
        matches!(self, ServingState::Revoked | ServingState::UnknownIdentity)
    }
}

/// The published `{state, D}` pair, read together (design §7 step 1).
/// Fields are private: readers can't build or alter a snapshot.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct AuthzSnapshot {
    state: ServingState,
    deadline: Option<Instant>,
    /// Number of open → closed transitions so far. A reader that only sees
    /// the latest snapshot still notices every loss of authorization, even a
    /// brief one that reopened before it looked (fail-closed, B2).
    closures: u64,
}

impl AuthzSnapshot {
    pub fn state(&self) -> ServingState {
        self.state
    }
    pub fn deadline(&self) -> Option<Instant> {
        self.deadline
    }
    pub fn closures(&self) -> u64 {
        self.closures
    }
    /// The admission rule (§5.3 guarantee 1, §6.3): an open state AND
    /// `now < D`. Independent of the expiry timer, so a starved timer never
    /// keeps the data plane open past D.
    pub fn admits(&self, now: Instant) -> bool {
        matches!(
            self.state,
            ServingState::Serving | ServingState::ServingDisconnected
        ) && self.deadline.is_some_and(|d| now < d)
    }
}

/// Read-only view of the serving state for data-plane entry paths.
#[derive(Debug, Clone)]
pub struct AuthzGate {
    rx: watch::Receiver<AuthzSnapshot>,
}

impl AuthzGate {
    pub fn snapshot(&self) -> AuthzSnapshot {
        *self.rx.borrow()
    }
    pub fn admits_now(&self) -> bool {
        self.snapshot().admits(Instant::now())
    }
    /// A receiver for state changes (read-only).
    pub(crate) fn subscribe(&self) -> watch::Receiver<AuthzSnapshot> {
        self.rx.clone()
    }

    /// A gate fixed at `snapshot` (the sender is dropped; `borrow` still
    /// returns the last value).
    #[cfg(test)]
    pub fn fixed(state: ServingState, deadline: Option<Instant>) -> Self {
        let (_tx, rx) = watch::channel(AuthzSnapshot {
            state,
            deadline,
            closures: 0,
        });
        Self { rx }
    }
    /// Serving with a deadline far in the future.
    #[cfg(test)]
    pub fn open_for_tests() -> Self {
        Self::fixed(
            ServingState::Serving,
            Some(Instant::now() + Duration::from_secs(365 * 24 * 3600)),
        )
    }
    #[cfg(test)]
    pub fn closed_for_tests() -> Self {
        Self::fixed(ServingState::Starting, None)
    }
}

/// Why an `AuthorizationAck` was not accepted (§4.4).
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum AckRejection {
    /// A terminal signal was already processed.
    Terminal(TerminalSignal),
    /// The stream it arrived on never asserted `authorization_acks`.
    NoCapability,
    /// heartbeat_seq = 0.
    Zero,
    /// k ≤ highest accepted: duplicate or out-of-order.
    NotNewer { highest_accepted: u64 },
    /// No request with this seq was sent (or it was pruned): unknown,
    /// mismatched or forged.
    Unknown,
    /// The request is already ≥ H old: the ack would confer nothing.
    Stale { age: Duration },
    /// The request was sent before authorization was lost
    /// (`sent[k] < lost_at`). This also covers the §4.4 row "Serving and
    /// `now ≥ D`": `on_ack` applies `expire` first, so such a state is already
    /// `Disabled` with `lost_at = D`, and a late ack for a request sent before
    /// D lands here.
    BeforeLoss,
}

impl std::fmt::Display for AckRejection {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            AckRejection::Terminal(s) => write!(f, "terminal state {s:?} already reached"),
            AckRejection::NoCapability => {
                write!(f, "stream did not assert the authorization capability")
            }
            AckRejection::Zero => write!(f, "heartbeat_seq is zero"),
            AckRejection::NotNewer { highest_accepted } => write!(
                f,
                "duplicate or out-of-order (highest accepted {highest_accepted})"
            ),
            AckRejection::Unknown => write!(f, "no outstanding request with this seq"),
            AckRejection::Stale { age } => write!(f, "request is stale ({age:?} old)"),
            AckRejection::BeforeLoss => {
                write!(f, "request was sent before authorization was lost")
            }
        }
    }
}

/// Process-global authorization bookkeeping and serving-state machine.
/// Created once per process (in `main`), so it survives reconnects and
/// renewal switches: sequence numbers start at 1 and never reset, and the
/// state starts in `Starting` (nothing is persisted, design D12).
#[derive(Debug)]
pub struct AckTracker {
    /// H: maximum authorization freshness.
    freshness: Duration,
    /// Last seq assigned (0 = none yet).
    last_assigned: u64,
    /// seq → instant the request was handed to the transport (first send).
    sent: BTreeMap<u64, Instant>,
    highest_accepted: u64,
    /// Send instant of the request behind the latest accepted ack.
    latest_accepted_sent_at: Option<Instant>,
    terminal: Option<TerminalSignal>,
    state: ServingState,
    /// D = sent[k] + H of the latest accepted ack. Meaningful only in
    /// `Serving`/`ServingDisconnected`.
    deadline: Option<Instant>,
    /// When authorization was last lost. None = never since process start:
    /// every recorded send is after the start, so nothing is excluded.
    lost_at: Option<Instant>,
    closures: u64,
    publish: watch::Sender<AuthzSnapshot>,
}

pub type SharedAckTracker = Arc<Mutex<AckTracker>>;

impl AckTracker {
    pub fn new(freshness: Duration) -> Self {
        let (publish, _) = watch::channel(AuthzSnapshot {
            state: ServingState::Starting,
            deadline: None,
            closures: 0,
        });
        Self {
            freshness,
            last_assigned: 0,
            sent: BTreeMap::new(),
            highest_accepted: 0,
            latest_accepted_sent_at: None,
            terminal: None,
            state: ServingState::Starting,
            deadline: None,
            lost_at: None,
            closures: 0,
            publish,
        }
    }

    /// A read-only gate on this tracker's published state.
    pub fn gate(&self) -> AuthzGate {
        AuthzGate {
            rx: self.publish.subscribe(),
        }
    }

    pub fn state(&self) -> ServingState {
        self.state
    }
    pub fn deadline(&self) -> Option<Instant> {
        self.deadline
    }
    pub fn lost_at(&self) -> Option<Instant> {
        self.lost_at
    }
    pub fn snapshot(&self) -> AuthzSnapshot {
        AuthzSnapshot {
            state: self.state,
            deadline: self.deadline,
            closures: self.closures,
        }
    }

    /// The only place the state changes. Publishes `{state, D}` as one
    /// snapshot and logs the transition.
    fn transition(&mut self, to: ServingState, reason: &'static str) {
        let from = self.state;
        let open = |s: ServingState| matches!(s, ServingState::Serving | ServingState::ServingDisconnected);
        if open(from) && !open(to) {
            self.closures += 1;
        }
        self.state = to;
        if !matches!(to, ServingState::Serving | ServingState::ServingDisconnected) {
            self.deadline = None;
        }
        self.publish.send_replace(self.snapshot());
        if from == to {
            return;
        }
        match to {
            ServingState::Serving => info!(
                authz_event = "state",
                from = ?from,
                to = ?to,
                reason,
                "connector authorized — data plane open"
            ),
            ServingState::ServingDisconnected => warn!(
                authz_event = "state",
                from = ?from,
                to = ?to,
                reason,
                "control stream lost — serving only until the authorization deadline"
            ),
            ServingState::Disabled | ServingState::Recovering | ServingState::Starting => warn!(
                authz_event = "state",
                from = ?from,
                to = ?to,
                reason,
                "connector not authorized — data plane closed (recoverable)"
            ),
            ServingState::Revoked => error!(
                authz_event = "state",
                from = ?from,
                to = ?to,
                reason,
                "connector identity REVOKED — data plane closed (terminal)"
            ),
            ServingState::UnknownIdentity => error!(
                authz_event = "state",
                from = ?from,
                to = ?to,
                reason,
                "connector identity UNKNOWN to the controller — data plane closed (terminal)"
            ),
        }
    }

    /// `Serving`/`ServingDisconnected` with `now ≥ D` → `Disabled`, with
    /// `lost_at = D` exactly (not the processing instant). Applied first by
    /// every event, so the outcome doesn't depend on timer scheduling.
    pub fn expire(&mut self, now: Instant) {
        if let (ServingState::Serving | ServingState::ServingDisconnected, Some(d)) =
            (self.state, self.deadline)
        {
            if now >= d {
                self.lost_at = Some(d);
                self.transition(ServingState::Disabled, "authorization deadline reached");
            }
        }
    }

    /// A Control stream delivered its first controller message (§5.2:
    /// `Disabled → Recovering`, `lost_at` unchanged).
    pub fn stream_up(&mut self, now: Instant) {
        self.expire(now);
        if self.state == ServingState::Disabled {
            self.transition(ServingState::Recovering, "control stream re-established");
        }
    }

    /// The Control stream ended (any return of a session or a failed open).
    /// A renewal switch is not a stream end.
    pub fn stream_down(&mut self, now: Instant) {
        self.expire(now);
        match self.state {
            ServingState::Serving => {
                self.transition(ServingState::ServingDisconnected, "control stream ended")
            }
            ServingState::Recovering => {
                self.lost_at = Some(now);
                self.transition(ServingState::Disabled, "control stream ended while recovering");
            }
            _ => {}
        }
    }

    pub fn shared(freshness: Duration) -> SharedAckTracker {
        Arc::new(Mutex::new(Self::new(freshness)))
    }

    /// A new seq for a fresh authorization request (periodic tick, or the
    /// initial report of a non-renewal stream).
    pub fn next_seq(&mut self) -> u64 {
        self.last_assigned += 1;
        self.last_assigned
    }

    /// The seq for a renewal replacement stream's initial report: the
    /// outstanding (assigned, not yet acknowledged) seq is re-sent so either
    /// stream's ack satisfies it; its original send time is kept, so the
    /// move to another stream extends nothing. With nothing outstanding a
    /// new seq is assigned.
    pub fn seq_for_replacement(&mut self) -> u64 {
        if self.last_assigned > self.highest_accepted {
            self.last_assigned
        } else {
            self.next_seq()
        }
    }

    /// Record that request `seq` was handed to the transport at `at`. The
    /// FIRST send wins: a re-send (renewal replacement) never moves it later.
    /// Requests already ≥ H old are pruned (no ack can be accepted for them).
    pub fn record_sent(&mut self, seq: u64, at: Instant) {
        if seq == 0 {
            return;
        }
        self.sent.entry(seq).or_insert(at);
        let h = self.freshness;
        self.sent.retain(|_, t| at < *t + h);
    }

    /// Validate an `AuthorizationAck` (§4.4 rows that do not depend on the
    /// Phase B deadline/state model). On acceptance the sent entries ≤ seq
    /// are dropped and the accepted request's send time is recorded.
    pub fn on_ack(&mut self, seq: u64, capable: bool, now: Instant) -> Result<(), AckRejection> {
        self.expire(now);
        if let Some(t) = self.terminal {
            return Err(AckRejection::Terminal(t));
        }
        if !capable {
            return Err(AckRejection::NoCapability);
        }
        if seq == 0 {
            return Err(AckRejection::Zero);
        }
        if seq <= self.highest_accepted {
            return Err(AckRejection::NotNewer {
                highest_accepted: self.highest_accepted,
            });
        }
        let Some(&sent_at) = self.sent.get(&seq) else {
            return Err(AckRejection::Unknown);
        };
        let age = now.saturating_duration_since(sent_at);
        if age >= self.freshness {
            return Err(AckRejection::Stale { age });
        }
        // Starting/Disabled/Recovering: only requests sent at or after the
        // loss count. (Serving/ServingDisconnected are before D here, after
        // `expire`, and every request since the last loss is admissible.)
        if self.lost_at.is_some_and(|lost| sent_at < lost) {
            return Err(AckRejection::BeforeLoss);
        }
        self.highest_accepted = seq;
        self.latest_accepted_sent_at = Some(sent_at);
        self.sent.retain(|k, _| *k > seq);
        // D = sent[k] + H; monotonic (k > highest accepted).
        let d = sent_at + self.freshness;
        self.deadline = Some(self.deadline.map_or(d, |old| old.max(d)));
        self.transition(ServingState::Serving, "authorization acknowledgement accepted");
        Ok(())
    }

    /// Record a terminal signal. The first one processed wins; returns
    /// whether this call set it.
    pub fn on_terminal(&mut self, signal: TerminalSignal) -> bool {
        if self.terminal.is_some() {
            return false;
        }
        self.terminal = Some(signal);
        match signal {
            TerminalSignal::Revoked => {
                self.transition(ServingState::Revoked, "controller: connector revoked")
            }
            TerminalSignal::UnknownIdentity => self.transition(
                ServingState::UnknownIdentity,
                "controller: connector identity unknown",
            ),
        }
        true
    }

    pub fn terminal(&self) -> Option<TerminalSignal> {
        self.terminal
    }
    pub fn highest_accepted(&self) -> u64 {
        self.highest_accepted
    }
    pub fn latest_accepted_sent_at(&self) -> Option<Instant> {
        self.latest_accepted_sent_at
    }
    pub fn sent_at(&self, seq: u64) -> Option<Instant> {
        self.sent.get(&seq).copied()
    }
}

/// Wrap a Control stream's outbound message stream so every
/// authorization-bearing health report is timestamped at the transport-send
/// boundary: the instant tonic's request encoder pulls the message to encode
/// it into the HTTP/2 stream. hyper polls the request body only after h2
/// has granted send capacity (hyper `proto/h2` `PipeToSendStream`), and the
/// encoded frame is handed straight to h2 `send_data`. Messages still queued
/// in the connector's mailbox are NOT considered sent. The remaining gap
/// (h2 connection task → socket → kernel) is not observable from the
/// application; the stamp precedes it, so a deadline anchored on it is
/// conservative (earlier), never later.
pub fn stamp_sends<S>(
    outbound: S,
    tracker: SharedAckTracker,
) -> impl Stream<Item = ConnectorControlMessage> + Send + 'static
where
    S: Stream<Item = ConnectorControlMessage> + Send + 'static,
{
    outbound.map(move |msg| {
        if let Some(CBody::ConnectorHealth(h)) = &msg.body {
            if h.heartbeat_seq != 0 {
                tracker.lock().record_sent(h.heartbeat_seq, Instant::now());
            }
        }
        msg
    })
}

/// Per-stream protocol state: whether this Control stream asserted the
/// authorization capability, and whether its first message was seen.
#[derive(Debug, Default, Clone, Copy)]
pub struct StreamAuthz {
    pub capable: bool,
    pub first_seen: bool,
}

/// Observe one controller → connector message for the authorization
/// contract. Logs every outcome; Phase A takes no serving action.
pub fn observe_controller_msg(
    msg: &ConnectorControlMessage,
    stream: &mut StreamAuthz,
    tracker: &SharedAckTracker,
) {
    let first = !stream.first_seen;
    stream.first_seen = true;
    if first {
        tracker.lock().stream_up(Instant::now());
    }
    match &msg.body {
        Some(CBody::AgentPolicy(p)) => {
            stream.capable = p.authorization_acks;
            if p.authorization_acks {
                info!(
                    authz_event = "capability_asserted",
                    "controller asserts the authorization contract"
                );
            } else {
                error!(
                    authz_event = "capability_absent",
                    "controller does not support the authorization contract (authorization_acks=false) — no acknowledgement on this stream will be accepted"
                );
            }
            return;
        }
        Some(CBody::AuthorizationAck(a)) => {
            let seq = a.heartbeat_seq;
            let result = tracker.lock().on_ack(seq, stream.capable, Instant::now());
            match result {
                Ok(()) => info!(
                    authz_event = "ack_accepted",
                    heartbeat_seq = seq,
                    "AuthorizationAck accepted"
                ),
                Err(reason) => warn!(
                    authz_event = "ack_rejected",
                    heartbeat_seq = seq,
                    reason = %reason,
                    "AuthorizationAck rejected"
                ),
            }
        }
        Some(CBody::ConnectorRevoked(_)) => {
            let newly = tracker.lock().on_terminal(TerminalSignal::Revoked);
            error!(
                authz_event = "connector_revoked",
                first_terminal = newly,
                "controller sent ConnectorRevoked: this connector identity is administratively revoked (terminal)"
            );
        }
        _ => {}
    }
    if first && !stream.capable {
        error!(
            authz_event = "capability_absent",
            "first controller message on this stream was not AgentPolicy{{authorization_acks:true}}: controller does not support the authorization contract — no acknowledgement on this stream will be accepted"
        );
    }
}

/// Moves `Serving`/`ServingDisconnected` to `Disabled` when D passes, so
/// the state (and its log) follows the deadline even with no other event.
/// Admission doesn't depend on this task: it compares `now` with D itself.
pub fn spawn_expiry(tracker: SharedAckTracker) -> JoinHandle<()> {
    let mut rx = tracker.lock().gate().rx;
    tokio::spawn(async move {
        loop {
            let snap = *rx.borrow_and_update();
            let open_deadline = match snap.state {
                ServingState::Serving | ServingState::ServingDisconnected => snap.deadline,
                _ => None,
            };
            match open_deadline {
                Some(d) => tokio::select! {
                    _ = sleep_until(d) => tracker.lock().expire(Instant::now()),
                    changed = rx.changed() => if changed.is_err() { return },
                },
                None => {
                    if rx.changed().await.is_err() {
                        return;
                    }
                }
            }
        }
    })
}

/// Classify and log a status received from the controller (stream open or
/// mid-stream). Records an authoritative terminal signal in the tracker.
pub fn observe_status(
    status: &tonic::Status,
    tracker: &SharedAckTracker,
    during: &str,
) -> StatusClass {
    let class = classify_status(status);
    match &class {
        StatusClass::Revoked => {
            let newly = tracker.lock().on_terminal(TerminalSignal::Revoked);
            error!(
                authz_event = "revoked_marker",
                during,
                code = ?status.code(),
                first_terminal = newly,
                "controller: connector is REVOKED (authoritative marker; terminal)"
            );
        }
        StatusClass::UnknownConnector => {
            let newly = tracker.lock().on_terminal(TerminalSignal::UnknownIdentity);
            error!(
                authz_event = "unknown_connector_marker",
                during,
                code = ?status.code(),
                first_terminal = newly,
                "controller: connector identity is UNKNOWN (authoritative marker; terminal, distinct from revocation)"
            );
        }
        StatusClass::Unmarked(code) => warn!(
            authz_event = "unmarked_status",
            during,
            code = ?code,
            "controller status without an authorization marker — recoverable, not revocation"
        ),
    }
    class
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::proto::{AgentPolicy, AuthorizationAck, ConnectorHealthReport, ConnectorRevoked};
    use tokio_stream::wrappers::ReceiverStream;

    const H: Duration = Duration::from_secs(15);

    fn tracker_with_sent(seqs: &[u64], at: Instant) -> AckTracker {
        let mut t = AckTracker::new(H);
        for &s in seqs {
            while t.last_assigned < s {
                t.next_seq();
            }
            t.record_sent(s, at);
        }
        t
    }

    // ── T6: one case per acceptance rule ───────────────────────────────────

    #[test]
    fn accepts_valid_ack_and_records_send_time() {
        let t0 = Instant::now();
        let mut t = tracker_with_sent(&[1], t0);
        assert_eq!(t.on_ack(1, true, t0 + Duration::from_secs(2)), Ok(()));
        assert_eq!(t.highest_accepted(), 1);
        assert_eq!(
            t.latest_accepted_sent_at(),
            Some(t0),
            "anchor is the send time, not the receipt time"
        );
    }

    #[test]
    fn rejects_zero() {
        let mut t = tracker_with_sent(&[1], Instant::now());
        assert_eq!(t.on_ack(0, true, Instant::now()), Err(AckRejection::Zero));
    }

    #[test]
    fn rejects_unknown_and_mismatched() {
        let mut t = tracker_with_sent(&[1, 2], Instant::now());
        assert_eq!(
            t.on_ack(9, true, Instant::now()),
            Err(AckRejection::Unknown)
        );
        // Assigned but never handed to the transport: not "sent".
        let s = t.next_seq();
        assert_eq!(
            t.on_ack(s, true, Instant::now()),
            Err(AckRejection::Unknown)
        );
    }

    #[test]
    fn rejects_duplicate_and_out_of_order() {
        let now = Instant::now();
        let mut t = tracker_with_sent(&[1, 2, 3], now);
        assert_eq!(t.on_ack(2, true, now), Ok(()));
        assert_eq!(
            t.on_ack(2, true, now),
            Err(AckRejection::NotNewer {
                highest_accepted: 2
            }),
            "duplicate"
        );
        assert_eq!(
            t.on_ack(1, true, now),
            Err(AckRejection::NotNewer {
                highest_accepted: 2
            }),
            "older ack after a newer one"
        );
        assert_eq!(t.on_ack(3, true, now), Ok(()));
    }

    #[test]
    fn rejects_stale_request_at_and_after_h() {
        let t0 = Instant::now();
        let mut t = tracker_with_sent(&[1, 2], t0);
        // now == sent + H: the request confers nothing any more.
        assert_eq!(
            t.on_ack(1, true, t0 + H),
            Err(AckRejection::Stale { age: H })
        );
        assert!(matches!(
            t.on_ack(2, true, t0 + H + Duration::from_secs(1)),
            Err(AckRejection::Stale { .. })
        ));
        // Just below H is still acceptable.
        let mut t = tracker_with_sent(&[1], t0);
        let just_before = t0 + H - Duration::from_millis(1);
        assert_eq!(t.on_ack(1, true, just_before), Ok(()));
    }

    #[test]
    fn rejects_without_capability() {
        let mut t = tracker_with_sent(&[1], Instant::now());
        assert_eq!(
            t.on_ack(1, false, Instant::now()),
            Err(AckRejection::NoCapability)
        );
        assert_eq!(t.highest_accepted(), 0, "a rejected ack changes nothing");
    }

    #[test]
    fn rejects_after_terminal_and_first_terminal_wins() {
        let mut t = tracker_with_sent(&[1], Instant::now());
        assert!(t.on_terminal(TerminalSignal::Revoked));
        assert!(
            !t.on_terminal(TerminalSignal::UnknownIdentity),
            "first terminal kept"
        );
        assert_eq!(t.terminal(), Some(TerminalSignal::Revoked));
        assert_eq!(
            t.on_ack(1, true, Instant::now()),
            Err(AckRejection::Terminal(TerminalSignal::Revoked))
        );
    }

    #[test]
    fn sequence_is_process_global_and_never_resets() {
        let mut t = AckTracker::new(H);
        assert_eq!(t.next_seq(), 1, "starts at 1");
        assert_eq!(t.next_seq(), 2);
        // No reset API exists; a reconnect just asks for the next seq.
        assert_eq!(t.next_seq(), 3);
    }

    // ── T10: renewal reuse keeps the original send time ────────────────────

    #[test]
    fn replacement_reuses_outstanding_seq_with_original_send_time() {
        let mut t = AckTracker::new(H);
        let s = t.next_seq();
        let t0 = Instant::now();
        t.record_sent(s, t0);
        let later = t0 + Duration::from_secs(3);
        let r = t.seq_for_replacement();
        assert_eq!(r, s, "outstanding seq re-sent on the replacement stream");
        t.record_sent(r, later); // re-send on the new stream
        assert_eq!(
            t.sent_at(s),
            Some(t0),
            "re-send must not move the send time later"
        );
        // Either stream's ack satisfies it, exactly once.
        assert_eq!(t.on_ack(s, true, later), Ok(()));
        assert_eq!(t.latest_accepted_sent_at(), Some(t0));
        assert!(matches!(
            t.on_ack(s, true, later),
            Err(AckRejection::NotNewer { .. })
        ));
        // Nothing outstanding any more → a new seq.
        assert_eq!(t.seq_for_replacement(), s + 1);
    }

    // ── Phase B1: serving state, deadline, lost_at (§4.4, §5) ──────────────

    /// A tracker that has sent request 1 at `t0` and accepted its ack.
    fn serving_at(t0: Instant) -> AckTracker {
        let mut t = tracker_with_sent(&[1], t0);
        assert_eq!(t.on_ack(1, true, t0 + Duration::from_millis(10)), Ok(()));
        t
    }

    /// Next seq, sent at `at`.
    fn send(t: &mut AckTracker, at: Instant) -> u64 {
        let s = t.next_seq();
        t.record_sent(s, at);
        s
    }

    /// AB1-1 (T15/T26): every process starts closed; the first accepted ack
    /// opens it with D = sent + H (the send time, not the receipt time).
    #[test]
    fn starts_closed_and_first_accepted_ack_opens_with_send_time_deadline() {
        let t0 = Instant::now();
        let mut t = AckTracker::new(H);
        let gate = t.gate();
        assert_eq!(t.state(), ServingState::Starting);
        assert!(!gate.admits_now(), "nothing is admitted before the first ack");
        assert!(!t.snapshot().admits(t0));
        t.next_seq();
        t.record_sent(1, t0);
        assert!(!gate.admits_now(), "a send alone opens nothing");
        assert_eq!(t.on_ack(1, true, t0 + Duration::from_secs(3)), Ok(()));
        assert_eq!(t.state(), ServingState::Serving);
        assert_eq!(t.deadline(), Some(t0 + H));
        let snap = gate.snapshot();
        assert_eq!(snap.state(), ServingState::Serving, "published");
        assert_eq!(snap.deadline(), Some(t0 + H), "published with D");
        assert!(snap.admits(t0 + H - Duration::from_millis(1)));
        assert!(!snap.admits(t0 + H), "closed exactly at D");
    }

    /// AB1-2 (T3): sending (or assigning) requests never moves D.
    #[test]
    fn a_send_never_moves_the_deadline() {
        let t0 = Instant::now();
        let mut t = serving_at(t0);
        let d = t.deadline();
        send(&mut t, t0 + Duration::from_secs(5));
        t.next_seq();
        t.seq_for_replacement();
        assert_eq!(t.deadline(), d);
        assert_eq!(t.gate().snapshot().deadline(), d);
    }

    /// AB1-3 (T4), the deadline row: in Serving, an ack for a request sent
    /// before D that arrives at or after D is rejected, and the connector is
    /// already Disabled with lost_at = D.
    #[test]
    fn late_ack_after_deadline_is_rejected() {
        let t0 = Instant::now();
        let mut t = serving_at(t0);
        let s2 = send(&mut t, t0 + Duration::from_secs(10));
        let at_d = t0 + H;
        assert_eq!(t.on_ack(s2, true, at_d), Err(AckRejection::BeforeLoss));
        assert_eq!(t.state(), ServingState::Disabled);
        assert_eq!(t.lost_at(), Some(t0 + H));
        assert!(!t.snapshot().admits(at_d));
    }

    /// AB1-3 (T4), the lost_at row in Disabled and in Recovering; a request
    /// sent at or after lost_at is accepted and reopens with its own D.
    #[test]
    fn pre_loss_acks_rejected_post_loss_ack_accepted() {
        let t0 = Instant::now();
        let mut t = serving_at(t0);
        let before = send(&mut t, t0 + Duration::from_secs(14));
        t.expire(t0 + H);
        assert_eq!(t.state(), ServingState::Disabled);
        let lost = t0 + H;
        let after = send(&mut t, lost); // sent exactly at lost_at: admissible
        // Disabled: the pre-loss request is rejected.
        assert_eq!(
            t.on_ack(before, true, lost + Duration::from_secs(1)),
            Err(AckRejection::BeforeLoss)
        );
        t.stream_up(lost + Duration::from_secs(1));
        assert_eq!(t.state(), ServingState::Recovering);
        assert_eq!(t.lost_at(), Some(lost), "Disabled → Recovering keeps lost_at (§5.2)");
        // Recovering: still rejected.
        assert_eq!(
            t.on_ack(before, true, lost + Duration::from_secs(2)),
            Err(AckRejection::BeforeLoss)
        );
        assert_eq!(t.on_ack(after, true, lost + Duration::from_secs(2)), Ok(()));
        assert_eq!(t.state(), ServingState::Serving);
        assert_eq!(t.deadline(), Some(lost + H));
    }

    /// AB1-4 (§5.2): the transition table, with injected time.
    #[test]
    fn transition_table() {
        use ServingState::*;
        let t0 = Instant::now();
        let ms = Duration::from_millis;

        // Starting: stream events don't open anything.
        let mut t = AckTracker::new(H);
        t.stream_up(t0);
        t.stream_down(t0);
        assert_eq!(t.state(), Starting);
        assert_eq!(t.lost_at(), None);

        // Serving → ServingDisconnected on stream end; still open until D.
        let mut t = serving_at(t0);
        t.stream_down(t0 + ms(100));
        assert_eq!(t.state(), ServingDisconnected);
        assert!(t.snapshot().admits(t0 + H - ms(1)));
        // ServingDisconnected → Serving on an accepted ack (before D).
        let s = send(&mut t, t0 + ms(200));
        assert_eq!(t.on_ack(s, true, t0 + ms(300)), Ok(()));
        assert_eq!(t.state(), Serving);
        assert_eq!(t.deadline(), Some(t0 + ms(200) + H));

        // ServingDisconnected → Disabled at D, lost_at = D even when the
        // event is processed late.
        let mut t = serving_at(t0);
        t.stream_down(t0 + ms(100));
        t.expire(t0 + H + Duration::from_secs(30));
        assert_eq!(t.state(), Disabled);
        assert_eq!(t.lost_at(), Some(t0 + H));

        // Serving → Disabled at D; before D nothing changes.
        let mut t = serving_at(t0);
        t.expire(t0 + H - ms(1));
        assert_eq!(t.state(), Serving);
        t.expire(t0 + H);
        assert_eq!(t.state(), Disabled);
        assert_eq!(t.lost_at(), Some(t0 + H));
        assert_eq!(t.deadline(), None);

        // Disabled → Recovering on a stream; Recovering → Disabled when it
        // ends, with lost_at moved to that instant.
        t.stream_up(t0 + H + ms(10));
        assert_eq!(t.state(), Recovering);
        assert!(!t.snapshot().admits(t0 + H + ms(10)));
        t.stream_down(t0 + H + ms(50));
        assert_eq!(t.state(), Disabled);
        assert_eq!(t.lost_at(), Some(t0 + H + ms(50)));

        // A stream event after D in Serving first applies the expiry.
        let mut t = serving_at(t0);
        t.stream_up(t0 + H + ms(1));
        assert_eq!(t.state(), Recovering, "expired to Disabled, then stream up");
        assert_eq!(t.lost_at(), Some(t0 + H));
    }

    /// AB1-5 / AB1-6 / AB1-7 (T5, T19, T16, T20): terminal signals close at
    /// once (no wait for D), are distinct, absorbing, and win over acks.
    #[test]
    fn terminal_states_close_immediately_and_absorb_everything() {
        let t0 = Instant::now();
        for (signal, state) in [
            (TerminalSignal::Revoked, ServingState::Revoked),
            (TerminalSignal::UnknownIdentity, ServingState::UnknownIdentity),
        ] {
            let mut t = serving_at(t0);
            let gate = t.gate();
            let pending = send(&mut t, t0 + Duration::from_secs(1));
            assert!(t.on_terminal(signal));
            assert_eq!(t.state(), state);
            assert!(state.is_terminal());
            assert_eq!(t.deadline(), None);
            assert!(!gate.snapshot().admits(t0 + Duration::from_secs(2)), "closed before D");
            // Absorbing: the other terminal kind, acks, stream and time events.
            let other = match signal {
                TerminalSignal::Revoked => TerminalSignal::UnknownIdentity,
                TerminalSignal::UnknownIdentity => TerminalSignal::Revoked,
            };
            assert!(!t.on_terminal(other));
            assert_eq!(
                t.on_ack(pending, true, t0 + Duration::from_secs(2)),
                Err(AckRejection::Terminal(signal))
            );
            t.stream_up(t0 + Duration::from_secs(3));
            t.stream_down(t0 + Duration::from_secs(4));
            t.expire(t0 + H * 3);
            assert_eq!(t.state(), state);
            assert_eq!(gate.snapshot().state(), state);
        }
        // Distinct from Disabled.
        let mut t = serving_at(t0);
        t.expire(t0 + H);
        assert_eq!(t.state(), ServingState::Disabled);
        assert!(!t.state().is_terminal());
    }

    /// AB1-12 (§4.6): without the capability no ack opens anything, even
    /// with a valid outstanding request.
    #[test]
    fn capability_absent_never_opens() {
        let t = AckTracker::shared(H);
        let gate = t.lock().gate();
        t.lock().next_seq();
        t.lock().record_sent(1, Instant::now());
        let mut s = StreamAuthz::default();
        observe_controller_msg(&ack(1), &mut s, &t);
        observe_controller_msg(&policy(false), &mut s, &t);
        observe_controller_msg(&ack(1), &mut s, &t);
        assert_eq!(t.lock().state(), ServingState::Starting);
        assert!(!gate.admits_now());
    }

    /// The first controller message on a stream is the stream-up event.
    #[test]
    fn first_controller_message_moves_disabled_to_recovering() {
        let t0 = Instant::now();
        let mut inner = serving_at(t0);
        inner.expire(t0 + H);
        let t = Arc::new(Mutex::new(inner));
        let mut s = StreamAuthz::default();
        observe_controller_msg(&policy(true), &mut s, &t);
        assert_eq!(t.lock().state(), ServingState::Recovering);
    }

    /// The expiry task moves Serving to Disabled at D without any other
    /// event (real time, short H).
    #[tokio::test]
    async fn expiry_task_disables_at_deadline() {
        let h = Duration::from_millis(150);
        let t = AckTracker::shared(h);
        let gate = t.lock().gate();
        let _task = spawn_expiry(t.clone());
        let t0 = Instant::now();
        t.lock().next_seq();
        t.lock().record_sent(1, t0);
        assert_eq!(t.lock().on_ack(1, true, Instant::now()), Ok(()));
        assert!(gate.admits_now());
        tokio::time::sleep(h + Duration::from_millis(150)).await;
        assert_eq!(t.lock().state(), ServingState::Disabled);
        assert_eq!(t.lock().lost_at(), Some(t0 + h));
        assert_eq!(gate.snapshot().state(), ServingState::Disabled);
        assert!(!gate.admits_now());
    }

    // ── send-boundary stamping ─────────────────────────────────────────────

    fn health(seq: u64) -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::ConnectorHealth(ConnectorHealthReport {
                heartbeat_seq: seq,
                ..Default::default()
            })),
        }
    }

    #[tokio::test]
    async fn stamps_at_transport_pull_not_at_enqueue() {
        let tracker = AckTracker::shared(H);
        let (tx, rx) = tokio::sync::mpsc::channel(4);
        let mut stamped = Box::pin(stamp_sends(ReceiverStream::new(rx), tracker.clone()));
        tx.send(health(1)).await.unwrap(); // enqueued into the mailbox
        let enqueued_at = Instant::now();
        assert_eq!(tracker.lock().sent_at(1), None, "enqueue is not a send");
        tokio::time::sleep(Duration::from_millis(50)).await;
        let before_pull = Instant::now();
        let _ = stamped.next().await.unwrap(); // the transport pulls it
        let stamp = tracker.lock().sent_at(1).expect("stamped at pull");
        assert!(
            stamp >= before_pull && stamp >= enqueued_at + Duration::from_millis(50),
            "stamp {stamp:?} must be the pull instant, not the enqueue instant {enqueued_at:?}"
        );
        // A non-authorization message (seq 0) is never recorded.
        tx.send(health(0)).await.unwrap();
        let _ = stamped.next().await.unwrap();
        assert_eq!(tracker.lock().sent_at(0), None);
    }

    // ── classification: markers only, never message text ──────────────────

    fn marked(code: tonic::Code, msg: &str, marker: Option<&str>) -> tonic::Status {
        let mut md = tonic::metadata::MetadataMap::new();
        if let Some(m) = marker {
            md.insert(CONNECTOR_STATE_MARKER_KEY, m.parse().unwrap());
        }
        tonic::Status::with_metadata(code, msg, md)
    }

    #[test]
    fn classifies_markers_not_text() {
        use tonic::Code;
        assert_eq!(
            classify_status(&marked(Code::PermissionDenied, "x", Some("revoked"))),
            StatusClass::Revoked
        );
        assert_eq!(
            classify_status(&marked(Code::NotFound, "x", Some("unknown"))),
            StatusClass::UnknownConnector
        );
        // Text that "looks" terminal without a marker is not terminal.
        assert_eq!(
            classify_status(&marked(
                Code::PermissionDenied,
                "connector is revoked",
                None
            )),
            StatusClass::Unmarked(Code::PermissionDenied)
        );
        assert_eq!(
            classify_status(&marked(Code::NotFound, "connector not found", None)),
            StatusClass::Unmarked(Code::NotFound)
        );
        assert_eq!(
            classify_status(&marked(Code::Unavailable, "db down", None)),
            StatusClass::Unmarked(Code::Unavailable)
        );
        // Unrecognised marker value or casing: not terminal.
        assert_eq!(
            classify_status(&marked(Code::PermissionDenied, "x", Some("REVOKED"))),
            StatusClass::Unmarked(Code::PermissionDenied)
        );
        // Transport-style status (tonic-generated, no metadata).
        assert_eq!(
            classify_status(&tonic::Status::unavailable("connection reset")),
            StatusClass::Unmarked(Code::Unavailable)
        );
    }

    #[test]
    fn observe_status_records_matching_terminal_only() {
        let t = AckTracker::shared(H);
        observe_status(&marked(tonic::Code::Unavailable, "x", None), &t, "test");
        assert_eq!(t.lock().terminal(), None, "Unavailable never terminal");
        observe_status(
            &marked(tonic::Code::NotFound, "x", Some("unknown")),
            &t,
            "test",
        );
        assert_eq!(t.lock().terminal(), Some(TerminalSignal::UnknownIdentity));
        let t = AckTracker::shared(H);
        observe_status(
            &marked(tonic::Code::PermissionDenied, "x", Some("revoked")),
            &t,
            "test",
        );
        assert_eq!(t.lock().terminal(), Some(TerminalSignal::Revoked));
    }

    // ── per-stream capability ──────────────────────────────────────────────

    fn policy(acks: bool) -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::AgentPolicy(AgentPolicy {
                shield_renewal_window_secs: 0,
                authorization_acks: acks,
            })),
        }
    }
    fn ack(seq: u64) -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::AuthorizationAck(AuthorizationAck {
                heartbeat_seq: seq,
            })),
        }
    }

    #[test]
    fn capability_absent_or_false_rejects_acks() {
        // No AgentPolicy at all (old controller).
        let t = AckTracker::shared(H);
        t.lock().next_seq();
        t.lock().record_sent(1, Instant::now());
        let mut s = StreamAuthz::default();
        observe_controller_msg(&ack(1), &mut s, &t);
        assert_eq!(t.lock().highest_accepted(), 0);
        // authorization_acks = false.
        let mut s = StreamAuthz::default();
        observe_controller_msg(&policy(false), &mut s, &t);
        observe_controller_msg(&ack(1), &mut s, &t);
        assert_eq!(t.lock().highest_accepted(), 0);
        // Asserted capability → accepted.
        let mut s = StreamAuthz::default();
        observe_controller_msg(&policy(true), &mut s, &t);
        observe_controller_msg(&ack(1), &mut s, &t);
        assert_eq!(t.lock().highest_accepted(), 1);
    }

    #[test]
    fn connector_revoked_event_is_terminal_and_blocks_later_acks() {
        let t = AckTracker::shared(H);
        t.lock().next_seq();
        t.lock().record_sent(1, Instant::now());
        let mut s = StreamAuthz::default();
        observe_controller_msg(&policy(true), &mut s, &t);
        observe_controller_msg(
            &ConnectorControlMessage {
                body: Some(CBody::ConnectorRevoked(ConnectorRevoked {})),
            },
            &mut s,
            &t,
        );
        assert_eq!(t.lock().terminal(), Some(TerminalSignal::Revoked));
        observe_controller_msg(&ack(1), &mut s, &t);
        assert_eq!(t.lock().highest_accepted(), 0, "no ack after revocation");
    }
}
