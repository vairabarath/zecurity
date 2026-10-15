use super::{ca_cert_url, http_base_url};

#[test]
fn http_base_bare_host_port_defaults_to_http() {
    assert_eq!(http_base_url("127.0.0.1:8080"), "http://127.0.0.1:8080");
    assert_eq!(
        http_base_url("controller.example.com:8080"),
        "http://controller.example.com:8080"
    );
}

#[test]
fn http_base_explicit_scheme_is_preserved() {
    assert_eq!(
        http_base_url("https://controller.zecurity.in"),
        "https://controller.zecurity.in"
    );
    assert_eq!(
        http_base_url("http://controller.zecurity.in:8080"),
        "http://controller.zecurity.in:8080"
    );
}

#[test]
fn http_base_trailing_slash_is_trimmed() {
    assert_eq!(
        http_base_url("https://controller.zecurity.in/"),
        "https://controller.zecurity.in"
    );
}

#[test]
fn bare_host_port_defaults_to_http() {
    assert_eq!(
        ca_cert_url("127.0.0.1:8080"),
        "http://127.0.0.1:8080/ca.crt"
    );
    assert_eq!(
        ca_cert_url("controller.example.com:8080"),
        "http://controller.example.com:8080/ca.crt"
    );
}

#[test]
fn explicit_scheme_is_preserved() {
    assert_eq!(
        ca_cert_url("https://controller.zecurity.in"),
        "https://controller.zecurity.in/ca.crt"
    );
    assert_eq!(
        ca_cert_url("http://controller.zecurity.in:8080"),
        "http://controller.zecurity.in:8080/ca.crt"
    );
}

#[test]
fn trailing_slash_is_trimmed() {
    assert_eq!(
        ca_cert_url("https://controller.zecurity.in/"),
        "https://controller.zecurity.in/ca.crt"
    );
}

// ── Bug 1 (certificate audit 2026-10-15): bootstrap trust ──────────────────
//
// Enrollment fetches /ca.crt over plain HTTP and pins the Platform
// Intermediate fingerprint carried in the token. These tests run a real
// enrollment (`bootstrap_enroll`) against a fake controller: an HTTP server
// for /ca.crt and a gRPC TLS server that records every token it receives.

mod bootstrap {
    use super::super::*;
    use crate::proto::connector_service_server::{ConnectorService, ConnectorServiceServer};
    use crate::test_support::{AttackerPki, TestPki, CONNECTOR_ID, TRUST_DOMAIN};
    use std::sync::{Arc, Mutex};
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tonic::transport::{Identity, Server, ServerTlsConfig};
    use tonic::{Request, Response, Status};

    #[derive(Clone, Copy)]
    enum Reply {
        /// What the real controller returns.
        Genuine,
        /// Mallory's leaf and her root posing as Workspace CA + Intermediate.
        Forged,
        /// Genuine CAs, but a leaf for some other key.
        OtherKey,
    }

    struct FakeController {
        pki: Arc<TestPki>,
        attacker: Arc<AttackerPki>,
        reply: Reply,
        tokens: Arc<Mutex<Vec<String>>>,
    }

    #[tonic::async_trait]
    impl ConnectorService for FakeController {
        async fn enroll(
            &self,
            request: Request<proto::EnrollRequest>,
        ) -> Result<Response<proto::EnrollResponse>, Status> {
            let req = request.into_inner();
            self.tokens
                .lock()
                .unwrap()
                .push(req.enrollment_token.clone());
            let (leaf, ws_ca, int_ca) = match self.reply {
                Reply::Genuine => (
                    self.pki.sign_enrollment_csr(&req.csr_der),
                    self.pki.workspace_ca_pem.clone(),
                    self.pki.intermediate_pem.clone(),
                ),
                Reply::Forged => (
                    self.attacker.sign_csr(&req.csr_der),
                    self.attacker.root_pem.clone(),
                    self.attacker.root_pem.clone(),
                ),
                Reply::OtherKey => (
                    self.pki.leaf_with_new_key(-60, 7 * 24 * 3600),
                    self.pki.workspace_ca_pem.clone(),
                    self.pki.intermediate_pem.clone(),
                ),
            };
            Ok(Response::new(proto::EnrollResponse {
                certificate_pem: leaf.into_bytes(),
                workspace_ca_pem: ws_ca.into_bytes(),
                intermediate_ca_pem: int_ca.into_bytes(),
                connector_id: CONNECTOR_ID.to_string(),
            }))
        }

        type ControlStream = std::pin::Pin<
            Box<
                dyn tokio_stream::Stream<Item = Result<proto::ConnectorControlMessage, Status>>
                    + Send,
            >,
        >;

        async fn control(
            &self,
            _request: Request<tonic::Streaming<proto::ConnectorControlMessage>>,
        ) -> Result<Response<Self::ControlStream>, Status> {
            Err(Status::unimplemented("not used in enrollment tests"))
        }

