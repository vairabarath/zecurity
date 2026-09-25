//! Single owner of the Relay's current certificate (Sprint 20 Phase F-2).
//!
//! The QUIC listener, the heartbeat channel and the renewal scheduler all read
//! the current certificate from here and subscribe to changes; nothing else
//! keeps its own copy. A renewed certificate is **persisted atomically before**
//! it is published, so a consumer never presents a certificate that is not on
//! disk, and after a crash the relay restarts with whatever was persisted.
//!
//! The private key is loaded once and never rewritten (D-19: renew with the
//! existing key only).

use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use anyhow::{bail, Context, Result};
use rcgen::KeyPair;
use rustls_pemfile::certs;
use tokio::sync::watch;
use x509_parser::certificate::X509Certificate;
use x509_parser::prelude::FromDer;

use crate::tls::validate_relay_certificate;

/// One immutable snapshot of the Relay's certificate material.
#[derive(Debug)]
pub struct CertMaterial {
    pub certificate_pem: Vec<u8>,
    pub key_pem: Vec<u8>,
    pub intermediate_ca_pem: Vec<u8>,
    /// Certificate serial, lowercase hex without leading zeros (matches the
    /// controller's `SerialNumber.Text(16)`).
    pub serial_hex: String,
    pub not_before_unix: i64,
    pub not_after_unix: i64,
}

impl CertMaterial {
    pub fn lifetime_secs(&self) -> i64 {
        self.not_after_unix - self.not_before_unix
    }
}

/// Paths of the persisted material inside `RELAY_STATE_DIR`.
#[derive(Debug, Clone)]
pub struct CertPaths {
    pub key_path: PathBuf,
    pub certificate_path: PathBuf,
    pub intermediate_ca_path: PathBuf,
}

pub struct CertManager {
    relay_id: String,
    paths: CertPaths,
    /// SubjectPublicKeyInfo DER of the Relay's (only) key pair.
    key_spki_der: Vec<u8>,
    current: watch::Sender<Arc<CertMaterial>>,
    /// Serializes installs so persist + publish is one step per renewal.
    install_lock: Mutex<()>,
    scheduler_claimed: AtomicBool,
}

impl CertManager {
    /// Load the persisted material and validate that the certificate is this
    /// relay's and belongs to the persisted key.
    pub fn load(relay_id: &str, paths: CertPaths) -> Result<Arc<Self>> {
        let key_pem = fs::read(&paths.key_path)
            .with_context(|| format!("read {}", paths.key_path.display()))?;
        let certificate_pem = fs::read(&paths.certificate_path)
            .with_context(|| format!("read {}", paths.certificate_path.display()))?;
        let intermediate_ca_pem = fs::read(&paths.intermediate_ca_path)
            .with_context(|| format!("read {}", paths.intermediate_ca_path.display()))?;

        let key_spki_der = key_spki_der(&key_pem)?;
        let material = parse_material(
            relay_id,
            &key_spki_der,
            certificate_pem,
            key_pem,
            intermediate_ca_pem,
        )?;
        let (current, _) = watch::channel(Arc::new(material));
        Ok(Arc::new(Self {
            relay_id: relay_id.to_owned(),
            paths,
            key_spki_der,
            current,
            install_lock: Mutex::new(()),
            scheduler_claimed: AtomicBool::new(false),
        }))
    }

    pub fn relay_id(&self) -> &str {
        &self.relay_id
    }

    /// The certificate every consumer must present right now.
    pub fn current(&self) -> Arc<CertMaterial> {
        self.current.borrow().clone()
    }

    /// Subscribe to certificate changes (listener, heartbeat).
    pub fn subscribe(&self) -> watch::Receiver<Arc<CertMaterial>> {
        self.current.subscribe()
    }

    /// Claim the single renewal-scheduler slot. Returns false if a scheduler
    /// already runs for this manager.
    pub fn claim_scheduler(&self) -> bool {
        !self.scheduler_claimed.swap(true, Ordering::SeqCst)
    }

    /// Validate, persist, then publish a renewed certificate.
    ///
    /// Order is the F-1 → F-2 contract: nothing is published unless the
    /// certificate is durably on disk, and once published every consumer
    /// switches to it; the old certificate is never presented again.
    pub fn install_renewed(&self, certificate_pem: &[u8]) -> Result<Arc<CertMaterial>> {
        let _guard = self
            .install_lock
            .lock()
            .map_err(|_| anyhow::anyhow!("certificate install lock poisoned"))?;
        let material = self.persist_renewed(certificate_pem)?;
        let material = Arc::new(material);
        self.current.send_replace(material.clone());
        Ok(material)
    }

