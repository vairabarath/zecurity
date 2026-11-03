//! Trust anchors for enrollment and renewal (certificate audit bug 1).
//!
//! Enrollment fetches the Platform Intermediate from the controller's
//! `/ca.crt` over plain HTTP and pins it against the fingerprint in the
//! enrollment token. Only that exact certificate may be trusted for the
//! enrollment TLS connection, and every certificate the controller (or, on
//! renewal, the connector) returns is validated against the pinned anchors
//! before anything is written. Trust files are written atomically.

use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::Path;

use anyhow::{bail, Context, Result};
use base64::Engine;
use rustls::pki_types::CertificateDer;
use rustls_pemfile::certs;
use sha2::{Digest, Sha256};
use tracing::warn;
use x509_parser::prelude::*;

/// `/ca.crt` must contain exactly ONE certificate and it must match the
/// pinned Platform Intermediate fingerprint. Returns that certificate — the
/// only one trusted for the enrollment TLS connection. (Previously only the
/// first certificate was checked while every certificate in the file was
/// trusted, so an on-path attacker could append their own CA.)
pub fn pinned_bootstrap_ca(
    ca_pem: &str,
    expected_fingerprint: &str,
) -> Result<CertificateDer<'static>> {
    let certificates = certs(&mut ca_pem.as_bytes())
        .collect::<std::result::Result<Vec<_>, _>>()
        .context("failed to parse CA PEM")?;
    let [ca] = certificates.as_slice() else {
        bail!(
            "/ca.crt must contain exactly one certificate (the Platform Intermediate), got {} — possible MITM, aborting",
            certificates.len()
        );
    };
    let fingerprint = hex::encode(Sha256::digest(ca.as_ref()));
    if fingerprint != expected_fingerprint {
        warn!(
            expected = expected_fingerprint,
            actual = %fingerprint,
            "CA FINGERPRINT MISMATCH — possible MITM attack, aborting!"
        );
        bail!(
            "CA fingerprint mismatch: expected {}, got {} — possible MITM attack, aborting",
            expected_fingerprint,
            fingerprint
        );
    }
    Ok(ca.clone().into_owned())
}

/// PEM for a single DER certificate.
pub fn pem_from_der(der: &CertificateDer<'_>) -> String {
    let body = base64::engine::general_purpose::STANDARD.encode(der.as_ref());
    let mut pem = String::from("-----BEGIN CERTIFICATE-----\n");
    for line in body.as_bytes().chunks(64) {
        pem.push_str(std::str::from_utf8(line).expect("base64 is ASCII"));
        pem.push('\n');
    }
    pem.push_str("-----END CERTIFICATE-----\n");
    pem
}

/// Exactly one certificate in `pem`.
pub fn single_certificate(pem: &[u8], label: &str) -> Result<CertificateDer<'static>> {
    let certificates = certs(&mut &pem[..])
        .collect::<std::result::Result<Vec<_>, _>>()
        .with_context(|| format!("parse {label}"))?;
    match <[_; 1]>::try_from(certificates) {
        Ok([certificate]) => Ok(certificate),
        Err(certificates) => bail!(
            "{label} must be exactly one certificate, got {}",
            certificates.len()
        ),
    }
}

/// The anchors a returned chain is checked against.
pub struct Anchors<'a> {
    /// The pinned Platform Intermediate.
    pub intermediate: &'a CertificateDer<'a>,
    /// On renewal, the Workspace CA saved at enrollment; the returned one must
    /// be identical. `None` at enrollment (it is learned then).
    pub workspace_ca: Option<&'a CertificateDer<'a>>,
}