        async fn renew_cert(
            &self,
            _request: Request<proto::RenewCertRequest>,
        ) -> Result<Response<proto::RenewCertResponse>, Status> {
            Err(Status::unimplemented("not used in enrollment tests"))
        }

        async fn goodbye(
            &self,
            _request: Request<proto::GoodbyeRequest>,
        ) -> Result<Response<proto::GoodbyeResponse>, Status> {
            Err(Status::unimplemented("not used in enrollment tests"))
        }
    }

    /// Serves `body` for every HTTP request (stands in for GET /ca.crt).
    async fn spawn_ca_http(body: String) -> std::net::SocketAddr {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        tokio::spawn(async move {
            while let Ok((mut sock, _)) = listener.accept().await {
                let body = body.clone();
                tokio::spawn(async move {
                    let mut buf = [0u8; 2048];
                    let _ = sock.read(&mut buf).await;
                    let head = format!(
                        "HTTP/1.1 200 OK\r\nContent-Type: application/x-pem-file\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                        body.len()
                    );
                    let _ = sock.write_all(head.as_bytes()).await;
                    let _ = sock.write_all(body.as_bytes()).await;
                });
            }
        });
        addr
    }

    /// Starts the fake controller's gRPC TLS server with `identity`.
    async fn spawn_grpc(identity: (String, String), controller: FakeController) -> u16 {
        let probe = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        let addr = probe.local_addr().unwrap();
        drop(probe);
        tokio::spawn(async move {
            Server::builder()
                .tls_config(
                    ServerTlsConfig::new().identity(Identity::from_pem(identity.0, identity.1)),
                )
                .unwrap()
                .add_service(ConnectorServiceServer::new(controller))
                .serve(addr)
                .await
                .unwrap();
        });
        for _ in 0..200 {
            if tokio::net::TcpStream::connect(addr).await.is_ok() {
                break;
            }
            tokio::time::sleep(std::time::Duration::from_millis(10)).await;
        }
        addr.port()
    }

    fn fingerprint(pem: &str) -> String {
        let der = certs(&mut pem.as_bytes()).next().unwrap().unwrap();
        hex::encode(Sha256::digest(der.as_ref()))
    }

