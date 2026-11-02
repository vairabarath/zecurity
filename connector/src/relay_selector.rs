// Sprint 11 ADR-016 Phase 2 — relay selector state machine.
//
// Drives the connector's relay attachment from the controller-pushed
// LabelledRelayList. Replaces the static `RELAY_ADDR` boot logic with a
// three-phase scheme:
//
//   1. Disconnected → first connect, preferring the persisted top-of-ranking
//      relay if it's still in the current list and fresh. Falls back to a
//      random Tier-1, then a Tier-2 (with a warning), then Backoff.
//   2. Connected → background-probe sweep on every list-version bump and
//      every RELAY_REPROBE_INTERVAL_SECS. Persists the top-5 ranking. If the
//      best candidate's score is materially better than the active relay's
//      score (>15% AND >10ms improvement, or active disappeared from the
//      list), enter Migrating.
//   3. Migrating → spawn a new session in parallel; on its register-OK,
//      publish the new relay as `pending`, emit `switched`, start the drain
//      timer; on drain expiry abort the old session and promote pending.
//
// All session lifecycle (`connected` / `disconnected`) is emitted by the
// underlying `relay_client::run_session`. The selector emits the
// `switched` lifecycle event on a successful migration.
//
// A `SelectorEvent` broadcast (under `cfg(any(test, feature = "test-hooks"))`)
// lets Phase-3 tests assert on transitions instead of sleeping.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tokio::sync::{mpsc, oneshot, watch};
use tokio::task::JoinHandle;
use tokio::time::{sleep_until, Instant as TokioInstant};
use tracing::{debug, info, warn};

use crate::authorization::{AuthzGate, AuthzSnapshot};
use crate::proto::connector_control_message;
use crate::proto::{
    ConnectorControlMessage, ConnectorRelayState, LabelledRelayInfo, LabelledRelayList,
    RelayCapacityLabel,
};
use crate::relay_attachment::RelayAttachmentSlot;
use crate::relay_client;
use crate::relay_handler::{RelayDrainTracker, RelayHandler};
use crate::relay_probe::{probe_relays, RelayProbeResult};
use crate::relay_ranking::{now_unix_seconds, RankedEntry, RelayRanking};
use crate::tls::cert_holder::CertHolder;

const MIGRATION_IMPROVEMENT_RATIO: f64 = 0.15;
const MIGRATION_IMPROVEMENT_MIN_MS: u64 = 10;
const TOP_RANKING_ENTRIES: usize = 5;
const FAILOVER_PER_ENTRY_BUDGET: Duration = Duration::from_secs(5);

#[derive(Clone)]
pub struct RelaySelectorConfig {
    pub state_dir: PathBuf,
    pub connector_id: String,
    pub connector_spiffe_id: String,
    /// Single owner of the Connector certificate (Sprint 20 G-2a). Every probe
    /// and session dial reads the CURRENT identity from here, so dials after
    /// a renewal present the renewed certificate.
    pub certs: Arc<CertHolder>,
    pub relay_crl_manager: crate::crl::CrlManager,
    pub max_incoming_bidi_streams: u32,
    pub idle_timeout: Duration,
    pub reprobe_interval: Duration,
    pub max_concurrent_probes: usize,
    pub probe_timeout: Duration,
    pub reconnect_base: Duration,
    pub reconnect_max: Duration,
    pub reconnect_backoff_factor: f64,
    pub drain_timeout: Duration,
}

/// Connector mTLS identity used for Relay dials and probes.
pub struct RelayIdentity {
    pub cert_pem: Vec<u8>,
    pub key_pem: Vec<u8>,
    pub workspace_ca_pem: Vec<u8>,
    pub intermediate_ca_pem: Vec<u8>,
}

impl RelaySelectorConfig {
    /// The CURRENT identity from the CertHolder. `workspace_ca.crt` holds the
    /// Workspace CA + Platform Intermediate bundle; relay_client picks the
    /// right anchor from it for each role.
    pub fn current_identity(&self) -> RelayIdentity {
        let material = self.certs.current();
        RelayIdentity {
            cert_pem: material.store.cert_pem.clone(),
            key_pem: material.store.key_pem.clone(),
            workspace_ca_pem: material.store.workspace_ca_pem.clone(),
            intermediate_ca_pem: material.store.workspace_ca_pem.clone(),
        }
    }
}

/// In-flight relay session: which relay we're attached to, the spawned
/// `run_session` task, and the most recent probe score for migration
/// comparisons.
struct ActiveRelay {
    info: LabelledRelayInfo,
    handle: JoinHandle<anyhow::Result<()>>,
    drain_tracker: RelayDrainTracker,
    last_score: Option<u64>,
}

enum State {
    /// `next_backoff` is the delay to use if this bootstrap attempt fails and
    /// we fall into `Backoff`. It grows across consecutive failed cycles and
    /// resets to `reconnect_base` after a healthy connection (see `failover`).
    Disconnected {
        next_backoff: Duration,
    },
    Connected(ActiveRelay),
    Backoff {
        delay: Duration,
    },
}

/// Compute the next exponential-backoff delay: `current * factor`, capped at
/// `max`, with a 1s floor so a misconfigured zero base can't busy-loop.
fn next_backoff_delay(current: Duration, factor: f64, max: Duration) -> Duration {
    let scaled = current.mul_f64(factor.max(1.0));
    scaled.min(max).max(Duration::from_secs(1))
}

/// Public entry. Runs forever; spawn from main.
pub async fn run(
    cfg: RelaySelectorConfig,
    relay_handler: Arc<RelayHandler>,
    attachment_slot: RelayAttachmentSlot,
    relay_list_rx: watch::Receiver<Option<LabelledRelayList>>,
    ctrl_tx: mpsc::Sender<ConnectorControlMessage>,
    gate: AuthzGate,
) -> ! {
    run_until_terminal(
        cfg,
        relay_handler,
        attachment_slot,
        relay_list_rx,
        ctrl_tx,
        gate,
    )
    .await;
    // Terminal is never left within the process: nothing to resume.
    std::future::pending().await
}

/// The selector loop. Returns only once the connector is terminal (revoked
/// or unknown to the controller), after stopping its relay session (Sprint 20
/// Fix 13, F38): every relay refuses a revoked connector, so dialling on
/// would only repeat refused registrations.
async fn run_until_terminal(
    cfg: RelaySelectorConfig,
    relay_handler: Arc<RelayHandler>,
    attachment_slot: RelayAttachmentSlot,
    mut relay_list_rx: watch::Receiver<Option<LabelledRelayList>>,
    ctrl_tx: mpsc::Sender<ConnectorControlMessage>,
    gate: AuthzGate,
) {
    info!("Relay selector starting");
    let mut gate_rx = gate.subscribe();
    let mut state = State::Disconnected {
        next_backoff: cfg.reconnect_base,
    };
    let mut current_list = relay_list_rx.borrow_and_update().clone();

    loop {
        if gate_rx.borrow().state().is_terminal() {
            stop_on_terminal(state, &attachment_slot).await;
            return;
        }

        // Block on first list arrival if we haven't seen one yet.
        if current_list.is_none() {
            info!("Relay selector waiting for first LabelledRelayList push");
            tokio::select! {
                _ = relay_list_rx.changed() => {}
                _ = wait_terminal(&mut gate_rx) => continue,
            }
            current_list = relay_list_rx.borrow_and_update().clone();
            continue;
        }
        let list = current_list.clone().expect("checked just above");

        state = match state {
            State::Disconnected { next_backoff } => {
                bootstrap(
                    &cfg,
                    &list,
                    &relay_handler,
                    &attachment_slot,
                    &ctrl_tx,
                    next_backoff,
                )
                .await
            }
            State::Connected(active) => {
                connected_step(
                    &cfg,
                    &list,
                    active,
                    &relay_handler,
                    &attachment_slot,
                    &ctrl_tx,
                    &mut relay_list_rx,
                    &mut current_list,
                    &mut gate_rx,
                )
                .await
            }
            State::Backoff { delay } => {
                emit_event(SelectorEvent::EnteredBackoff {
                    delay_ms: delay.as_millis() as u64,
                });
                match wait_backoff(delay, &mut relay_list_rx, &mut current_list, &mut gate_rx).await
                {
                    BackoffWake::ListChanged => {
                        info!("New LabelledRelayList during backoff; retrying now")
                    }
                    // The loop top stops the selector.
                    BackoffWake::Terminal => continue,
                    BackoffWake::Elapsed => {}
                }
                // The backoff keeps growing either way, so a controller that
                // keeps pushing lists of unreachable relays can't make the
                // selector dial in a tight loop.
                let next_backoff =
                    next_backoff_delay(delay, cfg.reconnect_backoff_factor, cfg.reconnect_max);
                State::Disconnected { next_backoff }
            }
        };
    }
}

