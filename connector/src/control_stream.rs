use std::convert::Infallible;
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use tokio::sync::{mpsc, watch};
use tokio::time::{interval, sleep_until, Instant};
use tokio_stream::wrappers::ReceiverStream;
use tokio_stream::{Stream, StreamExt};
use tonic::Request;
use tracing::{error, info, warn};

use crate::policy::PolicyCache;
use crate::session_registry::SessionRegistry;

use crate::agent_server::ShieldRegistry;
use crate::authorization::{
    observe_controller_msg, observe_status, stamp_sends, SharedAckTracker, StreamAuthz,
    TerminalSignal,
};
use crate::config::ConnectorConfig;
use crate::controller_client::{
    build_channel, fetch_public_ip, verify_controller_spiffe_preflight,
};
use crate::discovery::scan::{execute_scan, ScanCommand};
use crate::enrollment::EnrollmentState;
use crate::proto::connector_control_message::Body as CBody;
use crate::proto::{
    connector_service_client::ConnectorServiceClient, ConnectorControlMessage,
    ConnectorHealthReport, LabelledRelayList, ResourceAckBatch, ScanReport as ProtoScanReport,
    ScanResult as ProtoScanResult,
};
use crate::relay_attachment::RelayAttachmentSlot;
use crate::renewal;
use crate::shield_proto::ResourceAck;
use crate::tls::cert_holder::CertHolder;
use crate::util;

const BACKOFF_INITIAL_SECS: u64 = 2;
const BACKOFF_MAX_SECS: u64 = 60;
const HEALTH_INTERVAL_SECS: u64 = 15;
/// H — maximum authorization freshness (design D1): one heartbeat interval.
/// The deadline is D = sent[k] + H of the latest accepted acknowledgement.
pub const AUTHORIZATION_FRESHNESS: Duration = Duration::from_secs(HEALTH_INTERVAL_SECS);
/// How often a connector parked in a terminal state repeats its log line.
const TERMINAL_LOG_INTERVAL: Duration = Duration::from_secs(300);
const DISCOVERY_FLUSH_SECS: u64 = 5;

/// Delays of the outer reconnect loop.
#[derive(Debug, Clone, Copy)]
struct ReconnectTiming {
    /// After a clean end of a session.
    clean: Duration,
    backoff_initial: Duration,
    backoff_max: Duration,
    /// Repeat interval of the terminal-state log line.
    terminal_log: Duration,
}

const RECONNECT_TIMING: ReconnectTiming = ReconnectTiming {
    clean: Duration::from_secs(1),
    backoff_initial: Duration::from_secs(BACKOFF_INITIAL_SECS),
    backoff_max: Duration::from_secs(BACKOFF_MAX_SECS),
    terminal_log: TERMINAL_LOG_INTERVAL,
};

/// One pass of the outer loop: open a Control stream and run its session to
/// the end. Production is [`LiveAttempt`]; tests script the outcomes.
trait Attempt {
    fn attempt(&mut self) -> impl Future<Output = Result<()>>;
}

/// Outer reconnect loop. Every end of an attempt is a stream end for the
/// serving state. In `Revoked`/`UnknownIdentity` the loop stops for good:
/// no reconnects, health reports or renewals. It never returns — the process
/// stays alive with the data plane closed (design D12), because an exit
/// would be restarted by the service manager.
async fn reconnect_loop<A: Attempt>(
    attempt: &mut A,
    authz: &SharedAckTracker,
    timing: ReconnectTiming,
) -> Infallible {
    let mut backoff = timing.backoff_initial;
    loop {
        let result = attempt.attempt().await;
        authz.lock().stream_down(Instant::now());
        let terminal = authz.lock().terminal();
        if let Some(signal) = terminal {
            return park_terminal(signal, timing.terminal_log).await;
        }
        match result {
            Ok(()) => {
                backoff = timing.backoff_initial;
                info!("control stream closed cleanly, reconnecting");
                tokio::time::sleep(timing.clean).await;
            }
            Err(e) => {
                error!(
                    error = %e,
                    backoff_secs = backoff.as_secs(),
                    "control stream error, reconnecting with backoff"
                );
                tokio::time::sleep(backoff).await;
                backoff = (backoff * 2).min(timing.backoff_max);
            }
        }
    }
}

/// Idle in a terminal state for the rest of the process lifetime.
async fn park_terminal(signal: TerminalSignal, every: Duration) -> Infallible {
    loop {
        error!(
            authz_event = "terminal_idle",
            terminal = ?signal,
            "connector is in a terminal state — data plane closed, reconnect loop stopped; the process stays up. Recovery needs a new, explicitly authorized enrollment"
        );
        tokio::time::sleep(every).await;
    }
}

/// Production [`Attempt`]: everything `run_once` needs, for the process
/// lifetime.
struct LiveAttempt<'a> {
    cfg: &'a ConnectorConfig,
    state: EnrollmentState,
    shield_registry: ShieldRegistry,
    ack_rx: mpsc::Receiver<(String, ResourceAck)>,
    log_rx: mpsc::Receiver<crate::ControlMessage>,
    lan_addr: String,
    hostname: String,
    version: String,
    public_ip: String,
    policy_cache: Arc<PolicyCache>,
    registry: Arc<SessionRegistry>,
    relay_attachment_slot: RelayAttachmentSlot,
    relay_list_tx: watch::Sender<Option<LabelledRelayList>>,
    certs: Arc<CertHolder>,
    authz: SharedAckTracker,
}

impl Attempt for LiveAttempt<'_> {
    async fn attempt(&mut self) -> Result<()> {
        run_once(
            self.cfg,
            &mut self.state,
            &self.shield_registry,
            &mut self.ack_rx,
            &mut self.log_rx,
            &self.lan_addr,
            &self.hostname,
            &self.version,
            &self.public_ip,
            &self.policy_cache,
            &self.registry,
            &self.relay_attachment_slot,
            &self.relay_list_tx,
            &self.certs,
            &self.authz,
        )
        .await
    }
}

/// Outer reconnect loop. Blocks indefinitely — run via tokio::spawn or await directly from main.
/// `authz` is the process-global tracker created in `main`, whose gate the
/// data-plane entry paths already hold.
#[allow(clippy::too_many_arguments)]
pub async fn run_control_stream(
    cfg: &ConnectorConfig,
    state: &EnrollmentState,
    shield_registry: ShieldRegistry,
    ack_rx: mpsc::Receiver<(String, ResourceAck)>,
    log_rx: mpsc::Receiver<crate::ControlMessage>,
    policy_cache: Arc<PolicyCache>,
    registry: Arc<SessionRegistry>,
    relay_attachment_slot: RelayAttachmentSlot,
    relay_list_tx: watch::Sender<Option<LabelledRelayList>>,
    certs: Arc<CertHolder>,
    authz: SharedAckTracker,
) -> Result<()> {
    let hostname = util::read_hostname();
    let public_ip = fetch_public_ip().await;
    let version = env!("CARGO_PKG_VERSION").to_string();
    let lan_addr = cfg.lan_addr.clone().unwrap_or_else(|| {
        util::detect_lan_ip()
            .map(|ip| format!("{}:9091", ip))
            .unwrap_or_default()
    });

    if cfg.lan_addr.is_some() {
        info!(lan_addr = %lan_addr, "using configured CONNECTOR_LAN_ADDR");
    } else if !lan_addr.is_empty() {
        info!(lan_addr = %lan_addr, "auto-detected LAN address");
    } else {
        warn!(
            "could not detect LAN address — shields on the same network may be unable to connect"
        );
    }

    let mut attempt = LiveAttempt {
        cfg,
        state: state.clone(),
        shield_registry,
        ack_rx,
        log_rx,
        lan_addr,
        hostname,
        version,
        public_ip,
        policy_cache,
        registry,
        relay_attachment_slot,
        relay_list_tx,
        certs,
        authz: authz.clone(),
    };
    match reconnect_loop(&mut attempt, &authz, RECONNECT_TIMING).await {}
}

/// Send a message on the outbound control stream.
/// Returns an error if the stream is closed - causes run_once() to break and reconnect.
async fn send_msg(
    tx: &mpsc::Sender<ConnectorControlMessage>,
    msg: ConnectorControlMessage,
) -> Result<()> {
    tx.send(msg)
        .await
        .map_err(|_| anyhow::anyhow!("outbound channel closed"))
}

/// Read the active relay attachment and return (relay_id, relay_spiffe_id, attached_at_unix).
/// Fields are empty/zero when no relay is attached. Phase-2 migrations expose
/// only the active relay here — `pending` is invisible to the controller until
/// it promotes to active.
fn relay_attachment_fields(slot: &RelayAttachmentSlot) -> (String, String, i64) {
    match slot.try_active() {
        Some(a) => (a.relay_id, a.relay_spiffe_id, a.attached_at),
        None => (String::new(), String::new(), 0),
    }
}

/// Inputs of a `ConnectorHealthReport` that are fixed for the process
/// lifetime. The variable fields (ACL version, relay attachment) are read at
/// send time.
struct HealthInfo {
    lan_addr: String,
    hostname: String,
    version: String,
    public_ip: String,
}

impl HealthInfo {
    /// An authorization-bearing health report carrying `heartbeat_seq`
    /// (≥ 1, from the process-global [`AckTracker`]).
    fn report(
        &self,
        policy_cache: &PolicyCache,
        relay_attachment_slot: &RelayAttachmentSlot,
        heartbeat_seq: u64,
    ) -> ConnectorControlMessage {
        let (relay_id, relay_spiffe_id, relay_attached_at_unix) =
            relay_attachment_fields(relay_attachment_slot);
        ConnectorControlMessage {
            body: Some(CBody::ConnectorHealth(ConnectorHealthReport {
                version: self.version.clone(),
                hostname: self.hostname.clone(),
                public_ip: self.public_ip.clone(),
                lan_addr: self.lan_addr.clone(),
                acl_version: policy_cache.version(),
                relay_id,
                relay_spiffe_id,
                relay_attached_at_unix,
                heartbeat_seq,
            })),
        }
    }
}

type InboundStream =
    Pin<Box<dyn Stream<Item = std::result::Result<ConnectorControlMessage, tonic::Status>> + Send>>;
type OpenFuture = Pin<Box<dyn Future<Output = Result<ControlStream>> + Send>>;

/// One established controller Control stream: its outbound sender, its
/// inbound reader, and the expiry of the certificate it authenticated with.
/// Dropping it half-closes the stream (the controller sees EOF).
struct ControlStream {
    out_tx: mpsc::Sender<ConnectorControlMessage>,
    inbound: InboundStream,
    cert_not_after_unix: i64,
    /// Authorization-contract state of THIS stream (capability asserted?).
    authz: StreamAuthz,
    /// Keeps the stream's own mTLS channel alive for exactly the stream's
    /// lifetime (as the pre-fix `run_once` local did). None in tests.
    _channel: Option<tonic::transport::Channel>,
}

/// Final step of opening a stream: send the initial `ConnectorHealthReport`.
/// A stream counts as usable only once this succeeded (the controller answers
/// it with an ACL push when the connector's version is stale).
async fn establish_stream(
    out_tx: mpsc::Sender<ConnectorControlMessage>,
    inbound: InboundStream,
    initial_health: ConnectorControlMessage,
    cert_not_after_unix: i64,
    channel: Option<tonic::transport::Channel>,
) -> Result<ControlStream> {
    out_tx
        .send(initial_health)
        .await
        .context("failed to send initial health report")?;
    Ok(ControlStream {
        out_tx,
        inbound,
        cert_not_after_unix,
        authz: StreamAuthz::default(),
        _channel: channel,
    })
}

