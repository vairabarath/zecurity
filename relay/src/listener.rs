use std::net::SocketAddr;
use std::sync::Arc;

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

pub async fn run_listener(
    bind_addr: SocketAddr,
    certs: Arc<CertManager>,
    state: Arc<RelayState>,
    limits: RuntimeLimits,
    crl: crate::crl::WorkspaceCrlManager,
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

    while let Some(incoming) = endpoint.accept().await {
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
            match tls::peer_revocation(&connection) {
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
