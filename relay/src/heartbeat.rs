use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{Context, Result};
use tonic::transport::{Certificate, ClientTlsConfig, Endpoint, Identity};
use tracing::{info, warn};

use crate::config::RelayConfig;
use crate::relay::v1::relay_service_client::RelayServiceClient;
use crate::relay::v1::HeartbeatRequest;
use crate::session::ACTIVE_STREAMS;
use crate::state::RelayState;

const RECONNECT_DELAY: Duration = Duration::from_secs(5);
const RPC_TIMEOUT: Duration = Duration::from_secs(10);

pub async fn run(
    cfg: RelayConfig,
    certificate_pem: Vec<u8>,
    key_pem: Vec<u8>,
    intermediate_ca_pem: Vec<u8>,
    state: Arc<RelayState>,
) {
    let started_at = Instant::now();
    let hostname = read_hostname();

    loop {
        match run_connected(
            &cfg,
            &certificate_pem,
            &key_pem,
            &intermediate_ca_pem,
            &hostname,
            started_at,
            state.clone(),
        )
        .await
        {
            Ok(()) => warn!("Relay heartbeat connection exited cleanly"),
            Err(e) => warn!(error = %e, "Relay heartbeat connection ended"),
        }
        tokio::time::sleep(RECONNECT_DELAY).await;
    }
}

async fn run_connected(
    cfg: &RelayConfig,
    certificate_pem: &[u8],
    key_pem: &[u8],
    intermediate_ca_pem: &[u8],
    hostname: &str,
    started_at: Instant,
    state: Arc<RelayState>,
) -> Result<()> {
    let grpc_host = controller_host(&cfg.controller_addr)?;
    let grpc_addr = format!("https://{}", cfg.controller_addr);
    let channel = Endpoint::from_shared(grpc_addr.clone())
        .with_context(|| format!("invalid controller gRPC address: {grpc_addr}"))?
        .tls_config(
            ClientTlsConfig::new()
                .identity(Identity::from_pem(certificate_pem, key_pem))
                .ca_certificate(Certificate::from_pem(intermediate_ca_pem))
                .domain_name(grpc_host),
        )
        .context("configure Relay heartbeat mTLS")?
        .connect()
        .await
        .with_context(|| format!("connect Relay heartbeat to {grpc_addr}"))?;
    let mut client = RelayServiceClient::new(channel);

    loop {
        let mut request = tonic::Request::new(HeartbeatRequest {
            version: env!("CARGO_PKG_VERSION").to_owned(),
            hostname: hostname.to_owned(),
            uptime_seconds: started_at.elapsed().as_secs(),
            registered_connectors: state.connector_count() as u64,
            listen_port: cfg.bind_addr.port() as u32,
            connection_count: ACTIVE_STREAMS.load(std::sync::atomic::Ordering::Relaxed),
            max_connections: cfg.runtime_limits.max_connections as u32,
        });
        request.set_timeout(RPC_TIMEOUT);
        let response = client
            .heartbeat(request)
            .await
            .context("Relay Heartbeat RPC failed")?
            .into_inner();

        info!(
            server_time_unix = response.server_time_unix,
            registered_connectors = state.connector_count(),
            "Relay heartbeat acknowledged"
        );
        let interval = if response.next_heartbeat_seconds == 0 {
            cfg.heartbeat_interval
        } else {
            Duration::from_secs(response.next_heartbeat_seconds.into())
        };
        tokio::time::sleep(interval).await;
    }
}

fn controller_host(controller_addr: &str) -> Result<String> {
    let uri = format!("https://{controller_addr}")
        .parse::<http::Uri>()
        .context("invalid CONTROLLER_ADDR")?;
    uri.host()
        .map(str::to_owned)
        .context("CONTROLLER_ADDR must include a hostname")
}

fn read_hostname() -> String {
    std::fs::read_to_string("/etc/hostname")
        .map(|hostname| hostname.trim().to_owned())
        .unwrap_or_else(|_| "unknown".to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn extracts_controller_host() {
        assert_eq!(
            controller_host("controller.example.com:9090").unwrap(),
            "controller.example.com"
        );
        assert_eq!(controller_host("127.0.0.1:9090").unwrap(), "127.0.0.1");
    }
}
