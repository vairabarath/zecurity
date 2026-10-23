//! Test-only PKI: a Platform-Intermediate stand-in that issues relay leaves
//! (same key, fresh serial on every issuance) and connector/client chains.

use std::fs;
use std::path::Path;
use std::sync::Once;

use rcgen::{
    BasicConstraints, Certificate, CertificateParams, CertificateRevocationListParams,
    DistinguishedName, DnType, ExtendedKeyUsagePurpose, IsCa, KeyIdMethod, KeyPair,
    KeyUsagePurpose, RevokedCertParams, SanType, SerialNumber, PKCS_ECDSA_P384_SHA384,
};
use time::{Duration, OffsetDateTime};

pub const RELAY_ID: &str = "550e8400-e29b-41d4-a716-446655440000";

/// Set in a child process started by [`run_in_subprocess`]; holds the test
/// that child must run in-process.
const SUBPROCESS_TEST_ENV: &str = "ZECURITY_RELAY_SUBPROCESS_TEST";

/// Run `body` for test `test_path` (e.g. `"renewal::tests::foo"`) in a child
/// process of this test binary, under a hard wall-clock `limit`.
///
/// For tests whose code under test could spin without ever yielding (the
/// relay renewal scheduler): `tokio::time::timeout` only fires at an
/// `.await`, and a Rust thread cannot be killed, so neither can stop a
/// runaway loop in-process. A child process can be: on expiry it is killed
/// (SIGKILL) and reaped, and the test fails with a diagnostic.
///
/// The parent re-runs this binary filtered to exactly `test_path`, with
/// `SUBPROCESS_TEST_ENV` set; in that child the same test calls `body`
/// directly. The child must report exactly one passed test.
pub fn run_in_subprocess(test_path: &str, limit: std::time::Duration, body: impl FnOnce()) {
    use std::process::{Command, Stdio};
    use std::time::Instant;

    if std::env::var(SUBPROCESS_TEST_ENV).as_deref() == Ok(test_path) {
        body();
        return;
    }

    // Output goes to a file, not a pipe: a chatty runaway child must not
    // block on a full pipe buffer (and a file needs no reader thread).
    let log_path = std::env::temp_dir().join(format!(
        "relay-subprocess-{}.log",
        uuid::Uuid::new_v4().simple()
    ));
    let log = fs::File::create(&log_path).expect("create subprocess log");
    let mut child = Command::new(std::env::current_exe().expect("current test binary"))
        .args([test_path, "--exact", "--nocapture", "--test-threads=1"])
        .env(SUBPROCESS_TEST_ENV, test_path)
        .stdin(Stdio::null())
        .stdout(log.try_clone().expect("clone subprocess log"))
        .stderr(log)
        .spawn()
        .expect("spawn test subprocess");

    let started = Instant::now();
    let status = loop {
        if let Some(status) = child.try_wait().expect("poll test subprocess") {
            break Some(status);
        }
        if started.elapsed() >= limit {
            break None;
        }
        std::thread::sleep(std::time::Duration::from_millis(20));
    };
    let Some(status) = status else {
        let pid = child.id();
        let _ = child.kill();
        let _ = child.wait(); // reap: no zombie or orphan is left behind
        let output = fs::read_to_string(&log_path).unwrap_or_default();
        let _ = fs::remove_file(&log_path);
        panic!(
            "{test_path} did not finish within the hard limit of {limit:?}; child pid {pid} \
             was killed. The code under test is likely stuck in a loop that never yields \
             (tokio::time::timeout cannot interrupt that). Child output:\n{output}"
        );
    };
    let output = fs::read_to_string(&log_path).unwrap_or_default();
    let _ = fs::remove_file(&log_path);
    assert!(
        status.success() && output.contains("test result: ok. 1 passed"),
        "{test_path} failed in its subprocess ({status}). Child output:\n{output}"
    );
}

pub fn install_crypto_provider() {
    static INSTALL: Once = Once::new();
    INSTALL.call_once(|| {
        let _ = rustls::crypto::ring::default_provider().install_default();
    });
}

