//! Single owner of the Connector's current certificate (Sprint 20 Phase G-2a).
//!
//! Every TLS consumer — the control stream, the device-tunnel TLS and QUIC
//! listeners, the Relay inner-TLS handler, Relay dials/probes and the
//! Shield-proxy controller channel — reads the current certificate from here
//! and subscribes to changes. Nothing keeps its own copy of the PEM bytes.
//!
//! A renewed certificate is **verified, then persisted atomically, then
//! published**, so a consumer never presents a certificate that is not on
//! disk. `connector.key` is loaded once and never rewritten.

use std::fs::{self, File, OpenOptions};
use std::io::{self, Write};
use std::os::unix::fs::OpenOptionsExt;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant, SystemTime, UNIX_EPOCH};

use anyhow::{bail, Context, Result};
use parking_lot::Mutex;
use rcgen::{KeyPair, PublicKeyData};
use rustls::pki_types::{CertificateDer, PrivateKeyDer};
use rustls::server::{ClientHello, ResolvesServerCert};
use rustls::sign::CertifiedKey;
use rustls_pemfile::{certs, private_key};
use tokio::sync::watch;
use x509_parser::certificate::X509Certificate;
use x509_parser::extensions::GeneralName;
use x509_parser::prelude::FromDer;

use super::cert_store::CertStore;

const CERT_FILE: &str = "connector.crt";
const CA_BUNDLE_FILE: &str = "workspace_ca.crt";

/// One immutable snapshot of the Connector's certificate material.
pub struct CertMaterial {
    /// PEM material in the shape the existing TLS builders take.
    pub store: CertStore,
    /// Leaf serial, lowercase hex without leading zeros (controller format).
    pub serial_hex: String,
    pub not_after_unix: i64,
    /// Pre-built rustls key for holder-backed certificate resolvers.
    pub certified_key: Arc<CertifiedKey>,
}

impl std::fmt::Debug for CertMaterial {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("CertMaterial")
            .field("serial_hex", &self.serial_hex)
            .field("not_after_unix", &self.not_after_unix)
            .finish()
    }
}

/// Clears the single-flight renewal flag when dropped.
pub struct RenewalGuard<'a> {
    flag: &'a AtomicBool,
}

impl Drop for RenewalGuard<'_> {
    fn drop(&mut self) {
        self.flag.store(false, Ordering::SeqCst);
    }
}

pub struct CertHolder {
    state_dir: PathBuf,
    spiffe_id: String,
    key_spki_der: Vec<u8>,
    current: watch::Sender<Arc<CertMaterial>>,
    install_lock: Mutex<()>,
    renewing: AtomicBool,
    last_renewed_at: Mutex<Option<Instant>>,
}

impl CertHolder {
    /// Load `connector.crt`, `connector.key` and `workspace_ca.crt` from
    /// `state_dir`, checking the certificate is `spiffe_id` for this key.
    pub fn load(state_dir: &str, spiffe_id: &str) -> Result<Arc<Self>> {
        let store = CertStore::load(state_dir).context("load connector certificate material")?;
        let key_spki_der = key_spki_der(&store.key_pem)?;
        let material = build_material(spiffe_id, &key_spki_der, store)?;
        let (current, _) = watch::channel(Arc::new(material));
        Ok(Arc::new(Self {
            state_dir: PathBuf::from(state_dir),
            spiffe_id: spiffe_id.to_owned(),
            key_spki_der,
            current,
            install_lock: Mutex::new(()),
            renewing: AtomicBool::new(false),
            last_renewed_at: Mutex::new(None),
        }))
    }

    pub fn spiffe_id(&self) -> &str {
        &self.spiffe_id
    }

    /// The certificate every consumer must present right now.
    pub fn current(&self) -> Arc<CertMaterial> {
        self.current.borrow().clone()
    }

    /// Subscribe to certificate changes.
    pub fn subscribe(&self) -> watch::Receiver<Arc<CertMaterial>> {
        self.current.subscribe()
    }

