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
mod workspace_policy;

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
    // Relay pools PR 3 (D-40): the persisted workspace policy (none on a
    // first start: workspace peers are refused until the first heartbeat
    // applies one). The lease is also checked without heartbeats.
    let policy = workspace_policy::WorkspacePolicy::load(
        std::path::Path::new(&cfg.state_dir),
        workspace_policy::binding_for(&cfg.relay_id, &certs.current().intermediate_ca_pem),
    );
    policy
        .clone()
        .spawn_lease_watch(workspace_policy::LEASE_CHECK_PERIOD);
    // heartbeat::run returns only when the controller reports this relay
    // revoked (terminal); the listener then closes every connection (F-8).
    let (revoked_tx, revoked_rx) = tokio::sync::oneshot::channel::<()>();
    let heartbeat = heartbeat::run(cfg.clone(), certs.clone(), state.clone(), policy.clone());
    tokio::spawn(async move {
        heartbeat.await;
        let _ = revoked_tx.send(());
    });
    renewal::spawn(certs.clone(), &cfg)?;
    let crl = crl::WorkspaceCrlManager::new(cfg.controller_http_addr.clone());
    crl.clone().spawn_refresh(60, 15);
    let revoked = async move {
        // A dropped sender (the heartbeat task panicked) is not a revocation.
        if revoked_rx.await.is_err() {
            std::future::pending::<()>().await;
        }
    };
    listener::run_listener(
        cfg.bind_addr,
        certs,
        state,
        policy,
        cfg.runtime_limits,
        crl,
        revoked,
    )
    .await?;
    info!("Relay stopped after revocation");
    Ok(())
}