    /// An enrollment token as the controller mints it (the connector only
    /// decodes the payload; the controller verifies the signature).
    fn token(pki: &TestPki) -> String {
        let payload = serde_json::json!({
            "jti": "test-jti",
            "connector_id": CONNECTOR_ID,
            "workspace_id": "ws-test",
            "trust_domain": TRUST_DOMAIN,
            "ca_fingerprint": fingerprint(&pki.intermediate_pem),
        });
        format!(
            "{}.{}.sig",
            URL_SAFE_NO_PAD.encode(br#"{"alg":"HS256","typ":"JWT"}"#),
            URL_SAFE_NO_PAD.encode(payload.to_string())
        )
    }

    struct Scenario {
        pki: Arc<TestPki>,
        attacker: Arc<AttackerPki>,
        dir: tempfile::TempDir,
        tokens: Arc<Mutex<Vec<String>>>,
    }

    impl Scenario {
        fn new() -> Self {
            Self {
                pki: Arc::new(TestPki::new()),
                attacker: Arc::new(AttackerPki::new()),
                dir: tempfile::tempdir().unwrap(),
                tokens: Arc::new(Mutex::new(Vec::new())),
            }
        }

        /// Run one enrollment: `/ca.crt` serves `ca_body`, the gRPC server
        /// presents `identity` and answers with `reply`.
        async fn enroll(
            &self,
            ca_body: String,
            identity: (String, String),
            reply: Reply,
        ) -> Result<EnrollmentState> {
            let http = spawn_ca_http(ca_body).await;
            let grpc = spawn_grpc(
                identity,
                FakeController {
                    pki: self.pki.clone(),
                    attacker: self.attacker.clone(),
                    reply,
                    tokens: self.tokens.clone(),
                },
            )
            .await;
            let cfg: ConnectorConfig = serde_json::from_value(serde_json::json!({
                "controller_addr": format!("localhost:{grpc}"),
                "controller_http_addr": http.to_string(),
                "enrollment_token": token(&self.pki),
                "state_dir": self.dir.path().to_str().unwrap(),
            }))
            .unwrap();
            bootstrap_enroll(&cfg).await
        }

        fn tokens_received(&self) -> usize {
            self.tokens.lock().unwrap().len()
        }

        /// Files left in the state directory.
        fn files(&self) -> Vec<String> {
            let mut names: Vec<String> = fs::read_dir(self.dir.path())
                .unwrap()
                .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
                .collect();
            names.sort();
            names
        }
    }

    /// Attack (Act 3/4a): Mallory appends her root to /ca.crt. The pin
    /// matches the first certificate, so today the whole file is trusted and
    /// her TLS server receives the enrollment token.
    #[tokio::test]
    #[ignore = "bug 1: demonstrates the bootstrap hijack; un-ignore with the fix"]
    async fn appended_ca_cannot_intercept_the_enrollment_token() {
        let s = Scenario::new();
        let tampered = format!("{}\n{}", s.pki.intermediate_pem, s.attacker.root_pem);

        let result = s
            .enroll(tampered, s.attacker.server_identity(), Reply::Genuine)
            .await;

        assert_eq!(
            s.tokens_received(),
            0,
            "the enrollment token reached Mallory's server ({result:?})"
        );
        assert!(result.is_err(), "enrollment must fail");
        assert!(
            s.files().is_empty(),
            "nothing may be persisted: {:?}",
            s.files()
        );
    }

    /// Attack (Act 4b): the reply carries Mallory's root as the Workspace CA
    /// and Intermediate. Today it is saved as the connector's trust store.
    #[tokio::test]
    #[ignore = "bug 1: demonstrates the bootstrap hijack; un-ignore with the fix"]
    async fn forged_enrollment_reply_is_not_trusted_or_persisted() {
        let s = Scenario::new();
        let result = s
            .enroll(
                s.pki.intermediate_pem.clone(),
                s.pki.controller_server_identity(),
                Reply::Forged,
            )
            .await;

        assert!(result.is_err(), "a forged reply must be refused");
        assert!(
            s.files().is_empty(),
            "nothing may be persisted: {:?}",
            s.files()
        );
    }

    /// A reply whose leaf is for another key (genuine CAs) must be refused.
    #[tokio::test]
    #[ignore = "bug 1: demonstrates the bootstrap hijack; un-ignore with the fix"]
    async fn reply_for_another_key_is_refused_and_nothing_persisted() {
        let s = Scenario::new();
        let result = s
            .enroll(
                s.pki.intermediate_pem.clone(),
                s.pki.controller_server_identity(),
                Reply::OtherKey,
            )
            .await;

        assert!(result.is_err(), "a leaf for another key must be refused");
        assert!(
            s.files().is_empty(),
            "nothing may be persisted: {:?}",
            s.files()
        );
    }

    /// A failed attempt leaves nothing behind, so a retry with a genuine
    /// controller enrolls cleanly.
    #[tokio::test]
    #[ignore = "bug 1: demonstrates the bootstrap hijack; un-ignore with the fix"]
    async fn failed_attempt_leaves_nothing_and_retry_succeeds() {
        let s = Scenario::new();
        let first = s
            .enroll(
                s.pki.intermediate_pem.clone(),
                s.pki.controller_server_identity(),
                Reply::Forged,
            )
            .await;
        assert!(first.is_err(), "forged first attempt must fail");
        assert!(s.files().is_empty(), "failed attempt left {:?}", s.files());

        let second = s
            .enroll(
                s.pki.intermediate_pem.clone(),
                s.pki.controller_server_identity(),
                Reply::Genuine,
            )
            .await;
        assert!(second.is_ok(), "retry must succeed: {second:?}");
    }

    /// Baseline: a /ca.crt that is not the pinned certificate is refused.
    #[tokio::test]
    async fn wrong_ca_is_refused_before_connecting() {
        let s = Scenario::new();
        let result = s
            .enroll(
                s.attacker.root_pem.clone(),
                s.attacker.server_identity(),
                Reply::Genuine,
            )
            .await;
        assert!(result.is_err());
        assert_eq!(s.tokens_received(), 0);
    }

    /// Positive: a genuine enrollment installs the controller's CAs and a
    /// leaf for the connector's own key, and renewal then works against
    /// those pinned anchors.
    #[tokio::test]
    async fn genuine_enrollment_installs_trust_that_renewal_reuses() {
        let s = Scenario::new();
        let state = s
            .enroll(
                s.pki.intermediate_pem.clone(),
                s.pki.controller_server_identity(),
                Reply::Genuine,
            )
            .await
            .expect("genuine enrollment");
        assert_eq!(state.connector_id, CONNECTOR_ID);
        assert_eq!(s.tokens_received(), 1);

        let dir = s.dir.path().to_str().unwrap();
        let ca_bundle = fs::read_to_string(s.dir.path().join("workspace_ca.crt")).unwrap();
        assert_eq!(
            fingerprint(&ca_bundle),
            fingerprint(&s.pki.workspace_ca_pem)
        );
        assert!(ca_bundle.contains(s.pki.intermediate_pem.trim()));

        // The leaf is for the connector's own key: CertHolder::load verifies it.
        let holder = crate::tls::cert_holder::CertHolder::load(dir, &s.pki.spiffe_id)
            .expect("enrolled material loads");

        // Renewal reuses the pinned anchors: a same-key renewal from the
        // genuine CAs installs.
        let key = crypto::load_private_key(&s.dir.path().join("connector.key")).unwrap();
        let csr = crypto::build_csr(&key, "connector", &s.pki.spiffe_id).unwrap();
        let renewed = s.pki.sign_enrollment_csr(&csr);
        holder
            .install_renewed(
                renewed.as_bytes(),
                s.pki.workspace_ca_pem.as_bytes(),
                s.pki.intermediate_pem.as_bytes(),
            )
            .expect("renewal against the pinned anchors");
    }
}