pub struct TestPki {
    ca_cert: Certificate,
    ca_key: KeyPair,
    pub ca_pem: String,
}

fn random_serial() -> SerialNumber {
    let mut bytes = uuid::Uuid::new_v4().as_bytes().to_vec();
    bytes[0] &= 0x7f; // positive
    bytes[0] |= 0x01; // no leading zero byte
    SerialNumber::from_slice(&bytes)
}

impl TestPki {
    pub fn new() -> Self {
        install_crypto_provider();
        let ca_key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
        let mut params = CertificateParams::default();
        let mut dn = DistinguishedName::new();
        dn.push(DnType::CommonName, "Test Platform Intermediate");
        params.distinguished_name = dn;
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        params.key_usages = vec![
            KeyUsagePurpose::KeyCertSign,
            KeyUsagePurpose::CrlSign,
            KeyUsagePurpose::DigitalSignature,
        ];
        params.serial_number = Some(random_serial());
        let ca_cert = params.self_signed(&ca_key).unwrap();
        let ca_pem = ca_cert.pem();
        Self {
            ca_cert,
            ca_key,
            ca_pem,
        }
    }

    pub fn relay_key(&self) -> KeyPair {
        KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap()
    }

    /// Relay leaf for `key`, valid from now+`not_before_offset_secs` for
    /// `lifetime_secs`. Every call yields a new serial.
    pub fn relay_cert(
        &self,
        relay_id: &str,
        key: &KeyPair,
        not_before_offset_secs: i64,
        lifetime_secs: i64,
    ) -> String {
        let mut params = CertificateParams::default();
        let mut dn = DistinguishedName::new();
        dn.push(DnType::CommonName, format!("relay-{relay_id}"));
        params.distinguished_name = dn;
        params.subject_alt_names = vec![
            SanType::URI(
                crate::appmeta::relay_spiffe_id(relay_id)
                    .try_into()
                    .unwrap(),
            ),
            SanType::DnsName("localhost".try_into().unwrap()),
        ];
        params.extended_key_usages = vec![
            ExtendedKeyUsagePurpose::ServerAuth,
            ExtendedKeyUsagePurpose::ClientAuth,
        ];
        params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
        let not_before = OffsetDateTime::now_utc() + Duration::seconds(not_before_offset_secs);
        params.not_before = not_before;
        params.not_after = not_before + Duration::seconds(lifetime_secs);
        params.serial_number = Some(random_serial());
        params
            .signed_by(key, &self.ca_cert, &self.ca_key)
            .unwrap()
            .pem()
    }

    /// Connector/client chain accepted by the Relay listener:
    /// leaf -> Workspace CA -> this Intermediate. Returns (chain DER, key DER).
    pub fn client_chain(&self) -> (Vec<rustls::pki_types::CertificateDer<'static>>, KeyPair) {
        let ws_key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
        let mut ws_params = CertificateParams::default();
        let mut dn = DistinguishedName::new();
        dn.push(DnType::CommonName, "Test Workspace CA");
        ws_params.distinguished_name = dn;
        ws_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        ws_params.key_usages = vec![
            KeyUsagePurpose::KeyCertSign,
            KeyUsagePurpose::DigitalSignature,
        ];
        ws_params.subject_alt_names = vec![SanType::URI("tenant:ws-test".try_into().unwrap())];
        ws_params.serial_number = Some(random_serial());
        let ws_cert = ws_params
            .signed_by(&ws_key, &self.ca_cert, &self.ca_key)
            .unwrap();

        let leaf_key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
        let mut leaf_params = CertificateParams::default();
        leaf_params.subject_alt_names = vec![SanType::URI(
            "spiffe://ws-test.zecurity.in/client/9b2d5cae-5820-4702-adf4-231680852b11"
                .try_into()
                .unwrap(),
        )];
        leaf_params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        leaf_params.serial_number = Some(random_serial());
        let leaf = leaf_params.signed_by(&leaf_key, &ws_cert, &ws_key).unwrap();
        (vec![leaf.der().clone(), ws_cert.der().clone()], leaf_key)
    }

