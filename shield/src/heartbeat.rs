// heartbeat.rs — mTLS heartbeat loop for the ZECURITY Shield
//
// After enrollment, the shield heartbeats to its assigned Connector :9091.
// No pre-flight raw-TLS step (unlike connector→controller pattern) because
// SpiffeConnectorVerifier is embedded directly in the tonic channel — SPIFFE
// verification happens on every (re)connect automatically.
//
// Loop:
//   1. Build mTLS channel to connector_addr :9091 via SpiffeConnectorVerifier
//   2. Send HeartbeatRequest every shield_heartbeat_interval_secs
//      - Drains pending ResourceAcks from SharedResourceState into the request
//   3. On ok=true: reset backoff
//      - Process resp.resources: validate host, apply/remove nftables, push acks
//      - If re_enroll=true → call renewal::renew_cert() and rebuild channel
//   4. On error: exponential backoff (5s → 10s → 20s → 40s → 60s cap)
//
// goodbye(): best-effort Goodbye RPC sent on SIGTERM so the connector marks
// the shield DISCONNECTED immediately rather than waiting for the threshold.

use std::path::Path;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::Result;
use tokio::time::{interval, sleep};
use tracing::{error, info, warn};

use crate::config::ShieldConfig;
use crate::proto;
use crate::resources::{self, ActiveResource, SharedResourceState};
use crate::tls;
use crate::types::ShieldState;
use crate::util;

const BACKOFF_INITIAL_SECS: u64 = 5;
const BACKOFF_MAX_SECS: u64 = 60;

pub async fn run(
    state: ShieldState,
    cfg: ShieldConfig,
    resource_state: Arc<SharedResourceState>,
) -> Result<()> {
    let mut current_state = state;
    let mut client = build_client(&current_state, &cfg).await?;

    let hostname = util::read_hostname();
    let public_ip = util::get_public_ip().await.unwrap_or_default();
    let lan_ip = util::detect_lan_ip().unwrap_or_default();
    let version = env!("CARGO_PKG_VERSION").to_string();
    let interval_secs = cfg.shield_heartbeat_interval_secs;

    info!(
        shield_id = %current_state.shield_id,
        connector_addr = %current_state.connector_addr,
        interval_secs,
        "entering heartbeat loop"
    );

    let mut backoff_secs = BACKOFF_INITIAL_SECS;
    let mut hb_interval = interval(Duration::from_secs(interval_secs));

    loop {
        hb_interval.tick().await;

        let pending_acks = resource_state.acks.lock().unwrap().drain(..).collect::<Vec<_>>();

        let req = tonic::Request::new(proto::HeartbeatRequest {
            shield_id: current_state.shield_id.clone(),
            version: version.clone(),
            hostname: hostname.clone(),
            public_ip: public_ip.clone(),
            lan_ip: lan_ip.clone(),
            resource_acks: pending_acks,
        });

        match client.heartbeat(req).await {
            Ok(resp) => {
                let resp = resp.into_inner();
                backoff_secs = BACKOFF_INITIAL_SECS;

                info!(shield_id = %current_state.shield_id, "heartbeat ok");

                process_resource_instructions(&resp.resources, &resource_state).await;

                if resp.re_enroll {
                    info!(shield_id = %current_state.shield_id, "connector requested cert renewal");
                    match crate::renewal::renew_cert(&current_state, &cfg).await {
                        Ok(new_state) => {
                            info!(
                                shield_id = %new_state.shield_id,
                                new_expiry = new_state.cert_not_after,
                                "cert renewed — rebuilding mTLS channel"
                            );
                            current_state = new_state;
                            match build_client(&current_state, &cfg).await {
                                Ok(new_client) => client = new_client,
                                Err(e) => error!(error = %e, "failed to rebuild channel after renewal"),
                            }
                        }
                        Err(e) => error!(error = %e, "cert renewal failed"),
                    }
                }
            }
            Err(e) => {
                warn!(
                    error = %e,
                    backoff_secs,
                    "heartbeat failed, retrying with backoff"
                );
                sleep(Duration::from_secs(backoff_secs)).await;
                backoff_secs = (backoff_secs * 2).min(BACKOFF_MAX_SECS);
            }
        }
    }
}

async fn process_resource_instructions(
    instructions: &[proto::ResourceInstruction],
    state: &Arc<SharedResourceState>,
) {
    for instruction in instructions {
        match instruction.action.as_str() {
            "apply" => handle_apply(instruction, state).await,
            "remove" => handle_remove(instruction, state).await,
            other => warn!(action = other, resource_id = %instruction.resource_id, "unknown resource action"),
        }
    }
}

