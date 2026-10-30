//! Workspace CA selection and (D21-R A′) workspace CRL checks for the Shield.
//!
//! A copy of the Connector's `crl.rs` issuer logic (certificate audit bug 6),
//! with the same test vectors: the stored `workspace_ca.crt` bundle holds the
//! Workspace CA and the Platform Intermediate, and the roles are decided by
//! their relationship, never by their order. D21-R (a2) verifies the
//! controller-signed peer list with the Workspace CA key chosen here.

use std::collections::HashSet;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use tracing::{info, warn};
use x509_parser::extensions::ParsedExtension;
use x509_parser::oid_registry::{
    OID_X509_EXT_AUTHORITY_KEY_IDENTIFIER, OID_X509_EXT_SUBJECT_KEY_IDENTIFIER,
};
use x509_parser::prelude::*;

/// The two CAs in the stored bundle, by role.
#[derive(Clone, Debug)]
pub struct CrlIssuers {
    /// Signs `/ca.crl` (revoked devices and connectors) and, with a context
    /// prefix, the Shield's peer list (D21-R a2).
    pub workspace_ca_pem: Vec<u8>,
    /// Signs `/relay.crl` (revoked relays).
    #[allow(dead_code)]
    pub intermediate_pem: Vec<u8>,
}

/// Derives the two CRL issuers from the stored CA bundle by their actual
/// relationship, not their position: exactly two distinct, currently valid CA
/// certificates that may sign certificates and CRLs, where one (the Workspace
/// CA) is issued by the other (the Platform Intermediate) and not the reverse.
/// The Intermediate must not be self-signed (a root does not belong here) and
/// must be allowed to issue a CA. Anything else is rejected; callers then
/// leave revocation state unavailable, so checks deny.
///
/// The bundle itself was anchored in the platform root at enrollment and
/// renewal; this only decides which of its two certificates signs which CRL.
pub fn crl_issuers(bundle_pem: &[u8]) -> anyhow::Result<CrlIssuers> {
    let ders = issuer_certificates(bundle_pem, "CA bundle")?;
    let [first_der, second_der] = ders.as_slice() else {
        anyhow::bail!(
            "CA bundle must be exactly the Workspace CA and the Platform Intermediate, got {} certificates",
            ders.len()
        );
    };
    if first_der.as_ref() == second_der.as_ref() {
        anyhow::bail!("CA bundle contains the same certificate twice");
    }
    let first = parse_crl_signing_ca(first_der.as_ref(), "first CA bundle certificate")?;
    let second = parse_crl_signing_ca(second_der.as_ref(), "second CA bundle certificate")?;
    if first.subject() == second.subject() {
        anyhow::bail!("CA bundle certificates have the same subject");
    }
    let (workspace_der, workspace, intermediate_der, intermediate) =
        match (issued_by(&first, &second), issued_by(&second, &first)) {
            (true, false) => (first_der, first, second_der, second),
            (false, true) => (second_der, second, first_der, first),
            (true, true) => anyhow::bail!("CA bundle certificates certify each other"),
            (false, false) => {
                anyhow::bail!("CA bundle is not a Workspace CA issued by the Platform Intermediate")
            }
        };
    if issued_by(&intermediate, &intermediate) {
        anyhow::bail!("Platform Intermediate must not be self-signed");
    }
    if issued_by(&workspace, &workspace) {
        anyhow::bail!("Workspace CA must not be self-signed");
    }
    let path_len = intermediate
        .basic_constraints()
        .map_err(|e| anyhow::anyhow!("Platform Intermediate basic constraints: {e:?}"))?
        .and_then(|bc| bc.value.path_len_constraint);
    if path_len == Some(0) {
        anyhow::bail!("Platform Intermediate may not issue CA certificates (path length 0)");
    }
    Ok(CrlIssuers {
        workspace_ca_pem: pem_from_der(workspace_der.as_ref()),
        intermediate_pem: pem_from_der(intermediate_der.as_ref()),
    })
}

fn issuer_certificates(
    pem: &[u8],
    label: &str,
) -> anyhow::Result<Vec<rustls::pki_types::CertificateDer<'static>>> {
    let mut reader = pem;
    rustls_pemfile::certs(&mut reader)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| anyhow::anyhow!("failed to parse {label}: {e}"))
}

