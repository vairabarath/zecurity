// device_tunnel.rs — M4 Sprint 9 Phase C2
//
// The core of the RDE: connection handler that enforces ACL and routes
// either direct or via Shield relay.

use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{anyhow, Result};
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tokio::net::{TcpStream, UdpSocket};
use tokio::sync::mpsc;

use x509_parser::prelude::*;

use crate::agent_tunnel::AgentTunnelHub;
use crate::crl::CrlManager;
use crate::policy::PolicyCache;
use crate::tls::cert_store::CertStore;
use crate::tls::server_cfg::build_device_tunnel_tls;
use crate::AgentRegistry;
use crate::ControlMessage;

const MAX_HANDSHAKE_SIZE: usize = 4096;

static QUIC_ADVERTISE_ADDR: std::sync::OnceLock<String> = std::sync::OnceLock::new();

pub fn set_quic_advertise_addr(addr: String) {
    let _ = QUIC_ADVERTISE_ADDR.set(addr);
}

pub fn quic_advertise_addr() -> Option<&'static str> {
    QUIC_ADVERTISE_ADDR.get().map(|s| s.as_str())
}

#[derive(Deserialize)]
struct TunnelRequest {
    token: String,
    destination: String,
    port: u16,
    #[serde(default = "default_tcp")]
    protocol: String,
}

fn default_tcp() -> String {
    "tcp".to_string()
}

#[derive(Serialize)]
struct TunnelResponse {
    ok: bool,
    #[serde(skip_serializing_if = "Option::is_none")]
    error: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    quic_addr: Option<String>,
}

pub async fn listen(
    addr: &str,
    store: CertStore,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    agent_registry: Arc<AgentRegistry>,
    crl_manager: CrlManager,
    connector_id: String,
    control_tx: mpsc::Sender<ControlMessage>,
) -> Result<()> {
    use tokio::net::TcpListener;
    use tokio_rustls::TlsAcceptor;
    use std::sync::Arc as StdArc;

    let tls_config = build_device_tunnel_tls(&store)?;
    let acceptor = TlsAcceptor::from(StdArc::new(tls_config));

    let listener = TcpListener::bind(addr).await?;
    tracing::info!("device tunnel (TLS) listening on {}", addr);

    loop {
        let (stream, peer_addr) = listener.accept().await?;
        let acl_clone = acl.clone();
        let hub_clone = tunnel_hub.clone();
        let reg_clone = agent_registry.clone();
        let crl_clone = crl_manager.clone();
        let conn_id_clone = connector_id.clone();
        let tx_clone = control_tx.clone();
        let acceptor_clone = acceptor.clone();

        tokio::spawn(async move {
            let tls_stream = match acceptor_clone.accept(stream).await {
                Ok(s) => s,
                Err(e) => {
                    tracing::warn!(peer = %peer_addr, error = %e, "TLS handshake failed");
                    return;
                }
            };

            let (spiffe_id, cert_serial) = {
                let certs = tls_stream.get_ref().1.peer_certificates();
                match certs.and_then(|c| c.first()) {
                    Some(der) => match extract_peer_info(der.as_ref()) {
                        Ok(info) => info,
                        Err(e) => {
                            tracing::warn!(peer = %peer_addr, error = %e, "failed to extract peer cert info");
                            return;
                        }
                    },
                    None => {
                        tracing::warn!(peer = %peer_addr, "no peer certificate after mTLS handshake");
                        return;
                    }
                }
            };

            if let Err(e) = handle_stream(
                tls_stream,
                peer_addr,
                spiffe_id,
                cert_serial,
                acl_clone,
                hub_clone,
                reg_clone,
                crl_clone,
                &conn_id_clone,
                &tx_clone,
            )
            .await
            {
                tracing::error!(peer = %peer_addr, error = %e, "connection handler error");
            }
        });
    }
}

