// main.rs — ZECURITY Shield entry point
//
// WHAT THE SHIELD DOES:
//   The shield is a lightweight agent that runs on any resource host
//   (a server, VM, or device you want to protect). It:
//     1. Enrolls with the controller to get a SPIFFE certificate
//     2. Sets up a TUN interface (zecurity0) for Zero Trust routing
//     3. Sends heartbeats to its assigned connector every N seconds
//     4. Renews its certificate before it expires
//     5. Optionally auto-updates itself from GitHub releases
//
// STARTUP FLOW:
//   1. Install rustls crypto provider (required by rustls 0.23+)
//   2. Load config from env vars (CONTROLLER_ADDR, ENROLLMENT_TOKEN, etc.)
//   3. Initialize structured logging
//   4. Check state.json in state_dir:
//      - Not exists → first run → call enrollment::enroll()
//      - Exists     → already enrolled → load ShieldState from state.json
//   5. Spawn heartbeat loop (sends heartbeats to connector :9091 via mTLS)
//   6. Spawn auto-updater if AUTO_UPDATE_ENABLED=true
//   7. Wait for SIGTERM (systemd sends this on `systemctl stop`)
//   8. On SIGTERM: call heartbeat::goodbye() so connector marks us offline immediately
//
// MODULE LAYOUT:
//   appmeta    — SPIFFE/PKI constants (mirrors Go appmeta/identity.go)
//   config     — figment config loader (env vars → ShieldConfig)
//   crypto     — EC P-384 keygen, CSR builder, PEM/DER helpers
//   tls        — connector SPIFFE verification for mTLS handshake
//   util       — hostname reader, public IP helper
//   enrollment — (Phase I) full enrollment flow with controller
//   heartbeat  — (Phase J) mTLS heartbeat loop to connector :9091
//   renewal    — (Phase J) cert renewal via connector RenewCert RPC
//   network    — (Phase K) zecurity0 TUN interface + nftables setup
//   updater    — (Phase L) GitHub release checker + binary self-update

mod appmeta;
mod config;
mod crypto;
mod enrollment;
mod tls;
mod types;
mod util;

// These modules are implemented in later phases.
// Declared here so main.rs compiles now and wires them in as they land.
// mod heartbeat;  // Phase J
// mod renewal;    // Phase J
// mod network;    // Phase K
// mod updater;    // Phase L

/// Generated gRPC client stubs from proto/shield/v1/shield.proto.
///
/// build.rs compiles the proto via tonic_prost_build and writes the
/// generated Rust code to OUT_DIR. This macro pulls it into the binary.
///
/// Available types after Phase I:
///   proto::shield_service_client::ShieldServiceClient — gRPC client
///   proto::EnrollRequest / EnrollResponse
///   proto::HeartbeatRequest / HeartbeatResponse
///   proto::RenewCertRequest / RenewCertResponse
///   proto::GoodbyeRequest / GoodbyeResponse
pub mod proto {
    tonic::include_proto!("shield.v1");
}

use std::path::Path;

