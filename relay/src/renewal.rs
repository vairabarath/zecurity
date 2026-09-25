//! Relay certificate renewal scheduler (Sprint 20 Phase F-2, D-19).
//!
//! Consumes the controller's `RelayService.RenewCert` (F-1). Contract:
//!   * renew with the EXISTING private key only;
//!   * verify the reply before trusting it (parses, this relay's SPIFFE ID,
//!     this relay's key, pinned Intermediate fingerprint);
//!   * persist the renewed certificate atomically BEFORE switching any
//!     consumer, then switch every consumer (via `CertManager`) and never
//!     present the old certificate again;
//!   * an already-expired relay never calls `RenewCert` — it must be replaced
//!     (D-20);
//!   * `Aborted` (an identical attempt in flight) is retryable; F-1 returns the
//!     same certificate for an identical retry.

use std::future::Future;
use std::net::IpAddr;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{bail, Context, Result};
use tokio::task::JoinHandle;
use tonic::transport::{Certificate, ClientTlsConfig, Endpoint, Identity};
use tonic::Code;
use tracing::{error, info, warn};
use x509_parser::certificate::X509Certificate;
use x509_parser::prelude::FromDer;

use crate::cert_manager::{key_spki_der, serial_hex, single_leaf, CertManager, CertMaterial};
use crate::config::RelayConfig;
use crate::csr::relay_csr_from_key;
use crate::provision::{certificate_fingerprint, controller_host};
use crate::relay::v1::relay_service_client::RelayServiceClient;
use crate::relay::v1::{RenewCertRequest, RenewCertResponse};
use crate::tls::validate_relay_certificate;

/// Renew when this fraction of the lifetime remains (same ratio as the
/// client daemon's RENEWAL_WINDOW_SECS = TTL * 2/5).
const RENEW_REMAINING_NUM: i64 = 2;
const RENEW_REMAINING_DEN: i64 = 5;
/// Jitter is ±min(15 min, 5% of lifetime) so a fleet provisioned together
/// does not renew in lock-step, while short test TTLs stay well-behaved.
const MAX_JITTER_SECS: i64 = 15 * 60;
const BACKOFF_BASE: Duration = Duration::from_secs(60);
const BACKOFF_MAX: Duration = Duration::from_secs(30 * 60);
const BACKOFF_MIN: Duration = Duration::from_secs(5);
/// An identical attempt is already in flight at the controller (F-1 lock).
const ABORTED_RETRY: Duration = Duration::from_secs(5);
const RPC_TIMEOUT: Duration = Duration::from_secs(10);
const WARN_REMAINING_SECS: i64 = 24 * 60 * 60;

/// What the scheduler should do next for a certificate.
#[derive(Debug, PartialEq, Eq)]
pub enum Plan {
    /// The certificate has expired: never call RenewCert; replace the relay.
    Expired,
    /// Attempt renewal after this delay (zero = now).
    RenewIn(Duration),
}

/// Pure scheduling decision.
pub fn plan(now: i64, not_before: i64, not_after: i64, jitter_secs: i64) -> Plan {
    if now >= not_after {
        return Plan::Expired;
    }
    let lifetime = (not_after - not_before).max(0);
    let renew_at = (not_after - lifetime * RENEW_REMAINING_NUM / RENEW_REMAINING_DEN + jitter_secs)
        .clamp(not_before, not_after - 1);
    Plan::RenewIn(Duration::from_secs((renew_at - now).max(0) as u64))
}

/// Maximum jitter magnitude for a certificate lifetime.
pub fn max_jitter_secs(lifetime_secs: i64) -> i64 {
    (lifetime_secs / 20).clamp(0, MAX_JITTER_SECS)
}

/// Uniform jitter in [-max, +max] drawn from `random`.
pub fn jitter_from(random: u64, lifetime_secs: i64) -> i64 {
    let max = max_jitter_secs(lifetime_secs);
    if max == 0 {
        return 0;
    }
    let span = (2 * max + 1) as u64;
    (random % span) as i64 - max
}

fn random_jitter(lifetime_secs: i64) -> i64 {
    let bytes = uuid::Uuid::new_v4().into_bytes();
    let random = u64::from_le_bytes(bytes[..8].try_into().expect("8 bytes"));
    jitter_from(random, lifetime_secs)
}

