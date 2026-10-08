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
use std::time::Duration;

use anyhow::{bail, Context, Result};
use tokio::task::JoinHandle;
use tonic::transport::{Certificate, ClientTlsConfig, Endpoint, Identity};
use tonic::Code;
use tracing::{error, info, warn};
use x509_parser::certificate::X509Certificate;
use x509_parser::prelude::FromDer;

use crate::cert_manager::{
    key_spki_der, now_unix, serial_hex, single_leaf, CertManager, CertMaterial,
};
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

/// Where the certificate's lifetime starts for scheduling (Sprint 21 K1,
/// KI-1): the later of `NotBefore` and the time this relay installed it. The
/// controller backdates relay `NotBefore` by an hour for clock skew; measured
/// from `NotBefore`, a certificate with a TTL of 40 minutes or less was
/// already inside its renewal window when installed, so the relay renewed in
/// a loop. Same principle as the controller rotator's `issuedAt` (Sprint 20
/// PF-1).
///
/// Without an install time (`None`: a relay provisioned before K1, or no
/// usable record) this is `NotBefore`, i.e. the pre-K1 schedule. An install
/// time outside `[not_before, not_after)` is ignored the same way.
pub fn schedule_start(not_before: i64, not_after: i64, installed_at: Option<i64>) -> i64 {
    match installed_at {
        Some(at) if at < not_after => at.max(not_before),
        _ => not_before,
    }
}

