//! FU-8 (ADR-018 amendment 2026-11-02): advisory wake-up hints.
//!
//! The daemon keeps one `WatchChanges` stream open to the controller. Each
//! message carries only the workspace's current ACL and transport versions.
//! When either differs from the cached snapshot, this wakes the ACL sync
//! scheduler (`RuntimeState::change_hint`), which reconciles through the normal
//! GetACLSnapshot / GetTransportSnapshot path. A hint grants nothing: access
//! only ever comes from that authoritative sync. The scheduler's 60 s tick runs
//! whether or not the stream is up, so a lost hint or a dead stream only costs
//! speed, never correctness.
//!
//! Wakes coalesce in the Notify (one pending permit at most) and the scheduler
//! runs syncs one at a time, so a burst of hints never starts concurrent syncs.

use std::future::Future;
use std::pin::Pin;
use std::time::Duration;

use futures::{Stream, StreamExt};
use rand::Rng;
use tokio::sync::Notify;
use tonic::{Code, Status};
use tracing::{debug, info, warn};

use crate::config;
use crate::grpc::{
    self,
    client_v1::{ChangeHint, WatchChangesRequest},
};
use crate::runtime::{DeviceState, SharedState};

pub type HintStream = Pin<Box<dyn Stream<Item = Result<ChangeHint, Status>> + Send>>;

/// Reconnect and liveness timing.
#[derive(Clone, Copy, Debug)]
pub struct Timing {
    /// First reconnect delay, and the delay after any stream that delivered a hint.
    pub first: Duration,
    /// Cap of the doubling reconnect delay.
    pub max: Duration,
    /// No message (not even the controller's ~60 s heartbeat) for this long
    /// means a dead connection: reconnect.
    pub idle: Duration,
    /// Recheck interval while logged out or while the device carries a directive.
    pub not_ready: Duration,
}

impl Default for Timing {
    fn default() -> Self {
        Self {
            first: Duration::from_secs(1),
            max: Duration::from_secs(60),
            idle: Duration::from_secs(150),
            not_ready: Duration::from_secs(5),
        }
    }
}

/// Daemon entry point: runs for the daemon's lifetime.
pub async fn run(state: SharedState, conf: config::ClientConf) {
    let wake = { state.read().await.change_hint.clone() };
    let open_state = state.clone();
    watch_loop(
        &state,
        &wake,
        Timing::default(),
        move || open(open_state.clone(), conf.clone()),
        equal_jitter,
    )
    .await
}

/// A delay drawn from [d/2, d], so many clients that lost the controller at
/// once don't reconnect in lockstep.
pub fn equal_jitter(d: Duration) -> Duration {
    d.mul_f64(rand::thread_rng().gen_range(0.5..=1.0))
}

// tonic::Status is the natural error of a gRPC call; the loop inspects its code.
#[allow(clippy::result_large_err)]
async fn open(state: SharedState, conf: config::ClientConf) -> Result<HintStream, Status> {
    let (ca_pem, device_id, access_token) = {
        let s = state.read().await;
        match (s.device.as_ref(), s.session.as_ref()) {
            (Some(d), Some(sess)) => (
                d.ca_cert_pem.clone(),
                d.id.clone(),
                sess.access_token.clone(),
            ),
            _ => return Err(Status::failed_precondition("not logged in")),
        }
    };
    let mut client = grpc::connect_grpc(conf.controller(), &ca_pem)
        .await
        .map_err(|e| Status::unavailable(format!("connect: {e:#}")))?;
    let stream = client
        .watch_changes(WatchChangesRequest {
            access_token,
            device_id,
        })
        .await?
        .into_inner();
    Ok(Box::pin(stream))
}

/// Logged in with an active device: the only state in which a stream is useful.
async fn ready(state: &SharedState) -> bool {
    let s = state.read().await;
    s.device.is_some() && s.session.is_some() && s.device_state == DeviceState::Active
}

/// Whether `hint` names versions other than the cached snapshots'. Equality,
/// not order: versions restart at 0 when the controller restarts. A duplicate
/// or stale hint at worst causes one extra up-to-date poll.
pub fn hint_needs_sync(hint: &ChangeHint, acl: Option<u64>, transport: Option<u64>) -> bool {
    acl != Some(hint.acl_version) || transport != Some(hint.transport_version)
}

async fn cached_versions(state: &SharedState) -> (Option<u64>, Option<u64>) {
    let s = state.read().await;
    (
        s.acl_snapshot.as_ref().map(|a| a.version),
        s.transport_snapshot.as_ref().map(|t| t.version),
    )
}

