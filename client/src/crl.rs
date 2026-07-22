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
    if this_update > now || next_update <= this_update || now >= next_update {
        anyhow::bail!("CRL validity window is not current");
    }

    Ok(ValidCrl {
        revoked: crl
            .iter_revoked_certificates()
            .map(|entry| entry.raw_serial().to_vec())
            .collect(),
        next_update,
    })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn cold_boot_has_no_valid_cache() {
        assert!(!CrlManager::new().has_valid_cache());
    }
}
