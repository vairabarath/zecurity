use std::net::SocketAddr;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use quinn::Endpoint;
use tokio::sync::{watch, Semaphore};
use tokio::task::JoinHandle;
use tokio::time::timeout;
use tracing::{error, info, warn};

use crate::cert_manager::{CertManager, CertMaterial};
use crate::config::RuntimeLimits;
use crate::crl::RevocationStatus;
use crate::session;
use crate::state::RelayState;
use crate::tls;

/// Pure fail-closed revocation decision: only `NotRevoked` may proceed to
/// bridging. `Revoked` and `Unavailable` are both rejected — an unknown
/// revocation state is treated exactly like a confirmed revocation, never
/// as "allow by default". Ok(()) = allow; Err(reason) = reject with this
/// QUIC close reason.
fn revocation_action(status: RevocationStatus) -> Result<(), &'static [u8]> {
    match status {
        RevocationStatus::NotRevoked => Ok(()),
        RevocationStatus::Revoked => Err(b"peer certificate revoked"),
        RevocationStatus::Unavailable => Err(b"revocation state unavailable"),
    }
}

/// Build the QUIC server config for a certificate snapshot.
pub fn server_config_for(
    material: &CertMaterial,
    relay_id: &str,
    limits: &RuntimeLimits,
) -> Result<quinn::ServerConfig> {
    tls::build_server_config(
        &material.certificate_pem,
        &material.key_pem,
        &material.intermediate_ca_pem,
        relay_id,
        limits.max_bidi_streams,
        limits.idle_timeout,
    )
}

/// Swap the listener's certificate whenever the CertManager publishes a
/// renewed one. `set_server_config` only affects NEW handshakes; established
/// QUIC connections keep running on the certificate they negotiated.
pub fn spawn_server_config_updates(
    endpoint: Endpoint,
    mut certs: watch::Receiver<Arc<CertMaterial>>,
    relay_id: String,
    limits: RuntimeLimits,
) -> JoinHandle<()> {
    tokio::spawn(async move {
        while certs.changed().await.is_ok() {
            let material = certs.borrow_and_update().clone();
            match server_config_for(&material, &relay_id, &limits) {
                Ok(config) => {
                    endpoint.set_server_config(Some(config));
                    info!(serial = %material.serial_hex, "Relay listener switched to renewed certificate");
                }
                // CertManager validated identity + key before publishing, so this
                // is unexpected; the listener keeps its current certificate.
                Err(err) => {
                    error!(error = %err, serial = %material.serial_hex, "failed to build listener config for renewed certificate")
                }
            }
        }
    })
}

/// QUIC application close sent to every peer when the relay is revoked.
pub const RELAY_REVOKED_CLOSE_CODE: u32 = 0x52_45_56; // "REV"
pub const RELAY_REVOKED_REASON: &[u8] = b"relay revoked";