/// Exponential backoff for the n-th consecutive failure (n >= 1), capped at
/// 30 min and at a quarter of the remaining lifetime so retries still happen
/// before expiry.
pub fn backoff(failures: u32, remaining_secs: i64) -> Duration {
    let exp = BACKOFF_BASE.saturating_mul(1u32 << failures.saturating_sub(1).min(16));
    let by_remaining = Duration::from_secs((remaining_secs.max(0) / 4) as u64);
    exp.min(BACKOFF_MAX).min(by_remaining).max(BACKOFF_MIN)
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Controller call used by the scheduler (real gRPC or a test fake).
pub trait Renewer: Send + Sync {
    fn renew(
        &self,
        current: &CertMaterial,
        csr_der: Vec<u8>,
    ) -> impl Future<Output = std::result::Result<RenewCertResponse, tonic::Status>> + Send;
}

/// Real `RelayService.RenewCert` over mTLS, presenting the CURRENT certificate.
pub struct GrpcRenewer {
    controller_addr: String,
}

impl GrpcRenewer {
    pub fn new(controller_addr: String) -> Self {
        Self { controller_addr }
    }
}

impl Renewer for GrpcRenewer {
    async fn renew(
        &self,
        current: &CertMaterial,
        csr_der: Vec<u8>,
    ) -> std::result::Result<RenewCertResponse, tonic::Status> {
        let host = controller_host(&self.controller_addr)
            .map_err(|e| tonic::Status::invalid_argument(e.to_string()))?;
        let addr = format!("https://{}", self.controller_addr);
        let channel = Endpoint::from_shared(addr.clone())
            .map_err(|e| tonic::Status::invalid_argument(e.to_string()))?
            .tls_config(
                ClientTlsConfig::new()
                    .identity(Identity::from_pem(
                        &current.certificate_pem,
                        &current.key_pem,
                    ))
                    .ca_certificate(Certificate::from_pem(&current.intermediate_ca_pem))
                    .domain_name(host),
            )
            .map_err(|e| tonic::Status::unavailable(format!("configure renewal mTLS: {e}")))?
            .connect()
            .await
            .map_err(|e| tonic::Status::unavailable(format!("connect to {addr}: {e}")))?;
        let mut request = tonic::Request::new(RenewCertRequest { csr_der });
        request.set_timeout(RPC_TIMEOUT);
        Ok(RelayServiceClient::new(channel)
            .renew_cert(request)
            .await?
            .into_inner())
    }
}

/// Identity + request inputs for renewal (from RelayConfig).
#[derive(Clone, Debug)]
pub struct RenewalSettings {
    pub relay_id: String,
    pub pinned_ca_fingerprint: String,
    pub dns_sans: Vec<String>,
    pub ip_sans: Vec<IpAddr>,
}

impl RenewalSettings {
    pub fn from_config(cfg: &RelayConfig) -> Self {
        Self {
            relay_id: cfg.relay_id.clone(),
            pinned_ca_fingerprint: cfg.ca_fingerprint.clone(),
            dns_sans: cfg.dns_sans.clone(),
            ip_sans: cfg.ip_sans.clone(),
        }
    }
}

#[derive(Debug)]
pub enum RenewError {
    /// Current certificate already expired: RenewCert was NOT called (D-20).
    Expired,
    /// Controller says an identical attempt is in flight; retry shortly.
    Aborted,
    /// Controller refused or the call failed.
    Rpc(tonic::Status),
    /// The reply failed verification; nothing was persisted or switched.
    BadReply(anyhow::Error),
    /// Local failure (CSR build, persistence).
    Local(anyhow::Error),
}

impl std::fmt::Display for RenewError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RenewError::Expired => write!(f, "certificate expired; relay must be replaced"),
            RenewError::Aborted => write!(f, "identical renewal attempt in flight"),
            RenewError::Rpc(status) => write!(
                f,
                "RenewCert failed: {} ({:?})",
                status.message(),
                status.code()
            ),
            RenewError::BadReply(e) => write!(f, "renewal reply rejected: {e:#}"),
            RenewError::Local(e) => write!(f, "renewal failed locally: {e:#}"),
        }
    }
}

