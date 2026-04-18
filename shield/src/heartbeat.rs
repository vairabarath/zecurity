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
//   3. On ok=true: reset backoff; if re_enroll=true → call renewal::renew_cert()
//      and rebuild the channel with the renewed cert
//   4. On error: exponential backoff (5s → 10s → 20s → 40s → 60s cap)
//
// goodbye(): best-effort Goodbye RPC sent on SIGTERM so the connector marks
// the shield DISCONNECTED immediately rather than waiting for the threshold.

use std::path::Path;
use std::time::Duration;

use anyhow::Result;
use tokio::time::{interval, sleep};
use tracing::{error, info, warn};

use crate::config::ShieldConfig;
use crate::proto;
use crate::tls;
use crate::types::ShieldState;
use crate::util;

const BACKOFF_INITIAL_SECS: u64 = 5;
const BACKOFF_MAX_SECS: u64 = 60;

pub async fn run(state: ShieldState, cfg: ShieldConfig) -> Result<()> {
    let mut current_state = state;
    let mut client = build_client(&current_state, &cfg).await?;

    let hostname = util::read_hostname();
    let public_ip = util::get_public_ip().await.unwrap_or_default();
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

        let req = tonic::Request::new(proto::HeartbeatRequest {
            shield_id: current_state.shield_id.clone(),
            version: version.clone(),
            hostname: hostname.clone(),
            public_ip: public_ip.clone(),
        });

        match client.heartbeat(req).await {
            Ok(resp) => {
                let resp = resp.into_inner();
                backoff_secs = BACKOFF_INITIAL_SECS;

                info!(shield_id = %current_state.shield_id, "heartbeat ok");

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
