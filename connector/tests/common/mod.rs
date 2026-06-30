// Shared harness for Sprint 11 Phase 3 integration tests.
//
// Provides:
//   - `make_test_certs(num_relays)` — generates a workspace CA, a connector
//     leaf with `spiffe://workspace.zecurity.in/connector/<uuid>`, and N
//     relay leaves with `spiffe://zecurity.in/relay/<uuid>`. All P-384.
//   - `ProbeRelay::spawn(...)` — minimal in-process QUIC mTLS server that
//     responds to `HandshakeMsg::Probe` and closes (no register, no streams).
//     Sufficient for scenario4 probe-security tests. Scenario1/2/3 (full
//     selector lifecycle) will extend this with a register handler in a
//     follow-up file.
//   - `LabelledRelayList` construction helpers.
//
// This module is loaded as a normal Rust file (no `#[path]`) — cargo treats
// `tests/common/mod.rs` correctly when test binaries declare `mod common;`.

#![allow(dead_code)]

use std::net::{IpAddr, Ipv4Addr, SocketAddr};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{anyhow, Context, Result};
use quinn::Endpoint;
use rcgen::{
    BasicConstraints, CertificateParams, DistinguishedName, DnType, IsCa, Issuer, KeyPair, SanType,
    PKCS_ECDSA_P384_SHA384,
};
use rustls::pki_types::{CertificateDer, PrivateKeyDer};
use serde::{Deserialize, Serialize};
use tokio::task::JoinHandle;
use uuid::Uuid;
use zecurity_connector::proto::{LabelledRelayInfo, LabelledRelayList, RelayCapacityLabel};

const RELAY_ALPN: &[u8] = b"ztna-relay-v1";
const PROBE_MAX_MSG_SIZE: usize = 16 * 1024;

// --------------------------------------------------------------------------
// Cert factory.
// --------------------------------------------------------------------------

pub struct TestCerts {
    pub workspace_ca_pem: Vec<u8>,
    pub workspace_ca_der: CertificateDer<'static>,
    pub connector_cert_pem: Vec<u8>,
    pub connector_key_pem: Vec<u8>,
    pub connector_id: String,
    pub connector_spiffe_id: String,
    pub relays: Vec<TestRelayCert>,
    ca_key_pem: String,
    ca_cert_der: CertificateDer<'static>,
}

pub struct TestRelayCert {
    pub relay_id: String,
    pub spiffe_id: String,
    pub cert_pem: Vec<u8>,
    pub key_pem: Vec<u8>,
    pub cert_der: CertificateDer<'static>,
    pub key_der: PrivateKeyDer<'static>,
}

fn install_crypto_provider_once() {
    use std::sync::OnceLock;
    static INIT: OnceLock<()> = OnceLock::new();
    INIT.get_or_init(|| {
        let _ = rustls::crypto::ring::default_provider().install_default();
        if std::env::var("RUST_LOG").is_ok() {
            let _ = tracing_subscriber::fmt()
                .with_env_filter(
                    tracing_subscriber::EnvFilter::try_from_default_env()
                        .unwrap_or_else(|_| tracing_subscriber::EnvFilter::new("info")),
                )
                .with_test_writer()
                .try_init();
        }
    });
}

pub fn make_test_certs(num_relays: usize) -> TestCerts {
    install_crypto_provider_once();
    // ---- workspace CA (self-signed) ----
    let mut ca_params = CertificateParams::default();
    let mut ca_dn = DistinguishedName::new();
    ca_dn.push(DnType::CommonName, "zecurity-test-workspace-ca");
    ca_params.distinguished_name = ca_dn;
    ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    let ca_key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
    let ca_cert = ca_params.self_signed(&ca_key).unwrap();
    let ca_pem = ca_cert.pem();
    let ca_der_bytes = ca_cert.der().to_vec();
    let ca_key_pem_str = ca_key.serialize_pem();

    // `Issuer::new` consumes both params and key. We don't need ca_params
    // after this point; ca_key's PEM is already captured above.
    let issuer: Issuer<'static, KeyPair> = Issuer::new(ca_params, ca_key);

    // ---- connector leaf ----
    let connector_id = Uuid::new_v4().to_string();
    let connector_spiffe_id = format!("spiffe://workspace.zecurity.in/connector/{}", connector_id);
    let mut conn_params = CertificateParams::default();
    let mut conn_dn = DistinguishedName::new();
    conn_dn.push(DnType::CommonName, format!("connector-{connector_id}"));
    conn_params.distinguished_name = conn_dn;
    conn_params.subject_alt_names.push(SanType::URI(
        connector_spiffe_id.clone().try_into().unwrap(),
    ));
    let conn_key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
    let conn_cert = conn_params.signed_by(&conn_key, &issuer).unwrap();
    let conn_cert_pem = conn_cert.pem();
    let conn_key_pem = conn_key.serialize_pem();

    // ---- relay leaves ----
    let mut relays = Vec::with_capacity(num_relays);
    for _ in 0..num_relays {
        let relay_id = Uuid::new_v4().to_string();
        let spiffe_id = format!("spiffe://zecurity.in/relay/{relay_id}");
        let mut params = CertificateParams::default();
        let mut dn = DistinguishedName::new();
        dn.push(DnType::CommonName, format!("relay-{relay_id}"));
        params.distinguished_name = dn;
        params
            .subject_alt_names
            .push(SanType::URI(spiffe_id.clone().try_into().unwrap()));
        params
            .subject_alt_names
            .push(SanType::IpAddress(IpAddr::V4(Ipv4Addr::LOCALHOST)));
        let kp = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
        let cert = params.signed_by(&kp, &issuer).unwrap();
        let cert_pem = cert.pem();
        let key_pem = kp.serialize_pem();
        let cert_der = CertificateDer::from(cert.der().to_vec());
        let key_der_bytes = parse_pkcs8_pem(&key_pem);
        let key_der = PrivateKeyDer::try_from(key_der_bytes).expect("pkcs8 private key");
        relays.push(TestRelayCert {
            relay_id,
            spiffe_id,
            cert_pem: cert_pem.into_bytes(),
            key_pem: key_pem.into_bytes(),
            cert_der,
            key_der,
        });
    }

    TestCerts {
        workspace_ca_pem: ca_pem.clone().into_bytes(),
        workspace_ca_der: CertificateDer::from(ca_der_bytes.clone()),
        connector_cert_pem: conn_cert_pem.into_bytes(),
        connector_key_pem: conn_key_pem.into_bytes(),
        connector_id,
        connector_spiffe_id,
        relays,
        ca_key_pem: ca_key_pem_str,
        ca_cert_der: CertificateDer::from(ca_der_bytes),
    }
}

