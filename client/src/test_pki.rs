//! Test-only PKI (rcgen 0.13) shaped like the controller's:
//! Platform Root → Platform Intermediate → Workspace CA → device leaves, and
//! the controller's gRPC server identity issued by the Intermediate. Plus
//! Mallory's unrelated PKI for the bootstrap-hijack tests (certificate audit
//! bug 1, client part).

use rand::RngCore;
use rcgen::{
    BasicConstraints, Certificate, CertificateParams, DnType, ExtendedKeyUsagePurpose, IsCa,
    KeyPair, KeyUsagePurpose, SanType, SerialNumber, PKCS_ECDSA_P384_SHA384,
};

pub const TRUST_DOMAIN: &str = "ws-test.zecurity.in";
pub const DEVICE_ID: &str = "7c9e6679-7425-40de-944b-e07fc1f90ae7";

pub fn device_spiffe_id(device_id: &str) -> String {
    format!("spiffe://{TRUST_DOMAIN}/client/{device_id}")
}

fn new_key() -> KeyPair {
    KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap()
}

fn random_serial() -> SerialNumber {
    let mut bytes = [0u8; 16];
    rand::thread_rng().fill_bytes(&mut bytes);
    bytes[0] &= 0x7f; // positive
    bytes[0] |= 0x01; // no leading zero byte
    SerialNumber::from_slice(&bytes)
}

fn ca_params(cn: &str) -> CertificateParams {
    let mut p = CertificateParams::default();
    p.distinguished_name.push(DnType::CommonName, cn);
    p.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
    p.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
    p.serial_number = Some(random_serial());
    p
}

/// The public key carried in a CSR, for rcgen to sign (P-384, as the client
/// generates software keys).
struct CsrKey(Vec<u8>);

impl rcgen::PublicKeyData for CsrKey {
    fn der_bytes(&self) -> &[u8] {
        &self.0
    }

    fn algorithm(&self) -> &rcgen::SignatureAlgorithm {
        &PKCS_ECDSA_P384_SHA384
    }
}

fn csr_key(csr_pem: &str) -> CsrKey {
    use x509_parser::prelude::FromDer;
    let (_, pem) = x509_parser::pem::parse_x509_pem(csr_pem.as_bytes()).unwrap();
    let (_, csr) =
        x509_parser::certification_request::X509CertificationRequest::from_der(&pem.contents)
            .unwrap();
    CsrKey(
        csr.certification_request_info
            .subject_pki
            .subject_public_key
            .data
            .to_vec(),
    )
}

/// Sign `csr_pem` like the controller's `SignClientCert`: client auth, the
/// device SPIFFE ID, the CSR's own key; `shape` may adjust the parameters.
fn sign_device_csr(
    csr_pem: &str,
    device_id: &str,
    issuer: &Certificate,
    issuer_key: &KeyPair,
    shape: impl FnOnce(&mut CertificateParams),
) -> String {
    let mut p = CertificateParams::default();
    p.distinguished_name
        .push(DnType::CommonName, format!("client-{device_id}"));
    p.subject_alt_names = vec![SanType::URI(
        device_spiffe_id(device_id).try_into().unwrap(),
    )];
    p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
    p.key_usages = vec![KeyUsagePurpose::DigitalSignature];
    p.serial_number = Some(random_serial());
    shape(&mut p);
    p.signed_by(&csr_key(csr_pem), issuer, issuer_key)
        .unwrap()
        .pem()
}

/// A gRPC server identity for `localhost` issued by `issuer`.
/// Returns (cert PEM, key PEM).
fn server_identity(issuer: &Certificate, issuer_key: &KeyPair) -> (String, String) {
    let key = new_key();
    let mut p = CertificateParams::default();
    p.subject_alt_names = vec![SanType::DnsName("localhost".try_into().unwrap())];
    p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
    p.serial_number = Some(random_serial());
    let cert = p.signed_by(&key, issuer, issuer_key).unwrap();
    (cert.pem(), key.serialize_pem())
}

