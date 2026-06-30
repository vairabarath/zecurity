use std::collections::HashMap;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{bail, Context, Result};
use quinn::{Connection, RecvStream, SendStream};
use tokio::sync::{Mutex, Semaphore};
use tokio::time::timeout;
use tracing::{info, warn};

use crate::config::RuntimeLimits;
use crate::protocol::{
    decode_message, encode_message, HandshakeMsg, ProbeResponse, RelayAck, MAX_MSG_SIZE,
};
use crate::spiffe::{same_workspace, ParsedSpiffe};
use crate::state::RelayState;

pub static ACTIVE_STREAMS: AtomicU32 = AtomicU32::new(0);

struct ProbeRateWindow {
    count: u32,
    window_start: Instant,
}

#[derive(Clone)]
pub struct SessionLimits {
    handshake_timeout: Duration,
    message_timeout: Duration,
    lookup_permits: Arc<Semaphore>,
    probe_permits: Arc<Semaphore>,
    probe_timeout: Duration,
    max_probe_rate: u32,
    probe_rate_tracker: Arc<Mutex<HashMap<String, ProbeRateWindow>>>,
}

impl SessionLimits {
    pub fn new(limits: &RuntimeLimits) -> Self {
        Self {
            handshake_timeout: limits.handshake_timeout,
            message_timeout: limits.message_timeout,
            lookup_permits: Arc::new(Semaphore::new(limits.max_lookup_bridges)),
            probe_permits: Arc::new(Semaphore::new(limits.max_concurrent_probes)),
            probe_timeout: limits.probe_timeout,
            max_probe_rate: limits.max_probe_rate,
            probe_rate_tracker: Arc::new(Mutex::new(HashMap::new())),
        }
    }
}

pub async fn handle_connection(
    connection: Connection,
    identity: ParsedSpiffe,
    state: Arc<RelayState>,
    limits: SessionLimits,
) -> Result<()> {
    let (mut send, mut recv) = timeout(limits.handshake_timeout, connection.accept_bi())
        .await
        .context("Relay initial stream timed out")?
        .context("accept Relay handshake stream")?;
    let message: HandshakeMsg = read_message(&mut recv, limits.message_timeout).await?;

    match message {
        HandshakeMsg::Probe {
            connector_id,
            request_id,
        } => {
            if identity.role != "connector" {
                bail!("Probe requires connector identity");
            }
            if connector_id.is_empty() {
                bail!("Probe connector_id must be non-empty");
            }

            // Concurrent probe cap
            let _permit = match limits.probe_permits.clone().try_acquire_owned() {
                Ok(p) => p,
                Err(_) => {
                    warn!(
                        connector_id = %connector_id,
                        rejection_reason = "probe_concurrency_limit",
                        "Relay Probe rejected: concurrent limit reached"
                    );
                    return Ok(());
                }
            };

            // Per-connector rate limit (max N per 60s window)
            {
                let mut tracker = limits.probe_rate_tracker.lock().await;
                let now = Instant::now();
                let window = tracker.entry(connector_id.clone()).or_insert(ProbeRateWindow {
                    count: 0,
                    window_start: now,
                });
                if now.duration_since(window.window_start) >= Duration::from_secs(60) {
                    window.count = 0;
                    window.window_start = now;
                }
                if window.count >= limits.max_probe_rate {
                    warn!(
                        connector_id = %connector_id,
                        rejection_reason = "probe_rate_limit",
                        "Relay Probe rejected: rate limit exceeded"
                    );
                    return Ok(());
                }
                window.count += 1;
            }

            let response = ProbeResponse { request_id };
            let encoded = encode_message(&response).context("encode ProbeResponse")?;
            timeout(limits.probe_timeout, send.write_all(&encoded))
                .await
                .context("Relay Probe response timed out")?
                .context("write ProbeResponse")?;
            // Connection closes naturally when send/recv are dropped — do not register.
            Ok(())
        }
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
            if identity.role != "client" {
                write_ack(&mut send, false, Some("Lookup requires client identity")).await?;
                bail!("Lookup requires client identity");
            }

            spawn_lookup(
                send,
                recv,
                connector_id,
                identity.clone(),
                state.clone(),
                limits.clone(),
            );
            while let Ok((mut send, mut recv)) = connection.accept_bi().await {
                let message: HandshakeMsg =
                    match read_message(&mut recv, limits.message_timeout).await {
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
                spawn_lookup(
                    send,
                    recv,
                    connector_id,
                    identity.clone(),
                    state.clone(),
                    limits.clone(),
                );
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
    limits: SessionLimits,
) {
    let permit = match limits.lookup_permits.clone().try_acquire_owned() {
        Ok(permit) => permit,
        Err(_) => {
            tokio::spawn(async move {
                let mut send = send;
                let _ = write_ack(&mut send, false, Some("Relay Lookup capacity exhausted")).await;
                warn!(
                    connector_id = %connector_id,
                    rejection_reason = "lookup_limit",
                    "Relay Lookup rejected because capacity is exhausted"
                );
            });
            return;
        }
    };
    tokio::spawn(async move {
        let _permit = permit;
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
    ACTIVE_STREAMS.fetch_add(1, Ordering::Relaxed);
    let result = pipe_streams(client_send, client_recv, connector_send, connector_recv).await;
    ACTIVE_STREAMS.fetch_sub(1, Ordering::Relaxed);
    result
}

async fn read_message<T: for<'a> serde::Deserialize<'a>>(
    recv: &mut RecvStream,
    message_timeout: Duration,
) -> Result<T> {
    timeout(message_timeout, read_message_inner(recv))
        .await
        .context("Relay framed message timed out")?
}

async fn read_message_inner<T: for<'a> serde::Deserialize<'a>>(recv: &mut RecvStream) -> Result<T> {
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
    use crate::config::RuntimeLimits;

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
        identity.role = "client".to_owned();
        assert!(validate_register(&identity, CONNECTOR_ID, &identity.uri).is_err());
    }

    #[test]
    fn lookup_capacity_is_bounded_and_released() {
        let limits = SessionLimits::new(&RuntimeLimits {
            max_connections: 1,
            max_lookup_bridges: 1,
            max_bidi_streams: 1,
            idle_timeout: Duration::from_secs(1),
            handshake_timeout: Duration::from_secs(1),
            message_timeout: Duration::from_secs(1),
            max_probe_rate: 10,
            max_concurrent_probes: 20,
            probe_timeout: Duration::from_millis(2000),
        });

        let permit = limits.lookup_permits.clone().try_acquire_owned().unwrap();
        assert!(limits.lookup_permits.clone().try_acquire_owned().is_err());
        drop(permit);
        assert!(limits.lookup_permits.clone().try_acquire_owned().is_ok());
    }
}
