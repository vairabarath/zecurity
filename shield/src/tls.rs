// tls.rs — SPIFFE-based connector certificate verification for mTLS
//
// WHY THIS EXISTS:
//   mTLS (mutual TLS) means both sides present certificates.
//   After the TLS handshake, we know the peer has a cert signed by our CA.
//   But that's not enough — any cert signed by the same CA would pass.
//
//   We need to verify the peer is specifically the CONNECTOR we enrolled with,
//   not just "any valid cert from our CA". We do this by checking the SPIFFE URI
//   in the peer's certificate SAN (Subject Alternative Name) extension.
//
// HOW IT WORKS:
//   1. Shield connects to connector :9091 with mTLS
//   2. TLS handshake completes — both sides verify each other's CA chain
//   3. Shield calls verify_connector_spiffe() with the connector's cert DER
//   4. We parse the cert, find the URI SAN, compare to expected SPIFFE ID
//   5. Expected ID is built from state.json: "spiffe://<trust_domain>/connector/<connector_id>"
//   6. If mismatch → reject connection (could be a rogue server)
//
// CALLED BY:
//   control_stream.rs — while establishing mTLS channel to connector :9091

use std::sync::Arc;

use anyhow::{bail, Context, Result};
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::client::WebPkiServerVerifier;
use rustls::pki_types::pem::PemObject;
use rustls::pki_types::{CertificateDer, PrivateKeyDer, ServerName, UnixTime};
use rustls::{
    CertificateError, DigitallySignedStruct, Error as TlsError, RootCertStore, SignatureScheme,
};
use tonic::transport::{Channel, Endpoint};
use x509_parser::prelude::*;

use crate::appmeta;

/// Verify that a connector's certificate contains the expected SPIFFE URI.
///
/// Parameters:
///   - `cert_der`    — DER-encoded peer certificate from the TLS handshake
///   - `connector_id` — the connector UUID from state.json
///   - `trust_domain` — the workspace trust domain from state.json
///                      (e.g. "ws-acme.zecurity.in")
///
/// Returns `Ok(())` if the SPIFFE ID matches, `Err` with a descriptive
/// message if it doesn't.
///
/// Example expected SPIFFE ID:
///   "spiffe://ws-acme.zecurity.in/connector/abc-123"
pub fn verify_connector_spiffe(
    cert_der: &[u8],
    connector_id: &str,
    trust_domain: &str,
) -> Result<()> {
    // Build the expected SPIFFE URI from our enrolled state.
    // This is what the connector's cert MUST contain.
    let expected = appmeta::connector_spiffe_id(trust_domain, connector_id);

    // Parse the DER certificate
    let (_, cert) = X509Certificate::from_der(cert_der)
        .context("failed to parse connector certificate as X.509")?;

    // Find the Subject Alternative Name extension (OID 2.5.29.17).
    // All SPIFFE certificates must have a URI SAN — it's the identity carrier.
    let san = cert
        .subject_alternative_name()
        .context("failed to parse SAN extension from connector certificate")?
        .ok_or_else(|| anyhow::anyhow!("connector certificate has no SAN extension"))?;

    // Search for a URI SAN matching our expected connector SPIFFE ID
    for name in &san.value.general_names {
        if let GeneralName::URI(uri) = name {
            if *uri == expected {
                tracing::debug!(
                    spiffe_id = %uri,
                    "connector SPIFFE identity verified"
                );
                return Ok(());
            }
        }
    }

    // No matching URI SAN found — reject the connection
    bail!(
        "connector certificate does not contain expected SPIFFE URI '{}'. \
         Found SANs: {:?}. This may be a rogue server signed by the same CA.",
        expected,
        san.value
            .general_names
            .iter()
            .filter_map(|n| if let GeneralName::URI(u) = n {
                Some(*u)
            } else {
                None
            })
            .collect::<Vec<_>>()
    );
}

