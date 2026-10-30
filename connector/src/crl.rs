use parking_lot::RwLock;
use std::collections::HashSet;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use tokio::time::sleep;
use tracing::warn;
use x509_parser::extensions::ParsedExtension;
use x509_parser::oid_registry::{
    OID_X509_EXT_AUTHORITY_KEY_IDENTIFIER, OID_X509_EXT_SUBJECT_KEY_IDENTIFIER,
};
use x509_parser::prelude::*;

#[derive(Debug)]
struct ValidCrl {
    revoked: HashSet<Vec<u8>>,
    next_update: i64,
    /// The verified DER, forwarded to Shields as is (D21-R A′).
    der: Arc<Vec<u8>>,
}
/// Result of a revocation lookup. `Unavailable` is a distinct state so callers
/// cannot mistake "no valid CRL" for "not revoked" (fail-closed by construction).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RevocationStatus {
    Revoked,
    NotRevoked,
    Unavailable,
}
/// Caches revoked certificate serials fetched from the controller's /ca.crl endpoint.
///
/// The controller serves `GET /ca.crl?workspace_id=<uuid>` → DER-encoded CRL signed
/// by the workspace CA. Connector fetches on startup then refreshes every 60 s plus
/// 0–15 s of jitter (see `spawn_refresh` calls in main.rs).
///
/// M4 calls `check(serial_bytes)` inside `device_tunnel::handle_stream` after
/// extracting the peer cert serial from the TLS handshake.
#[derive(Clone, Debug, Default)]
pub struct CrlManager {
    cache: Arc<RwLock<Option<ValidCrl>>>,
}

impl CrlManager {
    pub fn new() -> Self {
        Self::default()
    }

    /// Fail-closed revocation lookup. Callers MUST deny on anything but
    /// `NotRevoked`, so an absent/expired cache can never read as "allow".
    pub fn check(&self, serial: &[u8]) -> RevocationStatus {
        let now = ASN1Time::now().timestamp();
        let guard = self.cache.read();
        match guard.as_ref() {
            Some(cache) if now < cache.next_update => {
                if cache.revoked.contains(serial) {
                    RevocationStatus::Revoked
                } else {
                    RevocationStatus::NotRevoked
                }
            }
            _ => RevocationStatus::Unavailable,
        }
    }

    /// Fetch the DER CRL from `url`, parse it, and replace the cached revoked set.
    ///
    /// Errors are non-fatal — the existing cache is kept on failure so a transient
    /// network blip does not grant access to revoked devices.
    pub async fn refresh(&self, url: &str, issuer_ca_pem: &[u8]) -> anyhow::Result<()> {
        let bytes = reqwest::get(url)
            .await
            .map_err(|e| anyhow::anyhow!("CRL fetch error: {e}"))?
            .error_for_status()
            .map_err(|e| anyhow::anyhow!("CRL fetch HTTP error: {e}"))?
            .bytes()
            .await
            .map_err(|e| anyhow::anyhow!("CRL body read error: {e}"))?;

        self.install_verified_der(&bytes, issuer_ca_pem)
    }

    /// Validate a DER-encoded CRL and atomically install it as the new cache.
    /// Invalid input never replaces a previously verified cache.
    pub fn install_verified_der(&self, crl_der: &[u8], issuer_ca_pem: &[u8]) -> anyhow::Result<()> {
        let (remaining, crl) =
            parse_x509_crl(crl_der).map_err(|e| anyhow::anyhow!("CRL parse error: {:?}", e))?;
        if !remaining.is_empty() {
            anyhow::bail!("trailing data after crl");
        }
        let mut validated = validate_crl(&crl, issuer_ca_pem)?;
        validated.der = Arc::new(crl_der.to_vec());
        *self.cache.write() = Some(validated);
        Ok(())
    }

    /// Spawn a background task that calls `refresh` every `interval_secs`.
    /// Logs a warning on failure but never panics — stale cache is kept.
    pub fn spawn_refresh(
        self,
        url: String,
        issuer_ca_pem: Vec<u8>,
        interval_secs: u64,
        jitter_secs: u64,
    ) {
        tokio::spawn(async move {
            loop {
                let jitter = if jitter_secs == 0 {
                    0
                } else {
                    SystemTime::now()
                        .duration_since(UNIX_EPOCH)
                        .unwrap_or_default()
                        .subsec_nanos() as u64
                        % (jitter_secs + 1)
                };

                sleep(Duration::from_secs(interval_secs + jitter)).await;

                if let Err(e) = self.refresh(&url, &issuer_ca_pem).await {
                    warn!("CRL refresh failed: {e}");
                }
            }
        });
    }

