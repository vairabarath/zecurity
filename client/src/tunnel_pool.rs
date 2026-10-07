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
        self.open_probed_stream(addr)
            .await
            .map(|(stream, _probe)| stream)
    }

    /// Fix 05-B: like `open_authenticated_stream`, plus a [`PathProbe`] that
    /// identifies the pooled connection the stream was opened on and its UDP
    /// receive count at that moment.
    ///
    /// The snapshot is taken after `open_bi` (a purely local operation) and
    /// before the caller writes anything, so any datagram counted later was
    /// sent by the peer after this flow started.
    pub async fn open_probed_stream(
        &self,
        addr: SocketAddr,
    ) -> Result<(AuthenticatedStream, PathProbe), TunnelOpenError> {
        let conn = self.get_or_connect(addr).await?;
        let (send, recv) = conn.open_bi().await.map_err(|e| {
            // open_bi failures imply the connection died; report as Connect.
            TunnelOpenError::Connect(anyhow::anyhow!("open QUIC stream: {e}"))
        })?;
        let probe = PathProbe {
            stable_id: conn.stable_id(),
            rx_at_open: conn.stats().udp_rx.datagrams,
        };
        Ok((Box::new(tokio::io::join(recv, send)), probe))
    }

    /// Fix 05-B: drop the pooled connection for `addr` from future selection
    /// when a flow's tunnel handshake stalled on it AND the peer has been
    /// silent since that flow opened its stream.
    ///
    /// Returns `true` only when the entry was removed. All of these must hold:
    /// - the pooled entry is still the connection the stall happened on
    ///   (`stable_id` matches), so a late report never removes a fresh
    ///   replacement;
    /// - its UDP receive count is unchanged since the flow's stream open.
    ///   A live connector ACKs the `TunnelRequest` within milliseconds even
    ///   when the resource behind it is slow, so a healthy connection fails
    ///   this gate and is kept.
    ///
    /// The connection is NOT closed: other flows multiplexed on it hold their
    /// own references and keep running. It closes on its own (idle timeout, or
    /// quinn's implicit close when the last reference drops).
    pub async fn evict_if_silent(&self, addr: SocketAddr, probe: &PathProbe) -> bool {
        let mut conns = self.connections.lock().await;
        let Some(conn) = conns.get(&addr) else {
            return false;
        };
        if conn.stable_id() != probe.stable_id {
            return false;
        }
        if conn.stats().udp_rx.datagrams != probe.rx_at_open {
            return false;
        }
        // Pool eviction only: dropping the map's clone is not a close.
        conns.remove(&addr);
        true
    }
}

/// Fix 05-B: which pooled QUIC connection a direct stream was opened on, and
/// how many UDP datagrams that connection had received at open time.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct PathProbe {
    pub stable_id: usize,
    pub rx_at_open: u64,
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

/// Fix 05-B: stale pooled QUIC connection handling, against REAL quinn
/// endpoints on loopback. A UDP forwarder between the client pool and the
/// test "connector" can be switched to drop everything, which is exactly what
/// an ungracefully killed connector looks like from the client (no
/// CONNECTION_CLOSE, no packets). The server's `max_idle_timeout` bounds how
/// long a silent connection lives (the negotiated idle is the minimum of both
/// sides), so the idle-expiry part of the bug runs in ~2 s instead of 30 s.
#[cfg(test)]
mod stale_pool_tests {
    use super::*;
    use crate::transport::{ClientTransport, DirectOpener};
    use rcgen::{
        BasicConstraints, CertificateParams, DistinguishedName, DnType, ExtendedKeyUsagePurpose,
        IsCa, KeyPair, SanType,
    };
    use std::sync::atomic::{AtomicBool, Ordering};
    use std::time::Duration;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    const SERVER_SPIFFE: &str = "spiffe://ws.test/connector/c1";
    const CLIENT_SPIFFE: &str = "spiffe://ws.test/client/d1";