/// Validate a returned (Workspace CA, Intermediate, leaf) before it is
/// persisted: pinned Platform Intermediate → Workspace CA → leaf, the leaf
/// carrying exactly `expected_spiffe_id`, client authentication, and the
/// shield's own key.
pub fn validate_issued_chain(
    anchors: &Anchors<'_>,
    workspace_ca_pem: &[u8],
    intermediate_ca_pem: &[u8],
    leaf_pem: &[u8],
    expected_spiffe_id: &str,
    key_spki_der: &[u8],
) -> Result<()> {
    let returned_intermediate =
        single_certificate(intermediate_ca_pem, "returned Intermediate CA")?;
    if returned_intermediate.as_ref() != anchors.intermediate.as_ref() {
        bail!("returned Intermediate CA is not the pinned Platform Intermediate");
    }
    let (_, intermediate) = X509Certificate::from_der(anchors.intermediate.as_ref())
        .map_err(|e| anyhow::anyhow!("parse pinned Intermediate CA: {e:?}"))?;

    let workspace_der = single_certificate(workspace_ca_pem, "returned Workspace CA")?;
    if let Some(saved) = anchors.workspace_ca {
        if workspace_der.as_ref() != saved.as_ref() {
            bail!("returned Workspace CA is not the pinned Workspace CA");
        }
    }
    let (_, workspace) = X509Certificate::from_der(workspace_der.as_ref())
        .map_err(|e| anyhow::anyhow!("parse returned Workspace CA: {e:?}"))?;
    if !workspace.is_ca() {
        bail!("returned Workspace CA is not a CA certificate");
    }
    if workspace.issuer() != intermediate.subject() {
        bail!("returned Workspace CA was not issued by the pinned Platform Intermediate");
    }
    workspace
        .verify_signature(Some(intermediate.public_key()))
        .map_err(|e| anyhow::anyhow!("returned Workspace CA signature invalid: {e:?}"))?;
    if !workspace.validity().is_valid() {
        bail!("returned Workspace CA is outside its validity period");
    }

    let leaf_der = single_certificate(leaf_pem, "returned certificate")?;
    let (_, leaf) = X509Certificate::from_der(leaf_der.as_ref())
        .map_err(|e| anyhow::anyhow!("parse returned certificate: {e:?}"))?;
    if leaf.is_ca() {
        bail!("returned certificate must not be a CA");
    }
    if leaf.issuer() != workspace.subject() {
        bail!("returned certificate was not issued by the returned Workspace CA");
    }
    leaf.verify_signature(Some(workspace.public_key()))
        .map_err(|e| anyhow::anyhow!("returned certificate signature invalid: {e:?}"))?;
    if !leaf.validity().is_valid() {
        bail!("returned certificate is outside its validity period");
    }
    let uris: Vec<&str> = leaf
        .subject_alternative_name()
        .ok()
        .flatten()
        .map(|san| {
            san.value
                .general_names
                .iter()
                .filter_map(|name| match name {
                    GeneralName::URI(uri) => Some(*uri),
                    _ => None,
                })
                .collect()
        })
        .unwrap_or_default();
    if uris != [expected_spiffe_id] {
        bail!("returned certificate identity {uris:?} is not {expected_spiffe_id}");
    }
    let eku = leaf
        .extended_key_usage()
        .ok()
        .flatten()
        .context("returned certificate has no extended key usage")?;
    if !eku.value.client_auth {
        bail!("returned certificate must allow client authentication");
    }
    if leaf.public_key().raw != key_spki_der {
        bail!("returned certificate is not for this shield's key");
    }
    Ok(())
}

/// Crash-safe replace of `path`: write a temp file in the same directory
/// (created with `mode`), fsync it, rename it over `path`, fsync the
/// directory. A crash leaves the old file or the new one, never a partial one.
pub fn write_atomic(path: &Path, data: &[u8], mode: u32) -> io::Result<()> {
    let dir = path
        .parent()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "path has no parent"))?;
    let name = path
        .file_name()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "path has no file name"))?
        .to_string_lossy();
    let nanos = std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap_or_default()
        .as_nanos();
    let tmp = dir.join(format!(".{name}.tmp.{}.{nanos}", std::process::id()));
    let result = (|| {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(mode)
            .open(&tmp)?;
        file.write_all(data)?;
        file.sync_all()?;
        drop(file);
        fs::rename(&tmp, path)?;
        File::open(dir)?.sync_all()
    })();
    if result.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    result
}

/// Write files atomically, in order (put the "enrolled" marker last). If any
/// write fails, the files already written by this call are removed.
pub fn install_files(dir: &Path, files: &[(&str, Vec<u8>, u32)]) -> Result<()> {
    fs::create_dir_all(dir).with_context(|| format!("failed to create {}", dir.display()))?;
    let mut written = Vec::new();
    for (name, data, mode) in files {
        let path = dir.join(name);
        if let Err(e) = write_atomic(&path, data, *mode) {
            for done in &written {
                let _ = fs::remove_file(done);
            }
            return Err(e).with_context(|| format!("failed to write {}", path.display()));
        }
        written.push(path);
    }
    Ok(())
}