    /// D21-R A′: the DER of the cached CRL while it is still valid, for
    /// forwarding to Shields. The same `Arc` until a new CRL is installed.
    pub fn current_der(&self) -> Option<Arc<Vec<u8>>> {
        let now = ASN1Time::now().timestamp();
        self.cache
            .read()
            .as_ref()
            .filter(|cache| now < cache.next_update)
            .map(|cache| cache.der.clone())
    }

    pub fn has_valid_cache(&self) -> bool {
        let now = ASN1Time::now().timestamp();

        self.cache
            .read()
            .as_ref()
            .is_some_and(|cache| now < cache.next_update)
    }

    /// D21-R A′ tests: a valid cache whose DER is `der`.
    #[cfg(test)]
    pub(crate) fn install_test_der(&self, der: &[u8]) {
        *self.cache.write() = Some(ValidCrl {
            revoked: HashSet::new(),
            next_update: ASN1Time::now().timestamp() + 60,
            der: Arc::new(der.to_vec()),
        });
    }

    #[cfg(test)]
    pub(crate) fn install_test_cache(&self, serials: Vec<Vec<u8>>) {
        *self.cache.write() = Some(ValidCrl {
            revoked: serials.into_iter().collect(),
            next_update: ASN1Time::now().timestamp() + 60,
            der: Arc::default(),
        });
    }
}

/// The issuer each CRL is checked against, derived from the stored CA bundle
/// (Workspace CA + Platform Intermediate). Certificate audit bug 6: the two
/// CRLs are signed by different CAs, and each must be accepted only from its
/// own signer.
#[derive(Clone, Debug)]
pub struct CrlIssuers {
    /// Signs `/ca.crl` (revoked devices and connectors).
    pub workspace_ca_pem: Vec<u8>,
    /// Signs `/relay.crl` (revoked relays).
    pub intermediate_pem: Vec<u8>,
}

