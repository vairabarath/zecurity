use std::collections::HashSet;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use futures_util::FutureExt;
use tokio::sync::mpsc;
// Tokio's clock (identical to std's unless a test pauses time).
use tokio::time::interval;
use tokio::time::Instant;
use tokio_stream::wrappers::ReceiverStream;
use tonic::Request;
use tracing::{error, info, warn};

use crate::config::ShieldConfig;
use crate::discovery;
use crate::proto::shield_control_message::Body;
use crate::proto::{
    shield_service_client::ShieldServiceClient, DiscoveredService as ProtoDiscoveredService,
    DiscoveryReport, ShieldControlMessage, ShieldHealthReport,
};
use crate::resources::SharedResourceState;
use crate::tunnel::{self, TunnelHub};
use crate::types::{ConnectorRef, ShieldState};
use crate::{renewal, resources, tls, util};

const BACKOFF_INITIAL_SECS: u64 = 5;
const BACKOFF_MAX_SECS: u64 = 60;
/// Retry interval while the Connector answers but is not serving yet
/// (`connector_not_serving`). Fixed, not exponential: the Connector is alive
/// and recovers on its own, so the Shield should re-attach within this
/// interval of the Connector serving again, not after a grown backoff.
const NOT_SERVING_RETRY_SECS: u64 = 2;
/// The `Unavailable` messages a live Connector uses to refuse or end a Shield
/// Control stream while it is not serving (connector/src/agent_server.rs,
/// `control()`: B1 admission gate, revocation-list gate, and the fail-closed
/// close-all). Keep in lockstep with the Connector.
const CONNECTOR_NOT_SERVING_MESSAGES: [&str; 2] = [
    "connector is not authorized to serve",
    "connector has not yet applied the controller's shield revocation list",
];
/// The Connector's refusal in a terminal state (revoked, unknown identity):
/// it will never serve again, so the Shield fails over instead of waiting.
/// Must match the Connector's `CONNECTOR_TERMINAL_MESSAGE`.
const CONNECTOR_TERMINAL_MESSAGE: &str = "connector is revoked or unknown to the controller";
/// Sprint 20 Fix 16 (F46): once every known Connector has refused the Shield
/// as revoked or unknown in a row, the retry interval doubles from
/// NOT_SERVING_RETRY_SECS up to this, instead of redialling every 2 s forever
/// (a Shield whose only Connector was revoked has nowhere else to go).
const TERMINAL_BACKOFF_MAX_SECS: u64 = 300;
const HEALTH_INTERVAL_SECS: u64 = 15;
/// Ignore a ReEnroll this soon after a successful renewal (Sprint 21 K2).
/// After renewing, the Shield reconnects and its first health report may
/// still be answered with ReEnroll by a connector that has not seen the new
/// certificate yet; that duplicate must not trigger another renewal.
const REENROLL_DEBOUNCE: Duration = Duration::from_secs(60);

/// When this process last renewed its certificate successfully. Owned by the
/// reconnect loop, so it survives the reconnect that follows a renewal.
#[derive(Debug, Default)]
struct RenewalDebounce {
    last_success: Option<Instant>,
}

impl RenewalDebounce {
    /// True while a ReEnroll should be ignored.
    fn suppresses(&self, now: Instant) -> bool {
        matches!(self.last_success, Some(at) if now.saturating_duration_since(at) < REENROLL_DEBOUNCE)
    }

    fn record_success(&mut self, now: Instant) {
        self.last_success = Some(now);
    }
}

pub async fn run_control_stream(
    state: ShieldState,
    cfg: ShieldConfig,
    resource_state: Arc<SharedResourceState>,
) -> Result<()> {
    reconnect_loop(
        state,
        LiveSession {
            cfg,
            resource_state,
        },
    )
    .await
}

/// One Control-stream connection: run until the stream ends.
trait ControlSession {
    fn run(
        &mut self,
        state: &mut ShieldState,
        debounce: &mut RenewalDebounce,
    ) -> impl std::future::Future<Output = Result<()>> + Send;
}

/// The real connection to the Connector.
struct LiveSession {
    cfg: ShieldConfig,
    resource_state: Arc<SharedResourceState>,
}

impl ControlSession for LiveSession {
    fn run(
        &mut self,
        state: &mut ShieldState,
        debounce: &mut RenewalDebounce,
    ) -> impl std::future::Future<Output = Result<()>> + Send {
        run_once(state, &self.cfg, &self.resource_state, debounce)
    }
}

