// main.rs — ZECURITY Connector entry point
//
// Startup flow:
//   1. Load config from env vars + /etc/zecurity/connector.conf  (config.rs)
//   2. Initialize structured logging with the configured log level (tracing)
//   3. Log startup info
//   4. Check state.json in state_dir:
//      - Not exists → run enrollment flow           (enrollment.rs)
//      - Exists     → load saved certs/keys
//   5a. Build ShieldRegistry and spawn shield-facing gRPC server on :9091
//   5b. Spawn auto-updater if enabled               (updater.rs)
//   6. Run bidirectional Control stream to controller (control_stream.rs)
//      — blocks with inner reconnect loop until process shutdown

pub mod agent_server;
mod appmeta;
pub mod discovery;
mod config;
mod control_stream;
mod controller_client;
mod crypto;
mod enrollment;
mod renewal;
mod tls;
mod updater;
mod util;

/// Generated gRPC client stubs from connector.proto.
pub mod shield {
    pub mod v1 {
        tonic::include_proto!("shield.v1");
    }
}

/// Generated client.v1 message types — used for ACLSnapshot referenced in connector.proto.
pub mod client {
    pub mod v1 {
        tonic::include_proto!("client.v1");
    }
}
/// Alias so existing agent_server.rs code can use `crate::shield_proto::*`.
pub use shield::v1 as shield_proto;

/// Generated connector gRPC stubs.
pub mod connector {
    pub mod v1 {
        tonic::include_proto!("connector.v1");
    }
}
/// Alias so connector modules can use `proto::*`.
pub use connector::v1 as proto;

use std::fs;
use std::net::SocketAddr;
use std::path::Path;

use anyhow::Context;
use config::ConnectorConfig;
use enrollment::EnrollmentState;
use tokio::sync::mpsc;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Identity};
use tracing::{error, info};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    rustls::crypto::ring::default_provider()
        .install_default()
        .expect("failed to install default crypto provider");

    if std::env::args().any(|a| a == "--check-update") {
        tracing_subscriber::fmt()
            .with_env_filter(tracing_subscriber::EnvFilter::new("info"))
            .init();
        info!(
            version = env!("CARGO_PKG_VERSION"),
            "running single update check"
        );
        return updater::run_single_check().await;
    }

    let cfg = ConnectorConfig::load()?;

    let env_filter = tracing_subscriber::EnvFilter::try_new(&cfg.log_level)
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    tracing_subscriber::fmt().with_env_filter(env_filter).init();

    info!(
        product = appmeta::PRODUCT_NAME,
        version = env!("CARGO_PKG_VERSION"),
        controller_addr = %cfg.controller_addr,
        state_dir = %cfg.state_dir,
        "starting connector"
    );

    let state_path = Path::new(&cfg.state_dir).join("state.json");

    let enrollment_state: EnrollmentState = if state_path.exists() {
        let state_json = fs::read_to_string(&state_path)
            .map_err(|e| anyhow::anyhow!("failed to read {}: {}", state_path.display(), e))?;
        let state: EnrollmentState = serde_json::from_str(&state_json)
            .map_err(|e| anyhow::anyhow!("failed to parse {}: {}", state_path.display(), e))?;
        info!(
            connector_id = %state.connector_id,
            trust_domain = %state.trust_domain,
            workspace_id = %state.workspace_id,
            enrolled_at = %state.enrolled_at,
            "connector already enrolled"
        );
        state
    } else {
        info!("no state found — starting enrollment");
        let result = enrollment::enroll(&cfg).await?;
        info!(
            connector_id = %result.connector_id,
            trust_domain = %result.trust_domain,
            "enrollment complete"
        );
        let state_json = fs::read_to_string(&state_path)
            .map_err(|e| anyhow::anyhow!("failed to read {}: {}", state_path.display(), e))?;
        serde_json::from_str(&state_json)
            .map_err(|e| anyhow::anyhow!("failed to parse {}: {}", state_path.display(), e))?
    };

    // Build controller channel for ShieldRegistry (proxies RenewCert to controller).
    let state_dir = Path::new(&cfg.state_dir);
    let cert_pem = fs::read(state_dir.join("connector.crt"))
        .context("failed to read connector.crt for ShieldRegistry channel")?;
    let key_pem = fs::read(state_dir.join("connector.key"))
        .context("failed to read connector.key for ShieldRegistry channel")?;
    let ca_pem = fs::read(state_dir.join("workspace_ca.crt"))
        .context("failed to read workspace_ca.crt for ShieldRegistry channel")?;

    let tls = ClientTlsConfig::new()
        .identity(Identity::from_pem(&cert_pem, &key_pem))
        .ca_certificate(Certificate::from_pem(&ca_pem));

    let grpc_addr = format!("https://{}", cfg.controller_addr);
    let controller_channel = Channel::from_shared(grpc_addr)
        .context("invalid controller gRPC address")?
        .tls_config(tls)
        .context("failed to configure TLS for controller channel")?
        .connect()
        .await
        .context("failed to connect to controller for ShieldRegistry")?;

    // Create ack channel shared between ShieldRegistry (producers) and control_stream (consumer).
    let (ack_tx, ack_rx) = mpsc::channel(128);

    let shield_registry = agent_server::ShieldRegistry::new(
        controller_channel,
        enrollment_state.trust_domain.clone(),
        enrollment_state.connector_id.clone(),
        ack_tx,
    );

    // Spawn shield-facing gRPC server on :9091.
    let reg_for_serve = shield_registry.clone();
    let shield_state_dir = cfg.state_dir.clone();
    let shield_addr: SocketAddr = "0.0.0.0:9091".parse().unwrap();
    tokio::spawn(async move {
        if let Err(e) = reg_for_serve.serve(shield_addr, &shield_state_dir).await {
            error!(error = %e, "Shield gRPC server on :9091 failed");
        }
    });
    info!("Shield gRPC server starting on :9091");

    // Spawn auto-updater if enabled.
    if cfg.auto_update_enabled {
        let upd_cfg = cfg.clone();
        tokio::spawn(async move {
            if let Err(e) = updater::run_update_loop(&upd_cfg).await {
                error!(error = %e, "auto-updater failed");
            }
        });
        info!("auto-updater spawned");
    }

    info!("connector running — entering Control stream loop");

    // Run bidirectional Control stream to controller (blocks with reconnect loop).
    control_stream::run_control_stream(&cfg, &enrollment_state, shield_registry, ack_rx).await
}
