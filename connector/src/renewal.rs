// renewal.rs — Certificate renewal for the ZECURITY connector
//
// Called when the Control stream receives ReEnroll (sent by the controller
// inside CONNECTOR_RENEWAL_WINDOW — Sprint 20 Phase G-1).
//
// The connector keeps its existing EC P-384 keypair; only the certificate is
// renewed. Lifecycle (Sprint 20 Phase G-2a):
//   1. single-flight + debounce: rapid/duplicate ReEnroll messages produce one
//      renewal;
//   2. CSR from the EXISTING key → RenewCert over the current mTLS identity;
//   3. CertHolder::install_renewed: verify → atomic persist → publish, after
//      which every consumer switches to the renewed certificate;
//   4. update state.json (informational cert_not_after).

use std::future::Future;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use time::OffsetDateTime;
use tracing::info;

use crate::config::ConnectorConfig;
use crate::controller_client;
use crate::crypto;
use crate::enrollment::EnrollmentState;
use crate::proto;
use crate::tls::cert_holder::{CertHolder, CertMaterial};

/// A ReEnroll arriving this soon after a successful renewal is a duplicate
/// (queued before the controller saw the renewed certificate) and is ignored.
pub const REENROLL_DEBOUNCE: Duration = Duration::from_secs(60);

/// The controller call behind renewal (real gRPC, or a test fake).
pub trait CertRenewer: Send + Sync {
    fn renew(
        &self,
        current: &CertMaterial,
        csr_der: Vec<u8>,
    ) -> impl Future<Output = Result<proto::RenewCertResponse>> + Send;
}

/// `ConnectorService.RenewCert` over mTLS, presenting the CURRENT certificate.
pub struct GrpcCertRenewer<'a> {
    cfg: &'a ConnectorConfig,
    connector_id: &'a str,
}

impl CertRenewer for GrpcCertRenewer<'_> {
    async fn renew(
        &self,
        current: &CertMaterial,
        csr_der: Vec<u8>,
    ) -> Result<proto::RenewCertResponse> {
        let channel = controller_client::build_channel(self.cfg, &current.store)
            .await
            .context("failed to build mTLS channel")?;
        let mut client = proto::connector_service_client::ConnectorServiceClient::new(channel);
        Ok(client
            .renew_cert(proto::RenewCertRequest {
                connector_id: self.connector_id.to_owned(),
                public_key_der: csr_der,
            })
            .await
            .context("renew_cert RPC failed")?
            .into_inner())
    }
}

#[derive(Debug)]
pub enum RenewalOutcome {
    /// Renewed, persisted and published.
    Renewed(Arc<CertMaterial>),
    /// Another renewal is already running (single-flight).
    InProgress,
    /// A renewal completed within REENROLL_DEBOUNCE; this ReEnroll is a duplicate.
    RecentlyRenewed,
}

/// Run one renewal through `holder`, guarded so concurrent or back-to-back
/// ReEnroll messages produce exactly one renewal.
pub async fn renew_with<R: CertRenewer>(
    holder: &CertHolder,
    renewer: &R,
) -> Result<RenewalOutcome> {
    let Some(_guard) = holder.try_begin_renewal() else {
        return Ok(RenewalOutcome::InProgress);
    };
    if holder.renewed_within(REENROLL_DEBOUNCE) {
        return Ok(RenewalOutcome::RecentlyRenewed);
    }

    let current = holder.current();
    let key_pem =
        std::str::from_utf8(&current.store.key_pem).context("connector.key is not valid UTF-8")?;
    // Same key: the CSR is proof of possession of the EXISTING private key.
    let csr_der = crypto::extract_public_key_der(key_pem).context("failed to build renewal CSR")?;

    let resp = renewer.renew(&current, csr_der).await?;
    let installed = holder
        .install_renewed(
            &resp.certificate_pem,
            &resp.workspace_ca_pem,
            &resp.intermediate_ca_pem,
        )
        .context("renewed certificate rejected")?;
    Ok(RenewalOutcome::Renewed(installed))
}

