// device_tunnel.rs — M4 Sprint 9 Phase C2
//
// The core of the RDE: connection handler that enforces ACL and routes
// either direct or via Shield relay.

use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, Result};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::{TcpStream, UdpSocket};
use tokio::sync::mpsc;

use x509_parser::prelude::*;

use crate::agent_tunnel::AgentTunnelHub;
use crate::crl::{CrlManager, RevocationStatus};
use crate::policy::PolicyCache;
use crate::tls::cert_store::CertStore;
use crate::tls::server_cfg::build_device_tunnel_tls;
use crate::ControlMessage;

const MAX_TUNNEL_HANDSHAKE_SIZE: usize = 16 * 1024;
pub const ERR_SHIELD_NOT_ATTACHED: &str = "SHIELD_NOT_ATTACHED";
pub const ERR_ACCESS_DENIED: &str = "ACCESS_DENIED";
pub const ERR_INTERNAL: &str = "INTERNAL";

static QUIC_ADVERTISE_ADDR: std::sync::OnceLock<String> = std::sync::OnceLock::new();

pub fn set_quic_advertise_addr(addr: String) {
    let _ = QUIC_ADVERTISE_ADDR.set(addr);
}

pub fn quic_advertise_addr() -> Option<&'static str> {
    QUIC_ADVERTISE_ADDR.get().map(|s| s.as_str())
}

