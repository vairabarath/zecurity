//! Test-only PKI (rcgen 0.14): Platform Intermediate → Workspace CA →
//! connector / client leaves. Every leaf issuance gets a fresh serial; the
//! connector key stays the same so renewals are same-key.

use std::fs;
use std::sync::{Arc, Once};

use rcgen::{
    BasicConstraints, CertificateParams, DistinguishedName, DnType, ExtendedKeyUsagePurpose, IsCa,
    Issuer, KeyPair, KeyUsagePurpose, SanType, SerialNumber, PKCS_ECDSA_P384_SHA384,
};
use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
use time::{Duration, OffsetDateTime};

use crate::tls::cert_holder::{serial_hex, CertHolder};

pub const CONNECTOR_ID: &str = "0b7e6a2c-4f1d-4c3a-9e2b-8f5d7a1c3e90";

/// Trust domain of the test PKI's SPIFFE IDs.
pub const TRUST_DOMAIN: &str = "ws-test.zecurity.in";

fn shield_spiffe_id(shield_id: &str) -> String {
    format!("spiffe://{TRUST_DOMAIN}/shield/{shield_id}")
}

pub fn install_crypto_provider() {
    static INSTALL: Once = Once::new();
    INSTALL.call_once(|| {
        let _ = rustls::crypto::ring::default_provider().install_default();
    });
}

fn random_serial() -> SerialNumber {
    let mut bytes = uuid::Uuid::new_v4().as_bytes().to_vec();
    bytes[0] &= 0x7f;
    bytes[0] |= 0x01;
    SerialNumber::from_slice(&bytes)
}

fn ca_params(cn: &str) -> CertificateParams {
    let mut params = CertificateParams::default();
    let mut dn = DistinguishedName::new();
    dn.push(DnType::CommonName, cn);
    params.distinguished_name = dn;
    params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    params.key_usages = vec![
        KeyUsagePurpose::KeyCertSign,
        KeyUsagePurpose::CrlSign,
        KeyUsagePurpose::DigitalSignature,
    ];
    params.serial_number = Some(random_serial());
    params
}

fn new_key() -> KeyPair {
    KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap()
}

pub struct TestPki {
    ws_params: CertificateParams,
    ws_key: KeyPair,
    connector_key: KeyPair,
    pub spiffe_id: String,
    pub workspace_ca_pem: String,
    pub intermediate_pem: String,
}

impl Default for TestPki {
    fn default() -> Self {
        Self::new()
    }
}

impl TestPki {
    pub fn new() -> Self {
        install_crypto_provider();
        let int_key = new_key();
        let int_params = ca_params("Test Platform Intermediate");
        let int_cert = int_params.self_signed(&int_key).unwrap();

        let ws_key = new_key();
        let mut ws_params = ca_params("Test Workspace CA");
        ws_params.subject_alt_names = vec![SanType::URI("tenant:ws-test".try_into().unwrap())];
        let ws_cert = ws_params
            .signed_by(&ws_key, &Issuer::from_params(&int_params, &int_key))
            .unwrap();

        Self {
            ws_params,
            ws_key,
            connector_key: new_key(),
            spiffe_id: crate::appmeta::connector_spiffe_id("ws-test.zecurity.in", CONNECTOR_ID),
            workspace_ca_pem: ws_cert.pem(),
            intermediate_pem: int_cert.pem(),
        }
    }

    fn leaf(
        &self,
        key: &KeyPair,
        spiffe_id: &str,
        client_only: bool,
        offset: i64,
        lifetime: i64,
    ) -> String {
        let mut params = CertificateParams::default();
        params.subject_alt_names = vec![
            SanType::URI(spiffe_id.try_into().unwrap()),
            SanType::DnsName("localhost".try_into().unwrap()),
        ];
        params.extended_key_usages = if client_only {
            vec![ExtendedKeyUsagePurpose::ClientAuth]
        } else {
            vec![
                ExtendedKeyUsagePurpose::ServerAuth,
                ExtendedKeyUsagePurpose::ClientAuth,
            ]
        };
        params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        let not_before = OffsetDateTime::now_utc() + Duration::seconds(offset);
        params.not_before = not_before;
        params.not_after = not_before + Duration::seconds(lifetime);
        params.serial_number = Some(random_serial());
        params
            .signed_by(key, &Issuer::from_params(&self.ws_params, &self.ws_key))
            .unwrap()
            .pem()
    }

