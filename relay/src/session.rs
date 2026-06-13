use std::sync::Arc;

use anyhow::{bail, Context, Result};
use quinn::{Connection, RecvStream, SendStream};
use tracing::{info, warn};

use crate::protocol::{decode_message, encode_message, HandshakeMsg, RelayAck, MAX_MSG_SIZE};
use crate::spiffe::{same_workspace, ParsedSpiffe};
use crate::state::RelayState;

pub async fn handle_connection(
    connection: Connection,
    identity: ParsedSpiffe,
    state: Arc<RelayState>,
) -> Result<()> {
    let (mut send, mut recv) = connection
        .accept_bi()
        .await
        .context("accept Relay handshake stream")?;
    let message: HandshakeMsg = read_message(&mut recv).await?;

    match message {
        HandshakeMsg::Register {
            connector_id,
            spiffe_id,
        } => {
            validate_register(&identity, &connector_id, &spiffe_id)?;
            let registration_id = state.insert_connector(
                connector_id.clone(),
                connection.clone(),
                identity.uri.clone(),
                identity.trust_domain.clone(),
            );
            write_ack(&mut send, true, None).await?;

            info!(
                connector_id = %connector_id,
                spiffe_id = %identity.uri,
                trust_domain = %identity.trust_domain,
                "Connector registered with Relay"
            );
            connection.closed().await;
            state.remove_connector(&connector_id, registration_id);
            info!(connector_id = %connector_id, "Connector removed from Relay registry");
            Ok(())
        }
        HandshakeMsg::Lookup { connector_id } => {
            if identity.role != "client_device" {
                write_ack(
                    &mut send,
                    false,
                    Some("Lookup requires client_device identity"),
                )
                .await?;
                bail!("Lookup requires client_device identity");
            }

            spawn_lookup(send, recv, connector_id, identity.clone(), state.clone());
            while let Ok((mut send, mut recv)) = connection.accept_bi().await {
                let message: HandshakeMsg = match read_message(&mut recv).await {
                    Ok(message) => message,
                    Err(error) => {
                        warn!(error = %error, "invalid Relay Lookup message");
                        continue;
                    }
                };
                let HandshakeMsg::Lookup { connector_id } = message else {
                    let _ = write_ack(
                        &mut send,
                        false,
                        Some("Client connection accepts only Lookup messages"),
                    )
                    .await;
                    continue;
                };
                spawn_lookup(send, recv, connector_id, identity.clone(), state.clone());
            }
            Ok(())
        }
    }
}

fn spawn_lookup(
    send: SendStream,
    recv: RecvStream,
    connector_id: String,
    identity: ParsedSpiffe,
    state: Arc<RelayState>,
) {
    tokio::spawn(async move {
        if let Err(error) = handle_lookup_stream(send, recv, &connector_id, &identity, &state).await
        {
            warn!(connector_id = %connector_id, error = %error, "Relay Lookup failed");
        }
    });
}

async fn handle_lookup_stream(
    mut client_send: SendStream,
    client_recv: RecvStream,
    connector_id: &str,
    identity: &ParsedSpiffe,
    state: &RelayState,
) -> Result<()> {
    let connector = match state.lookup_connector(connector_id) {
        Some(connector) => connector,
        None => {
            write_ack(&mut client_send, false, Some("Connector is not registered")).await?;
            bail!("Connector {connector_id} is not registered");
        }
    };
    if connector.trust_domain != identity.trust_domain
        || !same_workspace(&connector.spiffe_id, &identity.uri)
    {
        write_ack(
            &mut client_send,
            false,
            Some("Cross-workspace lookup denied"),
        )
        .await?;
        bail!("client and Connector trust domains do not match");
    }

    let (connector_send, connector_recv) = connector
        .connection
        .open_bi()
        .await
        .context("open stream to registered Connector")?;
    write_ack(&mut client_send, true, None).await?;

    info!(
        connector_id = %connector_id,
        client_spiffe_id = %identity.uri,
        trust_domain = %identity.trust_domain,
        "bridging Client to Connector through Relay"
    );
    pipe_streams(client_send, client_recv, connector_send, connector_recv).await
}

async fn read_message<T: for<'a> serde::Deserialize<'a>>(recv: &mut RecvStream) -> Result<T> {
    let mut length = [0u8; 4];
    recv.read_exact(&mut length)
        .await
        .context("read Relay message length")?;
    let length = u32::from_be_bytes(length) as usize;
    if length > MAX_MSG_SIZE {
        bail!("Relay message too large: {length} bytes");
    }

    let mut body = vec![0u8; length];
    recv.read_exact(&mut body)
        .await
        .context("read Relay message body")?;
    decode_message(&body)
}

async fn write_ack(send: &mut SendStream, ok: bool, error: Option<&str>) -> Result<()> {
    let encoded = encode_message(&RelayAck {
        ok,
        error: error.map(str::to_owned),
    })?;
    send.write_all(&encoded)
        .await
        .context("write Relay acknowledgement")
}

async fn pipe_streams(
    mut client_send: SendStream,
    mut client_recv: RecvStream,
    mut connector_send: SendStream,
    mut connector_recv: RecvStream,
) -> Result<()> {
    let client_to_connector = async {
        tokio::io::copy(&mut client_recv, &mut connector_send)
            .await
            .context("pipe Client to Connector")?;
        connector_send
            .finish()
            .context("finish Connector-bound stream")
    };
    let connector_to_client = async {
        tokio::io::copy(&mut connector_recv, &mut client_send)
            .await
            .context("pipe Connector to Client")?;
        client_send.finish().context("finish Client-bound stream")
    };

    if let Err(error) = tokio::try_join!(client_to_connector, connector_to_client) {
        warn!(error = %error, "Relay stream bridge ended with error");
        return Err(error);
    }
    Ok(())
}

fn validate_register(identity: &ParsedSpiffe, connector_id: &str, spiffe_id: &str) -> Result<()> {
    if identity.role != "connector" {
        bail!("Register requires Connector identity");
    }
    if identity.entity_id != connector_id {
        bail!("Register connector_id does not match authenticated certificate");
    }
    if identity.uri != spiffe_id {
        bail!("Register spiffe_id does not match authenticated certificate");
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;

    const CONNECTOR_ID: &str = "550e8400-e29b-41d4-a716-446655440000";

    fn connector_identity() -> ParsedSpiffe {
        ParsedSpiffe {
            uri: format!("spiffe://workspace-a.zecurity.in/connector/{CONNECTOR_ID}"),
            trust_domain: "workspace-a.zecurity.in".to_owned(),
            role: "connector".to_owned(),
            entity_id: CONNECTOR_ID.to_owned(),
        }
    }

    #[test]
    fn register_must_match_authenticated_certificate() {
        let identity = connector_identity();
        validate_register(&identity, CONNECTOR_ID, &identity.uri).unwrap();
        assert!(validate_register(
            &identity,
            "9b2d5cae-5820-4702-adf4-231680852b11",
            &identity.uri
        )
        .is_err());
        assert!(validate_register(&identity, CONNECTOR_ID, "spiffe://wrong").is_err());
    }

    #[test]
    fn client_cannot_register_as_connector() {
        let mut identity = connector_identity();
        identity.role = "client_device".to_owned();
        assert!(validate_register(&identity, CONNECTOR_ID, &identity.uri).is_err());
    }
}
