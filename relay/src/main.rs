mod appmeta;
mod cert_manager;
mod config;
mod crl;
mod csr;
mod heartbeat;
mod listener;
mod protocol;
mod provision;
mod renewal;
mod session;
mod spiffe;
mod state;
#[cfg(test)]
mod test_support;
mod tls;

pub mod relay {
    pub mod v1 {
        tonic::include_proto!("relay.v1");
    }
}

use anyhow::Result;
use config::RelayConfig;
use tracing::info;

#[tokio::main]
async fn main() -> Result<()> {
    rustls::crypto::ring::default_provider()
        .install_default()
        .expect("failed to install default crypto provider");

    let cfg = RelayConfig::load()?;
    let env_filter = tracing_subscriber::EnvFilter::try_new(&cfg.log_level)
        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info"));
    tracing_subscriber::fmt().with_env_filter(env_filter).init();

    info!(
        relay_id = %cfg.relay_id,
        controller_addr = %cfg.controller_addr,
        state_dir = %cfg.state_dir,
        "starting Relay"
    );

    let material = provision::ensure_provisioned(&cfg).await?;
    // Single owner of the current certificate: listener, heartbeat and the
    // renewal scheduler all read from and subscribe to it (Phase F-2).
    let certs = cert_manager::CertManager::load(
        &cfg.relay_id,
        cert_manager::CertPaths {
            key_path: material.key_path.clone(),
            certificate_path: material.certificate_path.clone(),
            intermediate_ca_path: material.intermediate_ca_path.clone(),
        },
    )?;
    info!(
        certificate = %material.certificate_path.display(),
        intermediate_ca = %material.intermediate_ca_path.display(),
        serial = %certs.current().serial_hex,
        bind_addr = %cfg.bind_addr,
        "Relay provisioned; starting multi-workspace mTLS QUIC listener"
    );

    let state = state::RelayState::new();
    tokio::spawn(heartbeat::run(cfg.clone(), certs.clone(), state.clone()));
    renewal::spawn(certs.clone(), &cfg)?;
    let crl = crl::WorkspaceCrlManager::new(cfg.controller_http_addr.clone());
    crl.clone().spawn_refresh(60, 15);
    listener::run_listener(cfg.bind_addr, certs, state, cfg.runtime_limits, crl).await
}