    /// Connector leaf (same key every time, new serial).
    pub fn connector_leaf(&self, offset: i64, lifetime: i64) -> String {
        self.leaf(
            &self.connector_key,
            &self.spiffe_id,
            false,
            offset,
            lifetime,
        )
    }

    pub fn leaf_for_spiffe(&self, spiffe_id: &str, offset: i64, lifetime: i64) -> String {
        self.leaf(&self.connector_key, spiffe_id, false, offset, lifetime)
    }

    pub fn leaf_with_new_key(&self, offset: i64, lifetime: i64) -> String {
        self.leaf(&new_key(), &self.spiffe_id, false, offset, lifetime)
    }

    /// Device/client chain accepted by the connector's mTLS listeners.
    pub fn client_chain(&self) -> (Vec<CertificateDer<'static>>, PrivateKeyDer<'static>) {
        self.chain_for("spiffe://ws-test.zecurity.in/client/9b2d5cae-5820-4702-adf4-231680852b11")
    }

    /// Shield client chain (leaf + Workspace CA) for the :9091 server.
    pub fn shield_chain(
        &self,
        shield_id: &str,
    ) -> (Vec<CertificateDer<'static>>, PrivateKeyDer<'static>) {
        self.chain_for(&shield_spiffe_id(shield_id))
    }

    /// Shield client identity as PEM (chain, key), for tonic's ClientTlsConfig.
    pub fn shield_identity_pem(&self, shield_id: &str) -> (String, String) {
        let (leaf_pem, key) = self.client_leaf(&shield_spiffe_id(shield_id));
        (
            format!(
                "{}\n{}",
                leaf_pem.trim_end(),
                self.workspace_ca_pem.trim_end()
            ),
            key.serialize_pem(),
        )
    }

    /// A client-auth leaf with a fresh key.
    fn client_leaf(&self, spiffe_id: &str) -> (String, KeyPair) {
        let key = new_key();
        let leaf_pem = self.leaf(&key, spiffe_id, true, -60, 3600);
        (leaf_pem, key)
    }

    fn chain_for(&self, spiffe_id: &str) -> (Vec<CertificateDer<'static>>, PrivateKeyDer<'static>) {
        let (leaf_pem, key) = self.client_leaf(spiffe_id);
        let mut chain: Vec<CertificateDer<'static>> =
            rustls_pemfile::certs(&mut leaf_pem.as_bytes())
                .collect::<Result<_, _>>()
                .unwrap();
        chain.extend(
            rustls_pemfile::certs(&mut self.workspace_ca_pem.as_bytes())
                .collect::<Result<Vec<_>, _>>()
                .unwrap(),
        );
        (
            chain,
            PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der())),
        )
    }

    /// Root store trusting this PKI's Workspace CA (connector server certs).
    pub fn workspace_roots(&self) -> rustls::RootCertStore {
        let mut roots = rustls::RootCertStore::empty();
        for cert in rustls_pemfile::certs(&mut self.workspace_ca_pem.as_bytes()) {
            roots.add(cert.unwrap()).unwrap();
        }
        roots
    }

    /// State dir in the enrollment layout, loaded into a CertHolder.
    pub fn holder(&self, offset: i64, lifetime: i64) -> (tempfile::TempDir, Arc<CertHolder>) {
        let dir = tempfile::tempdir().unwrap();
        let leaf = self.connector_leaf(offset, lifetime);
        fs::write(
            dir.path().join("connector.key"),
            self.connector_key.serialize_pem(),
        )
        .unwrap();
        fs::write(
            dir.path().join("connector.crt"),
            format!("{}\n{}", leaf.trim_end(), self.workspace_ca_pem.trim_end()),
        )
        .unwrap();
        fs::write(
            dir.path().join("workspace_ca.crt"),
            format!(
                "{}\n{}",
                self.workspace_ca_pem.trim_end(),
                self.intermediate_pem.trim_end()
            ),
        )
        .unwrap();
        let holder = CertHolder::load(dir.path().to_str().unwrap(), &self.spiffe_id).unwrap();
        (dir, holder)
    }

    /// Install a freshly issued same-key renewal into `holder`.
    pub fn renew(
        &self,
        holder: &CertHolder,
        lifetime: i64,
    ) -> Arc<crate::tls::cert_holder::CertMaterial> {
        holder
            .install_renewed(
                self.connector_leaf(-60, lifetime).as_bytes(),
                self.workspace_ca_pem.as_bytes(),
                self.intermediate_pem.as_bytes(),
            )
            .unwrap()
    }
}

