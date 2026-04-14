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
mod config;
mod crypto;
mod enrollment;
mod heartbeat;
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
pub mod proto {
    tonic::include_proto!("connector");
}

use std::fs;
use std::path::Path;

use anyhow::Context;
use config::ConnectorConfig;
use enrollment::EnrollmentState;
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

    let connector_id: String = if state_path.exists() {
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
        state.connector_id
    } else {
        // First run — perform enrollment.
        info!("no state found — starting enrollment");
        let result = enrollment::enroll(&cfg).await?;
        info!(
            connector_id = %result.connector_id,
            trust_domain = %result.trust_domain,
            "enrollment complete"
        );
        result.connector_id
    };

    // Step 5: Spawn heartbeat loop on mTLS channel.
    let hb_cfg = cfg.clone();
    let hb_id = connector_id.clone();
    let heartbeat_handle = tokio::spawn(async move {
        if let Err(e) = heartbeat::run_heartbeat(&hb_cfg, &hb_id).await {
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
