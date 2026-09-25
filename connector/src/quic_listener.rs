use std::sync::Arc;

use anyhow::Result;
use rustls;
use tokio::sync::{mpsc, watch};
use tokio::task::JoinHandle;
use tracing::{info, warn};

use crate::agent_tunnel::AgentTunnelHub;
use crate::crl::CrlManager;
use crate::device_tunnel;
use crate::policy::PolicyCache;
use crate::session_registry::{SessionRegistry, SessionTransport};
use crate::tls::cert_holder::{CertHolder, CertMaterial};
use crate::tls::cert_store::CertStore;
use crate::tls::server_cfg::build_device_tunnel_tls;
use crate::ControlMessage;

/// QUIC/UDP listener on the same port as the TLS/TCP device tunnel (:9092).
///
/// Each accepted QUIC bidirectional stream is handed off to
/// `device_tunnel::handle_stream` — the same handler used by the TLS listener.
/// The OS demultiplexes TCP vs UDP on the same port number.
///
/// `advertise_addr` is the external address included in every `TunnelResponse`
/// so clients can pre-warm a QUIC connection for subsequent streams.
/// QUIC server config for one certificate snapshot.
pub fn quic_server_config_for(store: &CertStore) -> Result<quinn::ServerConfig> {
    let tls_config = build_device_tunnel_tls(store)?;
    let quic_server_cfg = quinn::crypto::rustls::QuicServerConfig::try_from(tls_config)
        .map_err(|e| anyhow::anyhow!("QUIC server config: {}", e))?;
    Ok(quinn::ServerConfig::with_crypto(Arc::new(quic_server_cfg)))
}

/// Swap the QUIC listener's certificate whenever the CertHolder publishes a
/// renewal (Sprint 20 G-2a). `set_server_config` affects NEW handshakes only;
/// established QUIC connections keep running.
pub fn spawn_quic_config_updates(
    endpoint: quinn::Endpoint,
    mut certs: watch::Receiver<Arc<CertMaterial>>,
) -> JoinHandle<()> {
    tokio::spawn(async move {
        while certs.changed().await.is_ok() {
            let material = certs.borrow_and_update().clone();
            match quic_server_config_for(&material.store) {
                Ok(config) => {
                    endpoint.set_server_config(Some(config));
                    info!(serial = %material.serial_hex, "device tunnel (QUIC) switched to renewed certificate");
                }
                // The holder validated identity + key before publishing, so this
                // is unexpected; the listener keeps its current certificate.
                Err(e) => warn!(error = %e, serial = %material.serial_hex, "failed to build QUIC config for renewed certificate"),
            }
        }
    })
}