#[derive(Debug, PartialEq, Eq)]
enum BackoffWake {
    Elapsed,
    ListChanged,
    /// The connector became terminal (F38).
    Terminal,
}

/// Resolves once the published serving state is terminal; never otherwise
/// (also not when the tracker is gone). Cancel-safe.
async fn wait_terminal(gate_rx: &mut watch::Receiver<AuthzSnapshot>) {
    loop {
        if gate_rx.borrow_and_update().state().is_terminal() {
            return;
        }
        if gate_rx.changed().await.is_err() {
            std::future::pending::<()>().await;
        }
    }
}

/// F38: end the active relay session (aborting a `JoinHandle` stops the task;
/// dropping it would only detach it) and stop reporting a relay attachment.
async fn stop_on_terminal(state: State, attachment_slot: &RelayAttachmentSlot) {
    if let State::Connected(active) = state {
        active.handle.abort();
        info!(relay_id = %active.info.relay_id, "relay session ended: connector is terminal");
    }
    attachment_slot.set_active(None).await;
    warn!("relay selector stopped: connector is revoked or unknown to the controller");
    emit_event(SelectorEvent::StoppedTerminal);
}

/// Sleep out a backoff `delay`, ending early when the controller pushes a new
/// LabelledRelayList. The new list replaces `current_list`, so the next
/// bootstrap dials it. Without this, a list pushed while the selector is
/// disconnected (e.g. the first relay coming up after an empty list) was never
/// read: `current_list` only refreshed inside `connected_step`, so the
/// selector retried the stale list until the process restarted.
async fn wait_backoff(
    delay: Duration,
    relay_list_rx: &mut watch::Receiver<Option<LabelledRelayList>>,
    current_list: &mut Option<LabelledRelayList>,
    gate_rx: &mut watch::Receiver<AuthzSnapshot>,
) -> BackoffWake {
    tokio::select! {
        _ = tokio::time::sleep(delay) => BackoffWake::Elapsed,
        _ = wait_terminal(gate_rx) => BackoffWake::Terminal,
        changed = relay_list_rx.changed() => {
            if changed.is_err() {
                // Sender gone (shutdown): no list will ever arrive; finish the delay.
                tokio::time::sleep(delay).await;
                return BackoffWake::Elapsed;
            }
            if let Some(list) = relay_list_rx.borrow_and_update().clone() {
                *current_list = Some(list);
            }
            BackoffWake::ListChanged
        }
    }
}

async fn bootstrap(
    cfg: &RelaySelectorConfig,
    list: &LabelledRelayList,
    relay_handler: &Arc<RelayHandler>,
    attachment_slot: &RelayAttachmentSlot,
    ctrl_tx: &mpsc::Sender<ConnectorControlMessage>,
    next_backoff: Duration,
) -> State {
    if list.relays.is_empty() {
        warn!("LabelledRelayList is empty; entering backoff");
        return State::Backoff {
            delay: next_backoff,
        };
    }

    // Warm path: persisted ranking, fresh, version matches.
    let warm_candidate = RelayRanking::load(&cfg.state_dir).and_then(|ranking| {
        if !ranking.is_fresh() || !ranking.version_matches(list.version) {
            return None;
        }
        ranking
            .valid_entries(list)
            .into_iter()
            .next()
            .and_then(|entry| {
                list.relays
                    .iter()
                    .find(|r| r.relay_id == entry.relay_id)
                    .cloned()
            })
            .map(|info| (info, ranking))
    });

    if let Some((info, _ranking)) = warm_candidate {
        info!(relay_id = %info.relay_id, "Selector picked ranked relay for warm start");
        if let Some(active) =
            connect(cfg, info, relay_handler, attachment_slot, ctrl_tx, None).await
        {
            emit_event(SelectorEvent::EnteredConnected {
                relay_id: active.info.relay_id.clone(),
            });
            return State::Connected(active);
        }
        // Warm-start failed — fall through to random picks.
        debug!("Warm-start connect failed; falling back to random Tier-1/Tier-2");
    }

    for info in random_pick_order(list) {
        if let Some(active) =
            connect(cfg, info, relay_handler, attachment_slot, ctrl_tx, None).await
        {
            emit_event(SelectorEvent::EnteredConnected {
                relay_id: active.info.relay_id.clone(),
            });
            return State::Connected(active);
        }
    }

    warn!("All relays in current list failed to connect; entering backoff");
    State::Backoff {
        delay: next_backoff,
    }
}

#[allow(clippy::too_many_arguments)]
async fn connected_step(
    cfg: &RelaySelectorConfig,
    list: &LabelledRelayList,
    mut active: ActiveRelay,
    relay_handler: &Arc<RelayHandler>,
    attachment_slot: &RelayAttachmentSlot,
    ctrl_tx: &mpsc::Sender<ConnectorControlMessage>,
    relay_list_rx: &mut watch::Receiver<Option<LabelledRelayList>>,
    current_list: &mut Option<LabelledRelayList>,
    gate_rx: &mut watch::Receiver<AuthzSnapshot>,
) -> State {
    let reprobe_at = TokioInstant::now() + cfg.reprobe_interval;

    tokio::select! {
        biased;

        // Active session ended (connection drop / error).
        result = &mut active.handle => {
            match result {
                Ok(Ok(())) => info!(relay_id = %active.info.relay_id, "Active relay session closed cleanly; failing over"),
                Ok(Err(e)) => warn!(relay_id = %active.info.relay_id, error = %e, "Active relay session failed; failing over"),
                Err(e) => warn!(relay_id = %active.info.relay_id, error = %e, "Active relay session task crashed; failing over"),
            }
            failover(cfg, list, relay_handler, attachment_slot, ctrl_tx).await
        }

        // Terminal (F38): back to the loop top, which stops the session.
        _ = wait_terminal(gate_rx) => State::Connected(active),

        // New LabelledRelayList push.
        changed = relay_list_rx.changed() => {
            if changed.is_err() {
                warn!("relay_list channel closed; selector waiting indefinitely");
                State::Connected(active)
            } else {
                *current_list = relay_list_rx.borrow_and_update().clone();
                handle_list_change(cfg, current_list.as_ref().unwrap_or(list), active, relay_handler, attachment_slot, ctrl_tx).await
            }
        }

        // Background re-probe.
        _ = sleep_until(reprobe_at) => {
            reprobe_and_maybe_migrate(cfg, list, active, relay_handler, attachment_slot, ctrl_tx).await
        }
    }
}

