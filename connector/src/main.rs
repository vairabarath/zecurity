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
pub mod agent_tunnel;
mod appmeta;
pub mod crl;
pub mod discovery;
mod config;
mod control_stream;
mod controller_client;
mod crypto;
pub mod device_tunnel;
mod enrollment;
pub mod net_util;
pub mod policy;
pub mod quic_listener;
mod renewal;
pub mod tls;
mod updater;
mod util;
mod watchdog;

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

/// Type alias used by quic_listener.rs and device_tunnel.rs.
/// Maps the spec name to the real ShieldRegistry type.
pub type AgentRegistry = agent_server::ShieldRegistry;

/// Type alias used by device_tunnel.rs for the control stream message type.
pub type ControlMessage = proto::ConnectorControlMessage;

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

use std::sync::Arc;

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

    let policy_cache = Arc::new(policy::PolicyCache::new());

    // Load cert/key material for the device tunnel TLS/QUIC listeners.
    let cert_store = tls::cert_store::CertStore::load(&cfg.state_dir)
        .context("failed to load cert store for device tunnel")?;

    // Determine LAN IP for QUIC advertise address.
    let lan_ip = net_util::lan_ip()
        .map(|ip| ip.to_string())
        .unwrap_or_default();
    let quic_advertise = format!("{}:9092", lan_ip);

    let acl = policy_cache.clone();
    let tunnel_hub = shield_registry.tunnel_hub.clone();
    let agent_registry = Arc::new(shield_registry.clone());
    let connector_id = enrollment_state.connector_id.clone();

    // Build CRL URL from controller HTTP address (fallback: derive host from gRPC addr + port 8080).
    let http_base = cfg.controller_http_addr.clone().unwrap_or_else(|| {
        let host = cfg.controller_addr
            .split(':')
            .next()
            .unwrap_or("localhost")
            .to_string();
        format!("http://{}:8080", host)
    });
    let crl_url = format!("{}/ca.crl?workspace_id={}", http_base, enrollment_state.workspace_id);

    let crl_manager = crl::CrlManager::new();
    if let Err(e) = crl_manager.refresh(&crl_url).await {
        tracing::warn!("initial CRL fetch failed (using empty cache): {e}");
    }
    crl_manager.clone().spawn_refresh(crl_url, 300);

    // Control message channel for device_tunnel → control_stream (emits access logs).
    let (ctrl_tx, _ctrl_rx) = tokio::sync::mpsc::channel::<ControlMessage>(128);

    // Spawn TLS/TCP device tunnel listener on :9092 (M4 implements; stub for now).
    {
        let store       = cert_store.clone();
        let acl         = acl.clone();
        let hub         = tunnel_hub.clone();
        let reg         = agent_registry.clone();
        let crl         = crl_manager.clone();
        let cid         = connector_id.clone();
        let tx          = ctrl_tx.clone();
        tokio::spawn(async move {
            if let Err(e) = device_tunnel::listen("0.0.0.0:9092", store, acl, hub, reg, crl, cid, tx).await {
                error!(error = %e, "device tunnel (TLS) on :9092 failed");
            }
        });
    }

    // Spawn QUIC/UDP device tunnel listener on :9092.
    {
        let store       = cert_store.clone();
        let acl         = acl.clone();
        let hub         = tunnel_hub.clone();
        let reg         = agent_registry.clone();
        let crl         = crl_manager.clone();
        let cid         = connector_id.clone();
        let tx          = ctrl_tx.clone();
        tokio::spawn(async move {
            if let Err(e) = quic_listener::listen(
                "0.0.0.0:9092", &quic_advertise,
                store, acl, hub, reg, crl, cid, tx,
            ).await {
                error!(error = %e, "device tunnel (QUIC) on :9092 failed");
            }
        });
    }

    info!("device tunnel listeners spawned on :9092 (TLS+QUIC)");

    watchdog::notify_ready();
    watchdog::spawn_watchdog();

    // Run bidirectional Control stream to controller (blocks with reconnect loop).
    control_stream::run_control_stream(&cfg, &enrollment_state, shield_registry, ack_rx, policy_cache).await
}