/// A current CRL (fixed calendar window 2000–2999) revoking `revoked`.
fn signed_crl(issuer: &Certificate, issuer_key: &KeyPair, revoked: &[u8]) -> Vec<u8> {
    rcgen::CertificateRevocationListParams {
        this_update: rcgen::date_time_ymd(2000, 1, 1),
        next_update: rcgen::date_time_ymd(2999, 1, 1),
        crl_number: SerialNumber::from(1u64),
        issuing_distribution_point: None,
        revoked_certs: vec![rcgen::RevokedCertParams {
            serial_number: SerialNumber::from_slice(revoked),
            revocation_time: rcgen::date_time_ymd(2000, 1, 1),
            reason_code: None,
            invalidity_date: None,
        }],
        key_identifier_method: rcgen::KeyIdMethod::Sha256,
    }
    .signed_by(issuer, issuer_key)
    .unwrap()
    .der()
    .to_vec()
}

pub struct TestPki {
    root_cert: Certificate,
    root_key: KeyPair,
    int_cert: Certificate,
    int_key: KeyPair,
    ws_cert: Certificate,
    ws_key: KeyPair,
    pub root_pem: String,
    pub intermediate_pem: String,
    pub workspace_ca_pem: String,
}

impl TestPki {
    pub fn new() -> Self {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let root_key = new_key();
        let root_cert = ca_params("Test Platform Root")
            .self_signed(&root_key)
            .unwrap();
        let int_key = new_key();
        let int_cert = ca_params("Test Platform Intermediate")
            .signed_by(&int_key, &root_cert, &root_key)
            .unwrap();
        let ws_key = new_key();
        let mut ws_params = ca_params("workspace-ws-test");
        ws_params.subject_alt_names = vec![SanType::URI("tenant:ws-test".try_into().unwrap())];
        let ws_cert = ws_params.signed_by(&ws_key, &int_cert, &int_key).unwrap();
        Self {
            root_pem: root_cert.pem(),
            intermediate_pem: int_cert.pem(),
            workspace_ca_pem: ws_cert.pem(),
            root_cert,
            root_key,
            int_cert,
            int_key,
            ws_cert,
            ws_key,
        }
    }

    /// The controller's gRPC server identity, issued by the Intermediate.
    pub fn controller_identity(&self) -> (String, String) {
        server_identity(&self.int_cert, &self.int_key)
    }

    /// The device CA chain the client persists: Workspace CA + Intermediate.
    pub fn device_ca_chain(&self) -> String {
        format!("{}\n{}", self.workspace_ca_pem, self.intermediate_pem)
    }

    /// A device leaf for `csr_pem`, as `SignClientCert` issues it.
    pub fn sign_device_csr(&self, csr_pem: &str, device_id: &str) -> String {
        self.sign_device_csr_shaped(csr_pem, device_id, |_| {})
    }

    /// [`Self::sign_device_csr`] with the parameters adjusted by `shape`.
    pub fn sign_device_csr_shaped(
        &self,
        csr_pem: &str,
        device_id: &str,
        shape: impl FnOnce(&mut CertificateParams),
    ) -> String {
        sign_device_csr(csr_pem, device_id, &self.ws_cert, &self.ws_key, shape)
    }

    /// An Intermediate issued by the root but expired, and a Workspace CA
    /// issued by the real Intermediate but expired. Returns (int PEM, ws PEM).
    pub fn expired_cas(&self) -> (String, String) {
        let expire = |p: &mut CertificateParams| {
            p.not_before = rcgen::date_time_ymd(2000, 1, 1);
            p.not_after = rcgen::date_time_ymd(2000, 1, 2);
        };
        let mut int = ca_params("Test Platform Intermediate");
        expire(&mut int);
        let int = int
            .signed_by(&new_key(), &self.root_cert, &self.root_key)
            .unwrap();
        let mut ws = ca_params("workspace-ws-test");
        expire(&mut ws);
        let ws = ws
            .signed_by(&new_key(), &self.int_cert, &self.int_key)
            .unwrap();
        (int.pem(), ws.pem())
    }

