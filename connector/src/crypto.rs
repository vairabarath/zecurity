// crypto.rs — EC P-384 key generation, PEM I/O, and CSR building for the ZECURITY connector
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
    CertificateParams, DistinguishedName, DnType, IsCa, KeyPair, PKCS_ECDSA_P384_SHA384,
    SanType,
};

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

    let key_pair =
        KeyPair::from_pem(&pem).with_context(|| format!("failed to parse PEM key from {}", path.display()))?;

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
    params.subject_alt_names = vec![SanType::URI(spiffe_uri.try_into().context("invalid SPIFFE URI for SAN")?)];

    let csr = params
        .serialize_request(key_pair)
        .context("failed to serialize CSR")?;

    let der = csr
        .der()
        .to_vec();

    Ok(der)
}