fn parse_pkcs8_pem(pem: &str) -> Vec<u8> {
    // Extract the first PKCS8 block. We use rustls-pemfile via the rustls
    // crate to avoid pulling pem yet another time.
    let mut reader = pem.as_bytes();
    let key = rustls_pemfile::private_key(&mut reader)
        .expect("parse private key PEM")
        .expect("private key PEM contained no key");
    // PrivateKeyDer is opaque; we want the raw bytes for re-wrapping.
    // Easier: just return the key's DER bytes.
    match key {
        PrivateKeyDer::Pkcs8(k) => k.secret_pkcs8_der().to_vec(),
        PrivateKeyDer::Sec1(k) => k.secret_sec1_der().to_vec(),
        PrivateKeyDer::Pkcs1(k) => k.secret_pkcs1_der().to_vec(),
        _ => panic!("unsupported private-key encoding"),
    }
}

// --------------------------------------------------------------------------
// In-process probe-only test relay.
// --------------------------------------------------------------------------

/// Behaviour switch for the test relay's probe responder. The default echoes
/// the request_id correctly; the variants exist to exercise the connector
/// probe's failure paths in scenario4.
#[derive(Clone, Copy, Debug)]
pub enum ProbeBehaviour {
    /// Echo `request_id`, advertise the given `(connection_count, capacity)`.
    EchoCorrectly {
        connection_count: u32,
        capacity: u32,
    },
    /// Echo `request_id + 1` — connector probe must drop the result.
    WrongRequestId,
    /// Accept the connection then close without responding.
    NoResponse,
}

pub struct ProbeRelay {
    pub relay_id: String,
    pub spiffe_id: String,
    pub addr: SocketAddr,
    pub endpoint: Endpoint,
    pub handle: JoinHandle<()>,
}

impl ProbeRelay {
    pub async fn spawn(
        relay_cert: &TestRelayCert,
        workspace_ca_der: CertificateDer<'static>,
        behaviour: ProbeBehaviour,
    ) -> Result<Self> {
        // Build a rustls ServerConfig that requires client mTLS rooted in
        // the workspace CA. ALPN must match the connector's `RELAY_ALPN`.
        let mut roots = rustls::RootCertStore::empty();
        roots
            .add(workspace_ca_der)
            .context("add workspace CA root")?;
        let client_verifier = rustls::server::WebPkiClientVerifier::builder(Arc::new(roots))
            .build()
            .context("build client verifier")?;
        let chain = vec![relay_cert.cert_der.clone()];
        let mut server_config = rustls::ServerConfig::builder()
            .with_client_cert_verifier(client_verifier)
            .with_single_cert(chain, relay_cert.key_der.clone_key())
            .context("build server TLS config")?;
        server_config.alpn_protocols = vec![RELAY_ALPN.to_vec()];

        let quic_server_config = quinn::crypto::rustls::QuicServerConfig::try_from(server_config)
            .context("build quic server config")?;
        let server_config = quinn::ServerConfig::with_crypto(Arc::new(quic_server_config));
        let endpoint = quinn::Endpoint::server(
            server_config,
            "127.0.0.1:0".parse().expect("valid wildcard"),
        )
        .context("bind probe relay endpoint")?;
        let addr = endpoint.local_addr().context("local_addr")?;

        let endpoint_for_accept = endpoint.clone();
        let handle = tokio::spawn(async move {
            while let Some(incoming) = endpoint_for_accept.accept().await {
                let behaviour = behaviour;
                tokio::spawn(async move {
                    if let Err(e) = handle_probe_connection(incoming, behaviour).await {
                        tracing::debug!(error = %e, "test probe relay connection error");
                    }
                });
            }
        });

        Ok(Self {
            relay_id: relay_cert.relay_id.clone(),
            spiffe_id: relay_cert.spiffe_id.clone(),
            addr,
            endpoint,
            handle,
        })
    }

