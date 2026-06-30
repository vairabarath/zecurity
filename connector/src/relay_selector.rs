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

use crate::proto::connector_control_message;
use crate::proto::{
    ConnectorControlMessage, ConnectorRelayState, LabelledRelayInfo, LabelledRelayList,
    RelayCapacityLabel,
};
use crate::relay_attachment::{RelayAttachment, RelayAttachmentSlot};
use crate::relay_client;
use crate::relay_handler::RelayHandler;
use crate::relay_probe::{probe_relays, RelayProbeResult};
use crate::relay_ranking::{now_unix_seconds, RankedEntry, RelayRanking};

const MIGRATION_IMPROVEMENT_RATIO: f64 = 0.15;
const MIGRATION_IMPROVEMENT_MIN_MS: u64 = 10;
const TOP_RANKING_ENTRIES: usize = 5;
const FAILOVER_PER_ENTRY_BUDGET: Duration = Duration::from_secs(5);

#[derive(Clone)]
pub struct RelaySelectorConfig {
    pub state_dir: PathBuf,
    pub connector_id: String,
    pub connector_spiffe_id: String,
    pub cert_pem: Vec<u8>,
    pub key_pem: Vec<u8>,
    pub workspace_ca_pem: Vec<u8>,
    pub intermediate_ca_pem: Vec<u8>,
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

/// In-flight relay session: which relay we're attached to, the spawned
/// `run_session` task, and the most recent probe score for migration
/// comparisons.
struct ActiveRelay {
    info: LabelledRelayInfo,
    handle: JoinHandle<anyhow::Result<()>>,
    last_score: Option<u64>,
}

enum State {
    Disconnected,
    Connected(ActiveRelay),
    Backoff { delay: Duration },
}

/// Public entry. Runs forever; spawn from main.
pub async fn run(
    cfg: RelaySelectorConfig,
    relay_handler: Arc<RelayHandler>,
    attachment_slot: RelayAttachmentSlot,
    mut relay_list_rx: watch::Receiver<Option<LabelledRelayList>>,
    ctrl_tx: mpsc::Sender<ConnectorControlMessage>,
) -> ! {
    info!("Relay selector starting");
    let mut state = State::Disconnected;
    let mut current_list = relay_list_rx.borrow_and_update().clone();

    loop {
        // Block on first list arrival if we haven't seen one yet.
        if current_list.is_none() {
            info!("Relay selector waiting for first LabelledRelayList push");
            let _ = relay_list_rx.changed().await;
            current_list = relay_list_rx.borrow_and_update().clone();
            continue;
        }
        let list = current_list.clone().expect("checked just above");

        state = match state {
            State::Disconnected => bootstrap(&cfg, &list, &relay_handler, &attachment_slot, &ctrl_tx).await,
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
                )
                .await
            }
            State::Backoff { delay } => {
                emit_event(SelectorEvent::EnteredBackoff { delay_ms: delay.as_millis() as u64 });
                tokio::time::sleep(delay).await;
                let next = (delay.as_secs_f64() * cfg.reconnect_backoff_factor) as u64;
                let _next_delay = Duration::from_secs(next.min(cfg.reconnect_max.as_secs()).max(1));
                State::Disconnected
            }
        };
    }
}