/// Verify a RenewCert reply against the current material. Returns the leaf
/// PEM to install. Nothing is persisted or switched here.
pub fn verify_reply(
    current: &CertMaterial,
    settings: &RenewalSettings,
    reply: &RenewCertResponse,
    now: i64,
) -> Result<Vec<u8>> {
    if reply.certificate_pem.is_empty() || reply.intermediate_ca_pem.is_empty() {
        bail!("controller returned incomplete certificate material");
    }
    // Parses as exactly one certificate with THIS relay's SPIFFE ID.
    let leaf = single_leaf(&reply.certificate_pem)?;
    validate_relay_certificate(&leaf, &settings.relay_id)?;
    let (_, cert) = X509Certificate::from_der(leaf.as_ref())
        .map_err(|e| anyhow::anyhow!("parse renewed certificate: {e:?}"))?;
    // Same key (D-19).
    if cert.public_key().raw != key_spki_der(&current.key_pem)?.as_slice() {
        bail!("renewed certificate is not for the relay's existing key");
    }
    // Pinned Platform Intermediate.
    let fingerprint = certificate_fingerprint(&reply.intermediate_ca_pem)
        .context("parse returned Intermediate CA")?;
    if fingerprint != settings.pinned_ca_fingerprint {
        bail!(
            "returned Intermediate CA fingerprint mismatch: expected {}, got {}",
            settings.pinned_ca_fingerprint,
            fingerprint
        );
    }
    // A real renewal: a new certificate that is still valid.
    if serial_hex(cert.raw_serial()) == current.serial_hex {
        bail!("controller returned the current certificate, not a renewal");
    }
    if cert.validity().not_after.timestamp() <= now {
        bail!("renewed certificate is already expired");
    }
    Ok(reply.certificate_pem.clone())
}

/// One renewal attempt: CSR from the existing key → RenewCert → verify →
/// persist → publish.
pub async fn renew_once<R: Renewer>(
    manager: &CertManager,
    renewer: &R,
    settings: &RenewalSettings,
) -> std::result::Result<Arc<CertMaterial>, RenewError> {
    let current = manager.current();
    let now = now_unix();
    if now >= current.not_after_unix {
        return Err(RenewError::Expired);
    }
    let csr = relay_csr_from_key(
        &settings.relay_id,
        &current.key_pem,
        &settings.dns_sans,
        &settings.ip_sans,
    )
    .map_err(RenewError::Local)?;
    let reply = renewer.renew(&current, csr).await.map_err(|status| {
        if status.code() == Code::Aborted {
            RenewError::Aborted
        } else {
            RenewError::Rpc(status)
        }
    })?;
    let certificate_pem =
        verify_reply(&current, settings, &reply, now_unix()).map_err(RenewError::BadReply)?;
    manager
        .install_renewed(&certificate_pem)
        .map_err(RenewError::Local)
}

/// Scheduler loop. Runs until the certificate has expired (then logs that the
/// relay must be replaced and stops).
pub async fn run<R: Renewer>(manager: Arc<CertManager>, renewer: R, settings: RenewalSettings) {
    let mut jitter = random_jitter(manager.current().lifetime_secs());
    let mut failures: u32 = 0;
    loop {
        let current = manager.current();
        match plan(
            now_unix(),
            current.not_before_unix,
            current.not_after_unix,
            jitter,
        ) {
            Plan::Expired => {
                error!(
                    serial = %current.serial_hex,
                    "Relay certificate has expired; RenewCert is not attempted. \
                     This relay must be replaced (D-20)"
                );
                return;
            }
            Plan::RenewIn(delay) if !delay.is_zero() => {
                info!(serial = %current.serial_hex, delay_secs = delay.as_secs(), "next Relay certificate renewal scheduled");
                tokio::time::sleep(delay).await;
                continue; // re-plan after sleeping (expiry may have passed)
            }
            Plan::RenewIn(_) => {}
        }

        match renew_once(&manager, &renewer, &settings).await {
            Ok(renewed) => {
                info!(
                    old_serial = %current.serial_hex,
                    new_serial = %renewed.serial_hex,
                    not_after_unix = renewed.not_after_unix,
                    "Relay certificate renewed and installed"
                );
                failures = 0;
                jitter = random_jitter(renewed.lifetime_secs());
            }
            Err(RenewError::Expired) => continue, // next plan() logs and stops
            Err(RenewError::Aborted) => {
                info!("identical Relay renewal in flight at controller; retrying");
                tokio::time::sleep(ABORTED_RETRY).await;
            }
            Err(err) => {
                failures = failures.saturating_add(1);
                let remaining = current.not_after_unix - now_unix();
                let delay = backoff(failures, remaining);
                if remaining < WARN_REMAINING_SECS {
                    error!(error = %err, remaining_secs = remaining, retry_secs = delay.as_secs(), "Relay certificate renewal failing close to expiry");
                } else {
                    warn!(error = %err, retry_secs = delay.as_secs(), "Relay certificate renewal failed; will retry");
                }
                tokio::time::sleep(delay).await;
            }
        }
    }
}

