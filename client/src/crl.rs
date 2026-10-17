use parking_lot::RwLock;
use std::collections::HashSet;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};
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



/// Result of a revocation lookup. `Unavailable` is a distinct state so callers
/// cannot mistake "no valid CRL" for "not revoked" (fail-closed by construction).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RevocationStatus {
    Revoked,
    NotRevoked,
    Unavailable,
}
#[derive(Clone, Debug, Default)]
pub struct CrlManager {
    cache: Arc<RwLock<Option<ValidCrl>>>,
    refresh_started: Arc<AtomicBool>,
}

impl CrlManager {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn has_valid_cache(&self) -> bool {
        let now = ASN1Time::now().timestamp();
        self.cache
            .read()
            .as_ref()
            .is_some_and(|cache| now < cache.next_update)
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

    pub async fn refresh(&self, url: &str, issuer_ca_pem: &[u8]) -> anyhow::Result<()> {
        let bytes = reqwest::get(url).await?.error_for_status()?.bytes().await?;
        self.install_verified_der(&bytes, issuer_ca_pem)
    }

    /// Validate a DER-encoded CRL and atomically install it as the new cache.
    /// Invalid input never replaces a previously verified cache.
    pub fn install_verified_der(&self, bytes: &[u8], issuer_ca_pem: &[u8]) -> anyhow::Result<()> {
        let (remaining, crl) =
            parse_x509_crl(bytes).map_err(|e| anyhow::anyhow!("CRL parse error: {e:?}"))?;
        if !remaining.is_empty() {
            anyhow::bail!("trailing data after CRL");
        }
        let validated = validate_crl(&crl, issuer_ca_pem)?;
        *self.cache.write() = Some(validated);
        Ok(())
    }

    pub fn spawn_refresh(
        self,
        url: String,
        issuer_ca_pem: Vec<u8>,
        interval_secs: u64,
        jitter_secs: u64,
    ) {
        if self.refresh_started.swap(true, Ordering::AcqRel) {
            return;
        }
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
                tokio::time::sleep(Duration::from_secs(interval_secs + jitter)).await;
                if let Err(error) = self.refresh(&url, &issuer_ca_pem).await {
                    warn!(%error, "Relay CRL refresh failed; keeping last-good CRL");
                }
            }
        });
    }

    #[cfg(test)]
    pub(crate) fn install_test_cache(&self, serials: Vec<Vec<u8>>) {
        *self.cache.write() = Some(ValidCrl {
            revoked: serials.into_iter().collect(),
            next_update: ASN1Time::now().timestamp() + 60,
        });
    }
}

/// The issuer each CRL is checked against, derived from the saved device CA
/// chain (Workspace CA + Platform Intermediate). Certificate audit bug 6: the
/// two CRLs are signed by different CAs, and each must be accepted only from
/// its own signer.
#[derive(Clone, Debug)]
pub struct CrlIssuers {
    /// Signs `/ca.crl` (revoked devices and connectors).
    #[allow(dead_code)] // the client checks only the Relay CRL today
    pub workspace_ca_pem: Vec<u8>,
    /// Signs `/relay.crl` (revoked relays).
    pub intermediate_pem: Vec<u8>,
}

pub fn crl_issuers(bundle_pem: &[u8]) -> anyhow::Result<CrlIssuers> {
    Ok(CrlIssuers {
        workspace_ca_pem: bundle_pem.to_vec(),
        intermediate_pem: bundle_pem.to_vec(),
    })
}

fn validate_crl(
    crl: &CertificateRevocationList<'_>,
    issuer_ca_pem: &[u8],
) -> anyhow::Result<ValidCrl> {
    let mut issuer_reader = issuer_ca_pem;
    let issuer_ders =
        rustls_pemfile::certs(&mut issuer_reader).collect::<Result<Vec<_>, _>>()?;
    let mut issuer = None;
    for der in &issuer_ders {
        let (_, candidate) = parse_x509_certificate(der.as_ref())
            .map_err(|e| anyhow::anyhow!("issuer CA parse error: {e:?}"))?;
        if crl.issuer() == candidate.subject() {
            issuer = Some(candidate);
            break;
        }
    }
    let issuer = issuer.ok_or_else(|| anyhow::anyhow!("CRL issuer does not match expected CA"))?;

    let aki = crl
        .tbs_cert_list
        .find_extension(&OID_X509_EXT_AUTHORITY_KEY_IDENTIFIER)
        .ok_or_else(|| anyhow::anyhow!("CRL has no Authority Key Identifier"))?;
    let aki = match aki.parsed_extension() {
        ParsedExtension::AuthorityKeyIdentifier(aki) => {
            aki.key_identifier
                .as_ref()
                .ok_or_else(|| anyhow::anyhow!("CRL AKI has no key identifier"))?
                .0
        }
        _ => anyhow::bail!("CRL AKI is malformed"),
    };
    let ski = issuer
        .tbs_certificate
        .get_extension_unique(&OID_X509_EXT_SUBJECT_KEY_IDENTIFIER)
        .map_err(|e| anyhow::anyhow!("issuer SKI error: {e:?}"))?
        .ok_or_else(|| anyhow::anyhow!("issuer has no SKI"))?;
    let ski = match ski.parsed_extension() {
        ParsedExtension::SubjectKeyIdentifier(ski) => ski.0,
        _ => anyhow::bail!("issuer SKI is malformed"),
    };
    if aki != ski {
        anyhow::bail!("CRL AKI does not match issuer SKI");
    }
    crl.verify_signature(&issuer.tbs_certificate.subject_pki)
        .map_err(|e| anyhow::anyhow!("CRL signature verification failed: {e:?}"))?;

    let now = ASN1Time::now().timestamp();
    let this_update = crl.last_update().timestamp();
    let next_update = crl
        .next_update()
        .ok_or_else(|| anyhow::anyhow!("CRL is missing nextUpdate"))?
        .timestamp();
    check_update_times(this_update, next_update, now)?;

    Ok(ValidCrl {
        revoked: crl
            .iter_revoked_certificates()
            .map(|entry| entry.raw_serial().to_vec())
            .collect(),
        next_update,
    })
}