// ── SPIFFE-aware mTLS connector for Control/RenewCert to Connector :9091 ─────
//
// The connector cert only has clientAuth EKU (not serverAuth) because it was
// signed for connector→controller use. WebPkiServerVerifier enforces serverAuth,
// so we use a custom verifier that:
//   1. Runs WebPki chain validation (catches expired, bad signature, unknown CA)
//   2. Ignores name-mismatch and purpose errors (expected for SPIFFE/clientAuth certs)
//   3. Verifies the SPIFFE URI matches the enrolled connector identity

#[derive(Debug)]
pub struct SpiffeConnectorVerifier {
    inner: Arc<WebPkiServerVerifier>,
    connector_id: String,
    trust_domain: String,
}

impl SpiffeConnectorVerifier {
    pub fn new(ca_pem: &[u8], connector_id: String, trust_domain: String) -> Result<Self> {
        let mut roots = RootCertStore::empty();
        for cert in CertificateDer::pem_slice_iter(ca_pem) {
            let cert = cert.context("failed to parse CA cert from PEM")?;
            roots
                .add(cert)
                .context("failed to add cert to root store")?;
        }
        let inner = WebPkiServerVerifier::builder(Arc::new(roots))
            .build()
            .map_err(|e| anyhow::anyhow!("failed to build WebPki verifier: {}", e))?;
        Ok(Self {
            inner,
            connector_id,
            trust_domain,
        })
    }
}

impl ServerCertVerifier for SpiffeConnectorVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        _server_name: &ServerName<'_>,
        ocsp_response: &[u8],
        now: UnixTime,
    ) -> std::result::Result<ServerCertVerified, TlsError> {
        use rustls::pki_types::DnsName;
        let dummy = ServerName::DnsName(
            DnsName::try_from("spiffe.connector.local")
                .map_err(|e| TlsError::General(e.to_string()))?
                .to_owned(),
        );
        match self
            .inner
            .verify_server_cert(end_entity, intermediates, &dummy, ocsp_response, now)
        {
            Ok(_) => {}
            // Name / purpose / extension errors are expected for SPIFFE certs:
            //   - NotValidForName / NotValidForNameContext: SPIFFE URI SAN doesn't match a DNS name
            //   - InvalidPurpose: connector cert uses clientAuth EKU, not serverAuth
            //   - UnhandledCriticalExtension: webpki may not recognize URI SAN as critical
            // Security is provided by verify_connector_spiffe() below.
            Err(TlsError::InvalidCertificate(CertificateError::NotValidForName))
            | Err(TlsError::InvalidCertificate(CertificateError::NotValidForNameContext {
                ..
            }))
            | Err(TlsError::InvalidCertificate(CertificateError::InvalidPurpose))
            | Err(TlsError::InvalidCertificate(CertificateError::UnhandledCriticalExtension)) => {}
            Err(e) => return Err(e),
        }
        verify_connector_spiffe(end_entity.as_ref(), &self.connector_id, &self.trust_domain)
            .map_err(|e| TlsError::General(e.to_string()))?;
        Ok(ServerCertVerified::assertion())
    }

    fn verify_tls12_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> std::result::Result<HandshakeSignatureValid, TlsError> {
        self.inner.verify_tls12_signature(message, cert, dss)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> std::result::Result<HandshakeSignatureValid, TlsError> {
        self.inner.verify_tls13_signature(message, cert, dss)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.inner.supported_verify_schemes()
    }
}

// ── Custom tower Service that wraps tokio-rustls ──────────────────────────────
//
// Tonic 0.14's ClientTlsConfig has no `rustls_client_config()` escape hatch,
// so we bypass it entirely via connect_with_connector(). The URI scheme is
// `http://` so tonic doesn't add its own TLS layer on top of ours.

struct SpiffeConnectorService {
    tls: Arc<rustls::ClientConfig>,
    connector_addr: String,
}