    /// The controller's gRPC server identity as production issues it: signed
    /// by the Platform Intermediate, serverAuth, valid for `localhost`.
    /// Returns (cert PEM, key PEM).
    pub fn controller_server_identity(&self) -> (String, String) {
        server_identity(&self.ca_cert, &self.ca_key)
    }

    /// Sign a relay provisioning CSR the way the controller does: issued by
    /// the Platform Intermediate for the CSR's own key, with the relay's
    /// SPIFFE ID.
    pub fn sign_relay_csr(&self, csr_der: &[u8], relay_id: &str) -> String {
        sign_csr(csr_der, relay_id, &self.ca_cert, &self.ca_key)
    }

    /// [`Self::sign_relay_csr`] with the leaf parameters (usages, validity,
    /// CA flag …) adjusted by `shape` — for the wrong-shape rejection tests.
    pub fn sign_relay_csr_shaped(
        &self,
        csr_der: &[u8],
        relay_id: &str,
        shape: impl FnOnce(&mut CertificateParams),
    ) -> String {
        sign_csr_shaped(csr_der, relay_id, &self.ca_cert, &self.ca_key, shape)
    }

    /// A Workspace CA shaped like the controller's `GenerateWorkspaceCA`
    /// (CN `workspace-<tenant>`, CA with path length 0, CertSign + CRLSign,
    /// `tenant:<id>` URI SAN), issued by this Intermediate. Its key id is
    /// pinned so CRLs it signs carry a matching Authority Key Identifier.
    pub fn workspace_ca(&self, tenant_id: &str) -> TestWorkspaceCa {
        let key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
        let key_id = format!("kid-{tenant_id}").into_bytes();
        let mut params = CertificateParams::default();
        let mut dn = DistinguishedName::new();
        dn.push(DnType::CommonName, format!("workspace-{tenant_id}"));
        params.distinguished_name = dn;
        params.is_ca = IsCa::Ca(BasicConstraints::Constrained(0));
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        params.subject_alt_names = vec![SanType::URI(
            format!("tenant:{tenant_id}").try_into().unwrap(),
        )];
        params.key_identifier_method = KeyIdMethod::PreSpecified(key_id.clone());
        params.serial_number = Some(random_serial());
        let cert = params.signed_by(&key, &self.ca_cert, &self.ca_key).unwrap();
        TestWorkspaceCa {
            tenant_id: tenant_id.to_owned(),
            cert,
            key,
            key_id,
        }
    }

    /// Write relay.key / relay.crt / intermediate-ca.crt into `dir`.
    pub fn write_state(&self, dir: &Path, key: &KeyPair, certificate_pem: &str) {
        fs::write(dir.join("relay.key"), key.serialize_pem()).unwrap();
        fs::write(dir.join("relay.crt"), certificate_pem).unwrap();
        fs::write(dir.join("intermediate-ca.crt"), &self.ca_pem).unwrap();
    }
}

/// A Workspace CA under a [`TestPki`] Intermediate (see `TestPki::workspace_ca`).
pub struct TestWorkspaceCa {
    pub tenant_id: String,
    cert: Certificate,
    key: KeyPair,
    key_id: Vec<u8>,
}