// ---- handshake helpers ---------------------------------------------------------

pub const TUNNEL_ALPN: &[u8] = b"ztna-tunnel-v1";

impl TestPki {
    /// Device-style mTLS client config trusting this PKI's Workspace CA.
    pub fn client_tls_config(&self, tls13_only: bool) -> Arc<rustls::ClientConfig> {
        let (chain, key) = self.client_chain();
        let builder = if tls13_only {
            rustls::ClientConfig::builder_with_protocol_versions(&[&rustls::version::TLS13])
        } else {
            rustls::ClientConfig::builder()
        };
        let mut cfg = builder
            .with_root_certificates(self.workspace_roots())
            .with_client_auth_cert(chain, key)
            .unwrap();
        cfg.alpn_protocols = vec![TUNNEL_ALPN.to_vec()];
        Arc::new(cfg)
    }

    /// QUIC client endpoint presenting a device certificate.
    pub fn quic_client(&self) -> quinn::Endpoint {
        let tls =
            Arc::try_unwrap(self.client_tls_config(true)).unwrap_or_else(|arc| (*arc).clone());
        let quic = quinn::crypto::rustls::QuicClientConfig::try_from(tls).unwrap();
        let mut endpoint = quinn::Endpoint::client("127.0.0.1:0".parse().unwrap()).unwrap();
        endpoint.set_default_client_config(quinn::ClientConfig::new(Arc::new(quic)));
        endpoint
    }
}

/// Serial (controller hex format) of a leaf certificate.
pub fn leaf_serial(der: &CertificateDer<'_>) -> String {
    use x509_parser::prelude::FromDer;
    let (_, cert) = x509_parser::certificate::X509Certificate::from_der(der.as_ref()).unwrap();
    serial_hex(cert.raw_serial())
}

/// In-memory TLS handshake against `acceptor`; returns the server leaf serial.
pub async fn tls_handshake_serial(
    acceptor: &tokio_rustls::TlsAcceptor,
    client: Arc<rustls::ClientConfig>,
) -> String {
    let (server_io, client_io) = tokio::io::duplex(64 * 1024);
    let acceptor = acceptor.clone();
    let server = tokio::spawn(async move { acceptor.accept(server_io).await.map(|_| ()) });
    let tls = tokio_rustls::TlsConnector::from(client)
        .connect(
            rustls::pki_types::ServerName::try_from("localhost").unwrap(),
            client_io,
        )
        .await
        .expect("client TLS handshake");
    let serial = leaf_serial(
        &tls.get_ref()
            .1
            .peer_certificates()
            .expect("server certificate")[0],
    );
    server.await.unwrap().expect("server TLS handshake");
    serial
}

/// Server leaf serial seen on a QUIC connection.
pub fn quic_server_serial(conn: &quinn::Connection) -> String {
    let chain = conn
        .peer_identity()
        .unwrap()
        .downcast::<Vec<CertificateDer<'static>>>()
        .unwrap();
    leaf_serial(&chain[0])
}

/// Relay selector config wired to `certs` (other fields: harmless defaults).
pub fn selector_config(
    certs: Arc<CertHolder>,
    state_dir: &std::path::Path,
) -> crate::relay_selector::RelaySelectorConfig {
    use std::time::Duration as StdDuration;
    crate::relay_selector::RelaySelectorConfig {
        state_dir: state_dir.to_path_buf(),
        connector_id: CONNECTOR_ID.to_owned(),
        connector_spiffe_id: certs.spiffe_id().to_owned(),
        certs,
        relay_crl_manager: crate::crl::CrlManager::new(),
        max_incoming_bidi_streams: 16,
        idle_timeout: StdDuration::from_secs(30),
        reprobe_interval: StdDuration::from_secs(300),
        max_concurrent_probes: 2,
        probe_timeout: StdDuration::from_secs(2),
        reconnect_base: StdDuration::from_secs(1),
        reconnect_max: StdDuration::from_secs(10),
        reconnect_backoff_factor: 2.0,
        drain_timeout: StdDuration::from_secs(10),
    }
}