    fn install_provider() {
        let _ = rustls::crypto::ring::default_provider().install_default();
    }

    struct Pki {
        ca_bundle_pem: String,
        client_cert_pem: String,
        client_key_pem: String,
        server_chain: Vec<CertificateDer<'static>>,
        server_key: PrivateKeyDer<'static>,
    }

    fn ca(name: &str) -> (rcgen::Certificate, KeyPair) {
        let key = KeyPair::generate().unwrap();
        let mut params = CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        params.distinguished_name = DistinguishedName::new();
        params.distinguished_name.push(DnType::CommonName, name);
        let cert = params.self_signed(&key).unwrap();
        (cert, key)
    }

    fn leaf(
        uri: &str,
        eku: ExtendedKeyUsagePurpose,
        issuer: &rcgen::Certificate,
        issuer_key: &KeyPair,
    ) -> (rcgen::Certificate, KeyPair) {
        let key = KeyPair::generate().unwrap();
        let mut params = CertificateParams::new(Vec::<String>::new()).unwrap();
        params
            .subject_alt_names
            .push(SanType::URI(uri.try_into().unwrap()));
        params.extended_key_usages = vec![eku];
        let cert = params.signed_by(&key, issuer, issuer_key).unwrap();
        (cert, key)
    }

    fn pki() -> Pki {
        let (ws_ca, ws_key) = ca("ws-ca");
        let (inter_ca, _inter_key) = ca("platform-intermediate");
        let (client, client_key) = leaf(
            CLIENT_SPIFFE,
            ExtendedKeyUsagePurpose::ClientAuth,
            &ws_ca,
            &ws_key,
        );
        let (server, server_key) = leaf(
            SERVER_SPIFFE,
            ExtendedKeyUsagePurpose::ServerAuth,
            &ws_ca,
            &ws_key,
        );
        Pki {
            ca_bundle_pem: format!("{}{}", ws_ca.pem(), inter_ca.pem()),
            client_cert_pem: client.pem(),
            client_key_pem: client_key.serialize_pem(),
            server_chain: vec![server.der().clone()],
            server_key: PrivateKeyDer::try_from(server_key.serialize_der()).unwrap(),
        }
    }

    /// One test stream on the "connector": echo every chunk back, except a
    /// stream whose data starts with `HOLD`, which is read but never answered
    /// (a live connector whose resource/shield is slow).
    async fn serve_stream(mut send: quinn::SendStream, mut recv: quinn::RecvStream) {
        let mut buf = vec![0u8; 4096];
        let mut hold = false;
        while let Ok(Some(n)) = recv.read(&mut buf).await {
            if buf[..n].starts_with(b"HOLD") {
                hold = true;
            }
            if !hold && send.write_all(&buf[..n]).await.is_err() {
                break;
            }
        }
    }

    fn start_server(pki: &Pki, idle: Duration) -> SocketAddr {
        let mut tls = rustls::ServerConfig::builder()
            .with_no_client_auth()
            .with_single_cert(pki.server_chain.clone(), pki.server_key.clone_key())
            .unwrap();
        tls.alpn_protocols = vec![b"ztna-tunnel-v1".to_vec()];
        let crypto = quinn::crypto::rustls::QuicServerConfig::try_from(tls).unwrap();
        let mut cfg = quinn::ServerConfig::with_crypto(Arc::new(crypto));
        let mut transport = quinn::TransportConfig::default();
        // No server keep-alive: the only packets a live server sends are
        // responses/ACKs to what the client sent, like the real connector.
        transport.max_idle_timeout(Some(quinn::IdleTimeout::try_from(idle).unwrap()));
        cfg.transport_config(Arc::new(transport));
        let endpoint = quinn::Endpoint::server(cfg, "127.0.0.1:0".parse().unwrap()).unwrap();
        let addr = endpoint.local_addr().unwrap();
        tokio::spawn(async move {
            while let Some(incoming) = endpoint.accept().await {
                tokio::spawn(async move {
                    let Ok(conn) = incoming.await else { return };
                    while let Ok((send, recv)) = conn.accept_bi().await {
                        tokio::spawn(serve_stream(send, recv));
                    }
                });
            }
        });
        addr
    }

