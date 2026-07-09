use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{bail, Context, Result};
use quinn::Connection;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, ServerName, UnixTime};
use rustls::{CertificateError, DigitallySignedStruct, Error, SignatureScheme};
use rustls_pemfile::{certs, private_key};
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::sync::Mutex;
use x509_parser::extensions::GeneralName;
use x509_parser::prelude::{FromDer, X509Certificate};

pub trait AuthenticatedIo: AsyncRead + AsyncWrite + Unpin + Send {}
impl<T> AuthenticatedIo for T where T: AsyncRead + AsyncWrite + Unpin + Send {}
pub type AuthenticatedStream = Box<dyn AuthenticatedIo>;

/// Classification of a failure during direct or relay stream establishment.
///
/// `Connect` covers transient/network failures where retrying via the relay
/// path is sensible (DNS, refused, idle timeout, version mismatch, peer
/// `ApplicationClose` before stream open).
///
/// `Authenticate` covers identity/policy failures where retrying via the
/// relay path would just fail the same way (TLS certificate alerts, local
/// verifier rejection, PEM-parse or rustls config-build failure).
#[derive(Debug, thiserror::Error)]
pub enum TunnelOpenError {
    #[error("connect failed: {0}")]
    Connect(#[source] anyhow::Error),
    #[error("authenticate failed: {0}")]
    Authenticate(#[source] anyhow::Error),
}

/// Classify a quinn::ConnectionError as a transport-layer or
/// authentication-layer failure. Inspects both `TransportError` (locally
/// initiated) and `ConnectionClosed` (peer initiated); both carry a
/// `TransportErrorCode`. In QUIC, TLS alerts arrive encoded as
/// `Code(0x100 | alert_byte)`. The nine certificate/auth alerts (42, 43, 44,
/// 45, 46, 48, 49, 113, 114) map to `Authenticate`; everything else is
/// treated as `Connect`.
pub(crate) fn classify_quinn(e: &quinn::ConnectionError) -> TunnelOpenError {
    use quinn::ConnectionError::*;
    let code_bits: Option<u64> = match e {
        TransportError(t) => Some(u64::from(t.code)),
        ConnectionClosed(c) => Some(u64::from(c.error_code)),
        _ => None,
    };
    if let Some(bits) = code_bits {
        if (0x100..0x200).contains(&bits) {
            let alert = (bits - 0x100) as u8;
            if matches!(alert, 42 | 43 | 44 | 45 | 46 | 48 | 49 | 113 | 114) {
                return TunnelOpenError::Authenticate(anyhow::anyhow!("TLS alert {alert}: {e}"));
            }
        }
    }
    TunnelOpenError::Connect(anyhow::anyhow!("{e}"))
}

#[derive(Debug, Clone)]
pub struct ParsedCaBundle {
    pub workspace_ca: CertificateDer<'static>,
    pub intermediate_ca: CertificateDer<'static>,
}

#[derive(Debug)]
pub struct ExactSpiffeVerifier {
    inner: Arc<rustls::client::WebPkiServerVerifier>,
    expected_spiffe_id: String,
}

impl ExactSpiffeVerifier {
    pub fn new(
        roots: rustls::RootCertStore,
        expected_spiffe_id: impl Into<String>,
    ) -> Result<Arc<Self>> {
        let inner = rustls::client::WebPkiServerVerifier::builder(Arc::new(roots))
            .build()
            .context("build certificate chain verifier")?;
        Ok(Arc::new(Self {
            inner,
            expected_spiffe_id: expected_spiffe_id.into(),
        }))
    }

    fn verify_exact_spiffe(&self, end_entity: &CertificateDer<'_>) -> Result<(), Error> {
        let actual = extract_exact_spiffe_uri(end_entity)?;
        if actual == self.expected_spiffe_id {
            return Ok(());
        }
        Err(Error::InvalidCertificate(CertificateError::NotValidForName))
    }
}

impl ServerCertVerifier for ExactSpiffeVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        server_name: &ServerName<'_>,
        ocsp_response: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, Error> {
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
            Err(Error::InvalidCertificate(CertificateError::NotValidForName))
            | Err(Error::InvalidCertificate(CertificateError::NotValidForNameContext { .. })) => {
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
    ) -> Result<HandshakeSignatureValid, Error> {
        self.inner.verify_tls12_signature(message, cert, dss)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        self.inner.verify_tls13_signature(message, cert, dss)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.inner.supported_verify_schemes()
    }
}

pub fn parse_cert_chain(cert_pem: &str, label: &str) -> Result<Vec<CertificateDer<'static>>> {
    let mut reader = std::io::BufReader::new(cert_pem.as_bytes());
    let chain = certs(&mut reader)
        .collect::<Result<Vec<_>, _>>()
        .with_context(|| format!("parse {label} PEM"))?;
    if chain.is_empty() {
        bail!("{label} PEM contains no certificates");
    }
    Ok(chain)
}

pub fn parse_private_key_der(key_pem: &str, label: &str) -> Result<PrivateKeyDer<'static>> {
    let mut reader = std::io::BufReader::new(key_pem.as_bytes());
    private_key(&mut reader)
        .with_context(|| format!("parse {label} private key PEM"))?
        .with_context(|| format!("{label} private key PEM contains no private key"))
}

