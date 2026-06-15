use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{bail, Context, Result};
use quinn::{Connection, Endpoint, RecvStream, SendStream};
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, ServerName, UnixTime};
use rustls::{
    CertificateError, DigitallySignedStruct, Error as RustlsError, RootCertStore, SignatureScheme,
};
use rustls_pemfile::{certs, private_key};
use serde::{Deserialize, Serialize};
use tracing::{info, warn};
use x509_parser::extensions::GeneralName;
use x509_parser::prelude::{FromDer, X509Certificate};

use crate::relay_handler::RelayHandler;

const RELAY_ALPN: &[u8] = b"ztna-relay-v1";
const MAX_MESSAGE_SIZE: usize = 16 * 1024;
const RECONNECT_DELAY: Duration = Duration::from_secs(5);

#[derive(Debug, Serialize)]
#[serde(tag = "type", rename_all = "snake_case")]
enum HandshakeMsg<'a> {
    Register {
        connector_id: &'a str,
        spiffe_id: &'a str,
    },
}

#[derive(Debug, Deserialize)]
struct RelayAck {
    ok: bool,
    error: Option<String>,
}

/// Persistent Connector-side connection to a Relay.
///
/// `_endpoint` must remain alive for as long as the QUIC connection is used.
pub struct RelayClient {
    _endpoint: Endpoint,
    connection: Connection,
}

impl RelayClient {
    /// Connect to a Relay using the Connector's mTLS identity.
    ///
    /// The Connector presents `connector leaf + Workspace CA`, trusts only the
    /// Platform Intermediate CA, and requires the Relay's exact SPIFFE URI.
    pub async fn connect(
        relay_addr: SocketAddr,
        relay_spiffe_id: &str,
        cert_pem: &[u8],
        key_pem: &[u8],
        workspace_ca_bundle_pem: &[u8],
        intermediate_ca_bundle_pem: &[u8],
    ) -> Result<Self> {
        let tls_config = build_relay_tls_config(
            relay_spiffe_id,
            cert_pem,
            key_pem,
            workspace_ca_bundle_pem,
            intermediate_ca_bundle_pem,
        )?;
        let quic_config = quinn::crypto::rustls::QuicClientConfig::try_from(tls_config)
            .context("build Relay QUIC TLS config")?;
        let mut client_config = quinn::ClientConfig::new(Arc::new(quic_config));
        let mut transport = quinn::TransportConfig::default();
        transport.keep_alive_interval(Some(Duration::from_secs(10)));
        client_config.transport_config(Arc::new(transport));

        let mut endpoint =
            Endpoint::client("0.0.0.0:0".parse().expect("valid wildcard socket address"))
                .context("bind Relay QUIC client endpoint")?;
        endpoint.set_default_client_config(client_config);

        // Relay certificates use a SPIFFE URI rather than a DNS SAN. The
        // custom verifier below performs the exact identity check.
        let connection = endpoint
            .connect(relay_addr, "relay")
            .context("start Relay QUIC connection")?
            .await
            .with_context(|| format!("connect to Relay at {relay_addr}"))?;

        Ok(Self {
            _endpoint: endpoint,
            connection,
        })
    }

    /// Register this authenticated Connector with the Relay.
    pub async fn register(&self, connector_id: &str, spiffe_id: &str) -> Result<()> {
        let (mut send, mut recv) = self
            .connection
            .open_bi()
            .await
            .context("open Relay registration stream")?;
        write_message(
            &mut send,
            &HandshakeMsg::Register {
                connector_id,
                spiffe_id,
            },
        )
        .await?;

        let ack: RelayAck = read_message(&mut recv).await?;
        if !ack.ok {
            bail!(
                "Relay rejected Connector registration: {}",
                ack.error.as_deref().unwrap_or("unspecified error")
            );
        }

        info!(connector_id, spiffe_id, "Connector registered with Relay");
        Ok(())
    }

    /// Clone the registered outer QUIC connection for the Relay stream handler.
    pub fn connection(&self) -> Connection {
        self.connection.clone()
    }
}