/// Open a NEW Control stream with the holder's CURRENT certificate: SPIFFE
/// preflight → fresh mTLS channel (a tonic channel's client identity is fixed
/// when it is built) → `Control()` → initial health report. Any existing
/// stream is untouched, so a renewal can open its replacement first
/// (make-before-break).
async fn open_stream(
    cfg: ConnectorConfig,
    certs: Arc<CertHolder>,
    connector_id: String,
    version: String,
    initial_health: ConnectorControlMessage,
    authz: SharedAckTracker,
) -> Result<ControlStream> {
    // Always connect with the holder's CURRENT certificate (Sprint 20 G-2a):
    // after a renewal the new stream presents the renewed certificate.
    let material = certs.current();
    let cert_store = &material.store;

    info!("starting mTLS SPIFFE preflight check");
    verify_controller_spiffe_preflight(&cfg, cert_store)
        .await
        .context("controller SPIFFE preflight failed")?;
    info!("controller SPIFFE identity verified — opening Control stream");

    let channel = build_channel(&cfg, cert_store)
        .await
        .context("failed to build mTLS channel for Control stream")?;
    let mut client = ConnectorServiceClient::new(channel.clone());

    // Outbound: connector → controller (buffered mpsc → ReceiverStream)
    let (out_tx, out_rx) = mpsc::channel::<ConnectorControlMessage>(64);
    // Health reports are timestamped when tonic pulls them for the HTTP/2
    // stream (the transport-send boundary), not when they are enqueued here.
    let outbound = stamp_sends(ReceiverStream::new(out_rx), authz.clone());
    let response = match client.control(Request::new(outbound)).await {
        Ok(r) => r,
        Err(status) => return Err(open_refused(status, &authz)),
    };
    let inbound: InboundStream = Box::pin(response.into_inner());

    info!(
        connector_id = %connector_id,
        version = %version,
        serial = %material.serial_hex,
        "Control stream established — sending initial health report"
    );

    establish_stream(
        out_tx,
        inbound,
        initial_health,
        material.not_after_unix,
        Some(channel),
    )
    .await
}

/// A Control() call refused by the controller. The refusal may carry the
/// authoritative revoked/unknown marker (a trailers-only response); it is
/// classified on the marker only (a marker moves the tracker to its terminal
/// state), then surfaced as an ordinary open error.
fn open_refused(status: tonic::Status, authz: &SharedAckTracker) -> anyhow::Error {
    observe_status(&status, authz, "control_open");
    anyhow::Error::new(status).context("failed to open Control stream")
}

/// What the session needs from the outside world: opening a stream and
/// running a certificate renewal. Production is [`LiveStreamOps`]; tests
/// substitute fakes for the network.
trait StreamOps {
    /// Start opening a new Control stream (owned future, polled by the
    /// control-stream task itself — never a second owner task).
    fn open(&self, initial_health: ConnectorControlMessage) -> OpenFuture;

    /// Handle a ReEnroll: Ok(true) when a renewed certificate was installed
    /// and published, Ok(false) for a duplicate or failed renewal (non-fatal,
    /// as before).
    fn renew(&mut self) -> impl Future<Output = bool> + Send;
}

struct LiveStreamOps<'a> {
    cfg: &'a ConnectorConfig,
    state: &'a mut EnrollmentState,
    certs: &'a Arc<CertHolder>,
    version: &'a str,
    authz: &'a SharedAckTracker,
}

impl StreamOps for LiveStreamOps<'_> {
    fn open(&self, initial_health: ConnectorControlMessage) -> OpenFuture {
        Box::pin(open_stream(
            self.cfg.clone(),
            self.certs.clone(),
            self.state.connector_id.clone(),
            self.version.to_string(),
            initial_health,
            self.authz.clone(),
        ))
    }

    async fn renew(&mut self) -> bool {
        info!("controller requested cert renewal — starting renewal");
        match renewal::renew_cert(self.state, self.cfg, self.certs).await {
            Ok(Some(new_state)) => {
                info!(
                    "cert renewed successfully, new expiry: {}",
                    new_state.cert_not_after
                );
                *self.state = new_state;
                true
            }
            // Duplicate of an in-flight or just-completed renewal.
            Ok(None) => false,
            Err(e) => {
                error!(error = %e, "cert renewal failed");
                false
            }
        }
    }
}

/// Everything the session loop reads from or writes to besides the stream.
struct SessionIo<'a> {
    shield_registry: &'a ShieldRegistry,
    ack_rx: &'a mut mpsc::Receiver<(String, ResourceAck)>,
    log_rx: &'a mut mpsc::Receiver<crate::ControlMessage>,
    policy_cache: &'a Arc<PolicyCache>,
    registry: &'a Arc<SessionRegistry>,
    relay_attachment_slot: &'a RelayAttachmentSlot,
    relay_list_tx: &'a watch::Sender<Option<LabelledRelayList>>,
    health: &'a HealthInfo,
    authz: &'a SharedAckTracker,
}

/// A renewed certificate is waiting for its replacement stream.
struct PendingSwitch {
    /// When the next open attempt may start (existing BACKOFF_* schedule).
    next_attempt: Instant,
    /// Delay applied after the next failed attempt.
    backoff_secs: u64,
}

fn now_unix() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or(0)
}

/// Instant at which a certificate with `not_after_unix` stops being valid.
fn instant_at_unix(not_after_unix: i64) -> Instant {
    let remaining = not_after_unix.saturating_sub(now_unix()).max(0) as u64;
    Instant::now() + Duration::from_secs(remaining)
}

#[allow(clippy::too_many_arguments)]
async fn run_once(
    cfg: &ConnectorConfig,
    state: &mut EnrollmentState,
    shield_registry: &ShieldRegistry,
    ack_rx: &mut mpsc::Receiver<(String, ResourceAck)>,
    log_rx: &mut mpsc::Receiver<crate::ControlMessage>,
    lan_addr: &str,
    hostname: &str,
    version: &str,
    public_ip: &str,
    policy_cache: &Arc<PolicyCache>,
    registry: &Arc<SessionRegistry>,
    relay_attachment_slot: &RelayAttachmentSlot,
    relay_list_tx: &watch::Sender<Option<LabelledRelayList>>,
    certs: &Arc<CertHolder>,
    authz: &SharedAckTracker,
) -> Result<()> {
    let health = HealthInfo {
        lan_addr: lan_addr.to_string(),
        hostname: hostname.to_string(),
        version: version.to_string(),
        public_ip: public_ip.to_string(),
    };
    let mut ops = LiveStreamOps {
        cfg,
        state,
        certs,
        version,
        authz,
    };
    // A new (non-renewal) stream: its initial report carries a NEW seq.
    let seq = authz.lock().next_seq();
    let stream = ops
        .open(health.report(policy_cache, relay_attachment_slot, seq))
        .await?;
    let io = SessionIo {
        shield_registry,
        ack_rx,
        log_rx,
        policy_cache,
        registry,
        relay_attachment_slot,
        relay_list_tx,
        health: &health,
        authz,
    };
    run_session(stream, &mut ops, io).await
}

/// The Control-stream message loop. This task is the single owner of the
/// current stream, `ack_rx` and `log_rx`.
///
/// Certificate renewal is make-before-break + switch-and-drop: after a
/// renewal the replacement stream is opened (with the renewed certificate)
/// while the current one keeps being served. Once the new stream is
/// established, every read and write moves to it and the old stream is
/// dropped unread — it is never read again, so a stale ACL still queued on it
/// can never overwrite newer state. If opening fails, the old stream stays
/// authoritative and the open is retried on the existing BACKOFF_* schedule.
/// Fallback to the plain reconnect path (return from here) happens only when
/// the old stream ends by itself, or when the old stream's certificate
/// reaches `not_after` before a replacement could be opened.
///
/// Returns Ok(()) on a clean end (outer loop reconnects), Err on failure
/// (outer loop backs off).
async fn run_session<O: StreamOps>(
    mut stream: ControlStream,
    ops: &mut O,
    io: SessionIo<'_>,
) -> Result<()> {
    let SessionIo {
        shield_registry,
        ack_rx,
        log_rx,
        policy_cache,
        registry,
        relay_attachment_slot,
        relay_list_tx,
        health,
        authz,
    } = io;

    // Certificate audit bug 3: only a revocation list received on the current
    // stream is authoritative, so a newly (re)connected stream must deliver one
    // before the connector admits new shields again.
    shield_registry.require_revocation_resync();

    let mut pending: Option<PendingSwitch> = None;
    let mut opening: Option<OpenFuture> = None;

    let mut health_ticker = interval(Duration::from_secs(HEALTH_INTERVAL_SECS));
    // Consume the immediate first tick so we don't double-send health on connect.
    health_ticker.tick().await;

    let mut discovery_ticker = interval(Duration::from_secs(DISCOVERY_FLUSH_SECS));
    discovery_ticker.tick().await; // skip first tick

    loop {
        let next_attempt = pending
            .as_ref()
            .map(|p| p.next_attempt)
            .unwrap_or_else(Instant::now);
        let old_cert_expiry = instant_at_unix(stream.cert_not_after_unix);

        tokio::select! {
            result = stream.inbound.next() => {
                match result {
                    Some(Err(e)) => {
                        // Mid-stream termination: classified on the trailer
                        // marker only; unmarked = recoverable.
                        observe_status(&e, authz, "control_stream");
                        return Err(anyhow::anyhow!("control stream recv error: {}", e));
                    }
                    None => {
                        info!("controller closed Control stream");
                        return Ok(());
                    }
                    Some(Ok(msg)) => {
                        observe_controller_msg(&msg, &mut stream.authz, authz);
                        // Terminal: end the session now — no further health
                        // reports, renewals or message handling (§7 step 6).
                        if authz.lock().terminal().is_some() {
                            return Ok(());
                        }
                        match handle_controller_msg(msg, shield_registry, &stream.out_tx, policy_cache, registry, relay_list_tx).await {
                            MsgAction::Continue => {}
                            MsgAction::Fatal(e) => return Err(e),
                            MsgAction::ReEnroll => {
                                if ops.renew().await {
                                    // Open the replacement now, with the renewed
                                    // certificate (open reads certs.current()). If a
                                    // replacement is already pending, keep it: an
                                    // in-flight open is never cancelled, because the
                                    // controller may already have registered it.
                                    if pending.is_none() {
                                        pending = Some(PendingSwitch {
                                            next_attempt: Instant::now(),
                                            backoff_secs: BACKOFF_INITIAL_SECS,
                                        });
                                    }
                                    info!("certificate renewed — opening replacement Control stream (old stream stays active)");
                                }
                            }
                        }
                    }
                }
            }

            _ = sleep_until(next_attempt), if pending.is_some() && opening.is_none() => {
                // Renewal replacement: re-send the outstanding seq (its
                // original send time is kept), so the switch extends nothing.
                let seq = authz.lock().seq_for_replacement();
                opening = Some(ops.open(health.report(policy_cache, relay_attachment_slot, seq)));
            }

            opened = async { opening.as_mut().expect("guarded by is_some").await }, if opening.is_some() => {
                opening = None;
                match opened {
                    Ok(new_stream) => {
                        // Switch-and-drop: the new stream is authoritative from
                        // here on; the old one is dropped (half-closed) unread.
                        let old = std::mem::replace(&mut stream, new_stream);
                        drop(old);
                        pending = None;
                        // The old stream is never read again; the new one
                        // must deliver its own revocation list (bug 3).
                        shield_registry.require_revocation_resync();
                        info!("switched to renewed Control stream — old stream dropped");
                    }
                    Err(e) => {
                        // A replacement refused with a terminal marker.
                        if authz.lock().terminal().is_some() {
                            return Err(e);
                        }
                        if let Some(p) = pending.as_mut() {
                            warn!(
                                error = %e,
                                backoff_secs = p.backoff_secs,
                                "replacement Control stream failed — keeping current stream, retrying with backoff"
                            );
                            p.next_attempt = Instant::now() + Duration::from_secs(p.backoff_secs);
                            p.backoff_secs = (p.backoff_secs * 2).min(BACKOFF_MAX_SECS);
                        }
                    }
                }
            }

            _ = sleep_until(old_cert_expiry), if pending.is_some() => {
                warn!("current Control stream's certificate expired before a replacement stream could be opened — reconnecting");
                return Ok(());
            }

            Some((_, ack)) = ack_rx.recv() => {
                send_msg(&stream.out_tx, ConnectorControlMessage { body: Some(CBody::ResourceAcks(ResourceAckBatch { acks: vec![ack] })), }).await?;
            }

            Some(log_msg) = log_rx.recv() => {
                send_msg(&stream.out_tx, log_msg).await?;
            }

            _ = health_ticker.tick() => {
                info!("health tick — sending ConnectorHealthReport");
                // Drain any pending acks into a batch alongside the health tick.
                let mut acks = Vec::new();
                while let Ok((_, ack)) = ack_rx.try_recv() {
                    acks.push(ack);
                }
                if !acks.is_empty() {
                    send_msg(&stream.out_tx, ConnectorControlMessage { body: Some(CBody::ResourceAcks(ResourceAckBatch { acks })), }).await?;
                }

                let seq = authz.lock().next_seq();
                send_msg(&stream.out_tx, health.report(policy_cache, relay_attachment_slot, seq)).await?;

                let status = shield_registry.get_shield_status_batch();
                if !status.shields.is_empty() {
                    send_msg(&stream.out_tx, ConnectorControlMessage { body: Some(CBody::ShieldStatus(status)), }).await?;
                }

                // ADR-004 Phase 3: flush buffered shield actual-state reports
                // upstream for reconciliation.
                if let Some(state_batch) = shield_registry.drain_state_batch() {
                    send_msg(&stream.out_tx, state_batch).await?;
                }
            }

            _ = discovery_ticker.tick() => {
                if let Some(batch_msg) = shield_registry.drain_discovery_batch() {
                    let report_count = match &batch_msg.body {
                        Some(crate::proto::connector_control_message::Body::ShieldDiscovery(b)) => b.reports.len(),
                        _ => 0,
                    };
                    info!(report_count, "flushing ShieldDiscoveryBatch upstream");
                    send_msg(&stream.out_tx, batch_msg).await?;
                }
            }
        }
    }
}