async fn handle_apply(instruction: &proto::ResourceInstruction, state: &Arc<SharedResourceState>) {
    let now = now_unix();

    if !resources::validate_host(&instruction.host) {
        warn!(
            resource_id = %instruction.resource_id,
            host = %instruction.host,
            "resource host does not match this shield's LAN IP — rejecting"
        );
        push_ack(state, proto::ResourceAck {
            resource_id: instruction.resource_id.clone(),
            status: "failed".to_string(),
            error: "resource host does not match this shield's IP".to_string(),
            verified_at: now,
            port_reachable: false,
        });
        return;
    }

    {
        let mut active = state.active.lock().unwrap();
        // Replace if already present, otherwise push.
        if let Some(existing) = active.iter_mut().find(|r| r.resource_id == instruction.resource_id) {
            existing.protocol  = instruction.protocol.clone();
            existing.port_from = instruction.port_from as u16;
            existing.port_to   = instruction.port_to as u16;
        } else {
            active.push(ActiveResource {
                resource_id: instruction.resource_id.clone(),
                protocol:    instruction.protocol.clone(),
                port_from:   instruction.port_from as u16,
                port_to:     instruction.port_to as u16,
            });
        }
    }

    let snapshot = state.active.lock().unwrap().clone();
    match resources::apply_nftables(&snapshot).await {
        Ok(()) => {
            let reachable = resources::check_port(instruction.port_from as u16);
            info!(
                resource_id = %instruction.resource_id,
                port = instruction.port_from,
                port_reachable = reachable,
                "resource applied — nftables chain rebuilt"
            );
            push_ack(state, proto::ResourceAck {
                resource_id: instruction.resource_id.clone(),
                status: "protecting".to_string(),
                error: String::new(),
                verified_at: now,
                port_reachable: reachable,
            });
        }
        Err(e) => {
            error!(resource_id = %instruction.resource_id, error = %e, "nftables apply failed");
            push_ack(state, proto::ResourceAck {
                resource_id: instruction.resource_id.clone(),
                status: "failed".to_string(),
                error: e.to_string(),
                verified_at: now,
                port_reachable: false,
            });
        }
    }
}

async fn handle_remove(instruction: &proto::ResourceInstruction, state: &Arc<SharedResourceState>) {
    state.active.lock().unwrap()
        .retain(|r| r.resource_id != instruction.resource_id);

    let snapshot = state.active.lock().unwrap().clone();
    if let Err(e) = resources::apply_nftables(&snapshot).await {
        error!(resource_id = %instruction.resource_id, error = %e, "nftables rebuild after remove failed");
    }

    info!(resource_id = %instruction.resource_id, "resource removed from nftables");
    push_ack(state, proto::ResourceAck {
        resource_id: instruction.resource_id.clone(),
        status: "removed".to_string(),
        error: String::new(),
        verified_at: now_unix(),
        port_reachable: false,
    });
}

fn push_ack(state: &Arc<SharedResourceState>, ack: proto::ResourceAck) {
    let mut acks = state.acks.lock().unwrap();
    acks.retain(|a| a.resource_id != ack.resource_id);
    acks.push(ack);
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .unwrap_or_default()
        .as_secs() as i64
}

/// Best-effort Goodbye RPC on SIGTERM — lets connector mark us DISCONNECTED
/// immediately rather than waiting for SHIELD_DISCONNECT_THRESHOLD.
pub async fn goodbye(state: &ShieldState, cfg: &ShieldConfig) {
    match build_client(state, cfg).await {
        Ok(mut client) => {
            let req = tonic::Request::new(proto::GoodbyeRequest {
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

async fn build_client(
    state: &ShieldState,
    cfg: &ShieldConfig,
) -> Result<proto::shield_service_client::ShieldServiceClient<tonic::transport::Channel>> {
    let state_dir = Path::new(&cfg.state_dir);
    let ca_pem   = tokio::fs::read(state_dir.join("workspace_ca.crt")).await?;
    let cert_pem = tokio::fs::read(state_dir.join("shield.crt")).await?;
    let key_pem  = tokio::fs::read(state_dir.join("shield.key")).await?;

    let channel = tls::build_connector_channel(
        &ca_pem,
        &cert_pem,
        &key_pem,
        &state.connector_id,
        &state.trust_domain,
        &state.connector_addr,
    )
    .await?;

    Ok(proto::shield_service_client::ShieldServiceClient::new(channel))
}
