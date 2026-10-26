use parking_lot::{Mutex, RwLock};
use crate::authorization::AuthzGate;
use std::collections::{HashMap, HashSet};
use std::future::Future;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicBool, AtomicU64, Ordering};
use std::sync::Arc;
use std::time::{Duration, SystemTime, UNIX_EPOCH};

use ::time::OffsetDateTime;
use anyhow::{Context, Result};
use tokio::net::{TcpListener, TcpStream};
use tokio::sync::mpsc;
use tokio::task::JoinHandle;
use tokio_rustls::server::TlsStream;
use tokio_rustls::TlsAcceptor;
use tokio_stream::wrappers::ReceiverStream;
use tokio_stream::StreamExt;
use tokio_util::sync::CancellationToken;
use tonic::transport::{Channel, Server};
use tonic::{Request, Response, Status, Streaming};
use tracing::{debug, info, warn};
use x509_parser::prelude::*;

use crate::shield_proto::shield_service_client::ShieldServiceClient;
use crate::shield_proto::shield_service_server::{ShieldService, ShieldServiceServer};
use crate::shield_proto::{
    EnrollRequest, EnrollResponse, GoodbyeRequest, GoodbyeResponse, ReEnrollSignal,
    RenewCertRequest, RenewCertResponse, ResourceAck, ResourceInstruction, ResourceSnapshot,
    ResourceStateReport, ShieldControlMessage,
};
use crate::tls::cert_holder::{CertHolder, CertMaterial};
use crate::tls::server_cfg::build_shield_server_tls;

/// Shield renewal window until the controller sends AgentPolicy (K2).
const DEFAULT_RENEWAL_WINDOW_SECS: u64 = 48 * 60 * 60;
/// At most one ReEnroll per Shield Control stream per this interval (K2):
/// a Shield inside its window is reminded every 10 min, not on every 15 s
/// health report. Same interval as the controller's connector throttle.
const SHIELD_REENROLL_RESEND_INTERVAL: Duration = Duration::from_secs(10 * 60);
const SHIELD_STALE_THRESHOLD_SECS: i64 = 90;

/// A Shield must finish its TLS handshake on :9091 within this long; a
/// stalled client is dropped without affecting other connections.
const SHIELD_TLS_HANDSHAKE_TIMEOUT: Duration = Duration::from_secs(10);

/// Handshaken connections waiting for tonic to pick them up. Only
/// per-connection handshake tasks wait on it, never the accept loop.
const SHIELD_ACCEPTED_QUEUE_CAP: usize = 64;

/// Capacity of a connected shield's instruction forwarding channel. Sized with
/// generous headroom so push_instructions' non-blocking try_send effectively never
/// hits "Full" at realistic (one-per-mutation) instruction rates; a genuine Full is
/// treated as overload and left to the snapshot/reconciler to repair.
const SHIELD_INSTRUCTION_QUEUE_CAP: usize = 256;

/// All per-shield mutable state behind a single lock.
#[derive(Debug)]
struct ShieldMaps {
    /// Live per-shield instruction channels. INVARIANT: each channel has exactly
    /// ONE producer — push_instructions, whose sole caller (handle_controller_msg)
    /// is a single sequential loop. That single-producer property, NOT try_send, is
    /// what preserves cross-push instruction order (F18): mpsc accepts concurrent
    /// producers and interleaves them, so any future parallel/sharded controller
    /// processing MUST partition by shield_id to keep this — otherwise ordering
    /// breaks silently. Deltas need a serialization point; the snapshot (versioned,
    /// idempotent) is the order-independent durable authority.
    instruction_txs: HashMap<String, mpsc::Sender<ResourceInstruction>>,
    resource_instructions: HashMap<String, Vec<ResourceInstruction>>,
    /// Live channel to forward snapshots to a connected shield.
    snapshot_txs: HashMap<String, mpsc::Sender<ResourceSnapshot>>,
    /// Latest desired-state snapshot per shield — replayed on shield (re)connect
    /// (ADR-004 Phase 2: re-protects a rebooted shield).
    resource_snapshots: HashMap<String, ResourceSnapshot>,
    /// Latest actual-state report per shield — flushed upstream on the health
    /// tick (ADR-004 Phase 3). Latest-wins: it's a checkpoint, not a queue.
    pending_state: HashMap<String, ResourceStateReport>,
    health: HashMap<String, ShieldEntry>,
    pending_discovery: HashMap<String, crate::shield_proto::DiscoveryReport>,
    /// Certificate audit bug 3: revoked shield identities, from the
    /// controller's authoritative list. Terminal: the set only grows.
    revoked: HashSet<String>,
    /// Cancellation of each open Control stream, per shield: (stream id,
    /// token). Revocation cancels them all.
    stream_cancels: HashMap<String, Vec<(u64, CancellationToken)>>,
}

#[derive(Debug, Clone)]
struct ShieldEntry {
    status: String,
    version: String,
    last_seen_unix: i64,
    lan_ip: String,
}

/// Shield Control stream refusal from a connector in a terminal state. Must
/// match the Shield's `CONNECTOR_TERMINAL_MESSAGE`.
pub(crate) const CONNECTOR_TERMINAL_MESSAGE: &str =
    "connector is revoked or unknown to the controller";

/// Shared state for Shield-facing Control streams.
#[derive(Debug, Clone)]
pub struct ShieldRegistry {
    /// All per-shield mutable hansmaps behind a single lock.
    maps: Arc<Mutex<ShieldMaps>>,
    // Unified ack sink - consumed by control_stream.rs which forwards to controller
    pub ack_tx: mpsc::Sender<(String, ResourceAck)>,
    /// Channel used to proxy Shield RenewCert to the controller. Rebuilt with
    /// the renewed identity whenever the CertHolder publishes (Sprint 20 G-2a).
    controller_channel: Arc<RwLock<Channel>>,
    controller_channel_swaps: Arc<AtomicU64>,
    trust_domain: String,
    connector_id: String,
    /// Shield renewal window; shared by every clone, updated by AgentPolicy.
    renewal_window_secs: Arc<AtomicU64>,
    /// Tunnel hub — routes RDE tunnel messages between device connections and Shields.
    pub tunnel_hub: crate::agent_tunnel::AgentTunnelHub,
    /// Shared ACL snapshot cache. Read on every incoming ShieldHealthReport
    /// so the connector can piggyback the current peer-Connector list back to
    /// the Shield (see PeerConnectorList in the Shield Control stream).
    policy_cache: Arc<crate::policy::PolicyCache>,
    /// Certificate audit bug 3: set once this process has applied the
    /// controller's authoritative revocation list. Until then no shield is
    /// accepted — not at TCP accept, not on any RPC.
    revocations_synced: Arc<AtomicBool>,
    next_stream_id: Arc<AtomicU64>,
    /// Connector serving state (connector-revocation B1): no new shield
    /// Control stream while the connector itself isn't authorized to serve.
    serving_gate: AuthzGate,
    /// Connector-revocation fail-closed (§7 step 4, B2): bumped to close every
    /// open shield Control stream with `Unavailable` (the connector stopped
    /// serving), as distinct from a shield revocation (`PermissionDenied`).
    close_all: Arc<tokio::sync::watch::Sender<u64>>,
}

impl ShieldRegistry {
    pub fn new(
        controller_channel: Channel,
        trust_domain: String,
        connector_id: String,
        ack_tx: mpsc::Sender<(String, ResourceAck)>,
        policy_cache: Arc<crate::policy::PolicyCache>,
        serving_gate: AuthzGate,
    ) -> Self {
        Self {
            maps: Arc::new(Mutex::new(ShieldMaps {
                instruction_txs: HashMap::new(),
                resource_instructions: HashMap::new(),
                snapshot_txs: HashMap::new(),
                resource_snapshots: HashMap::new(),
                pending_state: HashMap::new(),
                health: HashMap::new(),
                pending_discovery: HashMap::new(),
                revoked: HashSet::new(),
                stream_cancels: HashMap::new(),
            })),
            ack_tx,
            controller_channel: Arc::new(RwLock::new(controller_channel)),
            controller_channel_swaps: Arc::new(AtomicU64::new(0)),
            trust_domain,
            connector_id,
            renewal_window_secs: Arc::new(AtomicU64::new(DEFAULT_RENEWAL_WINDOW_SECS)),
            tunnel_hub: crate::agent_tunnel::AgentTunnelHub::new(),
            policy_cache,
            revocations_synced: Arc::new(AtomicBool::new(false)),
            next_stream_id: Arc::new(AtomicU64::new(0)),
            serving_gate,
            close_all: Arc::new(tokio::sync::watch::Sender::new(0)),
        }
    }

    /// Refusal for a shield while this connector is not serving. A terminal
    /// state (revoked, unknown identity) gets its own message: this connector
    /// will never serve again, so the Shield fails over at once instead of
    /// waiting for it (connector-revocation Phase F, finding B).
    fn not_serving_status(&self) -> Status {
        if self.serving_gate.snapshot().state().is_terminal() {
            Status::unavailable(CONNECTOR_TERMINAL_MESSAGE)
        } else {
            Status::unavailable("connector is not authorized to serve")
        }
    }

    /// Close every open shield Control stream because this connector stopped
    /// serving (terminal state, or `Disabled` past the D15 grace). Shields see
    /// `Unavailable` and fail over; new streams are refused by the serving
    /// gate. Revocation state, caches and buffered instructions are kept.
    pub fn close_all_streams(&self, reason: &'static str) {
        self.close_all.send_modify(|n| *n += 1);
        warn!(reason, "fail-closed: closing every shield Control stream");
    }

    /// How many close-all requests were made (tests).
    #[cfg(test)]
    pub fn close_all_count(&self) -> u64 {
        *self.close_all.borrow()
    }

    /// Apply the controller's authoritative, complete list of revoked shield
    /// identities (certificate audit bug 3). Idempotent; revocation is
    /// terminal, so ids are only ever added. Every newly revoked shield has its
    /// Control streams closed, its channels unregistered, its cached snapshot,
    /// buffered instructions and reports discarded, and every relay session
    /// through it ended. The first call in a process opens the connector to
    /// shields.
    pub fn apply_revocations(&self, revoked_shield_ids: Vec<String>) {
        let mut newly = Vec::new();
        let mut cancels = Vec::new();
        {
            let mut maps = self.maps.lock();
            for id in revoked_shield_ids {
                if maps.revoked.insert(id.clone()) {
                    newly.push(id);
                }
            }
            for id in &newly {
                maps.instruction_txs.remove(id);
                maps.snapshot_txs.remove(id);
                maps.resource_snapshots.remove(id);
                maps.resource_instructions.remove(id);
                maps.pending_state.remove(id);
                maps.pending_discovery.remove(id);
                maps.health.remove(id);
                cancels.extend(maps.stream_cancels.remove(id).unwrap_or_default());
            }
            self.revocations_synced.store(true, Ordering::SeqCst);
        }
        for (_, token) in cancels {
            token.cancel();
        }
        for id in &newly {
            self.tunnel_hub.revoke_shield(id);
            warn!(shield_id = %id, "shield revoked: stream closed, sessions ended, identity refused");
        }
    }

    /// A new controller stream became current (certificate audit bug 3): the
    /// list held so far was received on an earlier stream, so no new shield is
    /// admitted until the current stream delivers its own list. Revocations
    /// already applied stay applied, and established shield streams and
    /// sessions are not touched.
    pub fn require_revocation_resync(&self) {
        if self.revocations_synced.swap(false, Ordering::SeqCst) {
            info!(
                "new controller stream: admitting no new shields until its revocation list arrives"
            );
        }
    }

    /// Whether this process currently holds an authoritative revocation
    /// list (one received on the current controller stream).
    pub fn revocations_synced(&self) -> bool {
        self.revocations_synced.load(Ordering::SeqCst)
    }