/// Outcome of handling one controller message.
enum MsgAction {
    Continue,
    /// The stream is broken; end the session with this error.
    Fatal(anyhow::Error),
    /// The controller asked for certificate renewal.
    ReEnroll,
}

#[allow(clippy::too_many_arguments)]
async fn handle_controller_msg(
    msg: ConnectorControlMessage,
    shield_registry: &ShieldRegistry,
    out_tx: &mpsc::Sender<ConnectorControlMessage>,
    policy_cache: &Arc<PolicyCache>,
    registry: &Arc<SessionRegistry>,
    relay_list_tx: &watch::Sender<Option<LabelledRelayList>>,
) -> MsgAction {
    match msg.body {
        Some(CBody::ResourceInstructions(batch)) => {
            for (shield_id, instr_batch) in batch.shield_resources {
                shield_registry.push_instructions(&shield_id, instr_batch.instructions);
            }
            MsgAction::Continue
        }
        // ADR-004 Phase 2: desired-state snapshots — cache per shield and
        // forward live if the shield is connected (replayed on shield reconnect).
        Some(CBody::ResourceSnapshots(batch)) => {
            for (shield_id, snapshot) in batch.shield_snapshots {
                shield_registry.push_snapshot(&shield_id, snapshot);
            }
            MsgAction::Continue
        }
        Some(CBody::ScanCommand(cmd)) => {
            let connector_id = shield_registry.connector_id().to_string();
            let out_tx = out_tx.clone();
            tokio::spawn(async move {
                let scan_cmd = ScanCommand {
                    request_id: cmd.request_id.clone(),
                    targets: cmd.targets,
                    ports: cmd.ports.into_iter().map(|p| p as u16).collect(),
                    max_targets: cmd.max_targets,
                    timeout_sec: cmd.timeout_sec as u64,
                };
                let report = execute_scan(scan_cmd, &connector_id).await;
                let proto_results: Vec<ProtoScanResult> = report
                    .results
                    .into_iter()
                    .map(|r| ProtoScanResult {
                        ip: r.ip,
                        port: r.port as u32,
                        protocol: r.protocol,
                        service_name: r.service_name,
                        reachable_from: r.reachable_from,
                        first_seen: r.first_seen,
                    })
                    .collect();
                let proto_report = ConnectorControlMessage {
                    body: Some(CBody::ScanReport(ProtoScanReport {
                        request_id: report.request_id,
                        results: proto_results,
                        error: report.error.unwrap_or_default(),
                    })),
                };
                let _ = out_tx.send(proto_report).await;
            });
            MsgAction::Continue
        }
        Some(CBody::Ping(p)) => {
            let pong = ConnectorControlMessage {
                body: Some(CBody::Pong(crate::shield_proto::Pong {
                    timestamp_unix: p.timestamp_unix,
                })),
            };
            if out_tx.send(pong).await.is_err() {
                return MsgAction::Fatal(anyhow::anyhow!("outbound channel closed on pong"));
            }
            MsgAction::Continue
        }
        // Renewal itself runs in the session loop (make-before-break).
        Some(CBody::ReEnroll(_)) => MsgAction::ReEnroll,
        Some(CBody::AclSnapshot(snap)) => {
            let version = snap.version;
            let workspace_id = snap.workspace_id.clone();
            let revoked = policy_cache.update_and_revoked(snap);
            for key in &revoked {
                registry.cancel_all(key);
            }
            if !revoked.is_empty() {
                info!(
                    count = revoked.len(),
                    "ACL diff: revoked sessions torn down"
                );
            }
            info!(version, %workspace_id, "ACL snapshot stored");
            MsgAction::Continue
        }
        Some(CBody::AgentPolicy(policy)) => {
            shield_registry.set_shield_renewal_window(policy.shield_renewal_window_secs);
            MsgAction::Continue
        }
        // Certificate audit bug 3: the authoritative revoked-shield list.
        Some(CBody::ShieldRevocations(list)) => {
            shield_registry.apply_revocations(list.revoked_shield_ids);
            MsgAction::Continue
        }
        Some(CBody::RelayList(list)) => {
            // Sprint 11 ADR-016: forward to the relay selector via the watch
            // channel. The selector owns version-skip decisions; we just
            // hand off the latest payload. A send error means the selector
            // task has dropped its receiver — non-fatal for the control stream.
            let version = list.version;
            let relay_count = list.relays.len();
            if relay_list_tx.send(Some(list)).is_err() {
                warn!(
                    version,
                    "no relay selector listening; dropping LabelledRelayList push"
                );
            } else {
                info!(version, relay_count, "received LabelledRelayList push");
            }
            MsgAction::Continue
        }
        _ => MsgAction::Continue,
    }
}

#[cfg(test)]
mod renewal_switch_tests {
    //! Make-before-break + switch-and-drop certificate renewal of the
    //! controller Control stream. The network is faked at the `StreamOps`
    //! seam; the session loop, message handling and the final
    //! initial-health step (`establish_stream`) are the production code.

    use super::*;
    use crate::authorization::{AckTracker, ServingState};
    use crate::client::v1::AclSnapshot;
    use crate::shield_proto::{Ping, ReEnrollSignal};
    use parking_lot::Mutex;
    use std::collections::VecDeque;
    use tonic::transport::Channel;

    const WAIT: Duration = Duration::from_secs(5);

    type InTx = mpsc::Sender<std::result::Result<ConnectorControlMessage, tonic::Status>>;

    /// The test's end of one fake Control stream.
    struct Peer {
        /// Messages the connector sent on this stream.
        out_rx: mpsc::Receiver<ConnectorControlMessage>,
        /// Inject controller → connector messages.
        in_tx: InTx,
    }

    /// Parts handed to the session for one fake stream.
    struct Parts {
        out_tx: mpsc::Sender<ConnectorControlMessage>,
        inbound: InboundStream,
        cert_not_after_unix: i64,
    }

    fn fake_stream(cert_not_after_unix: i64) -> (Parts, Peer) {
        let (out_tx, out_rx) = mpsc::channel(64);
        let (in_tx, in_rx) = mpsc::channel(64);
        (
            Parts {
                out_tx,
                inbound: Box::pin(ReceiverStream::new(in_rx)),
                cert_not_after_unix,
            },
            Peer { out_rx, in_tx },
        )
    }

    /// A fake stream whose "transport" (the test peer) receives the
    /// connector's messages through the production [`stamp_sends`]: the
    /// peer's receipt is the transport-send boundary.
    fn fake_stream_stamped(cert_not_after_unix: i64, authz: &SharedAckTracker) -> (Parts, Peer) {
        let (out_tx, conn_rx) = mpsc::channel::<ConnectorControlMessage>(64);
        let (peer_tx, out_rx) = mpsc::channel(64);
        let (in_tx, in_rx) = mpsc::channel(64);
        let mut stamped = Box::pin(stamp_sends(ReceiverStream::new(conn_rx), authz.clone()));
        tokio::spawn(async move {
            while let Some(m) = stamped.next().await {
                if peer_tx.send(m).await.is_err() {
                    break;
                }
            }
        });
        (
            Parts {
                out_tx,
                inbound: Box::pin(ReceiverStream::new(in_rx)),
                cert_not_after_unix,
            },
            Peer { out_rx, in_tx },
        )
    }

    fn far_future() -> i64 {
        now_unix() + 3600
    }

    enum Script {
        /// The open succeeds with this stream (initial health is sent on it).
        Ok(Parts),
        /// The open fails (preflight / channel / Control() error).
        Err,
        /// The controller refuses Control() with this status (production
        /// [`open_refused`] classification).
        Refuse(tonic::Status),
        /// The open never completes.
        Hang,
    }

    #[derive(Default)]
    struct Shared {
        script: VecDeque<Script>,
        /// Time of each open attempt.
        opens: Vec<Instant>,
        /// Whether the old stream's sender was still alive at each open.
        old_alive_at_open: Vec<bool>,
        renew_calls: usize,
    }

    struct FakeOps {
        shared: Arc<Mutex<Shared>>,
        renew_result: bool,
        /// Weak handle to the session's original stream sender.
        old_out: mpsc::WeakSender<ConnectorControlMessage>,
        authz: SharedAckTracker,
    }

