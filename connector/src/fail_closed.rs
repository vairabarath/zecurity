//! Connector-revocation fail-closed (design §7 steps 3–5, D15; Phase B2).
//!
//! One task follows the published serving state and acts on every loss of
//! authorization:
//! * **step 3** — every live session (connector- and shield-routed) is
//!   cancelled, on every open → closed transition (counted, so a brief
//!   closure that already reopened is not missed);
//! * **step 4** — terminal: every shield Control stream is closed at once;
//!   recoverable `Disabled`/`Recovering` (D15): shield streams stay open,
//!   carrying nothing, and are closed after `DISABLED_SHIELD_CLOSE_SECS`
//!   unless serving resumes first;
//! * **step 5** — terminal: the ACL cache is cleared.
//!
//! New traffic is already refused by the admission gate (B1); this task only
//! ends what is already running. It never opens anything.

use std::sync::Arc;
use std::time::Duration;

use tokio::task::JoinHandle;
use tokio::time::{sleep_until, Instant};
use tracing::{info, warn};

use crate::agent_server::ShieldRegistry;
use crate::authorization::{AuthzGate, ServingState};
use crate::policy::PolicyCache;
use crate::session_registry::SessionRegistry;

pub struct FailClosed {
    pub sessions: Arc<SessionRegistry>,
    pub shields: ShieldRegistry,
    pub policy: Arc<PolicyCache>,
    /// D15 grace before shield streams are closed in `Disabled`.
    pub disabled_shield_close: Duration,
}

impl FailClosed {
    pub fn new(
        sessions: Arc<SessionRegistry>,
        shields: ShieldRegistry,
        policy: Arc<PolicyCache>,
    ) -> Self {
        Self {
            sessions,
            shields,
            policy,
            disabled_shield_close: Duration::from_secs(
                crate::authz_timing::DISABLED_SHIELD_CLOSE_SECS,
            ),
        }
    }
}

pub fn spawn(gate: &AuthzGate, fc: FailClosed) -> JoinHandle<()> {
    let mut rx = gate.subscribe();
    tokio::spawn(async move {
        let mut seen_closures = rx.borrow().closures();
        let mut terminal_done = false;
        // D15: when the shield streams of the current Disabled episode close;
        // None outside an episode or once they have been closed.
        let mut shield_close_at: Option<Instant> = None;
        let mut episode_closed = false;
        loop {
            let snap = *rx.borrow_and_update();
            if snap.closures() != seen_closures {
                seen_closures = snap.closures();
                fc.sessions.cancel_every_session("authorization lost");
            }
            match snap.state() {
                ServingState::Revoked | ServingState::UnknownIdentity => {
                    if !terminal_done {
                        terminal_done = true;
                        fc.sessions.cancel_every_session("terminal state");
                        fc.shields.close_all_streams("terminal state");
                        fc.policy.clear();
                        warn!(state = ?snap.state(), "fail-closed: terminal — sessions cancelled, shield streams closed, ACL cleared");
                    }
                    shield_close_at = None;
                }
                ServingState::Disabled | ServingState::Recovering => {
                    if shield_close_at.is_none() && !episode_closed {
                        shield_close_at = Some(Instant::now() + fc.disabled_shield_close);
                        info!(
                            grace_secs = fc.disabled_shield_close.as_secs(),
                            "not serving: shield streams kept open, carrying nothing, until the grace ends"
                        );
                    }
                }
                ServingState::Serving | ServingState::ServingDisconnected | ServingState::Starting => {
                    if shield_close_at.take().is_some() {
                        info!("serving again before the grace ended: shield streams kept");
                    }
                    episode_closed = false;
                }
            }
            tokio::select! {
                changed = rx.changed() => {
                    if changed.is_err() {
                        return;
                    }
                }
                _ = async { sleep_until(shield_close_at.expect("guarded")).await }, if shield_close_at.is_some() => {
                    shield_close_at = None;
                    episode_closed = true;
                    fc.shields.close_all_streams("not serving past the D15 grace");
                }
            }
        }
    })
}

#[cfg(test)]
mod tests {
    //! AB2-4/5/6: what the enforcer does on each kind of loss, with the
    //! production tracker driving the state and a paused clock for D15.
    use super::*;
    use crate::authorization::{AckTracker, SharedAckTracker, TerminalSignal};
    use crate::session_registry::SessionTransport;
    use tokio_util::sync::CancellationToken;
    use tonic::transport::Channel;

    const H: Duration = Duration::from_secs(15);
    const GRACE: Duration = Duration::from_secs(30);

    struct Fx {
        authz: SharedAckTracker,
        sessions: Arc<SessionRegistry>,
        shields: ShieldRegistry,
        policy: Arc<PolicyCache>,
        _task: JoinHandle<()>,
    }

    fn fx() -> Fx {
        let authz = AckTracker::shared(H);
        let sessions = Arc::new(SessionRegistry::new());
        let (ack_tx, _) = tokio::sync::mpsc::channel(8);
        let policy = Arc::new(PolicyCache::new());
        let shields = ShieldRegistry::new(
            Channel::from_static("http://127.0.0.1:1").connect_lazy(),
            crate::test_support::TRUST_DOMAIN.to_string(),
            crate::test_support::CONNECTOR_ID.to_string(),
            ack_tx,
            policy.clone(),
            authz.lock().gate(),
        );
        let task = spawn(
            &authz.lock().gate(),
            FailClosed {
                sessions: sessions.clone(),
                shields: shields.clone(),
                policy: policy.clone(),
                disabled_shield_close: GRACE,
            },
        );
        Fx { authz, sessions, shields, policy, _task: task }
    }