use anyhow::Context;
use tracing::info;
use types::ShieldState;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    // Step 1: Install the rustls crypto provider.
    //
    // rustls 0.23+ requires an explicit crypto backend to be installed before
    // any TLS operations. We use the `ring` backend (same as connector).
    // Without this, ClientConfig::builder() panics at runtime.
    rustls::crypto::ring::default_provider()
        .install_default()
        .expect("failed to install rustls ring crypto provider");

    // Step 2: Load configuration from environment variables.
    //
    // Fails fast if CONTROLLER_ADDR or CONTROLLER_HTTP_ADDR is missing.
    // The systemd unit's EnvironmentFile= injects these from /etc/zecurity/shield.conf.
    let cfg = config::ShieldConfig::load()?;

    // Step 3: Initialize structured logging.
    //
    // tracing_subscriber reads cfg.log_level (e.g. "info", "debug").
    // All subsequent tracing::info!() / tracing::error!() calls go through this.
    let env_filter = tracing_subscriber::EnvFilter::try_new(&cfg.log_level)
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    tracing_subscriber::fmt().with_env_filter(env_filter).init();

    info!(
        product  = appmeta::PRODUCT_NAME,
        version  = env!("CARGO_PKG_VERSION"),
        controller_addr = %cfg.controller_addr,
        state_dir = %cfg.state_dir,
        "starting shield"
    );

    // Step 4: Check enrollment state.
    //
    // state.json exists  → already enrolled, load state and resume
    // state.json missing → first run, perform enrollment
    let state_path = Path::new(&cfg.state_dir).join("state.json");

    let _state: ShieldState = if state_path.exists() {
        // Already enrolled — load state from disk
        let state = ShieldState::load(&cfg.state_dir)?;
        info!(
            shield_id    = %state.shield_id,
            connector_id = %state.connector_id,
            trust_domain = %state.trust_domain,
            interface_addr = %state.interface_addr,
            "shield already enrolled, resuming"
        );
        state
    } else {
        // First run — enrollment flow (Phase I)
        //
        // enrollment::enroll() will:
        //   1. Fetch CA cert from controller HTTP endpoint
        //   2. Verify CA fingerprint against enrollment token
        //   3. Generate EC P-384 keypair + CSR
        //   4. Call controller Enroll RPC (plain TLS)
        //   5. Save cert, key, CA chain, and state.json to state_dir
        //   6. Call network::setup() to create zecurity0 interface
        info!("no state.json found — starting enrollment");
        enrollment::enroll(&cfg).await?
    };

    // Step 5: Spawn heartbeat loop (Phase J)
    //
    // heartbeat::run() connects to connector_addr:9091 via mTLS,
    // sends HeartbeatRequest every shield_heartbeat_interval_secs,
    // and calls renewal::renew_cert() when HeartbeatResponse.re_enroll=true.
    //
    // TODO: uncomment when Phase J (heartbeat.rs) is implemented
    // let hb_cfg = cfg.clone();
    // let hb_state = _state.clone();
    // let heartbeat_handle = tokio::spawn(async move {
    //     if let Err(e) = heartbeat::run(&hb_state, &hb_cfg).await {
    //         error!(error = %e, "heartbeat loop failed");
    //     }
    // });

    // Step 6: Spawn auto-updater if enabled (Phase L)
    //
    // updater::run() checks GitHub releases on a weekly timer and
    // replaces /usr/local/bin/zecurity-shield if a newer version exists.
    //
    // TODO: uncomment when Phase L (updater.rs) is implemented
    // if cfg.auto_update_enabled {
    //     let upd_cfg = cfg.clone();
    //     tokio::spawn(async move {
    //         if let Err(e) = updater::run(&upd_cfg).await {
    //             error!(error = %e, "auto-updater failed");
    //         }
    //     });
    //     info!("auto-updater spawned");
    // }

    info!("shield running — waiting for SIGTERM");

    // Step 7: Wait for SIGTERM (sent by systemd on `systemctl stop zecurity-shield`)
    //
    // We use tokio::signal::unix for SIGTERM (not just Ctrl+C) because
    // systemd sends SIGTERM, not SIGINT. Both are handled here.
    #[cfg(unix)]
    {
        use tokio::signal::unix::{signal, SignalKind};
        let mut sigterm = signal(SignalKind::terminate())
            .context("failed to register SIGTERM handler")?;
        tokio::select! {
            _ = sigterm.recv() => { info!("received SIGTERM"); }
            _ = tokio::signal::ctrl_c() => { info!("received Ctrl+C"); }
        }
    }
    #[cfg(not(unix))]
    {
        tokio::signal::ctrl_c()
            .await
            .context("failed to wait for Ctrl+C")?;
    }

    // Step 8: Graceful shutdown
    //
    // Send Goodbye RPC to connector so it marks us DISCONNECTED immediately
    // rather than waiting for the disconnect threshold timeout.
    // This is best-effort — if it fails, the connector will detect absence
    // of heartbeats and mark us disconnected after SHIELD_DISCONNECT_THRESHOLD.
    //
    // TODO: uncomment when Phase J (heartbeat.rs) is implemented
    // if let Err(e) = heartbeat::goodbye(&_state, &cfg).await {
    //     error!(error = %e, "goodbye RPC failed (non-fatal)");
    // }

    info!("shield shut down gracefully");
    Ok(())
}