async fn handle_list_change(
    cfg: &RelaySelectorConfig,
    list: &LabelledRelayList,
    active: ActiveRelay,
    relay_handler: &Arc<RelayHandler>,
    attachment_slot: &RelayAttachmentSlot,
    ctrl_tx: &mpsc::Sender<ConnectorControlMessage>,
) -> State {
    let still_present = list
        .relays
        .iter()
        .any(|r| r.relay_id == active.info.relay_id);
    if !still_present {
        info!(
            relay_id = %active.info.relay_id,
            "Active relay absent from new list; migrating immediately"
        );
        return migrate(
            cfg,
            list,
            active,
            relay_handler,
            attachment_slot,
            ctrl_tx,
            None,
        )
        .await;
    }
    // Same relay still listed: do a fresh probe sweep right away.
    reprobe_and_maybe_migrate(cfg, list, active, relay_handler, attachment_slot, ctrl_tx).await
}

async fn reprobe_and_maybe_migrate(
    cfg: &RelaySelectorConfig,
    list: &LabelledRelayList,
    mut active: ActiveRelay,
    relay_handler: &Arc<RelayHandler>,
    attachment_slot: &RelayAttachmentSlot,
    ctrl_tx: &mpsc::Sender<ConnectorControlMessage>,
) -> State {
    let identity = cfg.current_identity();
    let results = probe_relays(
        &list.relays,
        &cfg.connector_id,
        &identity.cert_pem,
        &identity.key_pem,
        &identity.workspace_ca_pem,
        &identity.intermediate_ca_pem,
        cfg.relay_crl_manager.clone(),
        cfg.max_concurrent_probes,
        cfg.probe_timeout,
    )
    .await;

    let entries = persist_ranking(&cfg.state_dir, list.version, &results);
    emit_event(SelectorEvent::ProbeSweepCompleted {
        results: results.clone(),
    });
    emit_event(SelectorEvent::RankingPersisted { entries });

    // Latch the active relay's current score for the next migration check.
    if let Some(r) = results.iter().find(|r| r.relay_id == active.info.relay_id) {
        active.last_score = Some(r.score);
    }

    let best = results
        .iter()
        .filter(|r| r.relay_id != active.info.relay_id)
        .min_by_key(|r| r.score);

    let active_score = active.last_score.unwrap_or(u64::MAX);
    if let Some(best) = best {
        if is_meaningful_improvement(active_score, best.score) {
            if let Some(info) = list
                .relays
                .iter()
                .find(|r| r.relay_id == best.relay_id)
                .cloned()
            {
                info!(
                    from = %active.info.relay_id,
                    to = %info.relay_id,
                    active_score,
                    best_score = best.score,
                    "Migrating to better-scoring relay"
                );
                return migrate(
                    cfg,
                    list,
                    active,
                    relay_handler,
                    attachment_slot,
                    ctrl_tx,
                    Some(info),
                )
                .await;
            }
        }
    }

    State::Connected(active)
}

fn is_meaningful_improvement(current_score: u64, best_score: u64) -> bool {
    if best_score >= current_score {
        return false;
    }
    let delta = current_score.saturating_sub(best_score);
    if delta < MIGRATION_IMPROVEMENT_MIN_MS {
        return false;
    }
    let ratio_threshold = (current_score as f64 * MIGRATION_IMPROVEMENT_RATIO).ceil() as u64;
    delta >= ratio_threshold.max(MIGRATION_IMPROVEMENT_MIN_MS)
}

/// Order migration targets by preference, de-duplicated and with the relay
/// we're leaving excluded: the explicit probe winner first, then valid
/// persisted-ranking entries (best RTT first), then any remaining listed
/// relays. `valid_entries` preserves rank order, and the ranking file is
/// persisted score-sorted, so ranking entries arrive best-first.
fn build_migration_candidates(
    state_dir: &std::path::Path,
    list: &LabelledRelayList,
    old_relay_id: &str,
    preferred: Option<LabelledRelayInfo>,
) -> Vec<LabelledRelayInfo> {
    let in_list = |id: &str| list.relays.iter().find(|r| r.relay_id == id).cloned();

    // Assemble in priority order, then de-dup preserving first occurrence.
    let mut prioritized: Vec<LabelledRelayInfo> = Vec::new();
    if let Some(p) = preferred.and_then(|p| in_list(&p.relay_id)) {
        prioritized.push(p);
    }
    if let Some(ranking) = RelayRanking::load(state_dir) {
        for entry in ranking.valid_entries(list) {
            if let Some(info) = in_list(&entry.relay_id) {
                prioritized.push(info);
            }
        }
    }
    prioritized.extend(list.relays.iter().cloned());

    let mut seen: std::collections::HashSet<String> = std::collections::HashSet::new();
    prioritized
        .into_iter()
        .filter(|info| info.relay_id != old_relay_id && seen.insert(info.relay_id.clone()))
        .collect()
}

/// Waits up to `drain_timeout` for the streams on a migrated-away relay to
/// end by themselves. Those still open at the timeout are cut: the old relay
/// connection is closed (FU-14). Before, only the session task was aborted and
/// detached stream tasks kept the connection, and the drained relay, in use
/// until they finished. Returns true when the drain completed in time.
async fn drain_old_relay(
    tracker: &RelayDrainTracker,
    drain_timeout: Duration,
    relay_id: &str,
) -> bool {
    if tokio::time::timeout(drain_timeout, tracker.wait_for_idle())
        .await
        .is_ok()
    {
        info!(relay_id = %relay_id, "old relay drain completed after active streams reached zero");
        return true;
    }
    emit_event(SelectorEvent::DrainTimeoutFired);
    warn!(
        relay_id = %relay_id,
        active_streams = tracker.active_streams(),
        "old relay drain timed out; cutting the streams still open"
    );
    tracker.cut();
    false
}