    /// UDP forwarder client <-> server. `blackhole = true` drops every
    /// datagram in both directions: the connector "died" without a close.
    async fn start_forwarder(server: SocketAddr) -> (SocketAddr, Arc<AtomicBool>) {
        let sock = Arc::new(tokio::net::UdpSocket::bind("127.0.0.1:0").await.unwrap());
        let addr = sock.local_addr().unwrap();
        let blackhole = Arc::new(AtomicBool::new(false));
        let bh = blackhole.clone();
        tokio::spawn(async move {
            let mut buf = vec![0u8; 65536];
            let mut client: Option<SocketAddr> = None;
            while let Ok((n, from)) = sock.recv_from(&mut buf).await {
                if bh.load(Ordering::SeqCst) {
                    continue;
                }
                if from == server {
                    if let Some(c) = client {
                        let _ = sock.send_to(&buf[..n], c).await;
                    }
                } else {
                    client = Some(from);
                    let _ = sock.send_to(&buf[..n], server).await;
                }
            }
        });
        (addr, blackhole)
    }

    struct Lab {
        pool: Arc<TunnelPool>,
        addr: SocketAddr,
        blackhole: Arc<AtomicBool>,
    }

    async fn lab(idle: Duration) -> Lab {
        install_provider();
        let pki = pki();
        let server = start_server(&pki, idle);
        let (addr, blackhole) = start_forwarder(server).await;
        let pool = Arc::new(
            TunnelPool::new(
                &pki.client_cert_pem,
                &pki.client_key_pem,
                None,
                &pki.ca_bundle_pem,
            )
            .unwrap(),
        );
        Lab {
            pool,
            addr,
            blackhole,
        }
    }

    impl Lab {
        fn silence(&self) {
            self.blackhole.store(true, Ordering::SeqCst);
        }
        fn restore(&self) {
            self.blackhole.store(false, Ordering::SeqCst);
        }
        async fn pooled(&self) -> Option<Connection> {
            self.pool.connections.lock().await.get(&self.addr).cloned()
        }
        async fn pooled_id(&self) -> Option<usize> {
            self.pooled().await.map(|c| c.stable_id())
        }
    }

    const WAIT: Duration = Duration::from_secs(5);

    async fn echo(stream: &mut AuthenticatedStream, msg: &[u8]) {
        stream.write_all(msg).await.unwrap();
        let mut buf = vec![0u8; msg.len()];
        tokio::time::timeout(WAIT, stream.read_exact(&mut buf))
            .await
            .expect("echo must arrive")
            .unwrap();
        assert_eq!(buf, msg);
    }

    /// The "TunnelRequest" a stalled flow sends, then a wait well past the
    /// live peer's ACK delay but far below any idle timeout.
    async fn send_request_and_wait(stream: &mut AuthenticatedStream, payload: &[u8]) {
        stream.write_all(payload).await.unwrap();
        tokio::time::sleep(Duration::from_millis(400)).await;
    }

