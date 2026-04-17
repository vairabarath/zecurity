// crypto.rs — EC P-384 key generation, CSR building, and PEM/DER helpers
//
// WHY EC P-384?
//   P-384 (also called secp384r1) is a NIST elliptic curve that provides
//   ~192-bit security. It's the same curve used by the connector and is
//   required by the controller's PKI for all SPIFFE certificates.
//   It's stronger than P-256 and widely supported by TLS stacks.
//
// WHAT IS A CSR?
//   A Certificate Signing Request (PKCS#10) is how the shield asks the
//   controller to issue it a certificate. It contains:
//     - The shield's public key
//     - A CN (Common Name): "shield-<shield_id>"
//     - A SAN URI: "spiffe://ws-<slug>.zecurity.in/shield/<shield_id>"
//     - A self-signature proving the shield holds the private key
//   The controller verifies the self-signature, then issues a signed cert.
//
// USED BY:
//   enrollment.rs — generate_keypair(), save_private_key(), build_csr()
//   renewal.rs    — load_private_key(), build_csr() (renewal uses existing key)
//   main.rs       — parse_cert_not_after() to check cert expiry on startup

use std::fs::OpenOptions;
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

use anyhow::{Context, Result};
use base64::Engine;
use rcgen::{
    CertificateParams, DistinguishedName, DnType, IsCa, KeyPair, SanType, PKCS_ECDSA_P384_SHA384,
};
use time::OffsetDateTime;
use x509_parser::certificate::X509Certificate;
use x509_parser::prelude::FromDer;

/// Generate a new EC P-384 keypair.
///
/// The keypair is ephemeral until saved with save_private_key().
/// On first enrollment, a new keypair is generated and saved.
/// On renewal, the SAME keypair is reused (load_private_key + build_csr).
pub fn generate_keypair() -> Result<KeyPair> {
    KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).context("failed to generate EC P-384 keypair")
}

/// Save a private key to disk as PEM with strict permissions (mode 0600).
///
/// WHY 0600?
///   The private key must never be readable by other users on the system.
///   0600 = owner read+write only. We set this atomically via OpenOptionsExt
///   rather than chmod-after-write to avoid a race window.
///
/// The file is written to `<state_dir>/shield.key`.
pub fn save_private_key(key_pair: &KeyPair, path: &Path) -> Result<()> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)
            .with_context(|| format!("failed to create directory {}", parent.display()))?;
    }

    OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600) // owner read+write only — no chmod race
        .open(path)
        .with_context(|| format!("failed to open {}", path.display()))?
        .write_all(key_pair.serialize_pem().as_bytes())
        .with_context(|| format!("failed to write private key to {}", path.display()))
}

/// Load a private key from a PEM file on disk.
///
/// Used on restart and during cert renewal to reload the previously
/// generated keypair without generating a new one.
pub fn load_private_key(path: &Path) -> Result<KeyPair> {
    let pem = std::fs::read_to_string(path)
        .with_context(|| format!("failed to read private key from {}", path.display()))?;
    KeyPair::from_pem(&pem)
        .with_context(|| format!("failed to parse PEM key from {}", path.display()))
}

/// Build a DER-encoded PKCS#10 CSR for enrollment or renewal.
///
/// Parameters:
///   - `key_pair`   — the EC P-384 keypair to sign the CSR with
///   - `cn`         — Common Name, e.g. `"shield-<shield_id>"`
///   - `spiffe_uri` — SAN URI, e.g. `"spiffe://ws-acme.zecurity.in/shield/<id>"`
///
/// Returns DER bytes sent as `EnrollRequest.csr_der` to the controller.
///
/// The controller:
///   1. Verifies the self-signature (proves the shield holds the private key)
///   2. Extracts the public key to sign a fresh SPIFFE certificate
///   3. Ignores the CN/SAN in the CSR — uses the DB-registered SPIFFE ID instead
pub fn build_csr(key_pair: &KeyPair, cn: &str, spiffe_uri: &str) -> Result<Vec<u8>> {
    let mut dn = DistinguishedName::new();
    dn.push(DnType::CommonName, cn);

    let mut params = CertificateParams::new(Vec::<String>::new())
        .context("failed to create certificate params")?;
    params.distinguished_name = dn;
    params.is_ca = IsCa::NoCa;
    params.subject_alt_names = vec![SanType::URI(
        spiffe_uri
            .try_into()
            .context("invalid SPIFFE URI for SAN")?,
    )];

    let csr = params
        .serialize_request(key_pair)
        .context("failed to serialize CSR")?;

    Ok(csr.der().to_vec())
}

/// Parse the NotAfter (expiry) timestamp from a PEM certificate.
///
/// Used in two places:
///   1. enrollment.rs — saves cert_not_after to state.json after enrollment
///   2. main.rs       — checks on startup whether the cert is still valid
///
/// The PEM is base64-decoded to DER, then parsed with x509-parser.
pub fn parse_cert_not_after(cert_pem: &[u8]) -> Result<OffsetDateTime> {
    let pem_str = std::str::from_utf8(cert_pem).context("cert PEM is not valid UTF-8")?;

    // Strip PEM header/footer lines and decode the base64 body to DER
    let b64: String = pem_str
        .lines()
        .filter(|l| !l.starts_with("-----") && !l.is_empty())
        .collect();

    let der = base64::engine::general_purpose::STANDARD
        .decode(b64.as_bytes())
        .context("failed to base64-decode PEM certificate body")?;

    let (_, cert) = X509Certificate::from_der(&der).context("failed to parse certificate DER")?;

    let ts = cert.validity().not_after.timestamp();
    OffsetDateTime::from_unix_timestamp(ts).context("invalid NotAfter timestamp in certificate")
}

/// Compute SHA-256 hex digest of raw bytes.
///
/// Used by enrollment.rs to verify the CA certificate fingerprint
/// against the hash embedded in the enrollment JWT claims.
/// This prevents a MITM from substituting a rogue CA cert.
pub fn sha256_hex(data: &[u8]) -> String {
    use sha2::{Digest, Sha256};
    let mut hasher = Sha256::new();
    hasher.update(data);
    hex::encode(hasher.finalize())
}
