use std::sync::Arc;

use anyhow::Result;
use tokio::sync::mpsc;
use tracing::{info, warn};

use crate::agent_tunnel::AgentTunnelHub;
use crate::crl::CrlManager;
use crate::device_tunnel;
use crate::policy::PolicyCache;
use crate::tls::cert_store::CertStore;
use crate::tls::server_cfg::build_device_tunnel_tls;
use crate::AgentRegistry;
use crate::ControlMessage;

/// QUIC/UDP listener on the same port as the TLS/TCP device tunnel (:9092).
///
/// Each accepted QUIC bidirectional stream is handed off to
/// `device_tunnel::handle_stream` — the same handler used by the TLS listener.
/// The OS demultiplexes TCP vs UDP on the same port number.
///
/// `advertise_addr` is the external address included in every `TunnelResponse`
/// so clients can pre-warm a QUIC connection for subsequent streams.
pub async fn listen(
    addr: &str,
    advertise_addr: &str,
    store: CertStore,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    agent_registry: Arc<AgentRegistry>,
    crl_manager: CrlManager,
    connector_id: String,
    control_tx: mpsc::Sender<ControlMessage>,
) -> Result<()> {
    let tls_config = build_device_tunnel_tls(&store)?;

    let quic_server_cfg =
        quinn::crypto::rustls::QuicServerConfig::try_from(tls_config)
            .map_err(|e| anyhow::anyhow!("QUIC server config: {}", e))?;
    let server_config = quinn::ServerConfig::with_crypto(Arc::new(quic_server_cfg));

    let socket_addr: std::net::SocketAddr = addr
        .parse()
        .map_err(|e| anyhow::anyhow!("bad QUIC addr '{}': {}", addr, e))?;
    let endpoint = quinn::Endpoint::server(server_config, socket_addr)?;

    // Register the QUIC advertise address so device_tunnel includes it in responses.
    device_tunnel::set_quic_advertise_addr(advertise_addr.to_string());
    info!("device tunnel (QUIC) listening on {} advertise={}", addr, advertise_addr);

    loop {
        let Some(incoming) = endpoint.accept().await else { break };

        let acl          = acl.clone();
        let tunnel_hub   = tunnel_hub.clone();
        let agent_reg    = agent_registry.clone();
        let crl          = crl_manager.clone();
        let conn_id      = connector_id.clone();
        let ctrl_tx      = control_tx.clone();

        tokio::spawn(async move {
            let conn = match incoming.await {
                Ok(c) => c,
                Err(e) => { warn!("QUIC connection error: {}", e); return; }
            };

            loop {
                let (send, recv) = match conn.accept_bi().await {
                    Ok(pair) => pair,
                    Err(e) => { warn!("QUIC accept_bi: {}", e); break; }
                };

                // Combine send + recv into a single bidirectional AsyncRead+AsyncWrite.
                let stream = tokio::io::join(recv, send);

                let acl        = acl.clone();
                let hub        = tunnel_hub.clone();
                let reg        = agent_reg.clone();
                let crl        = crl.clone();
                let conn_id    = conn_id.clone();
                let ctrl_tx    = ctrl_tx.clone();

                tokio::spawn(async move {
                    if let Err(e) = device_tunnel::handle_stream(
                        stream, acl, hub, reg, crl, &conn_id, &ctrl_tx,
                    ).await {
                        warn!("QUIC stream error: {}", e);
                    }
                });
            }
        });
    }

    Ok(())
}