#[allow(clippy::too_many_arguments)]
async fn migrate(
    cfg: &RelaySelectorConfig,
    list: &LabelledRelayList,
    old: ActiveRelay,
    relay_handler: &Arc<RelayHandler>,
    attachment_slot: &RelayAttachmentSlot,
    ctrl_tx: &mpsc::Sender<ConnectorControlMessage>,
    preferred: Option<LabelledRelayInfo>,
) -> State {
    // Order targets so we migrate to the relay the probe sweep actually chose:
    //   1. the explicit `preferred` winner (if still listed),
    //   2. valid persisted-ranking entries, best RTT first,
    //   3. any remaining listed relays.
    // Without this, migration would connect to whatever is first in the
    // controller's list, discarding the probe result entirely.
    let candidates =
        build_migration_candidates(&cfg.state_dir, list, &old.info.relay_id, preferred);

    for target in candidates {
        let (tx, rx) = oneshot::channel();
        let (new_handle, new_tracker) = spawn_session(
            cfg,
            target.clone(),
            relay_handler.clone(),
            attachment_slot.clone(),
            ctrl_tx.clone(),
            Some(tx),
        );

        // Wait for register-OK on the new connection, or for the new session
        // to die before that happens.
        match rx.await {
            Ok(()) => {
                emit_event(SelectorEvent::MigrationStarted {
                    from: old.info.relay_id.clone(),
                    to: target.relay_id.clone(),
                });
                // `run_session` already flipped the shared `active` attachment
                // to the new relay on register-OK, so new client streams route
                // there immediately. Report the switch to the controller, then
                // drain the old connection independently below.
                let _ = ctrl_tx
                    .send(ConnectorControlMessage {
                        body: Some(connector_control_message::Body::RelayState(
                            ConnectorRelayState {
                                connector_id: cfg.connector_id.clone(),
                                relay_id: target.relay_id.clone(),
                                relay_spiffe_id: target.spiffe_id.clone(),
                                observed_at_unix: now_unix_seconds(),
                                reason: "switched".to_string(),
                            },
                        )),
                    })
                    .await;

                drain_old_relay(&old.drain_tracker, cfg.drain_timeout, &old.info.relay_id).await;

                old.handle.abort();

                emit_event(SelectorEvent::MigrationCompleted);
                emit_event(SelectorEvent::EnteredConnected {
                    relay_id: target.relay_id.clone(),
                });

                return State::Connected(ActiveRelay {
                    info: target,
                    handle: new_handle,
                    drain_tracker: new_tracker,
                    last_score: None,
                });
            }
            Err(_) => {
                warn!(target = %target.relay_id, "Migration target failed to register; trying next");
                // The session task's been dropped or the sender was dropped
                // before signalling; either way the new task is unusable.
                new_handle.abort();
                continue;
            }
        }
    }

    warn!("Migration could not find a usable target; remaining on current relay");
    State::Connected(old)
}

async fn failover(
    cfg: &RelaySelectorConfig,
    list: &LabelledRelayList,
    relay_handler: &Arc<RelayHandler>,
    attachment_slot: &RelayAttachmentSlot,
    ctrl_tx: &mpsc::Sender<ConnectorControlMessage>,
) -> State {
    emit_event(SelectorEvent::EnteredFailover);

    // Try ranking candidates first (the warm-path entries).
    let ranking = RelayRanking::load(&cfg.state_dir);
    let ranked_targets: Vec<LabelledRelayInfo> = ranking
        .as_ref()
        .map(|r| {
            r.valid_entries(list)
                .into_iter()
                .filter_map(|entry| {
                    list.relays
                        .iter()
                        .find(|i| i.relay_id == entry.relay_id)
                        .cloned()
                })
                .collect()
        })
        .unwrap_or_default();

    for target in ranked_targets {
        if let Some(active) = tokio::time::timeout(
            FAILOVER_PER_ENTRY_BUDGET,
            connect(
                cfg,
                target.clone(),
                relay_handler,
                attachment_slot,
                ctrl_tx,
                None,
            ),
        )
        .await
        .ok()
        .flatten()
        {
            emit_event(SelectorEvent::EnteredConnected {
                relay_id: active.info.relay_id.clone(),
            });
            return State::Connected(active);
        }
    }

    // No ranked entry worked — re-probe and use the best result.
    let identity = cfg.current_identity();
    let results = probe_relays(
        &list.relays,
        &cfg.connector_id,
        &identity.cert_pem,
        &identity.key_pem,
        &identity.workspace_ca_pem,
        &identity.intermediate_ca_pem,
        cfg.relay_crl_manager.clone(),
        cfg.max_concurrent_probes,
        cfg.probe_timeout,
    )
    .await;

    let mut ordered = results.clone();
    ordered.sort_by_key(|r| r.score);
    for result in ordered {
        if let Some(info) = list
            .relays
            .iter()
            .find(|r| r.relay_id == result.relay_id)
            .cloned()
        {
            if let Some(active) =
                connect(cfg, info, relay_handler, attachment_slot, ctrl_tx, None).await
            {
                emit_event(SelectorEvent::EnteredConnected {
                    relay_id: active.info.relay_id.clone(),
                });
                return State::Connected(active);
            }
        }
    }

    State::Backoff {
        delay: cfg.reconnect_base,
    }
}

/// Spawn a `run_session` task, await register-OK via the oneshot, and on
/// success build the `ActiveRelay`. On failure (oneshot dropped or session
/// exits before registering) returns None and aborts the task.
async fn connect(
    cfg: &RelaySelectorConfig,
    info: LabelledRelayInfo,
    relay_handler: &Arc<RelayHandler>,
    attachment_slot: &RelayAttachmentSlot,
    ctrl_tx: &mpsc::Sender<ConnectorControlMessage>,
    on_registered: Option<oneshot::Sender<()>>,
) -> Option<ActiveRelay> {
    let (tx, rx) = on_registered.map(|t| (Some(t), None)).unwrap_or_else(|| {
        let (s, r) = oneshot::channel();
        (Some(s), Some(r))
    });
    let (handle, drain_tracker) = spawn_session(
        cfg,
        info.clone(),
        relay_handler.clone(),
        attachment_slot.clone(),
        ctrl_tx.clone(),
        tx,
    );

    if let Some(rx) = rx {
        // Caller did not supply their own oneshot — wait here for register-OK.
        match rx.await {
            Ok(()) => Some(ActiveRelay {
                info,
                handle,
                drain_tracker,
                last_score: None,
            }),
            Err(_) => {
                handle.abort();
                None
            }
        }
    } else {
        // Caller is awaiting the oneshot themselves; return without waiting.
        // (Currently no internal caller takes this branch — keeping the
        // option open for future use.)
        Some(ActiveRelay {
            info,
            handle,
            drain_tracker,
            last_score: None,
        })
    }
}

fn spawn_session(
    cfg: &RelaySelectorConfig,
    info: LabelledRelayInfo,
    relay_handler: Arc<RelayHandler>,
    attachment_slot: RelayAttachmentSlot,
    ctrl_tx: mpsc::Sender<ConnectorControlMessage>,
    on_registered: Option<oneshot::Sender<()>>,
) -> (JoinHandle<anyhow::Result<()>>, RelayDrainTracker) {
    let cfg_owned = cfg.clone();
    let relay_addr = info.relay_addr;
    let relay_spiffe_id = info.spiffe_id;
    let connector_id = cfg_owned.connector_id.clone();
    let connector_spiffe_id = cfg_owned.connector_spiffe_id.clone();
    // Identity read at dial time: a session started after a renewal presents
    // the renewed certificate; an established session keeps the one it used.
    let RelayIdentity {
        cert_pem,
        key_pem,
        workspace_ca_pem,
        intermediate_ca_pem,
    } = cfg_owned.current_identity();
    let relay_crl_manager = cfg_owned.relay_crl_manager.clone();
    let max_streams = cfg_owned.max_incoming_bidi_streams;
    let idle_timeout = cfg_owned.idle_timeout;
    let drain_tracker = RelayDrainTracker::new();
    let session_drain_tracker = drain_tracker.clone();
    let handle = tokio::spawn(async move {
        relay_client::run_session(
            &relay_addr,
            &relay_spiffe_id,
            &connector_id,
            &connector_spiffe_id,
            &cert_pem,
            &key_pem,
            &workspace_ca_pem,
            &intermediate_ca_pem,
            relay_crl_manager,
            relay_handler,
            max_streams,
            idle_timeout,
            attachment_slot,
            ctrl_tx,
            on_registered,
            session_drain_tracker,
        )
        .await
    });
    (handle, drain_tracker)
}