    /// Whether `shield_id` is revoked.
    pub fn is_revoked(&self, shield_id: &str) -> bool {
        self.maps.lock().revoked.contains(shield_id)
    }

    /// Certificate audit bug 3: a shield is admitted only after this process
    /// applied the authoritative revocation list, and only if not revoked.
    fn admit(&self, shield_id: &str) -> Result<(), Status> {
        if !self.revocations_synced.load(Ordering::SeqCst) {
            return Err(Status::unavailable(
                "connector has not yet applied the controller's shield revocation list",
            ));
        }
        if self.maps.lock().revoked.contains(shield_id) {
            return Err(Status::permission_denied("shield is revoked"));
        }
        Ok(())
    }

    /// The current controller channel (cheap clone).
    pub fn controller_channel(&self) -> Channel {
        self.controller_channel.read().clone()
    }

    /// Replace the controller channel (after a certificate renewal).
    pub fn replace_controller_channel(&self, channel: Channel) {
        *self.controller_channel.write() = channel;
        self.controller_channel_swaps.fetch_add(1, Ordering::SeqCst);
    }

    /// How many times the controller channel has been replaced.
    pub fn controller_channel_swaps(&self) -> u64 {
        self.controller_channel_swaps.load(Ordering::SeqCst)
    }

    /// Rebuild the Shield-proxy controller channel with the renewed identity
    /// as soon as the CertHolder publishes. A failed rebuild keeps the old
    /// channel and retries (switching to a newer certificate if one lands).
    pub fn spawn_controller_channel_refresh<F, Fut>(
        &self,
        certs: Arc<CertHolder>,
        connect: F,
    ) -> JoinHandle<()>
    where
        F: Fn(Arc<CertMaterial>) -> Fut + Send + Sync + 'static,
        Fut: Future<Output = Result<Channel>> + Send,
    {
        let registry = self.clone();
        let mut rx = certs.subscribe();
        tokio::spawn(async move {
            while rx.changed().await.is_ok() {
                loop {
                    let material = rx.borrow_and_update().clone();
                    match connect(material.clone()).await {
                        Ok(channel) => {
                            registry.replace_controller_channel(channel);
                            info!(serial = %material.serial_hex, "Shield-proxy controller channel rebuilt with renewed certificate");
                            break;
                        }
                        Err(err) => {
                            warn!(error = %err, serial = %material.serial_hex, "rebuild Shield-proxy controller channel failed; retrying");
                            tokio::select! {
                                _ = tokio::time::sleep(Duration::from_secs(5)) => {}
                                changed = rx.changed() => {
                                    if changed.is_err() {
                                        return;
                                    }
                                }
                            }
                        }
                    }
                }
            }
        })
    }

    /// Build the current peer-Connector list for this Connector's Remote
    /// Network, sorted deterministically by `connector_id`, plus the ACL's
    /// revoked connector ids (Phase E, D21). Revoked ids are never offered
    /// as peers. Returns `None` only when there are neither peers nor
    /// revoked ids; a list with no peers is still sent so the Shield can
    /// prune revoked connectors (it ignores the empty `peers`).
    ///
    /// Address derivation: `connector_tunnel_addr` in the ACL snapshot is
    /// `host:9092` (QUIC data plane). The Shield needs `host:9091` (gRPC).
    /// The port is swapped by stripping and re-attaching.
    pub(crate) fn build_peer_connector_list(
        &self,
    ) -> Option<crate::shield_proto::PeerConnectorList> {
        let revoked_connector_ids = self.policy_cache.revoked_connector_ids();
        let mut peers: Vec<crate::shield_proto::PeerConnector> = self
            .policy_cache
            .peers_of_connector(&self.connector_id)
            .into_iter()
            .filter(|(id, _)| !revoked_connector_ids.contains(id))
            .map(|(id, tunnel_addr)| crate::shield_proto::PeerConnector {
                connector_id: id,
                connector_addr: derive_grpc_addr(&tunnel_addr),
            })
            .collect();
        if peers.is_empty() && revoked_connector_ids.is_empty() {
            return None;
        }
        peers.sort_by(|a, b| a.connector_id.cmp(&b.connector_id));
        Some(crate::shield_proto::PeerConnectorList {
            peers,
            revoked_connector_ids,
        })
    }

    /// Deliver instructions to a shield via Control stream, or buffer until the shield reconnects.
    pub fn push_instructions(&self, shield_id: &str, instructions: Vec<ResourceInstruction>) {
        if instructions.is_empty() {
            return;
        }
        // Decide-and-buffer atomically under one lock. If the shield is offline we
        // MUST insert the buffer in the same critical section that observed the
        // missing tx: otherwise a shield connecting in the gap inserts its tx and
        // drains the (still empty) buffer before our write lands, stranding the
        // instruction until the next reconnect. The connect handler inserts the tx
        // BEFORE it drains, so any buffer present at drain time is delivered — which
        // holds only if the check and the buffer write are one atomic step here.
        // Mirrors push_snapshot's already-correct discipline.
        let tx = {
            let mut maps = self.maps.lock();
            if maps.revoked.contains(shield_id) {
                return; // certificate audit bug 3: never buffered for a revoked shield
            }
            match maps.instruction_txs.get(shield_id).cloned() {
                Some(tx) => tx,
                None => {
                    // Append, don't overwrite: a second offline push must not clobber
                    // a batch already waiting for this shield.
                    maps.resource_instructions
                        .entry(shield_id.to_string())
                        .or_default()
                        .extend(instructions);
                    return;
                }
            }
        };

        // Forward in arrival order WITHOUT spawning. push_instructions is the sole
        // producer for this channel (see the instruction_txs invariant), so one
        // producer + this FIFO channel + the single drainer in control() preserves
        // cross-push order (F18). try_send keeps it non-blocking: awaiting a full
        // channel here would stall the dispatcher and head-of-line-block every other
        // shield + acks on the shared controller stream.
        //
        // F19 — DELIBERATE best-effort delivery, not an oversight. On send failure we
        // DROP the rest of the batch rather than re-buffering it. Recovery is the
        // snapshot: the cached desired set is replayed on reconnect (covers a dropped
        // apply) and the Phase 3 reaper confirms removals (covers a dropped remove), so
        // a dropped delta is at most a self-healing latency blip — never permanent wrong
        // state. Instruction delivery is the fast path; the versioned snapshot is the
        // durable authority. Re-buffering here would reopen the F16 buffer race for
        // latency the snapshot already covers — revisit only if reconnect reap/apply
        // latency is ever shown to matter operationally.
        for instr in instructions {
            match tx.try_send(instr) {
                Ok(()) => {}
                Err(mpsc::error::TrySendError::Closed(_)) => {
                    warn!(shield_id = %shield_id, "shield instruction channel closed during push (dropped; snapshot recovers on reconnect)");
                    break;
                }
                Err(mpsc::error::TrySendError::Full(_)) => {
                    warn!(shield_id = %shield_id, "shield instruction channel full (dropped; snapshot/reconciler will repair)");
                    break;
                }
            }
        }
    }

    /// Cache the latest desired-state snapshot and forward it live if the
    /// shield is connected. The cache is replayed when a shield (re)connects —
    /// this is what re-protects a rebooted shield (ADR-004 Phase 2).
    pub fn push_snapshot(&self, shield_id: &str, snapshot: ResourceSnapshot) {
        let maybe_tx = {
            let mut maps = self.maps.lock();
            if maps.revoked.contains(shield_id) {
                return; // certificate audit bug 3: never cached for a revoked shield
            }
            maps.resource_snapshots
                .insert(shield_id.to_string(), snapshot.clone());
            maps.snapshot_txs.get(shield_id).cloned()
        };
        if let Some(tx) = maybe_tx {
            let id = shield_id.to_string();
            tokio::spawn(async move {
                if tx.send(snapshot).await.is_err() {
                    warn!(shield_id = %id, "shield snapshot channel closed during push");
                }
            });
        }
    }

    /// Return the shield_id whose lan_ip matches `host`, or None if no connected Shield owns it.
    pub fn shield_for_host(&self, host: &str) -> Option<String> {
        self.maps
            .lock()
            .health
            .iter()
            .find(|(_, entry)| entry.lan_ip == host)
            .map(|(id, _)| id.clone())
    }

    /// Snapshot of alive shields for the health report sent to controller.
    pub fn get_shield_status_batch(&self) -> crate::proto::ShieldStatusBatch {
        let cutoff = unix_now() - SHIELD_STALE_THRESHOLD_SECS;
        let shields = self
            .maps
            .lock()
            .health
            .iter()
            .filter(|(_, e)| e.last_seen_unix >= cutoff)
            .map(|(id, e)| crate::proto::ShieldStatusUpdate {
                shield_id: id.clone(),
                status: e.status.clone(),
                version: e.version.clone(),
                lan_ip: e.lan_ip.clone(),
                last_seen_unix: e.last_seen_unix,
            })
            .collect();
        crate::proto::ShieldStatusBatch { shields }
    }

    pub fn connector_id(&self) -> &str {
        &self.connector_id
    }

    /// Drain all pending discovery reports into a ShieldDiscoveryBatch message.
    /// Returns None if there are no pending reports.
    pub fn drain_discovery_batch(&self) -> Option<crate::proto::ConnectorControlMessage> {
        let mut maps = self.maps.lock();
        if maps.pending_discovery.is_empty() {
            return None;
        }
        let reports: Vec<crate::proto::ShieldDiscoveryReport> = maps
            .pending_discovery
            .drain()
            .map(|(shield_id, report)| crate::proto::ShieldDiscoveryReport {
                shield_id,
                report: Some(report),
            })
            .collect();
        Some(crate::proto::ConnectorControlMessage {
            body: Some(
                crate::proto::connector_control_message::Body::ShieldDiscovery(
                    crate::proto::ShieldDiscoveryBatch { reports },
                ),
            ),
        })
    }

    /// Drain buffered shield actual-state reports into a ResourceStateBatch
    /// message (ADR-004 Phase 3). Returns None if there are no pending reports.
    pub fn drain_state_batch(&self) -> Option<crate::proto::ConnectorControlMessage> {
        let mut maps = self.maps.lock();
        if maps.pending_state.is_empty() {
            return None;
        }
        let reports: Vec<ResourceStateReport> = maps
            .pending_state
            .drain()
            .map(|(_, report)| report)
            .collect();
        Some(crate::proto::ConnectorControlMessage {
            body: Some(
                crate::proto::connector_control_message::Body::ResourceState(
                    crate::proto::ResourceStateBatch { reports },
                ),
            ),
        })
    }

    /// Serve the Shield-facing gRPC API on `addr` (Sprint 20 G-2b).
    ///
    /// TLS is terminated here instead of by tonic's `ServerTlsConfig`, which
    /// reads the certificate once and cannot switch it. The server certificate
    /// is resolved from `certs` on every handshake: after a renewal, new Shield
    /// connections see the renewed certificate, while established ones —
    /// long-lived Control streams that carry shield-routed tunnels — keep
    /// running. The server is never restarted.
    pub async fn serve(self, addr: SocketAddr, certs: Arc<CertHolder>) -> Result<()> {
        let listener = TcpListener::bind(addr)
            .await
            .with_context(|| format!("failed to bind Shield-facing server on {addr}"))?;
        info!(addr = %addr, "starting Shield-facing Connector gRPC server");
        self.serve_on(listener, certs, SHIELD_TLS_HANDSHAKE_TIMEOUT)
            .await
    }

