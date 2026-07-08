use std::sync::atomic::{AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

use anyhow::{bail, Context, Result};
use quinn::{Connection, RecvStream, SendStream};
use rustls::pki_types::CertificateDer;
use rustls::server::WebPkiClientVerifier;
use rustls::{RootCertStore, ServerConfig};
use rustls_pemfile::{certs, private_key};
use tokio::sync::{mpsc, Notify, Semaphore};
use tokio::time::timeout;
use tokio_rustls::TlsAcceptor;
use tracing::{info, warn};
use x509_parser::extensions::GeneralName;
use x509_parser::prelude::{FromDer, X509Certificate};

use crate::agent_tunnel::AgentTunnelHub;
use crate::crl::CrlManager;
use crate::device_tunnel;
use crate::policy::PolicyCache;
use crate::tls::cert_store::CertStore;
use crate::ControlMessage;

const INNER_TUNNEL_ALPN: &[u8] = b"ztna-tunnel-v1";
const CLIENT_ROLE: &str = "client";

#[derive(Clone, Debug, Default)]
pub struct RelayDrainTracker {
    inner: Arc<RelayDrainTrackerInner>,
}

#[derive(Debug, Default)]
struct RelayDrainTrackerInner {
    active_streams: AtomicUsize,
    idle_notify: Notify,
}

impl RelayDrainTracker {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn active_streams(&self) -> usize {
        self.inner.active_streams.load(Ordering::Acquire)
    }

    pub fn track_stream(&self) -> RelayStreamGuard {
        self.inner.active_streams.fetch_add(1, Ordering::AcqRel);
        RelayStreamGuard {
            tracker: self.clone(),
        }
    }

    pub async fn wait_for_idle(&self) {
        loop {
            let notified = self.inner.idle_notify.notified();
            if self.active_streams() == 0 {
                return;
            }
            notified.await;
        }
    }
}

pub struct RelayStreamGuard {
    tracker: RelayDrainTracker,
}

impl Drop for RelayStreamGuard {
    fn drop(&mut self) {
        let previous = self
            .tracker
            .inner
            .active_streams
            .fetch_sub(1, Ordering::AcqRel);
        if previous == 1 {
            self.tracker.inner.idle_notify.notify_waiters();
        }
    }
}

/// Handles Client streams opened by the Relay.
///
/// Each stream must complete inner Client-to-Connector TLS 1.3 mTLS before
/// TunnelRequest or resource bytes are passed to `device_tunnel`.
pub struct RelayHandler {
    acceptor: TlsAcceptor,
    workspace_trust_domain: String,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    crl_manager: CrlManager,
    connector_id: String,
    control_tx: mpsc::Sender<ControlMessage>,
    handshake_timeout: Duration,
    stream_permits: Arc<Semaphore>,
}

impl RelayHandler {
    pub fn new(
        store: &CertStore,
        acl: Arc<PolicyCache>,
        tunnel_hub: AgentTunnelHub,
        crl_manager: CrlManager,
        connector_id: String,
        control_tx: mpsc::Sender<ControlMessage>,
        handshake_timeout_secs: u64,
        max_tunnel_streams: usize,
    ) -> Result<Self> {
        validate_runtime_limits(handshake_timeout_secs, max_tunnel_streams)?;
        let (tls_config, workspace_trust_domain) = build_inner_tls_server_config(store)?;
        Ok(Self {
            acceptor: TlsAcceptor::from(Arc::new(tls_config)),
            workspace_trust_domain,
            acl,
            tunnel_hub,
            crl_manager,
            connector_id,
            control_tx,
            handshake_timeout: Duration::from_secs(handshake_timeout_secs),
            stream_permits: Arc::new(Semaphore::new(max_tunnel_streams)),
        })
    }

    /// Accept Relay-opened streams until the outer Relay connection closes.
    pub async fn run(
        self: Arc<Self>,
        connection: Connection,
        drain_tracker: RelayDrainTracker,
    ) -> Result<()> {
        loop {
            let (send, recv) = connection
                .accept_bi()
                .await
                .context("accept Relay-opened Connector stream")?;
            let permit = match self.stream_permits.clone().try_acquire_owned() {
                Ok(permit) => permit,
                Err(_) => {
                    warn!(
                        rejection_reason = "relay_tunnel_stream_limit",
                        "rejecting Relay tunnel stream because capacity is exhausted"
                    );
                    reject_stream(send, recv);
                    continue;
                }
            };
            let handler = self.clone();
            let stream_guard = drain_tracker.track_stream();
            tokio::spawn(async move {
                let _permit = permit;
                let _stream_guard = stream_guard;
                if let Err(error) = handler.handle_stream(send, recv).await {
                    warn!(%error, "Relay tunnel stream failed");
                }
            });
        }
    }

    async fn handle_stream(&self, send: SendStream, recv: RecvStream) -> Result<()> {
        // The Relay only bridges these bytes. Inner TLS authenticates the
        // Client directly and hides tunnel payloads from the Relay.
        let relay_stream = tokio::io::join(recv, send);
        let tls_stream = timeout(self.handshake_timeout, self.acceptor.accept(relay_stream))
            .await
            .context("inner Client-to-Connector mTLS handshake timed out")?
            .context("inner Client-to-Connector mTLS handshake")?;

        let (client_spiffe_id, cert_serial) = {
            let peer_cert = tls_stream
                .get_ref()
                .1
                .peer_certificates()
                .and_then(|chain| chain.first())
                .context("inner mTLS Client did not present a certificate")?;
            extract_client_identity(peer_cert, &self.workspace_trust_domain)?
        };

        info!(
            client_spiffe_id,
            connector_id = %self.connector_id,
            "accepted inner mTLS Client stream through Relay"
        );
        device_tunnel::handle_stream(
            tls_stream,
            client_spiffe_id,
            cert_serial,
            self.acl.clone(),
            self.tunnel_hub.clone(),
            self.crl_manager.clone(),
            &self.connector_id,
            &self.control_tx,
        )
        .await
    }
}

fn reject_stream(mut send: SendStream, mut recv: RecvStream) {
    let _ = send.reset(0u32.into());
    let _ = recv.stop(0u32.into());
}

fn validate_runtime_limits(handshake_timeout_secs: u64, max_tunnel_streams: usize) -> Result<()> {
    if handshake_timeout_secs == 0 || max_tunnel_streams == 0 {
        bail!("Relay inner handshake timeout and tunnel stream limit must be greater than zero");
    }
    Ok(())
}

fn build_inner_tls_server_config(store: &CertStore) -> Result<(ServerConfig, String)> {
    let server_certs = parse_certificates(&store.cert_pem, "Connector certificate")?;
    let connector_leaf = server_certs
        .first()
        .context("Connector certificate PEM contains no leaf certificate")?;
    let workspace_trust_domain = extract_connector_trust_domain(connector_leaf)?;

    // Existing Connector state stores Workspace CA first and Platform
    // Intermediate second. Inner mTLS trusts only this Connector's Workspace CA.
    let workspace_bundle = parse_certificates(&store.workspace_ca_pem, "Workspace CA bundle")?;
    let workspace_ca = workspace_bundle
        .first()
        .context("Workspace CA bundle contains no Workspace CA")?;
    let mut roots = RootCertStore::empty();
    roots
        .add(workspace_ca.clone())
        .context("add Workspace CA as inner mTLS Client trust anchor")?;
    let client_verifier = WebPkiClientVerifier::builder(Arc::new(roots))
        .build()
        .context("build inner mTLS Client verifier")?;

    let server_key = private_key(&mut store.key_pem.as_slice())
        .context("parse Connector private key PEM")?
        .context("Connector private key PEM contains no private key")?;
    let mut config = ServerConfig::builder_with_protocol_versions(&[&rustls::version::TLS13])
        .with_client_cert_verifier(client_verifier)
        .with_single_cert(server_certs, server_key)
        .context("build inner mTLS server config; Connector certificate and key may not match")?;
    config.alpn_protocols = vec![INNER_TUNNEL_ALPN.to_vec()];
    Ok((config, workspace_trust_domain))
}

fn parse_certificates(pem: &[u8], label: &str) -> Result<Vec<CertificateDer<'static>>> {
    let certificates = certs(&mut pem.as_ref())
        .collect::<std::result::Result<Vec<_>, _>>()
        .with_context(|| format!("parse {label} PEM"))?;
    if certificates.is_empty() {
        bail!("{label} PEM contains no certificates");
    }
    Ok(certificates)
}