fn persist_ranking(
    state_dir: &std::path::Path,
    list_version: u64,
    results: &[RelayProbeResult],
) -> usize {
    let mut sorted: Vec<&RelayProbeResult> = results.iter().collect();
    sorted.sort_by_key(|r| r.score);
    let entries: Vec<RankedEntry> = sorted
        .into_iter()
        .take(TOP_RANKING_ENTRIES)
        .enumerate()
        .map(|(rank, r)| RankedEntry {
            rank,
            relay_id: r.relay_id.clone(),
            relay_addr: r.relay_addr.clone(),
            spiffe_id: r.spiffe_id.clone(),
            score: r.score,
            rtt_ms: r.rtt_ms,
        })
        .collect();

    let count = entries.len();
    let ranking = RelayRanking {
        list_version,
        probed_at_unix: now_unix_seconds(),
        entries,
    };
    if let Err(e) = ranking.save(state_dir) {
        warn!(error = %e, "Failed to persist relay ranking");
    }
    count
}

fn random_pick_order(list: &LabelledRelayList) -> Vec<LabelledRelayInfo> {
    // Pseudo-random rotation seeded by nanos — good enough to spread load
    // across N connectors without pulling the `rand` crate. Tier-1 relays
    // come first; Tier-2 next; identical-tier relays are rotated.
    let tier1: Vec<&LabelledRelayInfo> = list
        .relays
        .iter()
        .filter(|r| r.label == RelayCapacityLabel::RelayCapacityHigh as i32)
        .collect();
    let tier2: Vec<&LabelledRelayInfo> = list
        .relays
        .iter()
        .filter(|r| r.label == RelayCapacityLabel::RelayCapacityMedium as i32)
        .collect();

    let nanos = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_nanos())
        .unwrap_or(0) as usize;

    let mut rotated_tier1 = rotate(tier1, nanos);
    let rotated_tier2 = rotate(tier2, nanos);

    if !rotated_tier2.is_empty() && rotated_tier1.is_empty() {
        warn!("no Tier-1 relays available; falling back to Tier-2 — this is a degraded mode");
    }
    rotated_tier1.extend(rotated_tier2);
    rotated_tier1
}

fn rotate<T: Clone>(items: Vec<&T>, offset: usize) -> Vec<T> {
    if items.is_empty() {
        return Vec::new();
    }
    let n = items.len();
    let split = offset % n;
    let mut out: Vec<T> = items[split..].iter().map(|&t| t.clone()).collect();
    out.extend(items[..split].iter().map(|&t| t.clone()));
    out
}

// --------------------------------------------------------------------------
// Test-observability hook. Production builds (test-hooks off) compile this
// out entirely.
// --------------------------------------------------------------------------

#[derive(Clone, Debug)]
pub enum SelectorEvent {
    EnteredConnected {
        relay_id: String,
    },
    EnteredFailover,
    EnteredBackoff {
        delay_ms: u64,
    },
    ProbeSweepCompleted {
        results: Vec<RelayProbeResult>,
    },
    RankingPersisted {
        entries: usize,
    },
    MigrationStarted {
        from: String,
        to: String,
    },
    MigrationCompleted,
    DrainTimeoutFired,
    /// F38: the connector became terminal; the selector stopped for good.
    StoppedTerminal,
}

#[cfg(any(test, feature = "test-hooks"))]
mod observability {
    use super::SelectorEvent;
    use std::sync::OnceLock;
    use tokio::sync::broadcast;

    static EVENTS: OnceLock<broadcast::Sender<SelectorEvent>> = OnceLock::new();

    fn sender() -> &'static broadcast::Sender<SelectorEvent> {
        EVENTS.get_or_init(|| broadcast::channel(256).0)
    }

    pub fn subscribe() -> broadcast::Receiver<SelectorEvent> {
        sender().subscribe()
    }

    pub fn send(event: SelectorEvent) {
        let _ = sender().send(event);
    }
}

#[cfg(any(test, feature = "test-hooks"))]
pub fn subscribe_selector_events() -> tokio::sync::broadcast::Receiver<SelectorEvent> {
    observability::subscribe()
}

#[inline]
fn emit_event(event: SelectorEvent) {
    #[cfg(any(test, feature = "test-hooks"))]
    observability::send(event);
    #[cfg(not(any(test, feature = "test-hooks")))]
    let _ = event;
}

// --------------------------------------------------------------------------
// Unit tests — pure-function checks. State-machine integration tests live in
// connector/tests/ under the Phase 3 harness.
// --------------------------------------------------------------------------

#[cfg(test)]
mod tests {
    use super::*;
    use crate::proto::{LabelledRelayInfo, RelayCapacityLabel};

    /// FU-14: streams still open when the drain timeout fires are cut: the
    /// old relay connection is closed, so they end at ~drain_timeout instead
    /// of running on until they finish by themselves.
    #[tokio::test]
    async fn drain_timeout_cuts_the_streams_still_open() {
        let (connector_side, relay_side) = crate::test_support::plain_quic_pair().await;
        let tracker = RelayDrainTracker::new();
        tracker.attach_connection(connector_side);
        let _long_download = tracker.track_stream();

        let drained = drain_old_relay(&tracker, Duration::from_millis(100), "old-relay").await;

        assert!(!drained, "a stream was still open at the timeout");
        let reason = tokio::time::timeout(Duration::from_secs(2), relay_side.closed())
            .await
            .expect("the old relay connection must be closed at the timeout");
        assert!(
            matches!(&reason, quinn::ConnectionError::ApplicationClosed(c)
                if c.error_code == quinn::VarInt::from_u32(crate::relay_handler::DRAIN_TIMEOUT_CODE)),
            "relay saw {reason:?}"
        );
    }

    /// A drain that completes in time closes nothing early.
    #[tokio::test]
    async fn a_completed_drain_does_not_cut() {
        let (connector_side, relay_side) = crate::test_support::plain_quic_pair().await;
        let tracker = RelayDrainTracker::new();
        tracker.attach_connection(connector_side);
        let stream = tracker.track_stream();
        let t = tracker.clone();
        let drain =
            tokio::spawn(
                async move { drain_old_relay(&t, Duration::from_secs(5), "old-relay").await },
            );
        tokio::time::sleep(Duration::from_millis(50)).await;
        drop(stream); // the last stream ends on its own
        assert!(drain.await.unwrap(), "the drain completed");
        tokio::time::sleep(Duration::from_millis(50)).await;
        assert!(
            relay_side.close_reason().is_none(),
            "nothing may be cut after a completed drain"
        );
    }

    fn info(id: &str, tier: RelayCapacityLabel) -> LabelledRelayInfo {
        LabelledRelayInfo {
            relay_id: id.to_string(),
            relay_addr: format!("{id}.example.com:9093"),
            spiffe_id: format!("spiffe://zecurity.in/relay/{id}"),
            label: tier as i32,
        }
    }

    #[test]
    fn improvement_below_floor_rejected() {
        // 100 vs 95 — 5ms delta, below 10ms floor.
        assert!(!is_meaningful_improvement(100, 95));
    }

    #[test]
    fn improvement_below_ratio_rejected() {
        // 100 vs 86 — 14ms delta, above floor but ratio is 14/100 = 14% < 15%.
        assert!(!is_meaningful_improvement(100, 86));
    }

    #[test]
    fn improvement_above_both_thresholds_accepted() {
        // 100 vs 80 — 20ms delta, 20% ratio.
        assert!(is_meaningful_improvement(100, 80));
    }