/// The reconnect loop, with the stream opener and jitter injected for tests.
pub async fn watch_loop<O, Fut, J>(
    state: &SharedState,
    wake: &Notify,
    timing: Timing,
    mut open: O,
    mut jitter: J,
) where
    O: FnMut() -> Fut,
    Fut: Future<Output = Result<HintStream, Status>>,
    J: FnMut(Duration) -> Duration,
{
    let mut backoff = timing.first;
    loop {
        if !ready(state).await {
            tokio::time::sleep(timing.not_ready).await;
            continue;
        }
        match open().await {
            Ok(mut stream) => {
                debug!("change hints: stream open");
                loop {
                    match tokio::time::timeout(timing.idle, stream.next()).await {
                        Ok(Some(Ok(hint))) => {
                            backoff = timing.first;
                            if !ready(state).await {
                                // Logged out, or a directive arrived via the poll.
                                break;
                            }
                            let (acl, transport) = cached_versions(state).await;
                            if hint_needs_sync(&hint, acl, transport) {
                                info!(
                                    acl_version = hint.acl_version,
                                    transport_version = hint.transport_version,
                                    "change hint: versions moved, syncing now"
                                );
                                wake.notify_one();
                            }
                        }
                        Ok(Some(Err(status))) => {
                            stream_ended(&status, wake);
                            break;
                        }
                        Ok(None) => {
                            debug!("change hints: controller closed the stream");
                            break;
                        }
                        Err(_) => {
                            warn!(
                                idle_secs = timing.idle.as_secs(),
                                "change hints: no message within the idle limit, reconnecting"
                            );
                            break;
                        }
                    }
                }
            }
            Err(status) => stream_ended(&status, wake),
        }
        tokio::time::sleep(jitter(backoff)).await;
        backoff = (backoff * 2).min(timing.max);
    }
}

