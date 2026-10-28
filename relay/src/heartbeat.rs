use std::future::Future;
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{Context, Result};
use tokio::sync::watch;
use tonic::transport::{Certificate, Channel, ClientTlsConfig, Endpoint, Identity};
use tracing::{error, info, warn};

use crate::cert_manager::{CertManager, CertMaterial};
use crate::config::RelayConfig;
use crate::relay::v1::relay_service_client::RelayServiceClient;
use crate::relay::v1::{HeartbeatRequest, HeartbeatResponse};
use crate::session::ACTIVE_STREAMS;
use crate::state::RelayState;

const RECONNECT_DELAY: Duration = Duration::from_secs(5);
const RPC_TIMEOUT: Duration = Duration::from_secs(10);

/// Trailer key and value the controller attaches when this relay is revoked or
/// deleted (Sprint 20 F32; controller `connector.RelayStateMarkerKey`). Only
/// the marker is terminal: a refusal without it (a superseded serial, an
/// unreadable status, `Unavailable`) keeps the retry loop.
const RELAY_STATE_MARKER_KEY: &str = "zecurity-relay-state";
const RELAY_STATE_REVOKED: &str = "revoked";

/// Heartbeat inputs taken from RelayConfig.
#[derive(Clone, Debug)]
pub struct HeartbeatSettings {
    pub listen_port: u32,
    pub max_connections: u32,
    pub default_interval: Duration,
}

impl HeartbeatSettings {
    fn from_config(cfg: &RelayConfig) -> Self {
        Self {
            listen_port: cfg.bind_addr.port() as u32,
            max_connections: cfg.runtime_limits.max_connections as u32,
            default_interval: cfg.heartbeat_interval,
        }
    }
}

/// How the heartbeat reaches the controller (real mTLS gRPC, or a test fake).
/// `connect` is always given the CURRENT certificate from the CertManager.
pub trait HeartbeatTransport: Send + Sync {
    type Conn: Send;
    fn connect(&self, material: &CertMaterial) -> impl Future<Output = Result<Self::Conn>> + Send;
    fn send(
        &self,
        conn: &mut Self::Conn,
        request: HeartbeatRequest,
    ) -> impl Future<Output = Result<HeartbeatResponse>> + Send;
}

pub struct GrpcHeartbeat {
    controller_addr: String,
}

impl HeartbeatTransport for GrpcHeartbeat {
    type Conn = RelayServiceClient<Channel>;

    async fn connect(&self, material: &CertMaterial) -> Result<Self::Conn> {
        let grpc_host = controller_host(&self.controller_addr)?;
        let grpc_addr = format!("https://{}", self.controller_addr);
        let channel = Endpoint::from_shared(grpc_addr.clone())
            .with_context(|| format!("invalid controller gRPC address: {grpc_addr}"))?
            .tls_config(
                ClientTlsConfig::new()
                    .identity(Identity::from_pem(
                        &material.certificate_pem,
                        &material.key_pem,
                    ))
                    .ca_certificate(Certificate::from_pem(&material.intermediate_ca_pem))
                    .domain_name(grpc_host),
            )
            .context("configure Relay heartbeat mTLS")?
            .connect()
            .await
            .with_context(|| format!("connect Relay heartbeat to {grpc_addr}"))?;
        Ok(RelayServiceClient::new(channel))
    }

    async fn send(
        &self,
        conn: &mut Self::Conn,
        request: HeartbeatRequest,
    ) -> Result<HeartbeatResponse> {
        let mut request = tonic::Request::new(request);
        request.set_timeout(RPC_TIMEOUT);
        Ok(conn
            .heartbeat(request)
            .await
            .context("Relay Heartbeat RPC failed")?
            .into_inner())
    }
}

pub async fn run(cfg: RelayConfig, certs: Arc<CertManager>, state: Arc<RelayState>) {
    let transport = GrpcHeartbeat {
        controller_addr: cfg.controller_addr.clone(),
    };
    run_with(
        transport,
        HeartbeatSettings::from_config(&cfg),
        certs,
        state,
    )
    .await
}