    /// `serve` on an already-bound listener (tests bind port 0).
    pub(crate) async fn serve_on(
        self,
        listener: TcpListener,
        certs: Arc<CertHolder>,
        handshake_timeout: Duration,
    ) -> Result<()> {
        let tls =
            build_shield_server_tls(certs).context("failed to configure Shield server mTLS")?;
        let acceptor = TlsAcceptor::from(Arc::new(tls));

        let (conn_tx, conn_rx) = mpsc::channel(SHIELD_ACCEPTED_QUEUE_CAP);
        let accept_loop = tokio::spawn(accept_shield_connections(
            listener,
            acceptor,
            handshake_timeout,
            conn_tx,
            self.revocations_synced.clone(),
        ));

        // The raw tokio-rustls `TlsStream<TcpStream>` goes to tonic unwrapped:
        // tonic's `Connected` impl for exactly that type records the peer
        // certificate chain that `request.peer_certs()` returns, which
        // extract_shield_identity / verify_shield_identity depend on. Wrapping
        // it in another stream type would silently drop the client certificate.
        let incoming = ReceiverStream::new(conn_rx).map(Ok::<_, std::io::Error>);
        let result = Server::builder()
            .add_service(ShieldServiceServer::new(self))
            .serve_with_incoming(incoming)
            .await
            .context("Shield-facing Connector gRPC server failed");
        accept_loop.abort();
        result
    }

    /// Extract and verify shield identity purely from the peer certificate SPIFFE URI.
    /// Used by the Control stream handler (no claimed_id in the request body).
    fn extract_shield_identity<T>(
        &self,
        request: &Request<T>,
    ) -> Result<VerifiedShieldIdentity, Status> {
        let expected_prefix = format!("spiffe://{}/shield/", self.trust_domain);

        let peer_certs = request
            .peer_certs()
            .ok_or_else(|| Status::permission_denied("missing mTLS peer certificate"))?;
        let leaf_cert = peer_certs
            .first()
            .ok_or_else(|| Status::permission_denied("empty mTLS certificate chain"))?;

        let (_, cert) = X509Certificate::from_der(leaf_cert.as_ref())
            .map_err(|_| Status::permission_denied("invalid shield peer certificate"))?;

        let san = cert
            .subject_alternative_name()
            .map_err(|_| Status::permission_denied("invalid shield certificate SAN"))?
            .ok_or_else(|| Status::permission_denied("shield certificate missing SAN"))?;

        let mut found_id: Option<String> = None;
        for name in &san.value.general_names {
            if let GeneralName::URI(uri) = name {
                if let Some(id) = uri.strip_prefix(&expected_prefix) {
                    if !id.is_empty() && !id.contains('/') {
                        found_id = Some(id.to_string());
                        break;
                    }
                }
            }
        }

        let shield_id = found_id.ok_or_else(|| {
            Status::permission_denied("shield cert SPIFFE identity not in expected trust domain")
        })?;
        self.admit(&shield_id)?;

        let cert_not_after_unix = cert.validity().not_after.timestamp();
        info!(
            shield_id = %shield_id,
            connector_id = %self.connector_id,
            "verified shield mTLS identity on Control stream"
        );
        Ok(VerifiedShieldIdentity {
            shield_id,
            cert_not_after_unix,
        })
    }

    /// Verify shield identity for unary RPCs that carry a claimed shield_id.
    fn verify_shield_identity(
        &self,
        request: &Request<impl Sized>,
        claimed_shield_id: &str,
    ) -> Result<VerifiedShieldIdentity, Status> {
        if claimed_shield_id.trim().is_empty() {
            return Err(Status::permission_denied("missing shield identity"));
        }

        let expected_spiffe = format!(
            "spiffe://{}/shield/{}",
            self.trust_domain, claimed_shield_id
        );

        let peer_certs = request
            .peer_certs()
            .ok_or_else(|| Status::permission_denied("missing mTLS peer certificate"))?;
        let leaf_cert = peer_certs
            .first()
            .ok_or_else(|| Status::permission_denied("empty mTLS peer certificate chain"))?;

        let (_, cert) = X509Certificate::from_der(leaf_cert.as_ref())
            .map_err(|_| Status::permission_denied("invalid Shield peer certificate"))?;

        let san = cert
            .subject_alternative_name()
            .map_err(|_| Status::permission_denied("invalid Shield certificate SAN"))?
            .ok_or_else(|| Status::permission_denied("Shield certificate missing SAN"))?;

        let mut verified_spiffe = None;
        for name in &san.value.general_names {
            if let GeneralName::URI(uri) = name {
                if *uri == expected_spiffe {
                    verified_spiffe = Some((*uri).to_string());
                    break;
                }
            }
        }

        let spiffe_id = verified_spiffe.ok_or_else(|| {
            Status::permission_denied("Shield certificate SPIFFE identity mismatch")
        })?;
        self.admit(claimed_shield_id)?;

        let cert_not_after_unix = cert.validity().not_after.timestamp();
        info!(
            shield_id = %claimed_shield_id,
            spiffe_id = %spiffe_id,
            connector_id = %self.connector_id,
            "verified shield mTLS SPIFFE identity"
        );
        Ok(VerifiedShieldIdentity {
            shield_id: claimed_shield_id.to_string(),
            cert_not_after_unix,
        })
    }

    fn cert_needs_renewal(&self, cert_not_after_unix: i64) -> bool {
        let now = OffsetDateTime::now_utc().unix_timestamp();
        let window = self.renewal_window_secs.load(Ordering::Relaxed);
        cert_not_after_unix.saturating_sub(now) <= window.min(i64::MAX as u64) as i64
    }

    /// Apply the controller's AgentPolicy shield renewal window (K2). The
    /// controller already clamped it to 2/5 of the Shield certificate TTL.
    /// 0 means "not set" and keeps the current window.
    pub fn set_shield_renewal_window(&self, secs: u64) {
        if secs == 0 {
            return;
        }
        let previous = self.renewal_window_secs.swap(secs, Ordering::Relaxed);
        if previous != secs {
            info!(
                shield_renewal_window_secs = secs,
                previous, "applied AgentPolicy shield renewal window"
            );
        }
    }

    pub fn shield_renewal_window_secs(&self) -> u64 {
        self.renewal_window_secs.load(Ordering::Relaxed)
    }
}

/// Throttle for ReEnroll on one Shield Control stream (K2): send when none
/// was sent on this stream yet, or the last one is at least
/// SHIELD_REENROLL_RESEND_INTERVAL old.
fn reenroll_due(last_sent: Option<std::time::Instant>, now: std::time::Instant) -> bool {
    match last_sent {
        None => true,
        Some(at) => now.saturating_duration_since(at) >= SHIELD_REENROLL_RESEND_INTERVAL,
    }
}

/// Queue a ReEnroll on a Shield Control stream if the Shield needs renewal
/// and the per-stream throttle allows it. Only a ReEnroll that was actually
/// queued starts the throttle: a failed send leaves `last_sent` unchanged.
/// Returns whether one was queued.
async fn send_reenroll_if_due(
    out_tx: &mpsc::Sender<Result<ShieldControlMessage, Status>>,
    needs_renewal: bool,
    last_sent: &mut Option<std::time::Instant>,
    now: std::time::Instant,
) -> bool {
    use crate::shield_proto::shield_control_message::Body;
    if !needs_renewal || !reenroll_due(*last_sent, now) {
        return false;
    }
    let sent = out_tx
        .send(Ok(ShieldControlMessage {
            body: Some(Body::ReEnroll(ReEnrollSignal {})),
        }))
        .await
        .is_ok();
    if sent {
        *last_sent = Some(now);
    }
    sent
}

#[derive(Debug, Clone)]
struct VerifiedShieldIdentity {
    shield_id: String,
    cert_not_after_unix: i64,
}

fn unix_now() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_secs() as i64)
        .unwrap_or_default()
}

/// Convert an ACL snapshot's `connector_tunnel_addr` (host:9092 QUIC) into
/// the Shield-facing gRPC form (host:9091) by stripping the trailing
/// `:<port>` and appending `:9091`. Non-default port topologies are out of
/// scope — this function assumes the standard split (`:9092` tunnel /
/// `:9091` shield gRPC).
///
/// Preserves IPv6 bracketed forms (`[::1]:9092` → `[::1]:9091`).
pub(crate) fn derive_grpc_addr(tunnel_addr: &str) -> String {
    if let Some(idx) = tunnel_addr.rfind(':') {
        // Guard against a bare `:9091`-less input we accidentally split at
        // an IPv6 colon: rely on `rfind` (last colon) so `[::1]:9092` works.
        format!("{}:9091", &tunnel_addr[..idx])
    } else {
        // No port present; append :9091 as a best effort.
        format!("{tunnel_addr}:9091")
    }
}

/// Accept loop for the Shield-facing server (Sprint 20 G-2b).
///
/// It never awaits a TLS handshake itself: each accepted socket gets its own
/// task, bounded by `handshake_timeout`, so a stalled or hostile client cannot
/// hold up anyone else. It never waits on `conn_tx` either — only the
/// per-connection task does, after its handshake — so a momentarily slow gRPC
/// server cannot stop new TCP accepts.
async fn accept_shield_connections(
    listener: TcpListener,
    acceptor: TlsAcceptor,
    handshake_timeout: Duration,
    conn_tx: mpsc::Sender<TlsStream<TcpStream>>,
    revocations_synced: Arc<AtomicBool>,
) {
    loop {
        let (tcp, peer) = tokio::select! {
            accepted = listener.accept() => match accepted {
                Ok(pair) => pair,
                Err(err) => {
                    // e.g. EMFILE: back off briefly instead of spinning.
                    warn!(error = %err, "Shield-facing server accept failed");
                    tokio::time::sleep(Duration::from_millis(100)).await;
                    continue;
                }
            },
            // The gRPC server stopped consuming connections.
            _ = conn_tx.closed() => return,
        };
        // Certificate audit bug 3: no shield connection is accepted before
        // this process applied the controller's revocation list.
        if !revocations_synced.load(Ordering::SeqCst) {
            debug!(peer = %peer, "shield connection refused: revocation list not yet applied");
            continue;
        }
        // Same as tonic's own listener default.
        let _ = tcp.set_nodelay(true);

        let acceptor = acceptor.clone();
        let conn_tx = conn_tx.clone();
        tokio::spawn(async move {
            match tokio::time::timeout(handshake_timeout, acceptor.accept(tcp)).await {
                Ok(Ok(tls)) => {
                    // Err only if the gRPC server has stopped; the connection
                    // is dropped with it.
                    let _ = conn_tx.send(tls).await;
                }
                Ok(Err(err)) => {
                    debug!(peer = %peer, error = %err, "Shield TLS handshake failed");
                }
                Err(_) => {
                    debug!(peer = %peer, "Shield TLS handshake timed out");
                }
            }
        });
    }
}

#[cfg(test)]
mod peer_addr_tests {
    use super::derive_grpc_addr;

    #[test]
    fn ipv4_swaps_port() {
        assert_eq!(derive_grpc_addr("10.0.0.5:9092"), "10.0.0.5:9091");
    }

    #[test]
    fn hostname_swaps_port() {
        assert_eq!(
            derive_grpc_addr("connector.example.com:9092"),
            "connector.example.com:9091"
        );
    }

    #[test]
    fn ipv6_bracketed_swaps_port() {
        assert_eq!(derive_grpc_addr("[::1]:9092"), "[::1]:9091");
    }

    #[test]
    fn no_port_appends_9091() {
        assert_eq!(derive_grpc_addr("bare-host"), "bare-host:9091");
    }
}

#[tonic::async_trait]
impl ShieldService for ShieldRegistry {
    type ControlStream = ReceiverStream<Result<ShieldControlMessage, Status>>;

    async fn enroll(
        &self,
        _request: Request<EnrollRequest>,
    ) -> Result<Response<EnrollResponse>, Status> {
        Err(Status::unimplemented(
            "Shield enrolls directly with Controller, not through Connector",
        ))
    }

