// device_tunnel.rs — M4 Sprint 9 Phase C2
//
// The core of the RDE: connection handler that enforces ACL and routes
// either direct or via Shield relay.

use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::{anyhow, Result};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use std::net::SocketAddr;
use std::os::unix::io::AsRawFd;
use tokio::io::{AsyncRead, AsyncReadExt, AsyncWrite, AsyncWriteExt};
use tokio::net::{TcpSocket, TcpStream, UdpSocket};
use tokio::sync::mpsc;

use x509_parser::prelude::*;

use crate::agent_tunnel::AgentTunnelHub;
use crate::crl::{CrlManager, RevocationStatus};
use crate::policy::{Addressing, PolicyCache};
use crate::resolver::Resolver;
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
    resolver: Arc<Resolver>,
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
        let resolver_clone = resolver.clone();

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
                resolver_clone,
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
    resolver: Arc<Resolver>,
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
                //
                // Sprint 16 Phase 7.0: only meaningful when the ACL HAS a pinned
                // address to compare against. A name-addressed resource has none —
                // the client's synthetic IP is client-local and deliberately
                // meaningless here, so it sends `destination` empty (Phase 9.4).
                // The check stays fully strict for every IP-pinned resource, which
                // is every resource that exists today. Do NOT delete it: that
                // reopens the confused-deputy surface Stage 1 closed.
                Some(entry)
                    if !entry.address.is_empty()
                        && !req.destination.is_empty()
                        && req.destination != entry.address =>
                {
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

    // ── Sprint 16 Phase 7.1 — determine the dial target ─────────────────────
    //
    // Reached ONLY on the connector branch: the shield arm returned above, and
    // route_type has been validated. A shield-delivered resource is never resolved
    // — the Shield IS the endpoint. Never branch on `resolver.type` out here;
    // delivery (route_type, field 7) and resolution (resolver.type, field 12) are
    // orthogonal axes, and collapsing them would make a Protected resource
    // directly dialable (invariant #3).
    //
    // Invariant #2 holds structurally: authorization already succeeded above, so no
    // DNS query is ever issued for an unauthorized request.
    let (dial_ip, resource_hostname, from_stale) = match acl_entry.addressing() {
        Addressing::Pinned(address) => (address, String::new(), false),

        Addressing::Named {
            hostname,
            resolver: spec,
        } => {
            let started = std::time::Instant::now();
            match resolver
                .resolve(&acl_entry.remote_network_id, &hostname, &spec)
                .await
            {
                Ok(resolved) => {
                    // Phase 7.2 — resolution latency and cache hit/miss, so the TTL
                    // clamp can be tuned from observed data rather than guesswork.
                    // `debug` rather than `info`: this fires on every connection to a
                    // named resource, and the access log above is the audit record.
                    // The crate has no metrics facility, so structured logs are the
                    // only sink available today.
                    tracing::debug!(
                        resource_id = %acl_entry.resource_id,
                        hostname = %hostname,
                        resolved = %resolved.address,
                        cache_hit = resolved.cache_hit,
                        stale = resolved.stale,
                        resolve_us = started.elapsed().as_micros(),
                        "resolved resource endpoint",
                    );
                    (resolved.address.to_string(), hostname, resolved.stale)
                }
                Err(e) => {
                    // Unresolvable endpoint → deny (invariant #7). The reason is the
                    // resolver's own typed token, so nxdomain / resolver_unavailable /
                    // resolver_failure / no_address_record stay distinguishable — and
                    // stay distinct from a dial failure below, which means resolution
                    // SUCCEEDED and the resource is down.
                    tracing::warn!(
                        spiffe_id = %client_spiffe_id,
                        resource_id = %acl_entry.resource_id,
                        hostname = %hostname,
                        reason = e.reason(),
                        "access denied — could not resolve resource endpoint",
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
                            resource_id: &acl_entry.resource_id,
                            client_spiffe_id: &client_spiffe_id,
                            route_type: "connector",
                            destination: &req.destination,
                            port: req.port,
                            protocol: &req.protocol,
                            action: "deny",
                            error: e.reason(),
                            legacy_message: format!(
                                "deny spiffe_id={} resource={} hostname={} port={} proto={} reason={}",
                                client_spiffe_id,
                                acl_entry.resource_id,
                                hostname,
                                req.port,
                                req.protocol,
                                e.reason(),
                            ),
                        },
                    )
                    .await;
                    return Err(anyhow!(
                        "could not resolve {} for resource {}: {}",
                        hostname,
                        acl_entry.resource_id,
                        e.reason()
                    ));
                }
            }
        }

        // A self-contradictory or unaddressable ACL entry: both `address` and
        // `hostname` set, neither set, or a hostname with no usable resolver.
        // Fail closed — nothing here is entitled to pick a winner.
        Addressing::Invalid(reason) => {
            tracing::error!(
                spiffe_id = %client_spiffe_id,
                resource_id = %acl_entry.resource_id,
                reason,
                "access denied — resource is not addressable",
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
                    resource_id: &acl_entry.resource_id,
                    client_spiffe_id: &client_spiffe_id,
                    route_type: "connector",
                    destination: &req.destination,
                    port: req.port,
                    protocol: &req.protocol,
                    action: "deny",
                    error: reason,
                    legacy_message: format!(
                        "deny spiffe_id={} resource={} port={} proto={} reason={}",
                        client_spiffe_id, acl_entry.resource_id, req.port, req.protocol, reason,
                    ),
                },
            )
            .await;
            return Err(anyhow!(
                "resource {} is not addressable: {}",
                acl_entry.resource_id,
                reason
            ));
        }
    };

    // Phase 7.2 — `resource_id` is the identity; `hostname` and the resolved address
    // are observations. Never a synthetic IP: those are client-local and meaningless
    // to the connector (7.0 stops us even comparing against one).
    tracing::info!(
        spiffe_id = %client_spiffe_id,
        resource_id = %acl_entry.resource_id,
        hostname = %resource_hostname,
        dest = %dial_ip,
        stale = from_stale,
        port = req.port,
        proto = %req.protocol,
        route = "connector",
        "access allowed",
    );
    if from_stale {
        tracing::warn!(
            resource_id = %acl_entry.resource_id,
            hostname = %resource_hostname,
            dest = %dial_ip,
            "dialing a last-known-good address — resolver is unreachable",
        );
    }

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

        // Dial target is the RESOLVED address, never req.destination.
        relay_udp(&mut stream, &dial_ip, req.port).await?;
        return Ok(());
    }

    // Dial target is the RESOLVED address, never req.destination.
    let target = format!("{}:{}", dial_ip, req.port);
    let mut resource_conn = match connect_marked_tcp(&target).await {
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

/// Stamp `CONNECTOR_EGRESS_MARK` on a socket so a client running on the SAME host
/// does not capture our egress with its own nft interception rules (which would
/// create a routing loop: connector → resource → back into that client's TUN).
///
/// Best-effort: needs CAP_NET_ADMIN (the unit grants it via AmbientCapabilities).
/// If it fails we log once and continue — the mark only matters in the co-located
/// topology, and in the normal split-host deployment its absence is harmless.
fn set_egress_mark<F: AsRawFd>(sock: &F, what: &str) {
    let mark: libc::c_int = crate::appmeta::CONNECTOR_EGRESS_MARK as libc::c_int;
    // SAFETY: fd is owned by `sock` and outlives this call; we pass a valid
    // pointer/length pair for a c_int option value.
    let rc = unsafe {
        libc::setsockopt(
            sock.as_raw_fd(),
            libc::SOL_SOCKET,
            libc::SO_MARK,
            &mark as *const _ as *const libc::c_void,
            std::mem::size_of::<libc::c_int>() as libc::socklen_t,
        )
    };
    if rc != 0 {
        tracing::warn!(
            what,
            error = %std::io::Error::last_os_error(),
            "could not set SO_MARK on egress socket (needs CAP_NET_ADMIN); \
             a client co-located on this host may loop its own traffic"
        );
    }
}

/// TCP connect to a resource with the egress mark applied *before* connecting, so
/// the very first SYN already carries it.
async fn connect_marked_tcp(target: &str) -> std::io::Result<TcpStream> {
    let addr: SocketAddr = match target.parse() {
        Ok(a) => a,
        // Not a literal socket address (future FQDN resources): fall back to the
        // plain resolver path. The mark is skipped, which is safe — see above.
        Err(_) => return TcpStream::connect(target).await,
    };
    let socket = match addr {
        SocketAddr::V4(_) => TcpSocket::new_v4()?,
        SocketAddr::V6(_) => TcpSocket::new_v6()?,
    };
    set_egress_mark(&socket, "tcp");
    socket.connect(addr).await
}

async fn relay_udp<S>(stream: &mut S, dest: &str, port: u16) -> Result<()>
where
    S: tokio::io::AsyncRead + tokio::io::AsyncWrite + Unpin,
{
    let target = format!("{}:{}", dest, port);
    let udp = UdpSocket::bind("0.0.0.0:0")
        .await
        .map_err(|e| anyhow!("failed to bind UDP socket: {}", e))?;
    set_egress_mark(&udp, "udp");
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

#[cfg(test)]
mod tests {
    use super::*;
    use crate::client::v1::{AclEntry, AclResolver, AclSnapshot};
    use crate::proto::connector_control_message::Body;
    use crate::resolver::{DnsBackend, Resolver, UnavailableBackend};
    use tokio::net::TcpListener;

    const SPIFFE: &str = "spiffe://ws.test/client/device-1";
    const OTHER_SPIFFE: &str = "spiffe://ws.test/client/device-OTHER";

    // ── harness ──────────────────────────────────────────────────────────────

    /// Write a `TunnelRequest` in the handler's own wire format: a 4-byte
    /// big-endian length followed by the JSON body (see `read_framed_json`).
    async fn write_request<W: AsyncWrite + Unpin>(w: &mut W, req: serde_json::Value) {
        let body = serde_json::to_vec(&req).unwrap();
        w.write_all(&(body.len() as u32).to_be_bytes())
            .await
            .unwrap();
        w.write_all(&body).await.unwrap();
        w.flush().await.unwrap();
    }

    /// `TunnelResponse` is `Serialize`-only, so read it back as a generic value.
    /// `None` means the handler closed without responding — which is what the
    /// dial-failure path does.
    async fn read_response<R: AsyncRead + Unpin>(r: &mut R) -> Option<serde_json::Value> {
        let mut len = [0u8; 4];
        r.read_exact(&mut len).await.ok()?;
        let mut body = vec![0u8; u32::from_be_bytes(len) as usize];
        r.read_exact(&mut body).await.ok()?;
        serde_json::from_slice(&body).ok()
    }

    fn snapshot(entry: AclEntry) -> AclSnapshot {
        AclSnapshot {
            version: 1,
            workspace_id: "ws-test".into(),
            generated_at: 0,
            entries: vec![entry],
            ..Default::default()
        }
    }

    /// Classic IP-pinned entry: `address` set, `hostname` empty.
    fn pinned_entry(address: &str, port: u16) -> AclEntry {
        AclEntry {
            resource_id: "res-1".into(),
            name: "res-1".into(),
            remote_network_id: "rn-1".into(),
            address: address.into(),
            port: port as u32,
            protocol: "tcp".into(),
            allowed_spiffe_ids: vec![SPIFFE.to_string()],
            route_type: "connector".into(),
            ..Default::default()
        }
    }

    /// Name-addressed entry: `address` empty, `hostname` + resolver set.
    fn named_entry(hostname: &str, port: u16, resolver: Option<AclResolver>) -> AclEntry {
        AclEntry {
            hostname: hostname.into(),
            resolver,
            ..pinned_entry("", port)
        }
    }

    fn static_resolver(address: &str) -> AclResolver {
        let mut config = std::collections::HashMap::new();
        config.insert("address".to_string(), address.to_string());
        AclResolver {
            r#type: "static".into(),
            config,
        }
    }

    fn dns_resolver() -> AclResolver {
        AclResolver {
            r#type: "dns".into(),
            config: Default::default(),
        }
    }

    /// A loopback port with nothing listening: bind, note the port, drop the
    /// listener. Gives a deterministic ECONNREFUSED instead of guessing a port.
    async fn closed_port() -> u16 {
        let l = TcpListener::bind("127.0.0.1:0").await.unwrap();
        l.local_addr().unwrap().port()
    }

    struct Outcome {
        result: Result<()>,
        response: Option<serde_json::Value>,
        logs: Vec<crate::proto::ConnectorLog>,
    }

    impl Outcome {
        fn log(&self) -> &crate::proto::ConnectorLog {
            self.logs.last().expect("expected at least one access log")
        }

        /// Borrow the error text instead of `unwrap_err()`, which would partially
        /// move `Outcome` and block any later `log()` access in the same test.
        fn err_text(&self) -> String {
            match &self.result {
                Err(e) => e.to_string(),
                Ok(()) => panic!("expected handle_stream to fail"),
            }
        }

        fn response(&self) -> &serde_json::Value {
            self.response
                .as_ref()
                .expect("expected the handler to answer the client")
        }
    }

    async fn run_with(
        entry: AclEntry,
        backend: Arc<dyn DnsBackend>,
        spiffe: &str,
        req: serde_json::Value,
    ) -> Outcome {
        let acl = Arc::new(PolicyCache::new());
        acl.update(snapshot(entry));

        // A fresh CrlManager reports `Unavailable` — fail-closed by construction —
        // which would deny before authorization is ever reached. Install a valid,
        // empty CRL so these tests exercise the policy path, not the CRL gate.
        let crl = CrlManager::new();
        crl.install_test_cache(vec![]);

        let (tx, mut rx) = mpsc::channel::<ControlMessage>(16);
        let (mut client, server) = tokio::io::duplex(8192);
        let resolver = Arc::new(Resolver::new(backend));
        let spiffe = spiffe.to_string();

        let handle = tokio::spawn(async move {
            handle_stream(
                server,
                spiffe,
                vec![1, 2, 3],
                acl,
                AgentTunnelHub::new(),
                crl,
                "connector-test",
                &tx,
                resolver,
            )
            .await
        });

        write_request(&mut client, req).await;
        let response = read_response(&mut client).await;
        let result = handle.await.unwrap();

        let mut logs = Vec::new();
        while let Ok(msg) = rx.try_recv() {
            if let Some(Body::ConnectorLog(log)) = msg.body {
                logs.push(log);
            }
        }
        Outcome {
            result,
            response,
            logs,
        }
    }

    /// Most cases need no DNS at all: `static` resolves without I/O, and
    /// `UnavailableBackend` gives a deterministic resolver failure.
    async fn run(entry: AclEntry, req: serde_json::Value) -> Outcome {
        run_with(entry, Arc::new(UnavailableBackend), SPIFFE, req).await
    }

    // ── 7.0 — the destination cross-check ────────────────────────────────────

    /// Gate 1's outstanding negative case, and the guard for task 7.0: scoping the
    /// check to `!entry.address.is_empty()` must NOT weaken it for pinned resources.
    /// If someone deletes the check instead of scoping it, this fails.
    #[tokio::test]
    async fn destination_mismatch_is_denied_for_pinned_resources() {
        let out = run(
            pinned_entry("10.0.0.1", 443),
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "10.9.9.9",
                "port": 443,
                "protocol": "tcp"
            }),
        )
        .await;

        assert!(out.result.is_err());
        assert_eq!(out.log().action, "deny");
        assert_eq!(out.log().error, "destination_mismatch");
    }

    /// **This is the actual guard for task 7.0.** A named resource has no pinned
    /// address, so there is nothing for a non-empty `destination` to agree with.
    ///
    /// Without the `!entry.address.is_empty()` scoping, `entry.address` is `""`, the
    /// client's value is non-empty and differs, so the arm fires and the resource is
    /// denied. This is the case that matters if Phase 9 ever sends the synthetic IP
    /// rather than an empty string — which is the only reason the scoping exists.
    #[tokio::test]
    async fn named_resource_is_not_denied_for_a_non_empty_destination() {
        let port = closed_port().await;
        let out = run(
            named_entry("app.internal", port, Some(static_resolver("127.0.0.1"))),
            serde_json::json!({
                "resource_id": "res-1",
                // A client-local synthetic IP: meaningless to the connector, and
                // deliberately not comparable to anything in the ACL.
                "destination": "100.64.0.7",
                "port": port,
                "protocol": "tcp"
            }),
        )
        .await;

        assert_ne!(
            out.log().error,
            "destination_mismatch",
            "a named resource has no pinned address to cross-check against"
        );
        assert!(out.err_text().contains(&format!("127.0.0.1:{port}")));
    }

    /// The empty-destination case Phase 9.4 actually specifies. Note this passes
    /// with or without 7.0's scoping — the original arm was already inert for an
    /// empty `destination`. Kept as a contract test for 9.4, not as a 7.0 guard.
    #[tokio::test]
    async fn named_resource_with_empty_destination_is_not_denied() {
        let port = closed_port().await;
        let out = run(
            named_entry("app.internal", port, Some(static_resolver("127.0.0.1"))),
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "",
                "port": port,
                "protocol": "tcp"
            }),
        )
        .await;

        assert_ne!(
            out.log().error,
            "destination_mismatch",
            "a named resource must not be denied for having no pinned address"
        );
        assert_eq!(
            out.log().action,
            "error",
            "should reach the dial, not a denial"
        );
    }

    // ── 7.1 — the delivery branch ────────────────────────────────────────────

    /// Proves the connector dials the RESOLVED address. The returned error carries
    /// the dial target, which is the only place it is observable — the audit log
    /// deliberately does not gain a `resolved` wire field (that would need a
    /// ConnectorLog proto change).
    #[tokio::test]
    async fn named_resource_dials_the_resolved_address() {
        let port = closed_port().await;
        let out = run(
            named_entry("app.internal", port, Some(static_resolver("127.0.0.1"))),
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "",
                "port": port,
                "protocol": "tcp"
            }),
        )
        .await;

        let err = out.err_text();
        assert!(
            err.contains(&format!("127.0.0.1:{port}")),
            "expected the resolved address as the dial target, got: {err}"
        );
        assert!(out.log().error.starts_with("connect_failed"));
    }

    /// Regression: a pinned resource still dials its own ACL address, unchanged.
    #[tokio::test]
    async fn pinned_resource_still_dials_the_acl_address() {
        let port = closed_port().await;
        let out = run(
            pinned_entry("127.0.0.1", port),
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "127.0.0.1",
                "port": port,
                "protocol": "tcp"
            }),
        )
        .await;

        let err = out.err_text();
        assert!(err.contains(&format!("127.0.0.1:{port}")), "got: {err}");
    }

    /// A resolver failure is a DENIAL with the resolver's own typed reason — and it
    /// must stay distinct from `connect_failed`, which means resolution SUCCEEDED
    /// and the resource is down. Conflating them sends operators to the wrong system.
    #[tokio::test]
    async fn resolver_failure_denies_with_the_typed_reason() {
        let out = run(
            named_entry("app.internal", 443, Some(dns_resolver())),
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "",
                "port": 443,
                "protocol": "tcp"
            }),
        )
        .await;

        assert!(out.result.is_err());
        assert_eq!(out.log().action, "deny");
        assert_eq!(out.log().error, "resolver_unavailable");
        assert_ne!(out.log().error, "connect_failed");
    }

    /// A hostname with no resolver is unusable. `parseResolver` degrades to nil on
    /// malformed JSON precisely so the blast radius lands here, on one resource.
    #[tokio::test]
    async fn named_resource_without_a_resolver_is_denied() {
        let out = run(
            named_entry("app.internal", 443, None),
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "",
                "port": 443,
                "protocol": "tcp"
            }),
        )
        .await;

        assert!(out.result.is_err());
        assert_eq!(out.log().action, "deny");
        assert_eq!(out.log().error, "missing_resolver");
    }

    /// `"direct"` is a legacy alias for `"connector"` kept for older ACL snapshots.
    /// It reaches the same Phase 7.1 addressing code, so a future tightening of the
    /// route_type check would silently break named resources on old snapshots.
    #[tokio::test]
    async fn legacy_direct_route_type_still_resolves() {
        let port = closed_port().await;
        let entry = AclEntry {
            route_type: "direct".into(),
            ..named_entry("app.internal", port, Some(static_resolver("127.0.0.1")))
        };
        let out = run(
            entry,
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "",
                "port": port,
                "protocol": "tcp"
            }),
        )
        .await;

        assert!(
            out.err_text().contains(&format!("127.0.0.1:{port}")),
            "legacy alias must resolve and dial like \"connector\": {}",
            out.err_text()
        );
    }

    /// Verify item: "unknown resolver type → deny". `addressing()` accepts it (the
    /// type string is non-empty), so the rejection happens one layer down in the
    /// resolver and must surface as its own reason — never as a default to DNS.
    #[tokio::test]
    async fn unknown_resolver_type_is_denied() {
        let mut config = std::collections::HashMap::new();
        config.insert("address".to_string(), "127.0.0.1".to_string());
        let bogus = AclResolver {
            r#type: "k8s".into(), // interface reserved for later; not implemented
            config,
        };
        let out = run(
            named_entry("app.internal", 443, Some(bogus)),
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "",
                "port": 443,
                "protocol": "tcp"
            }),
        )
        .await;

        assert!(out.result.is_err());
        assert_eq!(out.log().action, "deny");
        assert_eq!(out.log().error, "unsupported_resolver");
    }

    /// Both `address` and `hostname` set. Exactly-one is enforced only at the
    /// GraphQL layer, so a SQL-inserted row can carry both — fail closed rather
    /// than silently preferring either value.
    #[tokio::test]
    async fn ambiguous_addressing_is_denied() {
        let entry = AclEntry {
            hostname: "app.internal".into(),
            resolver: Some(static_resolver("127.0.0.1")),
            ..pinned_entry("10.0.0.1", 443)
        };
        let out = run(
            entry,
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "10.0.0.1",
                "port": 443,
                "protocol": "tcp"
            }),
        )
        .await;

        assert!(out.result.is_err());
        assert_eq!(out.log().action, "deny");
        assert_eq!(out.log().error, "ambiguous_addressing");
    }

    /// Invariant #3. A shield-delivered resource is NEVER resolved — the Shield is
    /// the endpoint. With no shield attached it must fail closed as
    /// SHIELD_NOT_ATTACHED, never fall through to a direct dial, and never surface
    /// a resolver error (which would prove the resolver ran).
    #[tokio::test]
    async fn shield_route_is_never_resolved_and_fails_closed() {
        let entry = AclEntry {
            route_type: "shield".into(),
            shield_id: "shield-1".into(),
            hostname: "app.internal".into(),
            resolver: Some(dns_resolver()),
            ..pinned_entry("", 443)
        };
        let out = run(
            entry,
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "",
                "port": 443,
                "protocol": "tcp"
            }),
        )
        .await;

        assert!(out.result.is_err());
        assert_eq!(out.response()["error"], ERR_SHIELD_NOT_ATTACHED);
        assert_eq!(out.log().route_type, "shield");
        assert_ne!(
            out.log().error,
            "resolver_unavailable",
            "a shield route must not consult the resolver"
        );
    }

    // ── unchanged authorization paths (regression) ───────────────────────────

    #[tokio::test]
    async fn unauthorized_spiffe_is_denied() {
        let out = run_with(
            pinned_entry("10.0.0.1", 443),
            Arc::new(UnavailableBackend),
            OTHER_SPIFFE,
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "10.0.0.1",
                "port": 443,
                "protocol": "tcp"
            }),
        )
        .await;

        assert!(out.result.is_err());
        assert_eq!(out.log().error, "unauthorized_spiffe");
    }

    #[tokio::test]
    async fn missing_resource_id_is_denied() {
        let out = run(
            pinned_entry("10.0.0.1", 443),
            serde_json::json!({ "destination": "10.0.0.1", "port": 443, "protocol": "tcp" }),
        )
        .await;

        assert!(out.result.is_err());
        assert_eq!(out.log().error, "missing_resource_id");
    }

    /// Port and protocol are part of the identity lookup: a resource authorized on
    /// :443 is not reachable on :22. Reported as `unknown_resource`, deliberately
    /// indistinguishable from "no such id" so a caller cannot probe which resource
    /// ids exist by comparing error strings.
    #[tokio::test]
    async fn wrong_port_is_denied_as_unknown_resource() {
        let out = run(
            pinned_entry("10.0.0.1", 443),
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "10.0.0.1",
                "port": 22,
                "protocol": "tcp"
            }),
        )
        .await;

        assert!(out.result.is_err());
        assert_eq!(out.log().error, "unknown_resource");
        assert_eq!(out.log().action, "deny");
    }

    #[tokio::test]
    async fn unknown_resource_id_is_denied_without_dialing() {
        let out = run(
            pinned_entry("10.0.0.1", 443),
            serde_json::json!({
                "resource_id": "res-does-not-exist",
                "destination": "10.0.0.1",
                "port": 443,
                "protocol": "tcp"
            }),
        )
        .await;

        assert!(out.result.is_err());
        assert_eq!(out.log().error, "unknown_resource");
        assert_eq!(out.log().action, "deny", "no dial may be attempted");
    }

    // ── end-to-end over a real socket ────────────────────────────────────────

    /// The only test that proves bytes actually traverse a RESOLVED endpoint:
    /// a real listener, reached via a `static` resolver rather than a pinned address.
    #[tokio::test]
    async fn named_resource_relays_bytes_to_the_resolved_backend() {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();

        // Backend: echo one payload back, uppercased, so the response is provably ours.
        tokio::spawn(async move {
            let (mut sock, _) = listener.accept().await.unwrap();
            let mut buf = [0u8; 16];
            let n = sock.read(&mut buf).await.unwrap();
            let reply = String::from_utf8_lossy(&buf[..n]).to_uppercase();
            sock.write_all(reply.as_bytes()).await.unwrap();
            sock.flush().await.unwrap();
        });

        let acl = Arc::new(PolicyCache::new());
        acl.update(snapshot(named_entry(
            "app.internal",
            port,
            Some(static_resolver("127.0.0.1")),
        )));
        let crl = CrlManager::new();
        crl.install_test_cache(vec![]);
        let (tx, _rx) = mpsc::channel::<ControlMessage>(16);
        let (mut client, server) = tokio::io::duplex(8192);
        let resolver = Arc::new(Resolver::new(Arc::new(UnavailableBackend)));

        tokio::spawn(async move {
            let _ = handle_stream(
                server,
                SPIFFE.to_string(),
                vec![1, 2, 3],
                acl,
                AgentTunnelHub::new(),
                crl,
                "connector-test",
                &tx,
                resolver,
            )
            .await;
        });

        write_request(
            &mut client,
            serde_json::json!({
                "resource_id": "res-1",
                "destination": "",
                "port": port,
                "protocol": "tcp"
            }),
        )
        .await;

        let response = read_response(&mut client).await.expect("response");
        assert_eq!(response["ok"], true);

        client.write_all(b"ping").await.unwrap();
        client.flush().await.unwrap();
        let mut buf = [0u8; 4];
        client.read_exact(&mut buf).await.unwrap();
        assert_eq!(
            &buf, b"PING",
            "bytes must round-trip via the resolved backend"
        );
    }
}