/// Run `session` forever, reconnecting after each connection ends. The
/// renewal debounce is created once, here, so it survives the reconnect that
/// follows a successful renewal (K2); a per-connection debounce would let the
/// reconnected stream's first ReEnroll renew again.
async fn reconnect_loop<S: ControlSession>(state: ShieldState, mut session: S) -> Result<()> {
    let mut current_state = state;
    let mut backoff_secs = BACKOFF_INITIAL_SECS;
    let mut debounce = RenewalDebounce::default();
    // F46: consecutive "revoked or unknown" refusals, and the retry interval
    // once every known Connector has refused.
    let mut terminal_streak: usize = 0;
    let mut terminal_backoff_secs = NOT_SERVING_RETRY_SECS;

    loop {
        match session.run(&mut current_state, &mut debounce).await {
            Ok(()) => {
                backoff_secs = BACKOFF_INITIAL_SECS;
                terminal_streak = 0;
                terminal_backoff_secs = NOT_SERVING_RETRY_SECS;
                info!("control stream closed cleanly, reconnecting");
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            Err(e) if connector_terminal(&e) => {
                // Connector-revocation Phase F (finding B): the head refused
                // for good. Move it to the tail so the next attempt walks to
                // the next peer; in memory only — the failover that follows
                // persists the new head.
                if current_state.connectors.len() > 1 {
                    current_state.connectors.rotate_left(1);
                }
                terminal_streak += 1;
                let retry_secs = if terminal_streak < current_state.connectors.len().max(1) {
                    warn!(
                        retry_secs = NOT_SERVING_RETRY_SECS,
                        "connector is revoked or unknown; trying the next peer connector"
                    );
                    NOT_SERVING_RETRY_SECS
                } else {
                    // F46: every known Connector refused. Nothing else to try:
                    // back off instead of redialling every 2 s.
                    if terminal_streak == current_state.connectors.len().max(1) {
                        warn!(
                            connectors = current_state.connectors.len(),
                            "every known connector is revoked or unknown to the controller; \
                             backing off (re-enroll this shield or add a connector to its remote network)"
                        );
                    }
                    let wait = terminal_backoff_secs;
                    terminal_backoff_secs = (terminal_backoff_secs * 2).min(TERMINAL_BACKOFF_MAX_SECS);
                    wait
                };
                tokio::time::sleep(Duration::from_secs(retry_secs)).await;
                backoff_secs = BACKOFF_INITIAL_SECS;
            }
            Err(e) if connector_not_serving(&e) => {
                terminal_streak = 0;
                terminal_backoff_secs = NOT_SERVING_RETRY_SECS;
                // The Connector is up but not serving (e.g. Disabled while
                // the Controller is unreachable). Poll it at a short fixed
                // interval and reset the backoff: with the exponential
                // backoff the Shield re-attached up to ~40 s after the
                // Connector was serving again (B2 live finding 1).
                warn!(
                    retry_secs = NOT_SERVING_RETRY_SECS,
                    "connector is not serving yet; retrying"
                );
                tokio::time::sleep(Duration::from_secs(NOT_SERVING_RETRY_SECS)).await;
                backoff_secs = BACKOFF_INITIAL_SECS;
            }
            Err(e) => {
                terminal_streak = 0;
                terminal_backoff_secs = NOT_SERVING_RETRY_SECS;
                // Debug formatting includes the full anyhow source chain;
                // Display would only print the outermost context.
                error!(
                    error = ?e,
                    backoff_secs,
                    "control stream error, reconnecting with backoff"
                );
                tokio::time::sleep(Duration::from_secs(backoff_secs)).await;
                backoff_secs = (backoff_secs * 2).min(BACKOFF_MAX_SECS);
            }
        }
    }
}

/// True when `err` carries the Connector's "not serving" refusal (see
/// `CONNECTOR_NOT_SERVING_MESSAGES`), whether the stream was refused at open
/// or ended by the Connector mid-stream.
fn connector_terminal(err: &anyhow::Error) -> bool {
    err.chain().any(|cause| {
        cause.downcast_ref::<tonic::Status>().is_some_and(|status| {
            status.code() == tonic::Code::Unavailable
                && status.message() == CONNECTOR_TERMINAL_MESSAGE
        })
    })
}

fn connector_not_serving(err: &anyhow::Error) -> bool {
    err.chain().any(|cause| {
        cause.downcast_ref::<tonic::Status>().is_some_and(|status| {
            status.code() == tonic::Code::Unavailable
                && CONNECTOR_NOT_SERVING_MESSAGES.contains(&status.message())
        })
    })
}

async fn run_once(
    state: &mut ShieldState,
    cfg: &ShieldConfig,
    resource_state: &Arc<SharedResourceState>,
    debounce: &mut RenewalDebounce,
) -> Result<()> {
    let (mut client, selected_idx) = build_client(state, cfg)
        .await
        .context("failed to build mTLS client for Control stream")?;

    // If we failed over to a non-head Connector, rotate the list so the new
    // head is the surviving Connector and persist. Next reconnect prefers it.
    if selected_idx > 0 {
        state.connectors.rotate_left(selected_idx);
        if let Err(e) = state.save(&cfg.state_dir) {
            warn!(error = %e, "failed to persist state.json after connector head rotation");
        }
    }

    let hostname = util::read_hostname();
    let public_ip = util::get_public_ip().await.unwrap_or_default();
    let lan_ip = util::detect_lan_ip().unwrap_or_default();
    let version = env!("CARGO_PKG_VERSION").to_string();

    let mut discovery_sent: HashSet<(u16, String)> = HashSet::new();
    let mut discovery_fingerprint: u64 = 0;
    let mut discovery_seq: u64 = 0;

    let tunnel_hub: TunnelHub = tunnel::new_hub();
    // Fix 14 (F44): however this connection ends — other than by a handover,
    // which keeps them — its tunnels end with it.
    let _close_tunnels = CloseTunnelsOnDrop(tunnel_hub.clone());

    let (mut out_tx, out_rx) = mpsc::channel::<ShieldControlMessage>(32);
    let response = client
        .control(Request::new(ReceiverStream::new(out_rx)))
        .await
        .context("failed to open Shield Control stream")?;
    let mut inbound = response.into_inner();
    let connector_id = state.connectors[0].connector_id.clone();
    tunnel_hub.set_upstream(Some(out_tx.clone()));

    info!(
        shield_id = %state.shield_id,
        connector_id = %state.connectors[0].connector_id,
        connector_addr = %state.connectors[0].connector_addr,
        peers = %state.connectors.len(),
        "Shield Control stream established"
    );

    send_health(&out_tx, &version, &hostname, &public_ip, &lan_ip).await?;

    // Discovery: set up interval, consume its immediate first tick, then send full sync.
    let mut discovery_tick =
        tokio::time::interval(Duration::from_secs(cfg.discovery_interval_secs));
    discovery_tick.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
    discovery_tick.tick().await; // consume the immediate first tick

    match discovery::run_discovery_full_sync(
        &state.shield_id,
        &mut discovery_sent,
        &mut discovery_fingerprint,
        &mut discovery_seq,
    )
    .await
    {
        Ok(diff) => send_discovery_report(diff, &out_tx).await,
        Err(e) => warn!("discovery: full sync error: {:#}", e),
    }

    let mut health_ticker = interval(Duration::from_secs(HEALTH_INTERVAL_SECS));
    health_ticker.tick().await;

    loop {
        tokio::select! {
            result = inbound.message() => {
                match result {
                    // Keep the Status in the chain: reconnect_loop inspects it.
                    Err(e) => return Err(anyhow::Error::new(e).context("control stream recv error")),
                    Ok(None) => return Ok(()),
                    Ok(Some(msg)) => {
                        match handle_connector_msg(
                            msg,
                            state,
                            cfg,
                            resource_state,
                            &out_tx,
                            tunnel_hub.clone(),
                            debounce,
                        ).await {
                            None => {}
                            // The certificate was renewed: move to a stream
                            // with it, keeping the tunnels (Fix 14, D-1 = B).
                            Some(Ok(())) => {
                                if !hand_over(
                                    state,
                                    cfg,
                                    resource_state,
                                    debounce,
                                    &tunnel_hub,
                                    &connector_id,
                                    &mut client,
                                    &mut out_tx,
                                    &mut inbound,
                                )
                                .await?
                                {
                                    // Not possible here: reconnect as before
                                    // (the tunnels are closed).
                                    return Ok(());
                                }
                                let tunnels = tunnel_hub.len().await;
                                info!(
                                    shield_id = %state.shield_id,
                                    tunnels,
                                    "cert renewed — Control stream handed over, tunnels kept"
                                );
                                send_health(&out_tx, &version, &hostname, &public_ip, &lan_ip).await?;
                                match discovery::run_discovery_full_sync(
                                    &state.shield_id,
                                    &mut discovery_sent,
                                    &mut discovery_fingerprint,
                                    &mut discovery_seq,
                                )
                                .await
                                {
                                    Ok(diff) => send_discovery_report(diff, &out_tx).await,
                                    Err(e) => warn!("discovery: full sync error: {:#}", e),
                                }
                            }
                            Some(Err(e)) => return Err(e),
                        }
                    }
                }
            }

            _ = health_ticker.tick() => {
                send_health(&out_tx, &version, &hostname, &public_ip, &lan_ip).await?;
                for ack in resource_state.drain_acks() {
                    out_tx
                        .send(ShieldControlMessage {
                            body: Some(Body::ResourceAck(ack)),
                        })
                        .await
                        .context("failed to send resource ack")?;
                }
                // ADR-004 Phase 3: report actual enforced state every heartbeat
                // so the controller can reconcile observed vs desired.
                let report = resource_state.build_state_report(&state.shield_id);
                out_tx
                    .send(ShieldControlMessage {
                        body: Some(Body::ResourceState(report)),
                    })
                    .await
                    .context("failed to send resource state report")?;
                // Non-blocking poll: only runs when discovery_interval_secs have elapsed.
                if discovery_tick.tick().now_or_never().is_some() {
                    match discovery::run_discovery_diff(
                        &state.shield_id,
                        &mut discovery_sent,
                        &mut discovery_fingerprint,
                        &mut discovery_seq,
                    )
                    .await
                    {
                        Ok(Some(diff)) => send_discovery_report(diff, &out_tx).await,
                        Ok(None) => {}
                        Err(e) => warn!("discovery: diff error: {:#}", e),
                    }
                }
            }
        }
    }
}

/// Fix 14 (D-1 = B): after a renewal, move from the current Control stream to
/// one opened with the new certificate, to the same Connector, keeping every
/// tunnel (make before break). In order:
/// 1. open the new stream — the Connector registers it before answering;
/// 2. pause tunnel traffic and half-close the old stream: the Connector gets
///    everything sent on it, then its end, and hands the sessions over;
/// 3. read the old stream to its end (the Connector's last device data comes
///    on it) before reading anything from the new one;
/// 4. resume tunnel traffic on the new stream.
///
/// Returns false, changing nothing, when the new stream cannot be opened to
/// the same Connector; the caller then reconnects as before. An error after
/// the old stream was half-closed ends the connection (tunnels closed).
#[allow(clippy::too_many_arguments)]
async fn hand_over(
    state: &mut ShieldState,
    cfg: &ShieldConfig,
    resource_state: &Arc<SharedResourceState>,
    debounce: &mut RenewalDebounce,
    hub: &TunnelHub,
    connector_id: &str,
    client: &mut ShieldServiceClient<tonic::transport::Channel>,
    out_tx: &mut mpsc::Sender<ShieldControlMessage>,
    inbound: &mut tonic::Streaming<ShieldControlMessage>,
) -> Result<bool> {
    let (mut new_client, idx) = match build_client(state, cfg).await {
        Ok(found) => found,
        Err(e) => {
            warn!(error = %e, "renewed, but no Connector reachable with the new certificate");
            return Ok(false);
        }
    };
    if state.connectors[idx].connector_id != connector_id {
        warn!("renewed, but the current Connector is not reachable: reconnecting");
        return Ok(false);
    }
    let (new_out_tx, new_out_rx) = mpsc::channel::<ShieldControlMessage>(32);
    let new_inbound = match new_client
        .control(Request::new(ReceiverStream::new(new_out_rx)))
        .await
    {
        Ok(response) => response.into_inner(),
        Err(status) => {
            warn!(error = %status, "renewed, but the new Control stream was refused: reconnecting");
            return Ok(false);
        }
    };

    hub.set_upstream(None);
    drop(std::mem::replace(out_tx, new_out_tx));
    let mut old_inbound = std::mem::replace(inbound, new_inbound);
    let old_client = std::mem::replace(client, new_client);

    let drained = tokio::time::timeout(tunnel::HANDOVER_GRACE, async {
        loop {
            match old_inbound.message().await {
                Ok(None) => return Ok(()),
                Err(e) => {
                    return Err(anyhow::Error::new(e).context("old Control stream failed during handover"))
                }
                Ok(Some(msg)) => {
                    if let Some(action) = handle_connector_msg(
                        msg,
                        state,
                        cfg,
                        resource_state,
                        out_tx,
                        hub.clone(),
                        debounce,
                    )
                    .await
                    {
                        action?;
                    }
                }
            }
        }
    })
    .await;
    drop(old_client);
    match drained {
        Ok(Ok(())) => {
            hub.set_upstream(Some(out_tx.clone()));
            Ok(true)
        }
        Ok(Err(e)) => Err(e),
        Err(_) => Err(anyhow::anyhow!(
            "old Control stream did not end within the handover grace"
        )),
    }
}

/// Ends every tunnel of a Control stream when the stream's `run_once` returns.
struct CloseTunnelsOnDrop(TunnelHub);

impl Drop for CloseTunnelsOnDrop {
    fn drop(&mut self) {
        let hub = self.0.clone();
        tokio::spawn(async move {
            let closed = hub.close_all().await;
            if closed > 0 {
                info!(closed, "Control stream ended: its tunnels were closed");
            }
        });
    }
}

/// ReEnroll from the connector: renew unless a renewal succeeded within
/// REENROLL_DEBOUNCE. Success → `Some(Ok(()))` (move to a stream with the new
/// certificate, `hand_over`); debounced or failed → `None` (keep the stream).
async fn handle_reenroll<F, Fut>(
    state: &mut ShieldState,
    debounce: &mut RenewalDebounce,
    renew: F,
) -> Option<Result<()>>
where
    F: FnOnce(ShieldState) -> Fut,
    Fut: std::future::Future<Output = Result<ShieldState>>,
{
    if debounce.suppresses(Instant::now()) {
        info!(
            shield_id = %state.shield_id,
            "ignoring ReEnroll: certificate renewed less than 60 s ago"
        );
        return None;
    }
    info!(shield_id = %state.shield_id, "connector requested cert renewal");
    match renew(state.clone()).await {
        Ok(mut new_state) => {
            debounce.record_success(Instant::now());
            // Phase E (D21): the in-memory revoked set survives the renewal.
            new_state
                .revoked_connectors
                .extend(state.revoked_connectors.drain());
            info!(
                shield_id = %new_state.shield_id,
                new_expiry = new_state.cert_not_after,
                "cert renewed — moving to a Control stream with it"
            );
            *state = new_state;
            Some(Ok(()))
        }
        Err(e) => {
            error!(error = %e, "cert renewal failed");
            None
        }
    }
}

async fn handle_connector_msg(
    msg: ShieldControlMessage,
    state: &mut ShieldState,
    cfg: &ShieldConfig,
    resource_state: &Arc<SharedResourceState>,
    out_tx: &mpsc::Sender<ShieldControlMessage>,
    tunnel_hub: TunnelHub,
    debounce: &mut RenewalDebounce,
) -> Option<Result<()>> {
    match msg.body {
        Some(Body::ResourceInstruction(instr)) => {
            if let Some(ack) = resources::handle_instruction(&instr, resource_state).await {
                resource_state.store_ack(ack.clone());
                if out_tx
                    .send(ShieldControlMessage {
                        body: Some(Body::ResourceAck(ack)),
                    })
                    .await
                    .is_err()
                {
                    return Some(Err(anyhow::anyhow!("outbound channel closed on ack")));
                }
            }
            None
        }
        // ADR-004 Phase 2: authoritative desired-state snapshot — replace the
        // active set, rebuild the chain, ack every contained resource.
        Some(Body::ResourceSnapshot(snap)) => {
            let acks = resources::handle_snapshot(&snap, resource_state).await;
            for ack in acks {
                resource_state.store_ack(ack.clone());
                if out_tx
                    .send(ShieldControlMessage {
                        body: Some(Body::ResourceAck(ack)),
                    })
                    .await
                    .is_err()
                {
                    return Some(Err(anyhow::anyhow!(
                        "outbound channel closed on snapshot ack"
                    )));
                }
            }
            None
        }
        Some(Body::ReEnroll(_)) => {
            handle_reenroll(state, debounce, |current| async move {
                renewal::renew_cert(&current, cfg).await
            })
            .await
        }
        Some(Body::Ping(p)) => {
            if out_tx
                .send(ShieldControlMessage {
                    body: Some(Body::Pong(crate::proto::Pong {
                        timestamp_unix: p.timestamp_unix,
                    })),
                })
                .await
                .is_err()
            {
                return Some(Err(anyhow::anyhow!("outbound channel closed on pong")));
            }
            None
        }
        Some(Body::TunnelOpen(open)) => {
            tunnel::handle_tunnel_open(
                tunnel_hub,
                open.connection_id,
                open.destination,
                open.port,
                open.protocol,
            )
            .await;
            None
        }
        Some(Body::TunnelData(data)) => {
            tunnel::handle_tunnel_data(tunnel_hub, &data.connection_id, data.data).await;
            None
        }
        Some(Body::TunnelClose(close)) => {
            tunnel::handle_tunnel_close(tunnel_hub, &close.connection_id).await;
            None
        }
        Some(Body::PeerConnectorList(list)) => {
            apply_peer_connector_list(state, cfg, list);
            None
        }
        other => {
            warn!(?other, "ignored unsupported Shield Control message");
            None
        }
    }
}

/// Apply an incoming `PeerConnectorList` to the Shield's persistent state.
///
///   - Revoked ids (Phase E, D21) are added to the in-memory revoked set
///     first and pruned from the current list at once, whichever Connector
///     sent them; a revoked Connector is never re-added while the process
///     runs. A pruned list is saved unless it became empty (state.json must
///     keep a Connector to start from; the in-memory list stays empty, so
///     the Shield dials nothing).
///   - Empty `peers` (after dropping revoked ids) is ignored — the Shield's
///     existing list is safer than a truncated one.
///   - Identical content (order-insensitive by connector_id) is a no-op —
///     no rewrite of state.json.
///   - New content replaces the list. The current head is preserved when
///     the new list still contains it; otherwise the new head is index 0.
fn apply_peer_connector_list(
    state: &mut ShieldState,
    cfg: &ShieldConfig,
    list: crate::proto::PeerConnectorList,
) {
    prune_revoked_connectors(state, cfg, list.revoked_connector_ids);
    let peers: Vec<crate::proto::PeerConnector> = list
        .peers
        .into_iter()
        .filter(|p| !state.revoked_connectors.contains(&p.connector_id))
        .collect();
    if peers.is_empty() {
        warn!("ignored empty PeerConnectorList");
        return;
    }
    let mut new_list: Vec<ConnectorRef> = peers
        .into_iter()
        .map(|p| ConnectorRef {
            connector_id: p.connector_id,
            connector_addr: p.connector_addr,
        })
        .collect();

    let mut current_sorted = state.connectors.clone();
    current_sorted.sort_by(|a, b| a.connector_id.cmp(&b.connector_id));
    let mut new_sorted = new_list.clone();
    new_sorted.sort_by(|a, b| a.connector_id.cmp(&b.connector_id));
    if current_sorted == new_sorted {
        info!(
            peers = new_list.len(),
            "PeerConnectorList unchanged, no update"
        );
        return;
    }

    // Preserve current head if it survives in the new list. Otherwise the
    // new list's index-0 becomes head.
    if let Some(current_head_id) = state.connectors.first().map(|c| c.connector_id.clone()) {
        if let Some(idx) = new_list
            .iter()
            .position(|c| c.connector_id == current_head_id)
        {
            new_list.rotate_left(idx);
        }
    }

    let peer_count = new_list.len();
    state.connectors = new_list;
    if let Err(e) = state.save(&cfg.state_dir) {
        warn!(error = %e, "failed to persist state.json after peer-list update");
    } else {
        info!(peers = peer_count, "peer connector list refreshed");
    }
}

/// Phase E (D21): remember `revoked_ids` and drop them from the current list.
fn prune_revoked_connectors(state: &mut ShieldState, cfg: &ShieldConfig, revoked_ids: Vec<String>) {
    state
        .revoked_connectors
        .extend(revoked_ids.into_iter().filter(|id| !id.is_empty()));
    let before = state.connectors.len();
    let revoked = &state.revoked_connectors;
    state
        .connectors
        .retain(|c| !revoked.contains(&c.connector_id));
    let pruned = before - state.connectors.len();
    if pruned == 0 {
        return;
    }
    warn!(
        pruned,
        remaining = state.connectors.len(),
        "revoked connectors pruned from the peer list"
    );
    if state.connectors.is_empty() {
        warn!("every peer connector is revoked; the Shield dials none until a new list arrives (state.json left as is)");
        return;
    }
    if let Err(e) = state.save(&cfg.state_dir) {
        warn!(error = %e, "failed to persist state.json after pruning revoked connectors");
    }
}

async fn send_health(
    out_tx: &mpsc::Sender<ShieldControlMessage>,
    version: &str,
    hostname: &str,
    public_ip: &str,
    lan_ip: &str,
) -> Result<()> {
    out_tx
        .send(ShieldControlMessage {
            body: Some(Body::HealthReport(ShieldHealthReport {
                version: version.to_string(),
                hostname: hostname.to_string(),
                public_ip: public_ip.to_string(),
                lan_ip: lan_ip.to_string(),
            })),
        })
        .await
        .context("failed to send shield health report")
}

/// Build an mTLS client, walking the Shield's peer-Connector list from head
/// to tail. Returns the first Connector that answers and the index it lived
/// at in the list (so the caller can rotate it to head + persist). If every
/// Connector is unreachable, returns the last error the walk observed.
async fn build_client(
    state: &ShieldState,
    cfg: &ShieldConfig,
) -> Result<(ShieldServiceClient<tonic::transport::Channel>, usize)> {
    let state_dir = Path::new(&cfg.state_dir);
    let ca_pem = tokio::fs::read(state_dir.join("workspace_ca.crt")).await?;
    let cert_pem = tokio::fs::read(state_dir.join("shield.crt")).await?;
    let key_pem = tokio::fs::read(state_dir.join("shield.key")).await?;

    let mut last_err: Option<anyhow::Error> = None;
    for (idx, conn) in state.connectors.iter().enumerate() {
        match tls::build_connector_channel(
            &ca_pem,
            &cert_pem,
            &key_pem,
            &conn.connector_id,
            &state.trust_domain,
            &conn.connector_addr,
        )
        .await
        {
            Ok(channel) => {
                if idx > 0 {
                    info!(
                        connector_id = %conn.connector_id,
                        connector_addr = %conn.connector_addr,
                        from_idx = idx,
                        "Shield failed over to peer Connector",
                    );
                }
                return Ok((ShieldServiceClient::new(channel), idx));
            }
            Err(e) => {
                warn!(
                    connector_id = %conn.connector_id,
                    connector_addr = %conn.connector_addr,
                    error = %e,
                    "Shield: peer Connector unreachable, trying next",
                );
                last_err = Some(e);
            }
        }
    }

    Err(last_err.unwrap_or_else(|| anyhow::anyhow!("Shield has no peer connectors to dial")))
}

async fn send_discovery_report(
    diff: discovery::DiscoveryDiff,
    out_tx: &mpsc::Sender<ShieldControlMessage>,
) {
    let seq = diff.seq;
    let added_len = diff.added.len();
    let removed_len = diff.removed.len();
    let full_sync = diff.full_sync;

    let proto_report = DiscoveryReport {
        shield_id: diff.shield_id,
        seq,
        fingerprint: diff.fingerprint,
        full_sync,
        added: diff
            .added
            .into_iter()
            .map(|s| ProtoDiscoveredService {
                protocol: s.protocol.to_string(),
                port: s.port as u32,
                bound_ip: s.bound_ip,
                service_name: s.service_name,
            })
            .collect(),
        removed: diff
            .removed
            .into_iter()
            .map(|(port, proto)| ProtoDiscoveredService {
                protocol: proto,
                port: port as u32,
                bound_ip: String::new(),
                service_name: String::new(),
            })
            .collect(),
    };

    let msg = ShieldControlMessage {
        body: Some(Body::DiscoveryReport(proto_report)),
    };

    match out_tx.send(msg).await {
        Ok(()) => info!(
            seq,
            added = added_len,
            removed = removed_len,
            full_sync,
            "discovery: report sent"
        ),
        Err(e) => warn!("discovery: failed to send report: {}", e),
    }
}

/// Best-effort Goodbye RPC on SIGTERM so the connector removes this shield
/// from its in-memory health map immediately. Reuses the same peer-list
/// failover walk — if the head Connector is already gone, goodbye still
/// reaches a healthy sibling.
pub async fn goodbye(state: &ShieldState, cfg: &ShieldConfig) {
    match build_client(state, cfg).await {
        Ok((mut client, _idx)) => {
            let req = Request::new(crate::proto::GoodbyeRequest {
                shield_id: state.shield_id.clone(),
            });
            match client.goodbye(req).await {
                Ok(_) => info!(shield_id = %state.shield_id, "goodbye sent to connector"),
                Err(e) => warn!(error = %e, "goodbye RPC failed (non-fatal)"),
            }
        }
        Err(e) => warn!(error = %e, "failed to connect for goodbye (non-fatal)"),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn debounce_suppresses_for_sixty_seconds_after_success() {
        let t0 = Instant::now();
        let mut d = RenewalDebounce::default();
        assert!(!d.suppresses(t0), "no renewal yet: never suppressed");
        d.record_success(t0);
        assert!(d.suppresses(t0));
        assert!(d.suppresses(t0 + Duration::from_secs(59)));
        assert!(!d.suppresses(t0 + Duration::from_secs(60)));
    }

    fn test_state() -> ShieldState {
        ShieldState {
            shield_id: "shield-1".into(),
            trust_domain: "ws-test.zecurity.in".into(),
            connectors: vec![ConnectorRef {
                connector_id: "conn-1".into(),
                connector_addr: "127.0.0.1:1".into(),
            }],
            interface_addr: "100.64.0.5".into(),
            enrolled_at: "2026-10-09T00:00:00Z".into(),
            cert_not_after: 1_000,
            revoked_connectors: HashSet::new(),
        }
    }

    /// Renewal stand-in: counts calls; succeeds with a later NotAfter, or fails.
    fn fake_renew(
        calls: &std::cell::Cell<u32>,
        ok: bool,
    ) -> impl FnOnce(ShieldState) -> std::future::Ready<Result<ShieldState>> + '_ {
        move |current| {
            calls.set(calls.get() + 1);
            std::future::ready(if ok {
                Ok(ShieldState {
                    cert_not_after: current.cert_not_after + 3600,
                    ..current
                })
            } else {
                Err(anyhow::anyhow!("controller unreachable"))
            })
        }
    }

    /// What one scripted ReEnroll did.
    #[derive(Debug, PartialEq)]
    enum Reenroll {
        Renewed,
        Ignored,
    }

    /// A fake Control connection: delivers ReEnrolls through the real
    /// `handle_reenroll` (with a counting fake renewal) on a script, and
    /// returns `Ok(())` after a successful renewal — the reconnect `run_once`
    /// performs.
    struct ScriptedSession {
        connection: usize,
        renewals: Arc<std::sync::atomic::AtomicU32>,
        log: Arc<std::sync::Mutex<Vec<(usize, u64, Reenroll)>>>,
        started: tokio::time::Instant,
        done: Option<tokio::sync::oneshot::Sender<()>>,
    }

    impl ScriptedSession {
        async fn reenroll(
            &mut self,
            state: &mut ShieldState,
            debounce: &mut RenewalDebounce,
        ) -> bool {
            let renewals = self.renewals.clone();
            let outcome = handle_reenroll(state, debounce, move |current| {
                renewals.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                std::future::ready(Ok(ShieldState {
                    cert_not_after: current.cert_not_after + 3600,
                    ..current
                }))
            })
            .await;
            let renewed = matches!(outcome, Some(Ok(())));
            self.log.lock().unwrap().push((
                self.connection,
                self.started.elapsed().as_secs(),
                if renewed {
                    Reenroll::Renewed
                } else {
                    Reenroll::Ignored
                },
            ));
            renewed
        }
    }

    impl ControlSession for ScriptedSession {
        async fn run(
            &mut self,
            state: &mut ShieldState,
            debounce: &mut RenewalDebounce,
        ) -> Result<()> {
            self.connection += 1;
            match self.connection {
                // Connection 1: ReEnroll → renewal → reconnect.
                1 => assert!(self.reenroll(state, debounce).await),
                // Connection 2 (after the 1 s reconnect delay): the
                // connector repeats ReEnroll at once, at 58 s and at 60 s
                // after the renewal.
                2 => {
                    while !self.reenroll(state, debounce).await {
                        let since_renewal = self.started.elapsed();
                        let next = if since_renewal < Duration::from_secs(58) {
                            Duration::from_secs(58)
                        } else {
                            Duration::from_secs(60)
                        };
                        tokio::time::sleep(next - since_renewal).await;
                    }
                }
                // Connection 3: done.
                _ => {
                    if let Some(done) = self.done.take() {
                        let _ = done.send(());
                    }
                    std::future::pending::<()>().await;
                }
            }
            Ok(())
        }
    }

    // ---- Re-attach promptly when the Connector is up but not serving ----

    fn not_serving_at_open() -> anyhow::Error {
        anyhow::Error::new(tonic::Status::unavailable("connector is not authorized to serve"))
            .context("failed to open Shield Control stream")
    }

    fn not_serving_mid_stream() -> anyhow::Error {
        anyhow::Error::new(tonic::Status::unavailable("connector is not authorized to serve"))
            .context("control stream recv error")
    }

    /// Returns the scripted outcomes in order and records when each
    /// connection attempt started; pends forever once the script is done.
    struct OutcomeSession {
        script: std::collections::VecDeque<Result<()>>,
        starts: Arc<std::sync::Mutex<Vec<u64>>>,
        heads: Arc<std::sync::Mutex<Vec<String>>>,
        t0: tokio::time::Instant,
        done: Option<tokio::sync::oneshot::Sender<()>>,
    }

    impl ControlSession for OutcomeSession {
        async fn run(
            &mut self,
            state: &mut ShieldState,
            _debounce: &mut RenewalDebounce,
        ) -> Result<()> {
            self.starts.lock().unwrap().push(self.t0.elapsed().as_secs());
            self.heads
                .lock()
                .unwrap()
                .push(state.connectors[0].connector_id.clone());
            match self.script.pop_front() {
                Some(outcome) => outcome,
                None => {
                    if let Some(done) = self.done.take() {
                        let _ = done.send(());
                    }
                    std::future::pending().await
                }
            }
        }
    }

    async fn attempt_times(script: Vec<Result<()>>) -> Vec<u64> {
        attempts(test_state(), script).await.0
    }

    /// (start time, head connector) of every connection attempt.
    async fn attempts(state: ShieldState, script: Vec<Result<()>>) -> (Vec<u64>, Vec<String>) {
        let starts = Arc::new(std::sync::Mutex::new(Vec::new()));
        let heads = Arc::new(std::sync::Mutex::new(Vec::new()));
        let (done_tx, done_rx) = tokio::sync::oneshot::channel();
        let session = OutcomeSession {
            script: script.into(),
            starts: starts.clone(),
            heads: heads.clone(),
            t0: tokio::time::Instant::now(),
            done: Some(done_tx),
        };
        let task = tokio::spawn(reconnect_loop(state, session));
        done_rx.await.expect("script finished");
        task.abort();
        let times = starts.lock().unwrap().clone();
        let heads = heads.lock().unwrap().clone();
        (times, heads)
    }

    /// Matched by text against the Connector
    /// (`connector/src/agent_server.rs`); both sides pin the same literal.
    #[test]
    fn terminal_message_matches_the_connector() {
        assert_eq!(
            CONNECTOR_TERMINAL_MESSAGE,
            "connector is revoked or unknown to the controller"
        );
    }

    fn terminal_refusal() -> anyhow::Error {
        anyhow::Error::new(tonic::Status::unavailable(CONNECTOR_TERMINAL_MESSAGE))
            .context("failed to open Shield Control stream")
    }

    fn two_peer_state() -> ShieldState {
        let mut state = test_state();
        state.connectors.push(ConnectorRef {
            connector_id: "conn-2".into(),
            connector_addr: "127.0.0.1:2".into(),
        });
        state
    }

    /// Connector-revocation Phase F (finding B, AF-5): a revoked connector's
    /// refusal moves it to the tail, so the next attempt (after the short
    /// retry) starts at the next peer; "not serving yet" keeps the head.
    #[tokio::test(start_paused = true)]
    async fn terminal_refusal_fails_over_but_not_serving_waits() {
        let (times, heads) = attempts(two_peer_state(), vec![Err(terminal_refusal())]).await;
        assert_eq!(times, vec![0, 2]);
        assert_eq!(heads, vec!["conn-1", "conn-2"]);

        let (_, heads) = attempts(two_peer_state(), vec![Err(not_serving_at_open())]).await;
        assert_eq!(
            heads,
            vec!["conn-1", "conn-1"],
            "not serving yet: keep waiting for it"
        );

        let (times, heads) = attempts(test_state(), vec![Err(terminal_refusal())]).await;
        assert_eq!(
            (times, heads),
            (vec![0, 2], vec!["conn-1".into(), "conn-1".into()]),
            "single connector: retry"
        );
    }

    /// Sprint 20 Fix 16 (F46): a Shield whose only Connector was revoked no
    /// longer redials every 2 s forever: after every known Connector refused,
    /// the interval doubles up to TERMINAL_BACKOFF_MAX_SECS.
    #[tokio::test(start_paused = true)]
    async fn a_shield_whose_connectors_are_all_revoked_backs_off() {
        let script: Vec<Result<()>> = (0..10).map(|_| Err(terminal_refusal())).collect();
        let (times, _) = attempts(test_state(), script).await;
        assert_eq!(
            times,
            vec![0, 2, 6, 14, 30, 62, 126, 254, 510, 810, 1110],
            "2, 4, 8 … then capped at 300 s"
        );

        // Two peers: the first walk to the next peer stays fast; once both
        // refused, every further attempt (still alternating) backs off.
        let script: Vec<Result<()>> = (0..5).map(|_| Err(terminal_refusal())).collect();
        let (times, heads) = attempts(two_peer_state(), script).await;
        assert_eq!(times, vec![0, 2, 4, 8, 16, 32]);
        assert_eq!(heads[..4], ["conn-1", "conn-2", "conn-1", "conn-2"]);
    }

    /// A successful stream resets the back-off.
    #[tokio::test(start_paused = true)]
    async fn a_terminal_backoff_resets_after_a_successful_stream() {
        let script = vec![
            Err(terminal_refusal()), // +2
            Err(terminal_refusal()), // +4
            Ok(()),                  // +1 (clean close)
            Err(terminal_refusal()), // +2 again
        ];
        assert_eq!(attempt_times(script).await, vec![0, 2, 6, 7, 9]);
    }

    /// B2 live finding 1: a Connector that is Disabled (Controller outage)
    /// refuses the Shield. The Shield must keep polling every
    /// NOT_SERVING_RETRY_SECS instead of backing off 5/10/20/40 s, so it
    /// re-attaches within 2 s of the Connector serving again.
    #[tokio::test(start_paused = true)]
    async fn connector_not_serving_is_polled_at_a_fixed_short_interval() {
        let script = vec![
            Err(not_serving_mid_stream()), // streams closed (Disabled + 30 s)
            Err(not_serving_at_open()),    // refused while still Disabled
            Err(not_serving_at_open()),
            Err(not_serving_at_open()),
            Err(not_serving_at_open()),
        ];
        assert_eq!(attempt_times(script).await, vec![0, 2, 4, 6, 8, 10]);
    }

    /// Other errors keep the exponential backoff, and it starts from the
    /// initial value after a not-serving period (reset).
    #[tokio::test(start_paused = true)]
    async fn other_errors_keep_exponential_backoff_reset_after_not_serving() {
        let script = vec![
            Err(anyhow::anyhow!("transport error")), // +5
            Err(anyhow::anyhow!("transport error")), // +10
            Err(not_serving_at_open()),              // +2, resets backoff
            Err(anyhow::anyhow!("transport error")), // +5 (not 20)
            Err(anyhow::anyhow!("transport error")), // +10
        ];
        assert_eq!(attempt_times(script).await, vec![0, 5, 15, 17, 22, 32]);
    }

    #[test]
    fn only_the_connectors_not_serving_refusals_are_fast_retried() {
        assert!(connector_not_serving(&not_serving_at_open()));
        assert!(connector_not_serving(&not_serving_mid_stream()));
        assert!(connector_not_serving(&anyhow::Error::new(tonic::Status::unavailable(
            "connector has not yet applied the controller's shield revocation list"
        ))));
        // A different Unavailable (e.g. the Connector process is down).
        assert!(!connector_not_serving(&anyhow::Error::new(tonic::Status::unavailable(
            "transport error"
        ))));
        // Same text, different code.
        assert!(!connector_not_serving(&anyhow::Error::new(
            tonic::Status::permission_denied("connector is not authorized to serve")
        )));
        assert!(!connector_not_serving(&anyhow::anyhow!(
            "connector is not authorized to serve"
        )));
    }

    /// K2: the debounce survives the reconnect that follows a successful
    /// renewal — the reconnected stream's ReEnrolls are ignored until 60 s
    /// after the renewal, then honoured. Paused Tokio time: deterministic.
    #[tokio::test(start_paused = true)]
    async fn debounce_survives_reconnect_for_sixty_seconds() {
        let renewals = Arc::new(std::sync::atomic::AtomicU32::new(0));
        let log = Arc::new(std::sync::Mutex::new(Vec::new()));
        let (done_tx, done_rx) = tokio::sync::oneshot::channel();
        let session = ScriptedSession {
            connection: 0,
            renewals: renewals.clone(),
            log: log.clone(),
            started: tokio::time::Instant::now(),
            done: Some(done_tx),
        };
        let task = tokio::spawn(reconnect_loop(test_state(), session));
        done_rx.await.expect("reached the third connection");
        task.abort();

        assert_eq!(
            *log.lock().unwrap(),
            vec![
                (1, 0, Reenroll::Renewed),
                (2, 1, Reenroll::Ignored),  // right after reconnect
                (2, 58, Reenroll::Ignored), // still inside 60 s
                (2, 60, Reenroll::Renewed), // window over
            ]
        );
        assert_eq!(renewals.load(std::sync::atomic::Ordering::SeqCst), 2);
    }

    /// The K2 scenario: renew, then a duplicate ReEnroll (e.g. on the
    /// reconnected stream) inside 60 s is ignored without a renewal attempt.
    #[tokio::test]
    async fn duplicate_reenroll_after_success_is_ignored() {
        let mut state = test_state();
        let mut debounce = RenewalDebounce::default();
        let calls = std::cell::Cell::new(0);

        let first = handle_reenroll(&mut state, &mut debounce, fake_renew(&calls, true)).await;
        assert!(matches!(first, Some(Ok(()))), "success reconnects");
        assert_eq!(state.cert_not_after, 4_600);

        let second = handle_reenroll(&mut state, &mut debounce, fake_renew(&calls, true)).await;
        assert!(second.is_none(), "debounced ReEnroll keeps the stream");
        assert_eq!(calls.get(), 1, "no second renewal within 60 s");
        assert_eq!(state.cert_not_after, 4_600);
    }

    /// A failed renewal does not start the debounce: the next ReEnroll retries.
    #[tokio::test]
    async fn failed_renewal_does_not_debounce() {
        let mut state = test_state();
        let mut debounce = RenewalDebounce::default();
        let calls = std::cell::Cell::new(0);

        let failed = handle_reenroll(&mut state, &mut debounce, fake_renew(&calls, false)).await;
        assert!(failed.is_none(), "failure keeps the stream");
        assert_eq!(state.cert_not_after, 1_000, "state untouched on failure");

        let retried = handle_reenroll(&mut state, &mut debounce, fake_renew(&calls, true)).await;
        assert!(matches!(retried, Some(Ok(()))));
        assert_eq!(calls.get(), 2, "retry after a failure is not debounced");
    }

    // ── Connector-revocation Phase E (D21) ─────────────────────────────────

    fn peer(id: &str) -> crate::proto::PeerConnector {
        crate::proto::PeerConnector {
            connector_id: id.into(),
            connector_addr: format!("{id}.lan:9091"),
        }
    }

    fn list(peers: &[&str], revoked: &[&str]) -> crate::proto::PeerConnectorList {
        crate::proto::PeerConnectorList {
            peers: peers.iter().map(|p| peer(p)).collect(),
            revoked_connector_ids: revoked.iter().map(|r| r.to_string()).collect(),
        }
    }

    /// A state with peers conn-1, conn-2, conn-3 and a config whose state_dir
    /// is a fresh temp dir holding that state.json.
    fn three_peer_state(label: &str) -> (ShieldState, ShieldConfig) {
        let dir = std::env::temp_dir().join(format!(
            "zecurity-shield-pe-{label}-{}-{}",
            std::process::id(),
            std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        let cfg: ShieldConfig = serde_json::from_value(serde_json::json!({
            "controller_addr": "c:9090",
            "controller_http_addr": "c:8080",
            "state_dir": dir.to_str().unwrap(),
        }))
        .unwrap();
        let mut state = test_state();
        state.connectors = ["conn-1", "conn-2", "conn-3"]
            .iter()
            .map(|id| ConnectorRef {
                connector_id: id.to_string(),
                connector_addr: format!("{id}.lan:9091"),
            })
            .collect();
        state.save(&cfg.state_dir).unwrap();
        (state, cfg)
    }

    fn saved_ids(cfg: &ShieldConfig) -> Vec<String> {
        ShieldState::load(&cfg.state_dir)
            .unwrap()
            .connectors
            .into_iter()
            .map(|c| c.connector_id)
            .collect()
    }

    fn ids(state: &ShieldState) -> Vec<String> {
        state
            .connectors
            .iter()
            .map(|c| c.connector_id.clone())
            .collect()
    }

    /// AE-1: a revoked id prunes the current list at once and is saved,
    /// even when the list carries no peers (sender not in any RN).
    #[test]
    fn revoked_id_prunes_immediately_even_without_peers() {
        let (mut state, cfg) = three_peer_state("prune");
        apply_peer_connector_list(&mut state, &cfg, list(&[], &["conn-2"]));
        assert_eq!(ids(&state), vec!["conn-1", "conn-3"]);
        assert_eq!(saved_ids(&cfg), vec!["conn-1", "conn-3"]);
    }

    /// AE-2: never re-added while the process runs — a later list (from any
    /// connector) that offers the revoked id as a peer and no longer lists
    /// it as revoked still leaves it out.
    #[test]
    fn revoked_connector_is_never_re_added() {
        let (mut state, cfg) = three_peer_state("readd");
        apply_peer_connector_list(&mut state, &cfg, list(&["conn-1", "conn-3"], &["conn-2"]));
        apply_peer_connector_list(&mut state, &cfg, list(&["conn-1", "conn-2", "conn-4"], &[]));
        assert_eq!(ids(&state), vec!["conn-1", "conn-4"]);
        assert!(!saved_ids(&cfg).contains(&"conn-2".to_string()));
    }

    /// AE-3: memory only — the revoked set is not written to state.json and
    /// a reload starts empty.
    #[test]
    fn revoked_set_is_memory_only() {
        let (mut state, cfg) = three_peer_state("memory");
        apply_peer_connector_list(&mut state, &cfg, list(&[], &["conn-2"]));
        let raw = std::fs::read_to_string(Path::new(&cfg.state_dir).join("state.json")).unwrap();
        assert!(!raw.contains("revoked"), "{raw}");
        assert!(ShieldState::load(&cfg.state_dir)
            .unwrap()
            .revoked_connectors
            .is_empty());
    }

    /// AE-4: when every peer is revoked the Shield dials none; state.json
    /// keeps its last non-empty list so the Shield can still start.
    #[tokio::test]
    async fn all_peers_revoked_dials_none_and_keeps_the_file_loadable() {
        let (mut state, cfg) = three_peer_state("all");
        apply_peer_connector_list(&mut state, &cfg, list(&[], &["conn-1", "conn-2", "conn-3"]));
        assert!(state.connectors.is_empty());
        assert_eq!(saved_ids(&cfg), vec!["conn-1", "conn-2", "conn-3"]);
        for f in ["workspace_ca.crt", "shield.crt", "shield.key"] {
            std::fs::write(Path::new(&cfg.state_dir).join(f), b"unused").unwrap();
        }
        let err = build_client(&state, &cfg)
            .await
            .err()
            .expect("nothing to dial");
        assert!(err.to_string().contains("no peer connectors"), "{err:#}");
    }

    /// AE-5: a certificate renewal keeps the in-memory revoked set.
    #[tokio::test]
    async fn renewal_keeps_the_revoked_set() {
        let mut state = test_state();
        state.revoked_connectors.insert("conn-9".into());
        let mut debounce = RenewalDebounce::default();
        let calls = std::cell::Cell::new(0);
        let out = handle_reenroll(&mut state, &mut debounce, |s| {
            calls.set(calls.get() + 1);
            let mut renewed = s;
            renewed.revoked_connectors.clear(); // renew_cert builds a fresh state
            renewed.cert_not_after = 2_000;
            std::future::ready(Ok(renewed))
        })
        .await;
        assert!(matches!(out, Some(Ok(()))));
        assert_eq!(state.cert_not_after, 2_000);
        assert!(state.revoked_connectors.contains("conn-9"));
    }
}
