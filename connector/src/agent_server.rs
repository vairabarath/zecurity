use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::Path;
use std::sync::{Arc, Mutex};
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
    RenewCertRequest, RenewCertResponse, ResourceAck, ResourceInstruction, ShieldControlMessage,
};

const DEFAULT_RENEWAL_WINDOW_SECS: u64 = 48 * 60 * 60;
const SHIELD_STALE_THRESHOLD_SECS: i64 = 90;

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
    // Stream-connected shields: shield_id → instruction sender
    instruction_txs: Arc<Mutex<HashMap<String, mpsc::Sender<ResourceInstruction>>>>,
    // Buffered instructions for shields that reconnect after the connector already received work.
    resource_instructions: Arc<Mutex<HashMap<String, Vec<ResourceInstruction>>>>,
    // Unified ack sink — consumed by control_stream.rs which forwards to controller
    pub ack_tx: mpsc::Sender<(String, ResourceAck)>,
    health: Arc<Mutex<HashMap<String, ShieldEntry>>>,
    controller_channel: Channel,
    trust_domain: String,
    connector_id: String,
    renewal_window_secs: u64,
}

impl ShieldRegistry {
    pub fn new(
        controller_channel: Channel,
        trust_domain: String,
        connector_id: String,
        ack_tx: mpsc::Sender<(String, ResourceAck)>,
    ) -> Self {
        Self {
            instruction_txs: Arc::new(Mutex::new(HashMap::new())),
            resource_instructions: Arc::new(Mutex::new(HashMap::new())),
            ack_tx,
            health: Arc::new(Mutex::new(HashMap::new())),
            controller_channel,
            trust_domain,
            connector_id,
            renewal_window_secs: DEFAULT_RENEWAL_WINDOW_SECS,
        }
    }

    /// Deliver instructions to a shield via Control stream, or buffer until the shield reconnects.
    pub fn push_instructions(&self, shield_id: &str, instructions: Vec<ResourceInstruction>) {
        if instructions.is_empty() {
            return;
        }
        let maybe_tx = self
            .instruction_txs
            .lock()
            .expect("instruction_txs poisoned")
            .get(shield_id)
            .cloned();

        if let Some(tx) = maybe_tx {
            let id = shield_id.to_string();
            tokio::spawn(async move {
                for instr in instructions {
                    if tx.send(instr).await.is_err() {
                        warn!(shield_id = %id, "shield instruction channel closed during push");
                        break;
                    }
                }
            });
        } else {
            self.resource_instructions
                .lock()
                .expect("resource_instructions poisoned")
                .insert(shield_id.to_string(), instructions);
        }
    }

    /// Snapshot of alive shields for the health report sent to controller.
    pub fn get_shield_status_batch(&self) -> crate::proto::ShieldStatusBatch {
        let cutoff = unix_now() - SHIELD_STALE_THRESHOLD_SECS;
        let shields = self
            .health
            .lock()
            .expect("health map poisoned")
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
        let (instr_tx, mut instr_rx) = mpsc::channel::<ResourceInstruction>(32);

        {
            self.instruction_txs
                .lock()
                .expect("instruction_txs poisoned")
                .insert(identity.shield_id.clone(), instr_tx);
        }

        let registry = self.clone();
        let shield_id = identity.shield_id.clone();
        let cert_not_after = identity.cert_not_after_unix;

        tokio::spawn(async move {
            info!(shield_id = %shield_id, "shield Control stream connected");

            let buffered = registry
                .resource_instructions
                .lock()
                .expect("resource_instructions poisoned")
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
                                        registry.health
                                            .lock()
                                            .expect("health map poisoned")
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
                                    Some(Body::Pong(_)) => {}
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
                }
            }
            registry
                .instruction_txs
                .lock()
                .expect("instruction_txs poisoned")
                .remove(&shield_id);
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
            .health
            .lock()
            .map_err(|_| Status::internal("health map mutex poisoned"))?
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