pub async fn run_listener(
    bind_addr: SocketAddr,
    certs: Arc<CertManager>,
    state: Arc<RelayState>,
    limits: RuntimeLimits,
    crl: crate::crl::WorkspaceCrlManager,
    stop: impl std::future::Future<Output = ()>,
) -> Result<()> {
    let relay_id = certs.relay_id().to_owned();
    let server_config = server_config_for(&certs.current(), &relay_id, &limits)?;
    let endpoint = Endpoint::server(server_config, bind_addr).context("create Relay endpoint")?;
    let _config_updates = spawn_server_config_updates(
        endpoint.clone(),
        certs.subscribe(),
        relay_id,
        limits.clone(),
    );
    let connection_permits = Arc::new(Semaphore::new(limits.max_connections));
    let session_limits = session::SessionLimits::new(&limits);

    info!(
        addr = %bind_addr,
        max_connections = limits.max_connections,
        max_lookup_bridges = limits.max_lookup_bridges,
        max_bidi_streams = limits.max_bidi_streams,
        "Relay QUIC listener started"
    );

    tokio::pin!(stop);
    loop {
        let incoming = tokio::select! {
            incoming = endpoint.accept() => match incoming {
                Some(incoming) => incoming,
                None => break,
            },
            () = &mut stop => {
                // The controller revoked this relay (live E2E run 2026-10-30,
                // F-8). Close every connection with a clear reason instead of
                // serving on: clients and connectors fail over at once rather
                // than reaching a relay that can no longer bridge.
                warn!("Relay revoked: closing every connection and the listener");
                endpoint.close(RELAY_REVOKED_CLOSE_CODE.into(), RELAY_REVOKED_REASON);
                let _ = timeout(Duration::from_secs(2), endpoint.wait_idle()).await;
                return Ok(());
            }
        };
        let permit = match connection_permits.clone().try_acquire_owned() {
            Ok(permit) => permit,
            Err(_) => {
                warn!(
                    max_connections = limits.max_connections,
                    rejection_reason = "connection_limit",
                    "refusing Relay connection because capacity is exhausted"
                );
                incoming.refuse();
                continue;
            }
        };
        let state = state.clone();
        let crl = crl.clone();
        let certs = certs.clone();
        let session_limits = session_limits.clone();
        let handshake_timeout = limits.handshake_timeout;
        tokio::spawn(async move {
            let _permit = permit;
            let connection = match timeout(handshake_timeout, incoming).await {
                Ok(Ok(connection)) => connection,
                Ok(Err(error)) => {
                    warn!(error = %error, "Relay QUIC handshake failed");
                    return;
                }
                Err(_) => {
                    warn!(
                        timeout_ms = handshake_timeout.as_millis(),
                        timeout_class = "quic_handshake",
                        "Relay QUIC handshake timed out"
                    );
                    return;
                }
            };

            let identity = match tls::authenticated_peer_identity(&connection) {
                Ok(identity) => identity,
                Err(error) => {
                    warn!(
                        remote = %connection.remote_address(),
                        error = %error,
                        "rejecting Relay peer with invalid authenticated identity"
                    );
                    connection.close(0u32.into(), b"invalid peer identity");
                    return;
                }
            };

            info!(
                remote = %connection.remote_address(),
                spiffe_id = %identity.uri,
                role = %identity.role,
                trust_domain = %identity.trust_domain,
                "accepted authenticated Relay peer"
            );
            // Track-2: reject a revoked connector/client on the outer mTLS. Fail closed.
            let intermediate_ca_pem = certs.current().intermediate_ca_pem.clone();
            match tls::peer_revocation(&connection, &intermediate_ca_pem) {
                Ok(pr) => {
                    let status = crl
                        .check(&pr.workspace_id, &pr.leaf_serial, &pr.workspace_ca_der)
                        .await;
                    if let Err(reason) = revocation_action(status) {
                        warn!(spiffe_id = %identity.uri, ?status, "rejecting peer due to revocation check");
                        connection.close(0u32.into(), reason);
                        return;
                    }
                }
                Err(error) => {
                    warn!(error = %error, "cannot derive peer revocation context — failing closed");
                    connection.close(0u32.into(), b"revocation context error");
                    return;
                }
            }
            if let Err(error) =
                session::handle_connection(connection, identity, state, session_limits).await
            {
                warn!(error = %error, "Relay session ended with error");
            }
        });
    }

    Ok(())
}

#[cfg(test)]
mod revocation_action_tests {
    use super::*;

    #[test]
    fn allows_not_revoked() {
        assert!(revocation_action(RevocationStatus::NotRevoked).is_ok());
    }

    #[test]
    fn rejects_revoked() {
        assert_eq!(
            revocation_action(RevocationStatus::Revoked),
            Err(b"peer certificate revoked" as &[u8]),
        );
    }

    #[test]
    fn fails_closed_on_unavailable() {
        assert_eq!(
            revocation_action(RevocationStatus::Unavailable),
            Err(b"revocation state unavailable" as &[u8]),
        );
    }
}

#[cfg(test)]
mod live_swap_tests {
    use super::*;
    use crate::cert_manager::{serial_hex, CertPaths};
    use crate::test_support::{TestPki, RELAY_ID};
    use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
    use std::time::Duration;
    use x509_parser::certificate::X509Certificate;
    use x509_parser::prelude::FromDer;

    fn limits() -> RuntimeLimits {
        RuntimeLimits {
            max_connections: 16,
            max_lookup_bridges: 16,
            max_bidi_streams: 16,
            idle_timeout: Duration::from_secs(30),
            handshake_timeout: Duration::from_secs(5),
            message_timeout: Duration::from_secs(5),
            max_probe_rate: 10,
            max_concurrent_probes: 4,
            probe_timeout: Duration::from_secs(2),
        }
    }

