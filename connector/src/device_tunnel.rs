// device_tunnel.rs — OWNED BY M4 (Sprint 9 Phase C2)
//
// This file is a build stub created by M3 so quic_listener.rs can reference
// `device_tunnel::handle_stream` and `device_tunnel::set_quic_advertise_addr`
// without compile errors during M3's Phase 1 build gate.
//
// M4 replaces this with the full ACL-enforcement + routing implementation.
// DO NOT add business logic here — M4 owns this file entirely.

use std::sync::OnceLock;

use anyhow::Result;
use tokio::io::{AsyncRead, AsyncWrite};
use tokio::sync::mpsc;

use std::sync::Arc;

use crate::agent_tunnel::AgentTunnelHub;
use crate::crl::CrlManager;
use crate::policy::PolicyCache;
use crate::tls::cert_store::CertStore;
use crate::AgentRegistry;
use crate::ControlMessage;

static QUIC_ADVERTISE_ADDR: OnceLock<String> = OnceLock::new();

/// Called by quic_listener after binding the QUIC socket.
pub fn set_quic_advertise_addr(addr: String) {
    let _ = QUIC_ADVERTISE_ADDR.set(addr);
}

/// Returns the QUIC advertise address included in every TunnelResponse.
pub fn quic_advertise_addr() -> Option<&'static str> {
    QUIC_ADVERTISE_ADDR.get().map(|s| s.as_str())
}

/// Per-connection handler — stub implementation. M4 replaces with full logic.
pub async fn handle_stream<S>(
    _stream: S,
    _acl: Arc<PolicyCache>,
    _tunnel_hub: AgentTunnelHub,
    _agent_registry: Arc<AgentRegistry>,
    _crl_manager: CrlManager,
    _connector_id: &str,
    _control_tx: &mpsc::Sender<ControlMessage>,
) -> Result<()>
where
    S: AsyncRead + AsyncWrite + Unpin + Send + 'static,
{
    // M4 implements: JSON handshake → CRL check → ACL check → route decision →
    // direct copy_bidirectional OR AgentTunnelHub relay.
    anyhow::bail!("device_tunnel not yet implemented — M4 to complete")
}

/// TLS/TCP listener on :9092 — stub. M4 replaces with full implementation.
pub async fn listen(
    addr: &str,
    _store: CertStore,
    _acl: Arc<PolicyCache>,
    _tunnel_hub: AgentTunnelHub,
    _agent_registry: Arc<AgentRegistry>,
    _crl_manager: CrlManager,
    _connector_id: String,
    _control_tx: mpsc::Sender<ControlMessage>,
) -> Result<()> {
    tracing::info!("device tunnel (TLS) stub listening on {}", addr);
    // M4 implements: accept TLS connections and spawn handle_stream per conn.
    std::future::pending::<()>().await;
    Ok(())
}
