use std::collections::HashSet; // (already used by ValidCrl copied above)
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, bail, Result};
use dashmap::DashMap; // already a relay dependency
use tokio::time::sleep;
use tracing::warn;
use x509_parser::extensions::ParsedExtension;
use x509_parser::oid_registry::{
    OID_X509_EXT_AUTHORITY_KEY_IDENTIFIER, OID_X509_EXT_SUBJECT_KEY_IDENTIFIER,
};
use x509_parser::prelude::*;

/// Fail-closed 3-state revocation result. `Unavailable` is a distinct state so a
/// missing/expired CRL can never be read as "not revoked".
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RevocationStatus {
    Revoked,
    NotRevoked,
    Unavailable,
}

#[derive(Debug, Clone)]
struct ValidCrl {
    revoked: HashSet<Vec<u8>>,
    next_update: i64,
}

#[derive(Clone)]
struct WsEntry {
    issuer_ca_der: Vec<u8>, // the Workspace CA (chain[1]) that signed this CRL
    crl: ValidCrl,
}

#[derive(Clone)]
pub struct WorkspaceCrlManager {
    http_base: String,                    // e.g. cfg.controller_http_addr ("host:8080")
    cache: Arc<DashMap<String, WsEntry>>, // workspace_id -> verified CRL
}

impl WorkspaceCrlManager {
    pub fn new(http_base: String) -> Self {
        Self {
            http_base,
            cache: Arc::new(DashMap::new()),
        }
    }

    /// Fail-closed 3-state check for a workspace-CA-signed peer.
    /// `issuer_ca_der` is the Workspace CA the peer presented as chain[1]
    /// (already validated to the Intermediate during the TLS handshake).
    pub async fn check(
        &self,
        workspace_id: &str,
        serial: &[u8],
        issuer_ca_der: &[u8],
    ) -> RevocationStatus {
        let now = ASN1Time::now().timestamp();
        // fast path: fresh cache (guard dropped before any await)
        if let Some(entry) = self.cache.get(workspace_id) {
            if now < entry.crl.next_update {
                return if entry.crl.revoked.contains(serial) {
                    RevocationStatus::Revoked
                } else {
                    RevocationStatus::NotRevoked
                };
            }
        }
        // slow path: (re)fetch + verify; keep-last-good on failure
        if let Err(e) = self.refresh_one(workspace_id, issuer_ca_der).await {
            warn!(workspace_id, error = %e, "workspace CRL fetch/verify failed — fail closed");
        }
        let now = ASN1Time::now().timestamp();
        match self.cache.get(workspace_id) {
            Some(entry) if now < entry.crl.next_update => {
                if entry.crl.revoked.contains(serial) {
                    RevocationStatus::Revoked
                } else {
                    RevocationStatus::NotRevoked
                }
            }
            _ => RevocationStatus::Unavailable,
        }
    }

    async fn refresh_one(&self, workspace_id: &str, issuer_ca_der: &[u8]) -> Result<()> {
        // NOTE: match the URL scheme used by provision.rs::fetch_ca_cert (http vs https).
        let url = format!(
            "http://{}/ca.crl?workspace_id={}",
            self.http_base, workspace_id
        );
        let bytes = reqwest::get(&url)
            .await?
            .error_for_status()?
            .bytes()
            .await?;
        let (rem, crl) = parse_x509_crl(&bytes).map_err(|e| anyhow!("parse CRL: {e:?}"))?;
        if !rem.is_empty() {
            bail!("trailing data after CRL");
        }
        let validated = validate_crl(&crl, issuer_ca_der)?; // reused verbatim from connector
        self.cache.insert(
            workspace_id.to_string(),
            WsEntry {
                issuer_ca_der: issuer_ca_der.to_vec(),
                crl: validated,
            },
        );
        Ok(())
    }

    /// Background: refresh every known workspace on `interval + jitter`. Keep-last-good.
    pub fn spawn_refresh(self, interval_secs: u64, jitter_secs: u64) {
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

                let targets: Vec<(String, Vec<u8>)> = self
                    .cache
                    .iter()
                    .map(|e| (e.key().clone(), e.value().issuer_ca_der.clone()))
                    .collect();
                for (ws, issuer) in targets {
                    if let Err(e) = self.refresh_one(&ws, &issuer).await {
                        warn!(workspace_id = %ws, error = %e, "workspace CRL refresh failed — keeping last good");
                    }
                }
            }
        });
    }
}