    impl StreamOps for FakeOps {
        fn open(&self, initial_health: ConnectorControlMessage) -> OpenFuture {
            let mut sh = self.shared.lock();
            sh.opens.push(Instant::now());
            sh.old_alive_at_open.push(self.old_out.upgrade().is_some());
            let script = sh.script.pop_front().unwrap_or(Script::Hang);
            drop(sh);
            let authz = self.authz.clone();
            Box::pin(async move {
                match script {
                    Script::Ok(p) => {
                        establish_stream(
                            p.out_tx,
                            p.inbound,
                            initial_health,
                            p.cert_not_after_unix,
                            None,
                        )
                        .await
                    }
                    Script::Err => Err(anyhow::anyhow!("simulated open failure")),
                    Script::Refuse(status) => Err(open_refused(status, &authz)),
                    Script::Hang => std::future::pending().await,
                }
            })
        }

        async fn renew(&mut self) -> bool {
            self.shared.lock().renew_calls += 1;
            self.renew_result
        }
    }

    struct Env {
        shield_registry: ShieldRegistry,
        ack_tx: mpsc::Sender<(String, ResourceAck)>,
        ack_rx: mpsc::Receiver<(String, ResourceAck)>,
        _log_tx: mpsc::Sender<crate::ControlMessage>,
        log_rx: mpsc::Receiver<crate::ControlMessage>,
        policy_cache: Arc<PolicyCache>,
        registry: Arc<SessionRegistry>,
        slot: RelayAttachmentSlot,
        relay_list_tx: watch::Sender<Option<LabelledRelayList>>,
        health: HealthInfo,
        authz: SharedAckTracker,
    }

    fn env() -> Env {
        let policy_cache = Arc::new(PolicyCache::new());
        let (shield_ack_tx, _) = mpsc::channel(8);
        let (ack_tx, ack_rx) = mpsc::channel(8);
        let (log_tx, log_rx) = mpsc::channel(8);
        Env {
            shield_registry: ShieldRegistry::new(
                Channel::from_static("http://127.0.0.1:1").connect_lazy(),
                crate::test_support::TRUST_DOMAIN.to_string(),
                crate::test_support::CONNECTOR_ID.to_string(),
                shield_ack_tx,
                policy_cache.clone(),
                crate::authorization::AuthzGate::open_for_tests(),
            ),
            ack_tx,
            ack_rx,
            _log_tx: log_tx,
            log_rx,
            policy_cache,
            registry: Arc::new(SessionRegistry::new()),
            slot: RelayAttachmentSlot::new(),
            relay_list_tx: watch::channel(None).0,
            health: HealthInfo {
                lan_addr: "10.0.0.1:9091".into(),
                hostname: "host".into(),
                version: "test".into(),
                public_ip: String::new(),
            },
            authz: AckTracker::shared(AUTHORIZATION_FRESHNESS),
        }
    }

    /// K2: an AgentPolicy from the controller sets the Shield renewal window.
    #[tokio::test]
    async fn agent_policy_sets_the_shield_renewal_window() {
        let e = env();
        let (out_tx, _out_rx) = mpsc::channel(8);
        let msg = ConnectorControlMessage {
            body: Some(CBody::AgentPolicy(crate::proto::AgentPolicy {
                shield_renewal_window_secs: 1440,
                authorization_acks: true,
            })),
        };
        let action = handle_controller_msg(
            msg,
            &e.shield_registry,
            &out_tx,
            &e.policy_cache,
            &e.registry,
            &e.relay_list_tx,
        )
        .await;
        assert!(matches!(action, MsgAction::Continue));
        assert_eq!(e.shield_registry.shield_renewal_window_secs(), 1440);
    }

    /// Certificate audit bug 3: the controller's revocation list reaches the
    /// Shield registry.
    #[tokio::test]
    async fn shield_revocations_reach_the_shield_registry() {
        let e = env();
        let (out_tx, _out_rx) = mpsc::channel(8);
        let msg = ConnectorControlMessage {
            body: Some(CBody::ShieldRevocations(
                crate::proto::ShieldRevocationList {
                    revoked_shield_ids: vec!["5c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f".into()],
                },
            )),
        };
        let action = handle_controller_msg(
            msg,
            &e.shield_registry,
            &out_tx,
            &e.policy_cache,
            &e.registry,
            &e.relay_list_tx,
        )
        .await;
        assert!(matches!(action, MsgAction::Continue));
        assert!(e
            .shield_registry
            .is_revoked("5c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"));
    }