/// Heartbeat loop. Always connects with the CertManager's current certificate
/// and reconnects immediately when a renewed certificate is published, so the
/// old certificate is never presented again after a renewal.
pub async fn run_with<T: HeartbeatTransport>(
    transport: T,
    settings: HeartbeatSettings,
    certs: Arc<CertManager>,
    state: Arc<RelayState>,
) {
    let started_at = Instant::now();
    let hostname = read_hostname();
    let mut cert_rx = certs.subscribe();

    loop {
        let material = cert_rx.borrow_and_update().clone();
        match run_connected(
            &transport,
            &settings,
            &material,
            &mut cert_rx,
            &hostname,
            started_at,
            &state,
        )
        .await
        {
            Ok(()) => {
                info!("Relay certificate renewed; reconnecting heartbeat with the new identity");
                continue;
            }
            Err(e) if is_terminal_refusal(&e) => {
                // Revoked is terminal (Sprint 20 invariant): stop dialling
                // instead of retrying every RECONNECT_DELAY forever (F32).
                error!(
                    error = %e,
                    "controller reports this Relay revoked; heartbeat stopped. Replace the relay to bring it back"
                );
                return;
            }
            Err(e) => warn!(error = %e, "Relay heartbeat connection ended"),
        }
        // Back off before reconnecting — but reconnect at once if a renewed
        // certificate is published meanwhile.
        tokio::select! {
            _ = tokio::time::sleep(RECONNECT_DELAY) => {}
            changed = cert_rx.changed() => {
                if changed.is_err() {
                    tokio::time::sleep(RECONNECT_DELAY).await;
                }
            }
        }
    }
}

/// Runs one connection. Returns Ok(()) only when the certificate changed
/// (caller reconnects with the new identity); errors end the connection.
async fn run_connected<T: HeartbeatTransport>(
    transport: &T,
    settings: &HeartbeatSettings,
    material: &CertMaterial,
    cert_rx: &mut watch::Receiver<Arc<CertMaterial>>,
    hostname: &str,
    started_at: Instant,
    state: &RelayState,
) -> Result<()> {
    let mut conn = transport.connect(material).await?;
    info!(serial = %material.serial_hex, "Relay heartbeat connected");

    loop {
        let request = HeartbeatRequest {
            version: env!("CARGO_PKG_VERSION").to_owned(),
            hostname: hostname.to_owned(),
            uptime_seconds: started_at.elapsed().as_secs(),
            registered_connectors: state.connector_count() as u64,
            listen_port: settings.listen_port,
            connection_count: ACTIVE_STREAMS.load(std::sync::atomic::Ordering::Relaxed),
            max_connections: settings.max_connections,
            registered_connector_spiffe_ids: state.registered_connector_spiffe_ids(),
        };
        let response = transport.send(&mut conn, request).await?;
        // Connector-revocation Phase D: drop revoked connectors (sticky set).
        let closed = state.apply_revoked(&response.revoked_connector_spiffe_ids);
        if !response.revoked_connector_spiffe_ids.is_empty() {
            tracing::warn!(
                revoked = response.revoked_connector_spiffe_ids.len(),
                closed,
                "controller reported revoked connectors"
            );
        }

        info!(
            server_time_unix = response.server_time_unix,
            registered_connectors = state.connector_count(),
            "Relay heartbeat acknowledged"
        );
        let interval = if response.next_heartbeat_seconds == 0 {
            settings.default_interval
        } else {
            Duration::from_secs(response.next_heartbeat_seconds.into())
        };
        tokio::select! {
            _ = tokio::time::sleep(interval) => {}
            changed = cert_rx.changed() => {
                if changed.is_ok() {
                    return Ok(());
                }
                tokio::time::sleep(interval).await;
            }
        }
    }
}

/// True only for a PermissionDenied carrying the controller's revoked marker.
fn is_terminal_refusal(err: &anyhow::Error) -> bool {
    err.chain()
        .filter_map(|cause| cause.downcast_ref::<tonic::Status>())
        .any(|status| {
            status.code() == tonic::Code::PermissionDenied
                && status
                    .metadata()
                    .get(RELAY_STATE_MARKER_KEY)
                    .and_then(|v| v.to_str().ok())
                    == Some(RELAY_STATE_REVOKED)
        })
}

fn controller_host(controller_addr: &str) -> Result<String> {
    let uri = format!("https://{controller_addr}")
        .parse::<http::Uri>()
        .context("invalid CONTROLLER_ADDR")?;
    uri.host()
        .map(str::to_owned)
        .context("CONTROLLER_ADDR must include a hostname")
}