pub async fn listen(
    addr: &str,
    advertise_addr: &str,
    certs: Arc<CertHolder>,
    acl: Arc<PolicyCache>,
    registry: Arc<SessionRegistry>,
    tunnel_hub: AgentTunnelHub,
    crl_manager: CrlManager,
    connector_id: String,
    control_tx: mpsc::Sender<ControlMessage>,
) -> Result<()> {
    let server_config = quic_server_config_for(&certs.current().store)?;

    let socket_addr: std::net::SocketAddr = addr
        .parse()
        .map_err(|e| anyhow::anyhow!("bad QUIC addr '{}': {}", addr, e))?;
    let endpoint = quinn::Endpoint::server(server_config, socket_addr)?;
    let _config_updates = spawn_quic_config_updates(endpoint.clone(), certs.subscribe());

    // Register the QUIC advertise address so device_tunnel includes it in responses.
    device_tunnel::set_quic_advertise_addr(advertise_addr.to_string());
    info!(
        "device tunnel (QUIC) listening on {} advertise={}",
        addr, advertise_addr
    );

    loop {
        let Some(incoming) = endpoint.accept().await else {
            break;
        };

        let acl = acl.clone();
         let registry = registry.clone();
        let tunnel_hub = tunnel_hub.clone();
        let crl = crl_manager.clone();
        let conn_id = connector_id.clone();
        let ctrl_tx = control_tx.clone();

        tokio::spawn(async move {
            let conn = match incoming.await {
                Ok(c) => c,
                Err(e) => {
                    warn!("QUIC connection error: {}", e);
                    return;
                }
            };

            // Extract SPIFFE ID and cert serial from the peer's mTLS certificate.
            // The certificate is available on the connection after the handshake.
            let (spiffe_id, cert_serial) = conn
                .peer_identity()
                .and_then(|id| {
                    id.downcast::<Vec<rustls::pki_types::CertificateDer<'static>>>()
                        .ok()
                })
                .and_then(|certs| certs.first().cloned())
                .and_then(|cert| device_tunnel::extract_peer_info_pub(cert.as_ref()).ok())
                .unwrap_or_else(|| {
                    warn!("QUIC connection: no peer cert or SPIFFE extraction failed — rejecting");
                    (String::new(), vec![])
                });

            if spiffe_id.is_empty() {
                return;
            }

            loop {
                let (send, recv) = match conn.accept_bi().await {
                    Ok(pair) => pair,
                    Err(e) => {
                        warn!("QUIC accept_bi: {}", e);
                        break;
                    }
                };

                let stream = tokio::io::join(recv, send);
                let acl = acl.clone();
                 let registry = registry.clone();
                let hub = tunnel_hub.clone();
                let crl = crl.clone();
                let conn_id = conn_id.clone();
                let ctrl_tx = ctrl_tx.clone();
                let sid = spiffe_id.clone();
                let serial = cert_serial.clone();

                tokio::spawn(async move {
                    if let Err(e) = device_tunnel::handle_stream(
                        stream,
                        sid,
                        serial,
                        acl,
                        registry,
                        SessionTransport::Quic,
                        hub,
                        crl,
                        &conn_id,
                        &ctrl_tx,
                    )
                    .await
                    {
                        warn!("QUIC stream error: {}", e);
                    }
                });
            }
        });
    }

    Ok(())
}

#[cfg(test)]
mod swap_tests {
    use super::*;
    use crate::test_support::{quic_server_serial, TestPki};
    use std::time::Duration;

    async fn echo(conn: &quinn::Connection, msg: &[u8]) -> Vec<u8> {
        let (mut send, mut recv) = conn.open_bi().await.expect("open stream");
        send.write_all(msg).await.unwrap();
        send.finish().unwrap();
        recv.read_to_end(1024).await.expect("echo reply")
    }

    fn spawn_echo_server(endpoint: quinn::Endpoint) {
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

    /// Renewal swaps the QUIC listener certificate via set_server_config: new
    /// handshakes present the renewed serial, and a connection established
    /// BEFORE the swap keeps working on its original certificate.
    #[tokio::test]
    async fn quic_swap_serves_new_cert_and_preserves_existing_connections() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let original = holder.current().serial_hex.clone();

        let server = quinn::Endpoint::server(
            quic_server_config_for(&holder.current().store).unwrap(),
            "127.0.0.1:0".parse().unwrap(),
        )
        .unwrap();
        let addr = server.local_addr().unwrap();
        let _updates = spawn_quic_config_updates(server.clone(), holder.subscribe());
        spawn_echo_server(server);

        let client = pki.quic_client();
        let existing = client.connect(addr, "localhost").unwrap().await.expect("initial handshake");
        assert_eq!(quic_server_serial(&existing), original);
        assert_eq!(echo(&existing, b"before").await, b"before");

        let renewed = pki.renew(&holder, 7200);

        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        loop {
            let conn = client.connect(addr, "localhost").unwrap().await.expect("post-swap handshake");
            if quic_server_serial(&conn) == renewed.serial_hex {
                break;
            }
            assert!(tokio::time::Instant::now() < deadline, "QUIC listener never presented the renewed certificate");
            tokio::time::sleep(Duration::from_millis(20)).await;
        }

        assert!(existing.close_reason().is_none(), "existing QUIC connection dropped: {:?}", existing.close_reason());
        assert_eq!(echo(&existing, b"after").await, b"after");
        assert_eq!(quic_server_serial(&existing), original);
    }
}