    fn io(e: &mut Env) -> SessionIo<'_> {
        SessionIo {
            shield_registry: &e.shield_registry,
            ack_rx: &mut e.ack_rx,
            log_rx: &mut e.log_rx,
            policy_cache: &e.policy_cache,
            registry: &e.registry,
            relay_attachment_slot: &e.slot,
            relay_list_tx: &e.relay_list_tx,
            health: &e.health,
            authz: &e.authz,
        }
    }

    fn into_stream(p: Parts) -> ControlStream {
        ControlStream {
            out_tx: p.out_tx,
            inbound: p.inbound,
            cert_not_after_unix: p.cert_not_after_unix,
            authz: StreamAuthz::default(),
            _channel: None,
        }
    }

    fn fake_ops(
        script: Vec<Script>,
        renew_result: bool,
        old: &Parts,
    ) -> (FakeOps, Arc<Mutex<Shared>>) {
        fake_ops_with(
            script,
            renew_result,
            old,
            AckTracker::shared(AUTHORIZATION_FRESHNESS),
        )
    }

    fn fake_ops_with(
        script: Vec<Script>,
        renew_result: bool,
        old: &Parts,
        authz: SharedAckTracker,
    ) -> (FakeOps, Arc<Mutex<Shared>>) {
        let shared = Arc::new(Mutex::new(Shared {
            script: script.into(),
            ..Default::default()
        }));
        (
            FakeOps {
                shared: shared.clone(),
                renew_result,
                old_out: old.out_tx.downgrade(),
                authz,
            },
            shared,
        )
    }

    fn reenroll() -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::ReEnroll(ReEnrollSignal {})),
        }
    }

    fn ping(ts: i64) -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::Ping(Ping { timestamp_unix: ts })),
        }
    }

    fn acl(version: u64) -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::AclSnapshot(AclSnapshot {
                version,
                workspace_id: "ws".into(),
                ..Default::default()
            })),
        }
    }

    fn ack(id: &str) -> ResourceAck {
        ResourceAck {
            resource_id: id.into(),
            ..Default::default()
        }
    }

    async fn recv(rx: &mut mpsc::Receiver<ConnectorControlMessage>) -> ConnectorControlMessage {
        tokio::time::timeout(WAIT, rx.recv())
            .await
            .expect("timed out waiting for a message")
            .expect("stream sender dropped")
    }

    async fn expect_pong(peer: &mut Peer, ts: i64) {
        match recv(&mut peer.out_rx).await.body {
            Some(CBody::Pong(p)) => assert_eq!(p.timestamp_unix, ts),
            other => panic!("expected Pong, got {other:?}"),
        }
    }

    async fn wait_for<F: Fn() -> bool>(what: &str, f: F) {
        let deadline = Instant::now() + WAIT;
        while !f() {
            assert!(Instant::now() < deadline, "timed out waiting for {what}");
            tokio::time::sleep(Duration::from_millis(5)).await;
        }
    }

    /// Renewal opens the new stream while the old one is alive, switches to
    /// it, sends its initial health report, and drops the old stream — with
    /// no renewal-path sleep.
    #[tokio::test]
    async fn renewal_opens_new_stream_first_then_switches_and_drops_old() {
        let mut e = env();
        e.policy_cache.update(AclSnapshot {
            version: 7,
            ..Default::default()
        });
        let (old, mut old_peer) = fake_stream(far_future());
        let (new, mut new_peer) = fake_stream(far_future());
        let (mut ops, shared) = fake_ops(vec![Script::Ok(new)], true, &old);
        let ack_tx = e.ack_tx.clone();

        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        let driver = async {
            let started = Instant::now();
            old_peer.in_tx.send(Ok(reenroll())).await.unwrap();

            // 5. The new stream's first message is its initial health report.
            match recv(&mut new_peer.out_rx).await.body {
                Some(CBody::ConnectorHealth(h)) => {
                    assert_eq!(
                        h.acl_version, 7,
                        "initial health carries the cached ACL version"
                    );
                    assert_eq!(h.lan_addr, "10.0.0.1:9091");
                }
                other => panic!("expected initial ConnectorHealth, got {other:?}"),
            }
            assert!(
                started.elapsed() < Duration::from_millis(900),
                "renewal switch took {:?} — no renewal-path sleep expected",
                started.elapsed()
            );

            // 1. The open ran while the old stream was still alive.
            {
                let sh = shared.lock();
                assert_eq!(sh.renew_calls, 1);
                assert_eq!(sh.opens.len(), 1, "exactly one replacement open");
                assert_eq!(
                    sh.old_alive_at_open,
                    vec![true],
                    "new stream opened before old dropped"
                );
            }

            // 2/3. Old stream dropped: its sender is gone (controller sees EOF)
            // and its inbound is no longer read.
            assert!(
                tokio::time::timeout(WAIT, old_peer.out_rx.recv())
                    .await
                    .unwrap()
                    .is_none(),
                "old stream half-closed after the switch"
            );
            wait_for("old inbound dropped", || old_peer.in_tx.is_closed()).await;

            // Traffic now flows only on the new stream.
            new_peer.in_tx.send(Ok(ping(42))).await.unwrap();
            expect_pong(&mut new_peer, 42).await;
            ack_tx.send(("shield".into(), ack("r1"))).await.unwrap();
            match recv(&mut new_peer.out_rx).await.body {
                Some(CBody::ResourceAcks(b)) => assert_eq!(b.acks[0].resource_id, "r1"),
                other => panic!("expected ResourceAcks on new stream, got {other:?}"),
            }
        };
        tokio::select! {
            r = session => panic!("session ended unexpectedly: {r:?}"),
            _ = driver => {}
        }
    }

    fn revocations(ids: &[&str]) -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::ShieldRevocations(
                crate::proto::ShieldRevocationList {
                    revoked_shield_ids: ids.iter().map(|s| s.to_string()).collect(),
                },
            )),
        }
    }

    /// Certificate audit bug 3: only a revocation list received on the
    /// current Control stream is authoritative. When a new stream becomes
    /// current — a (re)connect or a renewal switch — the connector admits no
    /// new shield until that stream has delivered its list.
    #[tokio::test]
    async fn a_new_control_stream_must_deliver_its_revocation_list_before_shields_are_admitted() {
        let mut e = env();
        let shields = e.shield_registry.clone();
        shields.apply_revocations(vec![]); // synced on an earlier stream
        let (old, old_peer) = fake_stream(far_future());
        let (new, mut new_peer) = fake_stream(far_future());
        let (mut ops, _shared) = fake_ops(vec![Script::Ok(new)], true, &old);

        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        let driver = async {
            wait_for("reconnected stream requires a fresh list", || {
                !shields.revocations_synced()
            })
            .await;
            old_peer.in_tx.send(Ok(revocations(&[]))).await.unwrap();
            wait_for("list on the current stream applied", || {
                shields.revocations_synced()
            })
            .await;

            // Renewal switch: the replacement stream must deliver its own list.
            old_peer.in_tx.send(Ok(reenroll())).await.unwrap();
            let _initial_health = recv(&mut new_peer.out_rx).await;
            wait_for("switched stream requires a fresh list", || {
                !shields.revocations_synced()
            })
            .await;
            new_peer
                .in_tx
                .send(Ok(revocations(&["5c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"])))
                .await
                .unwrap();
            wait_for("list on the new stream applied", || {
                shields.revocations_synced()
            })
            .await;
            assert!(shields.is_revoked("5c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f"));
        };
        tokio::select! {
            r = session => panic!("session ended unexpectedly: {r:?}"),
            _ = driver => {}
        }
    }

    /// A stale ACL that would arrive on the old stream after the switch can't
    /// overwrite the state applied from the new stream.
    #[tokio::test]
    async fn stale_acl_on_old_stream_cannot_overwrite_after_switch() {
        let mut e = env();
        let (old, old_peer) = fake_stream(far_future());
        let (new, mut new_peer) = fake_stream(far_future());
        let (mut ops, _shared) = fake_ops(vec![Script::Ok(new)], true, &old);
        let cache = e.policy_cache.clone();

        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        let driver = async {
            old_peer.in_tx.send(Ok(acl(3))).await.unwrap();
            wait_for("ACL v3 from old stream", || cache.version() == 3).await;

            old_peer.in_tx.send(Ok(reenroll())).await.unwrap();
            let _initial_health = recv(&mut new_peer.out_rx).await;

            new_peer.in_tx.send(Ok(acl(5))).await.unwrap();
            wait_for("ACL v5 from new stream", || cache.version() == 5).await;

            // The old inbound was dropped at the switch: a late stale snapshot
            // can't even be delivered, so it can't be applied.
            assert!(
                old_peer.in_tx.send(Ok(acl(2))).await.is_err(),
                "old stream must not be read after the switch"
            );
            tokio::time::sleep(Duration::from_millis(100)).await;
            assert_eq!(
                cache.version(),
                5,
                "stale ACL must not overwrite newer state"
            );
        };
        tokio::select! {
            r = session => panic!("session ended unexpectedly: {r:?}"),
            _ = driver => {}
        }
    }

    /// A failed replacement open keeps the old stream fully usable, and the
    /// retries follow the existing BACKOFF_* schedule (immediate, +2 s, +4 s).
    #[tokio::test]
    async fn new_stream_failure_keeps_old_stream_and_retries_with_backoff() {
        let mut e = env();
        let (old, mut old_peer) = fake_stream(far_future());
        let (new, mut new_peer) = fake_stream(far_future());
        let (mut ops, shared) =
            fake_ops(vec![Script::Err, Script::Err, Script::Ok(new)], true, &old);

        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        let driver = async {
            old_peer.in_tx.send(Ok(reenroll())).await.unwrap();
            wait_for("first open attempt", || shared.lock().opens.len() == 1).await;

            // 6. After the failure the old stream is still served.
            old_peer.in_tx.send(Ok(ping(1))).await.unwrap();
            expect_pong(&mut old_peer, 1).await;
            assert!(
                !old_peer.in_tx.is_closed(),
                "old stream kept after a failed open"
            );

            // 7. Retries back off 2 s, then 4 s, then the swap completes.
            let _initial_health =
                tokio::time::timeout(Duration::from_secs(10), new_peer.out_rx.recv())
                    .await
                    .expect("swap after retries")
                    .expect("new stream open");
            let sh = shared.lock();
            assert_eq!(sh.opens.len(), 3);
            let gap1 = sh.opens[1] - sh.opens[0];
            let gap2 = sh.opens[2] - sh.opens[1];
            assert!(
                gap1 >= Duration::from_secs(BACKOFF_INITIAL_SECS)
                    && gap1 < Duration::from_secs(BACKOFF_INITIAL_SECS + 1),
                "first retry after {gap1:?}, want ~{BACKOFF_INITIAL_SECS}s"
            );
            assert!(
                gap2 >= Duration::from_secs(BACKOFF_INITIAL_SECS * 2)
                    && gap2 < Duration::from_secs(BACKOFF_INITIAL_SECS * 2 + 1),
                "second retry after {gap2:?}, want ~{}s",
                BACKOFF_INITIAL_SECS * 2
            );
            assert!(
                sh.old_alive_at_open.iter().all(|a| *a),
                "old stream alive during every attempt"
            );
        };
        tokio::select! {
            r = session => panic!("session ended unexpectedly: {r:?}"),
            _ = driver => {}
        }
    }

    /// Fallback 1: the old stream ends while the replacement is still being
    /// opened → the session ends cleanly (outer loop reconnects with the
    /// renewed certificate — today's path).
    #[tokio::test]
    async fn fallback_when_old_stream_ends_before_replacement() {
        let mut e = env();
        let (old, old_peer) = fake_stream(far_future());
        let (mut ops, shared) = fake_ops(vec![Script::Hang], true, &old);

        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        tokio::pin!(session);
        old_peer.in_tx.send(Ok(reenroll())).await.unwrap();
        tokio::select! {
            r = &mut session => panic!("session ended before the old stream did: {r:?}"),
            _ = wait_for("open attempt", || shared.lock().opens.len() == 1) => {}
        }
        drop(old_peer); // controller closes the old stream
        let r = tokio::time::timeout(WAIT, session)
            .await
            .expect("session ends");
        assert!(r.is_ok(), "clean end → outer-loop reconnect, got {r:?}");
    }

    /// Fallback 2: the old stream's certificate reaches not_after before a
    /// replacement could be opened → the session ends cleanly instead of
    /// relying on an expired identity.
    #[tokio::test]
    async fn fallback_when_old_certificate_expires_before_replacement() {
        let mut e = env();
        let (old, old_peer) = fake_stream(now_unix() + 1);
        let (mut ops, _shared) = fake_ops(vec![Script::Err, Script::Err, Script::Err], true, &old);

        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        old_peer.in_tx.send(Ok(reenroll())).await.unwrap();
        let r = tokio::time::timeout(WAIT, session)
            .await
            .expect("session ends at old-cert expiry");
        assert!(r.is_ok(), "clean end → outer-loop reconnect, got {r:?}");
    }

    /// The expiry fallback is armed only while a replacement is pending: an
    /// expired-cert stream with no renewal in flight is left alone (an
    /// established stream is not re-validated — unchanged behaviour).
    #[tokio::test]
    async fn expiry_fallback_only_while_replacement_pending() {
        let mut e = env();
        let (old, mut old_peer) = fake_stream(now_unix() - 10);
        let (mut ops, shared) = fake_ops(vec![], true, &old);

        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        let driver = async {
            tokio::time::sleep(Duration::from_millis(200)).await;
            old_peer.in_tx.send(Ok(ping(9))).await.unwrap();
            expect_pong(&mut old_peer, 9).await;
            assert_eq!(shared.lock().opens.len(), 0);
        };
        tokio::select! {
            r = session => panic!("session ended without a pending replacement: {r:?}"),
            _ = driver => {}
        }
    }

    /// A duplicate / failed renewal (no new certificate) opens nothing and
    /// keeps the current stream.
    #[tokio::test]
    async fn duplicate_or_failed_renewal_opens_nothing() {
        let mut e = env();
        let (old, mut old_peer) = fake_stream(far_future());
        let (mut ops, shared) = fake_ops(vec![], false, &old);

        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        let driver = async {
            old_peer.in_tx.send(Ok(reenroll())).await.unwrap();
            old_peer.in_tx.send(Ok(ping(3))).await.unwrap();
            expect_pong(&mut old_peer, 3).await;
            let sh = shared.lock();
            assert_eq!(sh.renew_calls, 1);
            assert_eq!(sh.opens.len(), 0);
        };
        tokio::select! {
            r = session => panic!("session ended unexpectedly: {r:?}"),
            _ = driver => {}
        }
    }

    /// Non-renewal endings are unchanged: a clean controller close is Ok(())
    /// (outer loop: 1 s then reconnect), a recv error is Err (outer loop:
    /// BACKOFF_*).
    #[tokio::test]
    async fn non_renewal_endings_unchanged() {
        let mut e = env();
        let (old, old_peer) = fake_stream(far_future());
        let (mut ops, _) = fake_ops(vec![], true, &old);
        drop(old_peer);
        assert!(run_session(into_stream(old), &mut ops, io(&mut e))
            .await
            .is_ok());

        let mut e = env();
        let (old, old_peer) = fake_stream(far_future());
        let (mut ops, _) = fake_ops(vec![], true, &old);
        old_peer
            .in_tx
            .send(Err(tonic::Status::unavailable("boom")))
            .await
            .unwrap();
        assert!(run_session(into_stream(old), &mut ops, io(&mut e))
            .await
            .is_err());
    }

    // ── Phase A authorization-contract plumbing (session level) ────────────

    fn policy_msg(acks: bool) -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::AgentPolicy(crate::proto::AgentPolicy {
                shield_renewal_window_secs: 0,
                authorization_acks: acks,
            })),
        }
    }

    fn authz_ack(seq: u64) -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::AuthorizationAck(crate::proto::AuthorizationAck {
                heartbeat_seq: seq,
            })),
        }
    }

    fn revoked_event() -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::ConnectorRevoked(crate::proto::ConnectorRevoked {})),
        }
    }

    fn marked_status(code: tonic::Code, marker: Option<&str>) -> tonic::Status {
        let mut md = tonic::metadata::MetadataMap::new();
        if let Some(m) = marker {
            md.insert(
                crate::authorization::CONNECTOR_STATE_MARKER_KEY,
                m.parse().unwrap(),
            );
        }
        tonic::Status::with_metadata(code, "connector is revoked", md)
    }

    fn health_seq(m: &ConnectorControlMessage) -> u64 {
        match &m.body {
            Some(CBody::ConnectorHealth(h)) => h.heartbeat_seq,
            other => panic!("expected ConnectorHealth, got {other:?}"),
        }
    }

    /// T10/T28: a make-before-break renewal re-sends the outstanding seq on the
    /// replacement stream with its ORIGINAL send time, an ack for it on the new
    /// stream is accepted exactly once, and the renewal produces no error and
    /// no terminal state.
    #[tokio::test]
    async fn renewal_reuses_outstanding_seq_and_keeps_its_send_time() {
        let mut e = env();
        let authz = e.authz.clone();
        let (old, mut old_peer) = fake_stream_stamped(far_future(), &authz);
        let (new, mut new_peer) = fake_stream_stamped(far_future(), &authz);

        // An authorization request already sent on the old stream.
        let s1 = authz.lock().next_seq();
        old.out_tx
            .send(e.health.report(&e.policy_cache, &e.slot, s1))
            .await
            .unwrap();
        assert_eq!(health_seq(&recv(&mut old_peer.out_rx).await), s1);
        let first_sent = authz.lock().sent_at(s1).expect("stamped at transport pull");

        let (mut ops, _shared) = fake_ops_with(vec![Script::Ok(new)], true, &old, authz.clone());
        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        let driver = async {
            old_peer.in_tx.send(Ok(policy_msg(true))).await.unwrap();
            tokio::time::sleep(Duration::from_millis(20)).await;
            old_peer.in_tx.send(Ok(reenroll())).await.unwrap();

            let initial = recv(&mut new_peer.out_rx).await;
            assert_eq!(
                health_seq(&initial),
                s1,
                "replacement re-sends the outstanding seq"
            );
            assert_eq!(
                authz.lock().sent_at(s1),
                Some(first_sent),
                "moving to the replacement stream must not move the send time"
            );

            // The controller answers on the replacement stream.
            new_peer.in_tx.send(Ok(policy_msg(true))).await.unwrap();
            new_peer.in_tx.send(Ok(authz_ack(s1))).await.unwrap();
            wait_for("ack accepted", || authz.lock().highest_accepted() == s1).await;
            assert_eq!(authz.lock().latest_accepted_sent_at(), Some(first_sent));
            // B1 (AB1-13): the switch extends nothing — D is the ORIGINAL send
            // time + H.
            assert_eq!(
                authz.lock().deadline(),
                Some(first_sent + AUTHORIZATION_FRESHNESS)
            );

            // A second ack for the same request (e.g. from the other stream) is
            // a duplicate: nothing changes.
            new_peer.in_tx.send(Ok(authz_ack(s1))).await.unwrap();
            new_peer.in_tx.send(Ok(ping(5))).await.unwrap();
            expect_pong(&mut new_peer, 5).await;
            assert_eq!(authz.lock().highest_accepted(), s1);
            assert_eq!(authz.lock().terminal(), None, "renewal is never terminal");
        };
        tokio::select! {
            r = session => panic!("session ended unexpectedly: {r:?}"),
            _ = driver => {}
        }
    }

    /// An ack on a replacement stream that has not asserted the capability yet
    /// is rejected; capability is per stream.
    #[tokio::test]
    async fn ack_on_stream_without_capability_is_rejected() {
        let mut e = env();
        let authz = e.authz.clone();
        let (old, mut old_peer) = fake_stream_stamped(far_future(), &authz);
        let s1 = authz.lock().next_seq();
        old.out_tx
            .send(e.health.report(&e.policy_cache, &e.slot, s1))
            .await
            .unwrap();
        let _ = recv(&mut old_peer.out_rx).await;
        let (mut ops, _) = fake_ops_with(vec![], true, &old, authz.clone());
        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        let driver = async {
            // Old controller: no AgentPolicy capability, just an ack.
            old_peer.in_tx.send(Ok(ping(1))).await.unwrap();
            old_peer.in_tx.send(Ok(authz_ack(s1))).await.unwrap();
            old_peer.in_tx.send(Ok(ping(2))).await.unwrap();
            expect_pong(&mut old_peer, 1).await;
            expect_pong(&mut old_peer, 2).await;
            assert_eq!(
                authz.lock().highest_accepted(),
                0,
                "no capability → no ack accepted"
            );
        };
        tokio::select! {
            r = session => panic!("session ended unexpectedly: {r:?}"),
            _ = driver => {}
        }
    }

    /// Revocation racing a renewal (connector side): the replacement is still
    /// opening, so the connector is still reading the OLD stream — the
    /// controller delivers ConnectorRevoked there too, and it is recognised.
    #[tokio::test]
    async fn connector_revoked_on_old_stream_while_replacement_pending() {
        let mut e = env();
        let authz = e.authz.clone();
        let (old, old_peer) = fake_stream(far_future());
        let (mut ops, shared) = fake_ops_with(vec![Script::Hang], true, &old, authz.clone());
        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        tokio::pin!(session);
        old_peer.in_tx.send(Ok(reenroll())).await.unwrap();
        tokio::select! {
            r = &mut session => panic!("session ended early: {r:?}"),
            _ = wait_for("replacement open in flight", || shared.lock().opens.len() == 1) => {}
        }
        old_peer.in_tx.send(Ok(revoked_event())).await.unwrap();
        // B1: the session ends on the event itself, without waiting for the
        // controller's marker, and the pending replacement is abandoned.
        let r = tokio::time::timeout(WAIT, session)
            .await
            .expect("session ends on ConnectorRevoked");
        assert!(r.is_ok(), "{r:?}");
        assert_eq!(
            authz.lock().terminal(),
            Some(crate::authorization::TerminalSignal::Revoked)
        );
        assert_eq!(authz.lock().state(), ServingState::Revoked);
        assert_eq!(shared.lock().opens.len(), 1, "no further open attempts");
    }

    /// Revocation after the switch: delivered on the replacement stream the
    /// connector now reads.
    #[tokio::test]
    async fn connector_revoked_on_new_stream_after_switch() {
        let mut e = env();
        let authz = e.authz.clone();
        let (old, old_peer) = fake_stream(far_future());
        let (new, mut new_peer) = fake_stream(far_future());
        let (mut ops, _) = fake_ops_with(vec![Script::Ok(new)], true, &old, authz.clone());
        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        tokio::pin!(session);
        let driver = async {
            old_peer.in_tx.send(Ok(reenroll())).await.unwrap();
            let _initial = recv(&mut new_peer.out_rx).await; // switched
            new_peer.in_tx.send(Ok(revoked_event())).await.unwrap();
        };
        tokio::select! {
            r = &mut session => panic!("session ended before the switch: {r:?}"),
            _ = driver => {}
        }
        // B1: the session ends on the event; no further health report.
        let r = tokio::time::timeout(WAIT, session)
            .await
            .expect("session ends on ConnectorRevoked");
        assert!(r.is_ok(), "{r:?}");
        assert_eq!(authz.lock().state(), ServingState::Revoked);
        assert!(
            new_peer.out_rx.try_recv().is_err(),
            "nothing is sent after the terminal signal"
        );
    }

    /// Revocation racing a renewal, other order: the revoke committed before
    /// the replacement activated, so the controller refuses the replacement
    /// Control() with the revoked marker. Recognised via the production
    /// open-refusal path.
    #[tokio::test]
    async fn replacement_refused_with_revoked_marker() {
        let mut e = env();
        let authz = e.authz.clone();
        let (old, mut old_peer) = fake_stream(far_future());
        let (mut ops, shared) = fake_ops_with(
            vec![Script::Refuse(marked_status(
                tonic::Code::PermissionDenied,
                Some("revoked"),
            ))],
            true,
            &old,
            authz.clone(),
        );
        let session = run_session(into_stream(old), &mut ops, io(&mut e));
        old_peer.in_tx.send(Ok(reenroll())).await.unwrap();
        // B1: the marked refusal is terminal; the session ends with it
        // instead of retrying the replacement on the old stream.
        let r = tokio::time::timeout(WAIT, session)
            .await
            .expect("session ends on the marked refusal");
        assert!(r.is_err());
        assert_eq!(shared.lock().opens.len(), 1, "no retry of the replacement");
        assert_eq!(
            authz.lock().terminal(),
            Some(crate::authorization::TerminalSignal::Revoked)
        );
        assert_eq!(authz.lock().state(), ServingState::Revoked);
        old_peer.in_tx.send(Ok(ping(7))).await.ok();
        assert!(
            old_peer.out_rx.try_recv().is_err(),
            "the old stream is no longer served"
        );
    }

    /// T20/T23 (connector side): only the matching marker reaches each
    /// terminal signal; unmarked statuses of any code — including the old
    /// "connector is revoked" text — are never terminal.
    #[tokio::test]
    async fn unmarked_errors_never_terminal_and_unknown_is_distinct() {
        for (status, want) in [
            (marked_status(tonic::Code::PermissionDenied, None), None),
            (marked_status(tonic::Code::NotFound, None), None),
            (marked_status(tonic::Code::Unavailable, None), None),
            (marked_status(tonic::Code::FailedPrecondition, None), None),
            (tonic::Status::unavailable("transport reset"), None),
            (
                marked_status(tonic::Code::NotFound, Some("unknown")),
                Some(crate::authorization::TerminalSignal::UnknownIdentity),
            ),
            (
                marked_status(tonic::Code::PermissionDenied, Some("revoked")),
                Some(crate::authorization::TerminalSignal::Revoked),
            ),
        ] {
            let mut e = env();
            let authz = e.authz.clone();
            let (old, old_peer) = fake_stream(far_future());
            let (mut ops, _) = fake_ops_with(vec![], true, &old, authz.clone());
            let code = status.code();
            old_peer.in_tx.send(Err(status)).await.unwrap();
            let r = run_session(into_stream(old), &mut ops, io(&mut e)).await;
            assert!(r.is_err(), "{code:?}: a recv error still ends the session");
            assert_eq!(authz.lock().terminal(), want, "{code:?}");
        }
        // Unmarked refusal at open (production open_refused) is not terminal.
        let t = AckTracker::shared(AUTHORIZATION_FRESHNESS);
        let _ = open_refused(marked_status(tonic::Code::Unavailable, None), &t);
        assert_eq!(t.lock().terminal(), None);
        let _ = open_refused(marked_status(tonic::Code::NotFound, Some("unknown")), &t);
        assert_eq!(
            t.lock().terminal(),
            Some(crate::authorization::TerminalSignal::UnknownIdentity)
        );
    }

    // ── Phase B1: reconnect loop, terminal idle (D12) ──────────────────────

    const FAST: ReconnectTiming = ReconnectTiming {
        clean: Duration::from_millis(1),
        backoff_initial: Duration::from_millis(1),
        backoff_max: Duration::from_millis(4),
        terminal_log: Duration::from_secs(3600),
    };

    enum Outcome {
        Ok,
        /// Transport-style failure (no status).
        Err,
        /// Control() refused with this code and optional marker (production
        /// `open_refused`).
        Refused(tonic::Code, Option<&'static str>),
        /// An authorization round trip succeeds, then the stream fails.
        ServeThenErr,
        /// The attempt never completes.
        Hang,
    }

    struct ScriptedAttempt {
        authz: SharedAckTracker,
        script: std::collections::VecDeque<Outcome>,
        attempts: Arc<std::sync::atomic::AtomicUsize>,
    }

    impl Attempt for ScriptedAttempt {
        async fn attempt(&mut self) -> Result<()> {
            self.attempts
                .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            match self.script.pop_front().unwrap_or(Outcome::Err) {
                Outcome::Ok => Ok(()),
                Outcome::Err => Err(anyhow::anyhow!("simulated transport failure")),
                Outcome::Refused(code, marker) => {
                    Err(open_refused(marked_status(code, marker), &self.authz))
                }
                Outcome::ServeThenErr => {
                    let mut t = self.authz.lock();
                    let s = t.next_seq();
                    let now = Instant::now();
                    t.record_sent(s, now);
                    t.stream_up(now);
                    t.on_ack(s, true, now).expect("ack accepted");
                    drop(t);
                    Err(anyhow::anyhow!("stream lost after authorization"))
                }
                Outcome::Hang => std::future::pending().await,
            }
        }
    }

    fn scripted(
        authz: &SharedAckTracker,
        script: Vec<Outcome>,
    ) -> (ScriptedAttempt, Arc<std::sync::atomic::AtomicUsize>) {
        let attempts = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        (
            ScriptedAttempt {
                authz: authz.clone(),
                script: script.into(),
                attempts: attempts.clone(),
            },
            attempts,
        )
    }

    fn attempts_of(a: &Arc<std::sync::atomic::AtomicUsize>) -> usize {
        a.load(std::sync::atomic::Ordering::SeqCst)
    }

    /// AB1-10 (D12), AB1-9 (T7/T25), AB1-8 (T24): a terminal marker stops the
    /// reconnect loop for good — no further attempts (no opens, health
    /// reports or renewals) — and the loop never returns, so the process
    /// stays up. A fresh tracker (a restarted process) starts closed and
    /// returns to the same terminal state without ever opening.
    #[tokio::test]
    async fn terminal_marker_stops_reconnecting_and_the_loop_never_returns() {
        for (marker, code, state) in [
            ("revoked", tonic::Code::PermissionDenied, ServingState::Revoked),
            ("unknown", tonic::Code::NotFound, ServingState::UnknownIdentity),
        ] {
            let authz = AckTracker::shared(AUTHORIZATION_FRESHNESS);
            let gate = authz.lock().gate();
            assert_eq!(gate.snapshot().state(), ServingState::Starting);
            let (mut attempt, attempts) = scripted(
                &authz,
                vec![
                    Outcome::Err,
                    Outcome::Ok,
                    Outcome::Refused(code, Some(marker)),
                    // Would be reached only if the loop kept reconnecting.
                    Outcome::Ok,
                    Outcome::Ok,
                ],
            );
            let loop_authz = authz.clone();
            let task = tokio::spawn(async move {
                reconnect_loop(&mut attempt, &loop_authz, FAST).await
            });
            wait_for("terminal marker attempt", || attempts_of(&attempts) == 3).await;
            // Many backoff periods (≤ 4 ms each) later: still parked.
            tokio::time::sleep(Duration::from_millis(200)).await;
            assert_eq!(attempts_of(&attempts), 3, "{marker}: no reconnect after the terminal marker");
            assert!(!task.is_finished(), "{marker}: the loop must never return (D12)");
            assert_eq!(authz.lock().state(), state);
            assert_eq!(authz.lock().highest_accepted(), 0, "never opened in between");
            assert!(!gate.admits_now());
            task.abort();
        }
    }

    /// AB1-11 (T8/T21/T23): unmarked refusals of any code and transport
    /// failures are never terminal; the loop keeps reconnecting.
    #[tokio::test]
    async fn unmarked_failures_keep_reconnecting_and_are_never_terminal() {
        let authz = AckTracker::shared(AUTHORIZATION_FRESHNESS);
        let (mut attempt, attempts) = scripted(
            &authz,
            vec![
                Outcome::Refused(tonic::Code::NotFound, None),
                Outcome::Refused(tonic::Code::PermissionDenied, None),
                Outcome::Refused(tonic::Code::Unavailable, None),
                Outcome::Refused(tonic::Code::Unauthenticated, None),
                Outcome::Refused(tonic::Code::PermissionDenied, Some("REVOKED")),
                Outcome::Err,
                Outcome::Ok,
            ],
        );
        let loop_authz = authz.clone();
        let task = tokio::spawn(async move { reconnect_loop(&mut attempt, &loop_authz, FAST).await });
        wait_for("reconnects continue", || attempts_of(&attempts) >= 10).await;
        assert_eq!(authz.lock().terminal(), None);
        assert_eq!(authz.lock().state(), ServingState::Starting);
        task.abort();
    }

    /// Every end of an attempt is a stream end: Serving → ServingDisconnected.
    #[tokio::test]
    async fn an_ended_attempt_is_a_stream_end() {
        let authz = AckTracker::shared(AUTHORIZATION_FRESHNESS);
        let (mut attempt, attempts) = scripted(&authz, vec![Outcome::ServeThenErr, Outcome::Hang]);
        let loop_authz = authz.clone();
        let task = tokio::spawn(async move { reconnect_loop(&mut attempt, &loop_authz, FAST).await });
        wait_for("second attempt", || attempts_of(&attempts) == 2).await;
        assert_eq!(authz.lock().state(), ServingState::ServingDisconnected);
        assert!(authz.lock().gate().admits_now(), "open until D");
        task.abort();
    }

    /// One real session per attempt over scripted fake streams.
    struct SessionAttempt<'a> {
        e: &'a mut Env,
        streams: std::collections::VecDeque<Parts>,
    }

    impl Attempt for SessionAttempt<'_> {
        async fn attempt(&mut self) -> Result<()> {
            let Some(parts) = self.streams.pop_front() else {
                return std::future::pending().await;
            };
            let (mut ops, _) = fake_ops_with(vec![], true, &parts, self.e.authz.clone());
            let seq = self.e.authz.lock().next_seq();
            let initial = self.e.health.report(&self.e.policy_cache, &self.e.slot, seq);
            let stream = establish_stream(
                parts.out_tx,
                parts.inbound,
                initial,
                parts.cert_not_after_unix,
                None,
            )
            .await?;
            run_session(stream, &mut ops, io(self.e)).await
        }
    }

    /// AB1-8 (T6): after the deadline passed during an interruption, a
    /// reconnect alone doesn't serve — not on stream open, not on the
    /// controller's first message — only a fresh accepted ack reopens.
    #[tokio::test]
    async fn reconnect_after_expiry_serves_only_after_a_fresh_ack() {
        let h = Duration::from_millis(400);
        let mut e = env();
        e.authz = AckTracker::shared(h);
        let authz = e.authz.clone();
        let gate = authz.lock().gate();
        let (s1, mut p1) = fake_stream_stamped(far_future(), &authz);
        let (s2, mut p2) = fake_stream_stamped(far_future(), &authz);
        let mut attempt = SessionAttempt {
            e: &mut e,
            streams: vec![s1, s2].into(),
        };
        let driver = async {
            let a = health_seq(&recv(&mut p1.out_rx).await);
            p1.in_tx.send(Ok(policy_msg(true))).await.unwrap();
            p1.in_tx.send(Ok(authz_ack(a))).await.unwrap();
            wait_for("serving", || gate.admits_now()).await;

            // Interruption: the controller ends stream 1.
            drop(p1);
            wait_for("disconnected", || {
                authz.lock().state() == ServingState::ServingDisconnected
            })
            .await;
            assert!(gate.admits_now(), "ServingDisconnected stays open until D");

            // Stream 2 opens (initial report sent) while D passes.
            let b = health_seq(&recv(&mut p2.out_rx).await);
            tokio::time::sleep(h).await;
            assert!(!gate.admits_now(), "closed at D");
            p2.in_tx.send(Ok(policy_msg(true))).await.unwrap();
            p2.in_tx.send(Ok(ping(1))).await.unwrap();
            expect_pong(&mut p2, 1).await;
            assert_eq!(authz.lock().state(), ServingState::Recovering);
            assert!(!gate.admits_now(), "a reconnect alone doesn't serve");
            // The stream-2 initial report was sent before the deadline passed
            // (before lost_at, and by now older than H): its ack can't reopen.
            p2.in_tx.send(Ok(authz_ack(b))).await.unwrap();
            p2.in_tx.send(Ok(ping(2))).await.unwrap();
            expect_pong(&mut p2, 2).await;
            assert!(!gate.admits_now(), "pre-loss request can't restore authorization");
            assert_eq!(authz.lock().state(), ServingState::Recovering);
            // A request sent after the loss, acknowledged: serving again.
            let c = authz.lock().next_seq();
            authz.lock().record_sent(c, Instant::now());
            p2.in_tx.send(Ok(authz_ack(c))).await.unwrap();
            wait_for("serving again", || gate.admits_now()).await;
            assert_eq!(authz.lock().state(), ServingState::Serving);
        };
        tokio::select! {
            never = reconnect_loop(&mut attempt, &authz, FAST) => match never {},
            _ = driver => {}
        }
    }
}