    fn client_endpoint(pki: &TestPki) -> Endpoint {
        let mut roots = rustls::RootCertStore::empty();
        let ca = rustls_pemfile::certs(&mut pki.ca_pem.as_bytes())
            .next()
            .unwrap()
            .unwrap();
        roots.add(ca).unwrap();
        let (chain, key) = pki.client_chain();
        let key = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der()));
        let mut tls = rustls::ClientConfig::builder()
            .with_root_certificates(roots)
            .with_client_auth_cert(chain, key)
            .unwrap();
        tls.alpn_protocols = vec![tls::RELAY_ALPN.to_vec()];
        let quic = quinn::crypto::rustls::QuicClientConfig::try_from(tls).unwrap();
        let mut endpoint = Endpoint::client("127.0.0.1:0".parse().unwrap()).unwrap();
        endpoint.set_default_client_config(quinn::ClientConfig::new(Arc::new(quic)));
        endpoint
    }

    fn server_serial(conn: &quinn::Connection) -> String {
        let chain = conn
            .peer_identity()
            .unwrap()
            .downcast::<Vec<CertificateDer<'static>>>()
            .unwrap();
        let (_, cert) = X509Certificate::from_der(chain[0].as_ref()).unwrap();
        serial_hex(cert.raw_serial())
    }

    async fn echo(conn: &quinn::Connection, msg: &[u8]) -> Vec<u8> {
        let (mut send, mut recv) = conn.open_bi().await.expect("open stream");
        send.write_all(msg).await.unwrap();
        send.finish().unwrap();
        recv.read_to_end(1024).await.expect("echo reply")
    }

    /// Echo server holding every accepted connection open.
    fn spawn_echo_server(endpoint: Endpoint) {
        tokio::spawn(async move {
            while let Some(incoming) = endpoint.accept().await {
                tokio::spawn(async move {
                    let Ok(conn) = incoming.await else { return };
                    while let Ok((mut send, mut recv)) = conn.accept_bi().await {
                        let data = recv.read_to_end(1024).await.unwrap_or_default();
                        let _ = send.write_all(&data).await;
                        let _ = send.finish();
                    }
                });
            }
        });
    }

    /// Renewal swaps the listener certificate live: new handshakes present the
    /// renewed certificate, and a QUIC connection established BEFORE the swap
    /// keeps working (it is not dropped).
    #[tokio::test]
    async fn listener_swap_serves_new_cert_without_dropping_existing_connections() {
        let pki = TestPki::new();
        let key = pki.relay_key();
        let dir =
            std::env::temp_dir().join(format!("relay-swap-{}", uuid::Uuid::new_v4().simple()));
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
        let original_serial = manager.current().serial_hex.clone();

        let server = Endpoint::server(
            server_config_for(&manager.current(), RELAY_ID, &limits()).unwrap(),
            "127.0.0.1:0".parse().unwrap(),
        )
        .unwrap();
        let addr = server.local_addr().unwrap();
        let _updates = spawn_server_config_updates(
            server.clone(),
            manager.subscribe(),
            RELAY_ID.to_owned(),
            limits(),
        );
        spawn_echo_server(server);

        let client = client_endpoint(&pki);
        let existing = client
            .connect(addr, "localhost")
            .unwrap()
            .await
            .expect("initial handshake");
        assert_eq!(server_serial(&existing), original_serial);
        assert_eq!(echo(&existing, b"before-swap").await, b"before-swap");

        let renewed = manager
            .install_renewed(pki.relay_cert(RELAY_ID, &key, -60, 7200).as_bytes())
            .unwrap();

        // New handshakes pick up the renewed certificate once the watcher swaps.
        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        loop {
            let conn = client
                .connect(addr, "localhost")
                .unwrap()
                .await
                .expect("post-swap handshake");
            if server_serial(&conn) == renewed.serial_hex {
                break;
            }
            assert!(
                tokio::time::Instant::now() < deadline,
                "listener never presented the renewed certificate"
            );
            tokio::time::sleep(Duration::from_millis(20)).await;
        }

        // The pre-swap connection is still alive and usable, still on the old cert.
        assert!(
            existing.close_reason().is_none(),
            "existing connection was dropped: {:?}",
            existing.close_reason()
        );
        assert_eq!(echo(&existing, b"after-swap").await, b"after-swap");
        assert_eq!(server_serial(&existing), original_serial);
    }
}

