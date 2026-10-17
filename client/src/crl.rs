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

/// Starts the Relay CRL `manager` from `url`: an initial fetch unless it
/// already holds a current CRL, then periodic refresh. The CRL is verified
/// against the Platform Intermediate only (certificate audit bug 6). If the
/// device CA chain does not yield it, the manager stays empty, so relay
/// routing is refused (fail closed).
pub async fn start_relay_crl(manager: &CrlManager, url: String, device_ca_chain_pem: &[u8]) {
    let issuers = match crl_issuers(device_ca_chain_pem) {
        Ok(issuers) => issuers,
        Err(error) => {
            warn!(
                error = format!("{error:#}"),
                "device CA chain does not yield the Relay CRL issuer; relay routing is fail-closed"
            );
            return;
        }
    };
    if !manager.has_valid_cache() {
        if let Err(error) = manager.refresh(&url, &issuers.intermediate_pem).await {
            warn!(%error, "initial Relay CRL fetch failed; relay routing is fail-closed");
        }
    }
    manager
        .clone()
        .spawn_refresh(url, issuers.intermediate_pem, 60, 15);
}

/// Derives the two CRL issuers from the saved device CA chain by their actual
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
        .map_err(|e| anyhow::anyhow!("issuer CA parse error: {e:?}"))?;
    if !remaining.is_empty() {
        anyhow::bail!("trailing data after issuer CA certificate");
    }
    if crl.issuer() != issuer.subject() {
        anyhow::bail!("CRL issuer does not match expected CA");
    }

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

    /// Serves one CRL body over plain HTTP; returns its URL.
    async fn serve(body: Vec<u8>) -> String {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            loop {
                let Ok((mut stream, _)) = listener.accept().await else {
                    return;
                };
                let body = body.clone();
                tokio::spawn(async move {
                    let mut buf = vec![0u8; 4096];
                    let _ = stream.read(&mut buf).await;
                    let head = format!(
                        "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                        body.len()
                    );
                    let _ = stream.write_all(head.as_bytes()).await;
                    let _ = stream.write_all(&body).await;
                });
            }
        });
        format!("http://{addr}/relay.crl")
    }

    #[tokio::test]
    async fn startup_installs_the_genuine_relay_crl() {
        let pki = TestPki::new();
        let relay = CrlManager::new();
        let url = serve(pki.relay_crl(RELAY_SERIAL)).await;
        start_relay_crl(&relay, url, pki.device_ca_chain().as_bytes()).await;
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Revoked);
    }

    // The attacker serves the genuine Workspace CRL as /relay.crl.
    #[tokio::test]
    async fn startup_rejects_the_workspace_crl_as_relay_crl() {
        let pki = TestPki::new();
        let relay = CrlManager::new();
        let url = serve(pki.workspace_crl(&[0x01])).await;
        start_relay_crl(&relay, url, pki.device_ca_chain().as_bytes()).await;
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Unavailable);
    }

    #[tokio::test]
    async fn startup_with_an_unusable_chain_fails_closed() {
        let pki = TestPki::new();
        let relay = CrlManager::new();
        let url = serve(pki.relay_crl(RELAY_SERIAL)).await;
        start_relay_crl(&relay, url, pki.intermediate_pem.as_bytes()).await;
        assert_eq!(relay.check(RELAY_SERIAL), RevocationStatus::Unavailable);
    }

    // Each case below is caught by exactly one check, so every check is
    // exercised on its own.
    #[test]
    fn each_role_check_rejects_on_its_own() {
        use rcgen::{BasicConstraints, IsCa, KeyUsagePurpose};
        let pki = TestPki::new();
        let with_int = |ws: String| format!("{}\n{}", ws, pki.intermediate_pem);
        let (mutual_a, mutual_b) = crate::test_pki::mutually_certifying_cas();
        let (capped_int, capped_ws) =
            pki.intermediate_shaped(|p| p.is_ca = IsCa::Ca(BasicConstraints::Constrained(0)));
        for (bundle, why) in [
            (
                with_int(pki.workspace_ca_shaped(|p| {
                    p.key_usages = vec![KeyUsagePurpose::KeyCertSign];
                })),
                "Workspace CA without cRLSign",
            ),
            (
                with_int(pki.workspace_ca_shaped(|p| {
                    p.is_ca = IsCa::ExplicitNoCa;
                    p.key_usages = vec![];
                    // rcgen omits key usages on a non-CA; add keyCertSign +
                    // cRLSign directly.
                    let mut ext = rcgen::CustomExtension::from_oid_content(
                        &[2, 5, 29, 15],
                        vec![0x03, 0x02, 0x01, 0x06],
                    );
                    ext.set_criticality(true);
                    p.custom_extensions = vec![ext];
                })),
                "non-CA with CA key usages",
            ),
            (
                with_int(pki.workspace_ca_shaped(|p| {
                    p.distinguished_name = rcgen::DistinguishedName::new();
                    p.distinguished_name
                        .push(rcgen::DnType::CommonName, "Test Platform Intermediate");
                })),
                "same subject as its issuer",
            ),
            (
                with_int(pki.workspace_ca_under_issuer_name("Somebody Else")),
                "right key, wrong issuer name",
            ),
            (with_int(pki.forged_workspace_ca()), "forged signature"),
            (
                format!("{capped_ws}\n{capped_int}"),
                "Intermediate with path length 0",
            ),
            (format!("{mutual_a}\n{mutual_b}"), "mutual certification"),
        ] {
            assert!(crl_issuers(bundle.as_bytes()).is_err(), "accepted: {why}");
        }
    }

    // A CRL signed with the Intermediate's key under another issuer name.
    #[test]
    fn relay_crl_with_the_wrong_issuer_name_is_rejected() {
        let pki = TestPki::new();
        let issuers = crl_issuers(pki.device_ca_chain().as_bytes()).unwrap();
        let der = pki.relay_crl_under_issuer_name("Somebody Else", RELAY_SERIAL);
        assert!(CrlManager::new()
            .install_verified_der(&der, &issuers.intermediate_pem)
            .is_err());
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
