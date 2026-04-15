// crypto.rs — EC P-384 key generation, PEM I/O, and CSR building for the ZECURITY connector

use base64::Engine;
//
// Provides utilities used by enrollment.rs (Phase 5):
//   - generate_keypair()   — creates an EC P-384 keypair via rcgen
//   - save_private_key()   — writes PEM to disk with mode 0600 (atomic via OpenOptionsExt)
//   - load_private_key()   — reads a PEM key back from disk (Phase 6 restart support)
//   - build_csr()          — builds a DER-encoded PKCS#10 CSR with CN and SAN URI

use std::fs::OpenOptions;
use std::io::Write;
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

use anyhow::{Context, Result};
use rcgen::{
    CertificateParams, DistinguishedName, DnType, IsCa, KeyPair, SanType, PKCS_ECDSA_P384_SHA384,
};
use time::OffsetDateTime;
use x509_parser::certificate::X509Certificate;
use x509_parser::prelude::FromDer;

/// Generate an EC P-384 keypair.
///
/// Uses `rcgen` with the `rcgen::PKCS_ECDSA_P384_SHA384` curve.
/// Returns a `KeyPair` that can be used to build a CSR or save to disk.
pub fn generate_keypair() -> Result<KeyPair> {
    let key_pair = KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384)
        .context("failed to generate EC P-384 keypair")?;
    Ok(key_pair)
}

/// Save a private key to disk as PEM with mode 0600.
///
/// Uses `OpenOptions::new().mode(0o600)` for atomic permissions —
/// no chmod race condition.
pub fn save_private_key(key_pair: &KeyPair, path: &Path) -> Result<()> {
    if let Some(parent) = path.parent() {
        std::fs::create_dir_all(parent)
            .with_context(|| format!("failed to create directory {}", parent.display()))?;
    }

    let pem = key_pair.serialize_pem();

    OpenOptions::new()
        .write(true)
        .create(true)
        .truncate(true)
        .mode(0o600)
        .open(path)
        .with_context(|| format!("failed to open {}", path.display()))?
        .write_all(pem.as_bytes())
        .with_context(|| format!("failed to write private key to {}", path.display()))?;

    Ok(())
}

/// Load a private key from a PEM file on disk.
///
/// Used on restart (Phase 6) to reload the previously generated keypair.
pub fn load_private_key(path: &Path) -> Result<KeyPair> {
    let pem = std::fs::read_to_string(path)
        .with_context(|| format!("failed to read private key from {}", path.display()))?;

    let key_pair = KeyPair::from_pem(&pem)
        .with_context(|| format!("failed to parse PEM key from {}", path.display()))?;

    Ok(key_pair)
}

/// Build a DER-encoded PKCS#10 CSR.
///
/// Parameters:
///   - `key_pair`: the EC P-384 keypair to sign with
///   - `cn`: Common Name, e.g. `"connector-<id>"`
///   - `spiffe_uri`: SAN URI, e.g. `"spiffe://ws-acme.zecurity.in/connector/<id>"`
///
/// Returns DER bytes suitable for `EnrollRequest.csr_der`.
pub fn build_csr(key_pair: &KeyPair, cn: &str, spiffe_uri: &str) -> Result<Vec<u8>> {
    let mut distinguished_name = DistinguishedName::new();
    distinguished_name.push(DnType::CommonName, cn);

    let mut params = CertificateParams::new(Vec::<String>::new())
        .context("failed to create certificate params")?;
    params.distinguished_name = distinguished_name;
    params.is_ca = IsCa::NoCa;
    params.subject_alt_names = vec![SanType::URI(
        spiffe_uri
            .try_into()
            .context("invalid SPIFFE URI for SAN")?,
    )];

    let csr = params
        .serialize_request(key_pair)
        .context("failed to serialize CSR")?;

    let der = csr.der().to_vec();

    Ok(der)
}

/// Build a PKCS#10 CSR from the connector's existing private key.
///
/// Used during cert renewal. The controller parses this CSR to:
///   1. Verify the self-signature (proof that the connector holds the private key)
///   2. Extract the public key to sign a fresh certificate for the same keypair
///
/// The CSR content (CN, SAN) is a placeholder — the controller ignores it
/// and uses the connector's registered SPIFFE ID from the database instead.
pub fn extract_public_key_der(private_key_pem: &str) -> Result<Vec<u8>> {
    let key_pair = KeyPair::from_pem(private_key_pem).context("failed to parse PEM key")?;
    build_csr(&key_pair, "renewal", "spiffe://renewal/renewal")
}

/// Parse the NotAfter timestamp from a PEM certificate.
///
/// Used to update state.json after cert renewal.
pub fn parse_cert_not_after(cert_pem: &[u8]) -> Result<OffsetDateTime> {
    // Get the raw PEM bytes
    let pem_bytes = cert_pem.to_vec();

    // Find the certificate block in PEM
    let pem_str = String::from_utf8(pem_bytes).context("cert PEM is not valid UTF-8")?;
    let start = pem_str
        .find("-----BEGIN")
        .context("no certificate found in PEM")?;
    let cert_section = &pem_str[start..];

    // Extract base64 content between the header and footer lines
    let content: String = cert_section
        .lines()
        .filter(|l| !l.starts_with("-----") && !l.is_empty())
        .collect();

    // Decode base64 to get DER
    let der = base64::engine::general_purpose::STANDARD
        .decode(content.as_bytes())
        .context("failed to decode base64")?;

    // Parse the certificate
    let (_, cert) = X509Certificate::from_der(&der).context("failed to parse certificate DER")?;

    let not_after = cert.validity().not_after;
    let ts = not_after.timestamp();
    OffsetDateTime::from_unix_timestamp(ts).context("invalid timestamp")
}
