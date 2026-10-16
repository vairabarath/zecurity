//! Test-only fake controller for the bootstrap-trust tests (certificate
//! audit bug 1, client part): a plain-HTTP `/ca.crt` server and a TLS gRPC
//! `ClientService` that records every call it receives and answers
//! EnrollDevice / RenewCert by signing the client's CSR per [`Reply`].

use std::sync::{Arc, Mutex};
use std::time::Duration;

use tokio::io::{AsyncReadExt, AsyncWriteExt};
use tonic::transport::{Identity, Server, ServerTlsConfig};
use tonic::{Request, Response, Status};

use crate::grpc::client_v1::client_service_server::{ClientService, ClientServiceServer};
use crate::grpc::client_v1::*;
use crate::test_pki::{AttackerPki, TestPki, DEVICE_ID, TRUST_DOMAIN};

/// How the fake answers EnrollDevice and RenewCert.
#[derive(Clone, Copy)]
pub enum Reply {
    /// As the real controller: the platform's chain, the client's own key.
    Genuine,
    /// Mallory's own chain (her Workspace CA under a look-alike
    /// intermediate) for the client's key.
    Forged,
    /// Another platform's complete, self-consistent chain (same names).
    OtherPlatform,
    /// The platform's chain, but a leaf for a different key.
    OtherKey,
}

/// Everything the fake received, in order: `"<Rpc> <token>"`.
pub type CallLog = Arc<Mutex<Vec<String>>>;

pub struct FakeController {
    pub pki: Arc<TestPki>,
    pub attacker: Arc<AttackerPki>,
    pub other: Arc<TestPki>,
    pub reply: Reply,
    pub calls: CallLog,
}

impl FakeController {
    fn record(&self, rpc: &str, token: &str) {
        self.calls.lock().unwrap().push(format!("{rpc} {token}"));
    }

    /// (leaf, Workspace CA, Intermediate) for `csr_pem` per `self.reply`.
    fn issue(&self, csr_pem: &str) -> (String, String, String) {
        match self.reply {
            Reply::Genuine => (
                self.pki.sign_device_csr(csr_pem, DEVICE_ID),
                self.pki.workspace_ca_pem.clone(),
                self.pki.intermediate_pem.clone(),
            ),
            Reply::Forged => (
                self.attacker.sign_device_csr(csr_pem, DEVICE_ID),
                self.attacker.workspace_ca_pem.clone(),
                self.attacker.fake_intermediate_pem.clone(),
            ),
            Reply::OtherPlatform => (
                self.other.sign_device_csr(csr_pem, DEVICE_ID),
                self.other.workspace_ca_pem.clone(),
                self.other.intermediate_pem.clone(),
            ),
            Reply::OtherKey => (
                self.pki
                    .sign_device_csr(&crate::test_pki::other_key_csr(), DEVICE_ID),
                self.pki.workspace_ca_pem.clone(),
                self.pki.intermediate_pem.clone(),
            ),
        }
    }
}

#[tonic::async_trait]
impl ClientService for FakeController {
    async fn get_auth_config(
        &self,
        _r: Request<GetAuthConfigRequest>,
    ) -> Result<Response<GetAuthConfigResponse>, Status> {
        self.record("GetAuthConfig", "");
        Err(Status::unimplemented("not used"))
    }

    async fn initiate_auth(
        &self,
        r: Request<InitiateAuthRequest>,
    ) -> Result<Response<InitiateAuthResponse>, Status> {
        let r = r.into_inner();
        self.record("InitiateAuth", &r.code_challenge);
        Ok(Response::new(InitiateAuthResponse {
            auth_url: "https://accounts.example.test/auth".into(),
            session_id: "session-1".into(),
        }))
    }

    async fn token_exchange(
        &self,
        r: Request<TokenExchangeRequest>,
    ) -> Result<Response<TokenExchangeResponse>, Status> {
        let r = r.into_inner();
        self.record("TokenExchange", &r.invite_token);
        Ok(Response::new(TokenExchangeResponse::default()))
    }

    async fn enroll_device(
        &self,
        r: Request<EnrollDeviceRequest>,
    ) -> Result<Response<EnrollDeviceResponse>, Status> {
        let r = r.into_inner();
        self.record("EnrollDevice", &r.access_token);
        let (certificate_pem, workspace_ca_pem, intermediate_ca_pem) = self.issue(&r.csr_pem);
        Ok(Response::new(EnrollDeviceResponse {
            certificate_pem,
            workspace_ca_pem,
            intermediate_ca_pem,
            spiffe_id: format!("spiffe://{TRUST_DOMAIN}/client/{DEVICE_ID}"),
            device_id: DEVICE_ID.into(),
        }))
    }

