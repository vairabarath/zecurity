use std::collections::HashSet;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use futures_util::FutureExt;
use tokio::sync::{mpsc, watch};
// Tokio's clock (identical to std's unless a test pauses time).
use tokio::time::interval;
use tokio::time::Instant;
use tokio_stream::wrappers::ReceiverStream;
use tonic::Request;
use tracing::{debug, error, info, warn};

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
use crate::{crl, peer_list, renewal, resources, tls, util};

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

/// D21-R A′: how long a new Control stream may run before the Shield holds a
/// workspace CRL that is still valid; and how often the Connector's
/// certificate is checked against it.
const CRL_FIRST_WAIT: Duration = Duration::from_secs(30);
const CRL_CHECK_INTERVAL: Duration = Duration::from_secs(5);

/// D21-R A′: the Shield left the Connector itself (its certificate is in the
/// workspace CRL, or no valid CRL arrived). Handled like the Connector's own
/// terminal refusal: fail over to the next peer.
#[derive(Debug)]
struct LeftConnector(&'static str);

const REVOKED_IN_CRL: &str = "its certificate is in the workspace CRL";

impl std::fmt::Display for LeftConnector {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "left the connector: {}", self.0)
    }
}

impl std::error::Error for LeftConnector {}
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

/// Sprint 20 Fix 13 (F39): the live peer list, published for the SIGTERM
/// goodbye. The control stream works on its own copy of the state, so
/// without this the goodbye used the list loaded at startup, which can lead
/// with a revoked or failed connector.
#[derive(Clone)]
pub struct PeerPublisher(Arc<watch::Sender<Vec<ConnectorRef>>>);

impl PeerPublisher {
    pub fn new(state: &ShieldState) -> (Self, watch::Receiver<Vec<ConnectorRef>>) {
        let (tx, rx) = watch::channel(state.connectors.clone());
        (Self(Arc::new(tx)), rx)
    }

    fn publish(&self, state: &ShieldState) {
        self.0.send_if_modified(|live| {
            if *live == state.connectors {
                return false;
            }
            *live = state.connectors.clone();
            true
        });
    }
}

/// The state the goodbye uses: this Shield's identity with the live peer
/// list, never the startup one (F39). An empty list stays empty.
pub fn with_live_peers(
    state: &ShieldState,
    live: &watch::Receiver<Vec<ConnectorRef>>,
) -> ShieldState {
    let mut state = state.clone();
    state.connectors = live.borrow().clone();
    state
}

pub async fn run_control_stream(
    state: ShieldState,
    cfg: ShieldConfig,
    resource_state: Arc<SharedResourceState>,
    peers: PeerPublisher,
) -> Result<()> {
    let crl = Arc::new(crl::CrlStore::load(&cfg.state_dir));
    reconnect_loop(
        state,
        LiveSession {
            cfg,
            resource_state,
            peers: peers.clone(),
            crl,
        },
        peers,
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
    peers: PeerPublisher,
    crl: Arc<crl::CrlStore>,
}

impl ControlSession for LiveSession {
    fn run(
        &mut self,
        state: &mut ShieldState,
        debounce: &mut RenewalDebounce,
    ) -> impl std::future::Future<Output = Result<()>> + Send {
        run_once(
            state,
            &self.cfg,
            &self.resource_state,
            debounce,
            &self.peers,
            &self.crl,
        )
    }
}

/// Run `session` forever, reconnecting after each connection ends. The
/// renewal debounce is created once, here, so it survives the reconnect that
/// follows a successful renewal (K2); a per-connection debounce would let the
/// reconnected stream's first ReEnroll renew again.
async fn reconnect_loop<S: ControlSession>(
    state: ShieldState,
    mut session: S,
    peers: PeerPublisher,
) -> Result<()> {
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
                peers.publish(&current_state);
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
        cause.downcast_ref::<LeftConnector>().is_some()
            || cause.downcast_ref::<tonic::Status>().is_some_and(|status| {
                status.code() == tonic::Code::Unavailable
                    && status.message() == CONNECTOR_TERMINAL_MESSAGE
            })
    })
}

