use std::path::Path;
use std::sync::Arc;

use anyhow::{Context, Result};
use reqwest::Client;
use rustls::pki_types::{pem::PemObject, CertificateDer, ServerName};
use rustls::{ClientConfig, RootCertStore};
use tokio::net::TcpStream;
use tokio_rustls::TlsConnector;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Identity};
use tracing::warn;

use crate::config::ConnectorConfig;
use crate::tls;

pub async fn fetch_public_ip() -> String {
    let result = Client::builder().build().ok().map(|c| async move {
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

fn load_pem(path: &Path) -> Result<Vec<u8>> {
    std::fs::read(path).with_context(|| format!("failed to read {}", path.display()))
}

pub async fn build_channel(cfg: &ConnectorConfig) -> Result<Channel> {
    let state_dir = Path::new(&cfg.state_dir);

    let cert_pem = load_pem(&state_dir.join("connector.crt"))?;
    let key_pem = load_pem(&state_dir.join("connector.key"))?;
    let ca_pem = load_pem(&state_dir.join("workspace_ca.crt"))?;

    let tls = ClientTlsConfig::new()
        .identity(Identity::from_pem(&cert_pem, &key_pem))
        .ca_certificate(Certificate::from_pem(&ca_pem));

    let grpc_addr = format!("https://{}", cfg.controller_addr);
    Channel::from_shared(grpc_addr.clone())
        .with_context(|| format!("invalid gRPC address: {}", grpc_addr))?
        .tls_config(tls)
        .with_context(|| format!("failed to configure TLS for {}", grpc_addr))?
        .connect()
        .await
        .with_context(|| format!("failed to connect to {}", grpc_addr))
}

pub async fn verify_controller_spiffe_preflight(cfg: &ConnectorConfig) -> Result<()> {
    let state_dir = Path::new(&cfg.state_dir);

    let cert_pem = load_pem(&state_dir.join("connector.crt"))?;
    let key_pem = load_pem(&state_dir.join("connector.key"))?;
    let ca_pem = load_pem(&state_dir.join("workspace_ca.crt"))?;

    let mut root_store = RootCertStore::empty();
    for cert_result in CertificateDer::pem_slice_iter(&ca_pem) {
        let cert = cert_result.context("failed to parse CA cert from chain")?;
        root_store
            .add(cert)
            .context("failed to add CA to root store")?;
    }

    let client_certs: Vec<CertificateDer<'static>> = CertificateDer::pem_slice_iter(&cert_pem)
        .map(|r| r.map_err(|e| anyhow::anyhow!("failed to parse client cert: {}", e)))
        .collect::<Result<_>>()?;

    let key = rustls::pki_types::PrivateKeyDer::from_pem_slice(&key_pem)
        .map_err(|e| anyhow::anyhow!("failed to parse client private key: {}", e))?;

    let config = ClientConfig::builder()
        .with_root_certificates(root_store)
        .with_client_auth_cert(client_certs, key)
        .context("failed to build TLS config with client auth")?;

    let tls_connector = TlsConnector::from(Arc::new(config));
    let (host, port) = parse_host_port(&cfg.controller_addr)?;
    let domain = ServerName::try_from(host.as_str())
        .map(|d| d.to_owned())
        .with_context(|| format!("invalid hostname in controller address: {}", host))?;

    let tcp = TcpStream::connect((host.as_str(), port))
        .await
        .with_context(|| format!("failed to TCP connect to {}:{}", host, port))?;

    let tls_stream = tls_connector
        .connect(domain, tcp)
        .await
        .context("TLS handshake failed")?;

    let (_, conn) = tls_stream.get_ref();
    let peer_cert = conn
        .peer_certificates()
        .and_then(|c| c.first().cloned())
        .context("no peer certificate received from controller")?;

    tls::verify_controller_spiffe(&peer_cert)
}

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