async fn bootstrap(
    cfg: &RelaySelectorConfig,
    list: &LabelledRelayList,
    relay_handler: &Arc<RelayHandler>,
    attachment_slot: &RelayAttachmentSlot,
    ctrl_tx: &mpsc::Sender<ConnectorControlMessage>,
) -> State {
    if list.relays.is_empty() {
        warn!("LabelledRelayList is empty; entering backoff");
        return State::Backoff { delay: cfg.reconnect_base };
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
            .and_then(|entry| list.relays.iter().find(|r| r.relay_id == entry.relay_id).cloned())
            .map(|info| (info, ranking))
    });

    if let Some((info, _ranking)) = warm_candidate {
        info!(relay_id = %info.relay_id, "Selector picked ranked relay for warm start");
        if let Some(active) = connect(cfg, info, relay_handler, attachment_slot, ctrl_tx, None).await {
            emit_event(SelectorEvent::EnteredConnected { relay_id: active.info.relay_id.clone() });
            return State::Connected(active);
        }
        // Warm-start failed — fall through to random picks.
        debug!("Warm-start connect failed; falling back to random Tier-1/Tier-2");
    }

    for info in random_pick_order(list) {
        if let Some(active) = connect(cfg, info, relay_handler, attachment_slot, ctrl_tx, None).await {
            emit_event(SelectorEvent::EnteredConnected { relay_id: active.info.relay_id.clone() });
            return State::Connected(active);
        }
    }

    warn!("All relays in current list failed to connect; entering backoff");
    State::Backoff { delay: cfg.reconnect_base }
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
    let still_present = list.relays.iter().any(|r| r.relay_id == active.info.relay_id);
    if !still_present {
        info!(
            relay_id = %active.info.relay_id,
            "Active relay absent from new list; migrating immediately"
        );
        return migrate(cfg, list, active, relay_handler, attachment_slot, ctrl_tx, None).await;
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
    let results = probe_relays(
        &list.relays,
        &cfg.connector_id,
        &cfg.cert_pem,
        &cfg.key_pem,
        &cfg.workspace_ca_pem,
        &cfg.intermediate_ca_pem,
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
            if let Some(info) = list.relays.iter().find(|r| r.relay_id == best.relay_id).cloned() {
                info!(
                    from = %active.info.relay_id,
                    to = %info.relay_id,
                    active_score,
                    best_score = best.score,
                    "Migrating to better-scoring relay"
                );
                return migrate(cfg, list, active, relay_handler, attachment_slot, ctrl_tx, Some(best.score)).await;
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

#[allow(clippy::too_many_arguments)]
async fn migrate(
    cfg: &RelaySelectorConfig,
    list: &LabelledRelayList,
    old: ActiveRelay,
    relay_handler: &Arc<RelayHandler>,
    attachment_slot: &RelayAttachmentSlot,
    ctrl_tx: &mpsc::Sender<ConnectorControlMessage>,
    _best_score_hint: Option<u64>,
) -> State {
    // Pick a target relay. Prefer the best-scoring candidate from the most
    // recent probe sweep that's still in the list; otherwise random order.
    let candidates: Vec<LabelledRelayInfo> = list
        .relays
        .iter()
        .filter(|r| r.relay_id != old.info.relay_id)
        .cloned()
        .collect();

    for target in candidates {
        let (tx, rx) = oneshot::channel();
        let new_handle = spawn_session(cfg, target.clone(), relay_handler.clone(), attachment_slot.clone(), ctrl_tx.clone(), Some(tx));

        // Wait for register-OK on the new connection, or for the new session
        // to die before that happens.
        match rx.await {
            Ok(()) => {
                emit_event(SelectorEvent::MigrationStarted {
                    from: old.info.relay_id.clone(),
                    to: target.relay_id.clone(),
                });
                // Pending publishes after register-OK; heartbeat still
                // reports old as active until drain expires.
                attachment_slot
                    .set_pending(Some(RelayAttachment {
                        relay_id: target.relay_id.clone(),
                        relay_spiffe_id: target.spiffe_id.clone(),
                        attached_at: now_unix_seconds(),
                    }))
                    .await;
                let _ = ctrl_tx
                    .send(ConnectorControlMessage {
                        body: Some(connector_control_message::Body::RelayState(ConnectorRelayState {
                            connector_id: cfg.connector_id.clone(),
                            relay_id: target.relay_id.clone(),
                            relay_spiffe_id: target.spiffe_id.clone(),
                            observed_at_unix: now_unix_seconds(),
                            reason: "switched".to_string(),
                        })),
                    })
                    .await;

                tokio::time::sleep(cfg.drain_timeout).await;
                emit_event(SelectorEvent::DrainTimeoutFired);

                old.handle.abort();
                attachment_slot.promote_pending().await;

                emit_event(SelectorEvent::MigrationCompleted);
                emit_event(SelectorEvent::EnteredConnected { relay_id: target.relay_id.clone() });

                return State::Connected(ActiveRelay {
                    info: target,
                    handle: new_handle,
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
                .filter_map(|entry| list.relays.iter().find(|i| i.relay_id == entry.relay_id).cloned())
                .collect()
        })
        .unwrap_or_default();

    for target in ranked_targets {
        if let Some(active) = tokio::time::timeout(
            FAILOVER_PER_ENTRY_BUDGET,
            connect(cfg, target.clone(), relay_handler, attachment_slot, ctrl_tx, None),
        )
        .await
        .ok()
        .flatten()
        {
            emit_event(SelectorEvent::EnteredConnected { relay_id: active.info.relay_id.clone() });
            return State::Connected(active);
        }
    }

    // No ranked entry worked — re-probe and use the best result.
    let results = probe_relays(
        &list.relays,
        &cfg.connector_id,
        &cfg.cert_pem,
        &cfg.key_pem,
        &cfg.workspace_ca_pem,
        &cfg.intermediate_ca_pem,
        cfg.max_concurrent_probes,
        cfg.probe_timeout,
    )
    .await;

    let mut ordered = results.clone();
    ordered.sort_by_key(|r| r.score);
    for result in ordered {
        if let Some(info) = list.relays.iter().find(|r| r.relay_id == result.relay_id).cloned() {
            if let Some(active) = connect(cfg, info, relay_handler, attachment_slot, ctrl_tx, None).await {
                emit_event(SelectorEvent::EnteredConnected { relay_id: active.info.relay_id.clone() });
                return State::Connected(active);
            }
        }
    }

    State::Backoff { delay: cfg.reconnect_base }
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
    let handle = spawn_session(cfg, info.clone(), relay_handler.clone(), attachment_slot.clone(), ctrl_tx.clone(), tx);

    if let Some(rx) = rx {
        // Caller did not supply their own oneshot — wait here for register-OK.
        match rx.await {
            Ok(()) => Some(ActiveRelay {
                info,
                handle,
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
) -> JoinHandle<anyhow::Result<()>> {
    let cfg_owned = cfg.clone();
    let relay_addr = info.relay_addr;
    let relay_spiffe_id = info.spiffe_id;
    let connector_id = cfg_owned.connector_id.clone();
    let connector_spiffe_id = cfg_owned.connector_spiffe_id.clone();
    let cert_pem = cfg_owned.cert_pem.clone();
    let key_pem = cfg_owned.key_pem.clone();
    let workspace_ca_pem = cfg_owned.workspace_ca_pem.clone();
    let intermediate_ca_pem = cfg_owned.intermediate_ca_pem.clone();
    let max_streams = cfg_owned.max_incoming_bidi_streams;
    let idle_timeout = cfg_owned.idle_timeout;
    tokio::spawn(async move {
        relay_client::run_session(
            &relay_addr,
            &relay_spiffe_id,
            &connector_id,
            &connector_spiffe_id,
            &cert_pem,
            &key_pem,
            &workspace_ca_pem,
            &intermediate_ca_pem,
            relay_handler,
            max_streams,
            idle_timeout,
            attachment_slot,
            ctrl_tx,
            on_registered,
        )
        .await
    })
}

fn persist_ranking(state_dir: &std::path::Path, list_version: u64, results: &[RelayProbeResult]) -> usize {
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
        warn!(
            "no Tier-1 relays available; falling back to Tier-2 — this is a degraded mode"
        );
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
    EnteredConnected { relay_id: String },
    EnteredFailover,
    EnteredBackoff { delay_ms: u64 },
    ProbeSweepCompleted { results: Vec<RelayProbeResult> },
    RankingPersisted { entries: usize },
    MigrationStarted { from: String, to: String },
    MigrationCompleted,
    DrainTimeoutFired,
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
        let dir = std::env::temp_dir().join(format!(
            "zecurity-selector-test-{}",
            uuid::Uuid::new_v4()
        ));
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
}
