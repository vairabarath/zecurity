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
use crate::types::ShieldState;
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
                error!(
                    error = %e,
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
    let mut client = build_client(state, cfg)
        .await
        .context("failed to build mTLS client for Control stream")?;

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
        connector_addr = %state.connector_addr,
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
        other => {
            warn!(?other, "ignored unsupported Shield Control message");
            None
        }
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

async fn build_client(
    state: &ShieldState,
    cfg: &ShieldConfig,
) -> Result<ShieldServiceClient<tonic::transport::Channel>> {
    let state_dir = Path::new(&cfg.state_dir);
    let ca_pem = tokio::fs::read(state_dir.join("workspace_ca.crt")).await?;
    let cert_pem = tokio::fs::read(state_dir.join("shield.crt")).await?;
    let key_pem = tokio::fs::read(state_dir.join("shield.key")).await?;

    let channel = tls::build_connector_channel(
        &ca_pem,
        &cert_pem,
        &key_pem,
        &state.connector_id,
        &state.trust_domain,
        &state.connector_addr,
    )
    .await?;

    Ok(ShieldServiceClient::new(channel))
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
        shield_id:   diff.shield_id,
        seq,
        fingerprint: diff.fingerprint,
        full_sync,
        added: diff
            .added
            .into_iter()
            .map(|s| ProtoDiscoveredService {
                protocol:     s.protocol.to_string(),
                port:         s.port as u32,
                bound_ip:     s.bound_ip,
                service_name: s.service_name,
            })
            .collect(),
        removed: diff
            .removed
            .into_iter()
            .map(|(port, proto)| ProtoDiscoveredService {
                protocol:     proto,
                port:         port as u32,
                bound_ip:     String::new(),
                service_name: String::new(),
            })
            .collect(),
    };

    let msg = ShieldControlMessage {
        body: Some(Body::DiscoveryReport(proto_report)),
    };

    match out_tx.send(msg).await {
        Ok(()) => info!(seq, added = added_len, removed = removed_len, full_sync, "discovery: report sent"),
        Err(e) => warn!("discovery: failed to send report: {}", e),
    }
}

/// Best-effort Goodbye RPC on SIGTERM so the connector removes this shield
/// from its in-memory health map immediately.
pub async fn goodbye(state: &ShieldState, cfg: &ShieldConfig) {
    match build_client(state, cfg).await {
        Ok(mut client) => {
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
