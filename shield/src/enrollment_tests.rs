use super::ca_cert_url;

#[test]
fn bare_host_port_defaults_to_http() {
    assert_eq!(ca_cert_url("127.0.0.1:8080"), "http://127.0.0.1:8080/ca.crt");
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
// A real enrollment (`bootstrap_enroll`) against a fake controller: an HTTP
// server for /ca.crt and a gRPC TLS server (test-only server copy of the
// shield proto) that records every token it receives.

mod bootstrap {
    use super::super::*;
    use rcgen::{
        BasicConstraints, CertificateParams, CertificateSigningRequestParams, DnType,
        ExtendedKeyUsagePurpose, IsCa, Issuer, KeyPair, KeyUsagePurpose, SanType,
        PKCS_ECDSA_P384_SHA384,
    };
    use sha2::{Digest, Sha256};
    use std::sync::{Arc, Mutex};
    use tokio::io::{AsyncReadExt, AsyncWriteExt};
    use tonic::transport::{Identity, Server, ServerTlsConfig};
    use tonic::{Request, Response, Status};

    #[allow(clippy::all, dead_code)]
    mod fake {
        include!(concat!(env!("OUT_DIR"), "/test_server/shield.v1.rs"));
    }
    use fake::shield_service_server::{ShieldService, ShieldServiceServer};

    const SHIELD_ID: &str = "5c1d2e3f-4a5b-4c6d-8e7f-9a0b1c2d3e4f";
    const TRUST_DOMAIN: &str = "ws-test.zecurity.in";

    fn new_key() -> KeyPair {
        KeyPair::generate_for(&PKCS_ECDSA_P384_SHA384).unwrap()
    }

    fn ca_params(cn: &str) -> CertificateParams {
        let mut p = CertificateParams::default();
        p.distinguished_name.push(DnType::CommonName, cn);
        p.is_ca = IsCa::Ca(BasicConstraints::Unconstrained);
        p.key_usages = vec![KeyUsagePurpose::KeyCertSign, KeyUsagePurpose::CrlSign];
        p
    }

    /// Controller-shaped PKI: Platform Intermediate → Workspace CA → leaves,
    /// plus Mallory's unrelated root.
    struct Pki {
        int_params: CertificateParams,
        int_key: KeyPair,
        ws_params: CertificateParams,
        ws_key: KeyPair,
        intermediate_pem: String,
        workspace_ca_pem: String,
        attacker_params: CertificateParams,
        attacker_key: KeyPair,
        attacker_pem: String,
    }

    impl Pki {
        fn new() -> Self {
            let _ = rustls::crypto::ring::default_provider().install_default();
            let int_key = new_key();
            let int_params = ca_params("Test Platform Intermediate");
            let intermediate_pem = int_params.self_signed(&int_key).unwrap().pem();
            let ws_key = new_key();
            let mut ws_params = ca_params("Test Workspace CA");
            ws_params.subject_alt_names = vec![SanType::URI("tenant:ws-test".try_into().unwrap())];
            let workspace_ca_pem = ws_params
                .signed_by(&ws_key, &Issuer::from_params(&int_params, &int_key))
                .unwrap()
                .pem();
            let attacker_key = new_key();
            let attacker_params = ca_params("Mallory Root");
            let attacker_pem = attacker_params.self_signed(&attacker_key).unwrap().pem();
            Self {
                int_params,
                int_key,
                ws_params,
                ws_key,
                intermediate_pem,
                workspace_ca_pem,
                attacker_params,
                attacker_key,
                attacker_pem,
            }
        }

        fn server_identity(issuer: &Issuer<'_, &KeyPair>) -> (String, String) {
            let key = new_key();
            let mut p = CertificateParams::default();
            p.subject_alt_names = vec![SanType::DnsName("localhost".try_into().unwrap())];
            p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
            let cert = p.signed_by(&key, issuer).unwrap();
            (cert.pem(), key.serialize_pem())
        }

        fn controller_identity(&self) -> (String, String) {
            Self::server_identity(&Issuer::from_params(&self.int_params, &self.int_key))
        }

        fn attacker_identity(&self) -> (String, String) {
            Self::server_identity(&Issuer::from_params(
                &self.attacker_params,
                &self.attacker_key,
            ))
        }

        fn sign_csr(csr_der: &[u8], issuer: &Issuer<'_, &KeyPair>) -> String {
            let csr = rustls::pki_types::CertificateSigningRequestDer::from(csr_der.to_vec());
            let mut p = CertificateSigningRequestParams::from_der(&csr).unwrap();
            p.params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
            p.params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
            p.signed_by(issuer).unwrap().pem()
        }

        fn genuine_leaf(&self, csr_der: &[u8]) -> String {
            Self::sign_csr(csr_der, &Issuer::from_params(&self.ws_params, &self.ws_key))
        }

        fn attacker_leaf(&self, csr_der: &[u8]) -> String {
            Self::sign_csr(
                csr_der,
                &Issuer::from_params(&self.attacker_params, &self.attacker_key),
            )
        }

        /// A genuine leaf for `csr_der` whose parameters `shape` adjusts
        /// (usages, validity …) — for the wrong-shape rejection tests.
        fn genuine_leaf_shaped(
            &self,
            csr_der: &[u8],
            shape: impl FnOnce(&mut CertificateParams),
        ) -> String {
            let csr = rustls::pki_types::CertificateSigningRequestDer::from(csr_der.to_vec());
            let mut p = CertificateSigningRequestParams::from_der(&csr).unwrap();
            p.params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
            p.params.key_usages = vec![KeyUsagePurpose::DigitalSignature];
            shape(&mut p.params);
            p.signed_by(&Issuer::from_params(&self.ws_params, &self.ws_key))
                .unwrap()
                .pem()
        }

        /// A "Workspace CA" that is not a CA (issued by the real Intermediate)
        /// and a leaf for `csr_der` signed by it. Returns (ws PEM, leaf PEM).
        fn non_ca_workspace(&self, csr_der: &[u8]) -> (String, String) {
            let key = new_key();
            let mut params = CertificateParams::default();
            params
                .distinguished_name
                .push(DnType::CommonName, "Test Workspace CA");
            let ws_pem = params
                .signed_by(&key, &Issuer::from_params(&self.int_params, &self.int_key))
                .unwrap()
                .pem();
            let csr = rustls::pki_types::CertificateSigningRequestDer::from(csr_der.to_vec());
            let mut p = CertificateSigningRequestParams::from_der(&csr).unwrap();
            p.params.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
            let leaf = p
                .signed_by(&Issuer::from_params(&params, &key))
                .unwrap()
                .pem();
            (ws_pem, leaf)
        }

        fn other_key_leaf(&self) -> String {
            let mut p = CertificateParams::default();
            p.subject_alt_names = vec![SanType::URI(
                appmeta::shield_spiffe_id(TRUST_DOMAIN, SHIELD_ID)
                    .as_str()
                    .try_into()
                    .unwrap(),
            )];
            p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ClientAuth];
            p.signed_by(
                &new_key(),
                &Issuer::from_params(&self.ws_params, &self.ws_key),
            )
            .unwrap()
            .pem()
        }
    }

    #[derive(Clone, Copy)]
    enum Reply {
        Genuine,
        Forged,
        OtherKey,
    }

    struct FakeController {
        pki: Arc<Pki>,
        reply: Reply,
        tokens: Arc<Mutex<Vec<String>>>,
    }

    #[tonic::async_trait]
    impl ShieldService for FakeController {
        async fn enroll(
            &self,
            request: Request<fake::EnrollRequest>,
        ) -> Result<Response<fake::EnrollResponse>, Status> {
            let req = request.into_inner();
            self.tokens
                .lock()
                .unwrap()
                .push(req.enrollment_token.clone());
            let (leaf, ws_ca, int_ca) = match self.reply {
                Reply::Genuine => (
                    self.pki.genuine_leaf(&req.csr_der),
                    self.pki.workspace_ca_pem.clone(),
                    self.pki.intermediate_pem.clone(),
                ),
                Reply::Forged => (
                    self.pki.attacker_leaf(&req.csr_der),
                    self.pki.attacker_pem.clone(),
                    self.pki.attacker_pem.clone(),
                ),
                Reply::OtherKey => (
                    self.pki.other_key_leaf(),
                    self.pki.workspace_ca_pem.clone(),
                    self.pki.intermediate_pem.clone(),
                ),
            };
            Ok(Response::new(fake::EnrollResponse {
                certificate_pem: leaf.into_bytes(),
                workspace_ca_pem: ws_ca.into_bytes(),
                intermediate_ca_pem: int_ca.into_bytes(),
                shield_id: SHIELD_ID.to_string(),
                interface_addr: "100.64.0.5".to_string(),
                connector_addr: "127.0.0.1:9091".to_string(),
                connector_id: "conn-1".to_string(),
            }))
        }

        type ControlStream = std::pin::Pin<
            Box<dyn tokio_stream::Stream<Item = Result<fake::ShieldControlMessage, Status>> + Send>,
        >;

        async fn control(
            &self,
            _request: Request<tonic::Streaming<fake::ShieldControlMessage>>,
        ) -> Result<Response<Self::ControlStream>, Status> {
            Err(Status::unimplemented("not used in enrollment tests"))
        }

        async fn renew_cert(
            &self,
            _request: Request<fake::RenewCertRequest>,
        ) -> Result<Response<fake::RenewCertResponse>, Status> {
            Err(Status::unimplemented("not used in enrollment tests"))
        }

        async fn goodbye(
            &self,
            _request: Request<fake::GoodbyeRequest>,
        ) -> Result<Response<fake::GoodbyeResponse>, Status> {
            Err(Status::unimplemented("not used in enrollment tests"))
        }
    }

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
                .add_service(ShieldServiceServer::new(controller))
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

    fn token(pki: &Pki) -> String {
        use base64::Engine;
        let payload = serde_json::json!({
            "shield_id": SHIELD_ID,
            "workspace_id": "ws-test",
            "trust_domain": TRUST_DOMAIN,
            "ca_fingerprint": fingerprint(&pki.intermediate_pem),
            "connector_id": "conn-1",
            "connector_addr": "127.0.0.1:9091",
            "interface_addr": "100.64.0.5",
        });
        let b64 = base64::engine::general_purpose::URL_SAFE_NO_PAD;
        format!(
            "{}.{}.sig",
            b64.encode(br#"{"alg":"HS256","typ":"JWT"}"#),
            b64.encode(payload.to_string())
        )
    }

    /// A unique state directory, removed on drop.
    struct TempDir(std::path::PathBuf);

    impl TempDir {
        fn new() -> Self {
            static N: std::sync::atomic::AtomicU64 = std::sync::atomic::AtomicU64::new(0);
            let n = N.fetch_add(1, std::sync::atomic::Ordering::Relaxed);
            let nanos = std::time::SystemTime::now()
                .duration_since(std::time::UNIX_EPOCH)
                .unwrap()
                .as_nanos();
            let dir = std::env::temp_dir()
                .join(format!("shield-enroll-{}-{nanos}-{n}", std::process::id()));
            fs::create_dir_all(&dir).unwrap();
            Self(dir)
        }

        fn path(&self) -> &std::path::Path {
            &self.0
        }
    }

    impl Drop for TempDir {
        fn drop(&mut self) {
            let _ = fs::remove_dir_all(&self.0);
        }
    }

    struct Scenario {
        pki: Arc<Pki>,
        dir: TempDir,
        tokens: Arc<Mutex<Vec<String>>>,
    }

    impl Scenario {
        fn new() -> Self {
            Self {
                pki: Arc::new(Pki::new()),
                dir: TempDir::new(),
                tokens: Arc::new(Mutex::new(Vec::new())),
            }
        }

        async fn enroll(
            &self,
            ca_body: String,
            identity: (String, String),
            reply: Reply,
        ) -> Result<ShieldState> {
            let http = spawn_ca_http(ca_body).await;
            let grpc = spawn_grpc(
                identity,
                FakeController {
                    pki: self.pki.clone(),
                    reply,
                    tokens: self.tokens.clone(),
                },
            )
            .await;
            let cfg: ShieldConfig = serde_json::from_value(serde_json::json!({
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

        fn files(&self) -> Vec<String> {
            let mut names: Vec<String> = fs::read_dir(self.dir.path())
                .unwrap()
                .map(|e| e.unwrap().file_name().to_string_lossy().into_owned())
                .collect();
            names.sort();
            names
        }
    }

    /// Attack: Mallory appends her root to /ca.crt; today the whole file is
    /// trusted and her TLS server receives the enrollment token.
    #[tokio::test]
    async fn appended_ca_cannot_intercept_the_enrollment_token() {
        let s = Scenario::new();
        let tampered = format!("{}\n{}", s.pki.intermediate_pem, s.pki.attacker_pem);
        let result = s
            .enroll(tampered, s.pki.attacker_identity(), Reply::Genuine)
            .await;
        assert_eq!(
            s.tokens_received(),
            0,
            "the enrollment token reached Mallory's server ({result:?})"
        );
        assert!(result.is_err());
        assert!(
            s.files().is_empty(),
            "nothing may be persisted: {:?}",
            s.files()
        );
    }

    /// Attack: the reply carries Mallory's root as the Workspace CA and
    /// Intermediate; today it becomes the shield's trust store.
    #[tokio::test]
    async fn forged_enrollment_reply_is_not_trusted_or_persisted() {
        let s = Scenario::new();
        let result = s
            .enroll(
                s.pki.intermediate_pem.clone(),
                s.pki.controller_identity(),
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

    #[tokio::test]
    async fn reply_for_another_key_is_refused_and_nothing_persisted() {
        let s = Scenario::new();
        let result = s
            .enroll(
                s.pki.intermediate_pem.clone(),
                s.pki.controller_identity(),
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

    #[tokio::test]
    async fn failed_attempt_leaves_nothing_and_retry_succeeds() {
        let s = Scenario::new();
        let first = s
            .enroll(
                s.pki.intermediate_pem.clone(),
                s.pki.controller_identity(),
                Reply::Forged,
            )
            .await;
        assert!(first.is_err(), "forged first attempt must fail");
        assert!(s.files().is_empty(), "failed attempt left {:?}", s.files());
        let second = s
            .enroll(
                s.pki.intermediate_pem.clone(),
                s.pki.controller_identity(),
                Reply::Genuine,
            )
            .await;
        assert!(second.is_ok(), "retry must succeed: {second:?}");
    }

    #[tokio::test]
    async fn wrong_ca_is_refused_before_connecting() {
        let s = Scenario::new();
        let result = s
            .enroll(
                s.pki.attacker_pem.clone(),
                s.pki.attacker_identity(),
                Reply::Genuine,
            )
            .await;
        assert!(result.is_err());
        assert_eq!(s.tokens_received(), 0);
    }

    #[tokio::test]
    async fn genuine_enrollment_installs_the_controllers_trust() {
        let s = Scenario::new();
        let state = s
            .enroll(
                s.pki.intermediate_pem.clone(),
                s.pki.controller_identity(),
                Reply::Genuine,
            )
            .await
            .expect("genuine enrollment");
        assert_eq!(state.shield_id, SHIELD_ID);
        let bundle = fs::read_to_string(s.dir.path().join("workspace_ca.crt")).unwrap();
        assert_eq!(fingerprint(&bundle), fingerprint(&s.pki.workspace_ca_pem));
        assert!(bundle.contains(s.pki.intermediate_pem.trim()));
        for f in ["shield.key", "shield.crt", "workspace_ca.crt", "state.json"] {
            assert!(s.dir.path().join(f).exists(), "{f} missing");
        }
    }

    // ---- unit tests for the bug 1 fix (trust.rs) ------------------------------

    fn der(pem: &str) -> rustls::pki_types::CertificateDer<'static> {
        certs(&mut pem.as_bytes()).next().unwrap().unwrap()
    }

    /// A shield key's SPKI and a CSR for the shield identity.
    fn shield_key() -> (Vec<u8>, Vec<u8>) {
        let key = crypto::generate_keypair().unwrap();
        let spki = crypto::public_key_spki_der(&key.serialize_pem()).unwrap();
        let csr = crypto::build_csr(
            &key,
            "shield",
            &appmeta::shield_spiffe_id(TRUST_DOMAIN, SHIELD_ID),
        )
        .unwrap();
        (spki, csr)
    }

    #[test]
    fn pinning_requires_exactly_the_one_pinned_certificate() {
        let pki = Pki::new();
        let pin = fingerprint(&pki.intermediate_pem);
        assert!(trust::pinned_bootstrap_ca(&pki.intermediate_pem, &pin).is_ok());
        let appended = format!("{}\n{}", pki.intermediate_pem, pki.attacker_pem);
        let err = trust::pinned_bootstrap_ca(&appended, &pin).expect_err("appended CA refused");
        assert!(err.to_string().contains("exactly one certificate"), "{err}");
        assert!(trust::pinned_bootstrap_ca(&pki.attacker_pem, &pin).is_err());
        assert!(trust::pinned_bootstrap_ca("", &pin).is_err());
    }

    #[test]
    fn enrollment_validation_rejects_each_broken_link() {
        let pki = Pki::new();
        let (spki, csr) = shield_key();
        let id = appmeta::shield_spiffe_id(TRUST_DOMAIN, SHIELD_ID);
        let int = der(&pki.intermediate_pem);
        let anchors = trust::Anchors {
            intermediate: &int,
            workspace_ca: None,
        };
        let genuine = pki.genuine_leaf(&csr);
        let check = |ws: &str, int_pem: &str, leaf: &str| {
            trust::validate_issued_chain(
                &anchors,
                ws.as_bytes(),
                int_pem.as_bytes(),
                leaf.as_bytes(),
                &id,
                &spki,
            )
        };
        check(&pki.workspace_ca_pem, &pki.intermediate_pem, &genuine).expect("genuine chain");
        assert!(check(&pki.workspace_ca_pem, &pki.attacker_pem, &genuine).is_err());
        assert!(check(&pki.attacker_pem, &pki.intermediate_pem, &genuine).is_err());
        assert!(check(
            &pki.workspace_ca_pem,
            &pki.intermediate_pem,
            &pki.attacker_leaf(&csr)
        )
        .is_err());
        assert!(check(
            &pki.workspace_ca_pem,
            &pki.intermediate_pem,
            &pki.other_key_leaf()
        )
        .is_err());
        let smuggled = format!("{}\n{}", pki.workspace_ca_pem, pki.attacker_pem);
        assert!(check(&smuggled, &pki.intermediate_pem, &genuine).is_err());
    }

    /// Renewal: the returned Workspace CA and Intermediate must be exactly the
    /// anchors saved at enrollment, and the leaf must be for the shield's key.
    #[test]
    fn renewal_validation_never_replaces_the_pinned_anchors() {
        let pki = Pki::new();
        let other = Pki::new(); // another platform's genuine-looking hierarchy
        let (spki, csr) = shield_key();
        let id = appmeta::shield_spiffe_id(TRUST_DOMAIN, SHIELD_ID);
        let int = der(&pki.intermediate_pem);
        let ws = der(&pki.workspace_ca_pem);
        let anchors = trust::Anchors {
            intermediate: &int,
            workspace_ca: Some(&ws),
        };
        let check = |ws_pem: &str, int_pem: &str, leaf: &str| {
            trust::validate_issued_chain(
                &anchors,
                ws_pem.as_bytes(),
                int_pem.as_bytes(),
                leaf.as_bytes(),
                &id,
                &spki,
            )
        };
        check(
            &pki.workspace_ca_pem,
            &pki.intermediate_pem,
            &pki.genuine_leaf(&csr),
        )
        .expect("same anchors, same key: renewal accepted");
        let err = check(
            &other.workspace_ca_pem,
            &pki.intermediate_pem,
            &other.genuine_leaf(&csr),
        )
        .expect_err("a different Workspace CA must be refused");
        assert!(err.to_string().contains("pinned Workspace CA"), "{err}");
        assert!(check(
            &other.workspace_ca_pem,
            &other.intermediate_pem,
            &other.genuine_leaf(&csr)
        )
        .is_err());
        assert!(check(
            &pki.workspace_ca_pem,
            &pki.intermediate_pem,
            &pki.other_key_leaf()
        )
        .is_err());
    }

    #[test]
    fn install_removes_partial_files_when_a_write_fails() {
        let dir = TempDir::new();
        fs::create_dir(dir.path().join("workspace_ca.crt")).unwrap();
        fs::write(dir.path().join("workspace_ca.crt").join("blocker"), b"x").unwrap();
        let err = trust::install_files(
            dir.path(),
            &[
                ("shield.key", b"key".to_vec(), 0o600),
                ("shield.crt", b"crt".to_vec(), 0o644),
                ("workspace_ca.crt", b"ca".to_vec(), 0o644),
                ("state.json", b"{}".to_vec(), 0o644),
            ],
        )
        .expect_err("third write must fail");
        assert!(format!("{err:#}").contains("workspace_ca.crt"), "{err:#}");
        assert!(!dir.path().join("shield.key").exists(), "key left behind");
        assert!(!dir.path().join("shield.crt").exists(), "cert left behind");
        assert!(
            !dir.path().join("state.json").exists(),
            "state.json written"
        );
    }

    #[test]
    fn installed_key_is_private() {
        use std::os::unix::fs::PermissionsExt;
        let dir = TempDir::new();
        trust::install_files(dir.path(), &[("shield.key", b"key".to_vec(), 0o600)]).unwrap();
        let mode = fs::metadata(dir.path().join("shield.key"))
            .unwrap()
            .permissions()
            .mode();
        assert_eq!(mode & 0o777, 0o600);
    }

    #[test]
    fn validation_rejects_wrong_shape_certificates() {
        let pki = Pki::new();
        let (spki, csr) = shield_key();
        let id = appmeta::shield_spiffe_id(TRUST_DOMAIN, SHIELD_ID);
        let int = der(&pki.intermediate_pem);
        let anchors = trust::Anchors {
            intermediate: &int,
            workspace_ca: None,
        };
        let check = |ws: &str, leaf: &str, spiffe: &str| {
            trust::validate_issued_chain(
                &anchors,
                ws.as_bytes(),
                pki.intermediate_pem.as_bytes(),
                leaf.as_bytes(),
                spiffe,
                &spki,
            )
            .expect_err("must be rejected")
            .to_string()
        };
        let ws = pki.workspace_ca_pem.as_str();

        let server_only = pki.genuine_leaf_shaped(&csr, |p| {
            p.extended_key_usages = vec![ExtendedKeyUsagePurpose::ServerAuth];
        });
        assert!(check(ws, &server_only, &id).contains("client authentication"));
        let expired = pki.genuine_leaf_shaped(&csr, |p| {
            let now = ::time::OffsetDateTime::now_utc();
            p.not_before = now - ::time::Duration::hours(2);
            p.not_after = now - ::time::Duration::hours(1);
        });
        assert!(check(ws, &expired, &id).contains("validity"));
        let (not_a_ca, leaf) = pki.non_ca_workspace(&csr);
        assert!(check(&not_a_ca, &leaf, &id).contains("not a CA"));
        let other_shield = appmeta::shield_spiffe_id(TRUST_DOMAIN, "other-shield");
        assert!(check(ws, &pki.genuine_leaf(&csr), &other_shield).contains("identity"));
    }

    #[test]
    fn renewal_installs_only_a_validated_certificate() {
        let pki = Pki::new();
        let other = Pki::new();
        let dir = TempDir::new();
        let key = crypto::generate_keypair().unwrap();
        let key_pem = key.serialize_pem();
        let id = appmeta::shield_spiffe_id(TRUST_DOMAIN, SHIELD_ID);
        let csr = crypto::build_csr(&key, "shield", &id).unwrap();
        let anchors = format!("{}\n{}", pki.workspace_ca_pem, pki.intermediate_pem);
        fs::write(dir.path().join("shield.crt"), "previous").unwrap();
        let reply = |ws: &str, int: &str, leaf: String| proto::RenewCertResponse {
            certificate_pem: leaf.into_bytes(),
            workspace_ca_pem: ws.as_bytes().to_vec(),
            intermediate_ca_pem: int.as_bytes().to_vec(),
        };
        let renew = |r: &proto::RenewCertResponse| {
            crate::renewal::install_renewed_certificate(
                dir.path(),
                anchors.as_bytes(),
                &key_pem,
                &id,
                r,
            )
        };
        let unchanged = || fs::read_to_string(dir.path().join("shield.crt")).unwrap() == "previous";

        // Another platform's complete, self-consistent chain: refused.
        let forged = reply(
            &other.workspace_ca_pem,
            &other.intermediate_pem,
            other.genuine_leaf(&csr),
        );
        let err = renew(&forged).expect_err("forged renewal must be refused");
        assert!(
            format!("{err:#}").contains("renewed certificate rejected"),
            "{err:#}"
        );
        assert!(unchanged(), "shield.crt replaced by a rejected renewal");
        // A certificate for another key: refused.
        let other_key = reply(
            &pki.workspace_ca_pem,
            &pki.intermediate_pem,
            pki.other_key_leaf(),
        );
        assert!(renew(&other_key).is_err());
        assert!(unchanged(), "shield.crt replaced by a rejected renewal");

        // The genuine renewal installs; the trust store is never written.
        let leaf = pki.genuine_leaf(&csr);
        renew(&reply(
            &pki.workspace_ca_pem,
            &pki.intermediate_pem,
            leaf.clone(),
        ))
        .unwrap();
        let installed = fs::read_to_string(dir.path().join("shield.crt")).unwrap();
        assert!(
            installed.starts_with(leaf.trim_end()),
            "renewed leaf not installed"
        );
        assert!(
            !dir.path().join("workspace_ca.crt").exists(),
            "renewal wrote the trust store"
        );
    }
}