/// A currently valid CA certificate allowed to sign certificates and CRLs.
fn parse_crl_signing_ca<'a>(der: &'a [u8], label: &str) -> anyhow::Result<X509Certificate<'a>> {
    let (remaining, cert) =
        parse_x509_certificate(der).map_err(|e| anyhow::anyhow!("{label} parse error: {e:?}"))?;
    if !remaining.is_empty() {
        anyhow::bail!("trailing data after {label}");
    }
    if !cert.is_ca() {
        anyhow::bail!("{label} is not a CA certificate");
    }
    let key_usage = cert
        .key_usage()
        .map_err(|e| anyhow::anyhow!("{label} key usage: {e:?}"))?
        .ok_or_else(|| anyhow::anyhow!("{label} has no key usage"))?;
    if !key_usage.value.key_cert_sign() || !key_usage.value.crl_sign() {
        anyhow::bail!("{label} may not sign certificates and CRLs");
    }
    if !cert.validity().is_valid() {
        anyhow::bail!("{label} is outside its validity period");
    }
    Ok(cert)
}

/// `child` names `issuer` as its issuer and carries its signature.
fn issued_by(child: &X509Certificate<'_>, issuer: &X509Certificate<'_>) -> bool {
    child.issuer() == issuer.subject() && child.verify_signature(Some(issuer.public_key())).is_ok()
}

fn pem_from_der(der: &[u8]) -> Vec<u8> {
    use base64::Engine as _;
    let body = base64::engine::general_purpose::STANDARD.encode(der);
    let mut pem = String::from("-----BEGIN CERTIFICATE-----\n");
    for line in body.as_bytes().chunks(64) {
        pem.push_str(std::str::from_utf8(line).expect("base64 is ASCII"));
        pem.push('\n');
    }
    pem.push_str("-----END CERTIFICATE-----\n");
    pem.into_bytes()
}

// ── D21-R A′: the workspace CRL, forwarded by the Connector ──────────────────

/// A verified workspace CRL.
#[derive(Debug)]
struct ValidCrl {
    revoked: HashSet<Vec<u8>>,
    this_update: i64,
    next_update: i64,
}

/// Result of a revocation lookup. `Unavailable` (no CRL, or past its
/// nextUpdate) is distinct from `NotRevoked` so it can't be mistaken for it.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RevocationStatus {
    Revoked,
    NotRevoked,
    Unavailable,
}

/// The newest workspace CRL the Shield has seen, from any Connector, and its
/// DER persisted in the state dir (`workspace.crl`) so a restart keeps it.
/// A Connector can withhold a CRL or offer an older one; neither replaces a
/// newer CRL, so a revoked Connector stays refused once any CRL listed it,
/// and an absent CRL fails closed after its nextUpdate.
#[derive(Debug, Default)]
pub struct CrlStore {
    current: Mutex<Option<ValidCrl>>,
    path: Option<PathBuf>,
}

impl CrlStore {
    /// A store persisting to `<state_dir>/workspace.crl`, starting from the
    /// saved CRL when it still verifies against the stored bundle and is
    /// within its validity window.
    pub fn load(state_dir: &str) -> Self {
        let path = Path::new(state_dir).join("workspace.crl");
        let store = Self {
            current: Mutex::new(None),
            path: Some(path.clone()),
        };
        let saved = std::fs::read(&path).ok();
        let issuer = std::fs::read(Path::new(state_dir).join("workspace_ca.crt"))
            .ok()
            .and_then(|bundle| crl_issuers(&bundle).ok());
        if let (Some(der), Some(issuers)) = (saved, issuer) {
            match verified(&der, &issuers.workspace_ca_pem) {
                Ok(crl) => *store.current.lock().unwrap() = Some(crl),
                Err(e) => warn!(error = %e, "saved workspace CRL not used"),
            }
        }
        store
    }

