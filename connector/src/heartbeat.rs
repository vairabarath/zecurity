// heartbeat.rs — mTLS heartbeat loop for the ZECURITY connector
//
// After enrollment, the connector runs a periodic heartbeat to the controller:
//   1. Build mTLS config: client cert + key, trust root = workspace CA chain
//   2. Pre-flight: raw TLS connection to extract peer cert → verify SPIFFE
//   3. Create tonic mTLS channel + ConnectorServiceClient
//   4. Loop: send HeartbeatRequest every heartbeat_interval_secs
//      - Success: reset backoff, log re_enroll if true, log new version
//      - Failure: exponential backoff (5s → 10s → 20s → 40s → 60s cap)

use std::path::Path;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use reqwest::Client;
use rustls::pki_types::{pem::PemObject, CertificateDer, ServerName};
use rustls::{ClientConfig, RootCertStore};
use tokio::net::TcpStream;
use tokio::time::{interval, sleep};
use tokio_rustls::TlsConnector;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Identity};
use tracing::{error, info, warn};

use crate::config::ConnectorConfig;
use crate::enrollment::EnrollmentState;
use crate::proto;
use crate::renewal;
use crate::tls;
use crate::util;

const BACKOFF_INITIAL_SECS: u64 = 5;
const BACKOFF_MAX_SECS: u64 = 60;

/// Fetch the connector's public IP from ipify.
async fn fetch_public_ip() -> String {
    let result = Client::builder()
        .build()
        .ok()
        .and_then(|c| Some(c))
        .map(|c| async move {
            let resp = c.get("https://api.ipify.org").send().await?;
            resp.text().await.map_err(anyhow::Error::from)
        });

    match result {
        Some(fut) => match fut.await {
            Ok(ip) => ip.trim().to_string(),
            Err(e) => {
                warn!(error = %e, "failed to fetch public IP, using empty string");
                String::new()
            }
        },
        None => {
            warn!("failed to build HTTP client, using empty string for IP");
            String::new()
        }
    }
}

/// Load PEM data from a file.
fn load_pem(path: &Path) -> Result<Vec<u8>> {
    std::fs::read(path).with_context(|| format!("failed to read {}", path.display()))
}

/// Build the tonic mTLS channel.
async fn build_channel(cfg: &ConnectorConfig) -> Result<Channel> {
    let state_dir = Path::new(&cfg.state_dir);

    let cert_pem = load_pem(&state_dir.join("connector.crt"))?;
    let key_pem = load_pem(&state_dir.join("connector.key"))?;
    let ca_pem = load_pem(&state_dir.join("workspace_ca.crt"))?;

    // Tonic accepts raw PEM bytes directly — no DER conversion needed.
    let identity = Identity::from_pem(&cert_pem, &key_pem);
    let ca = Certificate::from_pem(&ca_pem);

    let tls = ClientTlsConfig::new()
        .identity(identity)
        .ca_certificate(ca);

    let grpc_addr = format!("https://{}", cfg.controller_addr);
    let channel = Channel::from_shared(grpc_addr.clone())
        .with_context(|| format!("invalid gRPC address: {}", grpc_addr))?
        .tls_config(tls)
        .with_context(|| format!("failed to configure TLS for {}", grpc_addr))?
        .connect()
        .await
        .with_context(|| format!("failed to connect to {}", grpc_addr))?;

    Ok(channel)
}

/// Pre-flight TLS check: connect to controller, extract peer cert, verify SPIFFE.
async fn verify_controller_spiffe_preflight(cfg: &ConnectorConfig) -> Result<()> {
    let state_dir = Path::new(&cfg.state_dir);

    let cert_pem = load_pem(&state_dir.join("connector.crt"))?;
    let key_pem = load_pem(&state_dir.join("connector.key"))?;
    let ca_pem = load_pem(&state_dir.join("workspace_ca.crt"))?;

    // Parse CA chain into root store
    let mut root_store = RootCertStore::empty();
    for cert_result in CertificateDer::pem_slice_iter(&ca_pem) {
        let cert = cert_result.context("failed to parse CA cert from chain")?;
        root_store
            .add(cert)
            .context("failed to add CA to root store")?;
    }

    // Parse client cert chain
    let client_certs: Vec<CertificateDer<'static>> = CertificateDer::pem_slice_iter(&cert_pem)
        .map(|r| r.map_err(|e| anyhow::anyhow!("failed to parse client cert: {}", e)))
        .collect::<Result<_>>()?;

    // Parse client key (PEM → DER)
    let key = rustls::pki_types::PrivateKeyDer::from_pem_slice(&key_pem)
        .map_err(|e| anyhow::anyhow!("failed to parse client private key: {}", e))?;

    let config = ClientConfig::builder()
        .with_root_certificates(root_store)
        .with_client_auth_cert(client_certs, key)
        .context("failed to build TLS config with client auth")?;

    let tls_connector = TlsConnector::from(Arc::new(config));

    // Parse host:port
    let (host, port) = parse_host_port(&cfg.controller_addr)?;
    let domain = ServerName::try_from(host.as_str())
        .map(|d| d.to_owned())
        .with_context(|| format!("invalid hostname in controller address: {}", host))?;

    // Connect
    let tcp = TcpStream::connect((host.as_str(), port))
        .await
        .with_context(|| format!("failed to TCP connect to {}:{}", host, port))?;

    // TLS handshake — connect() completes the full handshake before returning.
    let tls_stream = tls_connector
        .connect(domain, tcp)
        .await
        .context("TLS handshake failed")?;

    // Handshake is complete — extract peer cert from the connection.
    let (_, conn) = tls_stream.get_ref();
    let peer_cert = conn
        .peer_certificates()
        .and_then(|c| c.first().cloned())
        .context("no peer certificate received from controller")?;

    // Verify SPIFFE identity
    tls::verify_controller_spiffe(&peer_cert)?;

    Ok(())
}