    pub fn info(&self, label: RelayCapacityLabel) -> LabelledRelayInfo {
        LabelledRelayInfo {
            relay_id: self.relay_id.clone(),
            relay_addr: format!("127.0.0.1:{}", self.addr.port()),
            spiffe_id: self.spiffe_id.clone(),
            label: label as i32,
        }
    }
}

impl Drop for ProbeRelay {
    fn drop(&mut self) {
        self.endpoint.close(0u32.into(), b"test relay dropping");
        self.handle.abort();
    }
}

#[derive(Debug, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum IncomingMsg {
    Probe {
        connector_id: String,
        request_id: u64,
    },
    Register {
        connector_id: String,
        spiffe_id: String,
    },
    Lookup {
        connector_id: String,
    },
}

#[derive(Debug, Serialize)]
struct ProbeResp {
    connection_count: u32,
    capacity: u32,
    request_id: u64,
}

#[derive(Debug, Serialize)]
struct RelayAck {
    ok: bool,
    error: Option<String>,
}

async fn handle_probe_connection(
    incoming: quinn::Incoming,
    behaviour: ProbeBehaviour,
) -> Result<()> {
    let connection = incoming.await.context("accept QUIC connection")?;

    let (mut send, mut recv) = tokio::time::timeout(Duration::from_secs(5), connection.accept_bi())
        .await
        .context("accept_bi timeout")?
        .context("accept_bi")?;

    // Read 4-byte length prefix + JSON body.
    let mut len_buf = [0u8; 4];
    recv.read_exact(&mut len_buf)
        .await
        .context("read length prefix")?;
    let len = u32::from_be_bytes(len_buf) as usize;
    if len > PROBE_MAX_MSG_SIZE {
        return Err(anyhow!("message too large: {len}"));
    }
    let mut body = vec![0u8; len];
    recv.read_exact(&mut body).await.context("read body")?;
    let msg: IncomingMsg = serde_json::from_slice(&body).context("decode handshake")?;

    let probe = match msg {
        IncomingMsg::Probe { request_id, .. } => request_id,
        IncomingMsg::Register { .. } | IncomingMsg::Lookup { .. } => {
            // This test relay rejects anything that isn't a Probe.
            let ack = RelayAck {
                ok: false,
                error: Some("probe-only test relay".into()),
            };
            write_json(&mut send, &ack).await?;
            let _ = send.finish();
            // Wait for the peer to acknowledge the stream close before
            // dropping the QUIC connection — quinn defers stream flushes
            // until the connection drains.
            let _ = send.stopped().await;
            return Ok(());
        }
    };

    match behaviour {
        ProbeBehaviour::EchoCorrectly {
            connection_count,
            capacity,
        } => {
            let resp = ProbeResp {
                connection_count,
                capacity,
                request_id: probe,
            };
            write_json(&mut send, &resp).await?;
            let _ = send.finish();
            let _ = send.stopped().await;
        }
        ProbeBehaviour::WrongRequestId => {
            let resp = ProbeResp {
                connection_count: 0,
                capacity: 100,
                request_id: probe.wrapping_add(1),
            };
            write_json(&mut send, &resp).await?;
            let _ = send.finish();
            let _ = send.stopped().await;
        }
        ProbeBehaviour::NoResponse => {
            // Drop without writing — peer must observe stream/connection
            // close as a probe failure.
            drop(send);
        }
    }

    // Hold the connection until the peer closes it, so the test relay
    // doesn't race with quinn's stream flush.
    let _ = connection.closed().await;
    Ok(())
}

async fn write_json<T: Serialize>(send: &mut quinn::SendStream, msg: &T) -> Result<()> {
    let body = serde_json::to_vec(msg)?;
    if body.len() > PROBE_MAX_MSG_SIZE {
        return Err(anyhow!("message too large"));
    }
    send.write_all(&(body.len() as u32).to_be_bytes()).await?;
    send.write_all(&body).await?;
    Ok(())
}

// --------------------------------------------------------------------------
// LabelledRelayList constructor.
// --------------------------------------------------------------------------

pub fn make_labelled_list(relays: &[ProbeRelay], version: u64) -> LabelledRelayList {
    LabelledRelayList {
        relays: relays
            .iter()
            .map(|r| r.info(RelayCapacityLabel::RelayCapacityHigh))
            .collect(),
        version,
    }
}