fn extract_connector_trust_domain(cert_der: &CertificateDer<'_>) -> Result<String> {
    let spiffe_id = extract_exact_spiffe_uri(cert_der)?;
    let (trust_domain, role, _) = parse_spiffe_identity(&spiffe_id)?;
    if role != "connector" {
        bail!("Connector certificate SPIFFE role must be connector");
    }
    Ok(trust_domain.to_owned())
}

fn extract_client_identity(
    cert_der: &CertificateDer<'_>,
    expected_trust_domain: &str,
) -> Result<(String, Vec<u8>)> {
    let (_, cert) = X509Certificate::from_der(cert_der.as_ref())
        .map_err(|error| anyhow::anyhow!("parse inner mTLS Client certificate: {error:?}"))?;
    let spiffe_id = extract_exact_spiffe_uri_from_cert(&cert)?;
    let (trust_domain, role, entity_id) = parse_spiffe_identity(&spiffe_id)?;

    if trust_domain != expected_trust_domain {
        bail!(
            "inner mTLS Client workspace {} does not match Connector workspace {}",
            trust_domain,
            expected_trust_domain
        );
    }
    if role != CLIENT_ROLE {
        bail!("inner mTLS peer SPIFFE role must be {CLIENT_ROLE}");
    }
    validate_canonical_uuid(entity_id)?;

    Ok((spiffe_id, cert.raw_serial().to_vec()))
}