pub fn parse_ca_bundle(ca_pem: &str) -> Result<ParsedCaBundle> {
    let mut reader = std::io::BufReader::new(ca_pem.as_bytes());
    let certs = certs(&mut reader)
        .collect::<Result<Vec<_>, _>>()
        .context("parse CA bundle PEM")?;
    if certs.len() < 2 {
        bail!("CA bundle must contain workspace CA followed by platform intermediate CA");
    }
    Ok(ParsedCaBundle {
        workspace_ca: certs[0].clone(),
        intermediate_ca: certs[1].clone(),
    })
}

pub fn root_store_from_cert(
    cert: &CertificateDer<'static>,
    label: &str,
) -> Result<rustls::RootCertStore> {
    let mut roots = rustls::RootCertStore::empty();
    roots
        .add(cert.clone())
        .with_context(|| format!("add {label} trust anchor"))?;
    Ok(roots)
}

pub fn extract_client_trust_domain(cert_der: &[u8]) -> Result<String> {
    let (_, cert) = X509Certificate::from_der(cert_der)
        .map_err(|e| anyhow::anyhow!("parse device certificate DER: {:?}", e))?;
    let san = cert
        .subject_alternative_name()
        .map_err(|e| anyhow::anyhow!("parse device certificate SAN: {:?}", e))?
        .context("device certificate has no SAN extension")?;

    for name in &san.value.general_names {
        if let GeneralName::URI(uri) = name {
            if let Some(rest) = uri.strip_prefix("spiffe://") {
                if let Some((trust_domain, path)) = rest.split_once('/') {
                    if path.starts_with("client/") {
                        return Ok(trust_domain.to_string());
                    }
                }
            }
        }
    }

    bail!("device certificate has no client SPIFFE URI SAN")
}

pub fn extract_exact_spiffe_uri(cert_der: &CertificateDer<'_>) -> Result<String, Error> {
    let (_, cert) = X509Certificate::from_der(cert_der.as_ref())
        .map_err(|_| Error::InvalidCertificate(CertificateError::BadEncoding))?;
    let san = cert
        .subject_alternative_name()
        .map_err(|_| Error::InvalidCertificate(CertificateError::BadEncoding))?
        .ok_or(Error::InvalidCertificate(CertificateError::NotValidForName))?;

    let uris: Vec<&str> = san
        .value
        .general_names
        .iter()
        .filter_map(|name| match name {
            GeneralName::URI(uri) => Some(*uri),
            _ => None,
        })
        .collect();
    if uris.len() != 1 {
        return Err(Error::InvalidCertificate(CertificateError::NotValidForName));
    }
    Ok(uris[0].to_string())
}

pub struct TunnelPool {
    connections: Arc<Mutex<HashMap<SocketAddr, Connection>>>,
    endpoint: quinn::Endpoint,
}