#[cfg(test)]
mod acl_diff_session_tests {
    //! F-3: a resource tuple edit (same `resource_id`, new port) pushed on the
    //! controller Control stream must cancel live connector sessions bound to
    //! the old tuple. Drives the production ACL arm (`handle_controller_msg`)
    //! and the production data path (`device_tunnel::handle_stream`, connector
    //! route, real `TcpStream::connect` to local listeners) over in-memory
    //! client streams.

    use super::*;
    use crate::agent_tunnel::AgentTunnelHub;
    use crate::client::v1::{AclEntry, AclSnapshot};
    use crate::crl::CrlManager;
    use crate::session_registry::SessionTransport;
    use ::time::{Duration as TimeDuration, OffsetDateTime};
    use rcgen::{
        BasicConstraints, CertificateParams, CertificateRevocationListParams, CertifiedIssuer,
        DistinguishedName, DnType, IsCa, KeyIdMethod, KeyPair, KeyUsagePurpose, SerialNumber,
        PKCS_ECDSA_P256_SHA256,
    };
    use tokio::io::{AsyncReadExt, AsyncWriteExt, DuplexStream};
    use tokio::net::TcpListener;
    use tokio::task::JoinHandle;
    use tonic::transport::Channel;

    const SPIFFE: &str = "spiffe://ws-test.zecurity.in/client/device-1";
    const WAIT: Duration = Duration::from_secs(5);