/// The CRL must be current: thisUpdate <= now < nextUpdate, with nextUpdate
/// after thisUpdate. No clock-skew allowance here; the controller backdates
/// thisUpdate by 5 min instead (Sprint 21 B4).
fn check_update_times(this_update: i64, next_update: i64, now: i64) -> anyhow::Result<()> {
    if this_update > now || next_update <= this_update || now >= next_update {
        anyhow::bail!("CRL validity window is not current");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    /// A CA and a CRL it signs for the fixed window [this_update, next_update]
    /// (calendar dates, so the result does not depend on the test clock).
    fn signed_crl(this_update: (i32, u8, u8), next_update: (i32, u8, u8)) -> (Vec<u8>, String) {
        use rcgen::{
            date_time_ymd, BasicConstraints, CertificateParams, CertificateRevocationListParams,
            IsCa, KeyIdMethod, KeyPair, KeyUsagePurpose, SerialNumber,
        };
        const KEY_ID: &[u8] = b"client-crl-test-kid";
        let key = KeyPair::generate().unwrap();
        let mut ca = CertificateParams::default();
        ca.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        ca.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        ca.key_identifier_method = KeyIdMethod::PreSpecified(KEY_ID.to_vec());
        let ca = ca.self_signed(&key).unwrap();
        let crl = CertificateRevocationListParams {
            this_update: date_time_ymd(this_update.0, this_update.1, this_update.2),
            next_update: date_time_ymd(next_update.0, next_update.1, next_update.2),
            crl_number: SerialNumber::from(1u64),
            issuing_distribution_point: None,
            revoked_certs: vec![],
            key_identifier_method: KeyIdMethod::PreSpecified(KEY_ID.to_vec()),
        }
        .signed_by(&ca, &key)
        .unwrap();
        (crl.der().to_vec(), ca.pem())
    }

    fn validate(der: &[u8], ca_pem: &str) -> anyhow::Result<ValidCrl> {
        let (_, crl) = parse_x509_crl(der).unwrap();
        validate_crl(&crl, ca_pem.as_bytes())
    }

    /// validate_crl enforces the time window on real signed CRLs: current is
    /// accepted, expired and future-dated are rejected.
    #[test]
    fn validate_crl_enforces_the_update_window() {
        let (der, ca) = signed_crl((2000, 1, 1), (2999, 1, 1));
        assert!(validate(&der, &ca).is_ok(), "current CRL must be accepted");
        let (der, ca) = signed_crl((2000, 1, 1), (2001, 1, 1));
        assert!(validate(&der, &ca).is_err(), "expired CRL must be rejected");
        let (der, ca) = signed_crl((2998, 1, 1), (2999, 1, 1));
        assert!(
            validate(&der, &ca).is_err(),
            "future-dated CRL must be rejected"
        );
    }

    #[test]
    fn cold_boot_has_no_valid_cache() {
        assert!(!CrlManager::new().has_valid_cache());
    }

    // ---- Sprint 21 B4: CRLs as the controller now signs them ----------------

    /// The controller's CRL window: thisUpdate = signing time − 5 min,
    /// nextUpdate = signing time + validity (controller/internal/pki crlWindow).
    const CONTROLLER_BACKDATE_SECS: i64 = 5 * 60;
    const CONSUMER_NOW: i64 = 1_791_441_726;

    fn controller_crl(signed_at: i64, validity: i64) -> (i64, i64) {
        (signed_at - CONTROLLER_BACKDATE_SECS, signed_at + validity)
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
            let (this_update, next_update) = controller_crl(CONSUMER_NOW + ahead, 600);
            assert!(
                check_update_times(this_update, next_update, CONSUMER_NOW).is_ok(),
                "controller {ahead}s ahead"
            );
        }
    }

    #[test]
    fn crls_outside_the_tolerance_or_window_are_still_rejected() {
        let (this_update, next_update) =
            controller_crl(CONSUMER_NOW + CONTROLLER_BACKDATE_SECS + 1, 600);
        assert!(check_update_times(this_update, next_update, CONSUMER_NOW).is_err());
        let (this_update, next_update) = controller_crl(CONSUMER_NOW - 600, 600);
        assert!(check_update_times(this_update, next_update, CONSUMER_NOW).is_err());
        let (this_update, next_update) = controller_crl(CONSUMER_NOW - 86_400, 600);
        assert!(check_update_times(this_update, next_update, CONSUMER_NOW).is_err());
        assert!(check_update_times(CONSUMER_NOW - 300, CONSUMER_NOW - 300, CONSUMER_NOW).is_err());
    }
}