/// D21-R A′: whether the Shield may stay on a Connector whose certificate
/// serial is `serial`, given the workspace CRL and how long the stream has
/// run. Fails closed: no serial, a listed serial, or no valid CRL once the
/// first wait is over.
fn crl_verdict(
    crl: &crl::CrlStore,
    serial: Option<&[u8]>,
    stream_age: Duration,
    now_unix: i64,
) -> std::result::Result<(), LeftConnector> {
    let Some(serial) = serial else {
        return Err(LeftConnector("no verified connector certificate"));
    };
    match crl.check(serial, now_unix) {
        crl::RevocationStatus::NotRevoked => Ok(()),
        crl::RevocationStatus::Revoked => Err(LeftConnector(REVOKED_IN_CRL)),
        crl::RevocationStatus::Unavailable if stream_age < CRL_FIRST_WAIT => Ok(()),
        crl::RevocationStatus::Unavailable => Err(LeftConnector(
            "no workspace CRL valid now (none received, or past its nextUpdate)",
        )),
    }
}

fn check_connector_crl(
    crl: &crl::CrlStore,
    connector_id: &str,
    stream_age: Duration,
) -> std::result::Result<(), LeftConnector> {
    let verdict = crl_verdict(
        crl,
        tls::connector_serial(connector_id).as_deref(),
        stream_age,
        crl::now_unix(),
    );
    if let Err(e) = &verdict {
        warn!(connector_id, reason = e.0, "leaving the connector (D21-R)");
    }
    verdict
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
    peers: &PeerPublisher,
    crl: &Arc<crl::CrlStore>,
) -> Result<()> {
    let (mut client, selected_idx) = build_client(state, cfg)
        .await
        .context("failed to build mTLS client for Control stream")?;

    adopt_failover_head(state, cfg, selected_idx, peers);

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

    // D21-R A′: check the Connector against the workspace CRL now and every
    // CRL_CHECK_INTERVAL. The CRL arrives on this stream; a CRL received
    // from any Connector before is kept.
    let stream_started = Instant::now();
    check_connector_crl(crl, &connector_id, stream_started.elapsed())?;
    let mut crl_ticker = interval(CRL_CHECK_INTERVAL);
    crl_ticker.tick().await;

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
                            &connector_id,
                            crl,
                            state,
                            cfg,
                            resource_state,
                            &out_tx,
                            tunnel_hub.clone(),
                            debounce,
                            peers,
                        ).await {
                            None => {}
                            // The certificate was renewed: move to a stream
                            // with it, keeping the tunnels (Fix 14, D-1 = B).
                            Some(Ok(())) => {
                                if !hand_over(
                                    state,
                                    cfg,
                                    crl,
                                    resource_state,
                                    debounce,
                                    &tunnel_hub,
                                    &connector_id,
                                    &mut client,
                                    &mut out_tx,
                                    &mut inbound,
                                    peers,
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

            _ = crl_ticker.tick() => {
                check_connector_crl(crl, &connector_id, stream_started.elapsed())?;
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
    crl: &crl::CrlStore,
    resource_state: &Arc<SharedResourceState>,
    debounce: &mut RenewalDebounce,
    hub: &TunnelHub,
    connector_id: &str,
    client: &mut ShieldServiceClient<tonic::transport::Channel>,
    out_tx: &mut mpsc::Sender<ShieldControlMessage>,
    inbound: &mut tonic::Streaming<ShieldControlMessage>,
    peers: &PeerPublisher,
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
                        connector_id,
                        crl,
                        state,
                        cfg,
                        resource_state,
                        out_tx,
                        hub.clone(),
                        debounce,
                        peers,
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

/// If we failed over to a non-head Connector, rotate the list so the new
/// head is the surviving Connector, persist it (the next reconnect prefers
/// it) and publish it for the goodbye (F39).
fn adopt_failover_head(
    state: &mut ShieldState,
    cfg: &ShieldConfig,
    selected_idx: usize,
    peers: &PeerPublisher,
) {
    if selected_idx > 0 {
        state.connectors.rotate_left(selected_idx);
        if let Err(e) = state.save(&cfg.state_dir) {
            warn!(error = %e, "failed to persist state.json after connector head rotation");
        }
        peers.publish(state);
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
        Ok(new_state) => {
            debounce.record_success(Instant::now());
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

#[allow(clippy::too_many_arguments)]
async fn handle_connector_msg(
    msg: ShieldControlMessage,
    connector_id: &str,
    crl: &crl::CrlStore,
    state: &mut ShieldState,
    cfg: &ShieldConfig,
    resource_state: &Arc<SharedResourceState>,
    out_tx: &mpsc::Sender<ShieldControlMessage>,
    tunnel_hub: TunnelHub,
    debounce: &mut RenewalDebounce,
    peers: &PeerPublisher,
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
            // D21-R (a2): the signed peer list first, independent of the
            // generation check (identical resources keep their generation).
            if let Some(signed) = &snap.signed_peer_list {
                match peer_list::workspace_ca_key(&cfg.state_dir) {
                    Ok(ca_key) => {
                        apply_signed_peer_list(state, cfg, signed, &ca_key);
                        peers.publish(state);
                    }
                    Err(e) => {
                        warn!(error = %e, "signed peer list not checked: no Workspace CA key")
                    }
                }
            }
            let acks = resources::handle_snapshot(&snap, connector_id, resource_state).await;
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
        // D21-R (a2): peers come only from the controller-signed list in
        // a ResourceSnapshot. A Connector's own list (and its revoked ids) is
        // ignored: it could strand the Shield on a hostile Connector.
        Some(Body::PeerConnectorList(_)) => {
            debug!("ignored unsigned PeerConnectorList");
            None
        }
        // D21-R A′: a workspace CRL from the Connector. Kept only when it
        // verifies and is newer; then the Connector is checked against it
        // at once (it may list its own certificate).
        Some(Body::WorkspaceCrl(c)) => {
            let ca = std::fs::read(Path::new(&cfg.state_dir).join("workspace_ca.crt"))
                .map_err(anyhow::Error::from)
                .and_then(|bundle| crl::crl_issuers(&bundle));
            match ca {
                Ok(issuers) => {
                    if let Err(e) = crl.offer(&c.crl_der, &issuers.workspace_ca_pem) {
                        warn!(error = %e, "rejected workspace CRL from the connector");
                    }
                }
                Err(e) => warn!(error = %e, "workspace CRL not checked: no Workspace CA"),
            }
            let serial = tls::connector_serial(connector_id);
            match serial.map(|serial| crl.check(&serial, crl::now_unix())) {
                Some(crl::RevocationStatus::Revoked) => {
                    let left = LeftConnector(REVOKED_IN_CRL);
                    warn!(
                        connector_id,
                        reason = left.0,
                        "leaving the connector (D21-R)"
                    );
                    Some(Err(anyhow::Error::new(left)))
                }
                _ => None,
            }
        }
        other => {
            warn!(?other, "ignored unsupported Shield Control message");
            None
        }
    }
}

/// D21-R (a2): apply a controller-signed peer list.
///
///   - Rejected (logged, nothing changes) unless it verifies against the
///     Workspace CA key and names this Shield and its trust domain.
///   - Rejected unless issued later than the last accepted list
///     (`peer_list_issued_at_ms`, persisted): a Connector can't roll the
///     Shield back to an older list, across restarts either.
///   - Otherwise it replaces `connectors`. The current head is kept when the
///     list still contains it; otherwise the new head is index 0. Revoked
///     Connectors are simply not in the list.
///   - An empty list empties the in-memory list (the Shield dials nothing)
///     but is not saved: state.json must keep a Connector to start from.
///
/// Returns true when the list was applied.
fn apply_signed_peer_list(
    state: &mut ShieldState,
    cfg: &ShieldConfig,
    signed: &crate::proto::SignedPeerList,
    ca_key: &[u8],
) -> bool {
    let payload = match peer_list::verify(signed, ca_key, &state.trust_domain, &state.shield_id) {
        Ok(payload) => payload,
        Err(e) => {
            warn!(error = %e, "rejected signed peer list");
            return false;
        }
    };
    if payload.issued_at_unix_ms <= state.peer_list_issued_at_ms {
        debug!(
            issued_at = payload.issued_at_unix_ms,
            last = state.peer_list_issued_at_ms,
            "ignored signed peer list: not newer than the last accepted"
        );
        return false;
    }
    let mut new_list: Vec<ConnectorRef> = payload
        .peers
        .into_iter()
        .map(|p| ConnectorRef {
            connector_id: p.connector_id,
            connector_addr: p.connector_addr,
        })
        .collect();
    if let Some(current_head_id) = state.connectors.first().map(|c| c.connector_id.clone()) {
        if let Some(idx) = new_list
            .iter()
            .position(|c| c.connector_id == current_head_id)
        {
            new_list.rotate_left(idx);
        }
    }
    let changed = {
        let mut current_sorted = state.connectors.clone();
        current_sorted.sort_by(|a, b| a.connector_id.cmp(&b.connector_id));
        let mut new_sorted = new_list.clone();
        new_sorted.sort_by(|a, b| a.connector_id.cmp(&b.connector_id));
        current_sorted != new_sorted
    };
    state.peer_list_issued_at_ms = payload.issued_at_unix_ms;
    if new_list.is_empty() {
        // state.json must keep a Connector to start from, so it keeps the
        // last non-empty list, but always with the new issued_at: an older
        // list must stay refused after a restart too.
        state.connectors = new_list;
        warn!("signed peer list is empty: the Shield dials no Connector until a new list arrives");
        let saved = ShieldState::load(&cfg.state_dir).and_then(|mut on_disk| {
            on_disk.peer_list_issued_at_ms = state.peer_list_issued_at_ms;
            on_disk.save(&cfg.state_dir)
        });
        if let Err(e) = saved {
            warn!(error = %e, "failed to persist the peer-list issued_at");
        }
        return true;
    }
    state.connectors = new_list;
    if let Err(e) = state.save(&cfg.state_dir) {
        warn!(error = %e, "failed to persist state.json after peer-list update");
    } else if changed {
        info!(
            peers = state.connectors.len(),
            "peer connector list refreshed (signed)"
        );
    }
    true
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
    if state.connectors.is_empty() {
        info!("no goodbye sent: every peer connector is revoked");
        return;
    }
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
            peer_list_issued_at_ms: 0,
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
        let (peers, _live) = PeerPublisher::new(&state);
        let task = tokio::spawn(reconnect_loop(state, session, peers));
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

    /// Fix 13 (F39, AF13-4): the failover after a terminal refusal is
    /// published at once, so a goodbye during the retry goes to the new head.
    #[tokio::test(start_paused = true)]
    async fn terminal_failover_is_published() {
        let state = two_peer_state();
        let (peers, live) = PeerPublisher::new(&state);
        let (done_tx, done_rx) = tokio::sync::oneshot::channel();
        let session = OutcomeSession {
            script: vec![Err(terminal_refusal())].into(),
            starts: Default::default(),
            heads: Default::default(),
            t0: tokio::time::Instant::now(),
            done: Some(done_tx),
        };
        let task = tokio::spawn(reconnect_loop(state, session, peers));
        tokio::time::sleep(Duration::from_millis(500)).await;
        let ids: Vec<String> = live
            .borrow()
            .iter()
            .map(|c| c.connector_id.clone())
            .collect();
        assert_eq!(ids, vec!["conn-2", "conn-1"], "published before the retry");
        done_rx.await.unwrap();
        task.abort();
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
        let (peers, _live) = PeerPublisher::new(&test_state());
        let task = tokio::spawn(reconnect_loop(test_state(), session, peers));
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

    // ── D21-R (a2): the controller-signed peer list ─────────────────────────

    use ring::rand::SystemRandom;
    use ring::signature::{EcdsaKeyPair, KeyPair as _, ECDSA_P384_SHA384_ASN1_SIGNING};

    /// Stands in for the Workspace CA key (and keeps its PKCS#8 form).
    struct Ca(EcdsaKeyPair, Vec<u8>);

    impl Ca {
        fn new() -> Self {
            let rng = SystemRandom::new();
            let pkcs8 =
                EcdsaKeyPair::generate_pkcs8(&ECDSA_P384_SHA384_ASN1_SIGNING, &rng).unwrap();
            Self(
                EcdsaKeyPair::from_pkcs8(&ECDSA_P384_SHA384_ASN1_SIGNING, pkcs8.as_ref(), &rng)
                    .unwrap(),
                pkcs8.as_ref().to_vec(),
            )
        }

        fn key(&self) -> Vec<u8> {
            self.0.public_key().as_ref().to_vec()
        }

        fn sign_for(
            &self,
            td: &str,
            shield: &str,
            issued_at: u64,
            peers: &[&str],
        ) -> crate::proto::SignedPeerList {
            let payload = crate::proto::PeerListPayload {
                trust_domain: td.into(),
                shield_id: shield.into(),
                issued_at_unix_ms: issued_at,
                peers: peers
                    .iter()
                    .map(|id| crate::proto::PeerConnector {
                        connector_id: id.to_string(),
                        connector_addr: format!("{id}.lan:9091"),
                    })
                    .collect(),
            };
            let payload = prost::Message::encode_to_vec(&payload);
            let mut message = crate::peer_list::PEER_LIST_CONTEXT.to_vec();
            message.extend_from_slice(&payload);
            let signature = self
                .0
                .sign(&SystemRandom::new(), &message)
                .unwrap()
                .as_ref()
                .to_vec();
            crate::proto::SignedPeerList { payload, signature }
        }

        /// A list for test_state()'s shield.
        fn sign(&self, issued_at: u64, peers: &[&str]) -> crate::proto::SignedPeerList {
            self.sign_for("ws-test.zecurity.in", "shield-1", issued_at, peers)
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

    /// A signed list replaces the peers (head kept when still listed) and is
    /// saved with its issued_at; revoked Connectors are simply absent.
    #[test]
    fn a_signed_list_replaces_the_peers_and_is_saved() {
        let ca = Ca::new();
        let (mut state, cfg) = three_peer_state("signed");
        assert!(apply_signed_peer_list(
            &mut state,
            &cfg,
            &ca.sign(10, &["conn-4", "conn-1"]),
            &ca.key()
        ));
        assert_eq!(ids(&state), vec!["conn-1", "conn-4"]);
        assert_eq!(saved_ids(&cfg), vec!["conn-1", "conn-4"]);
        assert_eq!(
            ShieldState::load(&cfg.state_dir)
                .unwrap()
                .peer_list_issued_at_ms,
            10
        );
    }

    /// A Connector can't forge or alter a list: another key, or a payload
    /// changed after signing, is rejected and nothing changes.
    #[test]
    fn a_forged_or_altered_list_is_rejected() {
        let ca = Ca::new();
        let (mut state, cfg) = three_peer_state("forged");
        let forged = Ca::new().sign(10, &["hostile"]);
        assert!(!apply_signed_peer_list(
            &mut state,
            &cfg,
            &forged,
            &ca.key()
        ));
        let mut altered = ca.sign(10, &["conn-1"]);
        let mut payload: crate::proto::PeerListPayload =
            prost::Message::decode(altered.payload.as_slice()).unwrap();
        payload.peers[0].connector_id = "hostile".into();
        altered.payload = prost::Message::encode_to_vec(&payload);
        assert!(!apply_signed_peer_list(
            &mut state,
            &cfg,
            &altered,
            &ca.key()
        ));
        assert_eq!(ids(&state), vec!["conn-1", "conn-2", "conn-3"]);
        assert_eq!(state.peer_list_issued_at_ms, 0);
    }

    /// A genuine list for another Shield or workspace is not this Shield's.
    #[test]
    fn a_list_for_another_shield_or_workspace_is_rejected() {
        let ca = Ca::new();
        let (mut state, cfg) = three_peer_state("bound");
        let other_shield = ca.sign_for("ws-test.zecurity.in", "shield-2", 10, &["hostile"]);
        let other_ws = ca.sign_for("ws-other.zecurity.in", "shield-1", 10, &["hostile"]);
        assert!(!apply_signed_peer_list(
            &mut state,
            &cfg,
            &other_shield,
            &ca.key()
        ));
        assert!(!apply_signed_peer_list(
            &mut state,
            &cfg,
            &other_ws,
            &ca.key()
        ));
        assert_eq!(ids(&state), vec!["conn-1", "conn-2", "conn-3"]);
    }

    /// A Connector can't roll the Shield back to an older (or the same)
    /// list, also after a restart: issued_at is persisted.
    #[test]
    fn an_older_list_is_rejected_across_restarts() {
        let ca = Ca::new();
        let (mut state, cfg) = three_peer_state("rollback");
        let old = ca.sign(10, &["conn-1", "conn-2", "conn-3", "hostile"]);
        assert!(apply_signed_peer_list(
            &mut state,
            &cfg,
            &ca.sign(20, &["conn-1"]),
            &ca.key()
        ));
        assert!(!apply_signed_peer_list(&mut state, &cfg, &old, &ca.key()));
        assert!(!apply_signed_peer_list(
            &mut state,
            &cfg,
            &ca.sign(20, &["hostile"]),
            &ca.key()
        ));
        let mut reloaded = ShieldState::load(&cfg.state_dir).unwrap();
        assert!(!apply_signed_peer_list(
            &mut reloaded,
            &cfg,
            &old,
            &ca.key()
        ));
        assert_eq!(ids(&reloaded), vec!["conn-1"]);
    }

    fn live_ids(live: &watch::Receiver<Vec<ConnectorRef>>) -> Vec<String> {
        live.borrow()
            .iter()
            .map(|c| c.connector_id.clone())
            .collect()
    }

    async fn deliver(
        state: &mut ShieldState,
        cfg: &ShieldConfig,
        peers: &PeerPublisher,
        body: Body,
    ) {
        let (out_tx, _out_rx) = mpsc::channel(8);
        let action = handle_connector_msg(
            ShieldControlMessage { body: Some(body) },
            "conn-1",
            &crl::CrlStore::default(),
            state,
            cfg,
            &Arc::new(SharedResourceState::new()),
            &out_tx,
            tunnel::new_hub(),
            &mut RenewalDebounce::default(),
            peers,
        )
        .await;
        assert!(action.is_none());
    }

    /// D21-R: a Connector's own PeerConnectorList no longer changes the
    /// peers, whatever it lists as peers or revoked (it could strand the
    /// Shield on a hostile Connector). Rewrites Phase E AE-1..AE-3 and
    /// Fix 13 F41, which pinned the old behaviour.
    #[tokio::test]
    async fn an_unsigned_peer_list_is_ignored() {
        let (mut state, cfg) = three_peer_state("unsigned");
        let (peers, live) = PeerPublisher::new(&state);
        deliver(
            &mut state,
            &cfg,
            &peers,
            Body::PeerConnectorList(crate::proto::PeerConnectorList {
                peers: vec![crate::proto::PeerConnector {
                    connector_id: "hostile".into(),
                    connector_addr: "hostile.lan:9091".into(),
                }],
                revoked_connector_ids: vec!["conn-1".into(), "conn-2".into(), "conn-3".into()],
            }),
        )
        .await;
        assert_eq!(ids(&state), vec!["conn-1", "conn-2", "conn-3"]);
        assert_eq!(saved_ids(&cfg), vec!["conn-1", "conn-2", "conn-3"]);
        assert_eq!(live_ids(&live), vec!["conn-1", "conn-2", "conn-3"]);
    }

    /// Fix 13 (F39, AF13-4): a signed list arriving in a snapshot reaches
    /// the live list the goodbye reads. The key is read from the stored
    /// workspace_ca.crt, chosen by role.
    #[tokio::test]
    async fn a_signed_list_in_a_snapshot_is_applied_and_published() {
        use rcgen::{
            BasicConstraints, CertificateParams, CertifiedIssuer, DistinguishedName, DnType, IsCa,
            KeyPair, KeyUsagePurpose, PKCS_ECDSA_P256_SHA256,
        };
        let ca_params = |cn: &str, c: BasicConstraints| {
            let mut p = CertificateParams::default();
            let mut dn = DistinguishedName::new();
            dn.push(DnType::CommonName, cn);
            p.distinguished_name = dn;
            p.is_ca = IsCa::Ca(c);
            p.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
            p
        };
        let root = CertifiedIssuer::self_signed(
            ca_params("Platform Root", BasicConstraints::Constrained(2)),
            KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256).unwrap(),
        )
        .unwrap();
        let intermediate = CertifiedIssuer::signed_by(
            ca_params("Platform Intermediate", BasicConstraints::Constrained(1)),
            KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256).unwrap(),
            &root,
        )
        .unwrap();
        let ca = Ca::new();
        let ws_key = KeyPair::from_pkcs8_der_and_sign_algo(
            &rustls::pki_types::PrivatePkcs8KeyDer::from(ca.1.clone()),
            &rcgen::PKCS_ECDSA_P384_SHA384,
        )
        .unwrap();
        let workspace = CertifiedIssuer::signed_by(
            ca_params("workspace-ws-test", BasicConstraints::Constrained(0)),
            ws_key,
            &intermediate,
        )
        .unwrap();

        let (mut state, cfg) = three_peer_state("f39-msg");
        std::fs::write(
            Path::new(&cfg.state_dir).join("workspace_ca.crt"),
            format!("{}{}", workspace.pem(), intermediate.pem()),
        )
        .unwrap();
        let (peers, live) = PeerPublisher::new(&state);
        deliver(
            &mut state,
            &cfg,
            &peers,
            Body::ResourceSnapshot(crate::proto::ResourceSnapshot {
                resources: vec![],
                generation: 0, // stale for the generation check: the list still applies
                signed_peer_list: Some(ca.sign(10, &["conn-1", "conn-3"])),
            }),
        )
        .await;
        assert_eq!(ids(&state), vec!["conn-1", "conn-3"]);
        assert_eq!(live_ids(&live), vec!["conn-1", "conn-3"]);
    }

    /// Fix 13 (F39, AF13-4): a failover at connect time is saved and
    /// published.
    #[test]
    fn a_failover_at_connect_is_published() {
        let (mut state, cfg) = three_peer_state("f39-failover");
        let (peers, live) = PeerPublisher::new(&state);
        adopt_failover_head(&mut state, &cfg, 0, &peers);
        assert_eq!(live_ids(&live), vec!["conn-1", "conn-2", "conn-3"]);
        adopt_failover_head(&mut state, &cfg, 2, &peers);
        assert_eq!(live_ids(&live), vec!["conn-3", "conn-1", "conn-2"]);
        assert_eq!(saved_ids(&cfg), vec!["conn-3", "conn-1", "conn-2"]);
    }

    /// Fix 13 (F39, AF13-5): an empty signed list → the goodbye gets the
    /// empty live list, never the startup one, and sends nothing.
    #[tokio::test]
    async fn goodbye_uses_the_live_list_even_when_empty() {
        let ca = Ca::new();
        let (mut state, cfg) = three_peer_state("f39-empty");
        let startup = state.clone();
        let (peers, live) = PeerPublisher::new(&state);
        assert!(apply_signed_peer_list(
            &mut state,
            &cfg,
            &ca.sign(10, &[]),
            &ca.key()
        ));
        peers.publish(&state);

        let bye = with_live_peers(&startup, &live);
        assert!(bye.connectors.is_empty(), "not the startup list");
        assert_eq!(bye.shield_id, startup.shield_id);
        goodbye(&bye, &cfg).await; // returns without dialling (no cert files here)
    }

    /// An empty signed list (every Connector revoked): the Shield dials
    /// none; state.json keeps its last non-empty list so the Shield can
    /// still start.
    #[tokio::test]
    async fn an_empty_signed_list_dials_none_and_keeps_the_file_loadable() {
        let ca = Ca::new();
        let (mut state, cfg) = three_peer_state("all");
        assert!(apply_signed_peer_list(
            &mut state,
            &cfg,
            &ca.sign(10, &[]),
            &ca.key()
        ));
        assert!(state.connectors.is_empty());
        assert_eq!(saved_ids(&cfg), vec!["conn-1", "conn-2", "conn-3"]);
        // The issued_at is saved anyway: an older list stays refused after a
        // restart.
        let mut reloaded = ShieldState::load(&cfg.state_dir).unwrap();
        assert_eq!(reloaded.peer_list_issued_at_ms, 10);
        assert!(!apply_signed_peer_list(
            &mut reloaded,
            &cfg,
            &ca.sign(9, &["hostile"]),
            &ca.key()
        ));
        for f in ["workspace_ca.crt", "shield.crt", "shield.key"] {
            std::fs::write(Path::new(&cfg.state_dir).join(f), b"unused").unwrap();
        }
        let err = build_client(&state, &cfg)
            .await
            .err()
            .expect("nothing to dial");
        assert!(err.to_string().contains("no peer connectors"), "{err:#}");
    }

    // ── D21-R A′: the workspace CRL at the Shield ──────────────────────────

    #[test]
    fn the_crl_verdict_fails_closed() {
        use crate::crl::tests::{Pki, CONNECTOR_SERIAL};
        let pki = Pki::new();
        let store = crl::CrlStore::default();
        let now = crl::now_unix();
        let young = CRL_FIRST_WAIT - Duration::from_secs(1);
        // No CRL yet: allowed only during the first wait.
        assert!(crl_verdict(&store, Some(CONNECTOR_SERIAL), young, now).is_ok());
        assert!(crl_verdict(&store, Some(CONNECTOR_SERIAL), CRL_FIRST_WAIT, now).is_err());
        // No verified certificate: never.
        assert!(crl_verdict(&store, None, Duration::ZERO, now).is_err());
        // Listed: leave at once, even during the first wait.
        let crl = pki.crl(
            &[CONNECTOR_SERIAL],
            ::time::Duration::minutes(1),
            ::time::Duration::hours(24),
        );
        store.offer(&crl, &pki.workspace_pem()).unwrap();
        assert!(crl_verdict(&store, Some(CONNECTOR_SERIAL), Duration::ZERO, now).is_err());
        assert!(crl_verdict(&store, Some(&[0x01]), CRL_FIRST_WAIT * 10, now).is_ok());
        // Past nextUpdate: leave.
        assert!(crl_verdict(&store, Some(&[0x01]), CRL_FIRST_WAIT * 10, now + 24 * 3600).is_err());
    }

    /// Leaving the Connector takes the terminal path: fail over to the next
    /// peer, as for the Connector's own "revoked" refusal.
    #[test]
    fn leaving_the_connector_is_terminal() {
        let err = anyhow::Error::new(LeftConnector(REVOKED_IN_CRL)).context("control stream");
        assert!(connector_terminal(&err));
        assert!(!connector_not_serving(&err));
    }

    async fn from(
        store: &crl::CrlStore,
        connector: &str,
        state: &mut ShieldState,
        cfg: &ShieldConfig,
        peers: &PeerPublisher,
        msg: ShieldControlMessage,
    ) -> Option<Result<()>> {
        let (out_tx, _out_rx) = mpsc::channel(8);
        handle_connector_msg(
            msg,
            connector,
            store,
            state,
            cfg,
            &Arc::new(SharedResourceState::new()),
            &out_tx,
            tunnel::new_hub(),
            &mut RenewalDebounce::default(),
            peers,
        )
        .await
    }

    /// A CRL that lists the attached Connector ends the stream at once.
    #[tokio::test]
    async fn a_crl_listing_the_connector_ends_the_stream() {
        use crate::crl::tests::{Pki, CONNECTOR_SERIAL};
        let pki = Pki::new();
        let (mut state, cfg) = three_peer_state("crl-msg");
        std::fs::write(
            Path::new(&cfg.state_dir).join("workspace_ca.crt"),
            pki.bundle(),
        )
        .unwrap();
        tls::set_connector_serial_for_test("conn-crl-a", CONNECTOR_SERIAL);
        tls::set_connector_serial_for_test("conn-crl-b", &[0x01]);
        let store = crl::CrlStore::load(&cfg.state_dir);
        let (peers, _live) = PeerPublisher::new(&state);
        let crl_msg = || ShieldControlMessage {
            body: Some(Body::WorkspaceCrl(crate::proto::WorkspaceCrl {
                crl_der: pki.crl(
                    &[CONNECTOR_SERIAL],
                    ::time::Duration::minutes(1),
                    ::time::Duration::hours(24),
                ),
            })),
        };
        assert!(
            from(&store, "conn-crl-b", &mut state, &cfg, &peers, crl_msg())
                .await
                .is_none(),
            "not listed: stays"
        );
        match from(&store, "conn-crl-a", &mut state, &cfg, &peers, crl_msg()).await {
            Some(Err(e)) => assert!(connector_terminal(&e), "{e:#}"),
            other => panic!("expected to leave, got {other:?}"),
        }
    }
}