fn stream_ended(status: &Status, wake: &Notify) {
    match status.code() {
        // Expected every token lifetime. The sync refreshes the session on
        // Unauthenticated, so wake it rather than duplicating the refresh.
        Code::Unauthenticated => {
            debug!(
                message = status.message(),
                "change hints: token rejected or expired"
            );
            wake.notify_one();
        }
        Code::Aborted => debug!("change hints: replaced by a newer stream"),
        _ => {
            debug!(code = ?status.code(), message = status.message(), "change hints: stream unavailable")
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::grpc::client_v1::{AclSnapshot, TransportSnapshot};
    use crate::runtime::{DeviceInfo, SessionInfo};
    use futures::channel::mpsc;
    use std::sync::{Arc, Mutex};
    use tokio::time::Instant;

    fn hint(acl: u64, transport: u64) -> ChangeHint {
        ChangeHint {
            acl_version: acl,
            transport_version: transport,
        }
    }

    async fn logged_in(acl: u64, transport: u64) -> SharedState {
        let state = crate::runtime::new_shared();
        {
            let mut s = state.write().await;
            s.device = Some(DeviceInfo {
                id: "dev-1".into(),
                spiffe_id: "spiffe://td/client/dev-1".into(),
                certificate_pem: String::new(),
                private_key_pem: String::new(),
                tpm_key_material: None,
                ca_cert_pem: String::new(),
                cert_expires_at: 0,
                hostname: "h".into(),
                os: "linux".into(),
            });
            s.session = Some(SessionInfo {
                access_token: "a".into(),
                refresh_token: "r".into(),
                expires_at: i64::MAX,
            });
            s.acl_snapshot = Some(AclSnapshot {
                version: acl,
                ..Default::default()
            });
            s.transport_snapshot = Some(TransportSnapshot {
                version: transport,
                ..Default::default()
            });
        }
        state
    }

    fn fast() -> Timing {
        Timing {
            first: Duration::from_secs(1),
            max: Duration::from_secs(60),
            idle: Duration::from_secs(150),
            not_ready: Duration::from_secs(5),
        }
    }

    /// Whether a wake is pending (consumes it).
    async fn woken(wake: &Notify) -> bool {
        tokio::time::timeout(Duration::from_millis(1), wake.notified())
            .await
            .is_ok()
    }

    /// A controllable stream: the test pushes items through the sender.
    fn channel_stream() -> (
        mpsc::UnboundedSender<Result<ChangeHint, Status>>,
        HintStream,
    ) {
        let (tx, rx) = mpsc::unbounded();
        (tx, Box::pin(rx))
    }

    /// Yield until the spawned loop has drained what it can (paused clock).
    async fn settle() {
        for _ in 0..50 {
            tokio::task::yield_now().await;
        }
    }

    #[test]
    fn only_moved_versions_need_a_sync() {
        assert!(!hint_needs_sync(&hint(3, 2), Some(3), Some(2)), "duplicate");
        assert!(hint_needs_sync(&hint(4, 2), Some(3), Some(2)), "ACL moved");
        assert!(
            hint_needs_sync(&hint(3, 5), Some(3), Some(2)),
            "transport moved"
        );
        // Lower than cached (controller restarted, or out of order): sync and
        // let the authoritative snapshot decide; never trust the hint.
        assert!(hint_needs_sync(&hint(0, 0), Some(3), Some(2)));
        assert!(
            hint_needs_sync(&hint(0, 0), None, None),
            "nothing cached yet"
        );
    }

    #[tokio::test(start_paused = true)]
    async fn a_hint_with_new_versions_wakes_the_sync_and_a_duplicate_does_not() {
        let state = logged_in(3, 2).await;
        let wake = Arc::new(Notify::new());
        let (tx, stream) = channel_stream();
        let mut stream = Some(stream);
        let task = {
            let (state, wake) = (state.clone(), wake.clone());
            tokio::spawn(async move {
                watch_loop(
                    &state,
                    &wake,
                    fast(),
                    move || {
                        let s = stream.take();
                        async move { s.ok_or_else(|| Status::unavailable("only one stream")) }
                    },
                    |d| d,
                )
                .await
            })
        };

        tx.unbounded_send(Ok(hint(3, 2))).unwrap();
        settle().await;
        assert!(
            !woken(&wake).await,
            "a hint matching the cache must not sync"
        );

        tx.unbounded_send(Ok(hint(4, 2))).unwrap();
        settle().await;
        assert!(woken(&wake).await, "a new ACL version must wake the sync");

        tx.unbounded_send(Ok(hint(4, 9))).unwrap();
        settle().await;
        assert!(
            woken(&wake).await,
            "a new transport version must wake the sync"
        );
        task.abort();
    }

    // Many hints before the scheduler gets to run collapse into one pending
    // wake: the sync can never be started once per hint.
    #[tokio::test(start_paused = true)]
    async fn a_burst_of_hints_leaves_one_pending_sync() {
        let state = logged_in(0, 0).await;
        let wake = Arc::new(Notify::new());
        let (tx, stream) = channel_stream();
        let mut stream = Some(stream);
        let task = {
            let (state, wake) = (state.clone(), wake.clone());
            tokio::spawn(async move {
                watch_loop(
                    &state,
                    &wake,
                    fast(),
                    move || {
                        let s = stream.take();
                        async move { s.ok_or_else(|| Status::unavailable("only one stream")) }
                    },
                    |d| d,
                )
                .await
            })
        };
        for v in 1..=100 {
            tx.unbounded_send(Ok(hint(v, 0))).unwrap();
        }
        settle().await;
        assert!(woken(&wake).await);
        assert!(
            !woken(&wake).await,
            "100 hints left more than one pending sync"
        );
        task.abort();
    }

    /// Records when each open happened and fails it, or hands out a stream.
    type Opens = Arc<Mutex<Vec<Instant>>>;

    #[tokio::test(start_paused = true)]
    async fn reconnects_back_off_exponentially_up_to_the_cap() {
        let state = logged_in(0, 0).await;
        let wake = Notify::new();
        let opens: Opens = Arc::default();
        let rec = opens.clone();
        let fut = watch_loop(
            &state,
            &wake,
            fast(),
            move || {
                rec.lock().unwrap().push(Instant::now());
                async { Err(Status::unavailable("controller down")) }
            },
            |d| d,
        );
        let _ = tokio::time::timeout(Duration::from_secs(400), fut).await;
        let opens = opens.lock().unwrap();
        let gaps: Vec<u64> = opens.windows(2).map(|w| (w[1] - w[0]).as_secs()).collect();
        assert_eq!(&gaps[..8], &[1, 2, 4, 8, 16, 32, 60, 60], "gaps {gaps:?}");
    }

    #[test]
    fn jitter_stays_within_half_to_full_delay() {
        for _ in 0..1000 {
            let d = equal_jitter(Duration::from_secs(60));
            assert!(
                d >= Duration::from_secs(30) && d <= Duration::from_secs(60),
                "{d:?}"
            );
        }
    }

    #[tokio::test(start_paused = true)]
    async fn a_stream_that_delivered_a_hint_resets_the_backoff() {
        let state = logged_in(0, 0).await;
        let wake = Notify::new();
        let opens: Opens = Arc::default();
        let rec = opens.clone();
        let mut n = 0;
        let fut = watch_loop(
            &state,
            &wake,
            fast(),
            move || {
                rec.lock().unwrap().push(Instant::now());
                n += 1;
                let attempt = n;
                async move {
                    if attempt == 4 {
                        // One hint, then the connection drops.
                        let s: HintStream = Box::pin(futures::stream::iter(vec![
                            Ok(hint(0, 0)),
                            Err(Status::unavailable("reset")),
                        ]));
                        Ok(s)
                    } else {
                        Err(Status::unavailable("down"))
                    }
                }
            },
            |d| d,
        );
        let _ = tokio::time::timeout(Duration::from_secs(30), fut).await;
        let opens = opens.lock().unwrap();
        let gaps: Vec<u64> = opens.windows(2).map(|w| (w[1] - w[0]).as_secs()).collect();
        // 1, 2, 4 while failing; the 4th open delivered a hint, so the next
        // reconnect starts again at 1 s, then doubles.
        assert_eq!(&gaps[..5], &[1, 2, 4, 1, 2], "gaps {gaps:?}");
    }

    #[tokio::test(start_paused = true)]
    async fn a_silent_connection_is_dropped_after_the_idle_limit() {
        let state = logged_in(0, 0).await;
        let wake = Notify::new();
        let opens: Opens = Arc::default();
        let rec = opens.clone();
        let fut = watch_loop(
            &state,
            &wake,
            fast(),
            move || {
                rec.lock().unwrap().push(Instant::now());
                async {
                    let s: HintStream = Box::pin(futures::stream::pending());
                    Ok(s)
                }
            },
            |d| d,
        );
        let _ = tokio::time::timeout(Duration::from_secs(200), fut).await;
        let opens = opens.lock().unwrap();
        assert_eq!(opens.len(), 2, "one reconnect after 150 s of silence");
        assert_eq!((opens[1] - opens[0]).as_secs(), 151);
    }

    #[tokio::test(start_paused = true)]
    async fn token_expiry_wakes_the_sync_to_refresh_and_reconnects() {
        let state = logged_in(0, 0).await;
        let wake = Notify::new();
        let opens: Opens = Arc::default();
        let rec = opens.clone();
        let fut = watch_loop(
            &state,
            &wake,
            fast(),
            move || {
                rec.lock().unwrap().push(Instant::now());
                async {
                    let s: HintStream = Box::pin(futures::stream::iter(vec![
                        Ok(hint(0, 0)),
                        Err(Status::unauthenticated("access token expired")),
                    ]));
                    Ok(s)
                }
            },
            |d| d,
        );
        let _ = tokio::time::timeout(Duration::from_millis(1500), fut).await;
        assert!(
            woken(&wake).await,
            "an expired token must wake the sync (it refreshes)"
        );
        assert_eq!(opens.lock().unwrap().len(), 2, "reconnected after 1 s");
    }

    #[tokio::test(start_paused = true)]
    async fn no_stream_is_opened_while_logged_out_or_revoked() {
        let state = logged_in(0, 0).await;
        state.write().await.session = None;
        let wake = Notify::new();
        let opens: Opens = Arc::default();
        let rec = opens.clone();
        let state2 = state.clone();
        let fut = watch_loop(
            &state2,
            &wake,
            fast(),
            move || {
                rec.lock().unwrap().push(Instant::now());
                async { Err(Status::unavailable("x")) }
            },
            |d| d,
        );
        let _ = tokio::time::timeout(Duration::from_secs(30), fut).await;
        assert!(
            opens.lock().unwrap().is_empty(),
            "opened a stream while logged out"
        );

        let state = logged_in(0, 0).await;
        state.write().await.device_state = DeviceState::Revoked;
        let opens: Opens = Arc::default();
        let rec = opens.clone();
        let fut = watch_loop(
            &state,
            &wake,
            fast(),
            move || {
                rec.lock().unwrap().push(Instant::now());
                async { Err(Status::unavailable("x")) }
            },
            |d| d,
        );
        let _ = tokio::time::timeout(Duration::from_secs(30), fut).await;
        assert!(
            opens.lock().unwrap().is_empty(),
            "opened a stream for a revoked device"
        );
    }

    // Aborting the task (daemon shutdown) drops the open stream.
    #[tokio::test(start_paused = true)]
    async fn aborting_the_task_drops_the_stream() {
        let state = logged_in(0, 0).await;
        let wake = Arc::new(Notify::new());
        let (tx, stream) = channel_stream();
        let mut stream = Some(stream);
        let task = {
            let (state, wake) = (state.clone(), wake.clone());
            tokio::spawn(async move {
                watch_loop(
                    &state,
                    &wake,
                    fast(),
                    move || {
                        let s = stream.take();
                        async move { s.ok_or_else(|| Status::unavailable("only one stream")) }
                    },
                    |d| d,
                )
                .await
            })
        };
        settle().await;
        assert!(!tx.is_closed());
        task.abort();
        let _ = task.await;
        assert!(tx.is_closed(), "the stream outlived the task");
    }
}