    /// `/ca.crl` as the controller signs it: by the Workspace CA, revoking
    /// `revoked` (certificate audit bug 6).
    pub fn workspace_crl(&self, revoked: &[u8]) -> Vec<u8> {
        signed_crl(&self.ws_cert, &self.ws_key, revoked)
    }

    /// `/relay.crl` as the controller signs it: by the Platform Intermediate.
    pub fn relay_crl(&self, revoked: &[u8]) -> Vec<u8> {
        signed_crl(&self.int_cert, &self.int_key, revoked)
    }

    /// A "Workspace CA" that is not a CA, issued by the real Intermediate,
    /// and a device leaf for `csr_pem` signed by it. Returns (ws PEM, leaf).
    pub fn non_ca_workspace(&self, csr_pem: &str, device_id: &str) -> (String, String) {
        let key = new_key();
        let mut p = CertificateParams::default();
        p.distinguished_name
            .push(DnType::CommonName, "workspace-ws-test");
        p.serial_number = Some(random_serial());
        let cert = p.signed_by(&key, &self.int_cert, &self.int_key).unwrap();
        let leaf = sign_device_csr(csr_pem, device_id, &cert, &key, |_| {});
        (cert.pem(), leaf)
    }
}

/// Mallory's PKI: her own root, an "intermediate" carrying the platform
/// Intermediate's exact name (self-issued, so not signed by the platform
/// root), a Workspace CA under it, and server identities.
pub struct AttackerPki {
    root_cert: Certificate,
    root_key: KeyPair,
    fake_int_cert: Certificate,
    fake_int_key: KeyPair,
    ws_cert: Certificate,
    ws_key: KeyPair,
    pub root_pem: String,
    pub fake_intermediate_pem: String,
    pub workspace_ca_pem: String,
}

impl AttackerPki {
    pub fn new() -> Self {
        let _ = rustls::crypto::ring::default_provider().install_default();
        let root_key = new_key();
        let root_cert = ca_params("Mallory Root").self_signed(&root_key).unwrap();
        let fake_int_key = new_key();
        let fake_int_cert = ca_params("Test Platform Intermediate")
            .signed_by(&fake_int_key, &root_cert, &root_key)
            .unwrap();
        let ws_key = new_key();
        let ws_cert = ca_params("workspace-ws-test")
            .signed_by(&ws_key, &fake_int_cert, &fake_int_key)
            .unwrap();
        Self {
            root_pem: root_cert.pem(),
            fake_intermediate_pem: fake_int_cert.pem(),
            workspace_ca_pem: ws_cert.pem(),
            root_cert,
            root_key,
            fake_int_cert,
            fake_int_key,
            ws_cert,
            ws_key,
        }
    }

    /// A `localhost` server identity under Mallory's root.
    pub fn identity(&self) -> (String, String) {
        server_identity(&self.root_cert, &self.root_key)
    }

    /// A `localhost` server identity under the look-alike intermediate.
    pub fn identity_under_fake_intermediate(&self) -> (String, String) {
        server_identity(&self.fake_int_cert, &self.fake_int_key)
    }

    /// A device leaf for `csr_pem` under Mallory's Workspace CA.
    pub fn sign_device_csr(&self, csr_pem: &str, device_id: &str) -> String {
        sign_device_csr(csr_pem, device_id, &self.ws_cert, &self.ws_key, |_| {})
    }
}

/// A fresh software device key and its CSR, as `login::run` builds them.
/// Returns (key PEM, CSR PEM).
pub fn device_key_and_csr() -> (String, String) {
    let key = new_key();
    let mut params = CertificateParams::default();
    params
        .distinguished_name
        .push(DnType::CommonName, "test-host");
    let csr = params.serialize_request(&key).unwrap().pem().unwrap();
    (key.serialize_pem(), csr)
}

/// Another key's leaf (right issuer and identity, wrong key).
pub fn other_key_csr() -> String {
    device_key_and_csr().1
}