#[derive(Deserialize)]
struct TunnelRequest {
    /// The resource the client is asserting it wants to reach. When present we
    /// authorize by identity and dial the ACL's own address for it. Optional only
    /// during the rollout — once every client sends it, a request without one is
    /// denied (Sprint 16 Phase 3).
    #[serde(default)]
    resource_id: Option<String>,
    /// Address the client believes the resource is at. Cross-checked against the
    /// ACL entry; never used as the dial target when `resource_id` is present.
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
    crl_manager: CrlManager,
    connector_id: String,
    control_tx: mpsc::Sender<ControlMessage>,
) -> Result<()> {
    use std::sync::Arc as StdArc;
    use tokio::net::TcpListener;
    use tokio_rustls::TlsAcceptor;

    let tls_config = build_device_tunnel_tls(&store)?;
    let acceptor = TlsAcceptor::from(StdArc::new(tls_config));

    let listener = TcpListener::bind(addr).await?;
    tracing::info!("device tunnel (TLS) listening on {}", addr);

    loop {
        let (stream, peer_addr) = listener.accept().await?;
        let acl_clone = acl.clone();
        let hub_clone = tunnel_hub.clone();
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
                spiffe_id,
                cert_serial,
                acl_clone,
                hub_clone,
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
    client_spiffe_id: String,
    cert_serial: Vec<u8>,
    acl: Arc<PolicyCache>,
    tunnel_hub: AgentTunnelHub,
    crl_manager: CrlManager,
    connector_id: &str,
    control_tx: &mpsc::Sender<ControlMessage>,
) -> Result<()>
where
    S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin + Send + 'static,
{
    match crl_manager.check(&cert_serial) {
        RevocationStatus::NotRevoked => {}
        RevocationStatus::Unavailable => {
            let response = TunnelResponse {
                ok: false,
                error: Some("certificate revocation state unavailable".to_string()),
                quic_addr: quic_advertise_addr().map(String::from),
            };
            send_response(&mut stream, &response).await?;
            return Err(anyhow!(
                "certificate revocation state unavailable for spiffe_id={}",
                client_spiffe_id
            ));
        }
        RevocationStatus::Revoked => {
            let response = TunnelResponse {
                ok: false,
                error: Some("certificate revoked".to_string()),
                quic_addr: quic_advertise_addr().map(String::from),
            };
            send_response(&mut stream, &response).await?;
            return Err(anyhow!(
                "certificate revoked for spiffe_id={}",
                client_spiffe_id
            ));
        }
    }
    let req: TunnelRequest = read_framed_json(&mut stream)
        .await
        .map_err(|e| anyhow!("invalid tunnel request: {}", e))?;

    // resource_id is mandatory (Phase 3): the connector authorizes by identity and
    // dials the ACL's address. A request without one cannot be authorized → deny.
    let asserted_resource_id = req.resource_id.as_deref().filter(|id| !id.is_empty());

    tracing::debug!(
        resource_id = asserted_resource_id.unwrap_or(""),
        destination = %req.destination,
        port = req.port,
        protocol = %req.protocol,
        spiffe_id = %client_spiffe_id,
        "received tunnel request"
    );

    // Order matters: look the resource up, THEN authorize the principal for it,
    // THEN cross-check the claimed destination. Policy lives on the resource, so
    // nothing can be authorized before it is resolved, and no dial target is
    // chosen before authorization succeeds.
    let (decision, deny_reason) = match asserted_resource_id {
        // Identity path: authorize by resource_id; the dial target comes from the ACL.
        Some(resource_id) => {
            match acl.resolve_by_resource_id(resource_id, req.port, &req.protocol) {
                None => (None, "unknown_resource"),
                Some(entry)
                    if !entry
                        .allowed_spiffe_ids
                        .iter()
                        .any(|id| id == &client_spiffe_id) =>
                {
                    (None, "unauthorized_spiffe")
                }
                // The client may state where it thinks the resource is, but it must
                // agree with the ACL. Disagreement is a denial, never a silent
                // preference for either value.
                Some(entry) if !req.destination.is_empty() && req.destination != entry.address => {
                    (None, "destination_mismatch")
                }
                Some(entry) => (Some(entry), ""),
            }
        }
        // No identity asserted → cannot be authorized. Default-deny; the legacy
        // "resolve by the client's destination string" path was removed in Phase 3.
        None => (None, "missing_resource_id"),
    };

    if decision.is_none() {
        tracing::warn!(
            spiffe_id = %client_spiffe_id,
            resource_id = asserted_resource_id.unwrap_or(""),
            dest = %req.destination,
            port = req.port,
            proto = %req.protocol,
            reason = deny_reason,
            "access denied",
        );
        let response = TunnelResponse {
            ok: false,
            error: Some("access denied".to_string()),
            quic_addr: quic_advertise_addr().map(String::from),
        };
        send_response(&mut stream, &response).await?;
        emit_access_log(
            control_tx,
            connector_id,
            AccessLogFields {
                resource_id: asserted_resource_id.unwrap_or(""),
                client_spiffe_id: &client_spiffe_id,
                route_type: "",
                destination: &req.destination,
                port: req.port,
                protocol: &req.protocol,
                action: "deny",
                error: deny_reason,
                legacy_message: format!(
                    "deny spiffe_id={} dest={}:{} proto={} reason={}",
                    client_spiffe_id, req.destination, req.port, req.protocol, deny_reason,
                ),
            },
        )
        .await;
        return Err(anyhow!("access denied"));
    }

    let acl_entry = decision.unwrap();

    if acl_entry.route_type == "shield" {
        if acl_entry.shield_id.is_empty() {
            tracing::error!(
                spiffe_id = %client_spiffe_id,
                resource_id = %acl_entry.resource_id,
                reason = "missing_shield_id",
                "access denied — shield route has no shield_id",
            );
            let response = TunnelResponse {
                ok: false,
                error: Some("shield routing configured but shield_id missing".to_string()),
                quic_addr: quic_advertise_addr().map(String::from),
            };
            send_response(&mut stream, &response).await?;
            emit_access_log(
                control_tx,
                connector_id,
                AccessLogFields {
                    resource_id:      &acl_entry.resource_id,
                    client_spiffe_id: &client_spiffe_id,
                    route_type:       "shield",
                    destination:      &req.destination,
                    port:             req.port,
                    protocol:         &req.protocol,
                    action:           "error",
                    error:            "missing_shield_id",
                    legacy_message: format!(
                        "deny spiffe_id={} resource={} dest={}:{} proto={} reason=missing_shield_id",
                        client_spiffe_id,
                        acl_entry.resource_id,
                        req.destination,
                        req.port,
                        req.protocol,
                    ),
                },
            )
            .await;
            return Err(anyhow!(
                "shield_id missing for shield-routed resource {}",
                acl_entry.resource_id
            ));
        }
        let shield_id = acl_entry.shield_id.clone();
        tracing::info!(
            spiffe_id = %client_spiffe_id,
            resource_id = %acl_entry.resource_id,
            dest = %req.destination,
            port = req.port,
            proto = %req.protocol,
            route = "shield",
            shield = %shield_id,
            "access allowed",
        );
        emit_access_log(
            control_tx,
            connector_id,
            AccessLogFields {
                resource_id: &acl_entry.resource_id,
                client_spiffe_id: &client_spiffe_id,
                route_type: "shield",
                destination: &req.destination,
                port: req.port,
                protocol: &req.protocol,
                action: "allow",
                error: "",
                legacy_message: format!(
                    "allow spiffe_id={} resource={} dest={}:{} proto={} route=shield shield={}",
                    client_spiffe_id,
                    acl_entry.resource_id,
                    req.destination,
                    req.port,
                    req.protocol,
                    shield_id,
                ),
            },
        )
        .await;

        // Dial target comes from the ACL entry, never from req.destination.
        match tunnel_hub
            .open_relay_session(&shield_id, &acl_entry.address, req.port, &req.protocol)
            .await
        {
            Ok(relay) => {
                tracing::info!(shield = %shield_id, resource_id = %acl_entry.resource_id, "tunnel_opened ok");

                // Only acknowledge success after the relay session is ready.
                let response = TunnelResponse {
                    ok: true,
                    error: None,
                    quic_addr: quic_advertise_addr().map(String::from),
                };
                send_response(&mut stream, &response).await?;
                relay.relay_stream(stream).await?;
            }
            Err(e) => {
                tracing::error!(shield = %shield_id,
                    resource_id = %acl_entry.resource_id,
                    error = %e,
                    "tunnel_opened error"
                );
                let response = if e.to_string().contains("not connected") {
                    TunnelResponse {
                        ok: false,
                        error: Some(ERR_SHIELD_NOT_ATTACHED.to_string()),
                        quic_addr: quic_advertise_addr().map(String::from),
                    }
                } else {
                    TunnelResponse {
                        ok: false,
                        error: Some("INTERNAL".to_string()),
                        quic_addr: quic_advertise_addr().map(String::from),
                    }
                };

                let _ = send_response(&mut stream, &response).await;
                return Err(e);
            }
        }
        return Ok(());
    }

    // Connector route — direct TCP/UDP bridge from the connector to the
    // resource. `"direct"` is kept as a temporary legacy alias for older
    // ACL snapshots; new compilations emit `"connector"`.
    if acl_entry.route_type != "connector" && acl_entry.route_type != "direct" {
        tracing::error!(
            spiffe_id = %client_spiffe_id,
            resource_id = %acl_entry.resource_id,
            route_type = %acl_entry.route_type,
            "access denied — unknown route_type",
        );
        let response = TunnelResponse {
            ok: false,
            error: Some(format!("unknown route_type {:?}", acl_entry.route_type)),
            quic_addr: quic_advertise_addr().map(String::from),
        };
        send_response(&mut stream, &response).await?;
        emit_access_log(
            control_tx,
            connector_id,
            AccessLogFields {
                resource_id:      &acl_entry.resource_id,
                client_spiffe_id: &client_spiffe_id,
                route_type:       &acl_entry.route_type,
                destination:      &req.destination,
                port:             req.port,
                protocol:         &req.protocol,
                action:           "error",
                error:            "unknown_route_type",
                legacy_message: format!(
                    "deny spiffe_id={} resource={} dest={}:{} proto={} reason=unknown_route_type={}",
                    client_spiffe_id,
                    acl_entry.resource_id,
                    req.destination,
                    req.port,
                    req.protocol,
                    acl_entry.route_type,
                ),
            },
        )
        .await;
        return Err(anyhow!(
            "unknown route_type {:?} for resource {}",
            acl_entry.route_type,
            acl_entry.resource_id
        ));
    }

    tracing::info!(
        spiffe_id = %client_spiffe_id,
        resource_id = %acl_entry.resource_id,
        dest = %req.destination,
        port = req.port,
        proto = %req.protocol,
        route = "connector",
        "access allowed",
    );

    if req.protocol.to_lowercase() == "udp" {
        let response = TunnelResponse {
            ok: true,
            error: None,
            quic_addr: quic_advertise_addr().map(String::from),
        };
        send_response(&mut stream, &response).await?;
        emit_access_log(
            control_tx,
            connector_id,
            AccessLogFields {
                resource_id: &acl_entry.resource_id,
                client_spiffe_id: &client_spiffe_id,
                route_type: "connector",
                destination: &req.destination,
                port: req.port,
                protocol: &req.protocol,
                action: "allow",
                error: "",
                legacy_message: format!(
                    "allow spiffe_id={} resource={} dest={}:{} proto={} route=connector",
                    client_spiffe_id,
                    acl_entry.resource_id,
                    req.destination,
                    req.port,
                    req.protocol,
                ),
            },
        )
        .await;

        // Dial target comes from the ACL entry, never from req.destination.
        relay_udp(&mut stream, &acl_entry.address, req.port).await?;
        return Ok(());
    }

    // Dial target comes from the ACL entry, never from req.destination.
    let target = format!("{}:{}", acl_entry.address, req.port);
    let mut resource_conn = match TcpStream::connect(&target).await {
        Ok(c) => {
            tracing::info!(resource_id = %acl_entry.resource_id, dest = %target, "tunnel_opened ok");
            c
        }
        Err(e) => {
            tracing::error!(resource_id = %acl_entry.resource_id, dest = %target, error = %e, "tunnel_opened error");
            // Resource unreachable from the connector — audit as an `error`
            // action rather than a `deny` (which is reserved for policy denial).
            emit_access_log(
                control_tx,
                connector_id,
                AccessLogFields {
                    resource_id:      &acl_entry.resource_id,
                    client_spiffe_id: &client_spiffe_id,
                    route_type:       "connector",
                    destination:      &req.destination,
                    port:             req.port,
                    protocol:         &req.protocol,
                    action:           "error",
                    error:            &format!("connect_failed: {}", e),
                    legacy_message: format!(
                        "error spiffe_id={} resource={} dest={}:{} proto={} reason=connect_failed: {}",
                        client_spiffe_id, acl_entry.resource_id, req.destination, req.port, req.protocol, e,
                    ),
                },
            )
            .await;
            return Err(anyhow!("failed to connect to {}: {}", target, e));
        }
    };

    let response = TunnelResponse {
        ok: true,
        error: None,
        quic_addr: quic_advertise_addr().map(String::from),
    };
    send_response(&mut stream, &response).await?;
    emit_access_log(
        control_tx,
        connector_id,
        AccessLogFields {
            resource_id: &acl_entry.resource_id,
            client_spiffe_id: &client_spiffe_id,
            route_type: "connector",
            destination: &req.destination,
            port: req.port,
            protocol: &req.protocol,
            action: "allow",
            error: "",
            legacy_message: format!(
                "allow spiffe_id={} resource={} dest={}:{} proto={} route=connector",
                client_spiffe_id, acl_entry.resource_id, req.destination, req.port, req.protocol,
            ),
        },
    )
    .await;

    tokio::io::copy_bidirectional(&mut stream, &mut resource_conn).await?;
    Ok(())
}

async fn relay_udp<S>(stream: &mut S, dest: &str, port: u16) -> Result<()>
where
    S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin,
{
    let target = format!("{}:{}", dest, port);
    let udp = UdpSocket::bind("0.0.0.0:0")
        .await
        .map_err(|e| anyhow!("failed to bind UDP socket: {}", e))?;
    udp.connect(&target)
        .await
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
    write_framed_json(stream, response).await
}

async fn write_framed_json<W, T>(writer: &mut W, value: &T) -> Result<()>
where
    W: AsyncWrite + Unpin,
    T: Serialize,
{
    let body = serde_json::to_vec(value)?;
    if body.len() > MAX_TUNNEL_HANDSHAKE_SIZE {
        return Err(anyhow!("tunnel handshake too large: {} bytes", body.len()));
    }

    writer.write_all(&(body.len() as u32).to_be_bytes()).await?;
    writer.write_all(&body).await?;
    writer.flush().await?;
    Ok(())
}

async fn read_framed_json<R, T>(reader: &mut R) -> Result<T>
where
    R: AsyncRead + Unpin,
    T: DeserializeOwned,
{
    let mut length = [0u8; 4];
    reader.read_exact(&mut length).await?;
    let length = u32::from_be_bytes(length) as usize;
    if length > MAX_TUNNEL_HANDSHAKE_SIZE {
        return Err(anyhow!("tunnel handshake too large: {length} bytes"));
    }

    let mut body = vec![0u8; length];
    reader.read_exact(&mut body).await?;
    serde_json::from_slice(&body).map_err(Into::into)
}

/// Typed access-log fields the connector forwards to the controller. Mirrors
/// the structured columns in connector_logs added by migration 021.
struct AccessLogFields<'a> {
    resource_id: &'a str,
    client_spiffe_id: &'a str,
    route_type: &'a str,
    destination: &'a str,
    port: u16,
    protocol: &'a str,
    action: &'a str, // "allow" | "deny" | "error"
    error: &'a str,
    legacy_message: String,
}