/// Verify a DER-encoded CRL against the single Workspace CA the peer presented
/// (chain[1]). Adapted from connector/src/crl.rs: subject-match + AKI==SKI +
/// signature + date window. `issuer_ca_der` is a DER cert (not a PEM bundle).
fn validate_crl(
    crl: &CertificateRevocationList<'_>,
    issuer_ca_der: &[u8],
) -> anyhow::Result<ValidCrl> {
    let (remaining, issuer) = parse_x509_certificate(issuer_ca_der)
        .map_err(|e| anyhow!("issuer CA parse error: {:?}", e))?;
    if !remaining.is_empty() {
        bail!("trailing data after issuer CA certificate");
    }
    if crl.issuer() != issuer.subject() {
        bail!("crl issuer does not match workspace CA subject");
    }

    let crl_aki = crl
        .tbs_cert_list
        .find_extension(&OID_X509_EXT_AUTHORITY_KEY_IDENTIFIER)
        .ok_or_else(|| anyhow!("CRL has no Authority Key Identifier"))?;
    let crl_key_id = match crl_aki.parsed_extension() {
        ParsedExtension::AuthorityKeyIdentifier(aki) => {
            aki.key_identifier
                .as_ref()
                .ok_or_else(|| anyhow!("CRL AKI has no key identifier"))?
                .0
        }
        _ => bail!("CRL AKI extension is malformed"),
    };
    let issuer_ski = issuer
        .tbs_certificate
        .get_extension_unique(&OID_X509_EXT_SUBJECT_KEY_IDENTIFIER)
        .map_err(|e| anyhow!("failed to read issuer ca ski: {:?}", e))?
        .ok_or_else(|| anyhow!("issuer CA has no Subject Key Identifier"))?;
    let issuer_key_id = match issuer_ski.parsed_extension() {
        ParsedExtension::SubjectKeyIdentifier(ski) => ski.0,
        _ => bail!("issuer CA SKI extension is malformed"),
    };
    if crl_key_id != issuer_key_id {
        bail!("CRL Authority Key Identifier does not match issuer CA");
    }

    crl.verify_signature(&issuer.tbs_certificate.subject_pki)
        .map_err(|e| anyhow!("CRL signature verification failed: {:?}", e))?;

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
        bail!("CRL thisUpdate is in the future");
    }
    let next_update = next_update.ok_or_else(|| anyhow!("CRL is missing nextUpdate"))?;
    if next_update <= this_update {
        bail!("CRL nextUpdate is not after thisUpdate");
    }
    if now >= next_update {
        bail!("CRL has expired");
    }
    Ok(next_update)
}

#[cfg(test)]
mod tests {
    use super::*;
    use rcgen::{
        BasicConstraints, Certificate, CertificateParams, CertificateRevocationListParams, DnType,
        IsCa, KeyIdMethod, KeyPair, KeyUsagePurpose, RevokedCertParams, SerialNumber,
    };
    use std::sync::Once;
    use ::time::{Duration as TimeDuration, OffsetDateTime};
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    const KEY_ID: &[u8] = b"zecurity-relay-crl-test-kid";
    const WS: &str = "11111111-1111-1111-1111-111111111111";
    // High-bit-clear leading byte, so the DER INTEGER content octets equal these
    // bytes exactly (no 0x00 sign padding) — matching what the relay passes to
    // check() from a peer cert's raw_serial().
    const REVOKED_SERIAL: &[u8] = &[0x12, 0x34, 0x56];
    const FRESH_SERIAL: &[u8] = &[0x77, 0x88];

    fn install_provider() {
        static ONCE: Once = Once::new();
        ONCE.call_once(|| {
            let _ = rustls::crypto::ring::default_provider().install_default();
        });
    }

    // Self-signed Workspace CA (CertSign|CrlSign) with a pinned SKI so the CRL's
    // AKI (also pinned) matches it.
    fn workspace_ca(cn: &str) -> (KeyPair, Certificate) {
        let key = KeyPair::generate().unwrap();
        let mut params = CertificateParams::default();
        params.distinguished_name.push(DnType::CommonName, cn);
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        params.key_identifier_method = KeyIdMethod::PreSpecified(KEY_ID.to_vec());
        let cert = params.self_signed(&key).unwrap();
        (key, cert)
    }

    fn signed_crl(
        key: &KeyPair,
        ca: &Certificate,
        revoked_serial: &[u8],
        this_update: OffsetDateTime,
        next_update: OffsetDateTime,
    ) -> Vec<u8> {
        let params = CertificateRevocationListParams {
            this_update,
            next_update,
            crl_number: SerialNumber::from(1u64),
            issuing_distribution_point: None,
            revoked_certs: vec![RevokedCertParams {
                serial_number: SerialNumber::from_slice(revoked_serial),
                revocation_time: this_update,
                reason_code: None,
                invalidity_date: None,
            }],
            key_identifier_method: KeyIdMethod::PreSpecified(KEY_ID.to_vec()),
        };
        params.signed_by(ca, key).unwrap().der().as_ref().to_vec()
    }