/// Starts the Workspace CRL (`crl_url`) and Relay CRL (`relay_crl_url`)
/// managers: an initial fetch, then periodic refresh. Each CRL is verified
/// against its own signer only (certificate audit bug 6). If the bundle does
/// not yield the issuers, both managers stay empty, so revocation checks deny
/// (fail closed). Returns (Workspace CRL manager, Relay CRL manager).
pub async fn start_crl_managers(
    bundle_pem: &[u8],
    crl_url: String,
    relay_crl_url: String,
) -> (CrlManager, CrlManager) {
    let crl_manager = CrlManager::new();
    let relay_crl_manager = CrlManager::new();
    let issuers = match crl_issuers(bundle_pem) {
        Ok(issuers) => issuers,
        Err(e) => {
            tracing::error!(
                "CA bundle does not yield the CRL issuers; revocation checks fail closed: {e:#}"
            );
            return (crl_manager, relay_crl_manager);
        }
    };

    if let Err(e) = crl_manager
        .refresh(&crl_url, &issuers.workspace_ca_pem)
        .await
    {
        warn!("initial CRL fetch failed; revocation state is unavailable: {e}");
    }
    crl_manager
        .clone()
        .spawn_refresh(crl_url, issuers.workspace_ca_pem, 60, 15);

    if let Err(e) = relay_crl_manager
        .refresh(&relay_crl_url, &issuers.intermediate_pem)
        .await
    {
        warn!("initial Relay CRL fetch failed; relay dialing is fail-closed: {e}");
    }
    relay_crl_manager
        .clone()
        .spawn_refresh(relay_crl_url, issuers.intermediate_pem, 60, 15);

    (crl_manager, relay_crl_manager)
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
        next_update,
        der: Arc::default(),
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
mod tests {
    use super::*;
    use ::time::{Duration as TimeDuration, OffsetDateTime};
    use rcgen::{
        BasicConstraints, CertificateParams, CertificateRevocationListParams, CertifiedIssuer,
        DistinguishedName, DnType, IsCa, KeyIdMethod, KeyPair, KeyUsagePurpose, RevokedCertParams,
        SerialNumber, PKCS_ECDSA_P256_SHA256,
    };

    const KEY_ID: &[u8] = b"zecurity-crl-test-key-id";
    const REVOKED_SERIAL: u64 = 42;
    const REVOKED_SERIAL_BYTES: &[u8] = &[42];

    fn issuer(common_name: &str, key_id: &[u8]) -> CertifiedIssuer<'static, KeyPair> {
        let mut params = CertificateParams::default();
        let mut distinguished_name = DistinguishedName::new();
        distinguished_name.push(DnType::CommonName, common_name);
        params.distinguished_name = distinguished_name;
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        params.key_identifier_method = KeyIdMethod::PreSpecified(key_id.to_vec());

        let key = KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256).unwrap();
        CertifiedIssuer::self_signed(params, key).unwrap()
    }

    fn crl_der(
        issuer: &CertifiedIssuer<'static, KeyPair>,
        this_update: OffsetDateTime,
        next_update: OffsetDateTime,
    ) -> Vec<u8> {
        CertificateRevocationListParams {
            this_update,
            next_update,
            crl_number: SerialNumber::from(1u64),
            issuing_distribution_point: None,
            revoked_certs: vec![RevokedCertParams {
                serial_number: SerialNumber::from(REVOKED_SERIAL),
                revocation_time: this_update,
                reason_code: None,
                invalidity_date: None,
            }],
            key_identifier_method: KeyIdMethod::PreSpecified(KEY_ID.to_vec()),
        }
        .signed_by(issuer)
        .unwrap()
        .der()
        .to_vec()
    }

    #[test]
    fn new_manager_has_no_valid_cache() {
        assert!(!CrlManager::new().has_valid_cache());
    }

    #[test]
    fn expired_cached_crl_is_not_valid_or_usable() {
        let manager = CrlManager::new();
        let mut revoked = HashSet::new();
        revoked.insert(REVOKED_SERIAL_BYTES.to_vec());
        *manager.cache.write() = Some(ValidCrl {
            revoked,
            next_update: ASN1Time::now().timestamp() - 1,
            der: Arc::default(),
        });

        assert!(!manager.has_valid_cache());
        assert_eq!(
            manager.check(REVOKED_SERIAL_BYTES),
            RevocationStatus::Unavailable
        );
    }

    #[test]
    fn accepts_valid_signed_crl_and_finds_revoked_serial() {
        let issuer = issuer("workspace-ca", KEY_ID);
        let now = OffsetDateTime::now_utc();
        let der = crl_der(
            &issuer,
            now - TimeDuration::minutes(1),
            now + TimeDuration::hours(1),
        );
        let manager = CrlManager::new();

        manager
            .install_verified_der(&der, issuer.pem().as_bytes())
            .unwrap();

        assert!(manager.has_valid_cache());
        assert_eq!(
            manager.check(REVOKED_SERIAL_BYTES),
            RevocationStatus::Revoked
        );
    }

    #[test]
    fn rejects_wrong_issuer() {
        let signer = issuer("workspace-ca", KEY_ID);
        let expected = issuer("different-ca", KEY_ID);
        let now = OffsetDateTime::now_utc();
        let der = crl_der(
            &signer,
            now - TimeDuration::minutes(1),
            now + TimeDuration::hours(1),
        );

        let error = CrlManager::new()
            .install_verified_der(&der, expected.pem().as_bytes())
            .unwrap_err();

        assert!(error.to_string().contains("issuer does not match"));
    }

    #[test]
    fn rejects_mismatched_authority_key_identifier() {
        let signer = issuer("workspace-ca", b"signer-key-id");
        let expected = issuer("workspace-ca", b"expected-key-id");
        let now = OffsetDateTime::now_utc();
        let der = crl_der(
            &signer,
            now - TimeDuration::minutes(1),
            now + TimeDuration::hours(1),
        );

        let error = CrlManager::new()
            .install_verified_der(&der, expected.pem().as_bytes())
            .unwrap_err();

        assert!(error.to_string().contains("Authority Key Identifier"));
    }

    #[test]
    fn rejects_wrong_signature() {
        let signer = issuer("workspace-ca", KEY_ID);
        let expected = issuer("workspace-ca", KEY_ID);
        let now = OffsetDateTime::now_utc();
        let der = crl_der(
            &signer,
            now - TimeDuration::minutes(1),
            now + TimeDuration::hours(1),
        );

        let error = CrlManager::new()
            .install_verified_der(&der, expected.pem().as_bytes())
            .unwrap_err();

        assert!(error.to_string().contains("signature verification failed"));
    }

    #[test]
    fn keeps_last_good_crl_after_failed_validation() {
        let valid_issuer = issuer("workspace-ca", KEY_ID);
        let wrong_issuer = issuer("wrong-ca", KEY_ID);
        let now = OffsetDateTime::now_utc();
        let der = crl_der(
            &valid_issuer,
            now - TimeDuration::minutes(1),
            now + TimeDuration::hours(1),
        );
        let manager = CrlManager::new();

        manager
            .install_verified_der(&der, valid_issuer.pem().as_bytes())
            .unwrap();
        assert!(manager
            .install_verified_der(&der, wrong_issuer.pem().as_bytes())
            .is_err());

        assert!(manager.has_valid_cache());
        assert_eq!(
            manager.check(REVOKED_SERIAL_BYTES),
            RevocationStatus::Revoked
        );
    }

    #[test]
    fn rejects_future_this_update() {
        assert!(validate_update_times(101, Some(200), 100).is_err());
    }

    #[test]
    fn rejects_missing_next_update() {
        assert!(validate_update_times(99, None, 100).is_err());
    }

    #[test]
    fn rejects_next_update_not_after_this_update() {
        assert!(validate_update_times(99, Some(99), 100).is_err());
    }

    #[test]
    fn rejects_expired_next_update() {
        assert!(validate_update_times(50, Some(100), 100).is_err());
    }

    #[test]
    fn accepts_current_update_window() {
        assert_eq!(validate_update_times(99, Some(101), 100).unwrap(), 101);
    }

    // ---- Sprint 21 B4: CRLs as the controller now signs them ----------------

    /// The controller's CRL window: thisUpdate = signing time − 5 min,
    /// nextUpdate = signing time + validity (controller/internal/pki crlWindow).
    const CONTROLLER_BACKDATE_SECS: i64 = 5 * 60;
    const CONSUMER_NOW: i64 = 1_791_441_726;

    fn controller_crl(signed_at: i64, validity: i64) -> (i64, Option<i64>) {
        (
            signed_at - CONTROLLER_BACKDATE_SECS,
            Some(signed_at + validity),
        )
    }

    #[test]
    fn controller_clock_up_to_five_minutes_ahead_is_accepted() {
        for ahead in [
            0,
            1,
            5,
            60,
            CONTROLLER_BACKDATE_SECS - 1,
            CONTROLLER_BACKDATE_SECS,
        ] {
            for validity in [600, 86_400] {
                let (this_update, next_update) = controller_crl(CONSUMER_NOW + ahead, validity);
                assert_eq!(
                    validate_update_times(this_update, next_update, CONSUMER_NOW).unwrap(),
                    CONSUMER_NOW + ahead + validity,
                    "controller {ahead}s ahead, validity {validity}s"
                );
            }
        }
    }

    #[test]
    fn crls_outside_the_tolerance_or_window_are_still_rejected() {
        // Controller more than 5 min ahead: thisUpdate is in the future.
        let (this_update, next_update) =
            controller_crl(CONSUMER_NOW + CONTROLLER_BACKDATE_SECS + 1, 600);
        assert!(validate_update_times(this_update, next_update, CONSUMER_NOW).is_err());
        // Signed 10 min ago with a 10 min validity: expired at exactly nextUpdate.
        let (this_update, next_update) = controller_crl(CONSUMER_NOW - 600, 600);
        assert!(validate_update_times(this_update, next_update, CONSUMER_NOW).is_err());
        // Long past nextUpdate.
        let (this_update, next_update) = controller_crl(CONSUMER_NOW - 86_400, 600);
        assert!(validate_update_times(this_update, next_update, CONSUMER_NOW).is_err());
        // No nextUpdate, or nextUpdate not after thisUpdate.
        assert!(validate_update_times(CONSUMER_NOW - 300, None, CONSUMER_NOW).is_err());
        assert!(
            validate_update_times(CONSUMER_NOW - 300, Some(CONSUMER_NOW - 300), CONSUMER_NOW)
                .is_err()
        );
    }
}

