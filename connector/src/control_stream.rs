use std::time::Duration;

use anyhow::{Context, Result};
use tokio::sync::mpsc;
use tokio::time::interval;
use tokio_stream::wrappers::ReceiverStream;
use tonic::Request;
use tracing::{error, info, warn};

use crate::agent_server::ShieldRegistry;
use crate::config::ConnectorConfig;
use crate::controller_client::{
    build_channel, fetch_public_ip, verify_controller_spiffe_preflight,
};
use crate::discovery::scan::{execute_scan, ScanCommand};
use crate::enrollment::EnrollmentState;
use crate::proto::connector_control_message::Body as CBody;
use crate::proto::{
    connector_service_client::ConnectorServiceClient, ConnectorControlMessage,
    ConnectorHealthReport, ResourceAckBatch, ScanReport as ProtoScanReport,
    ScanResult as ProtoScanResult,
};
use crate::renewal;
use crate::shield_proto::ResourceAck;
use crate::util;

const BACKOFF_INITIAL_SECS: u64 = 2;
const BACKOFF_MAX_SECS: u64 = 60;
const HEALTH_INTERVAL_SECS: u64 = 15;
const DISCOVERY_FLUSH_SECS: u64 = 5;

/// Outer reconnect loop. Blocks indefinitely — run via tokio::spawn or await directly from main.
pub async fn run_control_stream(
    cfg: &ConnectorConfig,
    state: &EnrollmentState,
    shield_registry: ShieldRegistry,
    mut ack_rx: mpsc::Receiver<(String, ResourceAck)>,
) -> Result<()> {
    let hostname = util::read_hostname();
    let public_ip = fetch_public_ip().await;
    let version = env!("CARGO_PKG_VERSION").to_string();
    let lan_addr = cfg.lan_addr.clone().unwrap_or_else(|| {
        util::detect_lan_ip()
            .map(|ip| format!("{}:9091", ip))
            .unwrap_or_default()
    });

    if cfg.lan_addr.is_some() {
        info!(lan_addr = %lan_addr, "using configured CONNECTOR_LAN_ADDR");
    } else if !lan_addr.is_empty() {
        info!(lan_addr = %lan_addr, "auto-detected LAN address");
    } else {
        warn!(
            "could not detect LAN address — shields on the same network may be unable to connect"
        );
    }

    let mut current_state = state.clone();
    let mut backoff_secs = BACKOFF_INITIAL_SECS;

    loop {
        match run_once(
            cfg,
            &mut current_state,
            &shield_registry,
            &mut ack_rx,
            &lan_addr,
            &hostname,
            &version,
            &public_ip,
        )
        .await
        {
            Ok(()) => {
                backoff_secs = BACKOFF_INITIAL_SECS;
                info!("control stream closed cleanly, reconnecting");
                tokio::time::sleep(Duration::from_secs(1)).await;
            }
            Err(e) => {
                error!(
                    error = %e,
                    backoff_secs = backoff_secs,
                    "control stream error, reconnecting with backoff"
                );
                tokio::time::sleep(Duration::from_secs(backoff_secs)).await;
                backoff_secs = (backoff_secs * 2).min(BACKOFF_MAX_SECS);
            }
        }
    }
}