/// Maintain Connector registration until the task is cancelled.
pub async fn maintain_registration(
    relay_addr: String,
    relay_spiffe_id: String,
    connector_id: String,
    spiffe_id: String,
    cert_pem: Vec<u8>,
    key_pem: Vec<u8>,
    workspace_ca_bundle_pem: Vec<u8>,
    intermediate_ca_bundle_pem: Vec<u8>,
    relay_handler: Arc<RelayHandler>,
) {
    loop {
        let result = async {
            let resolved_addr = resolve_relay_addr(&relay_addr).await?;
            let client = RelayClient::connect(
                resolved_addr,
                &relay_spiffe_id,
                &cert_pem,
                &key_pem,
                &workspace_ca_bundle_pem,
                &intermediate_ca_bundle_pem,
            )
            .await?;
            client.register(&connector_id, &spiffe_id).await?;
            relay_handler.clone().run(client.connection()).await
        }
        .await;

        match result {
            Ok(()) => info!(relay_addr, "Relay registration connection closed"),
            Err(error) => warn!(%relay_addr, %error, "Relay registration failed"),
        }
        tokio::time::sleep(RECONNECT_DELAY).await;
    }
}

async fn resolve_relay_addr(relay_addr: &str) -> Result<SocketAddr> {
    tokio::net::lookup_host(relay_addr)
        .await
        .with_context(|| format!("resolve Relay address {relay_addr}"))?
        .next()
        .with_context(|| format!("Relay address {relay_addr} resolved to no socket addresses"))
}

#[derive(Debug)]
struct ExactRelaySpiffeVerifier {
    inner: Arc<rustls::client::WebPkiServerVerifier>,
    expected_spiffe_id: String,
}

impl ExactRelaySpiffeVerifier {
    fn new(roots: RootCertStore, expected_spiffe_id: String) -> Result<Arc<Self>> {
        let inner = rustls::client::WebPkiServerVerifier::builder(Arc::new(roots))
            .build()
            .context("build Relay certificate chain verifier")?;
        Ok(Arc::new(Self {
            inner,
            expected_spiffe_id,
        }))
    }

    fn verify_exact_spiffe(&self, end_entity: &CertificateDer<'_>) -> Result<(), RustlsError> {
        let (_, cert) = X509Certificate::from_der(end_entity.as_ref())
            .map_err(|_| RustlsError::InvalidCertificate(CertificateError::BadEncoding))?;
        let san = cert
            .subject_alternative_name()
            .map_err(|_| RustlsError::InvalidCertificate(CertificateError::BadEncoding))?
            .ok_or(RustlsError::InvalidCertificate(
                CertificateError::NotValidForName,
            ))?;

        let uri_sans: Vec<&str> = san
            .value
            .general_names
            .iter()
            .filter_map(|name| match name {
                GeneralName::URI(uri) => Some(*uri),
                _ => None,
            })
            .collect();
        if uri_sans == [self.expected_spiffe_id.as_str()] {
            return Ok(());
        }

        Err(RustlsError::InvalidCertificate(
            CertificateError::NotValidForName,
        ))
    }
}

