use parking_lot::Mutex;
use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::Path;
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use ::time::OffsetDateTime;
use anyhow::{Context, Result};
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::transport::{Certificate, Channel, Identity, Server, ServerTlsConfig};
use tonic::{Request, Response, Status, Streaming};
use tracing::{info, warn};
use x509_parser::prelude::*;

use crate::shield_proto::shield_service_client::ShieldServiceClient;
use crate::shield_proto::shield_service_server::{ShieldService, ShieldServiceServer};
use crate::shield_proto::{
    EnrollRequest, EnrollResponse, GoodbyeRequest, GoodbyeResponse, ReEnrollSignal,
    RenewCertRequest, RenewCertResponse, ResourceAck, ResourceInstruction, ResourceSnapshot,
    ResourceStateReport, ShieldControlMessage,
};

const DEFAULT_RENEWAL_WINDOW_SECS: u64 = 48 * 60 * 60;
const SHIELD_STALE_THRESHOLD_SECS: i64 = 90;

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
    controller_channel: Channel,
    trust_domain: String,
    connector_id: String,
    renewal_window_secs: u64,
    /// Tunnel hub — routes RDE tunnel messages between device connections and Shields.
    pub tunnel_hub: crate::agent_tunnel::AgentTunnelHub,
}

impl ShieldRegistry {
    pub fn new(
        controller_channel: Channel,
        trust_domain: String,
        connector_id: String,
        ack_tx: mpsc::Sender<(String, ResourceAck)>,
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
            controller_channel,
            trust_domain,
            connector_id,
            renewal_window_secs: DEFAULT_RENEWAL_WINDOW_SECS,
            tunnel_hub: crate::agent_tunnel::AgentTunnelHub::new(),
        }
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
        let reports: Vec<ResourceStateReport> =
            maps.pending_state.drain().map(|(_, report)| report).collect();
        Some(crate::proto::ConnectorControlMessage {
            body: Some(crate::proto::connector_control_message::Body::ResourceState(
                crate::proto::ResourceStateBatch { reports },
            )),
        })
    }

    pub async fn serve(self, addr: SocketAddr, state_dir: impl AsRef<Path>) -> Result<()> {
        let state_dir = state_dir.as_ref();
        let cert_pem = std::fs::read(state_dir.join("connector.crt")).with_context(|| {
            format!(
                "failed to read {}",
                state_dir.join("connector.crt").display()
            )
        })?;
        let key_pem = std::fs::read(state_dir.join("connector.key")).with_context(|| {
            format!(
                "failed to read {}",
                state_dir.join("connector.key").display()
            )
        })?;
        let ca_pem = std::fs::read(state_dir.join("workspace_ca.crt")).with_context(|| {
            format!(
                "failed to read {}",
                state_dir.join("workspace_ca.crt").display()
            )
        })?;

        let tls = ServerTlsConfig::new()
            .identity(Identity::from_pem(cert_pem, key_pem))
            .client_ca_root(Certificate::from_pem(ca_pem))
            .client_auth_optional(false);

        info!(addr = %addr, "starting Shield-facing Connector gRPC server");

        Server::builder()
            .tls_config(tls)
            .context("failed to configure Shield server mTLS")?
            .add_service(ShieldServiceServer::new(self))
            .serve(addr)
            .await
            .context("Shield-facing Connector gRPC server failed")
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
        let (instr_tx, mut instr_rx) = mpsc::channel::<ResourceInstruction>(SHIELD_INSTRUCTION_QUEUE_CAP);
        let (snap_tx, mut snap_rx) = mpsc::channel::<ResourceSnapshot>(8);
        // Tunnel send channel — hub enqueues TunnelOpen/Data/Close messages to deliver to this Shield.
        let (tunnel_tx, mut tunnel_rx) = mpsc::channel::<ShieldControlMessage>(64);

        {
            let mut maps = self.maps.lock();
            maps.instruction_txs
                .insert(identity.shield_id.clone(), instr_tx);
            maps.snapshot_txs.insert(identity.shield_id.clone(), snap_tx);
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

        let mut client = ShieldServiceClient::new(self.controller_channel.clone());
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
        ShieldRegistry::new(
            channel,
            "test.example".to_string(),
            "connector-1".to_string(),
            ack_tx,
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