    /// Validate and atomically persist a renewed certificate WITHOUT
    /// publishing it. Split out so tests can model a crash between the rename
    /// and the runtime swap.
    pub(crate) fn persist_renewed(&self, certificate_pem: &[u8]) -> Result<CertMaterial> {
        let current = self.current();
        let material = parse_material(
            &self.relay_id,
            &self.key_spki_der,
            certificate_pem.to_vec(),
            current.key_pem.clone(),
            current.intermediate_ca_pem.clone(),
        )?;
        write_atomic(&self.paths.certificate_path, certificate_pem, 0o644)
            .with_context(|| format!("persist {}", self.paths.certificate_path.display()))?;
        Ok(material)
    }
}

/// SubjectPublicKeyInfo DER of the key in `key_pem`.
pub fn key_spki_der(key_pem: &[u8]) -> Result<Vec<u8>> {
    let key_pem = std::str::from_utf8(key_pem).context("Relay private key PEM is not UTF-8")?;
    let key = KeyPair::from_pem(key_pem).context("parse Relay private key")?;
    Ok(key.public_key_der())
}

/// Parse one PEM leaf and check it is this relay's certificate for this key.
fn parse_material(
    relay_id: &str,
    key_spki_der: &[u8],
    certificate_pem: Vec<u8>,
    key_pem: Vec<u8>,
    intermediate_ca_pem: Vec<u8>,
) -> Result<CertMaterial> {
    let leaf = single_leaf(&certificate_pem)?;
    validate_relay_certificate(&leaf, relay_id)?;
    let (_, cert) = X509Certificate::from_der(leaf.as_ref())
        .map_err(|e| anyhow::anyhow!("parse Relay certificate: {e:?}"))?;
    if cert.public_key().raw != key_spki_der {
        bail!("Relay certificate does not belong to the Relay's private key");
    }
    Ok(CertMaterial {
        serial_hex: serial_hex(cert.raw_serial()),
        not_before_unix: cert.validity().not_before.timestamp(),
        not_after_unix: cert.validity().not_after.timestamp(),
        certificate_pem,
        key_pem,
        intermediate_ca_pem,
    })
}

pub(crate) fn single_leaf(
    certificate_pem: &[u8],
) -> Result<rustls::pki_types::CertificateDer<'static>> {
    let mut pem = certificate_pem;
    let mut chain = certs(&mut pem)
        .collect::<std::result::Result<Vec<_>, _>>()
        .context("parse Relay certificate PEM")?;
    if chain.len() != 1 {
        bail!(
            "Relay certificate PEM must contain exactly one certificate, got {}",
            chain.len()
        );
    }
    Ok(chain.remove(0))
}

/// Lowercase hex of the serial without leading zero digits, the form the
/// controller stores (`SerialNumber.Text(16)`).
pub(crate) fn serial_hex(raw_serial: &[u8]) -> String {
    let hex = hex::encode(raw_serial);
    let trimmed = hex.trim_start_matches('0');
    if trimmed.is_empty() {
        "0".to_owned()
    } else {
        trimmed.to_owned()
    }
}

/// Crash-safe replace of `path`: write a temp file in the same directory,
/// fsync it, rename it over `path`, then fsync the directory. A crash leaves
/// either the old file or the new one — never a partial file.
pub fn write_atomic(path: &Path, data: &[u8], mode: u32) -> io::Result<()> {
    write_atomic_with_hook(path, data, mode, || Ok(()))
}