/// Renew the connector's certificate (Control stream ReEnroll handler).
///
/// Returns `Some(new_state)` when a renewed certificate was installed (the
/// caller reconnects the Control stream with it) and `None` when the request
/// was a duplicate of an in-flight or just-completed renewal.
pub async fn renew_cert(
    state: &EnrollmentState,
    cfg: &ConnectorConfig,
    holder: &CertHolder,
) -> Result<Option<EnrollmentState>> {
    info!("starting certificate renewal");
    let renewer = GrpcCertRenewer {
        cfg,
        connector_id: &state.connector_id,
    };
    match renew_with(holder, &renewer).await? {
        RenewalOutcome::Renewed(material) => {
            let not_after = OffsetDateTime::from_unix_timestamp(material.not_after_unix)
                .context("invalid renewed certificate expiry")?;
            let new_state = EnrollmentState {
                connector_id: state.connector_id.clone(),
                trust_domain: state.trust_domain.clone(),
                workspace_id: state.workspace_id.clone(),
                enrolled_at: state.enrolled_at.clone(),
                cert_not_after: format!("{}", not_after),
            };
            new_state
                .save(&cfg.state_dir)
                .context("failed to save renewed state")?;
            info!(
                serial = %material.serial_hex,
                new_expiry = %new_state.cert_not_after,
                "certificate renewed, persisted and published"
            );
            Ok(Some(new_state))
        }
        RenewalOutcome::InProgress => {
            info!("certificate renewal already in progress — ignoring duplicate ReEnroll");
            Ok(None)
        }
        RenewalOutcome::RecentlyRenewed => {
            info!("certificate renewed moments ago — ignoring duplicate ReEnroll");
            Ok(None)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_support::TestPki;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// Answers RenewCert with a pre-issued same-key renewal, after a delay so
    /// concurrent attempts overlap.
    struct FakeRenewer {
        response: proto::RenewCertResponse,
        calls: AtomicUsize,
        delay: Duration,
    }

    impl FakeRenewer {
        fn new(pki: &TestPki, delay: Duration) -> Self {
            Self {
                response: proto::RenewCertResponse {
                    certificate_pem: pki.connector_leaf(-60, 7200).into_bytes(),
                    workspace_ca_pem: pki.workspace_ca_pem.clone().into_bytes(),
                    intermediate_ca_pem: pki.intermediate_pem.clone().into_bytes(),
                },
                calls: AtomicUsize::new(0),
                delay,
            }
        }
    }

    impl CertRenewer for FakeRenewer {
        async fn renew(
            &self,
            _current: &CertMaterial,
            _csr: Vec<u8>,
        ) -> Result<proto::RenewCertResponse> {
            self.calls.fetch_add(1, Ordering::SeqCst);
            tokio::time::sleep(self.delay).await;
            Ok(self.response.clone())
        }
    }

    /// Several ReEnroll messages arriving together (concurrent) and
    /// back-to-back (sequential, before the controller sees the new cert)
    /// still produce exactly ONE renewal.
    #[tokio::test]
    async fn rapid_reenroll_messages_produce_one_renewal() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let renewer = FakeRenewer::new(&pki, Duration::from_millis(50));

        let (a, b, c) = tokio::join!(
            renew_with(&holder, &renewer),
            renew_with(&holder, &renewer),
            renew_with(&holder, &renewer)
        );
        let outcomes = [a.unwrap(), b.unwrap(), c.unwrap()];
        let renewed = outcomes
            .iter()
            .filter(|o| matches!(o, RenewalOutcome::Renewed(_)))
            .count();
        let in_progress = outcomes
            .iter()
            .filter(|o| matches!(o, RenewalOutcome::InProgress))
            .count();
        assert_eq!((renewed, in_progress), (1, 2), "outcomes: {outcomes:?}");

        // A stale ReEnroll queued before the controller saw the renewal.
        let later = renew_with(&holder, &renewer).await.unwrap();
        assert!(
            matches!(later, RenewalOutcome::RecentlyRenewed),
            "got {later:?}"
        );

        assert_eq!(
            renewer.calls.load(Ordering::SeqCst),
            1,
            "RenewCert must be called exactly once"
        );
    }

    /// A failed renewal does not arm the debounce: the next ReEnroll retries.
    #[tokio::test]
    async fn failed_renewal_allows_retry() {
        struct Failing(AtomicUsize);
        impl CertRenewer for Failing {
            async fn renew(
                &self,
                _c: &CertMaterial,
                _csr: Vec<u8>,
            ) -> Result<proto::RenewCertResponse> {
                self.0.fetch_add(1, Ordering::SeqCst);
                anyhow::bail!("controller unavailable")
            }
        }
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let failing = Failing(AtomicUsize::new(0));

        assert!(renew_with(&holder, &failing).await.is_err());
        assert!(renew_with(&holder, &failing).await.is_err());
        assert_eq!(
            failing.0.load(Ordering::SeqCst),
            2,
            "each ReEnroll after a failure must retry"
        );
    }

    /// The renewal CSR is built from the existing key (never a new one).
    #[tokio::test]
    async fn renewal_reuses_the_existing_key() {
        struct Capture(parking_lot::Mutex<Vec<u8>>, proto::RenewCertResponse);
        impl CertRenewer for Capture {
            async fn renew(
                &self,
                _c: &CertMaterial,
                csr: Vec<u8>,
            ) -> Result<proto::RenewCertResponse> {
                *self.0.lock() = csr;
                Ok(self.1.clone())
            }
        }
        let pki = TestPki::new();
        let (dir, holder) = pki.holder(-60, 3600);
        let key_before = std::fs::read(dir.path().join("connector.key")).unwrap();
        let capture = Capture(
            parking_lot::Mutex::new(vec![]),
            FakeRenewer::new(&pki, Duration::ZERO).response,
        );

        renew_with(&holder, &capture).await.unwrap();

        use x509_parser::certification_request::X509CertificationRequest;
        use x509_parser::prelude::FromDer;
        let csr = capture.0.lock().clone();
        let (_, parsed) = X509CertificationRequest::from_der(&csr).unwrap();
        parsed
            .verify_signature()
            .expect("CSR signed by the existing key");
        let spki = crate::tls::cert_holder::key_spki_der(&key_before).unwrap();
        assert_eq!(
            parsed.certification_request_info.subject_pki.raw,
            spki.as_slice()
        );
        assert_eq!(
            std::fs::read(dir.path().join("connector.key")).unwrap(),
            key_before,
            "key must never be rewritten"
        );
    }
}