    /// Offer a CRL received from a Connector. Installed (and saved) only when
    /// it verifies against `workspace_ca_pem` and is newer (later
    /// thisUpdate) than the current one. Returns whether it was installed.
    pub fn offer(&self, der: &[u8], workspace_ca_pem: &[u8]) -> anyhow::Result<bool> {
        let crl = verified(der, workspace_ca_pem)?;
        let mut current = self.current.lock().unwrap();
        if current
            .as_ref()
            .is_some_and(|c| crl.this_update <= c.this_update)
        {
            return Ok(false);
        }
        info!(
            revoked = crl.revoked.len(),
            next_update = crl.next_update,
            "workspace CRL installed"
        );
        *current = Some(crl);
        drop(current);
        if let Some(path) = &self.path {
            if let Err(e) = std::fs::write(path, der) {
                warn!(error = %e, "failed to save the workspace CRL");
            }
        }
        Ok(true)
    }

    /// Fail-closed lookup of a certificate serial (raw DER serial bytes).
    pub fn check(&self, serial: &[u8], now: i64) -> RevocationStatus {
        match self.current.lock().unwrap().as_ref() {
            Some(crl) if now < crl.next_update => {
                if crl.revoked.contains(serial) {
                    RevocationStatus::Revoked
                } else {
                    RevocationStatus::NotRevoked
                }
            }
            _ => RevocationStatus::Unavailable,
        }
    }
}

/// Seconds since the epoch, the clock CRL times are compared with.
pub fn now_unix() -> i64 {
    ASN1Time::now().timestamp()
}

fn verified(der: &[u8], workspace_ca_pem: &[u8]) -> anyhow::Result<ValidCrl> {
    let (remaining, crl) =
        parse_x509_crl(der).map_err(|e| anyhow::anyhow!("CRL parse error: {:?}", e))?;
    if !remaining.is_empty() {
        anyhow::bail!("trailing data after crl");
    }
    validate_crl(&crl, workspace_ca_pem)
}

fn validate_crl(
    crl: &CertificateRevocationList<'_>,
    issuer_ca_pem: &[u8],
) -> anyhow::Result<ValidCrl> {
    // Exactly one expected issuer (certificate audit bug 6): a bundle would
    // let the CRL pick whichever CA matches its issuer name.
    let issuer_ders = issuer_certificates(issuer_ca_pem, "issuer_ca_pem")?;
    let [issuer_der] = issuer_ders.as_slice() else {
        anyhow::bail!(
            "CRL issuer must be exactly one CA certificate, got {}",
            issuer_ders.len()
        );
    };
    let (remaining, issuer) = parse_x509_certificate(issuer_der.as_ref())
        .map_err(|e| anyhow::anyhow!("issuer CA parse error: {:?}", e))?;
    if !remaining.is_empty() {
        anyhow::bail!("trailing data after issuer CA certificate");
    }
    if crl.issuer() != issuer.subject() {
        anyhow::bail!("crl issuer does not match expected ca subject");
    }
    let crl_aki = crl
        .tbs_cert_list
        .find_extension(&OID_X509_EXT_AUTHORITY_KEY_IDENTIFIER)
        .ok_or_else(|| anyhow::anyhow!("CRL has no Authority Key Identifier"))?;

    let crl_key_id = match crl_aki.parsed_extension() {
        ParsedExtension::AuthorityKeyIdentifier(aki) => {
            aki.key_identifier
                .as_ref()
                .ok_or_else(|| anyhow::anyhow!("CRL AKI has no key identifier"))?
                .0
        }
        _ => anyhow::bail!("CRL AKI extension is malformed"),
    };
    let issuer_ski = issuer
        .tbs_certificate
        .get_extension_unique(&OID_X509_EXT_SUBJECT_KEY_IDENTIFIER)
        .map_err(|e| anyhow::anyhow!("failed to read issuer ca ski: {:?}", e))?
        .ok_or_else(|| anyhow::anyhow!("issuer CA has no Subject Key Identifier"))?;

    let issuer_key_id = match issuer_ski.parsed_extension() {
        ParsedExtension::SubjectKeyIdentifier(ski) => ski.0,
        _ => anyhow::bail!("issuer CA SKI extension is malformed"),
    };
    if crl_key_id != issuer_key_id {
        anyhow::bail!("CRL Authority Key Identifier does not match issuer CA");
    }
    crl.verify_signature(&issuer.tbs_certificate.subject_pki)
        .map_err(|e| anyhow::anyhow!("CRL signature verification failed: {:?}", e))?;
    let now = ASN1Time::now().timestamp();
    let this_update = crl.last_update().timestamp();
    let next_update = validate_update_times(
        this_update,
        crl.next_update().map(|value| value.timestamp()),
        now,
    )?;
    let revoked = crl
        .iter_revoked_certificates()
        .map(|entry| entry.raw_serial().to_vec())
        .collect();

    Ok(ValidCrl {
        revoked,
        this_update,
        next_update,
    })
}

