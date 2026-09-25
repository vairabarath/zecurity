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
