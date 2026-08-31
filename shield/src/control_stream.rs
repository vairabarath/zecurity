use std::collections::HashSet;
use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use futures_util::FutureExt;
use tokio::sync::mpsc;
use tokio::time::interval;
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
const HEALTH_INTERVAL_SECS: u64 = 15;

pub async fn run_control_stream(
    state: ShieldState,
    cfg: ShieldConfig,
    resource_state: Arc<SharedResourceState>,
) -> Result<()> {
    let mut current_state = state;
    let mut backoff_secs = BACKOFF_INITIAL_SECS;

    loop {
        match run_once(&mut current_state, &cfg, &resource_state).await {
            Ok(()) => {
                backoff_secs = BACKOFF_INITIAL_SECS;
                info!("control stream closed cleanly, reconnecting");
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            Err(e) => {
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

async fn run_once(
    state: &mut ShieldState,
    cfg: &ShieldConfig,
    resource_state: &Arc<SharedResourceState>,
) -> Result<()> {
    // Try the peers we know. If EVERY one is unreachable, fall back to the
    // controller for fresh coordinates and try once more — see recover_peers.
    // The fallback lives here rather than inside build_client on purpose:
    // renewal.rs calls build_client too, and a renewal attempt should not drag a
    // controller round-trip along on every failure.
    let (mut client, selected_idx) = match build_client(state, cfg).await {
        Ok(v) => v,
        Err(first_err) => {
            warn!(
                error = %first_err,
                "every known peer Connector is unreachable — asking the controller for current coordinates"
            );
            recover_peers(state, cfg).await?;
            build_client(state, cfg)
                .await
                .context("failed to build mTLS client for Control stream after peer recovery")?
        }
    };

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

    let (out_tx, out_rx) = mpsc::channel::<ShieldControlMessage>(32);
    let response = client
        .control(Request::new(ReceiverStream::new(out_rx)))
        .await
        .context("failed to open Shield Control stream")?;
    let mut inbound = response.into_inner();

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
                    Err(e) => return Err(anyhow::anyhow!("control stream recv error: {}", e)),
                    Ok(None) => return Ok(()),
                    Ok(Some(msg)) => {
                        if let Some(action) = handle_connector_msg(
                            msg,
                            state,
                            cfg,
                            resource_state,
                            &out_tx,
                            tunnel_hub.clone(),
                        ).await {
                            return action;
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

async fn handle_connector_msg(
    msg: ShieldControlMessage,
    state: &mut ShieldState,
    cfg: &ShieldConfig,
    resource_state: &Arc<SharedResourceState>,
    out_tx: &mpsc::Sender<ShieldControlMessage>,
    tunnel_hub: TunnelHub,
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
            info!(shield_id = %state.shield_id, "connector requested cert renewal");
            match renewal::renew_cert(state, cfg).await {
                Ok(new_state) => {
                    info!(
                        shield_id = %new_state.shield_id,
                        new_expiry = new_state.cert_not_after,
                        "cert renewed — reconnecting Control stream"
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
                // Identity from the message; the ADDRESS comes from resource_state.
                open.resource_id,
                resource_state.clone(),
                out_tx.clone(),
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
            apply_peer_connector_list(state, cfg, list.peers);
            None
        }
        other => {
            warn!(?other, "ignored unsupported Shield Control message");
            None
        }
    }
}

/// Ask the CONTROLLER for this shield's current peer connectors, and adopt them.
///
/// This exists because of a specific unrecoverable state. A shield learns peer
/// coordinates ONLY from `PeerConnectorList` pushes over the Control stream, so
/// when its connector's address changes, the only channel carrying the new
/// address is the peer whose address just changed. The shield retries the dead
/// address forever, while `connectors.lan_addr` on the controller is already
/// correct. Recovery used to mean re-adding the old IP to an interface so the
/// shield could reconnect once and be told the truth.
///
/// It is deliberately NOT on the happy path: it runs only after every known peer
/// has failed. Steady state remains "Shield talks to its Connector, never the
/// Controller" — this is the escape hatch for when that rule strands the shield.
///
/// The controller identifies us from our client certificate alone; the request
/// carries no fields, so there is nothing here for a caller to forge.
///
/// NOTE ON SCOPE: this does not close the recovery-path gap in general, it moves
/// it up one level. A shield whose own certificate has expired cannot open this
/// mTLS channel either, and still needs manual re-enrolment.
async fn recover_peers(state: &mut ShieldState, cfg: &ShieldConfig) -> Result<()> {
    let state_dir = Path::new(&cfg.state_dir);
    let ca_pem = tokio::fs::read(state_dir.join("workspace_ca.crt")).await?;
    let cert_pem = tokio::fs::read(state_dir.join("shield.crt")).await?;
    let key_pem = tokio::fs::read(state_dir.join("shield.key")).await?;

    let channel =
        tls::build_controller_channel(&ca_pem, &cert_pem, &key_pem, &cfg.controller_addr)
            .await
            .context("peer recovery: could not reach the controller")?;

    let peers = ShieldServiceClient::new(channel)
        .get_peer_connectors(Request::new(crate::proto::GetPeerConnectorsRequest {}))
        .await
        .context("peer recovery: GetPeerConnectors failed")?
        .into_inner()
        .peers;

    // The controller returns an error rather than an empty list precisely so this
    // cannot silently no-op: apply_peer_connector_list IGNORES an empty list, so
    // an empty success would leave us retrying the same dead addresses believing
    // we had recovered.
    if peers.is_empty() {
        anyhow::bail!("peer recovery: controller returned no peer connectors");
    }

    info!(
        peers = peers.len(),
        "peer recovery: got current connector coordinates from the controller"
    );
    apply_peer_connector_list(state, cfg, peers);
    Ok(())
}

/// Apply an incoming `PeerConnectorList` to the Shield's persistent state.
///
///   - Empty `peers` is ignored — the Shield's existing list is safer than a
///     truncated one.
///   - Identical content (order-insensitive by connector_id) is a no-op —
///     no rewrite of state.json.
///   - New content replaces the list. The current head is preserved when
///     the new list still contains it; otherwise the new head is index 0.
fn apply_peer_connector_list(
    state: &mut ShieldState,
    cfg: &ShieldConfig,
    peers: Vec<crate::proto::PeerConnector>,
) {
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
