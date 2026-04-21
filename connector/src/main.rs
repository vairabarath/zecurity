// main.rs — ZECURITY Connector entry point
//
// This is the binary's starting point. It wires together all modules.
//
// Startup flow (Phases 4-7 complete):
//   1. Load config from env vars + /etc/zecurity/connector.conf  (config.rs)
//   2. Initialize structured logging with the configured log level (tracing)
//   3. Log startup info
//   4. Check state.json in state_dir:
//      - Not exists → run enrollment flow           (enrollment.rs, Phase 5)
//      - Exists     → load saved certs/keys
//   5a. Spawn heartbeat loop on mTLS channel        (heartbeat.rs, Phase 6)
//   5b. Spawn auto-updater if enabled               (updater.rs, Phase 7)
//   6. Wait for SIGTERM / Ctrl+C → graceful shutdown
//
// Module layout:
//   appmeta  — SPIFFE identity constants (mirrors Go appmeta.go)
//   config   — figment-based config loading (env + TOML file)
//   proto    — auto-generated gRPC stubs from build.rs (EnrollRequest, ConnectorServiceClient, etc.)

mod appmeta;
pub mod agent_server;
mod config;
mod crypto;
mod enrollment;
mod heartbeat;
mod renewal;
mod tls;
mod updater;
mod util;

/// Generated gRPC client stubs from connector.proto.
///
/// build.rs compiles the proto file via tonic-prost-build.
/// This macro pulls the generated code into the binary.
///
/// Usage in later phases:
///   proto::connector_service_client::ConnectorServiceClient  — gRPC client
///   proto::EnrollRequest, proto::EnrollResponse              — enrollment types
///   proto::HeartbeatRequest, proto::HeartbeatResponse        — heartbeat types
/// Generated Shield gRPC stubs — nested as shield::v1 so the connector.v1
/// generated code can reach them via super::super::shield::v1 from within
/// the connector::v1 module below.
pub mod shield {
    pub mod v1 {
        tonic::include_proto!("shield.v1");
    }
}
/// Alias so existing agent_server.rs code can use `crate::shield_proto::*`.
pub use shield::v1 as shield_proto;

/// Generated connector gRPC stubs — nested as connector::v1 so that the
/// super::super::shield::v1 cross-package paths in generated code resolve
/// correctly (super = connector, super::super = crate root → shield::v1).
pub mod connector {
    pub mod v1 {
        tonic::include_proto!("connector.v1");
    }
}
/// Alias so existing heartbeat.rs / enrollment.rs code can use `proto::*`.
pub use connector::v1 as proto;

use std::fs;
use std::net::SocketAddr;
use std::path::Path;

use anyhow::Context;
use config::ConnectorConfig;
use enrollment::EnrollmentState;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Identity};
use tracing::{error, info};

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    // Install the rustls crypto provider before any TLS operations.
    // Required by rustls 0.23+ — without this, ClientConfig::builder() panics.
    rustls::crypto::ring::default_provider()
        .install_default()
        .expect("failed to install default crypto provider");

    // Handle --check-update flag (used by systemd oneshot update service).
    // Runs a single update check and exits — does not start the full daemon.
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

    // Step 1: Load config. Fails fast if CONTROLLER_ADDR is missing.
    let cfg = ConnectorConfig::load()?;

    // Step 2: Initialize structured logging.
    // Uses cfg.log_level (default "info"). Invalid filter strings fall back to "info".
    let env_filter = tracing_subscriber::EnvFilter::try_new(&cfg.log_level)
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));

    tracing_subscriber::fmt().with_env_filter(env_filter).init();

    // Step 3: Log startup info.
    // PRODUCT_NAME comes from appmeta.rs (mirrors Go's appmeta.ProductName).
    // CARGO_PKG_VERSION is set at compile time from Cargo.toml's version field.
    info!(
        product = appmeta::PRODUCT_NAME,
        version = env!("CARGO_PKG_VERSION"),
        controller_addr = %cfg.controller_addr,
        state_dir = %cfg.state_dir,
        "starting connector"
    );

    // Step 4: Check enrollment state.
    let state_path = Path::new(&cfg.state_dir).join("state.json");

    let enrollment_state: EnrollmentState = if state_path.exists() {
        // Already enrolled — load state and log connector_id.
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
        // First run — perform enrollment.
        info!("no state found — starting enrollment");
        let result = enrollment::enroll(&cfg).await?;
        info!(
            connector_id = %result.connector_id,
            trust_domain = %result.trust_domain,
            "enrollment complete"
        );
        // Load the saved state
        let state_json = fs::read_to_string(&state_path)
            .map_err(|e| anyhow::anyhow!("failed to read {}: {}", state_path.display(), e))?;
        serde_json::from_str(&state_json)
            .map_err(|e| anyhow::anyhow!("failed to parse {}: {}", state_path.display(), e))?
    };

    // Step 5: Build controller channel for ShieldServer (proxies RenewCert to controller).
    let state_dir = Path::new(&cfg.state_dir);
    let cert_pem = fs::read(state_dir.join("connector.crt"))
        .context("failed to read connector.crt for ShieldServer channel")?;
    let key_pem = fs::read(state_dir.join("connector.key"))
        .context("failed to read connector.key for ShieldServer channel")?;
    let ca_pem = fs::read(state_dir.join("workspace_ca.crt"))
        .context("failed to read workspace_ca.crt for ShieldServer channel")?;

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
        .context("failed to connect to controller for ShieldServer")?;

    // Build Shield-facing gRPC server (ShieldServer clones share the same in-memory health map).
    let shield_server = agent_server::ShieldServer::new(
        controller_channel,
        enrollment_state.trust_domain.clone(),
        enrollment_state.connector_id.clone(),
    );

    // Spawn ShieldServer on :9091 — shield agents heartbeat here.
    let shield_serve = shield_server.clone();
    let shield_state_dir = cfg.state_dir.clone();
    let shield_addr: SocketAddr = "0.0.0.0:9091".parse().unwrap();
    tokio::spawn(async move {
        if let Err(e) = shield_serve.serve(shield_addr, &shield_state_dir).await {
            error!(error = %e, "Shield gRPC server on :9091 failed");
        }
    });
    info!("Shield gRPC server starting on :9091");

    // Step 5b: Spawn heartbeat loop — passes shield health map to controller.
    let hb_cfg = cfg.clone();
    let heartbeat_handle = tokio::spawn(async move {
        if let Err(e) = heartbeat::run_heartbeat(&hb_cfg, &enrollment_state, shield_server).await {
            error!(error = %e, "heartbeat loop failed");
        }
    });

    // Step 5b: Spawn auto-updater if enabled.
    let mut updater_handle: Option<tokio::task::JoinHandle<()>> = None;
    if cfg.auto_update_enabled {
        let upd_cfg = cfg.clone();
        updater_handle = Some(tokio::spawn(async move {
            if let Err(e) = updater::run_update_loop(&upd_cfg).await {
                error!(error = %e, "auto-updater failed");
            }
        }));
        info!("auto-updater spawned");
    }

    info!("connector running — press Ctrl+C to shut down");

    // Step 6: Wait for shutdown signal.
    tokio::signal::ctrl_c()
        .await
        .context("failed to wait for Ctrl+C")?;

    info!("shutdown signal received, stopping background tasks");
    heartbeat_handle.abort();
    if let Some(handle) = updater_handle {
        handle.abort();
    }
    info!("connector shut down gracefully");

    Ok(())
}
