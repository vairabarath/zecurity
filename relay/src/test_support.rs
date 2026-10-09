//! Test-only PKI: a Platform-Intermediate stand-in that issues relay leaves
//! (same key, fresh serial on every issuance) and connector/client chains.

use std::fs;
use std::path::Path;
use std::sync::Once;

use rcgen::{
    BasicConstraints, Certificate, CertificateParams, DistinguishedName, DnType,
    ExtendedKeyUsagePurpose, IsCa, KeyPair, KeyUsagePurpose, SanType, SerialNumber,
    PKCS_ECDSA_P384_SHA384,
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

    /// Write relay.key / relay.crt / intermediate-ca.crt into `dir`.
    pub fn write_state(&self, dir: &Path, key: &KeyPair, certificate_pem: &str) {
        fs::write(dir.join("relay.key"), key.serialize_pem()).unwrap();
        fs::write(dir.join("relay.crt"), certificate_pem).unwrap();
        fs::write(dir.join("intermediate-ca.crt"), &self.ca_pem).unwrap();
    }
}
