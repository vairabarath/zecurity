use std::fs;
use std::os::unix::fs::PermissionsExt;
use std::path::{Path, PathBuf};

use anyhow::{bail, Context, Result};
use reqwest::Client;
use rustls_pemfile::certs;
use sha2::{Digest, Sha256};
use tonic::transport::{Certificate, ClientTlsConfig, Endpoint};
use tracing::{info, warn};

use crate::appmeta;
use crate::cert_manager::{now_unix, record_install};
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
    // if paths.exists = true then the remaining is not executed
    let provisioning_token = require_provisioning_token(cfg.provisioning_token.as_deref())?;
    let ca_pem = fetch_ca_cert(&cfg.controller_http_addr).await?;
    verify_ca_fingerprint(&ca_pem, &cfg.ca_fingerprint)?;
    info!("verified controller Intermediate CA fingerprint");

    let material = generate_relay_csr(&cfg.relay_id, &cfg.dns_sans, &cfg.ip_sans)?; /*RelayCsr {
                                                                                        private_key_pem,
                                                                                        csr_der,
                                                                                    } */

    let response =
        send_provision_request(cfg, provisioning_token, &ca_pem, material.csr_der).await?;
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

fn require_provisioning_token(token: Option<&str>) -> Result<&str> {
    token
        .map(str::trim)
        .filter(|token| !token.is_empty())
        .context(
            "no provisioning token: set RELAY_PROVISIONING_TOKEN or \
             RELAY_PROVISIONING_TOKEN_FILE",
        )
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
        // K1: record when this certificate was installed, for renewal
        // scheduling only, so a failure here does not fail provisioning (the
        // scheduler then falls back to NotBefore).
        if let Err(e) = record_install(&self.certificate_path, certificate, now_unix()) {
            warn!(error = %format!("{e:#}"), "could not record Relay certificate install time");
        }
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

pub(crate) fn certificate_fingerprint(ca_pem: &[u8]) -> Result<String> {
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
    provisioning_token: &str,
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
        provisioning_token: provisioning_token.to_owned(),
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

pub(crate) fn controller_host(controller_addr: &str) -> Result<String> {
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
    fn requires_non_empty_provisioning_token() {
        assert_eq!(
            require_provisioning_token(Some("  relay-token  ")).unwrap(),
            "relay-token"
        );
        assert!(require_provisioning_token(None).is_err());
        assert!(require_provisioning_token(Some("  ")).is_err());
    }

    #[test]
    fn validates_response_identity() {
        let (response, fingerprint) = provision_response();
        validate_response(RELAY_ID, &fingerprint, &response).unwrap();
    }

    #[test]
    fn store_records_the_install_time_of_the_provisioned_certificate() {
        let pki = crate::test_support::TestPki::new();
        let key = pki.relay_key();
        let cert = pki.relay_cert(RELAY_ID, &key, -3600, 4500);
        let dir =
            std::env::temp_dir().join(format!("relay-prov-{}", uuid::Uuid::new_v4().simple()));
        let paths = ProvisionedRelay::new(dir.to_str().unwrap());

        let before = now_unix();
        paths
            .store(
                key.serialize_pem().as_bytes(),
                cert.as_bytes(),
                pki.ca_pem.as_bytes(),
            )
            .unwrap();

        let meta = crate::cert_manager::read_install_meta(&paths.certificate_path)
            .unwrap()
            .expect("provisioning must record the install time");
        assert!((before..=now_unix()).contains(&meta.installed_at_unix));
        let manager = crate::cert_manager::CertManager::load(
            RELAY_ID,
            crate::cert_manager::CertPaths {
                key_path: paths.key_path.clone(),
                certificate_path: paths.certificate_path.clone(),
                intermediate_ca_path: paths.intermediate_ca_path.clone(),
            },
        )
        .unwrap();
        assert_eq!(manager.current().serial_hex, meta.serial_hex);
        assert_eq!(
            manager.current().installed_at_unix,
            Some(meta.installed_at_unix)
        );
    }

    #[test]
    fn rejects_wrong_response_identity() {
        let (mut response, fingerprint) = provision_response();
        response.spiffe_id = "spiffe://zecurity.in/relay/wrong".to_owned();
        assert!(validate_response(RELAY_ID, &fingerprint, &response).is_err());
    }

    // ── Bug 1 (certificate audit 2026-10-15): bootstrap trust ──────────────
    //
    // A real provisioning (`ensure_provisioned`) against a fake controller:
    // an HTTP server for /ca.crt and a gRPC TLS server (test-only server copy
    // of the relay proto) that records every token it receives.

    mod bootstrap {
        use super::super::*;
        use crate::config::RuntimeLimits;
        use crate::test_support::{AttackerPki, TestPki};
        use std::sync::{Arc, Mutex};
        use std::time::Duration;
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        use tonic::transport::{Identity, Server, ServerTlsConfig};
        use tonic::{Request, Response, Status};

        #[allow(clippy::all, dead_code)]
        mod fake {
            include!(concat!(env!("OUT_DIR"), "/test_server/relay.v1.rs"));
        }
        use fake::relay_service_server::{RelayService, RelayServiceServer};

        const RELAY_ID: &str = "550e8400-e29b-41d4-a716-446655440000";

        #[derive(Clone, Copy)]
        enum Reply {
            /// What the real controller returns.
            Genuine,
            /// Mallory's leaf and her root as the "intermediate" (the existing
            /// returned-intermediate pin check refuses this).
            ForgedIntermediate,
            /// Mallory's leaf, but the genuine intermediate returned.
            ForgedLeaf,
            /// Genuine intermediate-signed leaf for some other key.
            OtherKey,
        }

        struct FakeController {
            pki: Arc<TestPki>,
            attacker: Arc<AttackerPki>,
            reply: Reply,
            tokens: Arc<Mutex<Vec<String>>>,
        }

        #[tonic::async_trait]
        impl RelayService for FakeController {
            async fn provision(
                &self,
                request: Request<fake::ProvisionRequest>,
            ) -> Result<Response<fake::ProvisionResponse>, Status> {
                let req = request.into_inner();
                self.tokens
                    .lock()
                    .unwrap()
                    .push(req.provisioning_token.clone());
                let (leaf, int_ca) = match self.reply {
                    Reply::Genuine => (
                        self.pki.sign_relay_csr(&req.csr_der, RELAY_ID),
                        self.pki.ca_pem.clone(),
                    ),
                    Reply::ForgedIntermediate => (
                        self.attacker.sign_csr(&req.csr_der, RELAY_ID),
                        self.attacker.root_pem.clone(),
                    ),
                    Reply::ForgedLeaf => (
                        self.attacker.sign_csr(&req.csr_der, RELAY_ID),
                        self.pki.ca_pem.clone(),
                    ),
                    Reply::OtherKey => (
                        self.pki
                            .relay_cert(RELAY_ID, &self.pki.relay_key(), -60, 3600),
                        self.pki.ca_pem.clone(),
                    ),
                };
                Ok(Response::new(fake::ProvisionResponse {
                    certificate_pem: leaf.into_bytes(),
                    intermediate_ca_pem: int_ca.into_bytes(),
                    relay_id: RELAY_ID.to_string(),
                    spiffe_id: appmeta::relay_spiffe_id(RELAY_ID),
                    cert_not_after_unix: 0,
                    cert_not_before_unix: 0,
                }))
            }

            async fn heartbeat(
                &self,
                _request: Request<fake::HeartbeatRequest>,
            ) -> Result<Response<fake::HeartbeatResponse>, Status> {
                Err(Status::unimplemented("not used in provisioning tests"))
            }

            async fn renew_cert(
                &self,
                _request: Request<fake::RenewCertRequest>,
            ) -> Result<Response<fake::RenewCertResponse>, Status> {
                Err(Status::unimplemented("not used in provisioning tests"))
            }
        }

        async fn spawn_ca_http(body: String) -> std::net::SocketAddr {
            let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
            let addr = listener.local_addr().unwrap();
            tokio::spawn(async move {
                while let Ok((mut sock, _)) = listener.accept().await {
                    let body = body.clone();
                    tokio::spawn(async move {
                        let mut buf = [0u8; 2048];
                        let _ = sock.read(&mut buf).await;
                        let head = format!(
                            "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                            body.len()
                        );
                        let _ = sock.write_all(head.as_bytes()).await;
                        let _ = sock.write_all(body.as_bytes()).await;
                    });
                }
            });
            addr
        }

        async fn spawn_grpc(identity: (String, String), controller: FakeController) -> u16 {
            let probe = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
            let addr = probe.local_addr().unwrap();
            drop(probe);
            tokio::spawn(async move {
                Server::builder()
                    .tls_config(
                        ServerTlsConfig::new().identity(Identity::from_pem(identity.0, identity.1)),
                    )
                    .unwrap()
                    .add_service(RelayServiceServer::new(controller))
                    .serve(addr)
                    .await
                    .unwrap();
            });
            for _ in 0..200 {
                if tokio::net::TcpStream::connect(addr).await.is_ok() {
                    break;
                }
                tokio::time::sleep(Duration::from_millis(10)).await;
            }
            addr.port()
        }

        struct Scenario {
            pki: Arc<TestPki>,
            attacker: Arc<AttackerPki>,
            dir: std::path::PathBuf,
            tokens: Arc<Mutex<Vec<String>>>,
        }

        impl Drop for Scenario {
            fn drop(&mut self) {
                let _ = fs::remove_dir_all(&self.dir);
            }
        }

        impl Scenario {
            fn new() -> Self {
                let dir = std::env::temp_dir()
                    .join(format!("relay-provision-{}", uuid::Uuid::new_v4().simple()));
                fs::create_dir_all(&dir).unwrap();
                Self {
                    pki: Arc::new(TestPki::new()),
                    attacker: Arc::new(AttackerPki::new()),
                    dir,
                    tokens: Arc::new(Mutex::new(Vec::new())),
                }
            }

            async fn provision(
                &self,
                ca_body: String,
                identity: (String, String),
                reply: Reply,
            ) -> Result<ProvisionedRelay> {
                let http = spawn_ca_http(ca_body).await;
                let grpc = spawn_grpc(
                    identity,
                    FakeController {
                        pki: self.pki.clone(),
                        attacker: self.attacker.clone(),
                        reply,
                        tokens: self.tokens.clone(),
                    },
                )
                .await;
                let cfg = RelayConfig {
                    relay_id: RELAY_ID.to_string(),
                    provisioning_token: Some("provisioning-token".to_string()),
                    controller_addr: format!("localhost:{grpc}"),
                    controller_http_addr: http.to_string(),
                    bind_addr: "127.0.0.1:0".parse().unwrap(),
                    ca_fingerprint: certificate_fingerprint(self.pki.ca_pem.as_bytes()).unwrap(),
                    state_dir: self.dir.to_str().unwrap().to_string(),
                    dns_sans: vec![],
                    ip_sans: vec![],
                    log_level: "info".to_string(),
                    runtime_limits: RuntimeLimits {
                        max_connections: 16,
                        max_lookup_bridges: 16,
                        max_bidi_streams: 16,
                        idle_timeout: Duration::from_secs(30),
                        handshake_timeout: Duration::from_secs(5),
                        message_timeout: Duration::from_secs(5),
                        max_probe_rate: 10,
                        max_concurrent_probes: 4,
                        probe_timeout: Duration::from_secs(2),
                    },
                    heartbeat_interval: Duration::from_secs(30),
                };
                ensure_provisioned(&cfg).await
            }

            fn tokens_received(&self) -> usize {
                self.tokens.lock().unwrap().len()
            }

            fn files(&self) -> Vec<String> {
                let mut names: Vec<String> = fs::read_dir(&self.dir)
                    .unwrap()
                    .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
                    .collect();
                names.sort();
                names
            }
        }

        /// Attack: Mallory appends her root to /ca.crt; today the whole file
        /// is trusted and her TLS server receives the provisioning token.
        #[tokio::test]
        #[ignore = "bug 1: demonstrates the bootstrap hijack; un-ignore with the fix"]
        async fn appended_ca_cannot_intercept_the_provisioning_token() {
            let s = Scenario::new();
            let tampered = format!("{}\n{}", s.pki.ca_pem, s.attacker.root_pem);
            let result = s
                .provision(tampered, s.attacker.server_identity(), Reply::Genuine)
                .await;
            assert_eq!(
                s.tokens_received(),
                0,
                "the provisioning token reached Mallory's server (ok={})",
                result.is_ok()
            );
            assert!(result.is_err());
            assert!(
                s.files().is_empty(),
                "nothing may be persisted: {:?}",
                s.files()
            );
        }

        /// Regression: the existing returned-intermediate pin check refuses a
        /// reply carrying Mallory's root as the intermediate.
        #[tokio::test]
        async fn returned_intermediate_must_match_the_pin() {
            let s = Scenario::new();
            let result = s
                .provision(
                    s.pki.ca_pem.clone(),
                    s.pki.controller_server_identity(),
                    Reply::ForgedIntermediate,
                )
                .await;
            let err = result.expect_err("forged intermediate must be refused");
            assert!(
                format!("{err:#}").contains("Intermediate CA fingerprint mismatch"),
                "{err:#}"
            );
            assert!(
                s.files().is_empty(),
                "nothing may be persisted: {:?}",
                s.files()
            );
        }

        /// Attack: genuine intermediate returned, but the leaf is signed by
        /// Mallory; today it is stored.
        #[tokio::test]
        #[ignore = "bug 1: demonstrates the bootstrap hijack; un-ignore with the fix"]
        async fn leaf_not_issued_by_the_pinned_intermediate_is_refused() {
            let s = Scenario::new();
            let result = s
                .provision(
                    s.pki.ca_pem.clone(),
                    s.pki.controller_server_identity(),
                    Reply::ForgedLeaf,
                )
                .await;
            assert!(
                result.is_err(),
                "a leaf not issued by the intermediate must be refused"
            );
            assert!(
                s.files().is_empty(),
                "nothing may be persisted: {:?}",
                s.files()
            );
        }

        #[tokio::test]
        #[ignore = "bug 1: demonstrates the bootstrap hijack; un-ignore with the fix"]
        async fn leaf_for_another_key_is_refused_and_nothing_persisted() {
            let s = Scenario::new();
            let result = s
                .provision(
                    s.pki.ca_pem.clone(),
                    s.pki.controller_server_identity(),
                    Reply::OtherKey,
                )
                .await;
            assert!(result.is_err(), "a leaf for another key must be refused");
            assert!(
                s.files().is_empty(),
                "nothing may be persisted: {:?}",
                s.files()
            );
        }

        #[tokio::test]
        #[ignore = "bug 1: demonstrates the bootstrap hijack; un-ignore with the fix"]
        async fn failed_attempt_leaves_nothing_and_retry_succeeds() {
            let s = Scenario::new();
            let first = s
                .provision(
                    s.pki.ca_pem.clone(),
                    s.pki.controller_server_identity(),
                    Reply::ForgedLeaf,
                )
                .await;
            assert!(first.is_err(), "forged first attempt must fail");
            assert!(s.files().is_empty(), "failed attempt left {:?}", s.files());
            let second = s
                .provision(
                    s.pki.ca_pem.clone(),
                    s.pki.controller_server_identity(),
                    Reply::Genuine,
                )
                .await;
            assert!(second.is_ok(), "retry must succeed: {:?}", second.err());
        }

        #[tokio::test]
        async fn wrong_ca_is_refused_before_connecting() {
            let s = Scenario::new();
            let result = s
                .provision(
                    s.attacker.root_pem.clone(),
                    s.attacker.server_identity(),
                    Reply::Genuine,
                )
                .await;
            assert!(result.is_err());
            assert_eq!(s.tokens_received(), 0);
        }

        /// Positive: genuine provisioning installs material that the relay's
        /// CertManager loads (leaf for the relay's own key).
        #[tokio::test]
        async fn genuine_provisioning_installs_loadable_material() {
            let s = Scenario::new();
            let paths = s
                .provision(
                    s.pki.ca_pem.clone(),
                    s.pki.controller_server_identity(),
                    Reply::Genuine,
                )
                .await
                .expect("genuine provisioning");
            crate::cert_manager::CertManager::load(
                RELAY_ID,
                crate::cert_manager::CertPaths {
                    key_path: paths.key_path.clone(),
                    certificate_path: paths.certificate_path.clone(),
                    intermediate_ca_path: paths.intermediate_ca_path.clone(),
                },
            )
            .expect("provisioned material loads");
            assert_eq!(
                fs::read_to_string(&paths.intermediate_ca_path).unwrap(),
                s.pki.ca_pem
            );
        }
    }
}