/// Parse "host:port" string.
fn parse_host_port(addr: &str) -> Result<(String, u16)> {
    let colon = addr
        .rfind(':')
        .with_context(|| format!("invalid address (no port): {}", addr))?;
    let host = &addr[..colon];
    let port: u16 = addr[colon + 1..]
        .parse()
        .with_context(|| format!("invalid port in address: {}", addr))?;
    Ok((host.to_string(), port))
}

/// Run the mTLS heartbeat loop.
///
/// Called from main.rs after enrollment succeeds.
/// Blocks indefinitely — spawn on a tokio task.
pub async fn run_heartbeat(cfg: &ConnectorConfig, state: &EnrollmentState, shield_server: crate::agent_server::ShieldServer) -> Result<()> {
    info!("starting mTLS heartbeat pre-flight check");

    // Pre-flight: verify controller SPIFFE identity
    verify_controller_spiffe_preflight(cfg).await?;
    info!("controller SPIFFE identity verified — proceeding with heartbeat loop");

    // Build tonic mTLS channel
    let channel = build_channel(cfg).await?;
    let mut client = proto::connector_service_client::ConnectorServiceClient::new(channel);

    let hostname = util::read_hostname();
    let public_ip = fetch_public_ip().await;
    let version = env!("CARGO_PKG_VERSION").to_string();
    let interval_secs = cfg.heartbeat_interval_secs;

    info!(
        connector_id = %state.connector_id,
        interval_secs = interval_secs,
        version = %version,
        "entering heartbeat loop"
    );

    let mut backoff_secs = BACKOFF_INITIAL_SECS;
    let mut heartbeat_interval = interval(Duration::from_secs(interval_secs));

    // Mutable state for renewal
    let mut current_state = state.clone();

    loop {
        heartbeat_interval.tick().await;

        let request = tonic::Request::new(proto::HeartbeatRequest {
            connector_id: current_state.connector_id.clone(),
            version: version.clone(),
            hostname: hostname.clone(),
            public_ip: public_ip.clone(),
            shields: shield_server.get_alive_shields(),
        });

        match client.heartbeat(request).await {
            Ok(response) => {
                let resp = response.into_inner();
                backoff_secs = BACKOFF_INITIAL_SECS; // reset on success

                if resp.ok {
                    info!("heartbeat ok");
                } else {
                    warn!("heartbeat returned ok=false");
                }

                if resp.re_enroll {
                    info!("controller requested cert renewal — starting renewal");
                    match renewal::renew_cert(&current_state, cfg).await {
                        Ok(new_state) => {
                            info!(
                                "cert renewed successfully, new expiry: {}",
                                new_state.cert_not_after
                            );
                            current_state = new_state;
                            // Rebuild the mTLS channel so subsequent connections
                            // (and any reconnects after TCP drop) use the new cert.
                            match build_channel(cfg).await {
                                Ok(new_channel) => {
                                    client = proto::connector_service_client::ConnectorServiceClient::new(new_channel);
                                    info!("mTLS channel rebuilt with renewed certificate");
                                }
                                Err(e) => {
                                    error!("failed to rebuild mTLS channel after renewal: {}", e);
                                }
                            }
                        }
                        Err(e) => {
                            error!("cert renewal failed: {}", e);
                        }
                    }
                }

                if !resp.latest_version.is_empty() && resp.latest_version != version {
                    info!(
                        latest = %resp.latest_version,
                        current = %version,
                        "new connector version available"
                    );
                }
            }
            Err(e) => {
                error!(
                    error = %e,
                    backoff_secs = backoff_secs,
                    "heartbeat failed, retrying with backoff"
                );
                sleep(Duration::from_secs(backoff_secs)).await;
                backoff_secs = (backoff_secs * 2).min(BACKOFF_MAX_SECS);
            }
        }
    }
}