fn read_hostname() -> String {
    std::fs::read_to_string("/etc/hostname")
        .map(|hostname| hostname.trim().to_owned())
        .unwrap_or_else(|_| "unknown".to_owned())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::cert_manager::CertPaths;
    use crate::test_support::{TestPki, RELAY_ID};
    use std::sync::Mutex;

    #[test]
    fn extracts_controller_host() {
        assert_eq!(
            controller_host("controller.example.com:9090").unwrap(),
            "controller.example.com"
        );
        assert_eq!(controller_host("127.0.0.1:9090").unwrap(), "127.0.0.1");
    }

    /// Records the certificate serial each connection was opened with.
    #[derive(Default, Clone)]
    struct RecordingTransport {
        connected_with: Arc<Mutex<Vec<String>>>,
    }

    impl HeartbeatTransport for RecordingTransport {
        type Conn = ();

        async fn connect(&self, material: &CertMaterial) -> Result<()> {
            self.connected_with
                .lock()
                .unwrap()
                .push(material.serial_hex.clone());
            Ok(())
        }

        async fn send(
            &self,
            _conn: &mut (),
            _request: HeartbeatRequest,
        ) -> Result<HeartbeatResponse> {
            // Long interval: only a certificate change can trigger a reconnect.
            Ok(HeartbeatResponse {
                server_time_unix: 0,
                next_heartbeat_seconds: 3600,
                revoked_connector_spiffe_ids: vec![],
            })
        }
    }

    /// Answers every heartbeat with a fixed revoked list and records the
    /// registered ids each request carried.
    #[derive(Clone)]
    struct RevokingTransport {
        revoked: Vec<String>,
        sent_ids: Arc<Mutex<Vec<Vec<String>>>>,
    }

    impl HeartbeatTransport for RevokingTransport {
        type Conn = ();

        async fn connect(&self, _material: &CertMaterial) -> Result<()> {
            Ok(())
        }

        async fn send(
            &self,
            _conn: &mut (),
            request: HeartbeatRequest,
        ) -> Result<HeartbeatResponse> {
            self.sent_ids
                .lock()
                .unwrap()
                .push(request.registered_connector_spiffe_ids);
            Ok(HeartbeatResponse {
                server_time_unix: 0,
                next_heartbeat_seconds: 3600,
                revoked_connector_spiffe_ids: self.revoked.clone(),
            })
        }
    }

    /// Answers the first heartbeat with a revoked id, then fails every call.
    #[derive(Clone, Default)]
    struct FailingAfterFirstTransport {
        revoked: String,
        sends: Arc<std::sync::atomic::AtomicUsize>,
    }

    impl HeartbeatTransport for FailingAfterFirstTransport {
        type Conn = ();

        async fn connect(&self, _material: &CertMaterial) -> Result<()> {
            Ok(())
        }

        async fn send(
            &self,
            _conn: &mut (),
            _request: HeartbeatRequest,
        ) -> Result<HeartbeatResponse> {
            if self.sends.fetch_add(1, std::sync::atomic::Ordering::SeqCst) == 0 {
                Ok(HeartbeatResponse {
                    server_time_unix: 0,
                    next_heartbeat_seconds: 1,
                    revoked_connector_spiffe_ids: vec![self.revoked.clone()],
                })
            } else {
                anyhow::bail!("controller unreachable")
            }
        }
    }

    /// AD-5 (D20): a failing heartbeat keeps the revoked set.
    #[tokio::test]
    async fn heartbeat_error_keeps_the_revoked_set() {
        let pki = TestPki::new();
        let key = pki.relay_key();
        let dir = std::env::temp_dir().join(format!("relay-hb-{}", uuid::Uuid::new_v4().simple()));
        std::fs::create_dir_all(&dir).unwrap();
        pki.write_state(&dir, &key, &pki.relay_cert(RELAY_ID, &key, -60, 3600));
        let manager = CertManager::load(
            RELAY_ID,
            CertPaths {
                key_path: dir.join("relay.key"),
                certificate_path: dir.join("relay.crt"),
                intermediate_ca_path: dir.join("intermediate-ca.crt"),
            },
        )
        .unwrap();
        let state = RelayState::new();
        let revoked = crate::state::canonical_connector_spiffe(
            "ws.zecurity.in",
            "4f1c2d7e-1111-4a2b-9c3d-000000000001",
        );
        let transport = FailingAfterFirstTransport {
            revoked: revoked.clone(),
            sends: Arc::default(),
        };
        let settings = HeartbeatSettings {
            listen_port: 9093,
            max_connections: 16,
            default_interval: Duration::from_secs(3600),
        };
        let task = tokio::spawn(run_with(
            transport.clone(),
            settings,
            manager,
            state.clone(),
        ));
        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        while transport.sends.load(std::sync::atomic::Ordering::SeqCst) < 2 {
            assert!(
                tokio::time::Instant::now() < deadline,
                "second heartbeat never sent"
            );
            tokio::time::sleep(Duration::from_millis(20)).await;
        }
        tokio::time::sleep(Duration::from_millis(50)).await;
        task.abort();
        assert!(
            state.is_revoked(&revoked),
            "a failed heartbeat must not unblock"
        );
    }

    /// AD-5: the heartbeat reports the registered connectors and applies the
    /// controller's revoked answer — the revoked connector is closed and
    /// removed, the other one stays.
    #[tokio::test]
    async fn heartbeat_reports_registered_ids_and_applies_revoked_answer() {
        let pki = TestPki::new();
        let key = pki.relay_key();
        let dir = std::env::temp_dir().join(format!("relay-hb-{}", uuid::Uuid::new_v4().simple()));
        std::fs::create_dir_all(&dir).unwrap();
        pki.write_state(&dir, &key, &pki.relay_cert(RELAY_ID, &key, -60, 3600));
        let manager = CertManager::load(
            RELAY_ID,
            CertPaths {
                key_path: dir.join("relay.key"),
                certificate_path: dir.join("relay.crt"),
                intermediate_ca_path: dir.join("intermediate-ca.crt"),
            },
        )
        .unwrap();

        let state = RelayState::new();
        let revoked_id = "4f1c2d7e-1111-4a2b-9c3d-000000000001";
        let kept_id = "4f1c2d7e-2222-4a2b-9c3d-000000000002";
        let (c1, s1) = crate::test_support::quic_pair().await;
        let (c2, s2) = crate::test_support::quic_pair().await;
        state.insert_connector(revoked_id.into(), s1, "u1".into(), "ws.zecurity.in".into());
        state.insert_connector(kept_id.into(), s2, "u2".into(), "ws.zecurity.in".into());
        let revoked = crate::state::canonical_connector_spiffe("ws.zecurity.in", revoked_id);
        let kept = crate::state::canonical_connector_spiffe("ws.zecurity.in", kept_id);

        let transport = RevokingTransport {
            revoked: vec![revoked.clone()],
            sent_ids: Arc::default(),
        };
        let settings = HeartbeatSettings {
            listen_port: 9093,
            max_connections: 16,
            default_interval: Duration::from_secs(3600),
        };
        let task = tokio::spawn(run_with(
            transport.clone(),
            settings,
            manager,
            state.clone(),
        ));

        tokio::time::timeout(Duration::from_secs(5), c1.closed())
            .await
            .expect("revoked connector closed");
        task.abort();
        let mut sent = transport.sent_ids.lock().unwrap()[0].clone();
        sent.sort();
        let mut want = vec![revoked.clone(), kept];
        want.sort();
        assert_eq!(sent, want, "request carries the registered ids");
        assert!(state.is_revoked(&revoked));
        assert!(c2.close_reason().is_none(), "unlisted connector untouched");
    }

    /// Connects fine, then refuses every heartbeat with `refusal()`, wrapped
    /// the way `GrpcHeartbeat::send` wraps it.
    #[derive(Clone)]
    struct RefusingTransport {
        refusal: fn() -> tonic::Status,
        connects: Arc<std::sync::atomic::AtomicUsize>,
    }

    impl HeartbeatTransport for RefusingTransport {
        type Conn = ();

        async fn connect(&self, _material: &CertMaterial) -> Result<()> {
            self.connects
                .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            Ok(())
        }

        async fn send(
            &self,
            _conn: &mut (),
            _request: HeartbeatRequest,
        ) -> Result<HeartbeatResponse> {
            Err((self.refusal)()).context("Relay Heartbeat RPC failed")
        }
    }

    fn revoked_marker_status() -> tonic::Status {
        let mut md = tonic::metadata::MetadataMap::new();
        md.insert(RELAY_STATE_MARKER_KEY, RELAY_STATE_REVOKED.parse().unwrap());
        tonic::Status::with_metadata(
            tonic::Code::PermissionDenied,
            "relay certificate revoked",
            md,
        )
    }

    /// Runs the heartbeat against `refusal` for 60 s of paused time and
    /// returns (connect attempts, whether the loop ended).
    async fn connects_in_a_minute(refusal: fn() -> tonic::Status) -> (usize, bool) {
        let pki = TestPki::new();
        let key = pki.relay_key();
        let dir = std::env::temp_dir().join(format!("relay-hb-{}", uuid::Uuid::new_v4().simple()));
        std::fs::create_dir_all(&dir).unwrap();
        pki.write_state(&dir, &key, &pki.relay_cert(RELAY_ID, &key, -60, 3600));
        let manager = CertManager::load(
            RELAY_ID,
            CertPaths {
                key_path: dir.join("relay.key"),
                certificate_path: dir.join("relay.crt"),
                intermediate_ca_path: dir.join("intermediate-ca.crt"),
            },
        )
        .unwrap();
        let transport = RefusingTransport {
            refusal,
            connects: Arc::default(),
        };
        let settings = HeartbeatSettings {
            listen_port: 9093,
            max_connections: 16,
            default_interval: Duration::from_secs(3600),
        };
        let task = tokio::spawn(run_with(
            transport.clone(),
            settings,
            manager,
            RelayState::new(),
        ));
        tokio::time::sleep(Duration::from_secs(60)).await;
        let finished = task.is_finished();
        task.abort();
        (
            transport.connects.load(std::sync::atomic::Ordering::SeqCst),
            finished,
        )
    }

    /// F32: the controller's revoked marker stops the heartbeat for good.
    #[tokio::test(start_paused = true)]
    async fn revoked_marker_stops_the_heartbeat() {
        let (connects, finished) = connects_in_a_minute(revoked_marker_status).await;
        assert_eq!(connects, 1, "a revoked relay must not redial");
        assert!(finished, "the heartbeat loop must end");
    }

    /// F32 guard: refusals without the marker keep the 5 s retry — a revoked
    /// serial of a live relay, and the fail-closed Unavailable.
    #[tokio::test(start_paused = true)]
    async fn refusals_without_the_marker_keep_retrying() {
        for refusal in [
            (|| tonic::Status::permission_denied("relay certificate revoked"))
                as fn() -> tonic::Status,
            || tonic::Status::unavailable("relay revocation state unavailable"),
            || {
                let mut md = tonic::metadata::MetadataMap::new();
                md.insert(RELAY_STATE_MARKER_KEY, "unknown".parse().unwrap());
                tonic::Status::with_metadata(tonic::Code::PermissionDenied, "x", md)
            },
            || {
                let mut md = tonic::metadata::MetadataMap::new();
                md.insert(RELAY_STATE_MARKER_KEY, RELAY_STATE_REVOKED.parse().unwrap());
                tonic::Status::with_metadata(tonic::Code::Unavailable, "x", md)
            },
        ] {
            let (connects, finished) = connects_in_a_minute(refusal).await;
            assert!(connects >= 10, "only {connects} attempts in 60 s");
            assert!(!finished, "the heartbeat loop must keep running");
        }
    }

    async fn wait_for_connects(transport: &RecordingTransport, n: usize) -> Vec<String> {
        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        loop {
            let seen = transport.connected_with.lock().unwrap().clone();
            if seen.len() >= n {
                return seen;
            }
            assert!(
                tokio::time::Instant::now() < deadline,
                "only {} heartbeat connect(s): {seen:?}",
                seen.len()
            );
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
    }

    /// After a renewal is published, the heartbeat reconnects promptly (well
    /// before its 1 h interval) presenting the NEW certificate.
    #[tokio::test]
    async fn heartbeat_reconnects_with_renewed_certificate() {
        let pki = TestPki::new();
        let key = pki.relay_key();
        let dir = std::env::temp_dir().join(format!("relay-hb-{}", uuid::Uuid::new_v4().simple()));
        std::fs::create_dir_all(&dir).unwrap();
        pki.write_state(&dir, &key, &pki.relay_cert(RELAY_ID, &key, -60, 3600));
        let manager = CertManager::load(
            RELAY_ID,
            CertPaths {
                key_path: dir.join("relay.key"),
                certificate_path: dir.join("relay.crt"),
                intermediate_ca_path: dir.join("intermediate-ca.crt"),
            },
        )
        .unwrap();
        let original = manager.current().serial_hex.clone();

        let transport = RecordingTransport::default();
        let settings = HeartbeatSettings {
            listen_port: 9093,
            max_connections: 16,
            default_interval: Duration::from_secs(3600),
        };
        let task = tokio::spawn(run_with(
            transport.clone(),
            settings,
            manager.clone(),
            RelayState::new(),
        ));

        assert_eq!(
            wait_for_connects(&transport, 1).await,
            vec![original.clone()]
        );

        let renewed = manager
            .install_renewed(pki.relay_cert(RELAY_ID, &key, -60, 7200).as_bytes())
            .unwrap();

        let seen = wait_for_connects(&transport, 2).await;
        task.abort();
        assert_eq!(
            seen,
            vec![original, renewed.serial_hex.clone()],
            "heartbeat must reconnect with the renewed certificate"
        );
    }
}