    async fn control(
        &self,
        request: Request<Streaming<ShieldControlMessage>>,
    ) -> Result<Response<Self::ControlStream>, Status> {
        let identity = self.extract_shield_identity(&request)?;
        let mut in_stream = request.into_inner();

        let (out_tx, out_rx) = mpsc::channel::<Result<ShieldControlMessage, Status>>(32);
        let (instr_tx, mut instr_rx) =
            mpsc::channel::<ResourceInstruction>(SHIELD_INSTRUCTION_QUEUE_CAP);
        let (snap_tx, mut snap_rx) = mpsc::channel::<ResourceSnapshot>(8);
        // Tunnel send channel — hub enqueues TunnelOpen/Data/Close messages to deliver to this Shield.
        let (tunnel_tx, mut tunnel_rx) = mpsc::channel::<ShieldControlMessage>(64);

        // Certificate audit bug 3: check and register in one critical section,
        // so a concurrent revocation either refuses this stream here or finds
        // its cancel token and closes it. The hub keeps its own revoked set for
        // the same reason (revocation updates the maps first, then the hub).
        let cancel = CancellationToken::new();
        // Subscribed before admission: a close requested after this point
        // ends this stream too.
        let mut close_rx = self.close_all.subscribe();
        close_rx.borrow_and_update();
        let stream_id = self.next_stream_id.fetch_add(1, Ordering::Relaxed);
        {
            let mut maps = self.maps.lock();
            if !self.revocations_synced.load(Ordering::SeqCst) {
                return Err(Status::unavailable(
                    "connector has not yet applied the controller's shield revocation list",
                ));
            }
            // Connector-revocation B1 (§7 step 2): not while the connector
            // itself is unauthorized (Starting, Disabled, terminal, past D).
            if !self.serving_gate.admits_now() {
                return Err(self.not_serving_status());
            }
            if maps.revoked.contains(&identity.shield_id) {
                return Err(Status::permission_denied("shield is revoked"));
            }
            maps.instruction_txs
                .insert(identity.shield_id.clone(), instr_tx.clone());
            maps.snapshot_txs
                .insert(identity.shield_id.clone(), snap_tx.clone());
            maps.stream_cancels
                .entry(identity.shield_id.clone())
                .or_default()
                .push((stream_id, cancel.clone()));
        }
        if !self
            .tunnel_hub
            .register_shield(identity.shield_id.clone(), stream_id, tunnel_tx)
        {
            // Revoked in between: apply_revocations already cleaned the maps.
            return Err(Status::permission_denied("shield is revoked"));
        }

        let registry = self.clone();
        let shield_id = identity.shield_id.clone();
        let cert_not_after = identity.cert_not_after_unix;
        // Per-stream ReEnroll throttle (K2): when the last ReEnroll was
        // actually delivered to this Shield's outbound queue.
        let mut last_reenroll_sent: Option<std::time::Instant> = None;
        // Fix 14 handover: true only when the Shield half-closed this stream
        // after sending everything (it does so after opening its next stream
        // on a renewal). Any other end closes this stream's sessions.
        let mut clean_end = false;

        tokio::spawn(async move {
            info!(shield_id = %shield_id, "shield Control stream connected");

            // ADR-004 Phase 2: replay the cached desired-state snapshot first —
            // a rebooted shield starts empty and must be re-protected. The cache
            // is read (not consumed): it must survive for the next reconnect too.
            let cached = registry
                .maps
                .lock()
                .resource_snapshots
                .get(&shield_id)
                .cloned();
            if let Some(snap) = cached {
                use crate::shield_proto::shield_control_message::Body;
                let generation = snap.generation;
                if out_tx
                    .send(Ok(ShieldControlMessage {
                        body: Some(Body::ResourceSnapshot(snap)),
                    }))
                    .await
                    .is_err()
                {
                    warn!(shield_id = %shield_id, "failed to replay cached snapshot on connect");
                } else {
                    info!(shield_id = %shield_id, generation, "replayed cached resource snapshot on connect");
                }
            }

            let buffered = registry
                .maps
                .lock()
                .resource_instructions
                .remove(&shield_id)
                .unwrap_or_default();
            for instr in buffered {
                use crate::shield_proto::shield_control_message::Body;
                if out_tx
                    .send(Ok(ShieldControlMessage {
                        body: Some(Body::ResourceInstruction(instr)),
                    }))
                    .await
                    .is_err()
                {
                    break;
                }
            }

            // Fix 14 (D-1 expiry rule): the stream lives no longer than the
            // certificate it was opened with. A shield that renewed in time
            // is already on a newer stream; one that didn't is cut off here.
            let cert_expiry = tokio::time::sleep(Duration::from_secs(
                cert_not_after.saturating_sub(unix_now()).max(0) as u64,
            ));
            tokio::pin!(cert_expiry);

            loop {
                tokio::select! {
                    biased;
                    // Certificate audit bug 3: the shield was revoked.
                    _ = cancel.cancelled() => {
                        let _ = out_tx.try_send(Err(Status::permission_denied("shield is revoked")));
                        break;
                    }
                    _ = &mut cert_expiry => {
                        warn!(shield_id = %shield_id, "shield certificate expired: closing its Control stream");
                        let _ = out_tx.try_send(Err(Status::unauthenticated("shield certificate expired")));
                        break;
                    }
                    // Connector-revocation fail-closed: this connector stopped
                    // serving; the shield should fail over.
                    _ = close_rx.changed() => {
                        let _ = out_tx.try_send(Err(registry.not_serving_status()));
                        break;
                    }
                    msg = in_stream.message() => {
                        match msg {
                            Ok(Some(m)) => {
                                use crate::shield_proto::shield_control_message::Body;
                                match m.body {
                                    Some(Body::HealthReport(hr)) => {
                                        registry.maps.lock().health
                                            .insert(shield_id.clone(), ShieldEntry {
                                                status: "active".to_string(),
                                                version: hr.version,
                                                last_seen_unix: unix_now(),
                                                lan_ip: hr.lan_ip,
                                            });
                                        // Piggyback the current peer-Connector list so the
                                        // Shield can fail over if this Connector goes down.
                                        // Empty / snapshot-missing skips the push per design.
                                        if let Some(list) = registry.build_peer_connector_list() {
                                            let _ = out_tx
                                                .send(Ok(ShieldControlMessage {
                                                    body: Some(Body::PeerConnectorList(list)),
                                                }))
                                                .await;
                                        }
                                        send_reenroll_if_due(
                                            &out_tx,
                                            registry.cert_needs_renewal(cert_not_after),
                                            &mut last_reenroll_sent,
                                            std::time::Instant::now(),
                                        )
                                        .await;
                                    }
                                    Some(Body::ResourceAck(ack)) => {
                                        let _ = registry.ack_tx.send((shield_id.clone(), ack)).await;
                                    }
                                    Some(Body::DiscoveryReport(report)) => {
                                        let added = report.added.len();
                                        let removed = report.removed.len();
                                        let full_sync = report.full_sync;
                                        registry.maps.lock().pending_discovery
                                            .insert(shield_id.clone(), report);
                                        info!(
                                            shield_id = %shield_id,
                                            added,
                                            removed,
                                            full_sync,
                                            "received DiscoveryReport from shield (buffered for upstream flush)"
                                        );
                                    }
                                    // ADR-004 Phase 3: latest actual-state report,
                                    // buffered for upstream flush on the health tick.
                                    Some(Body::ResourceState(report)) => {
                                        registry.maps.lock().pending_state
                                            .insert(shield_id.clone(), report);
                                    }
                                    Some(Body::Pong(_)) => {}
                                    // RDE tunnel responses from Shield → dispatch to relay sessions.
                                    Some(Body::TunnelOpened(p)) => {
                                        registry.tunnel_hub.dispatch_opened(&p.connection_id, p.ok, p.error.clone());
                                    }
                                    Some(Body::TunnelData(p)) => {
                                        registry.tunnel_hub.dispatch_data(&p.connection_id, p.data.clone(), stream_id).await;
                                    }
                                    Some(Body::TunnelClose(p)) => {
                                        registry.tunnel_hub.dispatch_close(&p.connection_id, p.error.clone(), stream_id);
                                    }
                                    _ => {}
                                }
                            }
                            Ok(None) => {
                                clean_end = true;
                                break;
                            }
                            Err(e) => {
                                warn!(shield_id = %shield_id, error = %e, "shield Control stream error");
                                break;
                            }
                        }
                    }
                    Some(instr) = instr_rx.recv() => {
                        use crate::shield_proto::shield_control_message::Body;
                        let msg = ShieldControlMessage {
                            body: Some(Body::ResourceInstruction(instr)),
                        };
                        if out_tx.send(Ok(msg)).await.is_err() {
                            break;
                        }
                    }
                    // ADR-004 Phase 2: forward fresh desired-state snapshots live.
                    Some(snap) = snap_rx.recv() => {
                        use crate::shield_proto::shield_control_message::Body;
                        if out_tx.send(Ok(ShieldControlMessage {
                            body: Some(Body::ResourceSnapshot(snap)),
                        })).await.is_err() {
                            break;
                        }
                    }
                    // RDE: forward tunnel messages from the hub to the Shield's output stream.
                    Some(msg) = tunnel_rx.recv() => {
                        if out_tx.send(Ok(msg)).await.is_err() {
                            break;
                        }
                    }
                }
            }
            {
                // Fix 14 (F45): remove the channels only if they are still this
                // stream's. A newer stream of the same shield may have
                // registered before this one's end was noticed (half-open TCP).
                let mut maps = registry.maps.lock();
                if maps
                    .instruction_txs
                    .get(&shield_id)
                    .is_some_and(|tx| tx.same_channel(&instr_tx))
                {
                    maps.instruction_txs.remove(&shield_id);
                }
                if maps
                    .snapshot_txs
                    .get(&shield_id)
                    .is_some_and(|tx| tx.same_channel(&snap_tx))
                {
                    maps.snapshot_txs.remove(&shield_id); // cache itself survives for the next reconnect
                }
                if let Some(tokens) = maps.stream_cancels.get_mut(&shield_id) {
                    tokens.retain(|(id, _)| *id != stream_id);
                    if tokens.is_empty() {
                        maps.stream_cancels.remove(&shield_id);
                    }
                }
            }
            let handed_over = registry.tunnel_hub.end_stream(&shield_id, stream_id, clean_end);
            if !handed_over.is_empty() {
                // Device data already queued for this stream still goes out on
                // it, before its end: the Shield reads this stream to its end
                // before it reads the new one. close() first, then drain — a
                // send racing the close either lands here or fails and is
                // retried on the new stream (agent_tunnel::send_to_shield).
                tunnel_rx.close();
                while let Some(msg) = tunnel_rx.recv().await {
                    if out_tx.send(Ok(msg)).await.is_err() {
                        break;
                    }
                }
                // Shield data that arrived early on the new stream goes to the
                // devices only now, after everything this stream carried.
                for connection_id in handed_over {
                    let hub = registry.tunnel_hub.clone();
                    tokio::spawn(async move { hub.deliver_held_events(&connection_id).await });
                }
            }
            info!(shield_id = %shield_id, "shield Control stream disconnected");
        });

        Ok(Response::new(ReceiverStream::new(out_rx)))
    }

    async fn renew_cert(
        &self,
        request: Request<RenewCertRequest>,
    ) -> Result<Response<RenewCertResponse>, Status> {
        let verified = self.verify_shield_identity(&request, &request.get_ref().shield_id)?;
        let req = request.into_inner();

        info!(shield_id = %verified.shield_id, "proxying shield cert renewal to controller");

        let mut client = ShieldServiceClient::new(self.controller_channel());
        client.renew_cert(Request::new(req)).await.map_err(|err| {
            warn!(shield_id = %verified.shield_id, error = %err, "shield cert renewal proxy failed");
            Status::unavailable("failed to proxy shield cert renewal to controller")
        })
    }