    #[test]
    fn improvement_phase_doc_example_accepted() {
        // Phase doc: 30 → 10, 67% improvement, 20ms absolute.
        assert!(is_meaningful_improvement(30, 10));
    }

    #[test]
    fn improvement_equal_or_worse_rejected() {
        assert!(!is_meaningful_improvement(100, 100));
        assert!(!is_meaningful_improvement(100, 110));
    }

    #[test]
    fn improvement_low_baseline_uses_floor_only() {
        // Active score 5ms: 15% of 5 = 0.75, ceil = 1, but floor is 10.
        // Best 0ms is 5ms delta — under floor, reject.
        assert!(!is_meaningful_improvement(5, 0));
        // Best worse than 5+10 doesn't exist (best=0 is the lowest possible);
        // confirm the floor really blocks small-baseline noise.
    }

    #[test]
    fn random_pick_order_tier1_before_tier2() {
        let list = LabelledRelayList {
            relays: vec![
                info("t2-a", RelayCapacityLabel::RelayCapacityMedium),
                info("t1-a", RelayCapacityLabel::RelayCapacityHigh),
                info("t2-b", RelayCapacityLabel::RelayCapacityMedium),
                info("t1-b", RelayCapacityLabel::RelayCapacityHigh),
            ],
            version: 1,
        };
        let order = random_pick_order(&list);
        assert_eq!(order.len(), 4);
        // First two are Tier-1, in some rotation.
        assert!(order[0].relay_id.starts_with("t1-"));
        assert!(order[1].relay_id.starts_with("t1-"));
        // Last two are Tier-2.
        assert!(order[2].relay_id.starts_with("t2-"));
        assert!(order[3].relay_id.starts_with("t2-"));
    }

    #[test]
    fn random_pick_order_tier2_only_when_no_tier1() {
        let list = LabelledRelayList {
            relays: vec![info("t2-a", RelayCapacityLabel::RelayCapacityMedium)],
            version: 1,
        };
        let order = random_pick_order(&list);
        assert_eq!(order.len(), 1);
        assert_eq!(order[0].relay_id, "t2-a");
    }

    // ---- Disconnected selector re-reads pushed relay lists (B2 finding 3) ----

    /// A gate that stays Serving (its sender is gone: never terminal).
    fn open_gate() -> watch::Receiver<AuthzSnapshot> {
        AuthzGate::open_for_tests().subscribe()
    }

    fn one_relay_list(version: u64) -> LabelledRelayList {
        LabelledRelayList {
            relays: vec![info("r1", RelayCapacityLabel::RelayCapacityHigh)],
            version,
        }
    }

