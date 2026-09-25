use parking_lot::{Mutex, RwLock};
use std::collections::HashMap;
use std::future::Future;
use std::net::SocketAddr;
use std::sync::atomic::{AtomicU64, Ordering};
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

const DEFAULT_RENEWAL_WINDOW_SECS: u64 = 48 * 60 * 60;
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
}

#[derive(Debug, Clone)]
struct ShieldEntry {
    status: String,
    version: String,
    last_seen_unix: i64,
    lan_ip: String,
}

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
    renewal_window_secs: u64,
    /// Tunnel hub — routes RDE tunnel messages between device connections and Shields.
    pub tunnel_hub: crate::agent_tunnel::AgentTunnelHub,
    /// Shared ACL snapshot cache. Read on every incoming ShieldHealthReport
    /// so the connector can piggyback the current peer-Connector list back to
    /// the Shield (see PeerConnectorList in the Shield Control stream).
    policy_cache: Arc<crate::policy::PolicyCache>,
}

impl ShieldRegistry {
    pub fn new(
        controller_channel: Channel,
        trust_domain: String,
        connector_id: String,
        ack_tx: mpsc::Sender<(String, ResourceAck)>,
        policy_cache: Arc<crate::policy::PolicyCache>,
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
            })),
            ack_tx,
            controller_channel: Arc::new(RwLock::new(controller_channel)),
            controller_channel_swaps: Arc::new(AtomicU64::new(0)),
            trust_domain,
            connector_id,
            renewal_window_secs: DEFAULT_RENEWAL_WINDOW_SECS,
            tunnel_hub: crate::agent_tunnel::AgentTunnelHub::new(),
            policy_cache,
        }
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
    /// Network, sorted deterministically by `connector_id`. Returns `None`
    /// when the ACL snapshot is missing or this Connector isn't in any RN
    /// (per design — the Shield ignores empty lists, and we prefer not to
    /// send them at all).
    ///
    /// Address derivation: `connector_tunnel_addr` in the ACL snapshot is
    /// `host:9092` (QUIC data plane). The Shield needs `host:9091` (gRPC).
    /// The port is swapped by stripping and re-attaching.
    pub(crate) fn build_peer_connector_list(
        &self,
    ) -> Option<crate::shield_proto::PeerConnectorList> {
        let mut peers: Vec<crate::shield_proto::PeerConnector> = self
            .policy_cache
            .peers_of_connector(&self.connector_id)
            .into_iter()
            .map(|(id, tunnel_addr)| crate::shield_proto::PeerConnector {
                connector_id: id,
                connector_addr: derive_grpc_addr(&tunnel_addr),
            })
            .collect();
        if peers.is_empty() {
            return None;
        }
        peers.sort_by(|a, b| a.connector_id.cmp(&b.connector_id));
        Some(crate::shield_proto::PeerConnectorList { peers })
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
        cert_not_after_unix.saturating_sub(now) <= self.renewal_window_secs as i64
    }
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

        {
            let mut maps = self.maps.lock();
            maps.instruction_txs
                .insert(identity.shield_id.clone(), instr_tx);
            maps.snapshot_txs
                .insert(identity.shield_id.clone(), snap_tx);
        }
        self.tunnel_hub
            .register_shield(identity.shield_id.clone(), tunnel_tx);

        let registry = self.clone();
        let shield_id = identity.shield_id.clone();
        let cert_not_after = identity.cert_not_after_unix;

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

            loop {
                tokio::select! {
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
                                        if registry.cert_needs_renewal(cert_not_after) {
                                            let _ = out_tx
                                                .send(Ok(ShieldControlMessage {
                                                    body: Some(Body::ReEnroll(ReEnrollSignal {})),
                                                }))
                                                .await;
                                        }
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
                                        registry.tunnel_hub.dispatch_data(&p.connection_id, p.data.clone());
                                    }
                                    Some(Body::TunnelClose(p)) => {
                                        registry.tunnel_hub.dispatch_close(&p.connection_id, p.error.clone());
                                    }
                                    _ => {}
                                }
                            }
                            Ok(None) => break,
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
                let mut maps = registry.maps.lock();
                maps.instruction_txs.remove(&shield_id);
                maps.snapshot_txs.remove(&shield_id); // cache itself survives for the next reconnect
            }
            registry.tunnel_hub.unregister_shield(&shield_id);
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

    fn registry() -> ShieldRegistry {
        let (ack_tx, _ack_rx) = mpsc::channel(8);
        ShieldRegistry::new(
            Channel::from_static("http://127.0.0.1:1").connect_lazy(),
            TRUST_DOMAIN.to_string(),
            CONNECTOR_ID.to_string(),
            ack_tx,
            Arc::new(crate::policy::PolicyCache::new()),
        )
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
        let listener = TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let task = tokio::spawn(registry().serve_on(listener, certs, handshake_timeout));
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

    /// Send a health report on an open Control stream and wait for the reply.
    /// The test Shield certificate (1 h) is inside the 48 h renewal window, so
    /// every report is answered with ReEnroll — a round trip that proves the
    /// stream is alive.
    async fn health_round_trip(
        out: &mpsc::Sender<ShieldControlMessage>,
        replies: &mut Streaming<ShieldControlMessage>,
    ) {
        out.send(health_report())
            .await
            .expect("Control stream open");
        let reply = tokio::time::timeout(WAIT, replies.message())
            .await
            .expect("reply within timeout")
            .expect("Control stream healthy")
            .expect("Control stream not closed");
        assert!(
            matches!(reply.body, Some(Body::ReEnroll(_))),
            "unexpected reply: {reply:?}"
        );
    }

    /// A Shield connected before the renewal keeps its Control stream (the
    /// server is not restarted), while a new handshake presents the renewed
    /// certificate.
    #[tokio::test]
    async fn existing_shield_stream_survives_renewal() {
        let pki = TestPki::new();
        let (_dir, holder) = pki.holder(-60, 3600);
        let server = start(holder.clone(), SHIELD_TLS_HANDSHAKE_TIMEOUT).await;

        let (out, out_rx) = mpsc::channel(8);
        let mut replies = ShieldServiceClient::new(shield_channel(&pki, server.addr).await)
            .control(ReceiverStream::new(out_rx))
            .await
            .expect("Control stream accepted")
            .into_inner();
        health_round_trip(&out, &mut replies).await;

        let renewed = pki.renew(&holder, 7200);
        assert_eq!(
            handshake_serial(shield_tls_client(&pki), server.addr).await,
            renewed.serial_hex,
            "new handshakes present the renewed certificate"
        );

        // The pre-renewal stream is still the same live connection.
        health_round_trip(&out, &mut replies).await;
        health_round_trip(&out, &mut replies).await;
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
}