    /// A CrlManager holding a valid, empty, signed CRL: `check` → NotRevoked.
    fn valid_crl() -> CrlManager {
        const KEY_ID: &[u8] = b"f3-test-key-id";
        let mut params = CertificateParams::default();
        let mut dn = DistinguishedName::new();
        dn.push(DnType::CommonName, "f3-workspace-ca");
        params.distinguished_name = dn;
        params.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        params.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        params.key_identifier_method = KeyIdMethod::PreSpecified(KEY_ID.to_vec());
        let key = KeyPair::generate_for(&PKCS_ECDSA_P256_SHA256).unwrap();
        let issuer = CertifiedIssuer::self_signed(params, key).unwrap();
        let now = OffsetDateTime::now_utc();
        let der = CertificateRevocationListParams {
            this_update: now - TimeDuration::minutes(1),
            next_update: now + TimeDuration::hours(1),
            crl_number: SerialNumber::from(1u64),
            issuing_distribution_point: None,
            revoked_certs: vec![],
            key_identifier_method: KeyIdMethod::PreSpecified(KEY_ID.to_vec()),
        }
        .signed_by(&issuer)
        .unwrap()
        .der()
        .to_vec();
        let crl = CrlManager::new();
        crl.install_verified_der(&der, issuer.pem().as_bytes())
            .unwrap();
        crl
    }

    fn entry(resource_id: &str, port: u16) -> AclEntry {
        AclEntry {
            resource_id: resource_id.into(),
            name: resource_id.into(),
            address: "127.0.0.1".into(),
            port: port as u32,
            protocol: "tcp".into(),
            allowed_spiffe_ids: vec![SPIFFE.into()],
            route_type: "connector".into(),
            ..Default::default()
        }
    }

    fn acl(version: u64, entries: Vec<AclEntry>) -> ConnectorControlMessage {
        ConnectorControlMessage {
            body: Some(CBody::AclSnapshot(AclSnapshot {
                version,
                workspace_id: "ws-test".into(),
                entries,
                ..Default::default()
            })),
        }
    }

    /// Local echo server standing in for the internal application.
    async fn echo_server() -> u16 {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let port = listener.local_addr().unwrap().port();
        tokio::spawn(async move {
            loop {
                let Ok((mut sock, _)) = listener.accept().await else {
                    return;
                };
                tokio::spawn(async move {
                    let mut buf = [0u8; 1024];
                    loop {
                        match sock.read(&mut buf).await {
                            Ok(0) | Err(_) => return,
                            Ok(n) => {
                                if sock.write_all(&buf[..n]).await.is_err() {
                                    return;
                                }
                            }
                        }
                    }
                });
            }
        });
        port
    }

    struct Env {
        policy: Arc<PolicyCache>,
        registry: Arc<SessionRegistry>,
        hub: AgentTunnelHub,
        crl: CrlManager,
        log_tx: mpsc::Sender<crate::ControlMessage>,
        _log_rx: mpsc::Receiver<crate::ControlMessage>,
        shield_registry: ShieldRegistry,
        out_tx: mpsc::Sender<ConnectorControlMessage>,
        _out_rx: mpsc::Receiver<ConnectorControlMessage>,
        relay_list_tx: watch::Sender<Option<LabelledRelayList>>,
    }