/// Certificate audit bug 6: the client checked the Relay CRL against the
/// whole device CA chain (Workspace CA + Intermediate), so the Workspace CRL
/// (signed by the Workspace CA) served as `/relay.crl` was accepted, and
/// revoked relays passed.
#[cfg(test)]
mod issuer_binding_tests {
    use super::*;
    use crate::test_pki::TestPki;

    const RELAY_SERIAL: &[u8] = &[0x7e, 0x1a];

    #[test]
    fn genuine_relay_crl_validates_against_the_intermediate() {
        let pki = TestPki::new();
        let issuers = crl_issuers(pki.device_ca_chain().as_bytes()).unwrap();
        let relay = CrlManager::new();
        relay
            .install_verified_der(&pki.relay_crl(RELAY_SERIAL), &issuers.intermediate_pem)
            .unwrap();
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Revoked);
    }

    #[test]
    fn workspace_crl_substituted_for_relay_crl_is_rejected() {
        let pki = TestPki::new();
        let issuers = crl_issuers(pki.device_ca_chain().as_bytes()).unwrap();
        let relay = CrlManager::new();
        assert!(
            relay
                .install_verified_der(&pki.workspace_crl(&[0x01]), &issuers.intermediate_pem)
                .is_err(),
            "the Workspace CRL was accepted as the Relay CRL"
        );
        // Rejected, so still Unavailable (deny), never NotRevoked.
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Unavailable);

        // And it never replaces a genuine Relay CRL.
        relay
            .install_verified_der(&pki.relay_crl(RELAY_SERIAL), &issuers.intermediate_pem)
            .unwrap();
        assert!(relay
            .install_verified_der(&pki.workspace_crl(&[0x01]), &issuers.intermediate_pem)
            .is_err());
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Revoked);
    }

    #[test]
    fn the_device_ca_chain_is_not_accepted_as_a_crl_issuer() {
        let pki = TestPki::new();
        for der in [pki.workspace_crl(&[0x01]), pki.relay_crl(RELAY_SERIAL)] {
            assert!(CrlManager::new()
                .install_verified_der(&der, pki.device_ca_chain().as_bytes())
                .is_err());
        }
    }

    #[test]
    fn roles_follow_the_chain_not_the_order() {
        let pki = TestPki::new();
        let reversed = format!("{}\n{}", pki.intermediate_pem, pki.workspace_ca_pem);
        let issuers = crl_issuers(reversed.as_bytes()).unwrap();
        assert_eq!(issuers.intermediate_pem, pki.intermediate_pem.as_bytes());
        assert_eq!(issuers.workspace_ca_pem, pki.workspace_ca_pem.as_bytes());
    }

    #[test]
    fn malformed_or_invalid_chains_are_rejected() {
        let pki = TestPki::new();
        let (expired_int, expired_ws) = pki.expired_cas();
        let (_, csr) = crate::test_pki::device_key_and_csr();
        let (non_ca_ws, _) = pki.non_ca_workspace(&csr, crate::test_pki::DEVICE_ID);
        for (bundle, why) in [
            (String::new(), "empty"),
            ("garbage".to_string(), "garbage"),
            (pki.intermediate_pem.clone(), "Intermediate only"),
            (pki.workspace_ca_pem.clone(), "Workspace CA only"),
            (
                format!("{}\n{}", pki.device_ca_chain(), pki.root_pem),
                "extra certificate",
            ),
            (
                format!("{}\n{}", pki.workspace_ca_pem, pki.workspace_ca_pem),
                "duplicate",
            ),
            (
                format!("{}\n{}", pki.intermediate_pem, pki.root_pem),
                "root in the Intermediate role",
            ),
            (
                format!("{}\n{}", pki.workspace_ca_pem, pki.root_pem),
                "no issuing relationship",
            ),
            (
                format!("{}\n{}", expired_ws, pki.intermediate_pem),
                "expired Workspace CA",
            ),
            (
                format!("{}\n{}", pki.workspace_ca_pem, expired_int),
                "expired Intermediate (and not the Workspace CA's issuer key)",
            ),
            (
                format!("{}\n{}", non_ca_ws, pki.intermediate_pem),
                "non-CA Workspace certificate",
            ),
        ] {
            assert!(crl_issuers(bundle.as_bytes()).is_err(), "accepted: {why}");
        }
    }
}