    /// Single-flight renewal: returns a guard only if no renewal is running.
    pub fn try_begin_renewal(&self) -> Option<RenewalGuard<'_>> {
        if self.renewing.swap(true, Ordering::SeqCst) {
            None
        } else {
            Some(RenewalGuard {
                flag: &self.renewing,
            })
        }
    }

    /// Whether a renewed certificate was installed within `window`.
    pub fn renewed_within(&self, window: Duration) -> bool {
        self.last_renewed_at
            .lock()
            .map(|at| at.elapsed() < window)
            .unwrap_or(false)
    }

    /// Verify, persist, then publish a renewed certificate.
    ///
    /// `leaf_pem`, `workspace_ca_pem` and `intermediate_ca_pem` are the
    /// controller's `RenewCertResponse` fields. The certificate is written in
    /// the same shape enrollment uses (`connector.crt` = leaf + Workspace CA,
    /// `workspace_ca.crt` = Workspace CA + Platform Intermediate).
    pub fn install_renewed(
        &self,
        leaf_pem: &[u8],
        workspace_ca_pem: &[u8],
        intermediate_ca_pem: &[u8],
    ) -> Result<Arc<CertMaterial>> {
        let _guard = self.install_lock.lock();
        let current = self.current();
        let store = verify_renewed(
            &current,
            &self.spiffe_id,
            &self.key_spki_der,
            leaf_pem,
            workspace_ca_pem,
            intermediate_ca_pem,
        )?;
        let material = Arc::new(build_material(&self.spiffe_id, &self.key_spki_der, store)?);

        // Persist before publish. The CA bundle is pinned to the current one,
        // so writing it first never pairs a new leaf with a different CA.
        write_atomic(
            &self.state_dir.join(CA_BUNDLE_FILE),
            &material.store.workspace_ca_pem,
            0o644,
        )
        .context("persist renewed CA bundle")?;
        write_atomic(
            &self.state_dir.join(CERT_FILE),
            &material.store.cert_pem,
            0o644,
        )
        .context("persist renewed connector certificate")?;

        self.current.send_replace(material.clone());
        *self.last_renewed_at.lock() = Some(Instant::now());
        Ok(material)
    }
}

/// rustls certificate resolver that always serves the holder's current
/// certificate: new handshakes pick up a renewal, established sessions keep
/// the certificate they negotiated.
pub struct HolderCertResolver {
    holder: Arc<CertHolder>,
}

impl HolderCertResolver {
    pub fn new(holder: Arc<CertHolder>) -> Arc<Self> {
        Arc::new(Self { holder })
    }
}

impl std::fmt::Debug for HolderCertResolver {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("HolderCertResolver").finish_non_exhaustive()
    }
}

impl ResolvesServerCert for HolderCertResolver {
    fn resolve(&self, _client_hello: ClientHello<'_>) -> Option<Arc<CertifiedKey>> {
        Some(self.holder.current().certified_key.clone())
    }
}

// ---- verification ------------------------------------------------------------

fn parse_pem_chain(pem: &[u8], label: &str) -> Result<Vec<CertificateDer<'static>>> {
    let mut input = pem;
    let chain = certs(&mut input)
        .collect::<std::result::Result<Vec<_>, _>>()
        .with_context(|| format!("parse {label} PEM"))?;
    if chain.is_empty() {
        bail!("{label} PEM contains no certificates");
    }
    Ok(chain)
}

fn spiffe_uris(cert: &X509Certificate<'_>) -> Vec<String> {
    let Ok(Some(san)) = cert.subject_alternative_name() else {
        return Vec::new();
    };
    san.value
        .general_names
        .iter()
        .filter_map(|name| match name {
            GeneralName::URI(uri) if uri.starts_with("spiffe://") => Some((*uri).to_owned()),
            _ => None,
        })
        .collect()
}

/// SubjectPublicKeyInfo DER of the key in `key_pem`.
pub fn key_spki_der(key_pem: &[u8]) -> Result<Vec<u8>> {
    let pem = std::str::from_utf8(key_pem).context("connector.key is not UTF-8")?;
    Ok(KeyPair::from_pem(pem)
        .context("parse connector.key")?
        .subject_public_key_info())
}