    #[tokio::test(start_paused = true)]
    async fn backoff_ends_early_and_adopts_a_pushed_list() {
        let (tx, mut rx) = watch::channel(Some(LabelledRelayList {
            relays: vec![],
            version: 1,
        }));
        rx.borrow_and_update();
        let mut current = rx.borrow().clone();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_secs(1)).await;
            tx.send(Some(one_relay_list(2))).unwrap();
            tokio::time::sleep(Duration::from_secs(600)).await;
        });

        let start = TokioInstant::now();
        let wake = wait_backoff(
            Duration::from_secs(120),
            &mut rx,
            &mut current,
            &mut open_gate(),
        )
        .await;

        assert_eq!(wake, BackoffWake::ListChanged);
        assert_eq!(
            start.elapsed(),
            Duration::from_secs(1),
            "woke at the push, not after 120s"
        );
        let list = current.expect("list adopted");
        assert_eq!((list.version, list.relays.len()), (2, 1));
    }

    #[tokio::test(start_paused = true)]
    async fn list_pushed_before_backoff_is_seen_immediately() {
        // A push that landed while bootstrap was dialing must not wait a full delay.
        let (tx, mut rx) = watch::channel(None);
        let mut current = None;
        tx.send(Some(one_relay_list(7))).unwrap();

        let start = TokioInstant::now();
        let wake = wait_backoff(
            Duration::from_secs(60),
            &mut rx,
            &mut current,
            &mut open_gate(),
        )
        .await;

        assert_eq!(wake, BackoffWake::ListChanged);
        assert_eq!(start.elapsed(), Duration::ZERO);
        assert_eq!(current.map(|l| l.version), Some(7));
    }

    #[tokio::test(start_paused = true)]
    async fn backoff_without_push_runs_full_delay_and_keeps_list() {
        let (_tx, mut rx) = watch::channel(Some(one_relay_list(3)));
        rx.borrow_and_update();
        let mut current = rx.borrow().clone();

        let start = TokioInstant::now();
        let wake = wait_backoff(
            Duration::from_secs(20),
            &mut rx,
            &mut current,
            &mut open_gate(),
        )
        .await;

        assert_eq!(wake, BackoffWake::Elapsed);
        assert_eq!(start.elapsed(), Duration::from_secs(20));
        assert_eq!(current.map(|l| l.version), Some(3));
    }

    #[tokio::test(start_paused = true)]
    async fn closed_list_channel_still_waits_out_the_delay() {
        // No busy loop when the control-stream side is gone.
        let (tx, mut rx) = watch::channel(Some(one_relay_list(4)));
        rx.borrow_and_update();
        let mut current = rx.borrow().clone();
        drop(tx);

        let start = TokioInstant::now();
        let wake = wait_backoff(
            Duration::from_secs(30),
            &mut rx,
            &mut current,
            &mut open_gate(),
        )
        .await;

        assert_eq!(wake, BackoffWake::Elapsed);
        assert_eq!(start.elapsed(), Duration::from_secs(30));
        assert_eq!(current.map(|l| l.version), Some(4));
    }

    #[test]
    fn random_pick_order_empty() {
        let list = LabelledRelayList {
            relays: vec![],
            version: 1,
        };
        assert!(random_pick_order(&list).is_empty());
    }

    #[test]
    fn rotate_preserves_all_items() {
        let v = vec![&"a", &"b", &"c", &"d"];
        let r = rotate(v.clone(), 2);
        assert_eq!(r.len(), 4);
        assert_eq!(r, vec!["c", "d", "a", "b"]);
    }

    #[test]
    fn persist_ranking_takes_top_5() {
        let dir =
            std::env::temp_dir().join(format!("zecurity-selector-test-{}", uuid::Uuid::new_v4()));
        std::fs::create_dir_all(&dir).unwrap();
        let results: Vec<RelayProbeResult> = (0..10)
            .map(|i| RelayProbeResult {
                relay_id: format!("r{i}"),
                relay_addr: format!("r{i}.example.com:9093"),
                spiffe_id: format!("spiffe://zecurity.in/relay/r{i}"),
                rtt_ms: (10 - i) * 10,
                score: (10 - i) * 10,
            })
            .collect();
        let count = persist_ranking(&dir, 1, &results);
        assert_eq!(count, 5);
        let loaded = RelayRanking::load(&dir).unwrap();
        assert_eq!(loaded.entries.len(), 5);
        // Sorted ascending by score.
        for w in loaded.entries.windows(2) {
            assert!(w[0].score <= w[1].score);
        }
        let _ = std::fs::remove_dir_all(&dir);
    }

    // ---- Bug 2: exponential backoff ramp ----

    #[test]
    fn backoff_delay_ramps_and_caps() {
        let base = Duration::from_secs(5);
        let max = Duration::from_secs(120);
        let d1 = next_backoff_delay(base, 2.0, max);
        let d2 = next_backoff_delay(d1, 2.0, max);
        let d3 = next_backoff_delay(d2, 2.0, max);
        assert_eq!(d1, Duration::from_secs(10));
        assert_eq!(d2, Duration::from_secs(20));
        assert_eq!(d3, Duration::from_secs(40));
        // Ramp saturates at max and never exceeds it.
        let mut d = base;
        for _ in 0..20 {
            d = next_backoff_delay(d, 2.0, max);
        }
        assert_eq!(d, max);
    }

    #[test]
    fn backoff_delay_floors_and_clamps_factor() {
        // Zero base can't busy-loop: floored to 1s.
        assert_eq!(
            next_backoff_delay(Duration::ZERO, 2.0, Duration::from_secs(120)),
            Duration::from_secs(1)
        );
        // factor < 1.0 is clamped to 1.0 so the delay never shrinks.
        assert_eq!(
            next_backoff_delay(Duration::from_secs(30), 0.5, Duration::from_secs(120)),
            Duration::from_secs(30)
        );
    }

    // ---- Bug 1: migration target ordering ----

    fn temp_state_dir(label: &str) -> PathBuf {
        let p = std::env::temp_dir().join(format!(
            "zecurity-migtargets-{label}-{}",
            uuid::Uuid::new_v4()
        ));
        std::fs::create_dir_all(&p).unwrap();
        p
    }

    #[test]
    fn migration_prefers_explicit_target_over_list_order() {
        // list is [a, b, c] (a first). Probe winner is c. No ranking file.
        let dir = temp_state_dir("preferred");
        let list = LabelledRelayList {
            relays: vec![
                info("a", RelayCapacityLabel::RelayCapacityHigh),
                info("b", RelayCapacityLabel::RelayCapacityHigh),
                info("c", RelayCapacityLabel::RelayCapacityHigh),
            ],
            version: 1,
        };
        let order = build_migration_candidates(
            &dir,
            &list,
            "a",
            Some(info("c", RelayCapacityLabel::RelayCapacityHigh)),
        );
        // c (preferred) first, then b from list order; a (old) excluded, c de-duped.
        assert_eq!(
            order
                .iter()
                .map(|r| r.relay_id.as_str())
                .collect::<Vec<_>>(),
            vec!["c", "b"]
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn migration_uses_ranking_best_first_when_no_preferred() {
        // Persist a ranking [c(10), b(20)]; list is [a, b, c]; no preferred.
        let dir = temp_state_dir("ranking");
        let list = LabelledRelayList {
            relays: vec![
                info("a", RelayCapacityLabel::RelayCapacityHigh),
                info("b", RelayCapacityLabel::RelayCapacityHigh),
                info("c", RelayCapacityLabel::RelayCapacityHigh),
            ],
            version: 1,
        };
        persist_ranking(
            &dir,
            1,
            &[
                RelayProbeResult {
                    relay_id: "c".into(),
                    relay_addr: "c:9093".into(),
                    spiffe_id: "s".into(),
                    rtt_ms: 10,
                    score: 10,
                },
                RelayProbeResult {
                    relay_id: "b".into(),
                    relay_addr: "b:9093".into(),
                    spiffe_id: "s".into(),
                    rtt_ms: 20,
                    score: 20,
                },
            ],
        );
        let order = build_migration_candidates(&dir, &list, "a", None);
        // Ranking best-first: c then b; list adds nothing new; a excluded.
        assert_eq!(
            order
                .iter()
                .map(|r| r.relay_id.as_str())
                .collect::<Vec<_>>(),
            vec!["c", "b"]
        );
        let _ = std::fs::remove_dir_all(&dir);
    }

    #[test]
    fn migration_ignores_preferred_absent_from_list_and_excludes_old() {
        // preferred "z" is not in the list → ignored; falls back to list order.
        let dir = temp_state_dir("absent");
        let list = LabelledRelayList {
            relays: vec![
                info("a", RelayCapacityLabel::RelayCapacityHigh),
                info("b", RelayCapacityLabel::RelayCapacityHigh),
            ],
            version: 1,
        };
        let order = build_migration_candidates(
            &dir,
            &list,
            "a",
            Some(info("z", RelayCapacityLabel::RelayCapacityHigh)),
        );
        assert_eq!(
            order
                .iter()
                .map(|r| r.relay_id.as_str())
                .collect::<Vec<_>>(),
            vec!["b"]
        );
        let _ = std::fs::remove_dir_all(&dir);
    }
}

#[cfg(test)]
mod renewal_tests {
    use crate::test_support::{leaf_serial, selector_config, TestPki};

    fn first_cert_serial(pem: &[u8]) -> String {
        let chain: Vec<_> = rustls_pemfile::certs(&mut &pem[..])
            .collect::<Result<_, _>>()
            .unwrap();
        leaf_serial(&chain[0])
    }

    /// Relay probes and session dials read the identity at dial time: after a
    /// renewal is published, the next dial presents the renewed certificate
    /// (same key), not a copy captured at startup.
    #[test]
    fn relay_dials_use_the_renewed_identity() {
        let pki = TestPki::new();
        let (dir, holder) = pki.holder(-60, 3600);
        let cfg = selector_config(holder.clone(), dir.path());

        let before = cfg.current_identity();
        assert_eq!(
            first_cert_serial(&before.cert_pem),
            holder.current().serial_hex
        );

        let renewed = pki.renew(&holder, 7200);
        let after = cfg.current_identity();
        assert_eq!(
            first_cert_serial(&after.cert_pem),
            renewed.serial_hex,
            "dial must present the renewed certificate"
        );
        assert_ne!(
            first_cert_serial(&after.cert_pem),
            first_cert_serial(&before.cert_pem)
        );
        assert_eq!(after.key_pem, before.key_pem, "renewal keeps the key pair");
        assert_eq!(
            after.workspace_ca_pem, before.workspace_ca_pem,
            "CA bundle is pinned across renewals"
        );
    }
}

/// Sprint 20 Fix 13 (F38): a terminal connector stops dialling relays.
#[cfg(test)]
mod terminal_tests {
    use super::*;
    use crate::agent_tunnel::AgentTunnelHub;
    use crate::authorization::ServingState;
    use crate::crl::CrlManager;
    use crate::policy::PolicyCache;
    use crate::relay_attachment::RelayAttachment;
    use crate::session_registry::SessionRegistry;
    use crate::test_support::{selector_config, TestPki, CONNECTOR_ID};
    use std::sync::atomic::{AtomicBool, Ordering};

    struct Fx {
        _dir: tempfile::TempDir,
        cfg: RelaySelectorConfig,
        handler: Arc<RelayHandler>,
        slot: RelayAttachmentSlot,
        ctrl_tx: mpsc::Sender<ConnectorControlMessage>,
        _ctrl_rx: mpsc::Receiver<ConnectorControlMessage>,
    }

    fn fx() -> Fx {
        let pki = TestPki::new();
        let (dir, holder) = pki.holder(-60, 3600);
        let (ctrl_tx, ctrl_rx) = mpsc::channel(8);
        let handler = Arc::new(
            RelayHandler::new(
                holder.clone(),
                Arc::new(PolicyCache::new()),
                Arc::new(SessionRegistry::new()),
                AgentTunnelHub::new(),
                CrlManager::new(),
                CONNECTOR_ID.to_string(),
                ctrl_tx.clone(),
                5,
                16,
                AuthzGate::open_for_tests(),
            )
            .unwrap(),
        );
        Fx {
            cfg: selector_config(holder, dir.path()),
            _dir: dir,
            handler,
            slot: RelayAttachmentSlot::new(),
            ctrl_tx,
            _ctrl_rx: ctrl_rx,
        }
    }

    fn attachment(relay_id: &str) -> RelayAttachment {
        RelayAttachment {
            relay_id: relay_id.into(),
            relay_spiffe_id: format!("spiffe://zecurity.in/relay/{relay_id}"),
            attached_at: 0,
        }
    }

    /// A stand-in relay session: runs until aborted; `ended` is set when its
    /// future is dropped (abort), not when the handle is merely dropped.
    fn session() -> (ActiveRelay, Arc<AtomicBool>) {
        struct OnDrop(Arc<AtomicBool>);
        impl Drop for OnDrop {
            fn drop(&mut self) {
                self.0.store(true, Ordering::SeqCst);
            }
        }
        let ended = Arc::new(AtomicBool::new(false));
        let guard = OnDrop(ended.clone());
        let handle = tokio::spawn(async move {
            let _guard = guard;
            std::future::pending::<anyhow::Result<()>>().await
        });
        let active = ActiveRelay {
            info: LabelledRelayInfo {
                relay_id: "r1".into(),
                relay_addr: "127.0.0.1:1".into(),
                spiffe_id: "spiffe://zecurity.in/relay/r1".into(),
                label: RelayCapacityLabel::RelayCapacityHigh as i32,
            },
            handle,
            drain_tracker: RelayDrainTracker::new(),
            last_score: None,
        };
        (active, ended)
    }

    fn set_after(ctl: crate::authorization::TestGateControl, after: Duration, state: ServingState) {
        tokio::spawn(async move {
            tokio::time::sleep(after).await;
            ctl.set(state);
            // Keep the sender alive: a closed channel must not be what wakes anyone.
            std::future::pending::<()>().await;
        });
    }

    fn empty_list() -> watch::Receiver<Option<LabelledRelayList>> {
        let (tx, rx) = watch::channel(Some(LabelledRelayList {
            relays: vec![],
            version: 1,
        }));
        std::mem::forget(tx);
        rx
    }

    /// AF13-2: a terminal state ends the backoff at once.
    #[tokio::test(start_paused = true)]
    async fn terminal_ends_backoff_at_once() {
        let (gate, ctl) = AuthzGate::controllable_for_tests();
        set_after(ctl, Duration::from_secs(1), ServingState::UnknownIdentity);
        let mut rx = empty_list();
        let mut current = rx.borrow_and_update().clone();
        let start = TokioInstant::now();
        let wake = wait_backoff(
            Duration::from_secs(120),
            &mut rx,
            &mut current,
            &mut gate.subscribe(),
        )
        .await;
        assert_eq!(wake, BackoffWake::Terminal);
        assert_eq!(start.elapsed(), Duration::from_secs(1));
    }

    /// AF13-3: a recoverable loss doesn't end the backoff early.
    #[tokio::test(start_paused = true)]
    async fn a_recoverable_state_does_not_end_backoff() {
        for state in [ServingState::Disabled, ServingState::Recovering] {
            let (gate, ctl) = AuthzGate::controllable_for_tests();
            set_after(ctl, Duration::from_secs(1), state);
            let mut rx = empty_list();
            let mut current = rx.borrow_and_update().clone();
            let start = TokioInstant::now();
            let wake = wait_backoff(
                Duration::from_secs(20),
                &mut rx,
                &mut current,
                &mut gate.subscribe(),
            )
            .await;
            assert_eq!(wake, BackoffWake::Elapsed, "{state:?}");
            assert_eq!(start.elapsed(), Duration::from_secs(20), "{state:?}");
        }
    }

    /// AF13-1: stopping aborts the session task and clears the attachment.
    #[tokio::test]
    async fn stopping_aborts_the_session_and_clears_the_attachment() {
        let slot = RelayAttachmentSlot::new();
        slot.set_active(Some(attachment("r1"))).await;
        let (active, ended) = session();
        stop_on_terminal(State::Connected(active), &slot).await;
        for _ in 0..100 {
            if ended.load(Ordering::SeqCst) {
                break;
            }
            tokio::task::yield_now().await;
        }
        assert!(
            ended.load(Ordering::SeqCst),
            "the session task must be aborted, not detached"
        );
        assert!(
            slot.active().await.is_none(),
            "no relay is reported once terminal"
        );
    }

    /// AF13-1: a connected selector notices the terminal state at once, not
    /// at the next re-probe (300 s here).
    #[tokio::test(start_paused = true)]
    async fn terminal_wakes_a_connected_selector() {
        let f = fx();
        let (gate, ctl) = AuthzGate::controllable_for_tests();
        set_after(ctl, Duration::from_secs(1), ServingState::Revoked);
        let mut rx = empty_list();
        let mut current = rx.borrow_and_update().clone();
        let list = current.clone().unwrap();
        let (active, ended) = session();
        let start = TokioInstant::now();
        let next = connected_step(
            &f.cfg,
            &list,
            active,
            &f.handler,
            &f.slot,
            &f.ctrl_tx,
            &mut rx,
            &mut current,
            &mut gate.subscribe(),
        )
        .await;
        assert_eq!(start.elapsed(), Duration::from_secs(1));
        assert!(
            matches!(next, State::Connected(_)),
            "handed back to the loop top"
        );
        stop_on_terminal(next, &f.slot).await;
        tokio::task::yield_now().await;
        assert!(ended.load(Ordering::SeqCst));
    }

    /// AF13-1/2/3: the selector loop runs on through a recoverable loss and
    /// returns (stopped) as soon as the connector is terminal.
    #[tokio::test(start_paused = true)]
    async fn the_selector_stops_on_terminal_only() {
        let f = fx();
        f.slot.set_active(Some(attachment("stale"))).await;
        let (gate, ctl) = AuthzGate::controllable_for_tests();
        let ctl = Arc::new(ctl);
        let task = tokio::spawn(run_until_terminal(
            f.cfg.clone(),
            f.handler.clone(),
            f.slot.clone(),
            empty_list(),
            f.ctrl_tx.clone(),
            gate,
        ));

        tokio::time::sleep(Duration::from_secs(5)).await;
        ctl.set(ServingState::Disabled);
        tokio::time::sleep(Duration::from_secs(60)).await;
        assert!(
            !task.is_finished(),
            "Disabled is recoverable: the selector keeps running"
        );

        let at = TokioInstant::now();
        ctl.set(ServingState::Revoked);
        tokio::time::timeout(Duration::from_secs(1), task)
            .await
            .expect("stops at once on terminal, not after the backoff")
            .unwrap();
        assert!(at.elapsed() < Duration::from_secs(1));
        assert!(f.slot.active().await.is_none());
    }

    /// A selector still waiting for its first relay list stops too.
    #[tokio::test(start_paused = true)]
    async fn terminal_stops_a_selector_waiting_for_its_first_list() {
        let f = fx();
        let (gate, ctl) = AuthzGate::controllable_for_tests();
        set_after(ctl, Duration::from_secs(1), ServingState::Revoked);
        let (tx, rx) = watch::channel(None);
        let start = TokioInstant::now();
        tokio::time::timeout(
            Duration::from_secs(30),
            run_until_terminal(
                f.cfg.clone(),
                f.handler.clone(),
                f.slot.clone(),
                rx,
                f.ctrl_tx.clone(),
                gate,
            ),
        )
        .await
        .expect("stops without a list ever arriving");
        assert_eq!(start.elapsed(), Duration::from_secs(1));
        drop(tx);
    }
}