impl TestWorkspaceCa {
    pub fn der(&self) -> rustls::pki_types::CertificateDer<'static> {
        self.cert.der().clone()
    }

    /// An attacker's forgery of this CA (audit finding S1b): same subject
    /// name and same PUBLIC key (both public), but claiming `tenant:<claimed>`
    /// and signed by an unrelated attacker key, so it chains to nothing. Only
    /// this CA's public key is used — the attacker needs no private key.
    pub fn forged_copy_claiming(
        &self,
        claimed_tenant: &str,
    ) -> rustls::pki_types::CertificateDer<'static> {
        self.forged_copy_claiming_signed_as(claimed_tenant, "attacker")
    }

    /// Like [`Self::forged_copy_claiming`], with the attacker's signing CA
    /// named `attacker_cn` (e.g. the Platform Intermediate's own name).
    pub fn forged_copy_claiming_signed_as(
        &self,
        claimed_tenant: &str,
        attacker_cn: &str,
    ) -> rustls::pki_types::CertificateDer<'static> {
        let attacker_key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
        let mut attacker_params = CertificateParams::default();
        attacker_params
            .distinguished_name
            .push(DnType::CommonName, attacker_cn);
        attacker_params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        let attacker = attacker_params.self_signed(&attacker_key).unwrap();

        let mut params = CertificateParams::default();
        let mut dn = DistinguishedName::new();
        dn.push(DnType::CommonName, format!("workspace-{}", self.tenant_id));
        params.distinguished_name = dn;
        params.is_ca = IsCa::Ca(BasicConstraints::Constrained(0));
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        params.subject_alt_names = vec![SanType::URI(
            format!("tenant:{claimed_tenant}").try_into().unwrap(),
        )];
        params.serial_number = Some(random_serial());
        // Subject public key = this CA's public key; signature = attacker's.
        params
            .signed_by(&self.key, &attacker, &attacker_key)
            .unwrap()
            .der()
            .clone()
    }

    /// A client leaf (`spiffe://<tenant>.zecurity.in/client/<id>`) issued by
    /// this CA with the given serial (high bit clear, so the DER INTEGER
    /// content octets equal `serial` exactly, as the relay reads them).
    pub fn client_leaf(
        &self,
        serial: &[u8],
    ) -> (rustls::pki_types::CertificateDer<'static>, KeyPair) {
        let key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
        let mut params = CertificateParams::default();
        params.subject_alt_names = vec![SanType::URI(
            format!(
                "spiffe://{}.zecurity.in/client/9b2d5cae-5820-4702-adf4-231680852b11",
                self.tenant_id
            )
            .try_into()
            .unwrap(),
        )];
        params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        params.serial_number = Some(SerialNumber::from_slice(serial));
        let leaf = params.signed_by(&key, &self.cert, &self.key).unwrap();
        (leaf.der().clone(), key)
    }

    /// A DER CRL signed by this CA, valid from 1 min ago for 10 min, listing
    /// `revoked` serials.
    pub fn crl(&self, revoked: &[&[u8]]) -> Vec<u8> {
        let now = OffsetDateTime::now_utc();
        let params = CertificateRevocationListParams {
            this_update: now - Duration::minutes(1),
            next_update: now + Duration::minutes(10),
            crl_number: SerialNumber::from(1u64),
            issuing_distribution_point: None,
            revoked_certs: revoked
                .iter()
                .map(|serial| RevokedCertParams {
                    serial_number: SerialNumber::from_slice(serial),
                    revocation_time: now - Duration::minutes(1),
                    reason_code: None,
                    invalidity_date: None,
                })
                .collect(),
            key_identifier_method: KeyIdMethod::PreSpecified(self.key_id.clone()),
        };
        params
            .signed_by(&self.cert, &self.key)
            .unwrap()
            .der()
            .as_ref()
            .to_vec()
    }
}

fn server_identity(issuer: &Certificate, issuer_key: &KeyPair) -> (String, String) {
    let key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
    let mut params = CertificateParams::default();
    params.subject_alt_names = vec![SanType::DnsName("localhost".try_into().unwrap())];
    params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
    params.serial_number = Some(random_serial());
    let cert = params.signed_by(&key, issuer, issuer_key).unwrap();
    (cert.pem(), key.serialize_pem())
}

/// The public key carried in a CSR, for rcgen to sign (P-384, as the relay
/// generates).
struct CsrKey(Vec<u8>);

impl rcgen::PublicKeyData for CsrKey {
    fn der_bytes(&self) -> &[u8] {
        &self.0
    }

    fn algorithm(&self) -> &rcgen::SignatureAlgorithm {
        &PKCS_ECDSA_P384_SHA384
    }
}