/// Start the single renewal scheduler for this manager.
pub fn spawn(manager: Arc<CertManager>, cfg: &RelayConfig) -> Result<JoinHandle<()>> {
    if !manager.claim_scheduler() {
        bail!("a Relay renewal scheduler is already running");
    }
    let renewer = GrpcRenewer::new(cfg.controller_addr.clone());
    let settings = RenewalSettings::from_config(cfg);
    Ok(tokio::spawn(run(manager, renewer, settings)))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::cert_manager::CertPaths;
    use crate::test_support::{TestPki, RELAY_ID};
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::sync::Mutex;

    // ---- scheduling ---------------------------------------------------------

    #[test]
    fn plan_renews_when_two_fifths_of_lifetime_remain() {
        // 30-day cert issued at t=0: renew at day 18 (12 days remaining).
        let day = 86_400;
        let (nb, na) = (0, 30 * day);
        assert_eq!(
            plan(0, nb, na, 0),
            Plan::RenewIn(Duration::from_secs(18 * day as u64))
        );
        assert_eq!(
            plan(10 * day, nb, na, 0),
            Plan::RenewIn(Duration::from_secs(8 * day as u64))
        );
        // Inside the window: renew now.
        assert_eq!(plan(20 * day, nb, na, 0), Plan::RenewIn(Duration::ZERO));
    }

    #[test]
    fn plan_applies_jitter_and_stays_inside_validity() {
        let (nb, na) = (0, 1000);
        assert_eq!(plan(0, nb, na, 25), Plan::RenewIn(Duration::from_secs(625)));
        assert_eq!(
            plan(0, nb, na, -25),
            Plan::RenewIn(Duration::from_secs(575))
        );
        // Huge jitter is clamped into [not_before, not_after).
        assert_eq!(
            plan(0, nb, na, 10_000),
            Plan::RenewIn(Duration::from_secs(999))
        );
        assert_eq!(plan(0, nb, na, -10_000), Plan::RenewIn(Duration::ZERO));
    }

    #[test]
    fn plan_expired_certificate_never_renews() {
        assert_eq!(plan(1000, 0, 1000, 0), Plan::Expired);
        assert_eq!(plan(5000, 0, 1000, 0), Plan::Expired);
    }

    #[test]
    fn jitter_is_bounded_by_lifetime_and_fifteen_minutes() {
        assert_eq!(max_jitter_secs(900), 45); // 15-min dev cert → ±45 s
        assert_eq!(max_jitter_secs(30 * 86_400), 15 * 60); // 30 d → ±15 min
        for random in [0u64, 1, 44, 45, 90, 91, u64::MAX, 123_456_789] {
            let j = jitter_from(random, 900);
            assert!((-45..=45).contains(&j), "jitter {j} out of bounds");
        }
        // Both extremes are reachable.
        assert_eq!(jitter_from(0, 900), -45);
        assert_eq!(jitter_from(90, 900), 45);
    }

    #[test]
    fn backoff_grows_exponentially_and_respects_caps() {
        let far = 30 * 86_400;
        assert_eq!(backoff(1, far), Duration::from_secs(60));
        assert_eq!(backoff(2, far), Duration::from_secs(120));
        assert_eq!(backoff(3, far), Duration::from_secs(240));
        assert_eq!(backoff(20, far), Duration::from_secs(30 * 60));
        // Close to expiry: never wait longer than a quarter of what remains.
        assert_eq!(backoff(20, 400), Duration::from_secs(100));
        assert_eq!(backoff(20, 0), BACKOFF_MIN);
    }

    // ---- renew_once ---------------------------------------------------------

    struct FakeRenewer {
        reply: Mutex<Option<std::result::Result<RenewCertResponse, tonic::Status>>>,
        calls: AtomicUsize,
        presented: Mutex<Vec<String>>,
    }

    impl FakeRenewer {
        fn new(reply: std::result::Result<RenewCertResponse, tonic::Status>) -> Self {
            Self {
                reply: Mutex::new(Some(reply)),
                calls: AtomicUsize::new(0),
                presented: Mutex::new(vec![]),
            }
        }
    }

    impl Renewer for FakeRenewer {
        async fn renew(
            &self,
            current: &CertMaterial,
            _csr_der: Vec<u8>,
        ) -> std::result::Result<RenewCertResponse, tonic::Status> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            self.presented
                .lock()
                .unwrap()
                .push(current.serial_hex.clone());
            self.reply
                .lock()
                .unwrap()
                .take()
                .expect("unexpected extra RenewCert call")
        }
    }

    struct Fixture {
        pki: TestPki,
        key: rcgen::KeyPair,
        dir: std::path::PathBuf,
        manager: Arc<CertManager>,
        settings: RenewalSettings,
        original_pem: String,
    }

    fn fixture(not_before_offset: i64, lifetime: i64) -> Fixture {
        let pki = TestPki::new();
        let key = pki.relay_key();
        let dir =
            std::env::temp_dir().join(format!("relay-renew-{}", uuid::Uuid::new_v4().simple()));
        std::fs::create_dir_all(&dir).unwrap();
        let original_pem = pki.relay_cert(RELAY_ID, &key, not_before_offset, lifetime);
        pki.write_state(&dir, &key, &original_pem);
        let manager = CertManager::load(
            RELAY_ID,
            CertPaths {
                key_path: dir.join("relay.key"),
                certificate_path: dir.join("relay.crt"),
                intermediate_ca_path: dir.join("intermediate-ca.crt"),
            },
        )
        .unwrap();
        let settings = RenewalSettings {
            relay_id: RELAY_ID.to_owned(),
            pinned_ca_fingerprint: certificate_fingerprint(pki.ca_pem.as_bytes()).unwrap(),
            dns_sans: vec![],
            ip_sans: vec![],
        };
        Fixture {
            pki,
            key,
            dir,
            manager,
            settings,
            original_pem,
        }
    }

    impl Fixture {
        fn reply(&self, certificate_pem: String) -> RenewCertResponse {
            RenewCertResponse {
                certificate_pem: certificate_pem.into_bytes(),
                intermediate_ca_pem: self.pki.ca_pem.clone().into_bytes(),
                cert_not_after_unix: 0,
                cert_not_before_unix: 0,
            }
        }

        fn assert_unchanged(&self) {
            assert_eq!(
                std::fs::read(self.dir.join("relay.crt")).unwrap(),
                self.original_pem.as_bytes()
            );
            assert_eq!(
                self.manager.current().certificate_pem,
                self.original_pem.as_bytes()
            );
        }
    }

    #[tokio::test]
    async fn renew_once_installs_a_verified_same_key_certificate() {
        let f = fixture(0, 3600);
        let renewed = f.pki.relay_cert(RELAY_ID, &f.key, 0, 7200);
        let renewer = FakeRenewer::new(Ok(f.reply(renewed.clone())));
        let before = f.manager.current().serial_hex.clone();

        let installed = renew_once(&f.manager, &renewer, &f.settings).await.unwrap();

        assert_ne!(installed.serial_hex, before);
        assert_eq!(
            renewer.presented.lock().unwrap().as_slice(),
            &[before],
            "must present the current certificate"
        );
        assert_eq!(
            std::fs::read(f.dir.join("relay.crt")).unwrap(),
            renewed.as_bytes()
        );
        assert_eq!(f.manager.current().serial_hex, installed.serial_hex);
    }

    #[tokio::test]
    async fn already_expired_certificate_never_calls_renewcert() {
        let f = fixture(-7200, 3600); // expired an hour ago
        let renewer = FakeRenewer::new(Err(tonic::Status::internal("must not be called")));

        let err = renew_once(&f.manager, &renewer, &f.settings)
            .await
            .unwrap_err();

        assert!(matches!(err, RenewError::Expired), "got {err}");
        assert_eq!(
            renewer.calls.load(Ordering::SeqCst),
            0,
            "RenewCert must not be called (D-20)"
        );
        f.assert_unchanged();
    }

    #[tokio::test]
    async fn expired_scheduler_run_stops_without_calling_renewcert() {
        let f = fixture(-7200, 3600);
        let renewer = FakeRenewer::new(Err(tonic::Status::internal("must not be called")));
        tokio::time::timeout(
            Duration::from_secs(5),
            run(f.manager.clone(), renewer, f.settings.clone()),
        )
        .await
        .expect("scheduler must stop for an expired certificate");
    }

    #[tokio::test]
    async fn aborted_is_reported_as_retryable() {
        let f = fixture(0, 3600);
        let renewer = FakeRenewer::new(Err(tonic::Status::aborted(
            "renewal already in progress; retry",
        )));
        let err = renew_once(&f.manager, &renewer, &f.settings)
            .await
            .unwrap_err();
        assert!(matches!(err, RenewError::Aborted), "got {err}");
        f.assert_unchanged();
    }

    #[tokio::test]
    async fn bad_replies_are_rejected_and_nothing_changes() {
        // Wrong SPIFFE ID.
        let f = fixture(0, 3600);
        let wrong_id = f
            .pki
            .relay_cert("9b2d5cae-5820-4702-adf4-231680852b11", &f.key, 0, 7200);
        let err = renew_once(
            &f.manager,
            &FakeRenewer::new(Ok(f.reply(wrong_id))),
            &f.settings,
        )
        .await
        .unwrap_err();
        assert!(
            matches!(err, RenewError::BadReply(_)),
            "wrong SPIFFE: got {err}"
        );
        f.assert_unchanged();

        // Different key.
        let other_key = f.pki.relay_key();
        let foreign = f.pki.relay_cert(RELAY_ID, &other_key, 0, 7200);
        let err = renew_once(
            &f.manager,
            &FakeRenewer::new(Ok(f.reply(foreign))),
            &f.settings,
        )
        .await
        .unwrap_err();
        assert!(
            matches!(err, RenewError::BadReply(_)),
            "wrong key: got {err}"
        );
        f.assert_unchanged();

        // Intermediate that does not match the pinned fingerprint.
        let rogue = TestPki::new();
        let mut reply = f.reply(f.pki.relay_cert(RELAY_ID, &f.key, 0, 7200));
        reply.intermediate_ca_pem = rogue.ca_pem.clone().into_bytes();
        let err = renew_once(&f.manager, &FakeRenewer::new(Ok(reply)), &f.settings)
            .await
            .unwrap_err();
        assert!(
            matches!(err, RenewError::BadReply(_)),
            "rogue intermediate: got {err}"
        );
        f.assert_unchanged();

        // Garbage certificate PEM.
        let err = renew_once(
            &f.manager,
            &FakeRenewer::new(Ok(f.reply("not a certificate".into()))),
            &f.settings,
        )
        .await
        .unwrap_err();
        assert!(matches!(err, RenewError::BadReply(_)), "garbage: got {err}");
        f.assert_unchanged();

        // The current certificate echoed back (not a renewal).
        let err = renew_once(
            &f.manager,
            &FakeRenewer::new(Ok(f.reply(f.original_pem.clone()))),
            &f.settings,
        )
        .await
        .unwrap_err();
        assert!(matches!(err, RenewError::BadReply(_)), "echo: got {err}");
        f.assert_unchanged();
    }

    #[tokio::test]
    async fn scheduler_renews_in_window_and_rejects_second_instance() {
        // 100 s cert issued 80 s ago: 20 s remain < 2/5 → renew immediately.
        let f = fixture(-80, 100);
        let renewed = f.pki.relay_cert(RELAY_ID, &f.key, 0, 3600);
        let renewer = FakeRenewer::new(Ok(f.reply(renewed)));
        let mut rx = f.manager.subscribe();
        assert!(f.manager.claim_scheduler());

        let task = tokio::spawn(run(f.manager.clone(), renewer, f.settings.clone()));
        tokio::time::timeout(Duration::from_secs(10), rx.changed())
            .await
            .expect("scheduler must renew promptly inside the window")
            .unwrap();
        task.abort();

        // A second scheduler cannot be started for the same manager.
        assert!(!f.manager.claim_scheduler());
    }
}