    // Minimal one-shot HTTP/1.1 server: serves `body` with `status` on every
    // connection; returns "127.0.0.1:PORT" for use as the CRL http_base.
    async fn spawn_http(body: Vec<u8>, status: &'static str) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else {
                    break;
                };
                let body = body.clone();
                tokio::spawn(async move {
                    let mut buf = [0u8; 2048];
                    let _ = sock.read(&mut buf).await; // drain the request
                    let head = format!(
                        "HTTP/1.1 {status}\r\nContent-Type: application/pkix-crl\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                        body.len()
                    );
                    let _ = sock.write_all(head.as_bytes()).await;
                    let _ = sock.write_all(&body).await;
                    let _ = sock.flush().await;
                });
            }
        });
        addr.to_string()
    }

    // The core E2E: a real CA-signed CRL fetched over HTTP is verified and a
    // revoked serial is denied while a fresh one is allowed.
    #[tokio::test]
    async fn revoked_denied_fresh_allowed() {
        install_provider();
        let (key, ca) = workspace_ca("ws-ca-a");
        let now = OffsetDateTime::now_utc();
        let der = signed_crl(
            &key,
            &ca,
            REVOKED_SERIAL,
            now - TimeDuration::minutes(1),
            now + TimeDuration::minutes(10),
        );
        let mgr = WorkspaceCrlManager::new(spawn_http(der, "200 OK").await);
        let issuer = ca.der().as_ref().to_vec();

        assert_eq!(
            mgr.check(WS, REVOKED_SERIAL, &issuer).await,
            RevocationStatus::Revoked
        );
        assert_eq!(
            mgr.check(WS, FRESH_SERIAL, &issuer).await,
            RevocationStatus::NotRevoked
        );
    }

    // Cold cache + a failing fetch (404) must fail closed.
    #[tokio::test]
    async fn http_error_fails_closed() {
        install_provider();
        let (_key, ca) = workspace_ca("ws-ca-a");
        let mgr = WorkspaceCrlManager::new(spawn_http(b"nope".to_vec(), "404 Not Found").await);
        assert_eq!(
            mgr.check(WS, REVOKED_SERIAL, ca.der().as_ref()).await,
            RevocationStatus::Unavailable
        );
    }

    // A 200 with a non-CRL body must fail closed (parse failure, no cache write).
    #[tokio::test]
    async fn garbage_body_fails_closed() {
        install_provider();
        let (_key, ca) = workspace_ca("ws-ca-a");
        let mgr =
            WorkspaceCrlManager::new(spawn_http(b"this is not a DER CRL".to_vec(), "200 OK").await);
        assert_eq!(
            mgr.check(WS, REVOKED_SERIAL, ca.der().as_ref()).await,
            RevocationStatus::Unavailable
        );
    }

    // A validly-signed CRL verified against the WRONG CA must fail closed.
    #[tokio::test]
    async fn wrong_issuer_fails_closed() {
        install_provider();
        let (key_a, ca_a) = workspace_ca("ws-ca-a");
        let (_key_b, ca_b) = workspace_ca("ws-ca-b");
        let now = OffsetDateTime::now_utc();
        let der = signed_crl(
            &key_a,
            &ca_a,
            REVOKED_SERIAL,
            now - TimeDuration::minutes(1),
            now + TimeDuration::minutes(10),
        );
        let mgr = WorkspaceCrlManager::new(spawn_http(der, "200 OK").await);
        assert_eq!(
            mgr.check(WS, REVOKED_SERIAL, ca_b.der().as_ref()).await,
            RevocationStatus::Unavailable
        );
    }

    // A past-nextUpdate CRL must fail closed (stale = deny), not be trusted.
    #[tokio::test]
    async fn expired_crl_fails_closed() {
        install_provider();
        let (key, ca) = workspace_ca("ws-ca-a");
        let now = OffsetDateTime::now_utc();
        let der = signed_crl(
            &key,
            &ca,
            REVOKED_SERIAL,
            now - TimeDuration::minutes(20),
            now - TimeDuration::minutes(10),
        );
        let mgr = WorkspaceCrlManager::new(spawn_http(der, "200 OK").await);
        assert_eq!(
            mgr.check(WS, REVOKED_SERIAL, ca.der().as_ref()).await,
            RevocationStatus::Unavailable
        );
    }
}