    /// (1) Bug pin. After the connector goes silent the pool still hands out
    /// the same connection; `open_bi` succeeds locally; the request gets no
    /// answer and nothing is received. Only quinn's idle timeout ends it, and
    /// only the next lookup then removes the entry.
    #[tokio::test]
    async fn stale_pooled_connection_is_reused_after_peer_goes_silent() {
        let lab = lab(Duration::from_millis(1500)).await;
        let (mut first, p0) = lab.pool.open_probed_stream(lab.addr).await.unwrap();
        echo(&mut first, b"warm").await;

        lab.silence();
        let (mut stalled, p1) = tokio::time::timeout(WAIT, lab.pool.open_probed_stream(lab.addr))
            .await
            .expect("open on a silent connection is local and immediate")
            .expect("open_bi succeeds on the stale connection");
        assert_eq!(p1.stable_id, p0.stable_id, "the stale connection is reused");
        stalled.write_all(b"request").await.unwrap();
        let mut buf = [0u8; 7];
        assert!(
            tokio::time::timeout(Duration::from_millis(500), stalled.read_exact(&mut buf))
                .await
                .is_err(),
            "a silent connector never answers"
        );
        let conn = lab.pooled().await.expect("still pooled after the stall");
        assert_eq!(conn.stable_id(), p1.stable_id);
        assert_eq!(
            conn.stats().udp_rx.datagrams,
            p1.rx_at_open,
            "nothing received"
        );
        assert!(conn.close_reason().is_none(), "looks healthy to the pool");

        // Only the idle timeout (server-negotiated 1.5 s here, 30 s live)
        // flips close_reason; the entry is removed by the NEXT lookup.
        tokio::time::sleep(Duration::from_millis(2500)).await;
        assert!(conn.close_reason().is_some(), "idle timeout closed it");
        assert_eq!(lab.pooled_id().await, Some(p1.stable_id), "entry lingers");
        let _ = tokio::time::timeout(
            Duration::from_millis(300),
            lab.pool.get_or_connect(lab.addr),
        )
        .await;
        assert!(
            lab.pooled_id().await.is_none(),
            "lookup removed the closed entry"
        );
    }

    /// (2) Silent connector: a stall report evicts the pooled connection
    /// (without closing it) and puts the direct path into the existing
    /// cooldown, so the next flow does not reuse it.
    #[tokio::test]
    async fn silent_stall_evicts_pool_entry_and_cools_down_direct_path() {
        let lab = lab(Duration::from_secs(10)).await;
        let opener: Arc<dyn DirectOpener> = lab.pool.clone();
        let transport = ClientTransport::new(opener, lab.addr, None);

        let (mut warm, _) = transport.open_authenticated_stream_probed().await.unwrap();
        echo(&mut warm, b"warm").await;

        lab.silence();
        let (mut stalled, probe) = transport.open_authenticated_stream_probed().await.unwrap();
        let probe = probe.expect("a pooled direct stream carries a probe");
        send_request_and_wait(&mut stalled, b"request").await;
        let conn = lab.pooled().await.unwrap();
        assert_eq!(conn.stats().udp_rx.datagrams, probe.rx_at_open);

        assert!(
            transport.report_handshake_stall(Some(&probe)).await,
            "evicted"
        );
        assert!(lab.pooled_id().await.is_none(), "pool entry removed");
        assert!(conn.close_reason().is_none(), "eviction is NOT a close");

        let next = tokio::time::timeout(
            Duration::from_millis(200),
            transport.open_authenticated_stream_probed(),
        )
        .await
        .expect("cooldown fails fast instead of reusing the stale connection");
        let err = next.err().expect("direct path is cooling down");
        assert!(err.to_string().contains("cooldown"), "got: {err}");

        // A report without a probe (relay stream) never evicts anything.
        assert!(!transport.report_handshake_stall(None).await);
    }

