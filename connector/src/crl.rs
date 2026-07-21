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
}

/// Caches revoked certificate serials fetched from the controller's /ca.crl endpoint.
///
/// The controller serves `GET /ca.crl?workspace_id=<uuid>` → DER-encoded CRL signed
/// by the workspace CA. Connector fetches on startup then refreshes every 5 minutes.
///
/// M4 calls `is_revoked(serial_bytes)` inside `device_tunnel::handle_stream` after
/// extracting the peer cert serial from the TLS handshake.
#[derive(Clone, Debug, Default)]
pub struct CrlManager {
    cache: Arc<RwLock<Option<ValidCrl>>>,
}

impl CrlManager {
    pub fn new() -> Self {
        Self::default()
    }

    /// Returns true if `serial` (raw big-endian bytes from the peer cert) is revoked.
    pub fn is_revoked(&self, serial: &[u8]) -> bool {
        let now = ASN1Time::now().timestamp();
        self.cache
            .read()
            .as_ref()
            .filter(|cache| now < cache.next_update)
            .is_some_and(|cache| cache.revoked.contains(serial))
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
        let validated = validate_crl(&crl, issuer_ca_pem)?;
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

    pub fn has_valid_cache(&self) -> bool {
        let now = ASN1Time::now().timestamp();

        self.cache
            .read()
            .as_ref()
            .is_some_and(|cache| now < cache.next_update)
    }

    #[cfg(test)]
    pub(crate) fn install_test_cache(&self, serials: Vec<Vec<u8>>) {
        *self.cache.write() = Some(ValidCrl {
            revoked: serials.into_iter().collect(),
            next_update: ASN1Time::now().timestamp() + 60,
        });
    }
}

fn validate_crl(
    crl: &CertificateRevocationList<'_>,
    issuer_ca_pem: &[u8],
) -> anyhow::Result<ValidCrl> {
    let mut issuer_reader = issuer_ca_pem;
    let issuer_ders = rustls_pemfile::certs(&mut issuer_reader)
        .collect::<Result<Vec<_>, _>>()
        .map_err(|e| anyhow::anyhow!("failed to parse issuer_ca_pem {e}"))?;
    let mut issuer = None;
    for issuer_der in &issuer_ders {
        let (remaining, candidate) = parse_x509_certificate(issuer_der.as_ref())
            .map_err(|e| anyhow::anyhow!("issuer CA parse error: {:?}", e))?;
        if !remaining.is_empty() {
            anyhow::bail!("trailing data after issuer CA certificate");
        }
        if crl.issuer() == candidate.subject() {
            issuer = Some(candidate);
            break;
        }
    }
    let issuer =
        issuer.ok_or_else(|| anyhow::anyhow!("crl issuer does not match expected ca subject"))?;
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
        });

        assert!(!manager.has_valid_cache());
        assert!(!manager.is_revoked(REVOKED_SERIAL_BYTES));
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
        assert!(manager.is_revoked(REVOKED_SERIAL_BYTES));
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
        assert!(manager.is_revoked(REVOKED_SERIAL_BYTES));
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
}