    fn env() -> Env {
        let policy = Arc::new(PolicyCache::new());
        let (shield_ack_tx, _) = mpsc::channel(8);
        let (log_tx, log_rx) = mpsc::channel(1024);
        let (out_tx, out_rx) = mpsc::channel(64);
        Env {
            shield_registry: ShieldRegistry::new(
                Channel::from_static("http://127.0.0.1:1").connect_lazy(),
                crate::test_support::TRUST_DOMAIN.to_string(),
                crate::test_support::CONNECTOR_ID.to_string(),
                shield_ack_tx,
                policy.clone(),
                crate::authorization::AuthzGate::open_for_tests(),
            ),
            policy,
            registry: Arc::new(SessionRegistry::new()),
            hub: AgentTunnelHub::new(),
            crl: valid_crl(),
            log_tx,
            _log_rx: log_rx,
            out_tx,
            _out_rx: out_rx,
            relay_list_tx: watch::channel(None).0,
        }
    }

    /// Feeds one controller message through the production ACL arm.
    async fn push(e: &Env, msg: ConnectorControlMessage) {
        let action = handle_controller_msg(
            msg,
            &e.shield_registry,
            &e.out_tx,
            &e.policy,
            &e.registry,
            &e.relay_list_tx,
        )
        .await;
        assert!(matches!(action, MsgAction::Continue));
    }

    async fn write_frame(s: &mut DuplexStream, v: serde_json::Value) {
        let body = serde_json::to_vec(&v).unwrap();
        s.write_all(&(body.len() as u32).to_be_bytes())
            .await
            .unwrap();
        s.write_all(&body).await.unwrap();
    }

    async fn read_frame(s: &mut DuplexStream) -> serde_json::Value {
        let mut len = [0u8; 4];
        s.read_exact(&mut len).await.unwrap();
        let mut body = vec![0u8; u32::from_be_bytes(len) as usize];
        s.read_exact(&mut body).await.unwrap();
        serde_json::from_slice(&body).unwrap()
    }

    /// One client flow: spawns the production `handle_stream`, sends the
    /// tunnel request and returns (client end, handler task, response).
    async fn open(
        e: &Env,
        port: u16,
    ) -> (
        DuplexStream,
        JoinHandle<anyhow::Result<()>>,
        serde_json::Value,
    ) {
        let (mut client, server) = tokio::io::duplex(64 * 1024);
        let (acl, reg, hub, crl, log_tx) = (
            e.policy.clone(),
            e.registry.clone(),
            e.hub.clone(),
            e.crl.clone(),
            e.log_tx.clone(),
        );
        let task = tokio::spawn(async move {
            crate::device_tunnel::handle_stream(
                server,
                SPIFFE.to_string(),
                vec![0x01, 0x02],
                acl,
                reg,
                SessionTransport::Quic,
                hub,
                crl,
                crate::test_support::CONNECTOR_ID,
                &log_tx,
                &crate::authorization::AuthzGate::open_for_tests(),
            )
            .await
        });
        write_frame(
            &mut client,
            serde_json::json!({"destination": "127.0.0.1", "port": port, "protocol": "tcp"}),
        )
        .await;
        let resp = tokio::time::timeout(WAIT, read_frame(&mut client))
            .await
            .expect("tunnel response");
        (client, task, resp)
    }

    /// Spawns the production `handle_stream` with `gate`; the request is
    /// written after `delay`. Returns everything the client received before
    /// EOF and the handler result.
    async fn gated(
        e: &Env,
        port: u16,
        gate: crate::authorization::AuthzGate,
        delay: Duration,
        crl: Option<CrlManager>,
    ) -> (Vec<u8>, anyhow::Result<()>) {
        let (mut client, server) = tokio::io::duplex(64 * 1024);
        let (acl, reg, hub, crl, log_tx) = (
            e.policy.clone(),
            e.registry.clone(),
            e.hub.clone(),
            crl.unwrap_or_else(|| e.crl.clone()),
            e.log_tx.clone(),
        );
        let task = tokio::spawn(async move {
            crate::device_tunnel::handle_stream(
                server,
                SPIFFE.to_string(),
                vec![0x01, 0x02],
                acl,
                reg,
                SessionTransport::Quic,
                hub,
                crl,
                crate::test_support::CONNECTOR_ID,
                &log_tx,
                &gate,
            )
            .await
        });
        tokio::time::sleep(delay).await;
        let body = serde_json::to_vec(
            &serde_json::json!({"destination": "127.0.0.1", "port": port, "protocol": "tcp"}),
        )
        .unwrap();
        // The handler may already have dropped its end.
        let _ = client.write_all(&(body.len() as u32).to_be_bytes()).await;
        let _ = client.write_all(&body).await;
        let mut got = Vec::new();
        tokio::time::timeout(WAIT, client.read_to_end(&mut got))
            .await
            .expect("handler ends the stream")
            .expect("read");
        let result = tokio::time::timeout(WAIT, task).await.unwrap().unwrap();
        (got, result)
    }

    /// AB1-1 (§7 step 2): the funnel refuses every stream while the connector
    /// isn't serving — Starting, past D, terminal — and writes nothing (no
    /// TunnelResponse, never ok:false); an allowed request is still served
    /// when it is.
    #[tokio::test]
    async fn funnel_refuses_without_a_tunnel_response_when_not_serving() {
        use crate::authorization::{AuthzGate, ServingState};
        crate::test_support::install_crypto_provider();
        let port = echo_server().await;
        let e = env();
        push(&e, acl(1, vec![entry("res-a", port)])).await;
        for (what, gate) in [
            ("starting", AuthzGate::closed_for_tests()),
            (
                "past deadline",
                AuthzGate::fixed(ServingState::Serving, Some(Instant::now())),
            ),
            ("disabled", AuthzGate::fixed(ServingState::Disabled, None)),
            ("revoked", AuthzGate::fixed(ServingState::Revoked, None)),
            (
                "unknown",
                AuthzGate::fixed(ServingState::UnknownIdentity, None),
            ),
        ] {
            let (got, result) = gated(&e, port, gate.clone(), Duration::ZERO, None).await;
            assert!(got.is_empty(), "{what}: nothing may be written, got {got:?}");
            assert!(result.is_err(), "{what}: refused");
            // Requests that would otherwise get an ok:false TunnelResponse
            // (and so stop the client's fallback): still nothing written.
            let (got, _) = gated(&e, port + 1, gate.clone(), Duration::ZERO, None).await;
            assert!(got.is_empty(), "{what}, no ACL match: nothing written, got {got:?}");
            let (got, _) = gated(&e, port, gate, Duration::ZERO, Some(CrlManager::new())).await;
            assert!(got.is_empty(), "{what}, CRL unavailable: nothing written, got {got:?}");
        }
        // Serving (`open` uses an open gate): admitted and answered.
        let (_client, _task, resp) = open(&e, port).await;
        assert_eq!(resp["ok"], true, "serving: admitted: {resp}");
    }

    /// AB1-1 (§7 step 2): a stream that passed the entry check is re-checked
    /// after session registration; if D passed in between it is refused there,
    /// still without a TunnelResponse.
    #[tokio::test]
    async fn funnel_rechecks_the_serving_state_after_registration() {
        use crate::authorization::{AuthzGate, ServingState};
        crate::test_support::install_crypto_provider();
        let port = echo_server().await;
        let e = env();
        push(&e, acl(1, vec![entry("res-a", port)])).await;
        let gate = AuthzGate::fixed(
            ServingState::Serving,
            Some(Instant::now() + Duration::from_millis(150)),
        );
        let (got, result) = gated(&e, port, gate, Duration::from_millis(300), None).await;
        assert!(got.is_empty(), "nothing may be written, got {got:?}");
        let err = result.expect_err("refused after registration");
        assert!(
            err.to_string().contains("after registration"),
            "{err}"
        );
    }

    async fn echo_ok(client: &mut DuplexStream, msg: &[u8]) {
        client.write_all(msg).await.unwrap();
        let mut buf = vec![0u8; msg.len()];
        tokio::time::timeout(WAIT, client.read_exact(&mut buf))
            .await
            .expect("echo within WAIT")
            .expect("echo read");
        assert_eq!(buf, msg);
    }

    #[tokio::test]
    async fn port_edit_cancels_old_tuple_session_keeps_unrelated() {
        crate::test_support::install_crypto_provider();
        let old_port = echo_server().await;
        let new_port = echo_server().await;
        let other_port = echo_server().await;
        let e = env();

        push(
            &e,
            acl(
                1,
                vec![entry("res-a", old_port), entry("res-b", other_port)],
            ),
        )
        .await;

        // 1. A client flow opens a session to the allowed destination.
        let (mut old_client, old_task, resp) = open(&e, old_port).await;
        assert_eq!(resp["ok"], true, "old port admitted: {resp}");
        echo_ok(&mut old_client, b"old-before").await;

        // An unrelated resource's session.
        let (mut other_client, other_task, resp) = open(&e, other_port).await;
        assert_eq!(resp["ok"], true, "unrelated admitted: {resp}");
        echo_ok(&mut other_client, b"other-before").await;

        // 2. Same resource identity, new port.
        push(
            &e,
            acl(
                2,
                vec![entry("res-a", new_port), entry("res-b", other_port)],
            ),
        )
        .await;

        // 3. The old session is cancelled by the connector: the handler
        //    returns and the client side sees EOF.
        let joined = tokio::time::timeout(WAIT, old_task)
            .await
            .expect("old-tuple session must be cancelled by the ACL diff");
        assert!(joined.unwrap().is_ok(), "cancel is a clean session end");
        let mut buf = [0u8; 16];
        let n = tokio::time::timeout(WAIT, old_client.read(&mut buf))
            .await
            .expect("EOF within WAIT")
            .unwrap_or(0);
        assert_eq!(n, 0, "old client sees EOF after cancel");

        // 4. A new connection to the old port is rejected.
        let (_c, t, resp) = open(&e, old_port).await;
        assert_eq!(resp["ok"], false, "old port denied: {resp}");
        assert_eq!(resp["error"], "access denied");
        assert!(t.await.unwrap().is_err());

        // 5. A new connection to the new port succeeds.
        let (mut new_client, _new_task, resp) = open(&e, new_port).await;
        assert_eq!(resp["ok"], true, "new port admitted: {resp}");
        echo_ok(&mut new_client, b"new-after").await;

        // 6. The unrelated resource's session is still alive.
        echo_ok(&mut other_client, b"other-after").await;
        assert!(!other_task.is_finished());
    }

    #[tokio::test]
    async fn name_only_edit_keeps_session() {
        crate::test_support::install_crypto_provider();
        let port = echo_server().await;
        let e = env();
        push(&e, acl(1, vec![entry("res-a", port)])).await;

        let (mut client, task, resp) = open(&e, port).await;
        assert_eq!(resp["ok"], true);
        echo_ok(&mut client, b"before").await;

        let renamed = AclEntry {
            name: "renamed".into(),
            ..entry("res-a", port)
        };
        push(&e, acl(2, vec![renamed])).await;

        echo_ok(&mut client, b"after").await;
        assert!(!task.is_finished(), "name-only edit must not cancel");
    }
}
