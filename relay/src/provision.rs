use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

use anyhow::{bail, Context, Result};
use reqwest::Client;
use rustls_pemfile::certs;
use sha2::{Digest, Sha256};
use tonic::transport::{Certificate, ClientTlsConfig, Endpoint};
use tracing::info;

use crate::appmeta;
use crate::config::RelayConfig;
use crate::csr::generate_relay_csr;
use crate::relay::v1::relay_service_client::RelayServiceClient;
use crate::relay::v1::{ProvisionRequest, ProvisionResponse};
use crate::spiffe::extract_spiffe_uri;

#[derive(Debug)]
pub struct ProvisionedRelay {
    pub key_path: PathBuf,
    pub certificate_path: PathBuf,
    pub intermediate_ca_path: PathBuf,
}

pub async fn ensure_provisioned(cfg: &RelayConfig) -> Result<ProvisionedRelay> {
    let paths = ProvisionedRelay::new(&cfg.state_dir);
    if paths.exists() {
        info!(state_dir = %cfg.state_dir, "Relay certificate material already exists");
        return Ok(paths);
    } 
    // if paths.exists = ture then the remaining is not executed

    let ca_pem = fetch_ca_cert(&cfg.controller_http_addr).await?;
    verify_ca_fingerprint(&ca_pem, &cfg.ca_fingerprint)?;
    info!("verified controller Intermediate CA fingerprint");

    let material = generate_relay_csr(&cfg.relay_id, &cfg.dns_sans, &cfg.ip_sans)?;/*RelayCsr {
                                                                                                private_key_pem,
                                                                                                csr_der,
                                                                                            } */
                                                                                           
    let response = send_provision_request(cfg, &ca_pem, material.csr_der).await?;
    validate_response(&cfg.relay_id, &cfg.ca_fingerprint, &response)?;
    paths.store(
        material.private_key_pem.as_bytes(),
        &response.certificate_pem,
        &response.intermediate_ca_pem,
    )?;

    info!(
        certificate = %paths.certificate_path.display(),
        intermediate_ca = %paths.intermediate_ca_path.display(),
        "stored Relay certificate material"
    );
    Ok(paths)
}

impl ProvisionedRelay {
    fn new(state_dir: &str) -> Self {
        let state_dir = Path::new(state_dir); // converts path object
        Self {
            key_path: state_dir.join("relay.key"),
            certificate_path: state_dir.join("relay.crt"),
            intermediate_ca_path: state_dir.join("intermediate-ca.crt"),
        }
    }

    fn exists(&self) -> bool {
        self.key_path.exists()
            && self.certificate_path.exists()
            && self.intermediate_ca_path.exists()
    }

    fn store(&self, key: &[u8], certificate: &[u8], intermediate_ca: &[u8]) -> Result<()> {
        let state_dir = self
            .key_path
            .parent()
            .context("Relay key path has no parent directory")?;
        fs::create_dir_all(state_dir)
            .with_context(|| format!("failed to create {}", state_dir.display()))?;

        fs::write(&self.key_path, key)
            .with_context(|| format!("failed to write {}", self.key_path.display()))?;
        let mut permissions = fs::metadata(&self.key_path)?.permissions();
        permissions.set_mode(0o600);
        fs::set_permissions(&self.key_path, permissions)?;

        fs::write(&self.certificate_path, certificate)
            .with_context(|| format!("failed to write {}", self.certificate_path.display()))?;
        fs::write(&self.intermediate_ca_path, intermediate_ca)
            .with_context(|| format!("failed to write {}", self.intermediate_ca_path.display()))?;
        Ok(())
    }
}

async fn fetch_ca_cert(http_addr: &str) -> Result<String> {
    let url = format!("http://{http_addr}/ca.crt");
    let response = Client::new()
        .get(&url)
        .send()
        .await
        .with_context(|| format!("failed to GET {url}"))?;
    if !response.status().is_success() {
        bail!("failed to fetch CA certificate: HTTP {}", response.status());
    }

    let pem = response
        .text()
        .await
        .context("failed to read CA certificate response")?;
    if !pem.contains("-----BEGIN CERTIFICATE-----") {
        bail!("controller CA response did not contain a PEM certificate");
    }
    Ok(pem)
}

fn certificate_fingerprint(ca_pem: &[u8]) -> Result<String> {
    let mut pem = ca_pem;
    let certificates = certs(&mut pem)
        .collect::<Result<Vec<_>, _>>()
        .context("failed to parse controller CA PEM")?;
    let certificate = certificates
        .first()
        .context("controller CA PEM contained no certificates")?;
    Ok(hex::encode(Sha256::digest(certificate.as_ref())))
}

fn verify_ca_fingerprint(ca_pem: &str, expected: &str) -> Result<()> {
    let actual = certificate_fingerprint(ca_pem.as_bytes())?;
    if actual != expected {
        bail!("controller CA fingerprint mismatch: expected {expected}, got {actual}");
    }
    Ok(())
}