fn validate_update_times(
    this_update: i64,
    next_update: Option<i64>,
    now: i64,
) -> anyhow::Result<i64> {
    if this_update > now {
        anyhow::bail!("CRL thisUpdate is in the future");
    }

    let next_update = next_update.ok_or_else(|| anyhow::anyhow!("CRL is missing nextUpdate"))?;

    if next_update <= this_update {
        anyhow::bail!("CRL nextUpdate is not after thisUpdate");
    }
    if now >= next_update {
        anyhow::bail!("CRL has expired");
    }

    Ok(next_update)
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use ::time::{Duration as TimeDuration, OffsetDateTime};
    use rcgen::{
        BasicConstraints, CertificateParams, CertificateRevocationListParams, CertifiedIssuer,
        DistinguishedName, DnType, IsCa, Issuer, KeyIdMethod, KeyPair, KeyUsagePurpose,
        RevokedCertParams, SerialNumber, PKCS_ECDSA_P256_SHA256,
    };

    pub const CONNECTOR_SERIAL: &[u8] = &[0x5e, 0xed];
    const OTHER_SERIAL: &[u8] = &[0x7e, 0x1a];
    const INT_KID: &[u8] = b"intermediate-kid";
    const WS_KID: &[u8] = b"workspace-kid";

    fn ca_params(cn: &str, key_id: &[u8], constraint: BasicConstraints) -> CertificateParams {
        let mut params = CertificateParams::default();
        let mut dn = DistinguishedName::new();
        dn.push(DnType::CommonName, cn);
        params.distinguished_name = dn;
        params.is_ca = IsCa::Ca(constraint);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        params.key_identifier_method = KeyIdMethod::PreSpecified(key_id.to_vec());
        params
    }

    fn key() -> KeyPair {
        KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256).unwrap()
    }

    /// Platform Root → Platform Intermediate → Workspace CA.
    pub struct Pki {
        intermediate: CertifiedIssuer<'static, KeyPair>,
        workspace: CertifiedIssuer<'static, KeyPair>,
    }

    impl Pki {
        pub fn new() -> Self {
            let root = CertifiedIssuer::self_signed(
                ca_params(
                    "Platform Root",
                    b"root-kid",
                    BasicConstraints::Constrained(2),
                ),
                key(),
            )
            .unwrap();
            let intermediate = CertifiedIssuer::signed_by(
                ca_params(
                    "Platform Intermediate",
                    INT_KID,
                    BasicConstraints::Constrained(1),
                ),
                key(),
                &root,
            )
            .unwrap();
            let workspace = CertifiedIssuer::signed_by(
                ca_params(
                    "workspace-ws-test",
                    WS_KID,
                    BasicConstraints::Constrained(0),
                ),
                key(),
                &intermediate,
            )
            .unwrap();
            Self {
                intermediate,
                workspace,
            }
        }

        /// The stored workspace_ca.crt: Workspace CA + Platform Intermediate.
        pub fn bundle(&self) -> String {
            format!("{}{}", self.workspace.pem(), self.intermediate.pem())
        }

        pub fn workspace_pem(&self) -> Vec<u8> {
            self.workspace.pem().into_bytes()
        }

        /// A workspace CRL signed `age` ago, valid for `validity` after that.
        pub fn crl(&self, revoked: &[&[u8]], age: TimeDuration, validity: TimeDuration) -> Vec<u8> {
            signed_crl(&self.workspace, WS_KID, revoked, age, validity)
        }
    }

    fn signed_crl(
        issuer: &Issuer<'_, KeyPair>,
        key_id: &[u8],
        revoked: &[&[u8]],
        age: TimeDuration,
        validity: TimeDuration,
    ) -> Vec<u8> {
        let this_update = OffsetDateTime::now_utc() - age;
        CertificateRevocationListParams {
            this_update,
            next_update: this_update + validity,
            crl_number: SerialNumber::from(1u64),
            issuing_distribution_point: None,
            revoked_certs: revoked
                .iter()
                .map(|serial| RevokedCertParams {
                    serial_number: SerialNumber::from_slice(serial),
                    revocation_time: this_update,
                    reason_code: None,
                    invalidity_date: None,
                })
                .collect(),
            key_identifier_method: KeyIdMethod::PreSpecified(key_id.to_vec()),
        }
        .signed_by(issuer)
        .unwrap()
        .der()
        .to_vec()
    }

    fn day() -> TimeDuration {
        TimeDuration::hours(24)
    }

    #[test]
    fn a_workspace_crl_is_checked_fail_closed() {
        let pki = Pki::new();
        let store = CrlStore::default();
        let now = now_unix();
        assert_eq!(
            store.check(CONNECTOR_SERIAL, now),
            RevocationStatus::Unavailable
        );
        let crl = pki.crl(&[CONNECTOR_SERIAL], TimeDuration::minutes(1), day());
        assert!(store.offer(&crl, &pki.workspace_pem()).unwrap());
        assert_eq!(
            store.check(CONNECTOR_SERIAL, now),
            RevocationStatus::Revoked
        );
        assert_eq!(store.check(OTHER_SERIAL, now), RevocationStatus::NotRevoked);
        // Past its nextUpdate it says nothing.
        let later = now + 24 * 3600;
        assert_eq!(
            store.check(OTHER_SERIAL, later),
            RevocationStatus::Unavailable
        );
    }

    /// A Connector can't forge a CRL, pass off the Intermediate's, or roll
    /// the Shield back to an older one that doesn't list it yet.
    #[test]
    fn only_a_newer_workspace_ca_crl_replaces_the_current_one() {
        let pki = Pki::new();
        let store = CrlStore::default();
        let newer = pki.crl(&[CONNECTOR_SERIAL], TimeDuration::minutes(1), day());
        let older = pki.crl(&[], TimeDuration::minutes(10), day());
        assert!(store.offer(&newer, &pki.workspace_pem()).unwrap());
        assert!(!store.offer(&older, &pki.workspace_pem()).unwrap());
        assert!(
            !store.offer(&newer, &pki.workspace_pem()).unwrap(),
            "same CRL again"
        );
        let by_intermediate =
            signed_crl(&pki.intermediate, INT_KID, &[], TimeDuration::ZERO, day());
        assert!(store.offer(&by_intermediate, &pki.workspace_pem()).is_err());
        let forged = Pki::new().crl(&[], TimeDuration::ZERO, day());
        assert!(store.offer(&forged, &pki.workspace_pem()).is_err());
        assert!(store.offer(b"not a crl", &pki.workspace_pem()).is_err());
        assert_eq!(
            store.check(CONNECTOR_SERIAL, now_unix()),
            RevocationStatus::Revoked
        );
    }

    /// The newest CRL survives a restart; an expired saved one is not used.
    #[test]
    fn the_newest_crl_is_kept_across_restarts() {
        let pki = Pki::new();
        let dir = std::env::temp_dir().join(format!(
            "zecurity-shield-crl-{}-{}",
            std::process::id(),
            OffsetDateTime::now_utc().unix_timestamp_nanos()
        ));
        std::fs::create_dir_all(&dir).unwrap();
        std::fs::write(dir.join("workspace_ca.crt"), pki.bundle()).unwrap();
        let state_dir = dir.to_str().unwrap();

        let store = CrlStore::load(state_dir);
        let crl = pki.crl(&[CONNECTOR_SERIAL], TimeDuration::minutes(1), day());
        assert!(store.offer(&crl, &pki.workspace_pem()).unwrap());
        assert_eq!(
            CrlStore::load(state_dir).check(CONNECTOR_SERIAL, now_unix()),
            RevocationStatus::Revoked
        );

        let expired = pki.crl(&[], TimeDuration::hours(30), day());
        std::fs::write(dir.join("workspace.crl"), expired).unwrap();
        assert_eq!(
            CrlStore::load(state_dir).check(OTHER_SERIAL, now_unix()),
            RevocationStatus::Unavailable
        );
        let _ = std::fs::remove_dir_all(dir);
    }
}
