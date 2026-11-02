use std::collections::HashMap;
use std::sync::atomic::{AtomicU32, Ordering};
use std::sync::Arc;
use std::time::{Duration, Instant};

use anyhow::{bail, Context, Result};
use quinn::{Connection, RecvStream, SendStream};
use tokio::sync::{Mutex, Semaphore};
use tokio::time::timeout;
use tracing::{debug, info, warn};

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
            validate_probe(&identity, &connector_id)?;
            let authenticated_connector_id = identity.entity_id.clone();

            // Concurrent probe cap
            let _permit = match limits.probe_permits.clone().try_acquire_owned() {
                Ok(p) => p,
                Err(_) => {
                    warn!(
                        connector_id = %authenticated_connector_id,
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
                tracker.retain(|_, window| {
                    now.duration_since(window.window_start) < Duration::from_secs(60)
                });
                let window =
                    tracker
                        .entry(authenticated_connector_id.clone())
                        .or_insert(ProbeRateWindow {
                            count: 0,
                            window_start: now,
                        });
                if now.duration_since(window.window_start) >= Duration::from_secs(60) {
                    window.count = 0;
                    window.window_start = now;
                }
                if window.count >= limits.max_probe_rate {
                    warn!(
                        connector_id = %authenticated_connector_id,
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
            // Connector-revocation Phase D: a revoked identity never registers.
            let canonical =
                crate::state::canonical_connector_spiffe(&identity.trust_domain, &connector_id);
            if state.is_revoked(&canonical) {
                refuse_revoked(&connection, &mut send).await;
                bail!("revoked connector refused: {canonical}");
            }
            let registration_id = state.insert_connector(
                connector_id.clone(),
                connection.clone(),
                identity.uri.clone(),
                identity.trust_domain.clone(),
            );
            // Race re-check: a heartbeat may have revoked it between the check
            // and the insert (its sweep may have missed this entry).
            if state.is_revoked(&canonical) {
                state.remove_connector(&connector_id, registration_id);
                refuse_revoked(&connection, &mut send).await;
                bail!("revoked connector refused after registration: {canonical}");
            }
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
                            warn!(error = format!("{error:#}"), "invalid Relay Lookup message");
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
            warn!(connector_id = %connector_id, error = format!("{error:#}"), "Relay Lookup failed");
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

/// Sends the refusal ack and lets it reach the peer (bounded wait) before
/// closing the connection; closing at once would discard the unsent ack.
async fn refuse_revoked(connection: &Connection, send: &mut SendStream) {
    if write_ack(send, false, Some("connector is revoked"))
        .await
        .is_ok()
        && send.finish().is_ok()
    {
        let _ = tokio::time::timeout(Duration::from_secs(2), send.stopped()).await;
    }
    connection.close(0u32.into(), b"connector revoked");
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

/// QUIC application code the relay uses when it ends a bridge for a reason
/// other than a peer's reset or stop (a lost connection, a write that failed),
/// so neither side mistakes the end for a clean finish. "RLAB".
const RELAY_ABORT_CODE: u32 = 0x524C_4142;

/// How one direction of a bridge ended badly.
enum PipeError {
    /// The peer reset the stream we read from, with this code.
    Reset(quinn::VarInt),
    /// The peer stopped reading the stream we write to, with this code.
    Stopped(quinn::VarInt),
    Other(anyhow::Error),
}

impl PipeError {
    /// The code to forward to the other side.
    fn code(&self) -> quinn::VarInt {
        match self {
            PipeError::Reset(code) | PipeError::Stopped(code) => *code,
            PipeError::Other(_) => quinn::VarInt::from_u32(RELAY_ABORT_CODE),
        }
    }

    fn into_anyhow(self, direction: &str) -> anyhow::Error {
        match self {
            PipeError::Reset(code) => anyhow::anyhow!(
                "{direction}: stream reset by peer (code {:#x})",
                code.into_inner()
            ),
            PipeError::Stopped(code) => anyhow::anyhow!(
                "{direction}: stream stopped by peer (code {:#x})",
                code.into_inner()
            ),
            PipeError::Other(e) => e.context(direction.to_owned()),
        }
    }
}

async fn pipe_streams(
    mut client_send: SendStream,
    mut client_recv: RecvStream,
    mut connector_send: SendStream,
    mut connector_recv: RecvStream,
) -> Result<()> {
    // Per-direction byte counts and which direction finished first, logged
    // when the bridge ends (live E2E run 2026-10-30, F-7: relayed flows ended
    // without the inner TLS close_notify, and nothing showed which hop lost it).
    let up = std::sync::atomic::AtomicU64::new(0);
    let down = std::sync::atomic::AtomicU64::new(0);
    let first_done = std::sync::Mutex::new(None::<&'static str>);
    let client_to_connector = async {
        counted_copy(&mut client_recv, &mut connector_send, &up)
            .await
            .map_err(|e| ("pipe Client to Connector", e))?;
        first_done
            .lock()
            .unwrap()
            .get_or_insert("client to connector");
        connector_send.finish().map_err(|e| {
            (
                "pipe Client to Connector",
                PipeError::Other(anyhow::Error::new(e).context("finish Connector-bound stream")),
            )
        })
    };
    let connector_to_client = async {
        counted_copy(&mut connector_recv, &mut client_send, &down)
            .await
            .map_err(|e| ("pipe Connector to Client", e))?;
        first_done
            .lock()
            .unwrap()
            .get_or_insert("connector to client");
        client_send.finish().map_err(|e| {
            (
                "pipe Connector to Client",
                PipeError::Other(anyhow::Error::new(e).context("finish Client-bound stream")),
            )
        })
    };

    let result = tokio::try_join!(client_to_connector, connector_to_client);
    let up = up.load(std::sync::atomic::Ordering::Relaxed);
    let down = down.load(std::sync::atomic::Ordering::Relaxed);
    let first = first_done.lock().unwrap().unwrap_or("neither");
    if let Err((direction, error)) = result {
        // A failed bridge must not end gracefully: dropping a quinn SendStream
        // finishes it, which would deliver everything still queued and then a
        // clean EOF. A revoked session's tail would keep flowing to the client
        // (FU-15 relay path). Abort all four halves instead, forwarding the
        // peer's reset or stop code (the connector's RVKD reaches the client).
        let code = error.code();
        let _ = client_send.reset(code);
        let _ = connector_send.reset(code);
        let _ = client_recv.stop(code);
        let _ = connector_recv.stop(code);
        let error = error.into_anyhow(direction);
        // Full chain: the outer context alone ("pipe Connector to Client") hid
        // which side failed and why (live E2E run 2026-10-30, F-7).
        warn!(
            bytes_up = up,
            bytes_down = down,
            finished_first = first,
            forwarded_code = format!("{:#x}", code.into_inner()),
            error = format!("{error:#}"),
            "Relay stream bridge ended with error"
        );
        return Err(error);
    }
    debug!(
        bytes_up = up,
        bytes_down = down,
        finished_first = first,
        "Relay stream bridge ended"
    );
    Ok(())
}

/// Copies reader to writer, counting bytes as they are written. Uses quinn's
/// own read/write so a peer's reset or stop keeps its code (tokio's
/// AsyncRead/AsyncWrite would turn it into a plain io::Error).
async fn counted_copy(
    reader: &mut RecvStream,
    writer: &mut SendStream,
    count: &std::sync::atomic::AtomicU64,
) -> std::result::Result<(), PipeError> {
    let mut buf = vec![0u8; 64 * 1024];
    loop {
        let n = match reader.read(&mut buf).await {
            Ok(Some(n)) => n,
            Ok(None) => return Ok(()),
            Err(quinn::ReadError::Reset(code)) => return Err(PipeError::Reset(code)),
            Err(e) => return Err(PipeError::Other(anyhow::Error::new(e).context("read"))),
        };
        match writer.write_all(&buf[..n]).await {
            Ok(()) => {}
            Err(quinn::WriteError::Stopped(code)) => return Err(PipeError::Stopped(code)),
            Err(e) => return Err(PipeError::Other(anyhow::Error::new(e).context("write"))),
        }
        count.fetch_add(n as u64, std::sync::atomic::Ordering::Relaxed);
    }
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

fn validate_probe(identity: &ParsedSpiffe, connector_id: &str) -> Result<()> {
    if identity.role != "connector" {
        bail!("Probe requires Connector identity");
    }
    if connector_id.is_empty() {
        bail!("Probe connector_id must be non-empty");
    }
    if identity.entity_id != connector_id {
        bail!("Probe connector_id does not match authenticated certificate");
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
    fn probe_must_match_authenticated_certificate() {
        let identity = connector_identity();

        validate_probe(&identity, CONNECTOR_ID).unwrap();

        assert!(validate_probe(&identity, "9b2d5cae-5820-4702-adf4-231680852b11").is_err());
    }

    #[test]
    fn client_cannot_probe_as_connector() {
        let mut identity = connector_identity();
        identity.role = "client".to_owned();

        assert!(validate_probe(&identity, CONNECTOR_ID).is_err());
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

    // ── Connector-revocation Phase D ───────────────────────────────────────

    fn limits() -> SessionLimits {
        SessionLimits::new(&RuntimeLimits {
            max_connections: 8,
            max_lookup_bridges: 8,
            max_bidi_streams: 8,
            idle_timeout: Duration::from_secs(30),
            handshake_timeout: Duration::from_secs(5),
            message_timeout: Duration::from_secs(5),
            max_probe_rate: 10,
            max_concurrent_probes: 4,
            probe_timeout: Duration::from_secs(2),
        })
    }

    /// Plays the connector: sends Register on a new stream, returns the ack.
    async fn register(client: &quinn::Connection, identity: &ParsedSpiffe) -> RelayAck {
        let (mut send, mut recv) = client.open_bi().await.unwrap();
        let msg = encode_message(&HandshakeMsg::Register {
            connector_id: identity.entity_id.clone(),
            spiffe_id: identity.uri.clone(),
        })
        .unwrap();
        send.write_all(&msg).await.unwrap();
        read_message(&mut recv, Duration::from_secs(5))
            .await
            .unwrap()
    }

    fn canonical() -> String {
        crate::state::canonical_connector_spiffe("workspace-a.zecurity.in", CONNECTOR_ID)
    }

    /// AD-4: a revoked identity is refused at Register (ok:false) and never
    /// enters the registry.
    #[tokio::test]
    async fn revoked_connector_cannot_register() {
        let (client, server) = crate::test_support::quic_pair().await;
        let state = RelayState::new();
        state.apply_revoked(&[canonical()]);
        // The pre-check refuses before the insert: a revoked identity is
        // never visible in the registry, even briefly.
        let inserted = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let flag = inserted.clone();
        *state.before_insert.lock().unwrap() = Some(Box::new(move || {
            flag.store(true, std::sync::atomic::Ordering::SeqCst);
        }));
        let task = tokio::spawn(handle_connection(
            server,
            connector_identity(),
            state.clone(),
            limits(),
        ));
        let ack = register(&client, &connector_identity()).await;
        assert!(!ack.ok, "revoked connector must be refused");
        assert_eq!(ack.error.as_deref(), Some("connector is revoked"));
        assert!(task.await.unwrap().is_err());
        assert_eq!(state.connector_count(), 0);
        assert!(
            !inserted.load(std::sync::atomic::Ordering::SeqCst),
            "a revoked identity must be refused before the insert"
        );
    }

    /// AD-4 race: a revocation applied between the Register check and the
    /// insert (the sweep misses the not-yet-inserted entry) is caught by the
    /// post-insert re-check.
    #[tokio::test]
    async fn revocation_inside_the_register_window_is_caught_by_the_recheck() {
        let (client, server) = crate::test_support::quic_pair().await;
        let state = RelayState::new();
        let hook_state = state.clone();
        *state.before_insert.lock().unwrap() = Some(Box::new(move || {
            assert_eq!(
                hook_state.apply_revoked(&[canonical()]),
                0,
                "sweep finds nothing yet"
            );
        }));
        let task = tokio::spawn(handle_connection(
            server,
            connector_identity(),
            state.clone(),
            limits(),
        ));
        let ack = register(&client, &connector_identity()).await;
        assert!(!ack.ok, "re-check must refuse");
        assert_eq!(ack.error.as_deref(), Some("connector is revoked"));
        assert!(task.await.unwrap().is_err());
        assert_eq!(state.connector_count(), 0);
    }

    /// AD-3: a registered connector reported revoked is dropped (its
    /// connection closed, registry emptied); the set is sticky — an empty
    /// later answer unblocks nothing, and it can't re-register.
    #[tokio::test]
    async fn registered_connector_is_dropped_when_revoked_and_stays_blocked() {
        let (client, server) = crate::test_support::quic_pair().await;
        let state = RelayState::new();
        let task = tokio::spawn(handle_connection(
            server,
            connector_identity(),
            state.clone(),
            limits(),
        ));
        assert!(register(&client, &connector_identity()).await.ok);
        assert_eq!(state.registered_connector_spiffe_ids(), vec![canonical()]);

        assert_eq!(state.apply_revoked(&[canonical()]), 1);
        tokio::time::timeout(Duration::from_secs(5), client.closed())
            .await
            .expect("connection closed");
        tokio::time::timeout(Duration::from_secs(5), task)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
        assert_eq!(state.connector_count(), 0, "removed from the registry");

        assert_eq!(state.apply_revoked(&[]), 0);
        assert!(
            state.is_revoked(&canonical()),
            "sticky: an empty answer unblocks nothing"
        );
        state.apply_revoked(&[crate::state::canonical_connector_spiffe(
            "workspace-a.zecurity.in",
            "9b2d5cae-5820-4702-adf4-231680852b11",
        )]);
        assert!(
            state.is_revoked(&canonical()),
            "sticky: a later answer without it unblocks nothing"
        );
        let (client, server) = crate::test_support::quic_pair().await;
        tokio::spawn(handle_connection(
            server,
            connector_identity(),
            state.clone(),
            limits(),
        ));
        assert!(
            !register(&client, &connector_identity()).await.ok,
            "can't re-register"
        );
    }

    /// AD-3: only the listed connector is dropped; the registered ids are
    /// canonical (lower-case uuid) even for an upper-case certificate id.
    #[tokio::test]
    async fn only_listed_connectors_are_dropped_and_ids_are_canonical() {
        let (c1, s1) = crate::test_support::quic_pair().await;
        let (c2, s2) = crate::test_support::quic_pair().await;
        let state = RelayState::new();
        let other = "9b2d5cae-5820-4702-adf4-231680852b11";
        state.insert_connector(
            CONNECTOR_ID.to_uppercase(),
            s1,
            "u1".into(),
            "workspace-a.zecurity.in".into(),
        );
        state.insert_connector(
            other.into(),
            s2,
            "u2".into(),
            "workspace-b.zecurity.in".into(),
        );
        let ids = state.registered_connector_spiffe_ids();
        assert!(ids.contains(&canonical()), "{ids:?}");
        assert_eq!(state.apply_revoked(&[canonical()]), 1);
        tokio::time::timeout(Duration::from_secs(5), c1.closed())
            .await
            .expect("listed closed");
        tokio::time::sleep(Duration::from_millis(100)).await;
        assert!(c2.close_reason().is_none(), "unlisted connector untouched");
    }

    // ── FU-15 follow-up: the bridge forwards a reset or stop with its code ──

    /// The connector's code for a revoked session ("RVKD"), as it resets the
    /// device stream (connector/src/abort_stream.rs).
    const RVKD: u32 = 0x5256_4B44;

    struct Bridge {
        client_send: SendStream,
        client_recv: RecvStream,
        connector_send: SendStream,
        connector_recv: RecvStream,
        pipe: tokio::task::JoinHandle<Result<()>>,
        _conns: [Connection; 4],
    }

    /// A real client <-> relay <-> connector bridge: two QUIC pairs, one
    /// stream on each, joined by pipe_streams.
    async fn bridge() -> Bridge {
        let (client, relay_client_side) = crate::test_support::quic_pair().await;
        let (relay_connector_side, connector) = crate::test_support::quic_pair().await;
        let (mut client_send, client_recv) = client.open_bi().await.unwrap();
        client_send.write_all(b"hi").await.unwrap(); // a stream is announced by its first bytes
        let (relay_client_send, mut relay_client_recv) =
            relay_client_side.accept_bi().await.unwrap();
        let mut hi = [0u8; 2];
        relay_client_recv.read_exact(&mut hi).await.unwrap();
        let (mut relay_connector_send, relay_connector_recv) =
            relay_connector_side.open_bi().await.unwrap();
        relay_connector_send.write_all(b"hi").await.unwrap();
        let (connector_send, mut connector_recv) = connector.accept_bi().await.unwrap();
        connector_recv.read_exact(&mut hi).await.unwrap();
        let pipe = tokio::spawn(pipe_streams(
            relay_client_send,
            relay_client_recv,
            relay_connector_send,
            relay_connector_recv,
        ));
        Bridge {
            client_send,
            client_recv,
            connector_send,
            connector_recv,
            pipe,
            _conns: [client, relay_client_side, relay_connector_side, connector],
        }
    }

    /// Reads until the stream ends; returns (bytes read, how it ended).
    async fn drain(recv: &mut RecvStream) -> (usize, std::result::Result<(), quinn::ReadError>) {
        let mut total = 0;
        let mut buf = vec![0u8; 64 * 1024];
        loop {
            match recv.read(&mut buf).await {
                Ok(Some(n)) => total += n,
                Ok(None) => return (total, Ok(())),
                Err(e) => return (total, Err(e)),
            }
        }
    }

    /// The connector resets a revoked session's stream while megabytes are
    /// still queued toward a slow client. The client must get that reset, with
    /// the connector's code, without the backlog first: before this fix the
    /// relay finished the client-bound stream gracefully and delivered it all.
    #[tokio::test]
    async fn a_connector_reset_reaches_the_client_with_its_code() {
        let mut b = bridge().await;
        let chunk = vec![7u8; 64 * 1024];
        let sent = 4 * 1024 * 1024;
        let mut written = 0;
        // Fill every buffer on the way: the client doesn't read yet.
        while written < sent {
            match timeout(
                Duration::from_millis(200),
                b.connector_send.write_all(&chunk),
            )
            .await
            {
                Ok(Ok(())) => written += chunk.len(),
                _ => break, // flow control: buffers are full
            }
        }
        assert!(written >= 1024 * 1024, "only {written} bytes in flight");
        b.connector_send
            .reset(quinn::VarInt::from_u32(RVKD))
            .unwrap();
        let _ = b.connector_recv.stop(quinn::VarInt::from_u32(RVKD));

        let (got, end) = timeout(Duration::from_secs(5), drain(&mut b.client_recv))
            .await
            .expect("the client must learn of the reset promptly");
        assert!(
            matches!(end, Err(quinn::ReadError::Reset(code)) if code.into_inner() == u64::from(RVKD)),
            "client saw {end:?} after {got} bytes, want a reset with RVKD"
        );
        assert!(
            got < written,
            "the client received the whole backlog ({got} of {written} bytes) before the reset"
        );
        assert!(
            b.pipe.await.unwrap().is_err(),
            "the bridge ends with an error"
        );
    }

    /// The client stops reading (its app is gone): the connector's writes fail
    /// with the client's code instead of filling the relay.
    #[tokio::test]
    async fn a_client_stop_reaches_the_connector_with_its_code() {
        let mut b = bridge().await;
        b.client_recv.stop(quinn::VarInt::from_u32(0x4142)).unwrap();
        let chunk = vec![1u8; 64 * 1024];
        let err = timeout(Duration::from_secs(5), async {
            loop {
                if let Err(e) = b.connector_send.write_all(&chunk).await {
                    return e;
                }
            }
        })
        .await
        .expect("the connector must see the stop");
        assert!(
            matches!(err, quinn::WriteError::Stopped(code) if code.into_inner() == 0x4142),
            "connector saw {err:?}"
        );
        assert!(b.pipe.await.unwrap().is_err());
    }

    /// The client resets its upload: the connector reads the same reset.
    #[tokio::test]
    async fn a_client_reset_reaches_the_connector_with_its_code() {
        let mut b = bridge().await;
        b.client_send.write_all(b"partial upload").await.unwrap();
        b.client_send
            .reset(quinn::VarInt::from_u32(0x4243))
            .unwrap();
        let (_, end) = timeout(Duration::from_secs(5), drain(&mut b.connector_recv))
            .await
            .expect("the connector must learn of the reset");
        assert!(
            matches!(end, Err(quinn::ReadError::Reset(code)) if code.into_inner() == 0x4243),
            "connector saw {end:?}"
        );
    }

    /// A normal end is unchanged: every byte arrives, then a clean finish, in
    /// both directions, and the bridge ends without error.
    #[tokio::test]
    async fn a_clean_end_is_still_a_clean_finish() {
        let mut b = bridge().await;
        let body = vec![9u8; 3 * 1024 * 1024 + 17];
        let mut connector_send = b.connector_send;
        let up = tokio::spawn(async move {
            connector_send.write_all(&body).await.unwrap();
            connector_send.finish().unwrap();
            connector_send
        });
        let (got, end) = timeout(Duration::from_secs(10), drain(&mut b.client_recv))
            .await
            .unwrap();
        assert_eq!(got, 3 * 1024 * 1024 + 17, "every byte must arrive");
        assert!(end.is_ok(), "a clean finish, got {end:?}");
        let _keep = up.await.unwrap();
        b.client_send.write_all(b"bye").await.unwrap();
        b.client_send.finish().unwrap();
        let (got, end) = timeout(Duration::from_secs(5), drain(&mut b.connector_recv))
            .await
            .unwrap();
        assert_eq!(got, 3);
        assert!(end.is_ok(), "a clean finish upstream, got {end:?}");
        timeout(Duration::from_secs(5), b.pipe)
            .await
            .unwrap()
            .unwrap()
            .unwrap();
    }
}
