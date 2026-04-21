use std::collections::HashMap;
use std::net::SocketAddr;
use std::path::Path;
use std::sync::{Arc, Mutex};
use std::time::{SystemTime, UNIX_EPOCH};

use anyhow::{Context, Result};
use ::time::OffsetDateTime;
use tonic::transport::{Certificate, Channel, Identity, Server, ServerTlsConfig};
use tonic::{Request, Response, Status};
use tracing::{info, warn};
use x509_parser::prelude::*;

use crate::proto::ShieldHealth;
use crate::shield_proto::shield_service_client::ShieldServiceClient;
use crate::shield_proto::shield_service_server::{ShieldService, ShieldServiceServer};
use crate::shield_proto::{
    EnrollRequest, EnrollResponse, GoodbyeRequest, GoodbyeResponse, HeartbeatRequest,
    HeartbeatResponse, RenewCertRequest, RenewCertResponse, ResourceAck, ResourceInstruction,
};

const DEFAULT_RENEWAL_WINDOW_SECS: u64 = 48 * 60 * 60;

#[derive(Debug, Clone)]
struct ShieldEntry {
    status: String,
    version: String,
    last_seen_unix: i64,
    cert_not_after_unix: i64,
    lan_ip: String,
}

#[derive(Debug, Clone)]
pub struct ShieldServer {
    shields: Arc<Mutex<HashMap<String, ShieldEntry>>>,
    // resource_instructions: keyed by shield_id → instructions from Controller heartbeat
    resource_instructions: Arc<Mutex<HashMap<String, Vec<ResourceInstruction>>>>,
    // pending_acks: collected from shield heartbeats, drained on connector heartbeat
    pending_acks: Arc<Mutex<Vec<ResourceAck>>>,
    controller_channel: Channel,
    trust_domain: String,
    connector_id: String,
    renewal_window_secs: u64,
}

impl ShieldServer {
    pub fn new(controller_channel: Channel, trust_domain: String, connector_id: String) -> Self {
        Self {
            shields: Arc::new(Mutex::new(HashMap::new())),
            resource_instructions: Arc::new(Mutex::new(HashMap::new())),
            pending_acks: Arc::new(Mutex::new(Vec::new())),
            controller_channel,
            trust_domain,
            connector_id,
            renewal_window_secs: DEFAULT_RENEWAL_WINDOW_SECS,
        }
    }

    pub fn get_alive_shields(&self) -> Vec<ShieldHealth> {
        self.shields
            .lock()
            .expect("shield health map mutex poisoned")
            .iter()
            .map(|(id, entry)| ShieldHealth {
                shield_id: id.clone(),
                status: entry.status.clone(),
                version: entry.version.clone(),
                last_heartbeat_at: entry.last_seen_unix,
                lan_ip: entry.lan_ip.clone(),
            })
            .collect()
    }

    /// Called by connector heartbeat after receiving HeartbeatResponse from Controller.
    /// Updates the cached resource instructions for the given shield.
    pub fn update_resource_instructions(&self, shield_id: &str, instructions: Vec<ResourceInstruction>) {
        self.resource_instructions
            .lock()
            .expect("resource instructions mutex poisoned")
            .insert(shield_id.to_string(), instructions);
    }

    /// Called by connector heartbeat to collect and clear all pending shield acks.
    pub fn drain_resource_acks(&self) -> Vec<ResourceAck> {
        let mut acks = self.pending_acks
            .lock()
            .expect("pending acks mutex poisoned");
        std::mem::take(&mut *acks)
    }

    pub async fn serve(self, addr: SocketAddr, state_dir: impl AsRef<Path>) -> Result<()> {
        let state_dir = state_dir.as_ref();
        let cert_pem = std::fs::read(state_dir.join("connector.crt"))
            .with_context(|| format!("failed to read {}", state_dir.join("connector.crt").display()))?;
        let key_pem = std::fs::read(state_dir.join("connector.key"))
            .with_context(|| format!("failed to read {}", state_dir.join("connector.key").display()))?;
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
impl ShieldService for ShieldServer {
    async fn enroll(
        &self,
        _request: Request<EnrollRequest>,
    ) -> Result<Response<EnrollResponse>, Status> {
        Err(Status::unimplemented(
            "Shield enrolls directly with Controller, not through Connector",
        ))
    }

    async fn heartbeat(
        &self,
        request: Request<HeartbeatRequest>,
    ) -> Result<Response<HeartbeatResponse>, Status> {
        let verified = self.verify_shield_identity(&request, &request.get_ref().shield_id)?;
        let req = request.into_inner();
        let now = unix_now();

        {
            let mut shields = self
                .shields
                .lock()
                .map_err(|_| Status::internal("shield health map mutex poisoned"))?;
            shields.insert(
                verified.shield_id.clone(),
                ShieldEntry {
                    status: "active".to_string(),
                    version: req.version.clone(),
                    last_seen_unix: now,
                    cert_not_after_unix: verified.cert_not_after_unix,
                    lan_ip: req.lan_ip.clone(),
                },
            );
        }

        // Collect ResourceAcks from this shield into pending_acks for next connector heartbeat.
        if !req.resource_acks.is_empty() {
            let mut acks = self.pending_acks
                .lock()
                .map_err(|_| Status::internal("pending acks mutex poisoned"))?;
            acks.extend(req.resource_acks);
        }

        // Retrieve cached resource instructions for this shield.
        let resources = self.resource_instructions
            .lock()
            .map_err(|_| Status::internal("resource instructions mutex poisoned"))?
            .get(&verified.shield_id)
            .cloned()
            .unwrap_or_default();

        info!(
            shield_id = %verified.shield_id,
            version = %req.version,
            hostname = %req.hostname,
            public_ip = %req.public_ip,
            lan_ip = %req.lan_ip,
            pending_instructions = resources.len(),
            cert_not_after_unix = verified.cert_not_after_unix,
            "shield heartbeat received"
        );

        Ok(Response::new(HeartbeatResponse {
            ok: true,
            latest_version: String::new(),
            re_enroll: self.cert_needs_renewal(verified.cert_not_after_unix),
            resources,
        }))
    }

    async fn renew_cert(
        &self,
        request: Request<RenewCertRequest>,
    ) -> Result<Response<RenewCertResponse>, Status> {
        let verified = self.verify_shield_identity(&request, &request.get_ref().shield_id)?;
        let req = request.into_inner();

        info!(shield_id = %verified.shield_id, "proxying shield cert renewal to controller");

        let mut client = ShieldServiceClient::new(self.controller_channel.clone());
        client
            .renew_cert(Request::new(req))
            .await
            .map_err(|err| {
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
            .shields
            .lock()
            .map_err(|_| Status::internal("shield health map mutex poisoned"))?
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
