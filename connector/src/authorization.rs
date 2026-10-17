//! Controller ↔ connector authorization contract — connector-side protocol
//! plumbing (connector-revocation design §4.4–§4.6, Phase A).
//!
//! Phase A scope: this module only *records and validates* the protocol. It
//! assigns heartbeat sequence numbers, timestamps each authorization request
//! at the transport-send boundary, validates `AuthorizationAck`s, and
//! recognises the terminal signals (`ConnectorRevoked`, the `revoked` /
//! `unknown` trailer markers). It does NOT gate the data plane, run a
//! deadline, or stop the reconnect loop — that is Phase B1/B2. Nothing here
//! treats a missing acknowledgement, a missing capability or an unsupported
//! controller as authorization.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::Duration;

use parking_lot::Mutex;
use tokio::time::Instant;
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
        }
    }
}

/// Process-global authorization-request bookkeeping. Owned by the control
/// stream's outer loop, so it survives reconnects and renewal switches:
/// sequence numbers start at 1 and never reset.
#[derive(Debug)]
pub struct AckTracker {
    /// H: maximum authorization freshness.
    freshness: Duration,
    /// Last seq assigned (0 = none yet).
    last_assigned: u64,
    /// seq → instant the request was handed to the transport (first send).
    sent: BTreeMap<u64, Instant>,
    highest_accepted: u64,
    /// Send instant of the request behind the latest accepted ack. Phase B
    /// derives the deadline D = this + H; Phase A only records it.
    latest_accepted_sent_at: Option<Instant>,
    terminal: Option<TerminalSignal>,
}

pub type SharedAckTracker = Arc<Mutex<AckTracker>>;

impl AckTracker {
    pub fn new(freshness: Duration) -> Self {
        Self {
            freshness,
            last_assigned: 0,
            sent: BTreeMap::new(),
            highest_accepted: 0,
            latest_accepted_sent_at: None,
            terminal: None,
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
        self.highest_accepted = seq;
        self.latest_accepted_sent_at = Some(sent_at);
        self.sent.retain(|k, _| *k > seq);
        Ok(())
    }

    /// Record a terminal signal. The first one processed wins; returns
    /// whether this call set it.
    pub fn on_terminal(&mut self, signal: TerminalSignal) -> bool {
        if self.terminal.is_some() {
            return false;
        }
        self.terminal = Some(signal);
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
                "controller sent ConnectorRevoked: this connector identity is administratively revoked (terminal; Phase A records it, serving gate is Phase B1)"
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