    async fn goodbye(
        &self,
        request: Request<GoodbyeRequest>,
    ) -> Result<Response<GoodbyeResponse>, Status> {
        let verified = self.verify_shield_identity(&request, &request.get_ref().shield_id)?;
        let req = request.into_inner();

        let removed = self
            .maps
            .lock()
            .health
            .remove(&verified.shield_id)
            .is_some();

        info!(
            shield_id = %verified.shield_id,
            claimed_shield_id = %req.shield_id,
            removed = removed,
            "shield goodbye received"
        );

        Ok(Response::new(GoodbyeResponse { ok: true }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_registry() -> ShieldRegistry {
        // connect_lazy never dials until first use; push_instructions' offline path
        // never touches the channel, so this is a cheap, network-free registry.
        let channel = Channel::from_static("http://127.0.0.1:1").connect_lazy();
        let (ack_tx, _ack_rx) = mpsc::channel(8);
        let policy_cache = Arc::new(crate::policy::PolicyCache::new());
        ShieldRegistry::new(
            channel,
            "test.example".to_string(),
            "connector-1".to_string(),
            ack_tx,
            policy_cache,
            crate::authorization::AuthzGate::open_for_tests(),
        )
    }

    fn instr(id: &str) -> ResourceInstruction {
        ResourceInstruction {
            resource_id: id.to_string(),
            host: "10.0.0.1".to_string(),
            protocol: "tcp".to_string(),
            port_from: 80,
            port_to: 80,
            action: "apply".to_string(),
        }
    }

    // F17: while a shield is offline (no instruction_txs entry), a second push must
    // APPEND to the buffered batch, not overwrite it. Regression guard for the
    // `insert`→`entry().or_default().extend()` fix in push_instructions.
    #[tokio::test]
    async fn offline_pushes_append_not_overwrite() {
        let reg = test_registry();
        reg.push_instructions("shield-A", vec![instr("r1")]);
        reg.push_instructions("shield-A", vec![instr("r2")]);

        let maps = reg.maps.lock();
        let buffered = maps
            .resource_instructions
            .get("shield-A")
            .expect("buffered batch present for offline shield");
        let ids: Vec<&str> = buffered.iter().map(|i| i.resource_id.as_str()).collect();
        assert_eq!(
            ids,
            vec!["r1", "r2"],
            "second offline push must append, not overwrite the first"
        );
    }

    // F18: with the shield online, instructions must reach the channel in push order
    // across separate push_instructions calls. Under the old spawn-per-push this raced
    // (each call spawned an independent task); the in-order try_send makes it
    // deterministic. Relies on the single-producer invariant (this test is the only
    // producer here, mirroring the sequential dispatcher).
    #[tokio::test]
    async fn online_pushes_preserve_arrival_order() {
        let reg = test_registry();
        let (tx, mut rx) = mpsc::channel::<ResourceInstruction>(8);
        reg.maps
            .lock()
            .instruction_txs
            .insert("shield-B".to_string(), tx);

        reg.push_instructions("shield-B", vec![instr("a1")]);
        reg.push_instructions("shield-B", vec![instr("a2"), instr("a3")]);

        let mut got = Vec::new();
        for _ in 0..3 {
            got.push(rx.recv().await.expect("instruction delivered").resource_id);
        }
        assert_eq!(
            got,
            vec!["a1", "a2", "a3"],
            "instructions must arrive in push order"
        );
    }
}

#[cfg(test)]
mod controller_channel_refresh_tests {
    use super::*;
    use crate::test_support::TestPki;

    fn registry() -> ShieldRegistry {
        let (ack_tx, _ack_rx) = mpsc::channel(8);
        ShieldRegistry::new(
            Channel::from_static("http://127.0.0.1:1").connect_lazy(),
            "ws-test.zecurity.in".to_string(),
            crate::test_support::CONNECTOR_ID.to_string(),
            ack_tx,
            Arc::new(crate::policy::PolicyCache::new()),
            crate::authorization::AuthzGate::open_for_tests(),
        )
    }

    /// When the CertHolder publishes a renewal, the Shield-proxy controller
    /// channel is rebuilt immediately with the renewed certificate.
    #[tokio::test]
    async fn shield_proxy_channel_rebuilds_on_publish() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let registry = registry();
        let built_with: Arc<Mutex<Vec<String>>> = Arc::new(Mutex::new(Vec::new()));

        let recorder = built_with.clone();
        let _task = registry.spawn_controller_channel_refresh(holder.clone(), move |material| {
            recorder.lock().push(material.serial_hex.clone());
            async move { Ok(Channel::from_static("http://127.0.0.1:1").connect_lazy()) }
        });
        assert_eq!(
            registry.controller_channel_swaps(),
            0,
            "no rebuild before a renewal"
        );

        let renewed = pki.renew(&holder, 7200);

        let deadline = tokio::time::Instant::now() + Duration::from_secs(5);
        while registry.controller_channel_swaps() == 0 {
            assert!(
                tokio::time::Instant::now() < deadline,
                "controller channel was not rebuilt"
            );
            tokio::time::sleep(Duration::from_millis(10)).await;
        }
        assert_eq!(registry.controller_channel_swaps(), 1);
        assert_eq!(
            built_with.lock().as_slice(),
            std::slice::from_ref(&renewed.serial_hex),
            "rebuilt with the renewed certificate"
        );
    }
}

#[cfg(test)]
mod shield_server_tls_tests {
    //! Sprint 20 G-2b: the :9091 server terminates TLS itself with the
    //! CertHolder resolver and hands raw tokio-rustls streams to tonic.

    use super::*;
    use crate::shield_proto::shield_control_message::Body;
    use crate::shield_proto::ShieldHealthReport;
    use crate::test_support::{leaf_serial, TestPki, CONNECTOR_ID, TRUST_DOMAIN};
    use tonic::transport::{Certificate, ClientTlsConfig, Endpoint, Identity};

    const SHIELD_ID: &str = "5d1c7f0e-2b8a-4e6d-9c3f-1a7b2e4d6f80";
    const OTHER_SHIELD_ID: &str = "c2a9e4b1-7d3f-4a8e-b6c5-0f1e2d3c4b5a";
    const WAIT: Duration = Duration::from_secs(5);

    /// A registry that has applied an (empty) authoritative revocation list —
    /// what every connected connector has (certificate audit bug 3).
    fn registry() -> ShieldRegistry {
        let r = unsynced_base_registry();
        r.apply_revocations(vec![]);
        r
    }

    fn unsynced_base_registry() -> ShieldRegistry {
        unsynced_base_registry_with_gate(crate::authorization::AuthzGate::open_for_tests())
    }

    fn unsynced_base_registry_with_gate(
        gate: crate::authorization::AuthzGate,
    ) -> ShieldRegistry {
        let (ack_tx, _ack_rx) = mpsc::channel(8);
        // A peer in this connector's remote network: every Shield health
        // report is then answered with a PeerConnectorList, which these tests
        // use as a liveness reply independent of ReEnroll.
        let policy_cache = Arc::new(crate::policy::PolicyCache::new());
        policy_cache.update(crate::client::v1::AclSnapshot {
            version: 1,
            workspace_id: "ws-test".into(),
            remote_networks: vec![crate::client::v1::AclRemoteNetwork {
                remote_network_id: "rn-1".into(),
                name: "rn-1".into(),
                connectors: vec![crate::client::v1::AclConnector {
                    connector_id: CONNECTOR_ID.into(),
                    connector_tunnel_addr: "127.0.0.1:9092".into(),
                    connector_spiffe: format!("spiffe://{TRUST_DOMAIN}/connector/{CONNECTOR_ID}"),
                    relay_addr: String::new(),
                    relay_spiffe_id: String::new(),
                }],
            }],
            ..Default::default()
        });
        ShieldRegistry::new(
            Channel::from_static("http://127.0.0.1:1").connect_lazy(),
            TRUST_DOMAIN.to_string(),
            CONNECTOR_ID.to_string(),
            ack_tx,
            policy_cache,
            gate,
        )
    }

    // ── Connector-revocation Phase E (D21) ─────────────────────────────────

    fn snapshot_for_peers(peers: &[&str], revoked: &[&str]) -> crate::client::v1::AclSnapshot {
        crate::client::v1::AclSnapshot {
            version: 2,
            workspace_id: "ws-test".into(),
            remote_networks: if peers.is_empty() {
                vec![]
            } else {
                vec![crate::client::v1::AclRemoteNetwork {
                    remote_network_id: "rn-1".into(),
                    name: "rn-1".into(),
                    connectors: peers
                        .iter()
                        .map(|id| crate::client::v1::AclConnector {
                            connector_id: id.to_string(),
                            connector_tunnel_addr: format!("{id}.lan:9092"),
                            ..Default::default()
                        })
                        .collect(),
                }]
            },
            revoked_connectors: revoked
                .iter()
                .map(|id| crate::client::v1::RevokedConnector {
                    connector_id: id.to_string(),
                    connector_spiffe: format!("spiffe://{TRUST_DOMAIN}/connector/{id}"),
                })
                .collect(),
            ..Default::default()
        }
    }

    /// AE-6: the peer list carries the ACL's revoked ids and never offers a
    /// revoked connector as a peer.
    #[tokio::test]
    async fn peer_list_carries_revoked_ids_and_drops_revoked_peers() {
        let r = registry();
        r.policy_cache.update(snapshot_for_peers(
            &[CONNECTOR_ID, "peer-b", "peer-a"],
            &["peer-b", "gone-x"],
        ));
        let list = r.build_peer_connector_list().expect("list");
        let peers: Vec<_> = list.peers.iter().map(|p| p.connector_id.as_str()).collect();
        let mut want = vec![CONNECTOR_ID, "peer-a"];
        want.sort();
        assert_eq!(peers, want);
        assert_eq!(list.revoked_connector_ids, vec!["gone-x", "peer-b"]);
    }

    /// AE-6: sent even when this connector is in no RN (no peers), so the
    /// Shield can still prune; nothing to send without either.
    #[tokio::test]
    async fn peer_list_is_sent_for_revoked_ids_alone() {
        let r = registry();
        r.policy_cache.update(snapshot_for_peers(&[], &["peer-b"]));
        let list = r
            .build_peer_connector_list()
            .expect("revoked ids alone are sent");
        assert!(list.peers.is_empty());
        assert_eq!(list.revoked_connector_ids, vec!["peer-b"]);

        r.policy_cache.update(snapshot_for_peers(&[], &[]));
        assert!(r.build_peer_connector_list().is_none());
    }

    /// Aborts the server task when the test ends.
    struct Running {
        addr: SocketAddr,
        task: JoinHandle<Result<()>>,
    }

    impl Drop for Running {
        fn drop(&mut self) {
            self.task.abort();
        }
    }

    async fn start(certs: Arc<CertHolder>, handshake_timeout: Duration) -> Running {
        start_with(registry(), certs, handshake_timeout).await
    }

    async fn start_with(
        registry: ShieldRegistry,
        certs: Arc<CertHolder>,
        handshake_timeout: Duration,
    ) -> Running {
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let task = tokio::spawn(registry.serve_on(listener, certs, handshake_timeout));
        Running { addr, task }
    }

    /// A tonic channel to the server, presenting `identity` (None = no client cert).
    async fn channel(
        pki: &TestPki,
        addr: SocketAddr,
        identity: Option<(String, String)>,
    ) -> std::result::Result<Channel, tonic::transport::Error> {
        let mut tls = ClientTlsConfig::new()
            .ca_certificate(Certificate::from_pem(&pki.workspace_ca_pem))
            .domain_name("localhost");
        if let Some((chain, key)) = identity {
            tls = tls.identity(Identity::from_pem(chain, key));
        }
        Endpoint::from_shared(format!("https://{addr}"))
            .unwrap()
            .tls_config(tls)?
            .connect_timeout(WAIT)
            .timeout(WAIT)
            .connect()
            .await
    }

    async fn shield_channel(pki: &TestPki, addr: SocketAddr) -> Channel {
        channel(pki, addr, Some(pki.shield_identity_pem(SHIELD_ID)))
            .await
            .expect("valid Shield connects")
    }

    async fn goodbye(
        channel: Channel,
        claimed_shield_id: &str,
    ) -> std::result::Result<GoodbyeResponse, Status> {
        ShieldServiceClient::new(channel)
            .goodbye(GoodbyeRequest {
                shield_id: claimed_shield_id.to_string(),
            })
            .await
            .map(Response::into_inner)
    }

    /// Shield-style rustls client config (ALPN h2).
    fn shield_tls_client(pki: &TestPki) -> Arc<rustls::ClientConfig> {
        let (chain, key) = pki.shield_chain(SHIELD_ID);
        let mut cfg = rustls::ClientConfig::builder()
            .with_root_certificates(pki.workspace_roots())
            .with_client_auth_cert(chain, key)
            .unwrap();
        cfg.alpn_protocols = vec![b"h2".to_vec()];
        Arc::new(cfg)
    }

    /// A raw TLS handshake to `addr`; returns the server leaf serial.
    async fn handshake_serial(client: Arc<rustls::ClientConfig>, addr: SocketAddr) -> String {
        let tcp = TcpStream::connect(addr).await.unwrap();
        let tls = tokio::time::timeout(
            WAIT,
            tokio_rustls::TlsConnector::from(client).connect(
                rustls::pki_types::ServerName::try_from("localhost").unwrap(),
                tcp,
            ),
        )
        .await
        .expect("handshake within timeout")
        .expect("TLS handshake");
        leaf_serial(&tls.get_ref().1.peer_certificates().unwrap()[0])
    }

    fn health_report() -> ShieldControlMessage {
        ShieldControlMessage {
            body: Some(Body::HealthReport(ShieldHealthReport {
                version: "test".into(),
                hostname: "shield-host".into(),
                public_ip: String::new(),
                lan_ip: "10.0.0.9".into(),
            })),
        }
    }

    async fn next_reply(replies: &mut Streaming<ShieldControlMessage>) -> ShieldControlMessage {
        tokio::time::timeout(WAIT, replies.message())
            .await
            .expect("reply within timeout")
            .expect("Control stream healthy")
            .expect("Control stream not closed")
    }

    /// Send a health report on an open Control stream; it must be answered,
    /// first, with a PeerConnectorList (proves the stream is alive), then —
    /// only if `expect_reenroll` — a ReEnroll.
    ///
    /// The connector sends PeerConnectorList before any ReEnroll, so a
    /// ReEnroll left over from the PREVIOUS report would arrive here ahead of
    /// this PeerConnectorList and fail the check: a round trip with
    /// `expect_reenroll = false` proves the previous report sent none.
    async fn health_round_trip(
        out: &mpsc::Sender<ShieldControlMessage>,
        replies: &mut Streaming<ShieldControlMessage>,
        expect_reenroll: bool,
    ) {
        out.send(health_report())
            .await
            .expect("Control stream open");
        let reply = next_reply(replies).await;
        assert!(
            matches!(reply.body, Some(Body::PeerConnectorList(_))),
            "expected PeerConnectorList first, got {reply:?}"
        );
        if expect_reenroll {
            let reply = next_reply(replies).await;
            assert!(
                matches!(reply.body, Some(Body::ReEnroll(_))),
                "expected ReEnroll, got {reply:?}"
            );
        }
    }

    async fn open_control(
        pki: &TestPki,
        addr: SocketAddr,
    ) -> (
        mpsc::Sender<ShieldControlMessage>,
        Streaming<ShieldControlMessage>,
    ) {
        let (out, out_rx) = mpsc::channel(8);
        let replies = ShieldServiceClient::new(shield_channel(pki, addr).await)
            .control(ReceiverStream::new(out_rx))
            .await
            .expect("Control stream accepted")
            .into_inner();
        (out, replies)
    }

    /// A Shield connected before the renewal keeps its Control stream (the
    /// server is not restarted), while a new handshake presents the renewed
    /// certificate.
    #[tokio::test]
    async fn existing_shield_stream_survives_renewal() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let server = start(holder.clone(), SHIELD_TLS_HANDSHAKE_TIMEOUT).await;

        let (out, mut replies) = open_control(&pki, server.addr).await;
        // The test Shield certificate (1 h) is inside the default 48 h window:
        // the first report on this stream is answered with ReEnroll.
        health_round_trip(&out, &mut replies, true).await;

        let renewed = pki.renew(&holder, 7200);
        assert_eq!(
            handshake_serial(shield_tls_client(&pki), server.addr).await,
            renewed.serial_hex,
            "new handshakes present the renewed certificate"
        );

        // The pre-renewal stream is still the same live connection, and the
        // ReEnroll is throttled: no repeat within 10 minutes on this stream.
        health_round_trip(&out, &mut replies, false).await;
        health_round_trip(&out, &mut replies, false).await;
    }

    /// K2 throttle: a Shield inside its window gets ONE ReEnroll per stream
    /// per 10 minutes, not one per health report; a new stream (the Shield
    /// reconnecting) starts its own throttle.
    #[tokio::test]
    async fn reenroll_is_throttled_per_stream() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let server = start(holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;

        let (out, mut replies) = open_control(&pki, server.addr).await;
        health_round_trip(&out, &mut replies, true).await;
        for _ in 0..4 {
            health_round_trip(&out, &mut replies, false).await;
        }

        let (out2, mut replies2) = open_control(&pki, server.addr).await;
        health_round_trip(&out2, &mut replies2, true).await;
        health_round_trip(&out2, &mut replies2, false).await;
    }

    /// K2 AgentPolicy: the controller's window replaces the 48 h default. The
    /// test Shield certificate expires in ~1 h: outside a 10 min window (no
    /// ReEnroll), inside a 2 h one (ReEnroll).
    #[tokio::test]
    async fn agent_policy_window_decides_reenroll() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let registry = registry();
        assert_eq!(
            registry.shield_renewal_window_secs(),
            DEFAULT_RENEWAL_WINDOW_SECS
        );
        let server = start_with(registry.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;

        registry.set_shield_renewal_window(600);
        let (out, mut replies) = open_control(&pki, server.addr).await;
        health_round_trip(&out, &mut replies, false).await;
        health_round_trip(&out, &mut replies, false).await;

        // 0 = "not set": keeps the current window.
        registry.set_shield_renewal_window(0);
        assert_eq!(registry.shield_renewal_window_secs(), 600);

        registry.set_shield_renewal_window(2 * 3600);
        let (out2, mut replies2) = open_control(&pki, server.addr).await;
        health_round_trip(&out2, &mut replies2, true).await;
        health_round_trip(&out2, &mut replies2, false).await;
    }

    /// K2: a ReEnroll that could not be queued does not consume the 10-min
    /// throttle; the next due report sends one, which then does.
    #[tokio::test]
    async fn failed_reenroll_send_does_not_start_the_throttle() {
        let t0 = std::time::Instant::now();
        let mut last = None;

        // Outbound queue closed: the send fails, the throttle is untouched.
        let (closed_tx, closed_rx) = mpsc::channel(1);
        drop(closed_rx);
        assert!(!send_reenroll_if_due(&closed_tx, true, &mut last, t0).await);
        assert_eq!(last, None);

        // A later report one second on: sent, and now the throttle runs.
        let (tx, mut rx) = mpsc::channel(4);
        let t1 = t0 + Duration::from_secs(1);
        assert!(send_reenroll_if_due(&tx, true, &mut last, t1).await);
        assert_eq!(last, Some(t1));
        assert!(matches!(
            rx.try_recv().unwrap().unwrap().body,
            Some(Body::ReEnroll(_))
        ));

        // Throttled for 10 min after the successful send, then due again.
        assert!(!send_reenroll_if_due(&tx, true, &mut last, t1 + Duration::from_secs(599)).await);
        assert!(rx.try_recv().is_err());
        assert!(send_reenroll_if_due(&tx, true, &mut last, t1 + Duration::from_secs(600)).await);

        // Outside the renewal window nothing is sent or recorded.
        let mut untouched = None;
        assert!(!send_reenroll_if_due(&tx, false, &mut untouched, t0).await);
        assert_eq!(untouched, None);
    }

    #[test]
    fn reenroll_due_throttles_for_ten_minutes() {
        let t0 = std::time::Instant::now();
        assert!(reenroll_due(None, t0));
        assert!(!reenroll_due(Some(t0), t0));
        assert!(!reenroll_due(
            Some(t0),
            t0 + Duration::from_secs(10 * 60 - 1)
        ));
        assert!(reenroll_due(Some(t0), t0 + Duration::from_secs(10 * 60)));
        // A clock that appears to go backwards never sends early.
        assert!(!reenroll_due(Some(t0 + Duration::from_secs(5)), t0));
    }

    /// Each new handshake presents whatever certificate the holder has at
    /// that moment, with no rebuild of the server.
    #[tokio::test]
    async fn new_shield_handshakes_present_renewed_serial() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let server = start(holder.clone(), SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        let client = shield_tls_client(&pki);

        let before = holder.current().serial_hex.clone();
        assert_eq!(handshake_serial(client.clone(), server.addr).await, before);

        let first = pki.renew(&holder, 7200);
        assert_ne!(first.serial_hex, before);
        assert_eq!(
            handshake_serial(client.clone(), server.addr).await,
            first.serial_hex
        );

        let second = pki.renew(&holder, 7200);
        assert_eq!(
            handshake_serial(client, server.addr).await,
            second.serial_hex
        );
    }

    /// Peer certificates reach the handlers through the custom TLS stream:
    /// verify_shield_identity accepts the matching Shield and denies a
    /// mismatched claim; clients without a trusted certificate are refused.
    #[tokio::test]
    async fn shield_identity_is_verified_through_custom_tls() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let server = start(holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        let shield = shield_channel(&pki, server.addr).await;

        let ok = goodbye(shield.clone(), SHIELD_ID)
            .await
            .expect("matching Shield identity accepted");
        assert!(ok.ok);

        let denied = goodbye(shield, OTHER_SHIELD_ID)
            .await
            .expect_err("mismatched Shield identity must be denied");
        assert_eq!(denied.code(), tonic::Code::PermissionDenied, "{denied:?}");

        // No client certificate: refused during the TLS handshake.
        let no_cert = match channel(&pki, server.addr, None).await {
            Ok(ch) => goodbye(ch, SHIELD_ID).await.is_err(),
            Err(_) => true,
        };
        assert!(no_cert, "a client without a certificate must be refused");

        // Certificate from a different CA: refused during the TLS handshake.
        let foreign = TestPki::new();
        let untrusted = match channel(
            &pki,
            server.addr,
            Some(foreign.shield_identity_pem(SHIELD_ID)),
        )
        .await
        {
            Ok(ch) => goodbye(ch, SHIELD_ID).await.is_err(),
            Err(_) => true,
        };
        assert!(
            untrusted,
            "a certificate from an untrusted CA must be refused"
        );

        // The server kept serving after the refusals.
        let again = goodbye(shield_channel(&pki, server.addr).await, SHIELD_ID).await;
        assert!(again.is_ok(), "{again:?}");
    }

    /// Clients that open TCP and never start TLS do not block a legitimate
    /// Shield: handshakes run in their own tasks, never inline in the accept loop.
    #[tokio::test]
    async fn stalled_clients_do_not_block_shields() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        // Long timeout: the stalled sockets stay open for the whole test.
        let server = start(holder, Duration::from_secs(60)).await;

        let mut stalled = Vec::new();
        for _ in 0..8 {
            stalled.push(TcpStream::connect(server.addr).await.unwrap());
        }

        let result = tokio::time::timeout(WAIT, async {
            goodbye(shield_channel(&pki, server.addr).await, SHIELD_ID).await
        })
        .await
        .expect("a Shield must not wait behind stalled clients");
        assert!(result.is_ok(), "{result:?}");
        drop(stalled);
    }