/// Pure scheduling decision: renew when 2/5 of the lifetime measured from
/// `schedule_start` remains (± jitter). Expired certificates are never
/// renewed (D-20).
pub fn plan(
    now: i64,
    not_before: i64,
    not_after: i64,
    installed_at: Option<i64>,
    jitter_secs: i64,
) -> Plan {
    if now >= not_after {
        return Plan::Expired;
    }
    let start = schedule_start(not_before, not_after, installed_at);
    let lifetime = (not_after - start).max(0);
    let renew_at = (not_after - lifetime * RENEW_REMAINING_NUM / RENEW_REMAINING_DEN + jitter_secs)
        .clamp(start, not_after - 1);
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
    let mut jitter = random_jitter(manager.current().schedule_lifetime_secs());
    let mut failures: u32 = 0;
    loop {
        let current = manager.current();
        match plan(
            now_unix(),
            current.not_before_unix,
            current.not_after_unix,
            current.installed_at_unix,
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
                jitter = random_jitter(renewed.schedule_lifetime_secs());
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
            plan(0, nb, na, None, 0),
            Plan::RenewIn(Duration::from_secs(18 * day as u64))
        );
        assert_eq!(
            plan(10 * day, nb, na, None, 0),
            Plan::RenewIn(Duration::from_secs(8 * day as u64))
        );
        // Inside the window: renew now.
        assert_eq!(
            plan(20 * day, nb, na, None, 0),
            Plan::RenewIn(Duration::ZERO)
        );
    }

    #[test]
    fn plan_applies_jitter_and_stays_inside_validity() {
        let (nb, na) = (0, 1000);
        assert_eq!(
            plan(0, nb, na, None, 25),
            Plan::RenewIn(Duration::from_secs(625))
        );
        assert_eq!(
            plan(0, nb, na, None, -25),
            Plan::RenewIn(Duration::from_secs(575))
        );
        // Huge jitter is clamped into [not_before, not_after).
        assert_eq!(
            plan(0, nb, na, None, 10_000),
            Plan::RenewIn(Duration::from_secs(999))
        );
        assert_eq!(
            plan(0, nb, na, None, -10_000),
            Plan::RenewIn(Duration::ZERO)
        );
    }

    #[test]
    fn plan_expired_certificate_never_renews() {
        assert_eq!(plan(1000, 0, 1000, None, 0), Plan::Expired);
        assert_eq!(plan(5000, 0, 1000, None, 0), Plan::Expired);
    }

    // ---- K1: lifetime measured from install, not the backdated NotBefore -----

    /// The controller backdates relay NotBefore by an hour
    /// (controller/internal/pki/relay.go).
    const BACKDATE: i64 = 3600;
    const T: i64 = 1_791_441_726; // an arbitrary "now" (install time)

    /// Delay plan() gives a cert installed at T with `ttl`, NotBefore = T - 1h.
    fn delay_after_install(ttl: i64, installed_at: Option<i64>, jitter: i64) -> Plan {
        plan(T, T - BACKDATE, T + ttl, installed_at, jitter)
    }

    fn secs(s: i64) -> Plan {
        Plan::RenewIn(Duration::from_secs(s as u64))
    }

    #[test]
    fn schedule_start_is_later_of_not_before_and_install() {
        assert_eq!(schedule_start(100, 1000, None), 100);
        assert_eq!(schedule_start(100, 1000, Some(400)), 400);
        // Earlier than NotBefore: NotBefore wins (max, not min).
        assert_eq!(schedule_start(100, 1000, Some(50)), 100);
        // At or after NotAfter is not a usable install time: NotBefore.
        assert_eq!(schedule_start(100, 1000, Some(1000)), 100);
        assert_eq!(schedule_start(100, 1000, Some(5000)), 100);
    }

    #[test]
    fn backdated_15m_cert_renews_9m_after_install_not_immediately() {
        // 15 min measured from install: renew when 6 min (2/5) remain, i.e.
        // 9 min after install. Jitter for a 15-min lifetime is ±45 s.
        assert_eq!(delay_after_install(900, Some(T), 0), secs(540));
        let j = max_jitter_secs(900);
        assert_eq!(j, 45);
        assert_eq!(delay_after_install(900, Some(T), -j), secs(495));
        assert_eq!(delay_after_install(900, Some(T), j), secs(585));
    }

    #[test]
    fn backdated_1h_cert_renews_36m_after_install() {
        // 24 min (2/5) remaining = 36 min after install; jitter ±180 s.
        assert_eq!(delay_after_install(3600, Some(T), 0), secs(2160));
        let j = max_jitter_secs(3600);
        assert_eq!(j, 180);
        assert_eq!(delay_after_install(3600, Some(T), -j), secs(1980));
        assert_eq!(delay_after_install(3600, Some(T), j), secs(2340));
    }

    #[test]
    fn thirty_day_cert_schedule_is_unchanged_without_metadata() {
        // Pre-K1 formula, bit for bit: renew when 2/5 of (NotAfter - NotBefore)
        // remains.
        let ttl = 30 * 86_400;
        let lifetime = ttl + BACKDATE;
        let legacy = T + ttl - lifetime * 2 / 5 - T;
        assert_eq!(delay_after_install(ttl, None, 0), secs(legacy));
        assert_eq!(delay_after_install(ttl, None, 900), secs(legacy + 900));
        assert_eq!(delay_after_install(ttl, None, -900), secs(legacy - 900));
        // With metadata the hour of backdate no longer counts: renewal moves
        // 2/5 h = 24 min later, still 12 days before expiry.
        assert_eq!(
            delay_after_install(ttl, Some(T), 0),
            secs(legacy + BACKDATE * 2 / 5)
        );
        assert_eq!(delay_after_install(ttl, Some(T), 0), secs(18 * 86_400));
    }

    #[test]
    fn missing_metadata_keeps_the_pre_k1_schedule() {
        // Without metadata a backdated short cert behaves exactly as before
        // K1 (renew now) — no new behaviour is forced on upgrade; the next
        // install writes metadata. A 1h cert issued long ago is not forced
        // to renew merely because metadata is missing.
        assert_eq!(
            delay_after_install(900, None, 0),
            Plan::RenewIn(Duration::ZERO)
        );
        assert_eq!(plan(T, T - 60, T + 3600, None, 0), secs(3600 - 1464));
    }

    #[test]
    fn expired_certificate_never_renews_even_with_metadata() {
        for installed in [None, Some(T - 100), Some(T + 10_000)] {
            assert_eq!(
                plan(T + 900, T - BACKDATE, T + 900, installed, 0),
                Plan::Expired
            );
            assert_eq!(
                plan(T + 5000, T - BACKDATE, T + 900, installed, 0),
                Plan::Expired
            );
        }
    }

    #[test]
    fn short_ttl_install_cycles_do_not_renew_immediately() {
        // KI-1 regression: install a backdated 15-min cert, plan, sleep, renew,
        // install the next one, ... Before K1 every plan() said "renew now".
        let mut now = T;
        for cycle in 0..10 {
            let (nb, na) = (now - BACKDATE, now + 900);
            let jitter = jitter_from(cycle * 7919, na - now);
            let Plan::RenewIn(delay) = plan(now, nb, na, Some(now), jitter) else {
                panic!("cycle {cycle}: fresh certificate reported expired");
            };
            assert!(
                (495..=585).contains(&delay.as_secs()),
                "cycle {cycle}: renewal {}s after install, expected ~540s",
                delay.as_secs()
            );
            now += delay.as_secs() as i64; // renew; the next install happens now
        }
        // Ten 15-minute certificates lasted ~90 minutes, not ten instant loops.
        assert!(now - T >= 10 * 495);
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
        csrs: Mutex<Vec<Vec<u8>>>,
    }

    impl FakeRenewer {
        fn new(reply: std::result::Result<RenewCertResponse, tonic::Status>) -> Self {
            Self {
                reply: Mutex::new(Some(reply)),
                calls: AtomicUsize::new(0),
                presented: Mutex::new(vec![]),
                csrs: Mutex::new(vec![]),
            }
        }
    }

    impl Renewer for FakeRenewer {
        async fn renew(
            &self,
            current: &CertMaterial,
            csr_der: Vec<u8>,
        ) -> std::result::Result<RenewCertResponse, tonic::Status> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            self.csrs.lock().unwrap().push(csr_der);
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

        // Same key (D-19): the CSR carries the existing key, the key file is
        // untouched and the installed certificate is for that key.
        let key_spki = key_spki_der(&f.manager.current().key_pem).unwrap();
        assert_eq!(key_spki, f.key.public_key_der());
        let csrs = renewer.csrs.lock().unwrap();
        let (_, csr) =
            x509_parser::certification_request::X509CertificationRequest::from_der(&csrs[0])
                .unwrap();
        assert_eq!(
            csr.certification_request_info.subject_pki.raw,
            key_spki.as_slice(),
            "renewal CSR must use the relay's existing key"
        );
        assert_eq!(
            std::fs::read(f.dir.join("relay.key")).unwrap(),
            f.key.serialize_pem().as_bytes()
        );
        let leaf = single_leaf(&installed.certificate_pem).unwrap();
        let (_, cert) = X509Certificate::from_der(leaf.as_ref()).unwrap();
        assert_eq!(cert.public_key().raw, key_spki.as_slice());
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

    #[tokio::test]
    async fn scheduler_does_not_loop_after_installing_a_backdated_short_cert() {
        // KI-1 end to end. A pre-K1 relay (no metadata) holds a backdated
        // 15-min cert: the legacy schedule renews it at once (one call). The
        // renewed cert is backdated too, but now its install time is
        // recorded, so the scheduler must sleep ~9 min instead of calling
        // RenewCert again. FakeRenewer has exactly one reply.
        let f = fixture(-BACKDATE, BACKDATE + 900);
        assert_eq!(f.manager.current().installed_at_unix, None);
        let renewed = f
            .pki
            .relay_cert(RELAY_ID, &f.key, -BACKDATE, BACKDATE + 900);
        let renewer = Arc::new(FakeRenewer::new(Ok(f.reply(renewed))));
        let mut rx = f.manager.subscribe();

        let task = tokio::spawn(run(
            f.manager.clone(),
            ArcRenewer(renewer.clone()),
            f.settings.clone(),
        ));
        tokio::time::timeout(Duration::from_secs(10), rx.changed())
            .await
            .expect("legacy schedule renews the backdated cert promptly")
            .unwrap();
        tokio::time::sleep(Duration::from_millis(1500)).await;
        let calls = renewer.calls.load(Ordering::SeqCst);
        task.abort();

        assert_eq!(calls, 1, "RenewCert loop: called {calls} times");
        let current = f.manager.current();
        let meta = crate::cert_manager::read_install_meta(&f.dir.join("relay.crt"))
            .unwrap()
            .expect("renewal must record the install time");
        assert_eq!(meta.serial_hex, current.serial_hex);
        assert_eq!(current.installed_at_unix, Some(meta.installed_at_unix));
        let Plan::RenewIn(next) = plan(
            meta.installed_at_unix,
            current.not_before_unix,
            current.not_after_unix,
            current.installed_at_unix,
            0,
        ) else {
            panic!("fresh certificate reported expired");
        };
        assert!(
            (535..=545).contains(&next.as_secs()),
            "next renewal in {next:?}"
        );
    }

    #[tokio::test]
    async fn expired_certificate_with_metadata_still_never_renews() {
        let f = fixture(-7200, 3600); // expired an hour ago
        crate::cert_manager::record_install(
            &f.dir.join("relay.crt"),
            f.original_pem.as_bytes(),
            now_unix() - 7000,
        )
        .unwrap();
        let manager = CertManager::load(
            RELAY_ID,
            CertPaths {
                key_path: f.dir.join("relay.key"),
                certificate_path: f.dir.join("relay.crt"),
                intermediate_ca_path: f.dir.join("intermediate-ca.crt"),
            },
        )
        .unwrap();
        assert!(manager.current().installed_at_unix.is_some());
        let renewer = FakeRenewer::new(Err(tonic::Status::internal("must not be called")));
        let err = renew_once(&manager, &renewer, &f.settings)
            .await
            .unwrap_err();
        assert!(matches!(err, RenewError::Expired), "got {err}");
        tokio::time::timeout(
            Duration::from_secs(5),
            run(
                manager.clone(),
                FakeRenewer::new(Err(tonic::Status::internal("x"))),
                f.settings.clone(),
            ),
        )
        .await
        .expect("scheduler must stop for an expired certificate");
        assert_eq!(renewer.calls.load(Ordering::SeqCst), 0);
    }

    /// Lets a test keep a handle on the FakeRenewer the scheduler owns.
    struct ArcRenewer(Arc<FakeRenewer>);

    impl Renewer for ArcRenewer {
        async fn renew(
            &self,
            current: &CertMaterial,
            csr_der: Vec<u8>,
        ) -> std::result::Result<RenewCertResponse, tonic::Status> {
            self.0.renew(current, csr_der).await
        }
    }
}