    /// Accept a fresh ack now: → Serving.
    fn serve(authz: &SharedAckTracker) {
        let mut t = authz.lock();
        let s = t.next_seq();
        let now = Instant::now();
        t.record_sent(s, now);
        t.on_ack(s, true, now).unwrap();
    }

    fn sessions(f: &Fx) -> Vec<(CancellationToken, crate::session_registry::RegistryGuard)> {
        vec![
            f.sessions.register(("spiffe://c/1".into(), "res-a".into()), SessionTransport::Quic),
            f.sessions.register(("spiffe://c/2".into(), "res-shield".into()), SessionTransport::Relay),
        ]
    }

    async fn settle() {
        for _ in 0..20 {
            tokio::task::yield_now().await;
        }
    }

    /// AB2-4 (T9, §7 step 3): every live session — connector- and
    /// shield-routed — is cancelled when authorization is lost.
    #[tokio::test(start_paused = true)]
    async fn losing_authorization_cancels_every_session() {
        let f = fx();
        serve(&f.authz);
        let live = sessions(&f);
        settle().await;
        assert!(live.iter().all(|(t, _)| !t.is_cancelled()), "serving: untouched");
        tokio::time::sleep(H).await;
        f.authz.lock().expire(Instant::now());
        settle().await;
        assert!(live.iter().all(|(t, _)| t.is_cancelled()), "Disabled: all cancelled");
    }

    /// A closure that reopens before the enforcer looks is still acted on
    /// (closure counter), so no session survives a lapse.
    #[tokio::test(start_paused = true)]
    async fn a_brief_closure_is_not_missed() {
        let f = fx();
        serve(&f.authz);
        let live = sessions(&f);
        settle().await;
        {
            let mut t = f.authz.lock();
            let now = Instant::now();
            t.expire(now + H); // → Disabled
            t.stream_up(now + H); // → Recovering
            let s = t.next_seq();
            t.record_sent(s, now + H);
            t.on_ack(s, true, now + H).unwrap(); // → Serving again
        }
        settle().await;
        assert!(live.iter().all(|(t, _)| t.is_cancelled()));
    }

    /// AB2-5 (D15, T13) + AB2-6 (step 5): terminal closes shield streams and
    /// clears the ACL at once.
    #[tokio::test(start_paused = true)]
    async fn terminal_closes_shields_and_clears_the_acl_immediately() {
        let f = fx();
        f.policy.update(crate::client::v1::AclSnapshot { version: 7, ..Default::default() });
        serve(&f.authz);
        let live = sessions(&f);
        settle().await;
        f.authz.lock().on_terminal(TerminalSignal::Revoked);
        settle().await;
        assert_eq!(f.shields.close_all_count(), 1, "shield streams closed");
        assert_eq!(f.policy.version(), 0, "ACL cleared");
        assert!(live.iter().all(|(t, _)| t.is_cancelled()));
        // A snapshot pushed afterwards is applied but the gate stays closed.
        f.policy.update(crate::client::v1::AclSnapshot { version: 8, ..Default::default() });
        assert_eq!(f.policy.version(), 8);
        assert!(!f.authz.lock().gate().admits_now());
        tokio::time::sleep(GRACE * 2).await;
        assert_eq!(f.shields.close_all_count(), 1, "closed once");
    }

    /// AB2-5 (D15): Disabled keeps shield streams for exactly the grace, then
    /// closes them once; Recovering doesn't restart the grace.
    #[tokio::test(start_paused = true)]
    async fn disabled_closes_shields_after_exactly_the_grace() {
        let f = fx();
        serve(&f.authz);
        settle().await;
        tokio::time::sleep(H).await;
        let t0 = Instant::now();
        f.authz.lock().expire(t0); // Disabled at D
        settle().await;
        tokio::time::sleep_until(t0 + GRACE - Duration::from_millis(1)).await;
        f.authz.lock().stream_up(Instant::now()); // → Recovering: same episode
        settle().await;
        assert_eq!(f.shields.close_all_count(), 0, "kept during the grace");
        tokio::time::sleep_until(t0 + GRACE).await;
        settle().await;
        assert_eq!(f.shields.close_all_count(), 1, "closed at the grace");
        tokio::time::sleep(GRACE * 3).await;
        assert_eq!(f.shields.close_all_count(), 1, "once per episode");
        assert!(f.policy.version() == 0, "Disabled never clears the ACL (only terminal)");
    }

    /// AB2-5 (D15): serving again before the grace ends cancels the close.
    #[tokio::test(start_paused = true)]
    async fn recovery_before_the_grace_keeps_shield_streams() {
        let f = fx();
        f.policy.update(crate::client::v1::AclSnapshot { version: 3, ..Default::default() });
        serve(&f.authz);
        settle().await;
        tokio::time::sleep(H).await;
        f.authz.lock().expire(Instant::now());
        settle().await;
        tokio::time::sleep(GRACE / 3).await;
        serve(&f.authz); // a request sent after the loss, acknowledged
        settle().await;
        tokio::time::sleep(GRACE * 2).await;
        settle().await;
        assert_eq!(f.shields.close_all_count(), 0);
        assert_eq!(f.policy.version(), 3, "a recoverable loss keeps the ACL");
    }
}