pub async fn handle_stream<S>(
    mut stream: S,
    _peer_addr: SocketAddr,
    client_spiffe_id: String,
    cert_serial: Vec<u8>,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    agent_registry: Arc<AgentRegistry>,
    crl_manager: CrlManager,
    connector_id: &str,
    control_tx: &mpsc::Sender<ControlMessage>,
) -> Result<()>
where
    S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin + Send + 'static,
{
    if crl_manager.is_revoked(&cert_serial) {
        let response = TunnelResponse {
            ok: false,
            error: Some("certificate revoked".to_string()),
            quic_addr: quic_advertise_addr().map(String::from),
        };
        send_response(&mut stream, &response).await?;
        return Err(anyhow!("certificate revoked for spiffe_id={}", client_spiffe_id));
    }

    let mut buf = vec![0u8; MAX_HANDSHAKE_SIZE];
    let n = stream.read(&mut buf).await?;
    if n == 0 {
        return Err(anyhow!("client closed connection before sending handshake"));
    }

    let handshake = String::from_utf8(buf[..n].to_vec())
        .map_err(|_| anyhow!("handshake not valid UTF-8"))?;
    let handshake = handshake.trim();

    let req: TunnelRequest = serde_json::from_str(handshake)
        .map_err(|e| anyhow!("invalid tunnel request: {}", e))?;

    tracing::debug!(
        destination = %req.destination,
        port = req.port,
        protocol = %req.protocol,
        spiffe_id = %client_spiffe_id,
        "received tunnel request"
    );

    let decision = match acl.resolve_resource(&req.destination, req.port, &req.protocol) {
        Some(acl_entry) => {
            if !acl_entry.allowed_spiffe_ids.iter().any(|id| id == &client_spiffe_id) {
                None
            } else {
                Some(acl_entry)
            }
        }
        None => None,
    };

    if decision.is_none() {
        let response = TunnelResponse {
            ok: false,
            error: Some("access denied".to_string()),
            quic_addr: quic_advertise_addr().map(String::from),
        };
        send_response(&mut stream, &response).await?;
        emit_access_log(control_tx, connector_id, &format!("deny spiffe_id={} dest={}:{} proto={} reason=no_acl_match", client_spiffe_id, req.destination, req.port, req.protocol)).await;
        return Err(anyhow!("access denied"));
    }

    let _acl_entry = decision.unwrap();
    let shield_id = agent_registry.shield_for_host(&req.destination);

    if let Some(shield_id) = shield_id {
        let response = TunnelResponse {
            ok: true,
            error: None,
            quic_addr: quic_advertise_addr().map(String::from),
        };
        send_response(&mut stream, &response).await?;
        emit_access_log(control_tx, connector_id, &format!("allow spiffe_id={} dest={}:{} proto={} path=shield_relay shield={}", client_spiffe_id, req.destination, req.port, req.protocol, shield_id)).await;

        let relay = tunnel_hub
            .open_relay_session(&shield_id, &req.destination, req.port, &req.protocol)
            .await?;

        relay.relay_stream(stream).await?;
        return Ok(());
    }

    if req.protocol.to_lowercase() == "udp" {
        let response = TunnelResponse {
            ok: true,
            error: None,
            quic_addr: quic_advertise_addr().map(String::from),
        };
        send_response(&mut stream, &response).await?;
        emit_access_log(control_tx, connector_id, &format!("allow spiffe_id={} dest={}:{} proto={} path=direct", client_spiffe_id, req.destination, req.port, req.protocol)).await;

        relay_udp(&mut stream, &req.destination, req.port).await?;
        return Ok(());
    }

    let target = format!("{}:{}", req.destination, req.port);
    let mut resource_conn = TcpStream::connect(&target).await
        .map_err(|e| anyhow!("failed to connect to {}: {}", target, e))?;

    let response = TunnelResponse {
        ok: true,
        error: None,
        quic_addr: quic_advertise_addr().map(String::from),
    };
    send_response(&mut stream, &response).await?;
    emit_access_log(control_tx, connector_id, &format!("allow spiffe_id={} dest={}:{} proto={} path=direct", client_spiffe_id, req.destination, req.port, req.protocol)).await;

    tokio::io::copy_bidirectional(&mut stream, &mut resource_conn).await?;
    Ok(())
}

async fn relay_udp<S>(stream: &mut S, dest: &str, port: u16) -> Result<()>
where
    S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin,
{
    let target = format!("{}:{}", dest, port);
    let udp = UdpSocket::bind("0.0.0.0:0").await
        .map_err(|e| anyhow!("failed to bind UDP socket: {}", e))?;
    udp.connect(&target).await
        .map_err(|e| anyhow!("failed to connect UDP to {}: {}", target, e))?;

    let mut udp_buf = [0u8; 65535];
    let mut len_buf = [0u8; 4];

    loop {
        tokio::select! {
            result = stream.read_exact(&mut len_buf) => {
                if result.is_err() { break; }
                let len = u32::from_be_bytes(len_buf) as usize;
                if len > 65535 { break; }
                let mut buf = vec![0u8; len];
                if stream.read_exact(&mut buf).await.is_err() { break; }
                if udp.send(&buf).await.is_err() { break; }
            }
            result = udp.recv(&mut udp_buf) => {
                let n = match result { Ok(n) => n, Err(_) => break };
                let prefix = (n as u32).to_be_bytes();
                if stream.write_all(&prefix).await.is_err() { break; }
                if stream.write_all(&udp_buf[..n]).await.is_err() { break; }
                if stream.flush().await.is_err() { break; }
            }
        }
    }
    Ok(())
}

async fn send_response<S>(stream: &mut S, response: &TunnelResponse) -> Result<()>
where
    S: tokio::io::AsyncWrite + Unpin,
{
    let json = serde_json::to_string(response)?;
    stream.write_all(json.as_bytes()).await?;
    stream.flush().await?;
    Ok(())
}

async fn emit_access_log(
    control_tx: &mpsc::Sender<ControlMessage>,
    connector_id: &str,
    message: &str,
) {
    let log_msg = ControlMessage {
        body: Some(crate::proto::connector_control_message::Body::ConnectorLog(
            crate::proto::ConnectorLog {
                message: format!("[device_tunnel] {}", message),
                ..Default::default()
            },
        )),
    };
    let _ = control_tx.send(log_msg).await;
}

/// Extract (spiffe_uri, cert_serial_bytes) from a DER-encoded peer certificate.
pub fn extract_peer_info_pub(cert_der: &[u8]) -> Result<(String, Vec<u8>)> {
    extract_peer_info(cert_der)
}

fn extract_peer_info(cert_der: &[u8]) -> Result<(String, Vec<u8>)> {
    let (_, cert) = X509Certificate::from_der(cert_der)
        .map_err(|e| anyhow!("failed to parse peer certificate: {:?}", e))?;

    let serial = cert.raw_serial().to_vec();

    let san = cert
        .subject_alternative_name()
        .map_err(|e| anyhow!("failed to parse SAN: {:?}", e))?
        .ok_or_else(|| anyhow!("peer certificate has no SAN extension"))?;

    for name in &san.value.general_names {
        if let GeneralName::URI(uri) = name {
            if uri.starts_with("spiffe://") {
                return Ok((uri.to_string(), serial));
            }
        }
    }

    Err(anyhow!("peer certificate has no SPIFFE URI in SAN"))
}