    /// A client that never completes its handshake is dropped after the
    /// handshake timeout.
    #[tokio::test]
    async fn stalled_handshake_is_closed_after_timeout() {
        use tokio::io::AsyncReadExt;

        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let server = start(holder, Duration::from_millis(200)).await;

        let mut stalled = TcpStream::connect(server.addr).await.unwrap();
        let mut buf = [0u8; 16];
        let read = tokio::time::timeout(WAIT, stalled.read(&mut buf))
            .await
            .expect("stalled socket must be closed by the server");
        assert!(
            matches!(read, Ok(0) | Err(_)),
            "expected EOF/reset, got {read:?}"
        );
    }

    /// Accepted connections waiting for tonic never stop the accept loop:
    /// with the hand-off queue full and nobody reading it, further clients
    /// still complete their TLS handshakes.
    #[tokio::test]
    async fn full_handoff_queue_does_not_stop_accepts() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let acceptor = TlsAcceptor::from(Arc::new(build_shield_server_tls(holder).unwrap()));
        let (conn_tx, mut conn_rx) = mpsc::channel(1);
        let accept_loop = tokio::spawn(accept_shield_connections(
            listener,
            acceptor,
            SHIELD_TLS_HANDSHAKE_TIMEOUT,
            conn_tx,
            Arc::new(AtomicBool::new(true)),
        ));