    /// (3) Safety: a LIVE connector whose resource is slow. The request is
    /// never answered, but the connector ACKs it, so datagrams ARE received
    /// after the probe and the stall report must not evict or cool down.
    #[tokio::test]
    async fn live_connector_with_slow_resource_is_not_evicted() {
        let lab = lab(Duration::from_secs(10)).await;
        let opener: Arc<dyn DirectOpener> = lab.pool.clone();
        let transport = ClientTransport::new(opener, lab.addr, None);

        let (mut warm, _) = transport.open_authenticated_stream_probed().await.unwrap();
        echo(&mut warm, b"warm").await;
        tokio::time::sleep(Duration::from_millis(200)).await; // quiesce

        let (mut slow, probe) = transport.open_authenticated_stream_probed().await.unwrap();
        let probe = probe.unwrap();
        send_request_and_wait(&mut slow, b"HOLD request").await;
        let mut buf = [0u8; 1];
        assert!(
            tokio::time::timeout(Duration::from_millis(300), slow.read(&mut buf))
                .await
                .is_err(),
            "the resource never answers"
        );
        let conn = lab.pooled().await.unwrap();
        assert!(
            conn.stats().udp_rx.datagrams > probe.rx_at_open,
            "the live connector's ACKs were received (real RX activity)"
        );

        assert!(
            !transport.report_handshake_stall(Some(&probe)).await,
            "not evicted"
        );
        assert_eq!(lab.pooled_id().await, Some(probe.stable_id), "still pooled");
        let (mut next, next_probe) = transport
            .open_authenticated_stream_probed()
            .await
            .expect("no cooldown: the direct path is still used");
        assert_eq!(
            next_probe.unwrap().stable_id,
            probe.stable_id,
            "same connection reused"
        );
        echo(&mut next, b"still fine").await;
    }

    /// (4) Eviction removes only the pool's reference. Another flow already
    /// running on the same QUIC connection keeps working, and new flows get a
    /// fresh connection.
    #[tokio::test]
    async fn eviction_does_not_close_connection_and_existing_flow_survives() {
        let lab = lab(Duration::from_secs(10)).await;
        let (mut flow_x, px) = lab.pool.open_probed_stream(lab.addr).await.unwrap();
        echo(&mut flow_x, b"x-1").await;

        lab.silence();
        let (mut flow_y, py) = lab.pool.open_probed_stream(lab.addr).await.unwrap();
        assert_eq!(
            py.stable_id, px.stable_id,
            "both flows share one connection"
        );
        send_request_and_wait(&mut flow_y, b"request").await;
        assert!(lab.pool.evict_if_silent(lab.addr, &py).await, "evicted");
        assert!(lab.pooled_id().await.is_none());

        // The network path comes back: flow X, on the evicted connection,
        // still relays both ways (it was never closed).
        lab.restore();
        echo(&mut flow_x, b"x-2 after eviction").await;

        // New flows use a fresh connection.
        let (mut flow_z, pz) = lab.pool.open_probed_stream(lab.addr).await.unwrap();
        assert_ne!(
            pz.stable_id, px.stable_id,
            "fresh connection after eviction"
        );
        echo(&mut flow_z, b"z").await;
        echo(&mut flow_x, b"x-3").await;
    }

    /// (5) Race: a late stall report for the OLD connection must not evict a
    /// fresh replacement that already occupies the pool slot, even though the
    /// old connection is still silent.
    #[tokio::test]
    async fn late_stall_report_cannot_evict_replacement_connection() {
        let lab = lab(Duration::from_secs(10)).await;
        let (mut warm, _) = lab.pool.open_probed_stream(lab.addr).await.unwrap();
        echo(&mut warm, b"warm").await;

        lab.silence();
        let (mut a, pa) = lab.pool.open_probed_stream(lab.addr).await.unwrap();
        let (mut b, pb) = lab.pool.open_probed_stream(lab.addr).await.unwrap();
        assert_eq!(pa.stable_id, pb.stable_id);
        send_request_and_wait(&mut a, b"req-a").await;
        b.write_all(b"req-b").await.unwrap();

        // Flow A's report evicts; the path recovers; a new flow reconnects.
        assert!(lab.pool.evict_if_silent(lab.addr, &pa).await);
        lab.restore();
        let (mut fresh, pf) = lab.pool.open_probed_stream(lab.addr).await.unwrap();
        echo(&mut fresh, b"fresh").await;
        assert_ne!(pf.stable_id, pa.stable_id);

        // Flow B's report arrives late, for the old connection.
        assert!(
            !lab.pool.evict_if_silent(lab.addr, &pb).await,
            "stale report ignored"
        );
        assert_eq!(
            lab.pooled_id().await,
            Some(pf.stable_id),
            "replacement kept"
        );
        echo(&mut fresh, b"fresh-2").await;
    }
}