impl ServerCertVerifier for ExactRelaySpiffeVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        server_name: &ServerName<'_>,
        ocsp_response: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, RustlsError> {
        match self.inner.verify_server_cert(
            end_entity,
            intermediates,
            server_name,
            ocsp_response,
            now,
        ) {
            Ok(verified) => {
                self.verify_exact_spiffe(end_entity)?;
                Ok(verified)
            }
            Err(RustlsError::InvalidCertificate(CertificateError::NotValidForName))
            | Err(RustlsError::InvalidCertificate(CertificateError::NotValidForNameContext {
                ..
            })) => {
                self.verify_exact_spiffe(end_entity)?;
                Ok(ServerCertVerified::assertion())
            }
            Err(error) => Err(error),
        }
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, RustlsError> {
        self.inner.verify_tls12_signature(message, cert, dss)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, RustlsError> {
        self.inner.verify_tls13_signature(message, cert, dss)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.inner.supported_verify_schemes()
    }
}

fn build_relay_tls_config(
    relay_spiffe_id: &str,
    cert_pem: &[u8],
    key_pem: &[u8],
    workspace_ca_bundle_pem: &[u8],
    intermediate_ca_bundle_pem: &[u8],
) -> Result<rustls::ClientConfig> {
    validate_relay_spiffe_id(relay_spiffe_id)?;

    let mut client_chain = parse_certificates(cert_pem, "Connector certificate")?;
    if client_chain.len() > 2 {
        bail!(
            "Connector certificate PEM must contain leaf plus at most Workspace CA, got {} certificates",
            client_chain.len()
        );
    }
    let workspace_cas = parse_certificates(workspace_ca_bundle_pem, "Workspace CA bundle")?;
    if client_chain.len() == 1 {
        client_chain.push(workspace_cas[0].clone());
    }

    let private_key = parse_private_key(key_pem)?;
    // Existing Connector state stores `Workspace CA + Platform Intermediate`
    // in workspace_ca.crt. Taking the final certificate supports that bundle
    // while ensuring the Workspace CA is never installed as a Relay root.
    let intermediate_cas = parse_certificates(
        intermediate_ca_bundle_pem,
        "Platform Intermediate CA bundle",
    )?;
    let mut roots = RootCertStore::empty();
    roots
        .add(
            intermediate_cas
                .last()
                .expect("non-empty CA bundle")
                .clone(),
        )
        .context("add Platform Intermediate CA trust anchor")?;

    let verifier = ExactRelaySpiffeVerifier::new(roots, relay_spiffe_id.to_owned())?;
    let mut tls_config = rustls::ClientConfig::builder()
        .dangerous()
        .with_custom_certificate_verifier(verifier)
        .with_client_auth_cert(client_chain, private_key)
        .context("build Relay mTLS client config; certificate and key may not match")?;
    tls_config.alpn_protocols = vec![RELAY_ALPN.to_vec()];
    Ok(tls_config)
}

pub fn validate_relay_spiffe_id(spiffe_id: &str) -> Result<()> {
    let path = spiffe_id
        .strip_prefix("spiffe://zecurity.in/relay/")
        .context("Relay SPIFFE ID must use spiffe://zecurity.in/relay/<uuid>")?;
    let parsed = uuid::Uuid::parse_str(path).context("Relay SPIFFE ID must end with a UUID")?;
    if parsed.hyphenated().to_string() != path {
        bail!("Relay SPIFFE UUID must use canonical lowercase hyphenated form");
    }
    Ok(())
}

fn parse_certificates(pem: &[u8], label: &str) -> Result<Vec<CertificateDer<'static>>> {
    let certificates = certs(&mut pem.as_ref())
        .collect::<std::result::Result<Vec<_>, _>>()
        .with_context(|| format!("parse {label} PEM"))?;
    if certificates.is_empty() {
        bail!("{label} PEM contains no certificates");
    }
    Ok(certificates)
}

fn parse_private_key(pem: &[u8]) -> Result<PrivateKeyDer<'static>> {
    private_key(&mut pem.as_ref())
        .context("parse Connector private key PEM")?
        .context("Connector private key PEM contains no private key")
}

async fn write_message<T: Serialize>(send: &mut SendStream, message: &T) -> Result<()> {
    let body = serde_json::to_vec(message).context("encode Relay message")?;
    if body.len() > MAX_MESSAGE_SIZE {
        bail!("Relay message too large: {} bytes", body.len());
    }

    send.write_all(&(body.len() as u32).to_be_bytes())
        .await
        .context("write Relay message length")?;
    send.write_all(&body)
        .await
        .context("write Relay message body")
}

async fn read_message<T: for<'de> Deserialize<'de>>(recv: &mut RecvStream) -> Result<T> {
    let mut length = [0u8; 4];
    recv.read_exact(&mut length)
        .await
        .context("read Relay message length")?;
    let length = u32::from_be_bytes(length) as usize;
    if length > MAX_MESSAGE_SIZE {
        bail!("Relay message too large: {length} bytes");
    }

    let mut body = vec![0u8; length];
    recv.read_exact(&mut body)
        .await
        .context("read Relay message body")?;
    serde_json::from_slice(&body).context("decode Relay message")
}

#[cfg(test)]
mod tests {
    use super::*;

    const RELAY_ID: &str = "550e8400-e29b-41d4-a716-446655440000";

    #[test]
    fn accepts_canonical_relay_spiffe_id() {
        validate_relay_spiffe_id(&format!("spiffe://zecurity.in/relay/{RELAY_ID}")).unwrap();
    }

    #[test]
    fn rejects_wrong_or_noncanonical_relay_spiffe_id() {
        assert!(validate_relay_spiffe_id(&format!(
            "spiffe://workspace.zecurity.in/relay/{RELAY_ID}"
        ))
        .is_err());
        assert!(validate_relay_spiffe_id(
            "spiffe://zecurity.in/relay/550E8400-E29B-41D4-A716-446655440000"
        )
        .is_err());
    }

    #[test]
    fn register_message_matches_relay_protocol() {
        let encoded = serde_json::to_value(HandshakeMsg::Register {
            connector_id: "connector-id",
            spiffe_id: "spiffe://workspace.zecurity.in/connector/connector-id",
        })
        .unwrap();
        assert_eq!(encoded["type"], "register");
        assert_eq!(encoded["connector_id"], "connector-id");
    }
}