async fn send_provision_request(
    cfg: &RelayConfig,
    ca_pem: &str,
    csr_der: Vec<u8>,
) -> Result<ProvisionResponse> {
    let grpc_host = controller_host(&cfg.controller_addr)?;
    let grpc_addr = format!("https://{}", cfg.controller_addr);
    let channel = Endpoint::from_shared(grpc_addr.clone())
        .with_context(|| format!("invalid controller gRPC address: {grpc_addr}"))?
        .tls_config(
            ClientTlsConfig::new()
                .ca_certificate(Certificate::from_pem(ca_pem.as_bytes()))
                .domain_name(grpc_host),
        )
        .context("failed to configure controller TLS")?
        .connect()
        .await
        .with_context(|| format!("failed to connect to {grpc_addr}"))?;

    let request = ProvisionRequest {
        provisioning_token: String::new(),
        relay_id: cfg.relay_id.clone(),
        csr_der,
        version: env!("CARGO_PKG_VERSION").to_owned(),
        hostname: read_hostname(),
        dns_sans: cfg.dns_sans.clone(),
        ip_sans: cfg.ip_sans.iter().map(ToString::to_string).collect(),
    };

    Ok(RelayServiceClient::new(channel)
        .provision(tonic::Request::new(request))
        .await
        .context("Relay Provision RPC failed")?
        .into_inner())
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
    fs::read_to_string("/etc/hostname")
        .map(|hostname| hostname.trim().to_owned())
        .unwrap_or_else(|_| "unknown".to_owned())
}

fn validate_response(
    relay_id: &str,
    expected_ca_fingerprint: &str,
    response: &ProvisionResponse,
) -> Result<()> {
    if response.relay_id != relay_id {
        bail!(
            "controller returned Relay ID {}, expected {}",
            response.relay_id,
            relay_id
        );
    }
    let expected_spiffe = appmeta::relay_spiffe_id(relay_id);
    if response.spiffe_id != expected_spiffe {
        bail!(
            "controller returned SPIFFE ID {}, expected {}",
            response.spiffe_id,
            expected_spiffe
        );
    }
    if response.certificate_pem.is_empty() || response.intermediate_ca_pem.is_empty() {
        bail!("controller returned incomplete Relay certificate material");
    }

    let mut certificate_pem = response.certificate_pem.as_slice();
    let certificates = certs(&mut certificate_pem)
        .collect::<Result<Vec<_>, _>>()
        .context("failed to parse returned Relay certificate PEM")?;
    let leaf = certificates
        .first()
        .context("returned Relay certificate PEM contained no certificate")?;
    let certificate_spiffe = extract_spiffe_uri(leaf.as_ref())
        .context("returned Relay certificate has no SPIFFE URI")?;
    if certificate_spiffe != expected_spiffe {
        bail!(
            "returned Relay certificate SPIFFE ID {}, expected {}",
            certificate_spiffe,
            expected_spiffe
        );
    }

    let returned_ca_fingerprint = certificate_fingerprint(&response.intermediate_ca_pem)?;
    if returned_ca_fingerprint != expected_ca_fingerprint {
        bail!(
            "returned Intermediate CA fingerprint mismatch: expected {}, got {}",
            expected_ca_fingerprint,
            returned_ca_fingerprint
        );
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use rcgen::{CertificateParams, KeyPair, SanType};

    const RELAY_ID: &str = "550e8400-e29b-41d4-a716-446655440000";

    fn provision_response() -> (ProvisionResponse, String) {
        let spiffe_id = appmeta::relay_spiffe_id(RELAY_ID);
        let mut params = CertificateParams::default();
        params
            .subject_alt_names
            .push(SanType::URI(spiffe_id.as_str().try_into().unwrap()));
        let key = KeyPair::generate().unwrap();
        let certificate_pem = params.self_signed(&key).unwrap().pem().into_bytes();
        let fingerprint = certificate_fingerprint(&certificate_pem).unwrap();

        (
            ProvisionResponse {
                certificate_pem: certificate_pem.clone(),
                intermediate_ca_pem: certificate_pem,
                relay_id: RELAY_ID.to_owned(),
                spiffe_id,
                ..Default::default()
            },
            fingerprint,
        )
    }

    #[test]
    fn extracts_controller_host() {
        assert_eq!(
            controller_host("controller.example.com:9090").unwrap(),
            "controller.example.com"
        );
        assert_eq!(controller_host("127.0.0.1:9090").unwrap(), "127.0.0.1");
    }

    #[test]
    fn validates_response_identity() {
        let (response, fingerprint) = provision_response();
        validate_response(RELAY_ID, &fingerprint, &response).unwrap();
    }

    #[test]
    fn rejects_wrong_response_identity() {
        let (mut response, fingerprint) = provision_response();
        response.spiffe_id = "spiffe://zecurity.in/relay/wrong".to_owned();
        assert!(validate_response(RELAY_ID, &fingerprint, &response).is_err());
    }
}