pub(crate) fn serial_hex(raw_serial: &[u8]) -> String {
    let hex = hex::encode(raw_serial);
    let trimmed = hex.trim_start_matches('0');
    if trimmed.is_empty() {
        "0".to_owned()
    } else {
        trimmed.to_owned()
    }
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Check a renewal reply before trusting it and return the CertStore to
/// install. Nothing is written here.
fn verify_renewed(
    current: &CertMaterial,
    spiffe_id: &str,
    key_spki_der: &[u8],
    leaf_pem: &[u8],
    workspace_ca_pem: &[u8],
    intermediate_ca_pem: &[u8],
) -> Result<CertStore> {
    // Exactly one leaf certificate.
    let leaf_chain = parse_pem_chain(leaf_pem, "renewed certificate")?;
    if leaf_chain.len() != 1 {
        bail!(
            "renewed certificate PEM must contain exactly one certificate, got {}",
            leaf_chain.len()
        );
    }
    let (_, leaf) = X509Certificate::from_der(leaf_chain[0].as_ref())
        .map_err(|e| anyhow::anyhow!("parse renewed certificate: {e:?}"))?;

    // This Connector's identity and key (renewal keeps the key pair).
    if spiffe_uris(&leaf) != [spiffe_id.to_owned()] {
        bail!("renewed certificate is not for {spiffe_id}");
    }
    if leaf.public_key().raw != key_spki_der {
        bail!("renewed certificate is not for the connector's existing key");
    }

    // Trust anchors are pinned: the Workspace CA and Platform Intermediate
    // must be exactly the ones already on disk.
    let current_bundle = parse_pem_chain(&current.store.workspace_ca_pem, "current CA bundle")?;
    let new_workspace = parse_pem_chain(workspace_ca_pem, "returned Workspace CA")?;
    let new_intermediate = parse_pem_chain(intermediate_ca_pem, "returned Intermediate CA")?;
    if new_workspace[0] != current_bundle[0] {
        bail!("returned Workspace CA does not match the pinned Workspace CA");
    }
    if new_intermediate.last() != current_bundle.last() {
        bail!("returned Intermediate CA does not match the pinned Intermediate CA");
    }
    let (_, workspace_ca) = X509Certificate::from_der(new_workspace[0].as_ref())
        .map_err(|e| anyhow::anyhow!("parse Workspace CA: {e:?}"))?;
    leaf.verify_signature(Some(workspace_ca.public_key()))
        .map_err(|e| {
            anyhow::anyhow!("renewed certificate is not signed by the Workspace CA: {e:?}")
        })?;

    // A real renewal: new serial, still valid.
    if serial_hex(leaf.raw_serial()) == current.serial_hex {
        bail!("controller returned the current certificate, not a renewal");
    }
    if leaf.validity().not_after.timestamp() <= now_unix() {
        bail!("renewed certificate is already expired");
    }

    let leaf_pem = std::str::from_utf8(leaf_pem).context("renewed certificate is not UTF-8")?;
    let workspace_pem =
        std::str::from_utf8(workspace_ca_pem).context("Workspace CA is not UTF-8")?;
    let intermediate_pem =
        std::str::from_utf8(intermediate_ca_pem).context("Intermediate CA is not UTF-8")?;
    Ok(CertStore {
        cert_pem: format!("{}\n{}", leaf_pem.trim_end(), workspace_pem.trim_end()).into_bytes(),
        key_pem: current.store.key_pem.clone(),
        workspace_ca_pem: format!(
            "{}\n{}",
            workspace_pem.trim_end(),
            intermediate_pem.trim_end()
        )
        .into_bytes(),
    })
}

/// Build the material snapshot (and rustls key) for a store, checking the
/// leaf is `spiffe_id` for this key.
fn build_material(spiffe_id: &str, key_spki_der: &[u8], store: CertStore) -> Result<CertMaterial> {
    let chain = parse_pem_chain(&store.cert_pem, "connector certificate")?;
    let (_, leaf) = X509Certificate::from_der(chain[0].as_ref())
        .map_err(|e| anyhow::anyhow!("parse connector certificate: {e:?}"))?;
    if !spiffe_uris(&leaf).iter().any(|uri| uri == spiffe_id) {
        bail!("connector certificate is not for {spiffe_id}");
    }
    if leaf.public_key().raw != key_spki_der {
        bail!("connector certificate does not belong to connector.key");
    }
    let serial_hex = serial_hex(leaf.raw_serial());
    let not_after_unix = leaf.validity().not_after.timestamp();

    let key: PrivateKeyDer<'static> = private_key(&mut store.key_pem.as_slice())
        .context("parse connector.key")?
        .context("connector.key contains no private key")?;
    let certified_key =
        CertifiedKey::from_der(chain, key, &rustls::crypto::ring::default_provider())
            .context("build rustls key for connector certificate")?;

    Ok(CertMaterial {
        store,
        serial_hex,
        not_after_unix,
        certified_key: Arc::new(certified_key),
    })
}

// ---- atomic persistence --------------------------------------------------------

/// Crash-safe replace of `path`: temp file in the same directory → fsync(file)
/// → rename → fsync(parent directory). A crash leaves the old file or the new
/// one — never a partial file.
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
    use crate::test_support::TestPki;

    #[test]
    fn write_atomic_replaces_content_and_leaves_no_temp_files() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(CERT_FILE);
        fs::write(&path, b"old").unwrap();

        write_atomic(&path, b"new", 0o644).unwrap();

        assert_eq!(fs::read(&path).unwrap(), b"new");
        assert_eq!(
            fs::read_dir(dir.path()).unwrap().count(),
            1,
            "temp file left behind"
        );
    }

    #[test]
    fn write_atomic_crash_before_rename_keeps_old_file_intact() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join(CERT_FILE);
        fs::write(&path, b"old certificate").unwrap();

        let err = write_atomic_with_hook(&path, b"new certificate", 0o644, || {
            Err(io::Error::other("simulated crash before rename"))
        });

        assert!(err.is_err());
        assert_eq!(fs::read(&path).unwrap(), b"old certificate");
        assert_eq!(
            fs::read_dir(dir.path()).unwrap().count(),
            1,
            "partial temp file left behind"
        );
    }

    #[test]
    fn install_verifies_persists_then_notifies() {
        let pki = TestPki::new();
        let (dir, holder) = pki.holder(0, 3600);
        let old_serial = holder.current().serial_hex.clone();
        let key_before = fs::read(dir.path().join("connector.key")).unwrap();
        let mut rx = holder.subscribe();

        let renewed = pki.connector_leaf(0, 7200);
        let installed = holder
            .install_renewed(
                renewed.as_bytes(),
                pki.workspace_ca_pem.as_bytes(),
                pki.intermediate_pem.as_bytes(),
            )
            .unwrap();

        assert_ne!(installed.serial_hex, old_serial);
        assert!(rx.has_changed().unwrap(), "subscribers must be notified");
        assert_eq!(rx.borrow_and_update().serial_hex, installed.serial_hex);
        // Persisted in the enrollment shape: leaf + Workspace CA.
        let on_disk = fs::read_to_string(dir.path().join(CERT_FILE)).unwrap();
        assert!(on_disk.starts_with(renewed.trim_end()));
        assert!(on_disk.contains(pki.workspace_ca_pem.trim_end()));
        // Restart loads the persisted certificate; the key is never rewritten.
        let restarted = CertHolder::load(dir.path().to_str().unwrap(), &pki.spiffe_id).unwrap();
        assert_eq!(restarted.current().serial_hex, installed.serial_hex);
        assert_eq!(
            fs::read(dir.path().join("connector.key")).unwrap(),
            key_before
        );
    }

    #[test]
    fn install_rejects_bad_replies_without_touching_disk_or_memory() {
        let pki = TestPki::new();
        let (dir, holder) = pki.holder(0, 3600);
        let cert_before = fs::read(dir.path().join(CERT_FILE)).unwrap();
        let bundle_before = fs::read(dir.path().join(CA_BUNDLE_FILE)).unwrap();
        let serial_before = holder.current().serial_hex.clone();
        let ws = pki.workspace_ca_pem.as_bytes();
        let int = pki.intermediate_pem.as_bytes();

        let rogue = TestPki::new();
        let cases: Vec<(&str, String, Vec<u8>, Vec<u8>)> = vec![
            (
                "wrong SPIFFE ID",
                pki.leaf_for_spiffe("spiffe://ws-test.zecurity.in/connector/other", 0, 7200),
                ws.to_vec(),
                int.to_vec(),
            ),
            (
                "different key",
                pki.leaf_with_new_key(0, 7200),
                ws.to_vec(),
                int.to_vec(),
            ),
            (
                "rogue Workspace CA",
                rogue.connector_leaf(0, 7200),
                rogue.workspace_ca_pem.clone().into_bytes(),
                int.to_vec(),
            ),
            (
                "rogue Intermediate",
                pki.connector_leaf(0, 7200),
                ws.to_vec(),
                rogue.intermediate_pem.clone().into_bytes(),
            ),
            (
                "already expired",
                pki.connector_leaf(-7200, 3600),
                ws.to_vec(),
                int.to_vec(),
            ),
            (
                "garbage",
                "not a certificate".to_owned(),
                ws.to_vec(),
                int.to_vec(),
            ),
        ];
        for (name, leaf, ws_pem, int_pem) in cases {
            assert!(
                holder
                    .install_renewed(leaf.as_bytes(), &ws_pem, &int_pem)
                    .is_err(),
                "{name}: accepted"
            );
            assert_eq!(
                fs::read(dir.path().join(CERT_FILE)).unwrap(),
                cert_before,
                "{name}: cert rewritten"
            );
            assert_eq!(
                fs::read(dir.path().join(CA_BUNDLE_FILE)).unwrap(),
                bundle_before,
                "{name}: bundle rewritten"
            );
            assert_eq!(
                holder.current().serial_hex,
                serial_before,
                "{name}: published"
            );
        }
    }

    #[test]
    fn renewal_is_single_flight() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(0, 3600);
        let first = holder.try_begin_renewal();
        assert!(first.is_some());
        assert!(
            holder.try_begin_renewal().is_none(),
            "second concurrent renewal must be refused"
        );
        drop(first);
        assert!(
            holder.try_begin_renewal().is_some(),
            "guard must release on drop"
        );
    }
}