async fn run_once(
    cfg: &ConnectorConfig,
    state: &mut EnrollmentState,
    shield_registry: &ShieldRegistry,
    ack_rx: &mut mpsc::Receiver<(String, ResourceAck)>,
    lan_addr: &str,
    hostname: &str,
    version: &str,
    public_ip: &str,
) -> Result<()> {
    info!("starting mTLS SPIFFE preflight check");
    verify_controller_spiffe_preflight(cfg)
        .await
        .context("controller SPIFFE preflight failed")?;
    info!("controller SPIFFE identity verified — opening Control stream");

    let channel = build_channel(cfg)
        .await
        .context("failed to build mTLS channel for Control stream")?;
    let mut client = ConnectorServiceClient::new(channel);

    // Outbound: connector → controller (buffered mpsc → ReceiverStream)
    let (out_tx, out_rx) = mpsc::channel::<ConnectorControlMessage>(64);
    let outbound = ReceiverStream::new(out_rx);
    let response = client
        .control(Request::new(outbound))
        .await
        .context("failed to open Control stream")?;
    let mut inbound = response.into_inner();

    info!(
        connector_id = %state.connector_id,
        version = version,
        "Control stream established — sending initial health report"
    );

    // Send initial ConnectorHealthReport on connect.
    out_tx
        .send(ConnectorControlMessage {
            body: Some(CBody::ConnectorHealth(ConnectorHealthReport {
                version: version.to_string(),
                hostname: hostname.to_string(),
                public_ip: public_ip.to_string(),
                lan_addr: lan_addr.to_string(),
            })),
        })
        .await
        .context("failed to send initial health report")?;

    let mut health_ticker = interval(Duration::from_secs(HEALTH_INTERVAL_SECS));
    // Consume the immediate first tick so we don't double-send health on connect.
    health_ticker.tick().await;

    let mut discovery_ticker = interval(Duration::from_secs(DISCOVERY_FLUSH_SECS));
    discovery_ticker.tick().await; // skip first tick

    loop {
        tokio::select! {
            result = inbound.message() => {
                match result {
                    Err(e) => return Err(anyhow::anyhow!("control stream recv error: {}", e)),
                    Ok(None) => {
                        info!("controller closed Control stream");
                        return Ok(());
                    }
                    Ok(Some(msg)) => {
                        if let Some(action) = handle_controller_msg(msg, shield_registry, state, cfg, &out_tx).await {
                            return action;
                        }
                    }
                }
            }

            Some((_, ack)) = ack_rx.recv() => {
                if out_tx.send(ConnectorControlMessage {
                    body: Some(CBody::ResourceAcks(ResourceAckBatch { acks: vec![ack] })),
                }).await.is_err() {
                    return Err(anyhow::anyhow!("outbound channel closed"));
                }
            }

            _ = health_ticker.tick() => {
                info!("health tick — sending ConnectorHealthReport");
                // Drain any pending acks into a batch alongside the health tick.
                let mut acks = Vec::new();
                while let Ok((_, ack)) = ack_rx.try_recv() {
                    acks.push(ack);
                }
                if !acks.is_empty() {
                    if out_tx.send(ConnectorControlMessage {
                        body: Some(CBody::ResourceAcks(ResourceAckBatch { acks })),
                    }).await.is_err() {
                        return Err(anyhow::anyhow!("outbound channel closed"));
                    }
                }

                if out_tx.send(ConnectorControlMessage {
                    body: Some(CBody::ConnectorHealth(ConnectorHealthReport {
                        version: version.to_string(),
                        hostname: hostname.to_string(),
                        public_ip: public_ip.to_string(),
                        lan_addr: lan_addr.to_string(),
                    })),
                }).await.is_err() {
                    return Err(anyhow::anyhow!("outbound channel closed"));
                }

                let status = shield_registry.get_shield_status_batch();
                if !status.shields.is_empty() {
                    if out_tx.send(ConnectorControlMessage {
                        body: Some(CBody::ShieldStatus(status)),
                    }).await.is_err() {
                        return Err(anyhow::anyhow!("outbound channel closed"));
                    }
                }
            }

            _ = discovery_ticker.tick() => {
                if let Some(batch_msg) = shield_registry.drain_discovery_batch() {
                    let report_count = match &batch_msg.body {
                        Some(crate::proto::connector_control_message::Body::ShieldDiscovery(b)) => b.reports.len(),
                        _ => 0,
                    };
                    info!(report_count, "flushing ShieldDiscoveryBatch upstream");
                    if out_tx.send(batch_msg).await.is_err() {
                        return Err(anyhow::anyhow!("outbound channel closed sending discovery batch"));
                    }
                }
            }
        }
    }
}

/// Returns Some(Err) on fatal error, Some(Ok(())) to reconnect (e.g. after renewal), None to continue.
async fn handle_controller_msg(
    msg: ConnectorControlMessage,
    shield_registry: &ShieldRegistry,
    state: &mut EnrollmentState,
    cfg: &ConnectorConfig,
    out_tx: &mpsc::Sender<ConnectorControlMessage>,
) -> Option<Result<()>> {
    match msg.body {
        Some(CBody::ResourceInstructions(batch)) => {
            for (shield_id, instr_batch) in batch.shield_resources {
                shield_registry.push_instructions(&shield_id, instr_batch.instructions);
            }
            None
        }
        Some(CBody::ScanCommand(cmd)) => {
            let connector_id = shield_registry.connector_id().to_string();
            let out_tx = out_tx.clone();
            tokio::spawn(async move {
                let scan_cmd = ScanCommand {
                    request_id:  cmd.request_id.clone(),
                    targets:     cmd.targets,
                    ports:       cmd.ports.into_iter().map(|p| p as u16).collect(),
                    max_targets: cmd.max_targets,
                    timeout_sec: cmd.timeout_sec as u64,
                };
                let report = execute_scan(scan_cmd, &connector_id).await;
                let proto_results: Vec<ProtoScanResult> = report.results.into_iter().map(|r| ProtoScanResult {
                    ip:              r.ip,
                    port:            r.port as u32,
                    protocol:        r.protocol,
                    service_name:    r.service_name,
                    reachable_from:  r.reachable_from,
                    first_seen:      r.first_seen,
                }).collect();
                let proto_report = ConnectorControlMessage {
                    body: Some(CBody::ScanReport(ProtoScanReport {
                        request_id: report.request_id,
                        results:    proto_results,
                        error:      report.error.unwrap_or_default(),
                    })),
                };
                let _ = out_tx.send(proto_report).await;
            });
            None
        }
        Some(CBody::Ping(p)) => {
            let pong = ConnectorControlMessage {
                body: Some(CBody::Pong(crate::shield_proto::Pong {
                    timestamp_unix: p.timestamp_unix,
                })),
            };
            if out_tx.send(pong).await.is_err() {
                return Some(Err(anyhow::anyhow!("outbound channel closed on pong")));
            }
            None
        }
        Some(CBody::ReEnroll(_)) => {
            info!("controller requested cert renewal — starting renewal");
            match renewal::renew_cert(state, cfg).await {
                Ok(new_state) => {
                    info!(
                        "cert renewed successfully, new expiry: {}",
                        new_state.cert_not_after
                    );
                    *state = new_state;
                    // Break the inner loop to reconnect with the fresh cert.
                    Some(Ok(()))
                }
                Err(e) => {
                    error!(error = %e, "cert renewal failed");
                    None
                }
            }
        }
        _ => None,
    }
}