fn sign_csr(csr_der: &[u8], relay_id: &str, issuer: &Certificate, issuer_key: &KeyPair) -> String {
    sign_csr_shaped(csr_der, relay_id, issuer, issuer_key, |_| {})
}

/// [`sign_csr`] with the leaf parameters adjusted by `shape` before signing.
fn sign_csr_shaped(
    csr_der: &[u8],
    relay_id: &str,
    issuer: &Certificate,
    issuer_key: &KeyPair,
    shape: impl FnOnce(&mut CertificateParams),
) -> String {
    use x509_parser::prelude::FromDer;
    let (_, csr) =
        x509_parser::certification_request::X509CertificationRequest::from_der(csr_der).unwrap();
    let key = CsrKey(
        csr.certification_request_info
            .subject_pki
            .subject_public_key
            .data
            .to_vec(),
    );
    let mut params = CertificateParams::default();
    params.subject_alt_names = vec![SanType::URI(
        crate::appmeta::relay_spiffe_id(relay_id)
            .try_into()
            .unwrap(),
    )];
    params.extended_key_usages = vec![
        ExtendedKeyUsagePurpose::ServerAuth,
        ExtendedKeyUsagePurpose::ClientAuth,
    ];
    params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
    params.serial_number = Some(random_serial());
    shape(&mut params);
    params.signed_by(&key, issuer, issuer_key).unwrap().pem()
}

/// Mallory's PKI for bootstrap-hijack tests (bug 1).
pub struct AttackerPki {
    root: Certificate,
    root_key: KeyPair,
    pub root_pem: String,
}

impl Default for AttackerPki {
    fn default() -> Self {
        Self::new()
    }
}

impl AttackerPki {
    pub fn new() -> Self {
        install_crypto_provider();
        let root_key = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap();
        let mut params = CertificateParams::default();
        params
            .distinguished_name
            .push(DnType::CommonName, "Mallory Root");
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        let root = params.self_signed(&root_key).unwrap();
        let root_pem = root.pem();
        Self {
            root,
            root_key,
            root_pem,
        }
    }

    pub fn server_identity(&self) -> (String, String) {
        server_identity(&self.root, &self.root_key)
    }

    pub fn sign_csr(&self, csr_der: &[u8], relay_id: &str) -> String {
        sign_csr(csr_der, relay_id, &self.root, &self.root_key)
    }
}

/// Connector-revocation Phase D tests: a plain loopback QUIC connection pair
/// (self-signed server certificate trusted by the client). Returns
/// (client side, server side). The endpoints are leaked so the connections
/// outlive the call.
pub async fn quic_pair() -> (quinn::Connection, quinn::Connection) {
    install_crypto_provider();
    let cert = rcgen::generate_simple_self_signed(vec!["localhost".to_owned()]).unwrap();
    let cert_der = rustls::pki_types::CertificateDer::from(cert.cert.der().to_vec());
    let key_der = rustls::pki_types::PrivateKeyDer::Pkcs8(
        rustls::pki_types::PrivatePkcs8KeyDer::from(cert.key_pair.serialize_der()),
    );
    let server_cfg =
        quinn::ServerConfig::with_single_cert(vec![cert_der.clone()], key_der).unwrap();
    let server = quinn::Endpoint::server(server_cfg, "127.0.0.1:0".parse().unwrap()).unwrap();
    let mut roots = rustls::RootCertStore::empty();
    roots.add(cert_der).unwrap();
    let client_cfg =
        quinn::ClientConfig::with_root_certificates(std::sync::Arc::new(roots)).unwrap();
    let mut client = quinn::Endpoint::client("127.0.0.1:0".parse().unwrap()).unwrap();
    client.set_default_client_config(client_cfg);
    let addr = server.local_addr().unwrap();
    let connecting = client.connect(addr, "localhost").unwrap();
    let server_side = server.accept().await.unwrap().await.unwrap();
    let client_side = connecting.await.unwrap();
    std::mem::forget(server);
    std::mem::forget(client);
    (client_side, server_side)
}