/// Certificate audit bug 6: the Workspace CRL (signed by the Workspace CA) and
/// the Relay CRL (signed by the Platform Intermediate) were both checked
/// against the whole CA bundle, so whichever bundle certificate matched the
/// CRL's issuer was accepted. Over plain HTTP, either genuine CRL could be
/// served in place of the other: revoked devices and connectors, or revoked
/// relays, would then pass.
#[cfg(test)]
mod issuer_binding_tests {
    use super::*;
    use ::time::{Duration as TimeDuration, OffsetDateTime};
    use rcgen::{
        BasicConstraints, CertificateParams, CertificateRevocationListParams, CertifiedIssuer,
        DistinguishedName, DnType, ExtendedKeyUsagePurpose, IsCa, Issuer, KeyIdMethod, KeyPair,
        KeyUsagePurpose, RevokedCertParams, SerialNumber, PKCS_ECDSA_P256_SHA256,
    };

    // Minimal DER serials (high bit clear), as CRL entries store them.
    const DEVICE_SERIAL: &[u8] = &[0x5e, 0xed];
    const RELAY_SERIAL: &[u8] = &[0x7e, 0x1a];

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

    /// Platform Root → Platform Intermediate → Workspace CA, shaped like the
    /// controller's PKI.
    struct Pki {
        root: CertifiedIssuer<'static, KeyPair>,
        intermediate: CertifiedIssuer<'static, KeyPair>,
        workspace: CertifiedIssuer<'static, KeyPair>,
        /// The Intermediate's key, to forge certificates under other names.
        intermediate_key_pem: String,
    }