impl tower_service::Service<http::Uri> for SpiffeConnectorService {
    type Response = hyper_util::rt::TokioIo<tokio_rustls::client::TlsStream<tokio::net::TcpStream>>;
    type Error = Box<dyn std::error::Error + Send + Sync>;
    type Future = std::pin::Pin<
        Box<
            dyn std::future::Future<Output = std::result::Result<Self::Response, Self::Error>>
                + Send,
        >,
    >;

    fn poll_ready(
        &mut self,
        _cx: &mut std::task::Context<'_>,
    ) -> std::task::Poll<std::result::Result<(), Self::Error>> {
        std::task::Poll::Ready(Ok(()))
    }

    fn call(&mut self, _uri: http::Uri) -> Self::Future {
        let tls = self.tls.clone();
        let addr = self.connector_addr.clone();
        Box::pin(async move {
            let host = addr.rsplit_once(':').map(|(h, _)| h).unwrap_or(&addr);
            let server_name = ServerName::try_from(host)
                .map(|n| n.to_owned())
                .unwrap_or_else(|_| {
                    ServerName::DnsName(
                        rustls::pki_types::DnsName::try_from("connector.local")
                            .expect("static DNS name is valid")
                            .to_owned(),
                    )
                });
            tracing::debug!(addr = %addr, "TCP connecting to connector");
            let tcp = tokio::net::TcpStream::connect(&addr).await?;
            tracing::debug!(addr = %addr, "TLS handshake starting");
            let stream = match tokio_rustls::TlsConnector::from(tls)
                .connect(server_name, tcp)
                .await
            {
                Ok(s) => {
                    tracing::debug!(addr = %addr, "TLS handshake complete");
                    s
                }
                Err(e) => {
                    tracing::warn!(addr = %addr, error = %e, "mTLS handshake with connector failed");
                    return Err(e.into());
                }
            };
            Ok(hyper_util::rt::TokioIo::new(stream))
        })
    }
}

/// Build a tonic Channel to Connector :9091 using SPIFFE-aware mTLS.
///
/// Bypasses tonic's ClientTlsConfig (no rustls escape hatch in 0.14) via
/// connect_with_connector with a custom tokio-rustls service. SpiffeConnectorVerifier
/// handles chain validation + SPIFFE URI check without requiring serverAuth EKU.
pub async fn build_connector_channel(
    ca_pem: &[u8],
    cert_pem: &[u8],
    key_pem: &[u8],
    connector_id: &str,
    trust_domain: &str,
    connector_addr: &str,
) -> Result<Channel> {
    let verifier =
        SpiffeConnectorVerifier::new(ca_pem, connector_id.to_string(), trust_domain.to_string())?;

    let client_certs: Vec<CertificateDer<'static>> = CertificateDer::pem_slice_iter(cert_pem)
        .map(|r| r.map_err(|e| anyhow::anyhow!("failed to parse client cert: {}", e)))
        .collect::<Result<_>>()?;

    let key = PrivateKeyDer::from_pem_slice(key_pem)
        .map_err(|e| anyhow::anyhow!("failed to parse private key: {}", e))?;

    let mut rustls_cfg = rustls::ClientConfig::builder()
        .dangerous()
        .with_custom_certificate_verifier(Arc::new(verifier))
        .with_client_auth_cert(client_certs, key)
        .context("failed to build mTLS rustls config")?;
    rustls_cfg.alpn_protocols = vec![b"h2".to_vec()];

    // http:// so tonic doesn't add its own TLS layer — TLS is in SpiffeConnectorService
    let grpc_addr = format!("http://{}", connector_addr);
    Endpoint::from_shared(grpc_addr)
        .context("invalid connector gRPC address")?
        .connect_with_connector(SpiffeConnectorService {
            tls: Arc::new(rustls_cfg),
            connector_addr: connector_addr.to_string(),
        })
        .await
        .context("failed to connect to connector :9091")
}
