use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{Context, Result};
use quinn::Endpoint;
use tokio::sync::Semaphore;
use tokio::time::timeout;
use tracing::{info, warn};

use crate::config::RuntimeLimits;
use crate::crl::RevocationStatus;
use crate::session;
use crate::state::RelayState;
use crate::tls;

pub async fn run_listener(
    bind_addr: SocketAddr,
    server_config: quinn::ServerConfig,
    state: Arc<RelayState>,
    limits: RuntimeLimits,
    crl: crate::crl::WorkspaceCrlManager,
) -> Result<()> {
    let endpoint = Endpoint::server(server_config, bind_addr).context("create Relay endpoint")?;
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
                Ok(pr) => match crl
                    .check(&pr.workspace_id, &pr.leaf_serial, &pr.workspace_ca_der)
                    .await
                {
                    RevocationStatus::NotRevoked => {}
                    RevocationStatus::Revoked => {
                        warn!(spiffe_id = %identity.uri, "rejecting revoked peer certificate");
                        connection.close(0u32.into(), b"peer certificate revoked");
                        return;
                    }
                    RevocationStatus::Unavailable => {
                        warn!(spiffe_id = %identity.uri, "workspace CRL unavailable — failing closed");
                        connection.close(0u32.into(), b"revocation state unavailable");
                        return;
                    }
                },
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