    const ROOT_KID: &[u8] = b"root-kid";
    const INT_KID: &[u8] = b"intermediate-kid";
    const WS_KID: &[u8] = b"workspace-kid";

    impl Pki {
        fn new() -> Self {
            let root = CertifiedIssuer::self_signed(
                ca_params("Platform Root", ROOT_KID, BasicConstraints::Constrained(2)),
                key(),
            )
            .unwrap();
            let intermediate_key = key();
            let intermediate_key_pem = intermediate_key.serialize_pem();
            let intermediate = CertifiedIssuer::signed_by(
                ca_params(
                    "Platform Intermediate",
                    INT_KID,
                    BasicConstraints::Constrained(1),
                ),
                intermediate_key,
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
                root,
                intermediate,
                workspace,
                intermediate_key_pem,
            }
        }

        /// The stored bundle: Workspace CA + Platform Intermediate.
        fn bundle(&self) -> String {
            format!("{}{}", self.workspace.pem(), self.intermediate.pem())
        }

        /// `/ca.crl`: revokes a device, signed by the Workspace CA.
        fn workspace_crl(&self) -> Vec<u8> {
            crl(&self.workspace, WS_KID, DEVICE_SERIAL)
        }

        /// `/relay.crl`: revokes a relay, signed by the Platform Intermediate.
        fn relay_crl(&self) -> Vec<u8> {
            crl(&self.intermediate, INT_KID, RELAY_SERIAL)
        }
    }

    fn crl(issuer: &Issuer<'_, KeyPair>, key_id: &[u8], revoked: &[u8]) -> Vec<u8> {
        let now = OffsetDateTime::now_utc();
        CertificateRevocationListParams {
            this_update: now - TimeDuration::minutes(1),
            next_update: now + TimeDuration::hours(1),
            crl_number: SerialNumber::from(1u64),
            issuing_distribution_point: None,
            revoked_certs: vec![RevokedCertParams {
                serial_number: SerialNumber::from_slice(revoked),
                revocation_time: now - TimeDuration::minutes(1),
                reason_code: None,
                invalidity_date: None,
            }],
            key_identifier_method: KeyIdMethod::PreSpecified(key_id.to_vec()),
        }
        .signed_by(issuer)
        .unwrap()
        .der()
        .to_vec()
    }