        // Nobody drains conn_rx: after the first connection the queue is full.
        let client = shield_tls_client(&pki);
        for _ in 0..4 {
            handshake_serial(client.clone(), addr).await;
        }

        // All four were handshaken and are waiting for hand-off.
        for _ in 0..4 {
            tokio::time::timeout(WAIT, conn_rx.recv())
                .await
                .expect("queued connection delivered")
                .expect("accept loop alive");
        }
        accept_loop.abort();
    }

    // ---- certificate audit bug 3: shield revocation ----------------------
    //
    // Revoking a shield used to change nothing on the connector: the revoked
    // shield kept its Control stream, kept carrying user sessions, could
    // reconnect, and was replayed its cached snapshot. The connector now
    // applies the controller's authoritative revocation list: it closes the
    // shield's stream, unregisters it, drops its snapshot, tears down every
    // session through it, refuses its identity, and accepts no shield at all
    // until it has applied that list.

    /// A registry that has not applied an authoritative revocation list
    /// (fresh process: connector start or restart). Its renewal window is
    /// tiny, so no ReEnroll interleaves with what these tests watch for.
    fn unsynced_registry() -> ShieldRegistry {
        let r = unsynced_base_registry();
        r.set_shield_renewal_window(1);
        r
    }

    /// A registry that has applied an (empty) authoritative revocation list,
    /// with the tiny renewal window.
    fn synced_registry() -> ShieldRegistry {
        let r = unsynced_registry();
        r.apply_revocations(vec![]);
        r
    }

    /// The Control stream must end — closed or failed — and deliver nothing
    /// else first.
    async fn expect_closed(replies: &mut Streaming<ShieldControlMessage>) {
        match tokio::time::timeout(WAIT, replies.message()).await {
            Err(_) => panic!("revoked shield's Control stream is still open"),
            Ok(Ok(None)) | Ok(Err(_)) => {}
            Ok(Ok(Some(m))) => panic!("revoked shield was sent {m:?}"),
        }
    }

    /// Whether `shield_id` can open a Control stream on `addr` and get a
    /// liveness reply.
    async fn control_admitted(pki: &TestPki, addr: SocketAddr, shield_id: &str) -> bool {
        let Ok(channel) = channel(pki, addr, Some(pki.shield_identity_pem(shield_id))).await else {
            return false;
        };
        let (out, out_rx) = mpsc::channel(8);
        let Ok(replies) = ShieldServiceClient::new(channel)
            .control(ReceiverStream::new(out_rx))
            .await
        else {
            return false;
        };
        let mut replies = replies.into_inner();
        if out.send(health_report()).await.is_err() {
            return false;
        }
        matches!(
            tokio::time::timeout(WAIT, replies.message()).await,
            Ok(Ok(Some(ShieldControlMessage {
                body: Some(Body::PeerConnectorList(_))
            })))
        )
    }

    /// Fix 14 (F45): when a shield's newer Control stream is registered
    /// before the older one ends, the older stream's cleanup must not remove
    /// the newer stream's channels: snapshots still reach the newer stream.
    #[tokio::test]
    async fn an_older_streams_end_keeps_the_newer_streams_channels() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let r = synced_registry();
        let server = start_with(r.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        let (old_out, mut old_replies) = open_control(&pki, server.addr).await;
        health_round_trip(&old_out, &mut old_replies, false).await;
        let (new_out, mut new_replies) = open_control(&pki, server.addr).await;
        health_round_trip(&new_out, &mut new_replies, false).await;

        // The older stream ends (the shield half-closes it) after the newer
        // one registered; wait until the connector has finished with it.
        drop(old_out);
        expect_closed(&mut old_replies).await;

        r.push_snapshot(
            SHIELD_ID,
            ResourceSnapshot {
                generation: 7,
                ..Default::default()
            },
        );
        let reply = next_reply(&mut new_replies).await;
        assert!(
            matches!(reply.body, Some(Body::ResourceSnapshot(ref s)) if s.generation == 7),
            "the newer stream lost its snapshot channel: got {reply:?}"
        );
    }

    /// Fix 14, D-1 = B, over real streams: the Shield opens a second Control
    /// stream (its renewed certificate) and only then half-closes the first;
    /// the session keeps running on the second stream, both ways.
    #[tokio::test]
    async fn a_session_survives_a_make_before_break_stream_handover() {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let r = synced_registry();
        let server = start_with(r.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        let (out1, mut replies1) = open_control(&pki, server.addr).await;
        health_round_trip(&out1, &mut replies1, false).await;

        let hub = r.tunnel_hub.clone();
        let opening = tokio::spawn(async move {
            hub.open_relay_session(SHIELD_ID, "10.0.0.9", 22, "tcp").await
        });
        let Some(Body::TunnelOpen(open)) = next_reply(&mut replies1).await.body else {
            panic!("expected TunnelOpen");
        };
        let cid = open.connection_id.clone();
        out1.send(ShieldControlMessage {
            body: Some(Body::TunnelOpened(crate::shield_proto::TunnelOpened {
                connection_id: cid.clone(),
                ok: true,
                error: String::new(),
            })),
        })
        .await
        .unwrap();
        let session = opening.await.unwrap().expect("session opened");
        let (mut device, relay_end) = tokio::io::duplex(1024);
        let relay = tokio::spawn(session.relay_stream(relay_end));

        // Renewal: stream 2 first (registered once control() returns) ...
        let (out2, mut replies2) = open_control(&pki, server.addr).await;
        // ... then stream 1 is half-closed after its last chunk.
        out1.send(ShieldControlMessage {
            body: Some(Body::TunnelData(crate::shield_proto::TunnelData {
                connection_id: cid.clone(),
                data: b"one".to_vec(),
            })),
        })
        .await
        .unwrap();
        drop(out1);
        expect_closed(&mut replies1).await;

        out2.send(ShieldControlMessage {
            body: Some(Body::TunnelData(crate::shield_proto::TunnelData {
                connection_id: cid.clone(),
                data: b"two".to_vec(),
            })),
        })
        .await
        .unwrap();
        let mut got = [0u8; 6];
        tokio::time::timeout(WAIT, device.read_exact(&mut got))
            .await
            .expect("shield data after the handover")
            .unwrap();
        assert_eq!(&got, b"onetwo");

        device.write_all(b"up").await.unwrap();
        let reply = loop {
            let m = next_reply(&mut replies2).await;
            if let Some(Body::TunnelData(d)) = m.body {
                break d;
            }
        };
        assert_eq!(reply.connection_id, cid);
        assert_eq!(reply.data, b"up");
        assert!(!relay.is_finished(), "the session must survive the handover");
    }

    /// Fix 14 (D-1 expiry rule): a shield Control stream is closed when the
    /// certificate it was opened with expires.
    #[tokio::test]
    async fn a_shield_stream_is_closed_when_its_certificate_expires() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let r = synced_registry();
        let server = start_with(r.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        // Valid for about 2 more seconds.
        let identity = pki.shield_identity_pem_with_validity(SHIELD_ID, -60, 62);
        let channel = channel(&pki, server.addr, Some(identity))
            .await
            .expect("still valid at connect");
        let (out, out_rx) = mpsc::channel(8);
        let mut replies = ShieldServiceClient::new(channel)
            .control(ReceiverStream::new(out_rx))
            .await
            .expect("Control stream accepted")
            .into_inner();
        out.send(health_report()).await.unwrap();
        assert!(matches!(
            next_reply(&mut replies).await.body,
            Some(Body::PeerConnectorList(_))
        ));

        match tokio::time::timeout(WAIT, replies.message()).await {
            Err(_) => panic!("the stream outlived its certificate"),
            Ok(Err(status)) => {
                assert_eq!(status.code(), tonic::Code::Unauthenticated, "{status:?}")
            }
            Ok(Ok(None)) => {}
            // ReEnroll (inside the renewal window) may come first; the
            // stream must still close right after.
            Ok(Ok(Some(m))) => {
                assert!(matches!(m.body, Some(Body::ReEnroll(_))), "{m:?}");
                expect_closed(&mut replies).await;
            }
        }
    }

    #[tokio::test]
    async fn revoking_a_shield_closes_its_open_control_stream() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let r = synced_registry();
        let server = start_with(r.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        let (out, mut replies) = open_control(&pki, server.addr).await;
        health_round_trip(&out, &mut replies, false).await;

        r.apply_revocations(vec![SHIELD_ID.to_string()]);
        expect_closed(&mut replies).await;
        assert!(r.is_revoked(SHIELD_ID));
    }

    #[tokio::test]
    async fn a_revoked_shield_cannot_reconnect_or_call_the_connector() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let r = synced_registry();
        let server = start_with(r.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        r.apply_revocations(vec![SHIELD_ID.to_string()]);

        assert!(
            !control_admitted(&pki, server.addr, SHIELD_ID).await,
            "a revoked shield reconnected"
        );
        let revoked = channel(&pki, server.addr, Some(pki.shield_identity_pem(SHIELD_ID))).await;
        if let Ok(ch) = revoked {
            let err = goodbye(ch.clone(), SHIELD_ID)
                .await
                .expect_err("a revoked shield's Goodbye must be refused");
            assert_eq!(err.code(), tonic::Code::PermissionDenied, "{err}");
            let err = ShieldServiceClient::new(ch)
                .renew_cert(crate::shield_proto::RenewCertRequest {
                    shield_id: SHIELD_ID.into(),
                    public_key_der: vec![1, 2, 3],
                })
                .await
                .expect_err("a revoked shield's RenewCert must be refused");
            assert_eq!(err.code(), tonic::Code::PermissionDenied, "{err}");
        }
        assert!(
            control_admitted(&pki, server.addr, OTHER_SHIELD_ID).await,
            "an unrevoked shield must still be admitted"
        );
    }

    #[tokio::test]
    async fn revocation_discards_and_suppresses_the_shields_snapshot_and_instructions() {
        let r = synced_registry();
        let snapshot = |generation| ResourceSnapshot {
            generation,
            ..Default::default()
        };
        r.push_snapshot(SHIELD_ID, snapshot(1));
        r.push_instructions(SHIELD_ID, vec![]);
        r.apply_revocations(vec![SHIELD_ID.to_string()]);
        r.push_snapshot(SHIELD_ID, snapshot(2));
        r.push_instructions(
            SHIELD_ID,
            vec![ResourceInstruction {
                resource_id: "r".into(),
                ..Default::default()
            }],
        );
        let maps = r.maps.lock();
        assert!(
            !maps.resource_snapshots.contains_key(SHIELD_ID),
            "a revoked shield's snapshot is still cached"
        );
        assert!(
            maps.resource_instructions
                .get(SHIELD_ID)
                .is_none_or(|v| v.is_empty()),
            "instructions are still buffered for a revoked shield"
        );
    }

    /// Missing initial synchronization: until the controller's authoritative
    /// list is applied, no shield is accepted.
    #[tokio::test]
    async fn no_shield_is_accepted_before_the_authoritative_list_is_applied() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let r = unsynced_registry();
        let server = start_with(r.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        // Not even a TLS handshake: the connection is dropped at accept.
        let tcp = TcpStream::connect(server.addr).await.unwrap();
        let handshake = tokio::time::timeout(
            WAIT,
            tokio_rustls::TlsConnector::from(shield_tls_client(&pki)).connect(
                rustls::pki_types::ServerName::try_from("localhost").unwrap(),
                tcp,
            ),
        )
        .await
        .expect("refused promptly");
        assert!(
            handshake.is_err(),
            "a shield connection was accepted before the revocation list was applied"
        );
        assert!(
            !control_admitted(&pki, server.addr, SHIELD_ID).await,
            "a shield was accepted before the revocation list was applied"
        );
        if let Ok(ch) = channel(&pki, server.addr, Some(pki.shield_identity_pem(SHIELD_ID))).await {
            assert!(goodbye(ch, SHIELD_ID).await.is_err());
        }

        r.apply_revocations(vec![]);
        assert!(control_admitted(&pki, server.addr, SHIELD_ID).await);
    }

    /// Connector-revocation B1 (AB1-1, T15): no shield Control stream while
    /// the connector itself is not authorized — Starting, then terminal — and
    /// admitted once the connector serves.
    #[tokio::test]
    async fn shield_control_follows_the_connector_serving_state() {
        use crate::authorization::{AckTracker, TerminalSignal};
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let authz = AckTracker::shared(Duration::from_secs(3600));
        let r = unsynced_base_registry_with_gate(authz.lock().gate());
        r.set_shield_renewal_window(1);
        r.apply_revocations(vec![]);
        let server = start_with(r.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;

        assert!(
            !control_admitted(&pki, server.addr, SHIELD_ID).await,
            "a shield was admitted while the connector was Starting"
        );
        {
            let mut t = authz.lock();
            let s = t.next_seq();
            let now = tokio::time::Instant::now();
            t.record_sent(s, now);
            t.on_ack(s, true, now).unwrap();
        }
        assert!(control_admitted(&pki, server.addr, SHIELD_ID).await);
        authz.lock().on_terminal(TerminalSignal::Revoked);
        assert!(
            !control_admitted(&pki, server.addr, SHIELD_ID).await,
            "a shield was admitted by a revoked connector"
        );
    }

    /// Connector-revocation B2 (AB2-5): when the connector stops serving, an
    /// open shield Control stream ends with `Unavailable` — so the shield
    /// fails over — not with the `PermissionDenied` of a shield revocation;
    /// the shield itself is not marked revoked.
    #[tokio::test]
    async fn closing_all_streams_ends_them_with_unavailable() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let r = synced_registry();
        let server = start_with(r.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        let (out, mut replies) = open_control(&pki, server.addr).await;
        health_round_trip(&out, &mut replies, false).await;

        r.close_all_streams("test");
        match tokio::time::timeout(WAIT, replies.message()).await {
            Ok(Err(status)) => assert_eq!(status.code(), tonic::Code::Unavailable, "{status:?}"),
            other => panic!("expected the stream to end with Unavailable, got {other:?}"),
        }
        assert!(!r.is_revoked(SHIELD_ID), "closing for the connector's sake revokes nothing");
    }

    /// The status a refused Control stream ends with: refused at open, or
    /// ended before the first reply.
    async fn control_refusal(pki: &TestPki, addr: SocketAddr) -> tonic::Status {
        let (out, out_rx) = mpsc::channel(8);
        match ShieldServiceClient::new(shield_channel(pki, addr).await)
            .control(ReceiverStream::new(out_rx))
            .await
        {
            Err(status) => status,
            Ok(replies) => {
                let mut replies = replies.into_inner();
                let _ = out.send(health_report()).await;
                match tokio::time::timeout(WAIT, replies.message()).await {
                    Ok(Err(status)) => status,
                    other => panic!("expected a refusal, got {other:?}"),
                }
            }
        }
    }

    /// Connector-revocation Phase F (finding B, AF-3): a terminal connector
    /// refuses shields with its own message, so they fail over instead of
    /// waiting; non-terminal states keep the not-serving message.
    #[tokio::test]
    async fn terminal_connector_refuses_shields_with_the_terminal_message() {
        use crate::authorization::{AuthzGate, ServingState};
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        for (state, want) in [
            (ServingState::Revoked, CONNECTOR_TERMINAL_MESSAGE),
            (ServingState::UnknownIdentity, CONNECTOR_TERMINAL_MESSAGE),
            (
                ServingState::Starting,
                "connector is not authorized to serve",
            ),
            (
                ServingState::Disabled,
                "connector is not authorized to serve",
            ),
        ] {
            let r = unsynced_base_registry_with_gate(AuthzGate::fixed(state, None));
            r.apply_revocations(vec![]);
            let server = start_with(r, holder.clone(), SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
            let status = control_refusal(&pki, server.addr).await;
            assert_eq!(
                status.code(),
                tonic::Code::Unavailable,
                "{state:?}: {status:?}"
            );
            assert_eq!(status.message(), want, "{state:?}");
        }
    }

    /// The terminal message is matched by text in the Shield
    /// (`shield/src/control_stream.rs`); both sides pin the same literal.
    #[test]
    fn terminal_message_matches_the_shield() {
        assert_eq!(
            CONNECTOR_TERMINAL_MESSAGE,
            "connector is revoked or unknown to the controller"
        );
    }

    /// AF-4: an open stream closed by the terminal transition ends with the
    /// terminal message; a close while Disabled keeps the not-serving one.
    #[tokio::test]
    async fn open_stream_closed_by_a_terminal_transition_carries_the_terminal_message() {
        use crate::authorization::{AuthzGate, ServingState};
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        for (state, want) in [
            (ServingState::Revoked, CONNECTOR_TERMINAL_MESSAGE),
            (
                ServingState::Disabled,
                "connector is not authorized to serve",
            ),
        ] {
            let (gate, control) = AuthzGate::controllable_for_tests();
            let r = unsynced_base_registry_with_gate(gate);
            r.set_shield_renewal_window(1);
            r.apply_revocations(vec![]);
            let server = start_with(r.clone(), holder.clone(), SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
            let (out, mut replies) = open_control(&pki, server.addr).await;
            health_round_trip(&out, &mut replies, false).await;

            control.set(state);
            r.close_all_streams("test");
            match tokio::time::timeout(WAIT, replies.message()).await {
                Ok(Err(status)) => {
                    assert_eq!(status.code(), tonic::Code::Unavailable, "{status:?}");
                    assert_eq!(status.message(), want, "{state:?}");
                }
                other => panic!("expected the stream to end with Unavailable, got {other:?}"),
            }
        }
    }

    /// Connector restart: revocation state is not trusted across processes —
    /// the new process refuses every shield until the controller resends the
    /// list, then refuses only the revoked ones.
    #[tokio::test]
    async fn after_a_restart_shields_wait_for_the_resent_list() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let before = synced_registry();
        before.apply_revocations(vec![SHIELD_ID.to_string()]);

        let restarted = unsynced_registry();
        let server = start_with(restarted.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        assert!(!control_admitted(&pki, server.addr, SHIELD_ID).await);
        assert!(!control_admitted(&pki, server.addr, OTHER_SHIELD_ID).await);

        restarted.apply_revocations(vec![SHIELD_ID.to_string()]);
        assert!(!control_admitted(&pki, server.addr, SHIELD_ID).await);
        assert!(control_admitted(&pki, server.addr, OTHER_SHIELD_ID).await);
    }

    /// A live user session relayed through the shield is torn down when the
    /// shield is revoked.
    #[tokio::test]
    async fn revocation_tears_down_live_sessions_through_the_shield() {
        use tokio::io::{AsyncReadExt, AsyncWriteExt};
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let r = synced_registry();
        let server = start_with(r.clone(), holder, SHIELD_TLS_HANDSHAKE_TIMEOUT).await;
        let (out, mut replies) = open_control(&pki, server.addr).await;
        health_round_trip(&out, &mut replies, false).await;

        let hub = r.tunnel_hub.clone();
        let opening = tokio::spawn(async move {
            hub.open_relay_session(SHIELD_ID, "10.0.0.9", 22, "tcp")
                .await
        });
        let open = next_reply(&mut replies).await;
        let Some(Body::TunnelOpen(open)) = open.body else {
            panic!("expected TunnelOpen, got {open:?}");
        };
        out.send(ShieldControlMessage {
            body: Some(Body::TunnelOpened(crate::shield_proto::TunnelOpened {
                connection_id: open.connection_id,
                ok: true,
                error: String::new(),
            })),
        })
        .await
        .unwrap();
        let session = opening.await.unwrap().expect("session opened");
        let (mut device, relay_end) = tokio::io::duplex(1024);
        let relay = tokio::spawn(session.relay_stream(relay_end));
        device.write_all(b"git push").await.unwrap();

        r.apply_revocations(vec![SHIELD_ID.to_string()]);
        tokio::time::timeout(WAIT, relay)
            .await
            .expect("the session through a revoked shield must end")
            .unwrap()
            .ok();
        let mut buf = [0u8; 16];
        let n = tokio::time::timeout(WAIT, device.read(&mut buf))
            .await
            .expect("device side must see the session end")
            .unwrap_or(0);
        assert_eq!(
            n, 0,
            "the device still receives data through a revoked shield"
        );
    }

    /// Sessions being established while the shield is revoked: every session
    /// that opened is torn down, and none opens after revocation returned.
    #[tokio::test(flavor = "multi_thread", worker_threads = 4)]
    async fn concurrent_session_establishment_and_revocation() {
        use std::sync::atomic::{AtomicBool, Ordering};
        for _round in 0..20 {
            let r = synced_registry();
            let (shield_tx, mut shield_rx) = mpsc::channel::<ShieldControlMessage>(1024);
            assert!(r
                .tunnel_hub
                .register_shield(SHIELD_ID.to_string(), 1, shield_tx));
            // A shield that accepts every tunnel.
            let hub = r.tunnel_hub.clone();
            tokio::spawn(async move {
                while let Some(m) = shield_rx.recv().await {
                    if let Some(Body::TunnelOpen(o)) = m.body {
                        hub.dispatch_opened(&o.connection_id, true, String::new());
                    }
                }
            });

            let revoked = Arc::new(AtomicBool::new(false));
            let mut tasks = Vec::new();
            for _ in 0..16 {
                let hub = r.tunnel_hub.clone();
                let revoked = revoked.clone();
                tasks.push(tokio::spawn(async move {
                    let started_after_revocation = revoked.load(Ordering::SeqCst);
                    if let Ok(session) = hub
                        .open_relay_session(SHIELD_ID, "10.0.0.9", 22, "tcp")
                        .await
                    {
                        assert!(
                            !started_after_revocation,
                            "a session opened after the shield was revoked"
                        );
                        let (_device, relay_end) = tokio::io::duplex(64);
                        tokio::time::timeout(WAIT, session.relay_stream(relay_end))
                            .await
                            .expect("a session through a revoked shield must end")
                            .ok();
                    }
                }));
            }
            tokio::task::yield_now().await;
            r.apply_revocations(vec![SHIELD_ID.to_string()]);
            revoked.store(true, Ordering::SeqCst);
            for task in tasks {
                task.await.expect("session task");
            }
            assert!(r
                .tunnel_hub
                .open_relay_session(SHIELD_ID, "10.0.0.9", 22, "tcp")
                .await
                .is_err());
        }
    }
}
