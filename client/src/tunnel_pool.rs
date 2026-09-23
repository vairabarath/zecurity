use std::collections::HashMap;
use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{bail, Context, Result};
use quinn::Connection;
use rustls::client::danger::{HandshakeSignatureValid, ServerCertVerified, ServerCertVerifier};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, ServerName, UnixTime};
use rustls::sign::{CertifiedKey, SingleCertAndKey};
use rustls::{CertificateError, DigitallySignedStruct, Error, SignatureScheme};
use rustls_pemfile::{certs, private_key};
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::sync::Mutex;
use x509_parser::extensions::GeneralName;
use x509_parser::prelude::{FromDer, X509Certificate};

use crate::crl::RevocationStatus;

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
    relay_crl: Option<crate::crl::CrlManager>,
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
            relay_crl: None,
        }))
    }

    pub fn new_relay(
        roots: rustls::RootCertStore,
        expected_spiffe_id: impl Into<String>,
        relay_crl: crate::crl::CrlManager,
    ) -> Result<Arc<Self>> {
        let verifier = Self::new(roots, expected_spiffe_id)?;
        Ok(Arc::new(Self {
            inner: verifier.inner.clone(),
            expected_spiffe_id: verifier.expected_spiffe_id.clone(),
            relay_crl: Some(relay_crl),
        }))
    }

    fn verify_exact_spiffe(&self, end_entity: &CertificateDer<'_>) -> Result<(), Error> {
        let actual = extract_exact_spiffe_uri(end_entity)?;
        if actual == self.expected_spiffe_id {
            return Ok(());
        }
        Err(Error::InvalidCertificate(CertificateError::NotValidForName))
    }

     fn verify_relay_revocation(&self, end_entity: &CertificateDer<'_>) -> Result<(), Error> {
        let Some(manager) = &self.relay_crl else {
            return Ok(());
        };
        let (_, cert) = X509Certificate::from_der(end_entity.as_ref())
            .map_err(|_| Error::InvalidCertificate(CertificateError::BadEncoding))?;
        match manager.check(cert.raw_serial()) {
            RevocationStatus::NotRevoked => Ok(()),
            RevocationStatus::Revoked => Err(Error::General("relay certificate revoked".into())),
            RevocationStatus::Unavailable => {
                Err(Error::General("relay revocation state unavailable".into()))
            }
        }
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
                self.verify_relay_revocation(end_entity)?;
                Ok(verified)
            }
            Err(Error::InvalidCertificate(CertificateError::NotValidForName))
            | Err(Error::InvalidCertificate(CertificateError::NotValidForNameContext { .. })) => {
                self.verify_exact_spiffe(end_entity)?;
                self.verify_relay_revocation(end_entity)?;
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

/// The device's mTLS client identity: its certificate chain plus a signing
/// key that is either an in-memory software key or a TPM-backed signer.
///
/// Both data-plane pools build client auth through this one function so the
/// two key backends stay indistinguishable to everything downstream. It
/// returns rustls' own [`CertifiedKey`], which is exactly what
/// `with_client_auth_cert` constructs internally — that convenience method is
/// literally `CertifiedKey::from_der(...)` followed by
/// `with_client_cert_resolver(SingleCertAndKey::from(...))`. Callers use the
/// resolver form directly because the TPM branch has no private key DER to
/// hand `with_client_auth_cert` in the first place.
///
/// Software devices take the identical path rustls would have taken for them
/// anyway; only the TPM branch differs, and only in *where the signature
/// comes from*.
pub fn build_client_certified_key(
    cert_chain: Vec<CertificateDer<'static>>,
    key_pem: &str,
    tpm_key_material: Option<&str>,
) -> Result<Arc<CertifiedKey>> {
    match tpm_key_material {
        #[cfg(target_os = "linux")]
        Some(serialized) => {
            // No private key is produced, parsed, or passed to rustls here:
            // the signing key is a handle that asks the TPM to sign.
            let key_material = crate::tpm::deserialize_key_material(serialized)?;
            let signing_key = crate::tpm::signing_key(key_material)
                .context("build TPM-backed rustls signing key")?;
            Ok(Arc::new(CertifiedKey {
                cert: cert_chain,
                key: signing_key,
                ocsp: None,
            }))
        }
        #[cfg(not(target_os = "linux"))]
        Some(_) => bail!("device has TPM-backed key material but this build has no TPM support"),
        None => {
            let key_der = parse_private_key_der(key_pem, "device")?;
            let provider = rustls::crypto::CryptoProvider::get_default()
                .context("no rustls crypto provider installed")?;
            CertifiedKey::from_der(cert_chain, key_der, provider)
                .map(Arc::new)
                .context("build software-key client identity")
        }
    }
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
    pub fn new(
        cert_pem: &str,
        key_pem: &str,
        tpm_key_material: Option<&str>,
        ca_pem: &str,
    ) -> Result<Self> {
        let cert_chain = parse_cert_chain(cert_pem, "device certificate")?;
        let trust_domain = extract_client_trust_domain(
            cert_chain
                .first()
                .context("device cert chain is empty")?
                .as_ref(),
        )?;
        let certified_key = build_client_certified_key(cert_chain, key_pem, tpm_key_material)?;
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
            .with_client_cert_resolver(Arc::new(SingleCertAndKey::from(certified_key)));
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

    #[test]
    fn relay_verifier_fails_closed_without_crl() {
        let (cert, _pem) = issue_cert("spiffe://zecurity.in/relay/test");
        let mut roots = rustls::RootCertStore::empty();
        roots.add(cert.clone()).unwrap();
        let verifier = ExactSpiffeVerifier::new_relay(
            roots,
            "spiffe://zecurity.in/relay/test",
            crate::crl::CrlManager::new(),
        )
        .unwrap();
        assert!(verifier.verify_relay_revocation(&cert).is_err());
    }

    #[test]
    fn relay_verifier_rejects_revoked_serial() {
        let (cert, _pem) = issue_cert("spiffe://zecurity.in/relay/test");
        let (_, parsed) = X509Certificate::from_der(cert.as_ref()).unwrap();
        let manager = crate::crl::CrlManager::new();
        manager.install_test_cache(vec![parsed.raw_serial().to_vec()]);
        let mut roots = rustls::RootCertStore::empty();
        roots.add(cert.clone()).unwrap();
        let verifier =
            ExactSpiffeVerifier::new_relay(roots, "spiffe://zecurity.in/relay/test", manager)
                .unwrap();
        assert!(verifier.verify_relay_revocation(&cert).is_err());
    }
}

/// TPM-backed client authentication (PENDING-17). Exercises the real
/// `build_client_certified_key` path all three data-plane call sites use,
/// against real TPM hardware. Skips when no TPM is reachable.
#[cfg(all(test, target_os = "linux"))]
mod tpm_client_auth_tests {
    use super::*;
    use rcgen::{BasicConstraints, CertificateParams, DistinguishedName, DnType, IsCa, KeyPair};

    fn install_provider() {
        let _ = rustls::crypto::ring::default_provider().install_default();
    }

    struct TestPki {
        ca_der: CertificateDer<'static>,
        client_chain: Vec<CertificateDer<'static>>,
        server_chain: Vec<CertificateDer<'static>>,
        server_key_der: PrivateKeyDer<'static>,
    }

    /// Issues a client certificate whose SUBJECT PUBLIC KEY is the TPM's —
    /// `signed_by` takes the subject's public key and the issuer's signing
    /// key separately, so the TPM key pair supplies the former while the
    /// software test CA signs. This is the same shape the real controller
    /// produces from a TPM-backed CSR.
    fn test_pki(tpm_key_pair: &KeyPair) -> TestPki {
        let ca_key = KeyPair::generate().unwrap();
        let mut ca_params = CertificateParams::new(Vec::<String>::new()).unwrap();
        ca_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        ca_params.distinguished_name = DistinguishedName::new();
        ca_params
            .distinguished_name
            .push(DnType::CommonName, "tpm-test-ca");
        let ca_cert = ca_params.self_signed(&ca_key).unwrap();

        let mut client_params = CertificateParams::new(vec!["tpm-client".to_string()]).unwrap();
        client_params.distinguished_name = DistinguishedName::new();
        client_params
            .distinguished_name
            .push(DnType::CommonName, "tpm-client");
        let client_cert = client_params
            .signed_by(tpm_key_pair, &ca_cert, &ca_key)
            .unwrap();

        let server_key = KeyPair::generate().unwrap();
        let server_cert = CertificateParams::new(vec!["localhost".to_string()])
            .unwrap()
            .signed_by(&server_key, &ca_cert, &ca_key)
            .unwrap();

        TestPki {
            ca_der: ca_cert.der().clone(),
            client_chain: vec![client_cert.der().clone()],
            server_chain: vec![server_cert.der().clone()],
            server_key_der: PrivateKeyDer::try_from(server_key.serialize_der()).unwrap(),
        }
    }

    /// Requirements C/E/F: a real mTLS handshake authenticated by a key that
    /// only exists inside the TPM.
    ///
    /// The client identity is built through the same
    /// `build_client_certified_key` the tunnel and relay pools use, and is
    /// given an EMPTY private-key PEM — if any code path still needed private
    /// key material it would fail here rather than silently succeed, which is
    /// the negative assertion requirement E asks for.
    ///
    /// A completed handshake means the server verified a CertificateVerify
    /// signature that the TPM produced.
    #[tokio::test]
    async fn tpm_backed_client_auth_completes_a_real_mtls_handshake() {
        install_provider();
        if !crate::tpm::tpm_available() {
            eprintln!("skipping: no accessible TPM on this machine");
            return;
        }

        let (key_material, _public) = crate::tpm::generate().expect("generate TPM key");
        let serialized =
            crate::tpm::serialize_key_material(&key_material).expect("serialize key material");
        let tpm_key_pair = crate::tpm::load(key_material).expect("load TPM key");
        let pki = test_pki(&tpm_key_pair);

        let mut roots = rustls::RootCertStore::empty();
        roots.add(pki.ca_der.clone()).unwrap();
        let roots = Arc::new(roots);

        // Server demands a client certificate chaining to the test CA.
        let verifier = rustls::server::WebPkiClientVerifier::builder(roots.clone())
            .build()
            .expect("build client cert verifier");
        let server_config = rustls::ServerConfig::builder()
            .with_client_cert_verifier(verifier)
            .with_single_cert(pki.server_chain, pki.server_key_der)
            .expect("server config");

        // Client identity: TPM-backed, and note the empty key PEM.
        let certified_key = build_client_certified_key(pki.client_chain, "", Some(&serialized))
            .expect("build TPM-backed client identity");
        let client_config = rustls::ClientConfig::builder()
            .with_root_certificates(Arc::try_unwrap(roots).unwrap_or_else(|a| (*a).clone()))
            .with_client_cert_resolver(Arc::new(SingleCertAndKey::from(certified_key)));

        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();

        let server = tokio::spawn(async move {
            let acceptor = tokio_rustls::TlsAcceptor::from(Arc::new(server_config));
            let (stream, _) = listener.accept().await.expect("accept");
            let tls = acceptor.accept(stream).await.expect("server handshake");
            // Peer certificates are only present once client auth succeeded.
            let (_, conn) = tls.get_ref();
            conn.peer_certificates()
                .map(|c| c.len())
                .expect("server must have received a client certificate")
        });

        let connector = tokio_rustls::TlsConnector::from(Arc::new(client_config));
        let stream = tokio::net::TcpStream::connect(addr).await.unwrap();
        let server_name = rustls::pki_types::ServerName::try_from("localhost").unwrap();
        let client_tls = connector
            .connect(server_name, stream)
            .await
            .expect("client handshake must succeed using a TPM-held key");

        let (_, client_conn) = client_tls.get_ref();
        assert!(
            client_conn.negotiated_cipher_suite().is_some(),
            "handshake should be complete"
        );

        let peer_cert_count = server.await.expect("server task");
        assert_eq!(peer_cert_count, 1, "server saw the TPM-backed client cert");
    }

    /// Requirement E, stated directly: the software path genuinely needs a
    /// private key, and the TPM path genuinely does not. If the TPM branch
    /// ever started requiring key material, this test would start passing
    /// for the wrong reason — hence asserting the software branch fails on
    /// the same empty input.
    #[test]
    fn tpm_identity_needs_no_private_key_but_software_identity_does() {
        install_provider();
        if !crate::tpm::tpm_available() {
            eprintln!("skipping: no accessible TPM on this machine");
            return;
        }

        let (key_material, _public) = crate::tpm::generate().expect("generate TPM key");
        let serialized =
            crate::tpm::serialize_key_material(&key_material).expect("serialize key material");
        let tpm_key_pair = crate::tpm::load(key_material).expect("load TPM key");
        let pki = test_pki(&tpm_key_pair);

        build_client_certified_key(pki.client_chain.clone(), "", Some(&serialized))
            .expect("TPM identity must build with no private key whatsoever");

        assert!(
            build_client_certified_key(pki.client_chain, "", None).is_err(),
            "software identity must require an actual private key"
        );
    }
}