fn write_atomic_with_hook(
    path: &Path,
    data: &[u8],
    mode: u32,
    before_rename: impl FnOnce() -> io::Result<()>,
) -> io::Result<()> {
    let dir = path
        .parent()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "path has no parent"))?;
    let file_name = path
        .file_name()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "path has no file name"))?
        .to_string_lossy();
    let tmp = dir.join(format!(
        ".{file_name}.tmp.{}",
        uuid::Uuid::new_v4().simple()
    ));

    let result = (|| {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .mode(mode)
            .open(&tmp)?;
        file.write_all(data)?;
        file.sync_all()?;
        drop(file);
        before_rename()?;
        fs::rename(&tmp, path)?;
        File::open(dir)?.sync_all()
    })();
    if result.is_err() {
        let _ = fs::remove_file(&tmp);
    }
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_support::{TestPki, RELAY_ID};

    fn state_dir() -> PathBuf {
        let dir =
            std::env::temp_dir().join(format!("relay-cert-{}", uuid::Uuid::new_v4().simple()));
        fs::create_dir_all(&dir).unwrap();
        dir
    }

    fn paths(dir: &Path) -> CertPaths {
        CertPaths {
            key_path: dir.join("relay.key"),
            certificate_path: dir.join("relay.crt"),
            intermediate_ca_path: dir.join("intermediate-ca.crt"),
        }
    }

    #[test]
    fn write_atomic_replaces_content_and_leaves_no_temp_files() {
        let dir = state_dir();
        let path = dir.join("relay.crt");
        fs::write(&path, b"old").unwrap();

        write_atomic(&path, b"new", 0o644).unwrap();

        assert_eq!(fs::read(&path).unwrap(), b"new");
        let leftovers: Vec<_> = fs::read_dir(&dir).unwrap().filter_map(|e| e.ok()).collect();
        assert_eq!(leftovers.len(), 1, "temp file left behind: {leftovers:?}");
    }

    #[test]
    fn write_atomic_crash_before_rename_keeps_old_file_intact() {
        let dir = state_dir();
        let path = dir.join("relay.crt");
        fs::write(&path, b"old certificate").unwrap();

        let err = write_atomic_with_hook(&path, b"new certificate", 0o644, || {
            Err(io::Error::other("simulated crash before rename"))
        });

        assert!(err.is_err());
        assert_eq!(fs::read(&path).unwrap(), b"old certificate");
        let leftovers: Vec<_> = fs::read_dir(&dir).unwrap().filter_map(|e| e.ok()).collect();
        assert_eq!(
            leftovers.len(),
            1,
            "partial temp file left behind: {leftovers:?}"
        );
    }

    #[test]
    fn install_persists_before_publishing_and_notifies_subscribers() {
        let pki = TestPki::new();
        let dir = state_dir();
        let key = pki.relay_key();
        pki.write_state(&dir, &key, &pki.relay_cert(RELAY_ID, &key, 0, 3600));
        let manager = CertManager::load(RELAY_ID, paths(&dir)).unwrap();
        let old_serial = manager.current().serial_hex.clone();
        let mut rx = manager.subscribe();

        let renewed = pki.relay_cert(RELAY_ID, &key, 0, 7200);
        let installed = manager.install_renewed(renewed.as_bytes()).unwrap();

        assert_ne!(installed.serial_hex, old_serial);
        assert_eq!(fs::read(dir.join("relay.crt")).unwrap(), renewed.as_bytes());
        assert!(rx.has_changed().unwrap());
        assert_eq!(rx.borrow_and_update().serial_hex, installed.serial_hex);
        assert_eq!(manager.current().serial_hex, installed.serial_hex);
    }

    #[test]
    fn install_rejects_certificate_for_another_key_without_touching_disk() {
        let pki = TestPki::new();
        let dir = state_dir();
        let key = pki.relay_key();
        let original = pki.relay_cert(RELAY_ID, &key, 0, 3600);
        pki.write_state(&dir, &key, &original);
        let manager = CertManager::load(RELAY_ID, paths(&dir)).unwrap();

        let other_key = pki.relay_key();
        let foreign = pki.relay_cert(RELAY_ID, &other_key, 0, 3600);
        assert!(manager.install_renewed(foreign.as_bytes()).is_err());
        assert_eq!(
            fs::read(dir.join("relay.crt")).unwrap(),
            original.as_bytes()
        );
    }

    /// A crash after the atomic rename but before the runtime swap: the new
    /// certificate is on disk but was never published. On restart the relay
    /// loads the persisted (renewed) certificate with the same key.
    #[test]
    fn restart_after_crash_between_rename_and_swap_uses_persisted_certificate() {
        let pki = TestPki::new();
        let dir = state_dir();
        let key = pki.relay_key();
        pki.write_state(&dir, &key, &pki.relay_cert(RELAY_ID, &key, 0, 3600));
        let before = CertManager::load(RELAY_ID, paths(&dir)).unwrap();
        let key_before = fs::read(dir.join("relay.key")).unwrap();

        let renewed = pki.relay_cert(RELAY_ID, &key, 0, 7200);
        let persisted = before.persist_renewed(renewed.as_bytes()).unwrap();
        // "crash": nothing published
        assert_ne!(before.current().serial_hex, persisted.serial_hex);

        let restarted = CertManager::load(RELAY_ID, paths(&dir)).unwrap();
        assert_eq!(restarted.current().serial_hex, persisted.serial_hex);
        assert_eq!(
            fs::read(dir.join("relay.key")).unwrap(),
            key_before,
            "key must never be rewritten"
        );
    }

    #[test]
    fn only_one_scheduler_can_be_claimed() {
        let pki = TestPki::new();
        let dir = state_dir();
        let key = pki.relay_key();
        pki.write_state(&dir, &key, &pki.relay_cert(RELAY_ID, &key, 0, 3600));
        let manager = CertManager::load(RELAY_ID, paths(&dir)).unwrap();

        assert!(manager.claim_scheduler());
        assert!(!manager.claim_scheduler());
    }

    #[test]
    fn serial_hex_matches_controller_format() {
        assert_eq!(serial_hex(&[0x00, 0x2a]), "2a");
        assert_eq!(serial_hex(&[0x0b, 0x0b]), "b0b");
        assert_eq!(serial_hex(&[0x00]), "0");
    }
}