    async fn get_acl_snapshot(
        &self,
        r: Request<GetAclSnapshotRequest>,
    ) -> Result<Response<GetAclSnapshotResponse>, Status> {
        self.record("GetAclSnapshot", &r.into_inner().access_token);
        Err(Status::unimplemented("not used"))
    }

    async fn get_transport_snapshot(
        &self,
        r: Request<GetTransportSnapshotRequest>,
    ) -> Result<Response<GetTransportSnapshotResponse>, Status> {
        self.record("GetTransportSnapshot", &r.into_inner().access_token);
        Err(Status::unimplemented("not used"))
    }

    async fn revoke_device(
        &self,
        r: Request<RevokeDeviceRequest>,
    ) -> Result<Response<RevokeDeviceResponse>, Status> {
        self.record("RevokeDevice", &r.into_inner().access_token);
        Ok(Response::new(RevokeDeviceResponse {}))
    }

    async fn report_device_posture(
        &self,
        r: Request<ReportDevicePostureRequest>,
    ) -> Result<Response<ReportDevicePostureResponse>, Status> {
        self.record("ReportDevicePosture", &r.into_inner().access_token);
        Err(Status::unimplemented("not used"))
    }

    async fn renew_cert(
        &self,
        r: Request<RenewCertRequest>,
    ) -> Result<Response<RenewCertResponse>, Status> {
        let r = r.into_inner();
        self.record("RenewCert", &r.access_token);
        let (certificate_pem, workspace_ca_pem, intermediate_ca_pem) = self.issue(&r.csr_pem);
        Ok(Response::new(RenewCertResponse {
            certificate_pem,
            workspace_ca_pem,
            intermediate_ca_pem,
            cert_expires_at: 9_999_999_999,
        }))
    }
}

/// Serve `body` as the response to any HTTP request (the `/ca.crt` stand-in).
pub async fn spawn_ca_http(body: String) -> std::net::SocketAddr {
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        while let Ok((mut sock, _)) = listener.accept().await {
            let body = body.clone();
            tokio::spawn(async move {
                let mut buf = [0u8; 2048];
                let _ = sock.read(&mut buf).await;
                let head = format!(
                    "HTTP/1.1 200 OK\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                    body.len()
                );
                let _ = sock.write_all(head.as_bytes()).await;
                let _ = sock.write_all(body.as_bytes()).await;
            });
        }
    });
    addr
}

/// Serve `controller` over TLS with `identity` (cert PEM, key PEM); returns
/// the port once it accepts connections.
pub async fn spawn_grpc(identity: (String, String), controller: FakeController) -> u16 {
    let probe = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
    let addr = probe.local_addr().unwrap();
    drop(probe);
    tokio::spawn(async move {
        Server::builder()
            .tls_config(ServerTlsConfig::new().identity(Identity::from_pem(identity.0, identity.1)))
            .unwrap()
            .add_service(ClientServiceServer::new(controller))
            .serve(addr)
            .await
            .unwrap();
    });
    for _ in 0..200 {
        if tokio::net::TcpStream::connect(addr).await.is_ok() {
            break;
        }
        tokio::time::sleep(Duration::from_millis(10)).await;
    }
    addr.port()
}

/// A controller (or an impostor) reachable by the client: `/ca.crt` serves
/// `ca_body`, gRPC presents `identity` and answers per `reply`.
pub struct Endpoint {
    pub conf: crate::config::ClientConf,
    pub calls: CallLog,
}

pub async fn endpoint(
    pki: &Arc<TestPki>,
    attacker: &Arc<AttackerPki>,
    ca_body: String,
    identity: (String, String),
    reply: Reply,
) -> Endpoint {
    let calls: CallLog = Arc::default();
    let http = spawn_ca_http(ca_body).await;
    let port = spawn_grpc(
        identity,
        FakeController {
            pki: pki.clone(),
            attacker: attacker.clone(),
            other: Arc::new(TestPki::new()),
            reply,
            calls: calls.clone(),
        },
    )
    .await;
    Endpoint {
        conf: crate::config::ClientConf {
            workspace: "ws-test".into(),
            controller_address: format!("localhost:{port}"),
            http_base_url: format!("http://{http}"),
            ..Default::default()
        },
        calls,
    }
}