fn extract_exact_spiffe_uri(cert_der: &CertificateDer<'_>) -> Result<String> {
    let (_, cert) = X509Certificate::from_der(cert_der.as_ref())
        .map_err(|error| anyhow::anyhow!("parse certificate: {error:?}"))?;
    extract_exact_spiffe_uri_from_cert(&cert)
}

fn extract_exact_spiffe_uri_from_cert(cert: &X509Certificate<'_>) -> Result<String> {
    let san = cert
        .subject_alternative_name()
        .map_err(|error| anyhow::anyhow!("parse certificate SAN: {error:?}"))?
        .context("certificate has no SAN extension")?;
    let mut spiffe_uris = san
        .value
        .general_names
        .iter()
        .filter_map(|name| match name {
            GeneralName::URI(uri) if uri.starts_with("spiffe://") => Some((*uri).to_owned()),
            _ => None,
        });
    let spiffe_id = spiffe_uris
        .next()
        .context("certificate has no SPIFFE URI SAN")?;
    if spiffe_uris.next().is_some() {
        bail!("certificate contains multiple SPIFFE URI SANs");
    }
    Ok(spiffe_id)
}

fn parse_spiffe_identity(spiffe_id: &str) -> Result<(&str, &str, &str)> {
    let identity = spiffe_id
        .strip_prefix("spiffe://")
        .context("SPIFFE identity must start with spiffe://")?;
    let segments: Vec<_> = identity.split('/').collect();
    if segments.len() != 3 || segments.iter().any(|segment| segment.is_empty()) {
        bail!("SPIFFE identity must use spiffe://<trust-domain>/<role>/<uuid>");
    }
    if segments[0].contains(['@', ':', '?', '#'])
        || segments[1].contains(['?', '#'])
        || segments[2].contains(['?', '#'])
    {
        bail!("SPIFFE identity contains unsupported URI components");
    }
    Ok((segments[0], segments[1], segments[2]))
}

fn validate_canonical_uuid(value: &str) -> Result<()> {
    let parsed = uuid::Uuid::parse_str(value).context("SPIFFE entity ID must be a UUID")?;
    if parsed.hyphenated().to_string() != value {
        bail!("SPIFFE entity UUID must use canonical lowercase hyphenated form");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::time::Duration;

    const ID: &str = "550e8400-e29b-41d4-a716-446655440000";

    #[tokio::test]
    async fn drain_tracker_waits_until_last_stream_guard_drops() {
        let tracker = RelayDrainTracker::new();
        let first = tracker.track_stream();
        let second = tracker.track_stream();
        assert_eq!(tracker.active_streams(), 2);

        let waiter = tokio::spawn({
            let tracker = tracker.clone();
            async move {
                tracker.wait_for_idle().await;
            }
        });

        drop(first);
        assert_eq!(tracker.active_streams(), 1);
        tokio::time::sleep(Duration::from_millis(10)).await;
        assert!(!waiter.is_finished());

        drop(second);
        waiter.await.unwrap();
        assert_eq!(tracker.active_streams(), 0);
    }

    #[test]
    fn accepts_exact_client_identity_for_workspace() {
        let spiffe = format!("spiffe://ws-acme.zecurity.in/client/{ID}");
        let (trust_domain, role, entity_id) = parse_spiffe_identity(&spiffe).unwrap();
        assert_eq!(trust_domain, "ws-acme.zecurity.in");
        assert_eq!(role, CLIENT_ROLE);
        validate_canonical_uuid(entity_id).unwrap();
    }

    #[test]
    fn rejects_client_device_role_and_noncanonical_uuid() {
        let spiffe = format!("spiffe://ws-acme.zecurity.in/client_device/{ID}");
        let (_, role, _) = parse_spiffe_identity(&spiffe).unwrap();
        assert_ne!(role, CLIENT_ROLE);
        assert!(validate_canonical_uuid("550E8400-E29B-41D4-A716-446655440000").is_err());
    }

    #[test]
    fn rejects_malformed_spiffe_identity() {
        assert!(parse_spiffe_identity("spiffe://ws/client/id/extra").is_err());
        assert!(parse_spiffe_identity("https://ws/client/id").is_err());
        assert!(parse_spiffe_identity("spiffe://user@ws/client/id").is_err());
    }

    #[test]
    fn relay_runtime_limits_must_be_positive() {
        validate_runtime_limits(10, 256).unwrap();
        assert!(validate_runtime_limits(0, 256).is_err());
        assert!(validate_runtime_limits(10, 0).is_err());
    }
}