impl TunnelPool {
    pub fn new(cert_pem: &str, key_pem: &str, ca_pem: &str) -> Result<Self> {
        let cert_chain = parse_cert_chain(cert_pem, "device certificate")?;
        let trust_domain = extract_client_trust_domain(
            cert_chain
                .first()
                .context("device cert chain is empty")?
                .as_ref(),
        )?;
        let private_key = parse_private_key_der(key_pem, "device")?;
        let ca_bundle = parse_ca_bundle(ca_pem)?;
        let expected = format!("spiffe://{trust_domain}/connector/");

        let mut roots = rustls::RootCertStore::empty();
        roots
            .add(ca_bundle.workspace_ca)
            .context("add workspace CA trust anchor")?;
        roots
            .add(ca_bundle.intermediate_ca)
            .context("add intermediate CA trust anchor")?;

        let mut tls_config = rustls::ClientConfig::builder()
            .dangerous()
            .with_custom_certificate_verifier(PrefixSpiffeVerifier::new(roots, expected)?)
            .with_client_auth_cert(cert_chain, private_key)
            .context("build tunnel rustls client config")?;
        tls_config.alpn_protocols = vec![b"ztna-tunnel-v1".to_vec()];

        let quic_client_cfg = quinn_proto::crypto::rustls::QuicClientConfig::try_from(tls_config)
            .map_err(|e| anyhow::anyhow!("build QUIC client config: {}", e))?;
        let mut client_cfg = quinn::ClientConfig::new(Arc::new(quic_client_cfg));
        let mut transport = quinn::TransportConfig::default();
        transport.keep_alive_interval(Some(std::time::Duration::from_secs(10)));
        client_cfg.transport_config(Arc::new(transport));

        let mut endpoint = quinn::Endpoint::client("0.0.0.0:0".parse().unwrap())
            .context("bind QUIC client endpoint")?;
        endpoint.set_default_client_config(client_cfg);

        Ok(Self {
            connections: Arc::new(Mutex::new(HashMap::new())),
            endpoint,
        })
    }

    pub async fn get_or_connect(&self, addr: SocketAddr) -> Result<Connection, TunnelOpenError> {
        // Lock briefly only for the hit-check; release before the handshake await
        // so the 2-second timeout cancellation cannot leave the lock held.
        {
            let mut conns = self.connections.lock().await;
            if let Some(conn) = conns.get(&addr) {
                if conn.close_reason().is_none() {
                    return Ok(conn.clone());
                }
                conns.remove(&addr);
            }
        }

        let new_conn = self
            .endpoint
            .connect(addr, "connector")
            .map_err(|e| {
                TunnelOpenError::Connect(anyhow::Error::from(e).context("initiate QUIC connection"))
            })?
            .await
            .map_err(|e| classify_quinn(&e))?;

        // Re-acquire and double-check in case a concurrent caller raced and
        // already inserted a healthy connection while we were handshaking.
        let mut conns = self.connections.lock().await;
        if let Some(existing) = conns.get(&addr) {
            if existing.close_reason().is_none() {
                new_conn.close(0u32.into(), b"raced");
                return Ok(existing.clone());
            }
        }
        conns.insert(addr, new_conn.clone());
        Ok(new_conn)
    }

    /// Open a byte-zero authenticated bidirectional stream to the connector.
    ///
    /// "Byte-zero" means the TLS/QUIC handshake is complete but no application
    /// bytes (no `TunnelRequest`) have been written and no `TunnelResponse`
    /// has been read. This is the fallback boundary: failures before this
    /// point may be retried via the relay path; failures after this point
    /// (ACL denial, malformed TunnelResponse) live in `net_stack` and must
    /// never trigger a relay retry.
    pub async fn open_authenticated_stream(
        &self,
        addr: SocketAddr,
    ) -> Result<AuthenticatedStream, TunnelOpenError> {
        let conn = self.get_or_connect(addr).await?;
        let (send, recv) = conn.open_bi().await.map_err(|e| {
            // open_bi failures imply the connection died; report as Connect.
            TunnelOpenError::Connect(anyhow::anyhow!("open QUIC stream: {e}"))
        })?;
        Ok(Box::new(tokio::io::join(recv, send)))
    }
}