    #[test]
    fn genuine_crls_validate_at_their_own_endpoints() {
        let pki = Pki::new();
        let issuers = crl_issuers(pki.bundle().as_bytes()).unwrap();

        let workspace = CrlManager::new();
        workspace
            .install_verified_der(&pki.workspace_crl(), &issuers.workspace_ca_pem)
            .unwrap();
        assert_eq!(workspace.check(DEVICE_SERIAL), RevocationStatus::Revoked);

        let relay = CrlManager::new();
        relay
            .install_verified_der(&pki.relay_crl(), &issuers.intermediate_pem)
            .unwrap();
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Revoked);
    }

    // A substituted CRL has a valid signature from the other bundle CA; it
    // must still be rejected, and a fresh manager must stay Unavailable (deny),
    // never NotRevoked.
    #[test]
    fn relay_crl_substituted_for_workspace_crl_is_rejected() {
        let pki = Pki::new();
        let issuers = crl_issuers(pki.bundle().as_bytes()).unwrap();
        let workspace = CrlManager::new();
        assert!(
            workspace
                .install_verified_der(&pki.relay_crl(), &issuers.workspace_ca_pem)
                .is_err(),
            "the Relay CRL was accepted as the Workspace CRL"
        );
        assert_eq!(
            workspace.check(DEVICE_SERIAL),
            RevocationStatus::Unavailable
        );
    }

    #[test]
    fn workspace_crl_substituted_for_relay_crl_is_rejected() {
        let pki = Pki::new();
        let issuers = crl_issuers(pki.bundle().as_bytes()).unwrap();
        let relay = CrlManager::new();
        assert!(
            relay
                .install_verified_der(&pki.workspace_crl(), &issuers.intermediate_pem)
                .is_err(),
            "the Workspace CRL was accepted as the Relay CRL"
        );
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Unavailable);
    }

    // A substituted CRL never replaces a genuine one already installed.
    #[test]
    fn substitution_keeps_the_last_genuine_crl() {
        let pki = Pki::new();
        let issuers = crl_issuers(pki.bundle().as_bytes()).unwrap();
        let workspace = CrlManager::new();
        workspace
            .install_verified_der(&pki.workspace_crl(), &issuers.workspace_ca_pem)
            .unwrap();
        assert!(workspace
            .install_verified_der(&pki.relay_crl(), &issuers.workspace_ca_pem)
            .is_err());
        assert_eq!(workspace.check(DEVICE_SERIAL), RevocationStatus::Revoked);
    }

    // A bundle of CAs must never be used as the issuer: it lets the CRL pick
    // its own signer.
    #[test]
    fn a_bundle_is_not_accepted_as_a_crl_issuer() {
        let pki = Pki::new();
        for der in [pki.workspace_crl(), pki.relay_crl()] {
            assert!(CrlManager::new()
                .install_verified_der(&der, pki.bundle().as_bytes())
                .is_err());
        }
    }

    #[test]
    fn roles_follow_the_chain_not_the_order() {
        let pki = Pki::new();
        let reversed = format!("{}{}", pki.intermediate.pem(), pki.workspace.pem());
        let issuers = crl_issuers(reversed.as_bytes()).unwrap();
        assert_eq!(issuers.workspace_ca_pem, pki.workspace.pem().into_bytes());
        assert_eq!(
            issuers.intermediate_pem,
            pki.intermediate.pem().into_bytes()
        );
    }

    fn rejected(bundle: &str, why: &str) {
        assert!(crl_issuers(bundle.as_bytes()).is_err(), "accepted: {why}");
    }

    #[test]
    fn malformed_or_wrongly_shaped_bundles_are_rejected() {
        let pki = Pki::new();
        let unrelated = CertifiedIssuer::self_signed(
            ca_params(
                "Unrelated CA",
                b"other-kid",
                BasicConstraints::Unconstrained,
            ),
            key(),
        )
        .unwrap();
        rejected("", "empty bundle");
        rejected("not a certificate", "garbage");
        rejected(
            "-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n",
            "undecodable certificate",
        );
        rejected(&pki.workspace.pem(), "Workspace CA only");
        rejected(&pki.intermediate.pem(), "Intermediate only");
        rejected(
            &format!("{}{}", pki.bundle(), pki.root.pem()),
            "extra certificate (root)",
        );
        rejected(
            &format!("{}{}", pki.workspace.pem(), pki.workspace.pem()),
            "duplicate certificate",
        );
        rejected(
            &format!("{}{}", pki.workspace.pem(), unrelated.pem()),
            "no issuing relationship",
        );
        rejected(
            &format!("{}{}", pki.workspace.pem(), pki.root.pem()),
            "Workspace CA with the root instead of its issuer",
        );
        rejected(
            &format!("{}{}", pki.intermediate.pem(), pki.root.pem()),
            "self-signed root in the Intermediate role",
        );
    }

    #[test]
    fn invalid_chains_are_rejected() {
        let pki = Pki::new();

        // Names say "issued by the Intermediate", but another key signed it.
        let impostor_key = key();
        let impostor = Issuer::new(
            ca_params(
                "Platform Intermediate",
                INT_KID,
                BasicConstraints::Constrained(1),
            ),
            impostor_key,
        );
        let forged = ca_params(
            "workspace-ws-test",
            WS_KID,
            BasicConstraints::Constrained(0),
        )
        .signed_by(&key(), &impostor)
        .unwrap();
        rejected(
            &format!("{}{}", forged.pem(), pki.intermediate.pem()),
            "bad signature",
        );

        // A leaf in the Workspace CA role.
        let mut leaf = CertificateParams::default();
        leaf.distinguished_name.push(DnType::CommonName, "leaf");
        leaf.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
        let leaf = leaf.signed_by(&key(), &pki.intermediate).unwrap();
        rejected(
            &format!("{}{}", leaf.pem(), pki.intermediate.pem()),
            "non-CA Workspace certificate",
        );

        // A Workspace CA that may not sign CRLs.
        let mut no_crl_sign = ca_params(
            "workspace-ws-test",
            WS_KID,
            BasicConstraints::Constrained(0),
        );
        no_crl_sign.key_usages = vec![KeyUsagePurpose::KeyCertSign];
        let no_crl_sign = no_crl_sign.signed_by(&key(), &pki.intermediate).unwrap();
        rejected(
            &format!("{}{}", no_crl_sign.pem(), pki.intermediate.pem()),
            "Workspace CA without cRLSign",
        );

        // An expired Workspace CA.
        let mut expired = ca_params(
            "workspace-ws-test",
            WS_KID,
            BasicConstraints::Constrained(0),
        );
        expired.not_before = OffsetDateTime::now_utc() - TimeDuration::days(10);
        expired.not_after = OffsetDateTime::now_utc() - TimeDuration::days(1);
        let expired = expired.signed_by(&key(), &pki.intermediate).unwrap();
        rejected(
            &format!("{}{}", expired.pem(), pki.intermediate.pem()),
            "expired Workspace CA",
        );

        // An Intermediate that may not issue CA certificates.
        let capped = CertifiedIssuer::signed_by(
            ca_params(
                "Platform Intermediate",
                INT_KID,
                BasicConstraints::Constrained(0),
            ),
            key(),
            &pki.root,
        )
        .unwrap();
        let ws = ca_params(
            "workspace-ws-test",
            WS_KID,
            BasicConstraints::Constrained(0),
        )
        .signed_by(&key(), &capped)
        .unwrap();
        rejected(
            &format!("{}{}", ws.pem(), capped.pem()),
            "Intermediate with path length 0",
        );
    }

    /// Serves `/ca.crl` and `/relay.crl` over plain HTTP, as the controller
    /// does; returns the base URL.
    async fn serve_crls(ca_crl: Vec<u8>, relay_crl: Vec<u8>) -> String {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            loop {
                let Ok((mut stream, _)) = listener.accept().await else {
                    return;
                };
                let (ca_crl, relay_crl) = (ca_crl.clone(), relay_crl.clone());
                tokio::spawn(async move {
                    let mut buf = vec![0u8; 4096];
                    let n = stream.read(&mut buf).await.unwrap_or(0);
                    let request = String::from_utf8_lossy(&buf[..n]);
                    let body = if request.starts_with("GET /relay.crl") {
                        relay_crl
                    } else {
                        ca_crl
                    };
                    let head = format!(
                        "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                        body.len()
                    );
                    let _ = stream.write_all(head.as_bytes()).await;
                    let _ = stream.write_all(&body).await;
                });
            }
        });
        format!("http://{addr}")
    }

    async fn started(
        bundle: &str,
        ca_crl: Vec<u8>,
        relay_crl: Vec<u8>,
    ) -> (CrlManager, CrlManager) {
        crate::test_support::install_crypto_provider();
        let base = serve_crls(ca_crl, relay_crl).await;
        start_crl_managers(
            bundle.as_bytes(),
            format!("{base}/ca.crl?workspace_id=ws"),
            format!("{base}/relay.crl"),
        )
        .await
    }

    #[tokio::test]
    async fn startup_installs_genuine_crls_at_their_endpoints() {
        let pki = Pki::new();
        let (workspace, relay) = started(&pki.bundle(), pki.workspace_crl(), pki.relay_crl()).await;
        assert_eq!(workspace.check(DEVICE_SERIAL), RevocationStatus::Revoked);
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Revoked);
    }

    // The attacker swaps the two genuine CRLs on the wire.
    #[tokio::test]
    async fn startup_rejects_swapped_crls() {
        let pki = Pki::new();
        let (workspace, relay) = started(&pki.bundle(), pki.relay_crl(), pki.workspace_crl()).await;
        assert_eq!(
            workspace.check(DEVICE_SERIAL),
            RevocationStatus::Unavailable
        );
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Unavailable);
    }

    #[tokio::test]
    async fn startup_with_an_unusable_bundle_fails_closed() {
        let pki = Pki::new();
        let (workspace, relay) =
            started(&pki.workspace.pem(), pki.workspace_crl(), pki.relay_crl()).await;
        assert_eq!(
            workspace.check(DEVICE_SERIAL),
            RevocationStatus::Unavailable
        );
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Unavailable);
    }

    /// A critical KeyUsage extension with keyCertSign and cRLSign.
    fn ca_key_usage_extension() -> rcgen::CustomExtension {
        let mut ext =
            rcgen::CustomExtension::from_oid_content(&[2, 5, 29, 15], vec![0x03, 0x02, 0x01, 0x06]);
        ext.set_criticality(true);
        ext
    }

    // Each case below is caught by exactly one check, so every check is
    // exercised on its own.
    #[test]
    fn each_role_check_rejects_on_its_own() {
        let pki = Pki::new();
        let ws = |params: CertificateParams| params.signed_by(&key(), &pki.intermediate).unwrap();

        // A Workspace CA carrying the Intermediate's own name.
        let same_subject = ws(ca_params(
            "Platform Intermediate",
            WS_KID,
            BasicConstraints::Constrained(0),
        ));
        rejected(
            &format!("{}{}", same_subject.pem(), pki.intermediate.pem()),
            "same subject as its issuer",
        );

        // Not a CA, although it carries the CA key usages.
        let mut not_ca = CertificateParams::default();
        not_ca
            .distinguished_name
            .push(DnType::CommonName, "workspace-ws-test");
        not_ca.is_ca = IsCa::ExplicitNoCa;
        // rcgen omits key usages on a non-CA; add keyCertSign + cRLSign directly.
        not_ca.custom_extensions = vec![ca_key_usage_extension()];
        rejected(
            &format!("{}{}", ws(not_ca).pem(), pki.intermediate.pem()),
            "non-CA with CA key usages",
        );

        // Signed with the Intermediate's key under another issuer name.
        let intermediate_key = KeyPair::from_pem(&pki.intermediate_key_pem).unwrap();
        let renamed = Issuer::new(
            ca_params("Somebody Else", INT_KID, BasicConstraints::Constrained(1)),
            intermediate_key,
        );
        let misnamed = ca_params(
            "workspace-ws-test",
            WS_KID,
            BasicConstraints::Constrained(0),
        )
        .signed_by(&key(), &renamed)
        .unwrap();
        rejected(
            &format!("{}{}", misnamed.pem(), pki.intermediate.pem()),
            "right key, wrong issuer name",
        );
    }

    // Two CAs that each certify the other: no unambiguous Workspace CA.
    #[test]
    fn ambiguous_relationships_are_rejected() {
        let (key_a, key_b) = (key(), key());
        let params_a = ca_params("CA A", b"kid-a", BasicConstraints::Unconstrained);
        let params_b = ca_params("CA B", b"kid-b", BasicConstraints::Unconstrained);
        let a = params_a
            .signed_by(&key_a, &Issuer::from_params(&params_b, &key_b))
            .unwrap();
        let b = params_b
            .signed_by(&key_b, &Issuer::from_params(&params_a, &key_a))
            .unwrap();
        rejected(&format!("{}{}", a.pem(), b.pem()), "mutual certification");
    }
}