/// Audit finding S1: which Workspace CA (and so which CRL) the revocation
/// check uses, end to end through the real listener (`run_listener`): real
/// QUIC/mTLS handshake against the production server config, then the
/// production revocation gate with CRLs fetched over HTTP.
#[cfg(test)]
mod revocation_issuer_tests {
    use super::*;
    use crate::cert_manager::CertPaths;
    use crate::crl::WorkspaceCrlManager;
    use crate::test_support::{TestPki, TestWorkspaceCa, RELAY_ID};
    use rustls::pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer};
    use std::collections::HashMap;
    use std::time::Duration;
    use tokio::io::{AsyncReadExt, AsyncWriteExt};

    const TENANT_A: &str = "aaaaaaaa-0000-0000-0000-000000000001";
    const TENANT_B: &str = "bbbbbbbb-0000-0000-0000-000000000002";
    const REVOKED_SERIAL: &[u8] = &[0x51, 0x01, 0x01, 0x01];
    const GOOD_SERIAL: &[u8] = &[0x52, 0x02, 0x02, 0x02];

    fn limits() -> RuntimeLimits {
        RuntimeLimits {
            max_connections: 16,
            max_lookup_bridges: 16,
            max_bidi_streams: 16,
            idle_timeout: Duration::from_secs(30),
            // An admitted peer that opens no stream is dropped after this.
            handshake_timeout: Duration::from_secs(1),
            message_timeout: Duration::from_secs(1),
            max_probe_rate: 10,
            max_concurrent_probes: 4,
            probe_timeout: Duration::from_secs(1),
        }
    }

    /// Workspace ids whose CRL the relay fetched, in order.
    type Fetched = Arc<std::sync::Mutex<Vec<String>>>;

    /// Serves `/ca.crl?workspace_id=<id>` like the controller: each
    /// workspace's own CRL, signed by that workspace's CA; 404 otherwise.
    async fn spawn_crl_server(crls: HashMap<String, Vec<u8>>, fetched: Fetched) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let crls = Arc::new(crls);
        tokio::spawn(async move {
            while let Ok((mut sock, _)) = listener.accept().await {
                let crls = crls.clone();
                let fetched = fetched.clone();
                tokio::spawn(async move {
                    let mut buf = [0u8; 2048];
                    let n = sock.read(&mut buf).await.unwrap_or(0);
                    let request = String::from_utf8_lossy(&buf[..n]);
                    let workspace = request
                        .split_whitespace()
                        .nth(1)
                        .and_then(|path| path.split("workspace_id=").nth(1))
                        .unwrap_or("")
                        .to_owned();
                    fetched.lock().unwrap().push(workspace.clone());
                    let (status, body) = match crls.get(&workspace) {
                        Some(der) => ("200 OK", der.clone()),
                        None => ("404 Not Found", Vec::new()),
                    };
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

    /// Starts the production listener; returns its address.
    async fn start_relay(pki: &TestPki, crl_http: String) -> SocketAddr {
        let key = pki.relay_key();
        let dir = std::env::temp_dir().join(format!("relay-s1-{}", uuid::Uuid::new_v4().simple()));
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
        let addr = std::net::UdpSocket::bind("127.0.0.1:0")
            .unwrap()
            .local_addr()
            .unwrap();
        tokio::spawn(run_listener(
            addr,
            manager,
            RelayState::new(),
            limits(),
            WorkspaceCrlManager::new(crl_http),
            std::future::pending(),
        ));
        tokio::time::sleep(Duration::from_millis(200)).await;
        addr
    }

    /// QUIC close reasons the listener uses when it refuses a peer after the
    /// handshake (identity and revocation gates).
    const GATE_REJECTIONS: &[&str] = &[
        "invalid peer identity",
        "peer certificate revoked",
        "revocation state unavailable",
        "revocation context error",
    ];

    #[derive(Debug, PartialEq)]
    enum Outcome {
        /// A gate refused the peer (reason recorded).
        Rejected(String),
        /// Every gate passed; the session then closed the idle peer (no first
        /// stream within `handshake_timeout`) — reason recorded.
        Admitted(String),
    }

    /// Connect presenting `chain` (sent as-is; a rustls client does not
    /// validate its own chain). Panics if the TLS handshake itself fails, so
    /// an outcome always means "passed path validation".
    async fn connect(
        pki: &TestPki,
        relay: SocketAddr,
        chain: Vec<CertificateDer<'static>>,
        key: rcgen::KeyPair,
    ) -> Outcome {
        let mut roots = rustls::RootCertStore::empty();
        roots
            .add(
                rustls_pemfile::certs(&mut pki.ca_pem.as_bytes())
                    .next()
                    .unwrap()
                    .unwrap(),
            )
            .unwrap();
        let key = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(key.serialize_der()));
        let mut tls = rustls::ClientConfig::builder()
            .with_root_certificates(roots)
            .with_client_auth_cert(chain, key)
            .unwrap();
        tls.alpn_protocols = vec![tls::RELAY_ALPN.to_vec()];
        let mut endpoint = Endpoint::client("127.0.0.1:0".parse().unwrap()).unwrap();
        endpoint.set_default_client_config(quinn::ClientConfig::new(Arc::new(
            quinn::crypto::rustls::QuicClientConfig::try_from(tls).unwrap(),
        )));
        let connection = endpoint
            .connect(relay, "localhost")
            .unwrap()
            .await
            .expect("TLS handshake must succeed: the chain is a valid path");
        let reason = match tokio::time::timeout(Duration::from_secs(10), connection.closed())
            .await
            .expect("relay must close an idle peer within the session timeout")
        {
            quinn::ConnectionError::ApplicationClosed(close) => {
                String::from_utf8_lossy(&close.reason).into_owned()
            }
            other => format!("{other}"),
        };
        if GATE_REJECTIONS.contains(&reason.as_str()) {
            Outcome::Rejected(reason)
        } else {
            Outcome::Admitted(reason)
        }
    }

    struct Setup {
        pki: TestPki,
        ca_a: TestWorkspaceCa,
        ca_b: TestWorkspaceCa,
        relay: SocketAddr,
        fetched: Fetched,
    }

    /// Tenant A's CRL revokes REVOKED_SERIAL; tenant B's CRL revokes nothing.
    async fn setup() -> Setup {
        crate::test_support::install_crypto_provider();
        let pki = TestPki::new();
        let ca_a = pki.workspace_ca(TENANT_A);
        let ca_b = pki.workspace_ca(TENANT_B);
        let crls = HashMap::from([
            (TENANT_A.to_owned(), ca_a.crl(&[REVOKED_SERIAL])),
            (TENANT_B.to_owned(), ca_b.crl(&[])),
        ]);
        let fetched = Fetched::default();
        let relay = start_relay(&pki, spawn_crl_server(crls, fetched.clone()).await).await;
        Setup {
            pki,
            ca_a,
            ca_b,
            relay,
            fetched,
        }
    }

    /// Baseline: the normal chain `[leaf, its CA]` is checked against its own
    /// workspace's CRL — a good leaf is admitted, a revoked one rejected.
    #[tokio::test]
    async fn normal_chain_is_checked_against_its_own_workspace_crl() {
        let s = setup().await;

        let (good, good_key) = s.ca_a.client_leaf(GOOD_SERIAL);
        let outcome = connect(&s.pki, s.relay, vec![good, s.ca_a.der()], good_key).await;
        assert!(
            matches!(outcome, Outcome::Admitted(_)),
            "good leaf must pass the revocation gate, got {outcome:?}"
        );

        let (revoked, revoked_key) = s.ca_a.client_leaf(REVOKED_SERIAL);
        assert_eq!(
            connect(&s.pki, s.relay, vec![revoked, s.ca_a.der()], revoked_key).await,
            Outcome::Rejected("peer certificate revoked".into())
        );
        assert_eq!(s.fetched.lock().unwrap().as_slice(), [TENANT_A.to_owned()]);
    }

    /// F-8 (live E2E run 2026-10-30): when the relay is revoked the listener
    /// closes every connection with "relay revoked" and returns, instead of
    /// keeping peers on a relay that can no longer bridge.
    #[tokio::test]
    async fn revoked_relay_closes_connections_and_stops() {
        crate::test_support::install_crypto_provider();
        let pki = TestPki::new();
        let ca_a = pki.workspace_ca(TENANT_A);
        let crls = HashMap::from([(TENANT_A.to_owned(), ca_a.crl(&[]))]);
        let crl_http = spawn_crl_server(crls, Fetched::default()).await;
        let key = pki.relay_key();
        let dir = std::env::temp_dir().join(format!("relay-f8-{}", uuid::Uuid::new_v4().simple()));
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
        let addr = std::net::UdpSocket::bind("127.0.0.1:0")
            .unwrap()
            .local_addr()
            .unwrap();
        let (stop_tx, stop_rx) = tokio::sync::oneshot::channel::<()>();
        let listener = tokio::spawn(run_listener(
            addr,
            manager,
            RelayState::new(),
            limits(),
            WorkspaceCrlManager::new(crl_http),
            async move {
                let _ = stop_rx.await;
            },
        ));
        tokio::time::sleep(Duration::from_millis(200)).await;

        let (good, good_key) = ca_a.client_leaf(GOOD_SERIAL);
        let chain = vec![good, ca_a.der()];
        let peer = tokio::spawn(async move { connect(&pki, addr, chain, good_key).await });
        tokio::time::sleep(Duration::from_millis(500)).await;
        stop_tx.send(()).unwrap();

        // Well inside the 5 s session timeout that would otherwise close it.
        let outcome = tokio::time::timeout(Duration::from_secs(3), peer)
            .await
            .expect("peer must be closed promptly")
            .unwrap();
        assert_eq!(outcome, Outcome::Admitted("relay revoked".into()));
        tokio::time::timeout(Duration::from_secs(5), listener)
            .await
            .expect("listener must return after the stop")
            .unwrap()
            .unwrap();
    }

    /// S1: a revoked tenant-A leaf presented as `[leaf, tenant B's CA,
    /// tenant A's CA]`. Path validation ignores the unrelated CA and builds
    /// leaf -> CA_A -> Intermediate, so the handshake succeeds; the peer must
    /// still be refused (revoked per tenant A's CRL, or the chain refused),
    /// and tenant B's CRL must never decide it.
    ///
    /// Before the fix `peer_revocation` took chain[1] (tenant B's CA) as the
    /// issuer, checked tenant B's CRL, and admitted the revoked leaf.
    #[tokio::test]
    async fn s1_revoked_leaf_with_foreign_ca_in_chain_is_still_rejected() {
        let s = setup().await;
        let (revoked, revoked_key) = s.ca_a.client_leaf(REVOKED_SERIAL);
        let outcome = connect(
            &s.pki,
            s.relay,
            vec![revoked, s.ca_b.der(), s.ca_a.der()],
            revoked_key,
        )
        .await;
        let fetched = s.fetched.lock().unwrap().clone();
        assert!(
            matches!(outcome, Outcome::Rejected(_)) && !fetched.iter().any(|w| w == TENANT_B),
            "revoked tenant-A leaf: {outcome:?} after a successful handshake; CRLs fetched \
             for the check: {fetched:?} (tenant A = {TENANT_A}, tenant B = {TENANT_B})"
        );
    }

    /// S1b (certificate audit, 2026-10-15): the S1 fix proves chain[1] has
    /// the leaf issuer's NAME and PUBLIC KEY, then trusts chain[1]'s
    /// `tenant:` label — but anyone can mint a certificate with tenant A's CA
    /// name and public key (both public) that claims `tenant:B`. Path
    /// validation skips that forgery (it chains to nothing) and builds
    /// leaf -> CA_A -> Intermediate, so the handshake succeeds; the CRL cache
    /// is keyed by workspace only, so once tenant B's CRL is cached the
    /// revoked tenant-A leaf is checked against B's CRL and admitted.
    ///
    /// Before the fix the revoked leaf was admitted with only tenant B's CRL
    /// consulted.
    #[tokio::test]
    async fn s1b_revoked_leaf_with_forged_issuer_copy_is_still_rejected() {
        let s = setup().await;

        // Normal operation: any tenant-B peer connects, caching B's CRL.
        let (b_peer, b_key) = s.ca_b.client_leaf(GOOD_SERIAL);
        let primed = connect(&s.pki, s.relay, vec![b_peer, s.ca_b.der()], b_key).await;
        assert!(
            matches!(primed, Outcome::Admitted(_)),
            "setup: a good tenant-B peer must be admitted, got {primed:?}"
        );

        // The attack: revoked tenant-A leaf + forged "tenant B" copy of CA_A.
        let forged = s.ca_a.forged_copy_claiming(TENANT_B);
        let (revoked, revoked_key) = s.ca_a.client_leaf(REVOKED_SERIAL);
        let outcome = connect(
            &s.pki,
            s.relay,
            vec![revoked, forged, s.ca_a.der()],
            revoked_key,
        )
        .await;

        let fetched = s.fetched.lock().unwrap().clone();
        assert!(
            matches!(outcome, Outcome::Rejected(_)),
            "revoked tenant-A leaf with a forged tenant-B copy of its CA: {outcome:?} after a \
             successful handshake; CRLs fetched: {fetched:?} (tenant A = {TENANT_A}, tenant B = {TENANT_B})"
        );
    }
}