#[derive(Debug)]
struct PrefixSpiffeVerifier {
    inner: Arc<rustls::client::WebPkiServerVerifier>,
    expected_prefix: String,
}

impl PrefixSpiffeVerifier {
    fn new(roots: rustls::RootCertStore, expected_prefix: String) -> Result<Arc<Self>> {
        let inner = rustls::client::WebPkiServerVerifier::builder(Arc::new(roots))
            .build()
            .context("build prefix SPIFFE verifier")?;
        Ok(Arc::new(Self {
            inner,
            expected_prefix,
        }))
    }

    fn verify_spiffe_prefix(&self, end_entity: &CertificateDer<'_>) -> Result<(), Error> {
        let actual = extract_exact_spiffe_uri(end_entity)?;
        if actual.starts_with(&self.expected_prefix) {
            return Ok(());
        }
        Err(Error::InvalidCertificate(CertificateError::NotValidForName))
    }
}

impl ServerCertVerifier for PrefixSpiffeVerifier {
    fn verify_server_cert(
        &self,
        end_entity: &CertificateDer<'_>,
        intermediates: &[CertificateDer<'_>],
        server_name: &ServerName<'_>,
        ocsp_response: &[u8],
        now: UnixTime,
    ) -> Result<ServerCertVerified, Error> {
        match self.inner.verify_server_cert(
            end_entity,
            intermediates,
            server_name,
            ocsp_response,
            now,
        ) {
            Ok(verified) => {
                self.verify_spiffe_prefix(end_entity)?;
                Ok(verified)
            }
            Err(Error::InvalidCertificate(CertificateError::NotValidForName))
            | Err(Error::InvalidCertificate(CertificateError::NotValidForNameContext { .. })) => {
                self.verify_spiffe_prefix(end_entity)?;
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
    ) -> Result<HandshakeSignatureValid, Error> {
        self.inner.verify_tls12_signature(message, cert, dss)
    }

    fn verify_tls13_signature(
        &self,
        message: &[u8],
        cert: &CertificateDer<'_>,
        dss: &DigitallySignedStruct,
    ) -> Result<HandshakeSignatureValid, Error> {
        self.inner.verify_tls13_signature(message, cert, dss)
    }

    fn supported_verify_schemes(&self) -> Vec<SignatureScheme> {
        self.inner.supported_verify_schemes()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use rcgen::{CertificateParams, KeyPair, SanType};
    use std::sync::Once;

    fn install_crypto_provider() {
        static INSTALL: Once = Once::new();
        INSTALL.call_once(|| {
            let _ = rustls::crypto::ring::default_provider().install_default();
        });
    }

    fn issue_cert(uri: &str) -> (CertificateDer<'static>, String) {
        install_crypto_provider();
        let key = KeyPair::generate().unwrap();
        let mut params = CertificateParams::default();
        params
            .subject_alt_names
            .push(SanType::URI(uri.try_into().unwrap()));
        let cert = params.self_signed(&key).unwrap();
        (cert.der().clone(), cert.pem())
    }

    #[test]
    fn ca_bundle_requires_workspace_and_intermediate() {
        let (_cert, pem) = issue_cert("spiffe://ws/client/abc");
        assert!(parse_ca_bundle(&pem).is_err());
    }

    #[test]
    fn exact_spiffe_verifier_rejects_wrong_spiffe() {
        let (cert, _pem) = issue_cert("spiffe://ws/connector/right");
        let mut roots = rustls::RootCertStore::empty();
        roots.add(cert.clone()).unwrap();
        let verifier = ExactSpiffeVerifier::new(roots, "spiffe://ws/connector/wrong").unwrap();
        assert!(verifier.verify_exact_spiffe(&cert).is_err());
    }

    #[test]
    fn extract_client_trust_domain_reads_client_uri() {
        let (cert, _pem) = issue_cert("spiffe://workspace.zecurity.in/client/123");
        let trust_domain = extract_client_trust_domain(cert.as_ref()).unwrap();
        assert_eq!(trust_domain, "workspace.zecurity.in");
    }
}