async fn emit_access_log<'a>(
    control_tx: &mpsc::Sender<ControlMessage>,
    _connector_id: &str,
    fields: AccessLogFields<'a>,
) {
    let occurred_at = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0);

    let log_msg = ControlMessage {
        body: Some(crate::proto::connector_control_message::Body::ConnectorLog(
            crate::proto::ConnectorLog {
                message: format!("[device_tunnel] {}", fields.legacy_message),
                resource_id: fields.resource_id.to_string(),
                client_spiffe_id: fields.client_spiffe_id.to_string(),
                route_type: fields.route_type.to_string(),
                destination: fields.destination.to_string(),
                port: fields.port as u32,
                protocol: fields.protocol.to_string(),
                action: fields.action.to_string(),
                error: fields.error.to_string(),
                occurred_at_unix: occurred_at,
                ..Default::default()
            },
        )),
    };
    // Never block the data plane on audit logging. This runs immediately before
    // copy_bidirectional, so a blocking send on a full mailbox stops the tunnel's
    // byte pump entirely: the connector stops reading, QUIC flow control blocks the
    // client's write, its flow queue fills, and the connection dies with no data
    // transferred. A full mailbox means the control stream is wedged or slow —
    // drop the log line instead. Mirrors connectorStreamClient::send's fail-fast
    // contract ("a wedged connector can't stall a GraphQL resolver").
    if control_tx.try_send(log_msg).is_err() {
        tracing::warn!("control mailbox full — dropping access log entry");
    }
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
