mod appmeta;
mod config;
mod csr;
mod listener;
mod protocol;
mod provision;
mod session;
mod spiffe;
mod state;
mod tls;

pub mod relay {
    pub mod v1 {
        tonic::include_proto!("relay.v1");
    }
}

use anyhow::Result;
use config::RelayConfig;
use std::fs;
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
    let relay_certificate = fs::read(&material.certificate_path)?;
    let relay_key = fs::read(&material.key_path)?;
    let intermediate_ca = fs::read(&material.intermediate_ca_path)?;
    let server_config = tls::build_server_config(
        &relay_certificate,
        &relay_key,
        &intermediate_ca,
        &cfg.relay_id,
    )?;
    info!(
        certificate = %material.certificate_path.display(),
        intermediate_ca = %material.intermediate_ca_path.display(),
        bind_addr = %cfg.bind_addr,
        "Relay provisioned; starting multi-workspace mTLS QUIC listener"
    );

    listener::run_listener(cfg.bind_addr, server_config, state::RelayState::new()).await
}
