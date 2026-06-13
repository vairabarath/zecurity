use std::net::SocketAddr;
use std::sync::Arc;

use anyhow::{Context, Result};
use quinn::Endpoint;
use tracing::{info, warn};

use crate::session;
use crate::state::RelayState;
use crate::tls;

pub async fn run_listener(
    bind_addr: SocketAddr,
    server_config: quinn::ServerConfig,
    state: Arc<RelayState>,
) -> Result<()> {
    let endpoint = Endpoint::server(server_config, bind_addr).context("create Relay endpoint")?;
    info!(addr = %bind_addr, "Relay QUIC listener started");

    while let Some(incoming) = endpoint.accept().await {
        let state = state.clone();
        tokio::spawn(async move {
            let connection = match incoming.await {
                Ok(connection) => connection,
                Err(error) => {
                    warn!(error = %error, "Relay QUIC handshake failed");
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

            if let Err(error) = session::handle_connection(connection, identity, state).await {
                warn!(error = %error, "Relay session ended with error");
            }
        });
    }

    Ok(())
}
