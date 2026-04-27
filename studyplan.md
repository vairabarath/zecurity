Zecurity — Full Codebase Study Plan
Updated: 2026-04-27 — Sprint 4 fully merged (Shield + Connector agent_server + Admin UI)

> **Note:** This file predates the control_stream refactor (commit 722995c). All `heartbeat.rs` references in the study plan reflect the state of the codebase when each section was written. The old `heartbeat.rs` modules in both Connector and Shield were replaced by persistent bidirectional `control_stream.rs` in a later refactor.

─────────────────────────────────────────────
WHAT THIS FILE IS
─────────────────────────────────────────────

A reading-order guide + function-level call trace for every major flow.
Read each step in order. Every concept builds on the previous one.
Do NOT skip to the code you're working on — read the proto files first.


─────────────────────────────────────────────
30-SECOND ARCHITECTURE
─────────────────────────────────────────────

  ┌──────────────────────────────────────────────────────────────────────┐
  │  Admin UI (React/TS)  →  Controller (Go) :8080 HTTP + :9090 gRPC    │
  │                                  ↕ mTLS                              │
  │              Connector (Rust)  :9090 → Controller                   │
  │                  ↑ mTLS :9091                                        │
  │              Shield (Rust)  →  Connector :9091                      │
  └──────────────────────────────────────────────────────────────────────┘

  Admin UI     → talks to Controller via GraphQL over HTTPS
  Controller   → issues X.509 certs, stores state in Postgres + Redis
  Connector    → Linux agent on network edge; relays Shield traffic
  Shield       → Linux agent on resource host; creates TUN + firewall


─────────────────────────────────────────────
STEP 1 — CONTRACTS (proto files)  ~30 min
─────────────────────────────────────────────

Read these FIRST. Every other file implements them.

  proto/connector/v1/connector.proto
  proto/shield/v1/shield.proto

CONNECTOR SERVICE — 4 RPCs:

  rpc Enroll(EnrollRequest)         returns (EnrollResponse)
  rpc Heartbeat(HeartbeatRequest)   returns (HeartbeatResponse)
  rpc RenewCert(RenewCertRequest)   returns (RenewCertResponse)
  rpc Goodbye(GoodbyeRequest)       returns (GoodbyeResponse)

  HeartbeatRequest carries:
    connector_id, version, hostname, public_ip
    shields: repeated ShieldHealth   ← Connector reports all Shields it tracks

  ShieldHealth carries:
    shield_id, status, version, last_heartbeat_at

SHIELD SERVICE — same 4 RPCs, but EnrollResponse is richer:
  + interface_addr   string   ← /32 IP assigned to zecurity0 TUN
  + connector_addr   string   ← which Connector to heartbeat to (:9091)
  + connector_id     string   ← UUID of that Connector

WHY: The extra fields in ShieldEnrollResponse drive network setup in
     shield/src/enrollment.rs and shield/src/network.rs.


─────────────────────────────────────────────
STEP 2 — SHARED CONSTANTS (appmeta)  ~15 min
─────────────────────────────────────────────

These constants appear in every component. Mismatch = cert rejection.

  controller/internal/appmeta/identity.go
  connector/src/appmeta.rs
  shield/src/appmeta.rs

KEY VALUES (must be identical in Go and Rust):

  PRODUCT_NAME                = "ZECURITY"
  SPIFFE_GLOBAL_TRUST_DOMAIN  = "zecurity.in"
  SPIFFE_CONTROLLER_ID        = "spiffe://zecurity.in/controller/global"
  SPIFFE_ROLE_CONNECTOR       = "connector"
  SPIFFE_ROLE_SHIELD          = "shield"
  SHIELD_INTERFACE_NAME       = "zecurity0"
  SHIELD_INTERFACE_CIDR_RANGE = "100.64.0.0/10"

HELPER FUNCTIONS (Go):
  WorkspaceTrustDomain(slug) → "ws-acme.zecurity.in"
  ConnectorSPIFFEID(trustDomain, id) → "spiffe://ws-acme.zecurity.in/connector/<id>"
  ShieldSPIFFEID(trustDomain, id)    → "spiffe://ws-acme.zecurity.in/shield/<id>"


─────────────────────────────────────────────
STEP 3 — PKI (3-tier certificate chain)  ~45 min
─────────────────────────────────────────────

Read controller/internal/pki/ top to bottom:

  root.go          → Root CA (self-signed, 10yr, maxPathLen=2)
  intermediate.go  → Intermediate CA (signed by Root, 5yr, maxPathLen=1)
  workspace.go     → Workspace CA (per-tenant, 2yr) + signing functions
  crypto.go        → AES-256-GCM key encryption, PEM/DER helpers
  service.go       → Service interface (what callers use)  

KEY FUNCTIONS in workspace.go:

  pki.Service.SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, ttl)
    → validates CSR self-signature
    → sets SPIFFE URI SAN = ConnectorSPIFFEID(trustDomain, connectorID)
    → signs with workspace CA private key (decrypted from DB with AES-GCM)
    → returns *Certificate with PEM chain

  pki.Service.SignShieldCert(ctx, tenantID, shieldID, trustDomain, csr, ttl)
    → same flow but ShieldSPIFFEID

  pki.Service.GenerateControllerServerTLS(ctx, hosts, ttl)
    → called at startup to create ephemeral Controller TLS cert

MENTAL MODEL: Root → Intermediate → WorkspaceCA → Connector/Shield leaf cert
Every cert is 7 days. Renewal starts 48 hours before expiry.
Private keys NEVER leave the device they were generated on.


─────────────────────────────────────────────
STEP 4 — DATABASE SCHEMA  ~20 min
─────────────────────────────────────────────

Read controller/migrations/ in numeric order:

  001_schema.sql        → workspaces, users, ca_root, ca_intermediate, workspace_ca_keys
  002_connector_schema.sql → remote_networks, connectors
  003_shield_schema.sql → shields

CRITICAL COLUMNS TO NOTICE:

  connectors:
    status            ENUM('pending','active','disconnected','revoked')
    enrollment_token_jti  TEXT UNIQUE   ← stored in Redis too (single-use burn)
    last_heartbeat_at TIMESTAMPTZ
    cert_not_after    TIMESTAMPTZ

  shields:
    connector_id      UUID REFERENCES connectors   ← which Connector owns this Shield
    interface_addr    INET   ← /32 from 100.64.0.0/10 (unique per workspace)
    (everything else mirrors connectors)

  workspace_ca_keys:
    encrypted_private_key BYTEA   ← AES-256-GCM ciphertext
    nonce                 BYTEA   ← GCM nonce
    certificate_pem       TEXT


─────────────────────────────────────────────
STEP 5 — CONTROLLER (Go backend)  ~2 hours
─────────────────────────────────────────────

Read in this exact order:

① controller/internal/auth/
    → Google OAuth callback, JWT issuance, httpOnly refresh cookie
    → JWT payload: { sub: userID, workspace_id, tenant_id, role, exp }

② controller/internal/connector/token.go
    GenerateEnrollmentToken(connectorID, workspaceID, trustDomain, ttl)
      → HMAC-SHA256 JWT, claims: { sub: connectorID, jti: UUID, workspace_id, trust_domain }
      → Redis: SET "connector:jti:{jti}" connectorID EX {ttl}
    VerifyEnrollmentToken(tokenStr)
      → parses + verifies signature + expiry
      → returns claims struct
    BurnEnrollmentJTI(ctx, redis, jti)
      → Redis: GET + DEL in a transaction (atomic single-use burn)

③ controller/internal/connector/enrollment.go
    EnrollmentHandler.Enroll(ctx, req *EnrollRequest) (*EnrollResponse, error)
      Step 1: VerifyEnrollmentToken(req.EnrollmentToken)
      Step 2: BurnEnrollmentJTI → if already burned: return codes.AlreadyExists
      Step 3: SELECT connector WHERE id = claims.sub AND status = 'pending'
      Step 4: Parse req.CsrDer as PKCS#10
      Step 5: Verify CSR self-signature (proves key ownership)
      Step 6: Verify SPIFFE SAN == ConnectorSPIFFEID(trustDomain, connectorID)
      Step 7: pki.SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, cfg.CertTTL)
      Step 8: UPDATE connectors SET status='active', cert_serial, cert_not_after, last_heartbeat_at=NOW()
      Step 9: return EnrollResponse{ CertificatePem, WorkspaceCaPem, IntermediateCaPem }

④ controller/internal/connector/heartbeat.go
    Heartbeat(ctx, req *HeartbeatRequest) (*HeartbeatResponse, error)
      → verify mTLS client cert SPIFFE (UnarySPIFFEInterceptor ran first)
      → UPDATE connectors SET last_heartbeat_at=NOW(), version, hostname, public_ip
      → for each s in req.Shields:
          shieldSvc.UpdateShieldHealth(ctx, s.ShieldId, s.Status, s.Version, s.LastHeartbeatAt)
      → check cert_not_after: if < now + cfg.RenewalWindow → set re_enroll=true
      → return HeartbeatResponse{ Ok: true, ReEnroll: re_enroll }

    RunDisconnectWatcher(ctx, pool, cfg)
      → goroutine: every 60s runs:
          UPDATE connectors SET status='disconnected'
          WHERE status='active'
          AND last_heartbeat_at < NOW() - cfg.DisconnectThreshold (90s)

⑤ controller/internal/connector/goodbye.go
    Goodbye(ctx, req *GoodbyeRequest) (*GoodbyeResponse, error)
      → UPDATE connectors SET status='disconnected', last_heartbeat_at=NOW()
      → returns GoodbyeResponse{ Ok: true }

⑥ controller/internal/shield/token.go
    GenerateShieldToken(ctx, pool, redis, cfg, remoteNetworkID, shieldName, workspaceID, trustDomain)
      → selectConnector(ctx, pool, tenantID, remoteNetworkID)
            SELECT id FROM connectors
            WHERE tenant_id=$1 AND remote_network_id=$2 AND status='active'
            ORDER BY (SELECT COUNT(*) FROM shields WHERE connector_id=connectors.id AND status='active') ASC
            LIMIT 1
      → assignInterfaceAddr(ctx, pool, tenantID)
            iterate 100.64.0.0/10 (/32 per shield)
            SELECT WHERE interface_addr=$1 AND tenant_id=$2 (check uniqueness)
      → INSERT shields (name, status='pending', connector_id, interface_addr ...)
      → HMAC JWT: { sub: shieldID, jti, workspace_id, trust_domain, connector_id, interface_addr }
      → Redis: SET "shield:jti:{jti}" shieldID EX {ttl}
      → return (shieldID, jwtToken)

⑦ controller/internal/shield/enrollment.go
    Enroll(ctx, req *EnrollRequest) (*EnrollResponse, error)
      Step 1:  VerifyShieldToken(req.EnrollmentToken)
      Step 2:  BurnShieldJTI(redis, jti) — atomic GET+DEL
      Step 3:  SELECT shield WHERE id=claims.sub AND status='pending'
      Step 4:  SELECT connector WHERE id=claims.connector_id AND status='active'
      Step 5:  Verify workspace trust domain matches
      Step 6:  Parse req.CsrDer as PKCS#10
      Step 7:  Verify CSR SPIFFE SAN == ShieldSPIFFEID(trustDomain, shieldID)
      Step 8:  pki.SignShieldCert(ctx, tenantID, shieldID, trustDomain, csr, cfg.CertTTL)
      Step 9:  UPDATE shields SET status='active', cert_serial, cert_not_after, last_heartbeat_at=NOW()
      Step 10: SELECT connector WHERE id=claims.connector_id → get connector_addr
      Step 11: return EnrollResponse{
                 CertificatePem, WorkspaceCaPem, IntermediateCaPem,
                 InterfaceAddr: shield.InterfaceAddr,
                 ConnectorAddr: connector.PublicIP + ":9091",
                 ConnectorId:   connector.ID,
               }

⑧ controller/internal/shield/heartbeat.go
    UpdateShieldHealth(ctx, pool, shieldID, status, version, lastHeartbeatAt)
      → UPDATE shields SET status, version, last_heartbeat_at WHERE id=$1

    RunDisconnectWatcher(ctx, pool, cfg)
      → goroutine: every 60s:
          UPDATE shields SET status='disconnected'
          WHERE status='active'
          AND last_heartbeat_at < NOW() - cfg.DisconnectThreshold (120s)

⑨ controller/cmd/server/main.go  — startup wiring
    main()
      → db.Init()                            → *pgxpool.Pool
      → pki.Init()                           → pki.Service (loads or creates 3-tier CA)
      → auth.NewService()                    → auth.Service (Google OAuth)
      → connector.NewConfig() + shield.NewConfig()
      → redis.NewClient()
      → gqlgen.NewDefaultServer(NewSchema(resolver))
      → RegisterRoutes:
          POST /graphql    → gqlgen handler (JWT + workspace middleware)
          GET  /auth/callback  → auth.CallbackHandler
          POST /auth/refresh   → auth.RefreshHandler
          GET  /ca.crt         → connector.CACertHandler
          GET  /health         → 200 OK
      → grpc.NewServer(tlsCredentials, UnarySPIFFEInterceptor)
      → pb_connector.RegisterConnectorServiceServer(grpc, enrollmentHandler)
      → pb_shield.RegisterShieldServiceServer(grpc, shieldService)
      → go connector.RunDisconnectWatcher(ctx, pool, connCfg)
      → go shield.RunDisconnectWatcher(ctx, pool, shieldCfg)
      → grpcServer.Serve(lis) + httpServer.ListenAndServe()


─────────────────────────────────────────────
STEP 6 — GRAPHQL API  ~1 hour
─────────────────────────────────────────────

Read the schema files first, then the resolvers:

  controller/graph/schema.graphqls          → root Query + Mutation types
  controller/graph/connector.graphqls       → RemoteNetwork, Connector, NetworkHealth enum
  controller/graph/shield.graphqls          → Shield type, GenerateShieldToken, RevokeShield, DeleteShield

  controller/graph/resolvers/resolver.go
    type Resolver struct {
      TenantDB     *db.TenantDB
      AuthService  auth.Service
      ConnectorCfg connector.Config
      ShieldCfg    shield.Config
      Redis        *redis.Client
      Pool         *pgxpool.Pool
    }

  controller/graph/resolvers/connector.resolvers.go
    → NetworkHealth computation:
        compute from connector list: all ONLINE → ONLINE, any DISCONNECTED → DEGRADED, all DISCONNECTED → OFFLINE

  controller/graph/resolvers/shield.resolvers.go
    GenerateShieldToken resolver
      → calls shield.GenerateShieldToken(...)
      → returns ShieldTokenPayload { shieldId, installCommand }

  controller/graph/resolvers/helpers.go
    → shared utilities used by multiple resolvers


─────────────────────────────────────────────
STEP 7 — CONNECTOR (Rust)  ~2 hours
─────────────────────────────────────────────

Read files in this order:

① connector/src/config.rs
    ConnectorConfig {
      controller_addr: String,           // gRPC (e.g. "controller.example.com:9090")
      controller_http_addr: String,      // HTTP (for /ca.crt)
      enrollment_token: String,          // JWT (from admin UI)
      state_dir: String,                 // /var/lib/zecurity-connector
      auto_update_enabled: bool,
      log_level: String,
    }
    Loaded via figment: env vars > /etc/zecurity/connector.conf (TOML)

② connector/src/crypto.rs
    generate_ec_p384_keypair() → (PrivateKey, PublicKey)
    build_csr(private_key, spiffe_id: &str) → CertificateRequest
    public_key_to_der(key) → Vec<u8>
    Key stays local. Only CSR (public) goes over the wire.

③ connector/src/tls.rs
    verify_controller_spiffe(cert, trust_domain)
      → checks cert has URI SAN == SPIFFE_CONTROLLER_ID
      → called after TLS handshake to ensure we're talking to the right Controller

④ connector/src/enrollment.rs
    async fn enroll(cfg: &ConnectorConfig) -> Result<EnrollmentState>
      1. GET {controller_http_addr}/ca.crt  → ca_cert_pem
      2. Compute SHA-256(ca_cert_der) → verify matches JWT claim ca_fingerprint
      3. generate_ec_p384_keypair()
      4. Parse JWT claims: connector_id, trust_domain, workspace_id
      5. build_csr(private_key, ConnectorSPIFFEID(trust_domain, connector_id))
      6. tonic::Channel::from_shared(controller_addr) with plain TLS (no client cert)
      7. ConnectorServiceClient::new(channel).enroll(EnrollRequest{
           enrollment_token, csr_der, version, hostname
         })
      8. Write state_dir/connector.crt   (response.certificate_pem)
      9. Write state_dir/connector.key   (private key, never sent)
      10. Write state_dir/workspace_ca.crt (response.workspace_ca_pem)
      11. Write state_dir/state.json { connector_id, trust_domain, workspace_id, enrolled_at }
      12. return EnrollmentState { connector_id, trust_domain, workspace_id, enrolled_at }

⑤ connector/src/heartbeat.rs (historical — now `connector/src/control_stream.rs`, commit 722995c)
    async fn run_heartbeat(cfg, enrollment_state, shield_server) -> Result<()>
      1. Build mTLS channel: client cert = connector.crt + connector.key, CA = workspace_ca.crt
      2. Pre-flight: raw TLS connection → verify_controller_spiffe()
      3. loop every cfg.heartbeat_interval_secs (default 30s):
           shields_health = shield_server.get_alive_shields()
           res = client.heartbeat(HeartbeatRequest{
             connector_id, version, hostname, public_ip,
             shields: shields_health,
           })
           if res.re_enroll → renewal::renew_cert(state, cfg) → rebuild channel
           on error → exponential backoff (5s → 60s cap, reset on success)

⑥ connector/src/renewal.rs
    async fn renew_cert(state: &EnrollmentState, cfg: &ConnectorConfig) -> Result<EnrollmentState>
      1. Read state_dir/connector.key
      2. Extract public key DER: public_key_to_der(key)
      3. Call mTLS channel: client.renew_cert(RenewCertRequest{ connector_id, public_key_der })
      4. Write new connector.crt (response.certificate_pem)
      5. Update state.json with new cert_not_after
      6. return updated EnrollmentState

⑦ connector/src/agent_server.rs  ← NEW (Sprint 4)
    ShieldServer struct:
      alive_shields: Arc<Mutex<HashMap<String, ShieldHealth>>>
      controller_channel: Channel  (mTLS to Controller :9090)
      trust_domain: String
      connector_id: String

    ShieldServer::new(controller_channel, trust_domain, connector_id) → Self
    ShieldServer::serve(addr, state_dir) → starts tonic gRPC on :9091

    Implements ShieldService for ShieldServer:

    Heartbeat(req: HeartbeatRequest) → HeartbeatResponse
      → update alive_shields[req.shield_id] = ShieldHealth { status, version, last_heartbeat_at: now }
      → check cert_not_after: if within renewal window → set re_enroll=true
      → return HeartbeatResponse{ ok: true, re_enroll }

    RenewCert(req: RenewCertRequest) → RenewCertResponse
      → forward to Controller via controller_channel (proxied, Shield's proof-of-possession)
      → return response directly to Shield

    Goodbye(req: GoodbyeRequest) → GoodbyeResponse
      → remove req.shield_id from alive_shields map
      → return GoodbyeResponse{ ok: true }

    Enroll(req: EnrollRequest) → EnrollResponse
      → return Err(Status::unimplemented("Shield enrolls directly with Controller"))

    ShieldServer::get_alive_shields() → Vec<ShieldHealth>
      → read alive_shields map → return values for heartbeat loop

⑧ connector/src/main.rs
    main()
      → rustls::crypto::ring::default_provider().install_default()
      → parse args: if --check-update → updater::run_single_check() → exit
      → load ConnectorConfig
      → init tracing
      → if !state_dir/state.json exists → enrollment::enroll(&cfg) → EnrollmentState
      → else → load EnrollmentState from state.json
      → load certs from state_dir
      → build controller_channel (mTLS)
      → let shield_server = ShieldServer::new(controller_channel.clone(), trust_domain, connector_id)
      → tokio::spawn(shield_server.serve("0.0.0.0:9091", state_dir))
      → tokio::spawn(heartbeat::run_heartbeat(cfg, state, shield_server.clone()))
      → if cfg.auto_update_enabled → tokio::spawn(updater::run_update_loop(cfg))
      → signal::ctrl_c().await (SIGTERM)  → graceful shutdown


─────────────────────────────────────────────
STEP 8 — SHIELD (Rust)  ~2 hours
─────────────────────────────────────────────

Same structure as Connector but heartbeats to Connector and adds kernel networking.

① shield/src/config.rs
    ShieldConfig {
      controller_addr: String,           // gRPC to Controller :9090 (enrollment only)
      controller_http_addr: String,      // HTTP (for /ca.crt, enrollment only)
      enrollment_token: String,
      state_dir: String,                 // /var/lib/zecurity-shield
      shield_heartbeat_interval_secs: u64,  // default 60s
      auto_update_enabled: bool,
      log_level: String,
    }

② shield/src/types.rs
    ShieldState {
      shield_id: String,
      trust_domain: String,
      connector_id: String,
      connector_addr: String,     // "{connector_ip}:9091"
      interface_addr: String,     // "100.64.x.x/32"
      enrolled_at: String,        // RFC 3339
      cert_not_after: i64,        // Unix timestamp
    }
    ShieldState::load(state_dir) → Result<ShieldState>
    ShieldState::save(&self, state_dir) → Result<()>

③ shield/src/crypto.rs
    Mirrors connector/src/crypto.rs exactly.
    generate_ec_p384_keypair(), build_csr(), public_key_to_der()

④ shield/src/tls.rs
    verify_connector_spiffe(cert, trust_domain, connector_id)
      → checks URI SAN == ConnectorSPIFFEID(trust_domain, connector_id)
      → Shield verifies it's talking to its assigned Connector, not any Connector

⑤ shield/src/enrollment.rs
    async fn enroll(cfg: &ShieldConfig) -> Result<ShieldState>
      1. GET {controller_http_addr}/ca.crt
      2. Verify SHA-256(ca) matches JWT claim
      3. generate_ec_p384_keypair()
      4. Parse JWT: shield_id, trust_domain, connector_id, interface_addr
      5. build_csr(key, ShieldSPIFFEID(trust_domain, shield_id))
      6. Connect to controller_addr (plain TLS, no client cert)
      7. ShieldServiceClient::new(channel).enroll(EnrollRequest{
           enrollment_token, csr_der, version, hostname
         })
      8. Write shield.crt, shield.key, workspace_ca.crt
      9. network::setup(response.interface_addr, response.connector_addr)  ← see step ⑦
      10. Save state.json as ShieldState {
            shield_id, trust_domain,
            connector_id: response.connector_id,
            connector_addr: response.connector_addr,
            interface_addr: response.interface_addr,
            cert_not_after,
          }
      11. return ShieldState

⑥ shield/src/heartbeat.rs (historical — now `shield/src/control_stream.rs`, commit 722995c)
    async fn run(state: ShieldState, cfg: ShieldConfig) -> Result<()>
      1. Build mTLS channel to state.connector_addr (Shield cert + workspace CA)
      2. Pre-flight: verify_connector_spiffe(peer_cert, trust_domain, connector_id)
      3. loop every cfg.shield_heartbeat_interval_secs (default 60s):
           res = client.heartbeat(HeartbeatRequest{
             shield_id: state.shield_id,
             version, hostname, public_ip,
           })
           if res.re_enroll → renewal::renew_cert(&state, &cfg) → rebuild channel
           on error → exponential backoff (5s → 60s cap)

    async fn goodbye(state: &ShieldState, cfg: &ShieldConfig)
      → best-effort Goodbye RPC to connector_addr
      → removes this Shield from Connector's alive_shields map immediately

⑦ shield/src/network.rs  ← UNIQUE to Shield (kernel networking)
    async fn setup(interface_addr: &str, connector_addr: &str) -> Result<()>
      TUN INTERFACE:
        1. rtnetlink::new_connection() → Handle
        2. handle.link().add().name("zecurity0").kind(LinkKind::Tun).execute()
        3. handle.address().add(if_index, interface_addr /32, 32).execute()
        4. handle.link().set(if_index).up().execute()

      NFTABLES FIREWALL:
        1. nftables::helper::get_current_ruleset()
        2. Create table: inet zecurity
        3. Create chain: input (type filter, hook input, policy drop)
        4. Add rules to chain:
             accept iif lo
             accept ip saddr {connector_ip}   ← allow heartbeat traffic from Connector
             drop                             ← drop everything else on zecurity0

      WHY: Shield is a protected resource. Only the assigned Connector can reach it.
      REQUIRES: CAP_NET_ADMIN + CAP_NET_RAW (set in systemd unit)

⑧ shield/src/renewal.rs
    async fn renew_cert(state: &ShieldState, cfg: &ShieldConfig) -> Result<ShieldState>
      1. Read shield.key
      2. public_key_to_der(key)
      3. Connect mTLS to connector_addr:9091
      4. ShieldServiceClient::new(channel).renew_cert(RenewCertRequest{
           shield_id, public_key_der
         })
         ↑ This goes to agent_server.rs::RenewCert which proxies to Controller
      5. Save new shield.crt
      6. Update and save ShieldState with new cert_not_after
      7. return updated ShieldState

⑨ shield/src/updater.rs
    Same as connector: poll GitHub releases for shield-v* tags, verify checksum, atomic replace.

⑩ shield/src/main.rs
    main()
      → rustls::crypto::ring::default_provider().install_default()
      → parse args: --check-update → updater::run_single_check() → exit
      → load ShieldConfig
      → init tracing
      → if !state_dir/state.json → enrollment::enroll(&cfg) → ShieldState
      → else → ShieldState::load(state_dir)
      → tokio::spawn(heartbeat::run(state.clone(), cfg.clone()))
      → if cfg.auto_update_enabled → tokio::spawn(updater::run_update_loop(cfg))
      → signal handler: SIGTERM/SIGINT → heartbeat::goodbye(&state, &cfg) → exit


─────────────────────────────────────────────
STEP 9 — SYSTEMD + CI  ~30 min
─────────────────────────────────────────────

  shield/systemd/zecurity-shield.service
    → AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW  ← required for TUN + nftables
    → EnvironmentFile=/etc/zecurity/shield.env
    → ExecStart=/usr/local/bin/zecurity-shield

  shield/systemd/zecurity-shield-update.service + .timer
    → Timer: OnCalendar=weekly
    → Service: ExecStart=zecurity-shield --check-update
    → If newer version found: download, verify SHA-256, atomic replace binary, restart

  shield/scripts/shield-install.sh
    → Downloads shield binary from GitHub Releases
    → Writes /etc/zecurity/shield.env (ENROLLMENT_TOKEN etc.)
    → Installs systemd units + enables them
    → Runs systemctl start zecurity-shield

  .github/workflows/shield-release.yml
    → Triggers on tag push: shield-v*
    → Uses cross-rs to build: x86_64-unknown-linux-musl + aarch64-unknown-linux-musl
    → Uploads artifacts to GitHub Release:
        zecurity-shield-linux-amd64
        zecurity-shield-linux-arm64
        checksums.txt


─────────────────────────────────────────────
STEP 10 — ADMIN UI (React/TS)  ~1 hour
─────────────────────────────────────────────

① admin/src/apollo/client.ts
    ApolloClient with link chain:
      errorLink → authLink → httpLink(/graphql)
    errorLink: on 401 → attempt silent refresh → redirect to /login on failure
    authLink: attach Authorization: Bearer {accessToken} header

② admin/src/apollo/links/auth.ts
    Public operations (no JWT needed):
      InitiateAuth, LookupWorkspace, LookupWorkspacesByEmail
    All others → attach Authorization header from Zustand auth store

③ admin/src/graphql/queries.graphql
    Key queries added in Sprint 4:
      GetShields(remoteNetworkId) → Shield[]
      GetRemoteNetworks → RemoteNetwork { id, name, location, status, networkHealth, connectors, shields }

④ admin/src/graphql/mutations.graphql
    New in Sprint 4:
      GenerateShieldToken(remoteNetworkId, shieldName) → ShieldTokenPayload
      RevokeShield(id) → Shield
      DeleteShield(id) → Boolean

⑤ admin/src/pages/RemoteNetworks.tsx
    → Shows NetworkHealth badge (🟢 ONLINE / 🟡 DEGRADED / 🔴 OFFLINE)
    → Shows shield count per network card
    → useQuery(GET_REMOTE_NETWORKS, { pollInterval: 30000 })

⑥ admin/src/pages/Shields.tsx
    → Lists shields: Name, Status, Interface IP (/32 from zecurity0), Via (connector name), Last Seen
    → "Add Shield" → InstallCommandModal (same pattern as connectors)
    → useMutation(GENERATE_SHIELD_TOKEN) → shows install command
    → useQuery(GET_SHIELDS, { pollInterval: 30000 })

⑦ admin/src/components/InstallCommandModal.tsx
    Step 1: Enter shield/connector name
    Step 2: Display one-liner:
      curl -fsSL https://github.com/.../shield-install.sh | \
        ENROLLMENT_TOKEN=<jwt> SHIELD_ID=<id> bash

⑧ admin/src/store/auth.ts  (Zustand)
    { accessToken, setAccessToken, clearAccessToken, isRefreshing }
    Token stored in memory only (NOT localStorage — XSS protection)
    Refresh token stored in httpOnly cookie (set by Controller /auth/callback)


─────────────────────────────────────────────
COMPLETE END-TO-END FLOWS
─────────────────────────────────────────────

═══════════════════════════════════════════
FLOW A: Admin creates a Shield (full trace)
═══════════════════════════════════════════

1. Admin UI (Shields.tsx)
   → click "Add Shield" → InstallCommandModal opens
   → enter name → click "Generate"
   → useMutation(GENERATE_SHIELD_TOKEN, { variables: { remoteNetworkId, shieldName } })

2. Apollo Client
   → POST /graphql  Authorization: Bearer {jwt}
   → body: { query: "mutation GenerateShieldToken(...)" }

3. Controller HTTP (main.go)
   → authMiddleware: parse JWT, set userID + tenantID in context
   → workspaceGuard: verify user belongs to workspace
   → gqlgen handler routes to GenerateShieldToken resolver

4. shield.resolvers.go → GenerateShieldToken
   → shield.GenerateShieldToken(ctx, pool, redis, cfg, remoteNetworkID, shieldName, workspaceID, trustDomain)

5. controller/internal/shield/token.go
   → selectConnector() SQL: pick least-loaded active Connector in same network
   → assignInterfaceAddr() SQL: iterate 100.64.0.0/10, find unused /32
   → INSERT shields (name='my-shield', status='pending', connector_id, interface_addr='100.64.0.5')
   → GenerateShieldEnrollmentJWT: { sub: shieldID, jti: UUID, connector_id, interface_addr, trust_domain }
   → Redis: SET "shield:jti:{jti}" shieldID EX 86400
   → return (shieldID, jwtToken)

6. Resolver returns ShieldTokenPayload { installCommand: "curl ... | ENROLLMENT_TOKEN=<jwt> bash" }

7. Admin UI shows install command. Admin copies + runs on resource host.

8. installer runs on Linux host:
   → shield-install.sh downloads binary, writes /etc/zecurity/shield.env
   → systemctl start zecurity-shield

9. shield/src/main.rs
   → no state.json → enrollment::enroll(&cfg)

10. shield/src/enrollment.rs
    → GET controller_http_addr/ca.crt
    → SHA-256(ca_cert_der) → verify matches JWT claim ca_fingerprint
    → generate_ec_p384_keypair()
    → parse JWT claims: shield_id="abc", trust_domain="ws-acme.zecurity.in", connector_id="xyz", interface_addr="100.64.0.5/32"
    → build_csr(key, "spiffe://ws-acme.zecurity.in/shield/abc")
    → tonic channel to controller_addr (plain TLS, no client cert)
    → ShieldServiceClient.enroll(EnrollRequest { enrollment_token, csr_der, version, hostname })

11. controller/internal/shield/enrollment.go → Enroll()
    → VerifyShieldToken(req.EnrollmentToken)
    → BurnShieldJTI(redis, jti) → Redis GET+DEL (atomic, single-use)
    → SELECT shield WHERE id='abc' AND status='pending'
    → SELECT connector WHERE id='xyz' AND status='active'
    → parse + verify CSR (SPIFFE SAN must == "spiffe://ws-acme.zecurity.in/shield/abc")
    → pki.SignShieldCert() → 7-day X.509 cert with SPIFFE URI SAN
    → UPDATE shields SET status='active', cert_serial, cert_not_after, last_heartbeat_at=NOW()
    → return EnrollResponse {
        certificate_pem,
        workspace_ca_pem,
        intermediate_ca_pem,
        interface_addr: "100.64.0.5/32",
        connector_addr: "192.168.1.10:9091",
        connector_id: "xyz",
      }

12. shield/src/enrollment.rs (continued)
    → write shield.crt, shield.key, workspace_ca.crt
    → network::setup("100.64.0.5/32", "192.168.1.10:9091")

13. shield/src/network.rs → setup()
    → rtnetlink: ip link add zecurity0 type tun
    → rtnetlink: ip addr add 100.64.0.5/32 dev zecurity0
    → rtnetlink: ip link set zecurity0 up
    → nftables: table inet zecurity { chain input { type filter hook input priority 0; policy drop;
        iif lo accept; ip saddr 192.168.1.10 accept; drop } }

14. shield/src/enrollment.rs → save state.json, return ShieldState

15. shield/src/main.rs → tokio::spawn(heartbeat::run(state, cfg))

16. shield/src/control_stream.rs (formerly heartbeat.rs) → run_once()
    → mTLS channel to "192.168.1.10:9091" (shield.crt + workspace_ca.crt)
    → verify_connector_spiffe(peer_cert, "ws-acme.zecurity.in", "xyz")
    → persistent bidirectional stream:
        every 60s: ShieldServiceClient.heartbeat(HeartbeatRequest { shield_id: "abc", ... })
        on inbound: handle Control stream messages

17. connector/src/agent_server.rs → Heartbeat()
    → alive_shields["abc"] = ShieldHealth { status: ONLINE, version, last_heartbeat_at: now }
    → cert expiry check → re_enroll = false (fresh cert)
    → return HeartbeatResponse { ok: true, re_enroll: false }

18. connector/src/control_stream.rs (formerly heartbeat.rs) → run()
    → (30s tick): get_alive_shields() → [ShieldHealth { shield_id: "abc", status: ONLINE, ... }]
    → persistent bidirectional stream to Controller:
        ConnectorServiceClient.heartbeat(HeartbeatRequest {
            connector_id: "xyz",
            shields: [ShieldHealth{ shield_id: "abc", status: ONLINE, ... }]
        })

19. controller/internal/connector/heartbeat.go → Heartbeat()
    → UPDATE connectors SET last_heartbeat_at=NOW() WHERE id='xyz'
    → for s in req.Shields: shieldSvc.UpdateShieldHealth(ctx, "abc", ONLINE, version, ts)
         → UPDATE shields SET status='active', version, last_heartbeat_at WHERE id='abc'
    → return HeartbeatResponse { ok: true, re_enroll: false }

20. Admin UI (Shields.tsx)
    → 30s poll fires: useQuery(GET_SHIELDS) refetches
    → Shield "my-shield" now shows Status: ONLINE, Interface: 100.64.0.5, Via: connector-xyz
    ✓ DONE


═══════════════════════════════════════════
FLOW B: Certificate Auto-Renewal
═══════════════════════════════════════════

Trigger: cert_not_after is within 48 hours.

1. connector/src/heartbeat.rs
   → HeartbeatResponse.re_enroll == true
   → call renewal::renew_cert(&state, &cfg)

2. connector/src/renewal.rs
   → read state_dir/connector.key (private key stays local)
   → public_key_to_der(key) → DER bytes (only public key sent)
   → mTLS to controller_addr
   → ConnectorServiceClient.renew_cert(RenewCertRequest { connector_id, public_key_der })

3. controller: RenewCert handler (connector/renewal.go)
   → verify mTLS client cert: SPIFFE SAN must match stored connector_id
   → verify cert is within renewal window (not expired + not too early)
   → pki.SignConnectorCert() with existing key (DER public key from req)
   → UPDATE connectors SET cert_serial, cert_not_after
   → return RenewCertResponse { certificate_pem, workspace_ca_pem, intermediate_ca_pem }

4. connector/src/renewal.rs
   → write new connector.crt
   → update state.json cert_not_after
   → return updated EnrollmentState

5. connector/src/heartbeat.rs
   → rebuild mTLS channel with new cert
   → continue heartbeat loop

For Shield renewal:
  shield heartbeat.rs gets re_enroll → renewal::renew_cert()
    → RenewCert RPC to Connector:9091 (agent_server.rs)
    → agent_server.rs proxies to Controller:9090
  Same steps 3-5 but for Shield cert.


═══════════════════════════════════════════
FLOW C: Graceful Shutdown (SIGTERM)
═══════════════════════════════════════════

Shield SIGTERM:
  shield/src/main.rs signal handler
    → heartbeat::goodbye(&state, &cfg)
    → ShieldServiceClient.goodbye(GoodbyeRequest{ shield_id: "abc" }) → Connector:9091
    → connector/agent_server.rs Goodbye()
         → remove "abc" from alive_shields map
    → Shield exits

  On next Connector heartbeat → shields[] no longer includes "abc"
  → Controller sees empty/missing entry → does NOT update shield's last_heartbeat_at
  → RunDisconnectWatcher: after 120s → UPDATE shields SET status='disconnected' WHERE id='abc'

  OR: Shield crashes (no SIGTERM)
  → alive_shields map still has "abc" until Connector's own restart
  → RunDisconnectWatcher marks DISCONNECTED after 120s of no DB update

Connector SIGTERM:
  connector/src/main.rs signal handler
    → graceful_shutdown: ConnectorServiceClient.goodbye(GoodbyeRequest{ connector_id: "xyz" }) → Controller:9090
    → controller/connector/goodbye.go Goodbye()
         → UPDATE connectors SET status='disconnected' WHERE id='xyz'
    → Connector exits


═══════════════════════════════════════════
FLOW D: Disconnect Detection (missed heartbeats)
═══════════════════════════════════════════

controller/internal/connector/heartbeat.go:RunDisconnectWatcher(ctx, pool, cfg)
  goroutine, runs every 60s:
    UPDATE connectors SET status='disconnected'
    WHERE status='active'
    AND last_heartbeat_at < NOW() - interval '90 seconds'

controller/internal/shield/heartbeat.go:RunDisconnectWatcher(ctx, pool, cfg)
  goroutine, runs every 60s:
    UPDATE shields SET status='disconnected'
    WHERE status='active'
    AND last_heartbeat_at < NOW() - interval '120 seconds'

Admin UI sees updated status on next 30s poll.


─────────────────────────────────────────────
KEY CONCEPTS CHEAT SHEET
─────────────────────────────────────────────

  ┌─────────────────────────────────┬────────────────────────────────────────────────────────┐
  │ Concept                         │ Where                                                  │
  ├─────────────────────────────────┼────────────────────────────────────────────────────────┤
  │ SPIFFE identity                 │ appmeta, pki/workspace.go, tls.rs in both Rust crates  │
  │ Single-use enrollment token     │ token.go (Redis JTI atomic GET+DEL)                    │
  │ CA fingerprint verification     │ enrollment.rs step 2 (prevents MITM on enrollment)     │
  │ Private key never leaves device │ crypto.rs (only CSR/public key goes over wire)         │
  │ Proof-of-possession renewal     │ renewal.rs (CSR self-signed by existing private key)   │
  │ Shield heartbeats via Connector │ shield/control_stream.rs → agent_server.rs → control_stream.rs   │
  │ Connector proxies RenewCert     │ agent_server.rs RenewCert → Controller :9090           │
  │ Least-loaded Connector assign   │ shield/token.go selectConnector() (SQL COUNT shields)  │
  │ Interface address pool          │ shield/token.go assignInterfaceAddr() (100.64.0.0/10)  │
  │ Disconnect detection            │ RunDisconnectWatcher goroutine (90s/120s threshold)    │
  │ CAP_NET_ADMIN requirement       │ systemd service unit + network.rs                      │
  │ mTLS everywhere                 │ All post-enrollment gRPC (Controller:9090, Connector:9091) │
  └─────────────────────────────────┴────────────────────────────────────────────────────────┘


─────────────────────────────────────────────
FULL CALL CHAIN REFERENCE
─────────────────────────────────────────────

Shield heartbeat reaches Controller:

  shield/control_stream.rs::run_once()  (formerly heartbeat.rs)
    → ShieldServiceClient::heartbeat()         [mTLS to Connector :9091]
    → connector/agent_server.rs::Heartbeat()   [updates alive_shields map]
    → (every 30s tick) connector/control_stream.rs::run()
    → ConnectorServiceClient::heartbeat()      [mTLS to Controller :9090]
    → controller/connector/heartbeat.go::Heartbeat()
    → UPDATE connectors SET last_heartbeat_at
    → shieldSvc.UpdateShieldHealth() for each shield in req.Shields
    → UPDATE shields SET status, last_heartbeat_at

Admin reads Shield status:

  Shields.tsx useQuery(GET_SHIELDS, pollInterval: 30000)
    → Apollo POST /graphql { query: GetShields(remoteNetworkId) }
    → authMiddleware + workspaceGuard
    → shield.resolvers.go::Shields()
    → SELECT * FROM shields WHERE tenant_id=$1 AND remote_network_id=$2
    → return Shield[] { id, name, status, interfaceAddr, lastHeartbeatAt, version, hostname }


─────────────────────────────────────────────
ENVIRONMENT VARIABLES REFERENCE
─────────────────────────────────────────────

CONTROLLER (.env):
  DATABASE_URL                    PostgreSQL connection string
  REDIS_URL                       Redis (JTI tracking)
  PORT                            HTTP port (default 8080)
  GRPC_PORT                       gRPC port (default 9090)
  JWT_SECRET                      HMAC-SHA256 signing key
  GOOGLE_CLIENT_ID                OAuth
  GOOGLE_CLIENT_SECRET
  GOOGLE_REDIRECT_URI
  PKI_MASTER_SECRET               AES-GCM key for CA private key encryption
  ALLOWED_ORIGIN                  CORS
  CONNECTOR_CERT_TTL              default 168h (7 days)
  CONNECTOR_HEARTBEAT_INTERVAL    default 30s
  CONNECTOR_DISCONNECT_THRESHOLD  default 90s
  CONNECTOR_RENEWAL_WINDOW        default 48h
  SHIELD_CERT_TTL                 default 168h
  SHIELD_DISCONNECT_THRESHOLD     default 120s
  SHIELD_RENEWAL_WINDOW           default 48h

CONNECTOR (/etc/zecurity/connector.conf):
  CONTROLLER_ADDR                 e.g. controller.example.com:9090
  CONTROLLER_HTTP_ADDR            e.g. https://controller.example.com
  ENROLLMENT_TOKEN                one-time JWT from admin UI
  STATE_DIR                       /var/lib/zecurity-connector
  AUTO_UPDATE_ENABLED             true/false

SHIELD (/etc/zecurity/shield.env):
  CONTROLLER_ADDR
  CONTROLLER_HTTP_ADDR
  ENROLLMENT_TOKEN
  STATE_DIR                       /var/lib/zecurity-shield
  SHIELD_HEARTBEAT_INTERVAL_SECS  default 60
  AUTO_UPDATE_ENABLED


─────────────────────────────────────────────
BUILD COMMANDS (run after studying each layer)
─────────────────────────────────────────────

  buf generate                                        # Proto → Go stubs
  cd controller && go build ./...                     # Controller
  cd connector && cargo build                         # Connector (incl. agent_server)
  cargo build --manifest-path shield/Cargo.toml       # Shield (full crate)
  cd admin && npm run build                           # Admin UI


─────────────────────────────────────────────
VERIFICATION QUESTIONS (answer without notes)
─────────────────────────────────────────────

  1. Why does Shield heartbeat to Connector :9091 and NOT directly to Controller?
     → Controller doesn't know individual Shield IPs (Shields are behind NAT).
       Connector is the network-edge relay. Connector batches Shield health into its own heartbeat.

  2. What makes an enrollment token single-use?
     → Redis: BurnEnrollmentJTI() does atomic GET+DEL. Second call gets nil → rejected.

  3. How does the Controller verify a cert renewal is legitimate?
     → The RenewCert request is sent over mTLS. Client cert IS the current cert.
       If the cert is valid and within the renewal window, it's genuine.
       Only the holder of the private key can establish that mTLS session.

  4. What is 100.64.0.0/10 and why?
     → IANA CGNAT range. Each Shield gets one /32 for its zecurity0 TUN interface.
       Not routable on public internet, unique per workspace.

  5. What happens if a Shield stops heartbeating?
     → Connector's alive_shields map still has the entry.
     → Connector still reports it in its heartbeat to Controller.
     → Controller's RunDisconnectWatcher detects last_heartbeat_at > 120s ago.
     → UPDATE shields SET status='disconnected'.

  6. What is SPIFFE and why does every cert carry a URI SAN?
     → SPIFFE = Secure Production Identity Framework for Everyone.
       URI SAN in X.509 cert = verifiable workload identity.
       During mTLS handshake, the peer cert's URI SAN is checked — no shared secrets needed.

  7. What does Connector's agent_server.rs return for Enroll?
     → Status::UNIMPLEMENTED. Shield always enrolls directly with Controller :9090.
       Connector only handles Heartbeat, RenewCert, and Goodbye for Shields.


─────────────────────────────────────────────────────────────────────
COMPLETE FILE REFERENCE — every file, every function, what it does
─────────────────────────────────────────────────────────────────────

Use this section as a lookup table. When you open any file, come here
first to know exactly what to expect inside it.

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
PROTO FILES
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

proto/connector/v1/connector.proto
  PURPOSE: Defines the gRPC contract between Connector ↔ Controller.
           This is the SOURCE OF TRUTH. All Go and Rust code is generated
           from or must match this file.
  CONTAINS:
    service ConnectorService
      rpc Enroll          → plain TLS, one-time: Connector sends JWT+CSR, gets cert back
      rpc Heartbeat       → mTLS, every 30s: Connector reports health + all Shield healths
      rpc RenewCert       → mTLS: Connector sends public key DER, gets new 7-day cert
      rpc Goodbye         → mTLS: Connector notifies Controller it is shutting down
    message EnrollRequest      (enrollment_token, csr_der, version, hostname)
    message EnrollResponse     (certificate_pem, workspace_ca_pem, intermediate_ca_pem, connector_id)
    message HeartbeatRequest   (connector_id, version, hostname, public_ip, shields[])
    message HeartbeatResponse  (ok, latest_version, re_enroll)
    message ShieldHealth       (shield_id, status, version, last_heartbeat_at)
    message RenewCertRequest   (connector_id, public_key_der)
    message RenewCertResponse  (certificate_pem, workspace_ca_pem, intermediate_ca_pem)
    message GoodbyeRequest     (connector_id)
    message GoodbyeResponse    (ok)
  BUILD CMD: buf generate  (from repo root → writes Go stubs to controller/gen/go/)

proto/shield/v1/shield.proto
  PURPOSE: Defines gRPC contract for Shield ↔ Controller (Enroll) and
           Shield ↔ Connector (Heartbeat, RenewCert, Goodbye).
           Identical structure to connector.proto but EnrollResponse has
           3 extra fields that drive network setup.
  CONTAINS:
    service ShieldService  (same 4 RPCs as ConnectorService)
    message EnrollResponse  EXTRA FIELDS vs connector:
      interface_addr  → "100.64.x.x/32" IP assigned to zecurity0 TUN
      connector_addr  → "{ip}:9091" where Shield must heartbeat
      connector_id    → UUID of the assigned Connector
    (all other messages mirror connector.proto)
  BUILD CMD: buf generate  (same command, same output directory)

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONTROLLER — MIGRATIONS
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

controller/migrations/001_schema.sql
  PURPOSE: Core tables — creates the foundation every other migration builds on.
  CREATES:
    workspaces     → one row per tenant (id, slug, name, status, trust_domain, ca_cert_pem)
    users          → one row per human (id, tenant_id, email, provider, role, status)
    ca_root        → single global Root CA (encrypted_private_key, nonce, certificate_pem)
    ca_intermediate → single global Intermediate CA (same shape as ca_root)
    workspace_ca_keys → per-tenant Workspace CA keys (encrypted, AES-GCM)
  RUN: applies automatically (or manually via psql -f)

controller/migrations/002_connector_schema.sql
  PURPOSE: Connector and network tables.
  CREATES:
    remote_networks  → grouping of connectors/shields (id, tenant_id, name, location, status)
    connectors       → one row per deployed Connector agent
      Key columns: status, enrollment_token_jti, trust_domain,
                   cert_serial, cert_not_after, last_heartbeat_at,
                   version, hostname, public_ip

controller/migrations/003_shield_schema.sql
  PURPOSE: Shield table.
  CREATES:
    shields  → one row per deployed Shield agent
      Key columns (same as connectors PLUS):
        connector_id   → FK to connectors (which Connector owns this Shield)
        interface_addr → INET, the /32 IP on zecurity0 (unique per workspace)

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONTROLLER — PKI
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

controller/internal/pki/service.go
  PURPOSE: Interface definition only. Everything calls pki.Service, not concrete types.
  CONTAINS:
    type Service interface
      SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, ttl) → *Certificate, error
      SignShieldCert(ctx, tenantID, shieldID, trustDomain, csr, ttl) → *Certificate, error
      GenerateControllerServerTLS(ctx, hosts []string, ttl) → *Certificate, error

controller/internal/pki/root.go
  PURPOSE: Manages the global Root CA — created once, stored encrypted in ca_root table.
  CONTAINS:
    initRootCA(ctx, pool, masterSecret) → loads existing or creates new Root CA
    createRootCA(ctx, pool, masterSecret) → generates self-signed X.509 (10yr, maxPathLen=2)
    loadRootCA(ctx, pool, masterSecret) → reads from DB, decrypts with AES-GCM

controller/internal/pki/intermediate.go
  PURPOSE: Manages the global Intermediate CA — signed by Root CA.
  CONTAINS:
    initIntermediateCA(ctx, pool, rootCA, masterSecret) → loads or creates
    createIntermediateCA(...) → signed by Root, 5yr, maxPathLen=1

controller/internal/pki/workspace.go
  PURPOSE: Per-tenant CA — created when a workspace is provisioned.
           This is what actually signs Connector and Shield leaf certs.
  CONTAINS:
    GetOrCreateWorkspaceCA(ctx, pool, tenantID, trustDomain, intermediateCA, masterSecret)
      → checks workspace_ca_keys table, creates 2yr CA if missing
    SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, ttl)
      → loads workspace CA from DB (decrypt AES-GCM)
      → validates CSR self-signature
      → sets SPIFFE URI SAN = "spiffe://{trustDomain}/connector/{connectorID}"
      → signs with workspace CA private key
      → returns PEM cert + CA chain
    SignShieldCert(ctx, tenantID, shieldID, trustDomain, csr, ttl)
      → same flow, SPIFFE SAN = "spiffe://{trustDomain}/shield/{shieldID}"

controller/internal/pki/crypto.go
  PURPOSE: Low-level cryptography helpers used by the CA files.
  CONTAINS:
    encryptKey(plaintext, masterSecret) → (ciphertext, nonce)  [AES-256-GCM]
    decryptKey(ciphertext, nonce, masterSecret) → plaintext
    pemToX509Cert(pem) → *x509.Certificate
    derToPem(der, blockType) → pemString
    pemToDer(pem, blockType) → []byte

controller/internal/pki/controller.go
  PURPOSE: Generates the ephemeral TLS cert for the Controller's gRPC server.
  CONTAINS:
    GenerateControllerServerTLS(ctx, hosts, ttl) → *Certificate
      → generates EC keypair, self-signs cert with SPIFFE SAN = SPIFFE_CONTROLLER_ID
      → used at startup to configure tls.Config for the gRPC listener

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONTROLLER — AUTH
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

controller/internal/auth/session.go
  PURPOSE: Manages user sessions — creates and parses JWTs.
  CONTAINS:
    CreateAccessToken(userID, workspaceID, tenantID, role, secret, ttl) → jwtString
      → HMAC-SHA256 signed, payload: { sub, workspace_id, tenant_id, role, jti, exp }
    VerifyAccessToken(tokenStr, secret) → claims, error
    CreateRefreshToken(userID, secret, ttl) → jwtString
    VerifyRefreshToken(tokenStr, secret) → claims, error

controller/internal/auth/exchange.go
  PURPOSE: Handles the OAuth authorization code → token exchange with Google.
  CONTAINS:
    ExchangeCode(ctx, code, redirectURI) → (idToken, refreshToken, error)
      → POST to Google's token endpoint, returns ID token + refresh token

controller/internal/auth/idtoken.go
  PURPOSE: Parses and verifies Google ID tokens.
  CONTAINS:
    ParseIDToken(ctx, tokenStr, clientID) → (email, googleSub, error)
      → verifies signature with Google's JWKS, extracts email + provider_sub

controller/internal/auth/refresh.go
  PURPOSE: HTTP handler for POST /auth/refresh.
  CONTAINS:
    RefreshHandler(secret string) → http.HandlerFunc
      → reads httpOnly refresh cookie
      → VerifyRefreshToken()
      → SELECT user WHERE id=claims.sub AND status='active'
      → CreateAccessToken() → return new JWT in response body
      → CreateRefreshToken() → set new httpOnly cookie

controller/internal/auth/callback.go  (or exchange.go)
  PURPOSE: HTTP handler for GET /auth/callback (OAuth redirect landing).
  CONTAINS:
    CallbackHandler(...) → http.HandlerFunc
      → extracts `code` query param
      → ExchangeCode() → Google tokens
      → ParseIDToken() → email, googleSub
      → upsert user row (INSERT ON CONFLICT UPDATE last_login_at)
      → CreateAccessToken() + CreateRefreshToken()
      → set httpOnly cookie, return access token in body

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONTROLLER — MIDDLEWARE
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

controller/internal/middleware/auth.go
  PURPOSE: HTTP middleware that validates JWT on every protected request.
  CONTAINS:
    AuthMiddleware(secret string) → func(http.Handler) http.Handler
      → reads Authorization: Bearer {token} header
      → VerifyAccessToken() → claims
      → injects userID, tenantID, role into request context
      → public operations (no header needed): checked by operation name

controller/internal/middleware/workspace.go
  PURPOSE: Ensures the requesting user belongs to the workspace in context.
  CONTAINS:
    WorkspaceGuard() → func(http.Handler) http.Handler
      → reads tenantID from context (set by AuthMiddleware)
      → SELECT workspace WHERE id=tenantID AND status='active'
      → injects workspace into context

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONTROLLER — APPMETA
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

controller/internal/appmeta/identity.go
  PURPOSE: Single source of truth for all identity constants in Go.
           Must be byte-for-byte identical to Rust appmeta files.
  CONTAINS:
    const ProductName                = "ZECURITY"
    const SPIFFEGlobalTrustDomain    = "zecurity.in"
    const SPIFFEControllerID         = "spiffe://zecurity.in/controller/global"
    const SPIFFETrustDomainPrefix    = "ws-"
    const SPIFFERoleConnector        = "connector"
    const SPIFFERoleShield           = "shield"
    const ShieldInterfaceName        = "zecurity0"
    const ShieldInterfaceCIDR        = "100.64.0.0/10"
    func WorkspaceTrustDomain(slug string) string
      → returns "ws-{slug}.zecurity.in"
    func ConnectorSPIFFEID(trustDomain, id string) string
      → returns "spiffe://{trustDomain}/connector/{id}"
    func ShieldSPIFFEID(trustDomain, id string) string
      → returns "spiffe://{trustDomain}/shield/{id}"

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONTROLLER — CONNECTOR SUBSYSTEM
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

controller/internal/connector/config.go
  PURPOSE: Loads Connector-related timeouts from env vars.
  CONTAINS:
    type Config struct
      CertTTL              time.Duration   (CONNECTOR_CERT_TTL, default 7d)
      EnrollmentTokenTTL   time.Duration   (default 24h)
      HeartbeatInterval    time.Duration   (default 30s)
      DisconnectThreshold  time.Duration   (default 90s)
      RenewalWindow        time.Duration   (default 48h)
    func NewConfig() Config  → reads env, sets defaults

controller/internal/connector/token.go
  PURPOSE: Creates and verifies one-time enrollment JWTs for Connectors.
  CONTAINS:
    GenerateEnrollmentToken(connectorID, workspaceID, trustDomain, ttl) → (tokenStr, jti, error)
      → builds JWT claims: { sub: connectorID, jti: uuid, workspace_id, trust_domain, iss, exp }
      → signs with HMAC-SHA256 using JWT_SECRET
      → stores jti in Redis: SET "connector:jti:{jti}" connectorID EX {ttl}
    VerifyEnrollmentToken(tokenStr) → (claims, error)
      → parses and verifies signature + expiry
    BurnEnrollmentJTI(ctx, redis, jti) → (connectorID, error)
      → Redis GET "connector:jti:{jti}" → if nil: already burned → error
      → Redis DEL "connector:jti:{jti}"
      → returns connectorID the JTI belonged to

controller/internal/connector/enrollment.go
  PURPOSE: gRPC handler for ConnectorService.Enroll RPC.
           The most critical function in the whole system — first contact.
  CONTAINS:
    type EnrollmentHandler struct { cfg Config, db *pgxpool.Pool, pki pki.Service, redis *redis.Client }
    func (h *EnrollmentHandler) Enroll(ctx, req) → (resp, error)
      Step 1: VerifyEnrollmentToken(req.EnrollmentToken) → claims
      Step 2: BurnEnrollmentJTI(redis, claims.JTI) → single-use enforcement
      Step 3: SELECT connectors WHERE id=claims.Sub AND status='pending'
      Step 4: Parse req.CsrDer → *x509.CertificateRequest
      Step 5: Verify CSR.CheckSignature() — proves Connector owns the private key
      Step 6: Extract SPIFFE URI SAN from CSR, assert == ConnectorSPIFFEID(trustDomain, id)
      Step 7: pki.SignConnectorCert(..., req.CsrDer, cfg.CertTTL) → *Certificate
      Step 8: UPDATE connectors SET status='active', cert_serial, cert_not_after,
                                    last_heartbeat_at=NOW(), version, hostname
      Step 9: return EnrollResponse{ CertificatePem, WorkspaceCaPem, IntermediateCaPem }

controller/internal/connector/heartbeat.go
  PURPOSE: gRPC handler for ConnectorService.Heartbeat RPC + disconnect watcher.
  CONTAINS:
    func Heartbeat(ctx, req) → (resp, error)
      → UPDATE connectors SET last_heartbeat_at=NOW(), version=req.Version,
                              hostname=req.Hostname, public_ip=req.PublicIp
      → for each s in req.Shields:
            shieldSvc.UpdateShieldHealth(ctx, s.ShieldId, s.Status, s.Version, s.LastHeartbeatAt)
      → load connector.cert_not_after from DB
      → if cert_not_after - now < cfg.RenewalWindow → reEnroll = true
      → return HeartbeatResponse{ Ok: true, ReEnroll: reEnroll }
    func RunDisconnectWatcher(ctx, pool, cfg)
      → goroutine, loops every 60s
      → UPDATE connectors SET status='disconnected'
        WHERE status='active' AND last_heartbeat_at < NOW() - cfg.DisconnectThreshold

controller/internal/connector/goodbye.go
  PURPOSE: gRPC handler for ConnectorService.Goodbye RPC.
  CONTAINS:
    func Goodbye(ctx, req) → (resp, error)
      → UPDATE connectors SET status='disconnected', last_heartbeat_at=NOW()
        WHERE id=req.ConnectorId
      → return GoodbyeResponse{ Ok: true }

controller/internal/connector/spiffe.go
  PURPOSE: gRPC unary interceptor — verifies every mTLS call carries a valid SPIFFE cert.
  CONTAINS:
    func UnarySPIFFEInterceptor(ctx, req, info, handler) → (resp, error)
      → skips Enroll (no client cert on enrollment)
      → reads peer TLS info from ctx
      → extracts URI SAN from client cert
      → verifies it has format "spiffe://{workspaceTrustDomain}/connector/{id}"
      → verifies trust domain matches workspace in JWT context
      → calls handler() if all checks pass

controller/internal/connector/ca_endpoint.go
  PURPOSE: HTTP handler for GET /ca.crt — serves the Workspace CA cert.
  CONTAINS:
    CACertHandler(pool) → http.HandlerFunc
      → extracts workspace slug from query param or subdomain
      → SELECT ca_cert_pem FROM workspaces WHERE slug=$1
      → responds with PEM bytes, Content-Type: application/x-pem-file

controller/internal/connector/token_handler.go
  PURPOSE: HTTP handler for regenerating a new enrollment token for an existing Connector.
  CONTAINS:
    RegenerateTokenHandler(pool, redis, cfg) → http.HandlerFunc
      → POST /api/connectors/{id}/token
      → protected by JWT middleware
      → SELECT connector WHERE id=$1 AND tenant_id=$2
      → GenerateEnrollmentToken(connectorID, ...) → new JWT
      → UPDATE connectors SET enrollment_token_jti=$1
      → return new install command

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONTROLLER — SHIELD SUBSYSTEM
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

controller/internal/shield/config.go
  PURPOSE: Loads Shield-related timeouts from env vars.
  CONTAINS:  (mirrors connector/config.go, different env var names)
    type Config struct
      CertTTL, EnrollmentTokenTTL, DisconnectThreshold (120s), RenewalWindow
    func NewConfig() Config

controller/internal/shield/token.go
  PURPOSE: Creates the one-time enrollment JWT for Shields.
           Also handles Connector selection and IP assignment.
  CONTAINS:
    func GenerateShieldToken(ctx, pool, redis, cfg, remoteNetworkID, name, workspaceID, trustDomain)
      → (shieldID string, tokenStr string, error)
    func selectConnector(ctx, pool, tenantID, remoteNetworkID) → (connectorID, connectorIP, error)
      → SQL: SELECT id, public_ip FROM connectors
             WHERE tenant_id=$1 AND remote_network_id=$2 AND status='active'
             ORDER BY (SELECT COUNT(*) FROM shields WHERE connector_id=connectors.id
                       AND status='active') ASC
             LIMIT 1
      → picks the Connector with fewest active Shields (load balancing)
    func assignInterfaceAddr(ctx, pool, tenantID) → (addr string, error)
      → iterates 100.64.0.0/10 as /32 addresses
      → for each: SELECT 1 FROM shields WHERE interface_addr=$1 AND tenant_id=$2
      → returns first address with no existing row

controller/internal/shield/enrollment.go
  PURPOSE: gRPC handler for ShieldService.Enroll RPC.
  CONTAINS:
    type service struct { cfg Config, db *pgxpool.Pool, pki pki.Service, redis *redis.Client }
    func (s *service) Enroll(ctx, req) → (resp, error)
      Step 1:  VerifyShieldToken(req.EnrollmentToken) → claims
      Step 2:  BurnShieldJTI(redis, claims.JTI)
      Step 3:  SELECT shield WHERE id=claims.Sub AND status='pending'
      Step 4:  SELECT connector WHERE id=claims.ConnectorID AND status='active'
      Step 5:  Verify workspace trust domain matches claims
      Step 6:  Parse + verify CSR self-signature
      Step 7:  Verify SPIFFE URI SAN == ShieldSPIFFEID(trustDomain, shieldID)
      Step 8:  pki.SignShieldCert(...) → 7-day X.509 cert
      Step 9:  UPDATE shields SET status='active', cert_serial, cert_not_after,
                                  last_heartbeat_at=NOW()
      Step 10: load connector.public_ip
      Step 11: return EnrollResponse{
                 CertificatePem, WorkspaceCaPem, IntermediateCaPem,
                 InterfaceAddr: shield.InterfaceAddr,   (100.64.x.x/32)
                 ConnectorAddr: connectorIP + ":9091",
                 ConnectorId:   connector.ID,
               }

controller/internal/shield/heartbeat.go
  PURPOSE: Updates shield health in DB + disconnect watcher goroutine.
  CONTAINS:
    func UpdateShieldHealth(ctx, pool, shieldID, status, version string, lastHeartbeatAt int64)
      → UPDATE shields SET status=$2, version=$3, last_heartbeat_at=$4 WHERE id=$1
    func RunDisconnectWatcher(ctx, pool, cfg)
      → goroutine, loops every 60s
      → UPDATE shields SET status='disconnected'
        WHERE status='active' AND last_heartbeat_at < NOW() - cfg.DisconnectThreshold (120s)

controller/internal/shield/spiffe.go
  PURPOSE: SPIFFE verification for Shield mTLS calls (mirrors connector/spiffe.go).
  CONTAINS:
    func UnarySPIFFEInterceptor(...)
      → same pattern as connector, but checks "spiffe://{domain}/shield/{id}" format

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONTROLLER — GRAPHQL SCHEMA
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

controller/graph/schema.graphqls
  PURPOSE: Root Query and Mutation types — the top-level GraphQL entry points.
  DEFINES:
    type Query  { me, workspace, remoteNetworks, connectors, shields, lookupWorkspace,
                  lookupWorkspacesByEmail }
    type Mutation { initiateAuth, createRemoteNetwork, deleteRemoteNetwork,
                    generateConnectorToken, revokeConnector, deleteConnector,
                    generateShieldToken, revokeShield, deleteShield }
    scalar types, common enums

controller/graph/connector.graphqls
  PURPOSE: Types for remote networks and connectors.
  DEFINES:
    type RemoteNetwork  { id, name, location, status, networkHealth, connectors, shields }
    type Connector      { id, name, status, hostname, publicIp, certNotAfter,
                          lastHeartbeatAt, version, remoteNetworkId }
    enum NetworkHealth  { ONLINE, DEGRADED, OFFLINE }
    input CreateRemoteNetworkInput
    type ConnectorTokenPayload  { connectorId, installCommand }

controller/graph/shield.graphqls
  PURPOSE: Types for shields.
  DEFINES:
    type Shield         { id, name, status, interfaceAddr, connectorId,
                          lastHeartbeatAt, version, hostname, publicIp }
    type ShieldTokenPayload  { shieldId, installCommand }
    input GenerateShieldTokenInput

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONTROLLER — GRAPHQL RESOLVERS
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

controller/graph/resolvers/resolver.go
  PURPOSE: Defines the Resolver struct that all resolver methods hang off.
  CONTAINS:
    type Resolver struct
      TenantDB     *db.TenantDB
      AuthService  auth.Service
      ConnectorCfg connector.Config
      ShieldCfg    shield.Config
      Redis        *redis.Client
      Pool         *pgxpool.Pool

controller/graph/resolvers/schema.resolvers.go
  PURPOSE: Resolvers for auth, user, and workspace queries.
  CONTAINS:
    func (r *queryResolver) Me(ctx) → *User
      → reads userID from context (set by authMiddleware)
      → SELECT id, email, role FROM users WHERE id=$1
    func (r *queryResolver) Workspace(ctx) → *Workspace
      → reads tenantID from context
      → SELECT name, slug, status FROM workspaces WHERE id=$1
    func (r *mutationResolver) InitiateAuth(ctx, provider, workspaceName) → *AuthPayload
      → builds Google OAuth URL with state param
      → returns { redirectUrl }
    func (r *queryResolver) LookupWorkspace(ctx, slug) → *WorkspaceLookup
      → SELECT 1 FROM workspaces WHERE slug=$1 (public, no JWT)
    func (r *queryResolver) LookupWorkspacesByEmail(ctx, email) → []*Workspace
      → SELECT w.name, w.slug FROM workspaces w
        JOIN users u ON u.tenant_id = w.id WHERE u.email=$1

controller/graph/resolvers/connector.resolvers.go
  PURPOSE: Resolvers for remote networks and connectors.
  CONTAINS:
    func CreateRemoteNetwork(ctx, input) → *RemoteNetwork
      → INSERT remote_networks (tenant_id, name, location, status='active')
    func DeleteRemoteNetwork(ctx, id) → bool
      → UPDATE remote_networks SET status='deleted' WHERE id=$1 AND tenant_id=$2
    func GenerateConnectorToken(ctx, input) → *ConnectorTokenPayload
      → connector.GenerateEnrollmentToken(...) → jwtString
      → builds install command string
      → return { connectorId, installCommand }
    func Connectors(ctx, remoteNetworkId) → []*Connector
      → SELECT * FROM connectors WHERE tenant_id=$1 AND remote_network_id=$2
    func RevokeConnector(ctx, id) → *Connector
      → UPDATE connectors SET status='revoked' WHERE id=$1
    func DeleteConnector(ctx, id) → bool
      → DELETE FROM connectors WHERE id=$1 AND status='pending'
    NetworkHealth computation (inside RemoteNetwork resolver):
      all active → ONLINE
      mixed      → DEGRADED
      all disconnected/revoked → OFFLINE

controller/graph/resolvers/shield.resolvers.go
  PURPOSE: Resolvers for shields.
  CONTAINS:
    func GenerateShieldToken(ctx, input) → *ShieldTokenPayload
      → shield.GenerateShieldToken(...) → (shieldID, jwtString)
      → builds install command
      → return { shieldId, installCommand }
    func Shields(ctx, remoteNetworkId) → []*Shield
      → SELECT * FROM shields WHERE tenant_id=$1 AND remote_network_id=$2
    func RevokeShield(ctx, id) → *Shield
      → UPDATE shields SET status='revoked' WHERE id=$1
    func DeleteShield(ctx, id) → bool
      → DELETE FROM shields WHERE id=$1 AND status='pending'

controller/graph/resolvers/helpers.go
  PURPOSE: Shared utilities used across multiple resolver files.
  CONTAINS:
    extractTenantID(ctx) → string
    extractUserID(ctx) → string
    toShieldGQL(dbShield) → *gqlmodel.Shield   (DB row → GraphQL type)
    toConnectorGQL(dbConn) → *gqlmodel.Connector

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONTROLLER — ENTRY POINT
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

controller/cmd/server/main.go
  PURPOSE: Wires every subsystem together and starts HTTP + gRPC servers.
           Reading this file last makes everything else click into place.
  CONTAINS:
    func main()
      → load .env (optional)
      → db.Init(DATABASE_URL) → *pgxpool.Pool
      → pki.Init(ctx, pool, PKI_MASTER_SECRET) → pki.Service
      → auth.NewService(GOOGLE_CLIENT_ID, SECRET, REDIRECT_URI)
      → connector.NewConfig() + shield.NewConfig()
      → redis.NewClient(REDIS_URL)
      → build gqlgen schema: NewSchema(&Resolver{...})
      → build chi router, register middleware:
            POST /graphql      ← authMiddleware + workspaceGuard + gqlgen handler
            GET  /auth/callback ← auth.CallbackHandler
            POST /auth/refresh  ← auth.RefreshHandler
            GET  /ca.crt        ← connector.CACertHandler (public)
            GET  /health        ← 200 OK
            POST /api/connectors/{id}/token ← connector.RegenerateTokenHandler
      → pki.GenerateControllerServerTLS() → TLS config for gRPC
      → grpc.NewServer(grpc.Creds(tlsCreds), grpc.UnaryInterceptor(SPIFFEInterceptor))
      → pb.RegisterConnectorServiceServer(grpcServer, enrollmentHandler)
      → pb.RegisterShieldServiceServer(grpcServer, shieldService)
      → go connector.RunDisconnectWatcher(ctx, pool, connCfg)
      → go shield.RunDisconnectWatcher(ctx, pool, shieldCfg)
      → go grpcServer.Serve(grpcListener)   ← :9090
      → httpServer.ListenAndServe()          ← :8080
  BUILD CMD: cd controller && go build ./...

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
CONNECTOR RUST — ALL FILES
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

connector/build.rs
  PURPOSE: Runs at `cargo build` time — compiles proto files into Rust code.
  CONTAINS:
    fn main()
      → tonic_build::configure().compile(
            ["../proto/connector/v1/connector.proto",
             "../proto/shield/v1/shield.proto"], ["../proto"])
      → output goes to target/build/.../out/ and included via include_proto!()
  BUILD CMD: runs automatically on `cargo build`

connector/src/appmeta.rs
  PURPOSE: Rust mirror of controller/internal/appmeta/identity.go.
           Must be byte-for-byte identical to Go constants.
  CONTAINS:
    pub const PRODUCT_NAME: &str = "ZECURITY"
    pub const SPIFFE_GLOBAL_TRUST_DOMAIN: &str = "zecurity.in"
    pub const SPIFFE_CONTROLLER_ID: &str = "spiffe://zecurity.in/controller/global"
    pub const SPIFFE_ROLE_CONNECTOR: &str = "connector"
    pub const SPIFFE_ROLE_SHIELD: &str = "shield"
    pub fn connector_spiffe_id(trust_domain, id) → String
    pub fn shield_spiffe_id(trust_domain, id) → String

connector/src/config.rs
  PURPOSE: Loads all runtime configuration from env vars + TOML file.
  CONTAINS:
    pub struct ConnectorConfig
      controller_addr: String          (gRPC endpoint)
      controller_http_addr: String     (HTTP for /ca.crt)
      enrollment_token: String         (one-time JWT)
      state_dir: String                (/var/lib/zecurity-connector)
      auto_update_enabled: bool
      log_level: String
    pub fn load() → Result<ConnectorConfig>
      → figment::Figment::new()
            .merge(Toml::file("/etc/zecurity/connector.conf"))
            .merge(Env::raw())
            .extract()

connector/src/crypto.rs
  PURPOSE: All cryptographic operations — key generation and CSR building.
  CONTAINS:
    pub fn generate_ec_p384_keypair() → (EcKey<Private>, EcKey<Public>)
      → openssl or rcgen: generates EC P-384 keypair in memory
      → private key written to disk, NEVER sent over wire
    pub fn build_csr(private_key, spiffe_id: &str) → CertificateSigningRequest
      → rcgen::CertificateParams with URI SAN = spiffe_id
      → self-signs CSR (proves key ownership to Controller)
      → returns DER bytes for EnrollRequest.csr_der
    pub fn public_key_to_der(key) → Vec<u8>
      → extracts public key bytes from private key
      → used in RenewCertRequest (only public key sent, not private)

connector/src/tls.rs
  PURPOSE: Verifies Controller's SPIFFE identity during TLS handshake.
  CONTAINS:
    pub fn verify_controller_spiffe(cert: &X509, trust_domain: &str) → Result<()>
      → extracts SubjectAltName URI extensions from cert
      → asserts one of them == SPIFFE_CONTROLLER_ID
      → if mismatch → Err("not the expected controller")
      → called after every new TLS connection to Controller is established

connector/src/enrollment.rs
  PURPOSE: Full enrollment flow — runs once when no state.json exists.
  CONTAINS:
    pub async fn enroll(cfg: &ConnectorConfig) → Result<EnrollmentState>
      1. reqwest::get("{controller_http_addr}/ca.crt") → ca_pem bytes
      2. SHA-256(ca_der) → compare with JWT claim ca_fingerprint
      3. generate_ec_p384_keypair() → (priv_key, pub_key)
      4. decode_jwt_claims(cfg.enrollment_token) → { connector_id, trust_domain, workspace_id }
      5. build_csr(priv_key, connector_spiffe_id(trust_domain, connector_id))
      6. tonic::transport::Channel::from_shared(cfg.controller_addr)
             .tls_config(plain_tls_with_ca(ca_pem))
             .connect()
      7. ConnectorServiceClient::new(channel)
             .enroll(EnrollRequest { enrollment_token, csr_der, version, hostname })
      8. fs::write(state_dir/connector.crt, resp.certificate_pem)
         fs::write(state_dir/connector.key, priv_key_pem)
         fs::write(state_dir/workspace_ca.crt, resp.workspace_ca_pem)
      9. serde_json::to_file(state_dir/state.json, EnrollmentState { connector_id, trust_domain, ... })
      10. return Ok(EnrollmentState)

connector/src/heartbeat.rs
  PURPOSE: The main heartbeat loop — keeps Connector alive with Controller.
  CONTAINS:
    pub async fn run_heartbeat(cfg, state: EnrollmentState, shield_server: ShieldServer)
      1. build_mtls_channel(cfg.controller_addr, state_dir/connector.crt+key, workspace_ca.crt)
      2. verify_controller_spiffe(peer_cert, state.trust_domain)
      3. loop:
           shields = shield_server.get_alive_shields()
           match client.heartbeat(HeartbeatRequest { connector_id, version, hostname,
                                                      public_ip, shields }).await
             Ok(resp) →
               backoff.reset()
               if resp.re_enroll → renewal::renew_cert(&state, &cfg) → rebuild channel
             Err(e) →
               log error
               sleep(backoff.next())   (5s → 10s → 20s → ... → 60s cap)
         sleep(cfg.heartbeat_interval_secs)

connector/src/renewal.rs
  PURPOSE: Certificate renewal — called when Controller says re_enroll=true.
  CONTAINS:
    pub async fn renew_cert(state: &EnrollmentState, cfg: &ConnectorConfig) → Result<EnrollmentState>
      1. fs::read(state_dir/connector.key) → private key
      2. public_key_to_der(key) → public key bytes only
      3. mTLS channel (using current cert)
      4. ConnectorServiceClient.renew_cert(RenewCertRequest { connector_id, public_key_der })
      5. fs::write(state_dir/connector.crt, resp.certificate_pem)
      6. update state.json with new cert_not_after
      7. return updated EnrollmentState

connector/src/agent_server.rs
  PURPOSE: Runs a gRPC server on :9091 that Shields heartbeat into.
           Acts as the relay between Shields and Controller.
  CONTAINS:
    pub struct ShieldServer
      alive_shields: Arc<Mutex<HashMap<String, ShieldHealth>>>
      controller_channel: Channel      (mTLS to Controller :9090)
      trust_domain: String
      connector_id: String
    pub fn new(controller_channel, trust_domain, connector_id) → ShieldServer
    pub async fn serve(self, addr: &str, state_dir: &str) → Result<()>
      → tonic::transport::Server::builder()
             .add_service(ShieldServiceServer::new(self))
             .serve(addr)
    pub fn get_alive_shields(&self) → Vec<ShieldHealth>
      → reads alive_shields map snapshot for heartbeat loop

    impl ShieldService for ShieldServer:
      async fn heartbeat(req: HeartbeatRequest) → HeartbeatResponse
        → lock alive_shields
        → insert/update entry for req.shield_id with { status, version, last_heartbeat_at: now }
        → check cert expiry from stored cert_not_after
        → return HeartbeatResponse { ok: true, re_enroll }
      async fn renew_cert(req: RenewCertRequest) → RenewCertResponse
        → ShieldServiceClient::new(self.controller_channel.clone())
               .renew_cert(req)     ← proxy, no modification
      async fn goodbye(req: GoodbyeRequest) → GoodbyeResponse
        → lock alive_shields → remove req.shield_id
        → return GoodbyeResponse { ok: true }
      async fn enroll(req: EnrollRequest) → EnrollResponse
        → return Err(Status::unimplemented("shield enrolls directly with controller"))

connector/src/updater.rs
  PURPOSE: Weekly self-update check — downloads newer binary from GitHub Releases.
  CONTAINS:
    pub async fn run_update_loop(cfg) → never returns (infinite loop)
      → loop: sleep until next weekly check → run_single_check() → sleep again
    pub async fn run_single_check() → Result<()>
      → GET https://api.github.com/repos/vairabarath/zecurity/releases/latest
      → compare tag version with current CARGO_PKG_VERSION
      → if newer:
            download binary asset for current arch
            download checksums.txt
            SHA-256 verify binary against checksums.txt
            fs::rename(tmp_binary, /usr/local/bin/zecurity-connector)  (atomic)
            exec new binary (replace current process)
  CALLED BY: main.rs --check-update flag (systemd oneshot timer)
             main.rs auto_update_enabled (background loop)

connector/src/util.rs
  PURPOSE: Small utility functions.
  CONTAINS:
    pub fn read_hostname() → String
      → gethostname::gethostname().to_string_lossy().to_string()

connector/src/main.rs
  PURPOSE: Entry point — wires everything together and manages process lifecycle.
  CONTAINS:
    #[tokio::main] async fn main()
      → rustls::crypto::ring::default_provider().install_default()
           (required: rustls 0.23+ needs an explicit crypto provider)
      → parse std::env::args: "--check-update" → updater::run_single_check() → std::process::exit(0)
      → let cfg = config::load()
      → tracing_subscriber::init() with cfg.log_level
      → let state = if state.json exists
                       → load EnrollmentState from JSON
                    else
                       → enrollment::enroll(&cfg).await
      → build controller mTLS channel
      → let shield_srv = ShieldServer::new(channel, trust_domain, connector_id)
      → tokio::spawn(shield_srv.serve("0.0.0.0:9091", &cfg.state_dir))
      → tokio::spawn(heartbeat::run_heartbeat(cfg.clone(), state, shield_srv.clone()))
      → if cfg.auto_update_enabled:
              tokio::spawn(updater::run_update_loop(cfg.clone()))
      → tokio::signal::ctrl_c().await  (blocks until SIGTERM/Ctrl-C)
      → ConnectorServiceClient.goodbye(GoodbyeRequest { connector_id }) → Controller
      → process exits
  BUILD CMD: cd connector && cargo build
             cd connector && cargo build --release

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
SHIELD RUST — ALL FILES
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

shield/build.rs
  PURPOSE: Compiles shield.proto into Rust at build time.
  CONTAINS:
    fn main()
      → tonic_build::compile_protos("../proto/shield/v1/shield.proto")
  BUILD CMD: runs automatically on `cargo build --manifest-path shield/Cargo.toml`

shield/src/appmeta.rs
  PURPOSE: Rust mirror of appmeta/identity.go — must match Go and Connector exactly.
  CONTAINS: (same constants as connector/src/appmeta.rs PLUS)
    pub const SHIELD_INTERFACE_NAME: &str = "zecurity0"
    pub const SHIELD_INTERFACE_CIDR_RANGE: &str = "100.64.0.0/10"
    pub const PKI_SHIELD_CN_PREFIX: &str = "shield-"

shield/src/config.rs
  PURPOSE: Loads Shield runtime config.
  CONTAINS:
    pub struct ShieldConfig
      controller_addr: String
      controller_http_addr: String
      enrollment_token: String
      state_dir: String                (/var/lib/zecurity-shield)
      shield_heartbeat_interval_secs: u64   (default 60)
      auto_update_enabled: bool
      log_level: String
    pub fn load() → Result<ShieldConfig>
      → figment merge of /etc/zecurity/shield.env + env vars

shield/src/types.rs
  PURPOSE: Core data types — especially ShieldState (what persists to disk).
  CONTAINS:
    #[derive(Serialize, Deserialize)]
    pub struct ShieldState
      shield_id: String
      trust_domain: String
      connector_id: String
      connector_addr: String         ("{connector_ip}:9091")
      interface_addr: String         ("100.64.x.x/32")
      enrolled_at: String            (RFC 3339)
      cert_not_after: i64            (Unix timestamp)
    impl ShieldState
      pub fn load(state_dir: &str) → Result<ShieldState>
        → fs::read_to_string(state_dir/state.json) → serde_json::from_str()
      pub fn save(&self, state_dir: &str) → Result<()>
        → serde_json::to_string_pretty(self) → fs::write(state.json)

shield/src/crypto.rs
  PURPOSE: Identical to connector/src/crypto.rs — EC P-384 keygen and CSR.
  CONTAINS:  (same 3 functions — see connector/src/crypto.rs above)

shield/src/tls.rs
  PURPOSE: Verifies Connector's SPIFFE identity — Shield-side TLS check.
  CONTAINS:
    pub fn verify_connector_spiffe(cert, trust_domain, connector_id) → Result<()>
      → extracts URI SAN from peer cert during TLS handshake
      → asserts URI SAN == connector_spiffe_id(trust_domain, connector_id)
      → stricter than connector: checks SPECIFIC connector_id, not just format
      → if mismatch → Err("not the expected connector")

shield/src/enrollment.rs
  PURPOSE: Full Shield enrollment — runs once on first start.
  CONTAINS:
    pub async fn enroll(cfg: &ShieldConfig) → Result<ShieldState>
      1. GET {controller_http_addr}/ca.crt
      2. verify SHA-256 fingerprint vs JWT claim
      3. generate_ec_p384_keypair()
      4. decode JWT: shield_id, trust_domain, connector_id, interface_addr
      5. build_csr(priv_key, shield_spiffe_id(trust_domain, shield_id))
      6. plain TLS channel to cfg.controller_addr
      7. ShieldServiceClient.enroll(EnrollRequest { enrollment_token, csr_der, version, hostname })
      8. fs::write(state_dir/shield.crt + shield.key + workspace_ca.crt)
      9. network::setup(resp.interface_addr, resp.connector_addr).await
           ← THIS IS THE UNIQUE STEP vs Connector enrollment
      10. let state = ShieldState { shield_id, trust_domain, connector_id,
                connector_addr: resp.connector_addr,
                interface_addr: resp.interface_addr,
                cert_not_after: parse_cert_not_after(resp.certificate_pem) }
      11. state.save(cfg.state_dir)
      12. return Ok(state)

shield/src/network.rs
  PURPOSE: Kernel-level network setup — creates the TUN interface and firewall rules.
           Requires CAP_NET_ADMIN + CAP_NET_RAW. Unique to Shield.
  CONTAINS:
    pub async fn setup(interface_addr: &str, connector_addr: &str) → Result<()>
      TUN INTERFACE (via rtnetlink crate):
        1. let (conn, handle, _) = rtnetlink::new_connection()?
        2. handle.link().add()
               .name("zecurity0")
               .kind(LinkKind::Tun)
               .execute().await
        3. let if_index = get_link_index(&handle, "zecurity0").await
        4. parse interface_addr into (IpAddr, prefix_len)   e.g. 100.64.0.5, 32
        5. handle.address().add(if_index, ip_addr, 32).execute().await
        6. handle.link().set(if_index).up().execute().await
      NFTABLES FIREWALL (via nftables crate):
        7. let mut batch = nftables::batch::Batch::new()
        8. batch.add(Table::new(TableFamily::Inet, "zecurity"))
        9. batch.add(Chain::new("zecurity", "input",
               Some(ChainType::Filter), Some(ChainHook::Input),
               Some(0), Some(ChainPolicy::Drop)))
        10. extract connector_ip from connector_addr (strip :9091)
        11. batch.add(Rule::new("zecurity", "input", "iif lo accept"))
            batch.add(Rule::new("zecurity", "input",
               format!("ip saddr {} accept", connector_ip)))
            batch.add(Rule::new("zecurity", "input", "drop"))
        12. nftables::helper::apply_ruleset(&batch)
    WHY THIS MATTERS: After setup(), the zecurity0 interface exists with:
      - Assigned /32 IP (e.g. 100.64.0.5)
      - nftables base DROP — only Connector's IP can reach this Shield

shield/src/heartbeat.rs
  PURPOSE: Heartbeat loop to Connector :9091 (NOT Controller directly).
  CONTAINS:
    pub async fn run(state: ShieldState, cfg: ShieldConfig) → Result<()>
      1. build_mtls_channel(state.connector_addr, shield.crt+key, workspace_ca.crt)
      2. verify_connector_spiffe(peer_cert, trust_domain, connector_id)
      3. loop every cfg.shield_heartbeat_interval_secs (60s):
           match client.heartbeat(HeartbeatRequest { shield_id, version, hostname, public_ip })
             Ok(resp) →
               if resp.re_enroll → renewal::renew_cert(&state, &cfg) → rebuild channel
             Err(e) → exponential backoff (5s→10s→20s→60s cap)
    pub async fn goodbye(state: &ShieldState, cfg: &ShieldConfig)
      → best-effort, called on SIGTERM
      → ShieldServiceClient.goodbye(GoodbyeRequest { shield_id }) → Connector :9091
      → Connector removes Shield from alive_shields map immediately
      → if network is down: silently ignores error (disconnect watcher handles it)

shield/src/renewal.rs
  PURPOSE: Cert renewal via Connector proxy (not direct to Controller).
  CONTAINS:
    pub async fn renew_cert(state: &ShieldState, cfg: &ShieldConfig) → Result<ShieldState>
      1. fs::read(state_dir/shield.key)
      2. public_key_to_der(key)
      3. mTLS channel to state.connector_addr (:9091)
      4. ShieldServiceClient.renew_cert(RenewCertRequest { shield_id, public_key_der })
           ← connector agent_server.rs receives this and proxies to Controller :9090
      5. fs::write(state_dir/shield.crt, resp.certificate_pem)
      6. state.cert_not_after = parse_cert_not_after(resp.certificate_pem)
      7. state.save(cfg.state_dir)
      8. return Ok(updated_state)

shield/src/updater.rs
  PURPOSE: Weekly self-update for shield binary from GitHub Releases.
  CONTAINS: (identical logic to connector/src/updater.rs — same 2 functions)
    pub async fn run_update_loop(cfg) → never returns
    pub async fn run_single_check() → Result<()>
  TAG FORMAT: shield-v* (separate from connector-v* tags)

shield/src/util.rs
  PURPOSE: Small utilities.
  CONTAINS:
    pub fn read_hostname() → String
    pub fn parse_cert_not_after(cert_pem: &str) → i64   (Unix timestamp)

shield/src/main.rs
  PURPOSE: Shield entry point — manages full lifecycle.
  CONTAINS:
    #[tokio::main] async fn main()
      → rustls::crypto::ring::default_provider().install_default()
      → "--check-update" → updater::run_single_check() → exit
      → let cfg = config::load()
      → tracing_subscriber::init()
      → let state = if state.json exists
                       → ShieldState::load(&cfg.state_dir)
                    else
                       → enrollment::enroll(&cfg).await
      → tokio::spawn(heartbeat::run(state.clone(), cfg.clone()))
      → if cfg.auto_update_enabled:
              tokio::spawn(updater::run_update_loop(cfg.clone()))
      → select!
            _ = signal::ctrl_c() => {
                  heartbeat::goodbye(&state, &cfg).await;
                  // process exits
            }
  BUILD CMD: cargo build --manifest-path shield/Cargo.toml
             cargo build --release --manifest-path shield/Cargo.toml

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
SHIELD — SYSTEMD + INSTALL
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

shield/systemd/zecurity-shield.service
  PURPOSE: Runs the Shield binary as a systemd service.
  KEY SETTINGS:
    Type=simple
    EnvironmentFile=/etc/zecurity/shield.env     (where ENROLLMENT_TOKEN lives)
    ExecStart=/usr/local/bin/zecurity-shield
    AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW
      ↑ CAP_NET_ADMIN needed for rtnetlink (create TUN interface)
      ↑ CAP_NET_RAW needed for nftables writes
    Restart=on-failure
    RestartSec=5s

shield/systemd/zecurity-shield-update.timer
  PURPOSE: Triggers the update check once per week.
  KEY SETTINGS:
    OnCalendar=weekly
    Unit=zecurity-shield-update.service

shield/systemd/zecurity-shield-update.service
  PURPOSE: Runs `zecurity-shield --check-update` as a oneshot.
  KEY SETTINGS:
    Type=oneshot
    ExecStart=/usr/local/bin/zecurity-shield --check-update

shield/scripts/shield-install.sh
  PURPOSE: One-liner installer script — what users run via curl | bash.
  DOES:
    1. Detect arch (amd64 or arm64)
    2. Download zecurity-shield binary from GitHub Releases
    3. Verify SHA-256 against checksums.txt
    4. mv binary to /usr/local/bin/zecurity-shield
    5. Create user/group zecurity-shield
    6. mkdir /etc/zecurity /var/lib/zecurity-shield
    7. Write /etc/zecurity/shield.env with ENROLLMENT_TOKEN + other vars
    8. Install systemd units to /etc/systemd/system/
    9. systemctl daemon-reload
    10. systemctl enable --now zecurity-shield
  CALLED VIA: curl -fsSL .../shield-install.sh | ENROLLMENT_TOKEN=<jwt> bash

.github/workflows/shield-release.yml
  PURPOSE: CI pipeline — builds and publishes Shield binaries on tag push.
  TRIGGERS ON: git push tag matching shield-v*
  STEPS:
    1. checkout
    2. install cross-rs (cross-compilation tool)
    3. cross build --release --target x86_64-unknown-linux-musl
    4. cross build --release --target aarch64-unknown-linux-musl
    5. generate checksums.txt (sha256sum of both binaries)
    6. gh release create shield-v* with:
         zecurity-shield-linux-amd64
         zecurity-shield-linux-arm64
         checksums.txt
         shield-install.sh
         systemd unit files

━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━
ADMIN UI — ALL FILES
━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━

admin/src/main.tsx
  PURPOSE: React application entry point.
  DOES:
    ReactDOM.createRoot(document.getElementById('root'))
      .render(
        <ApolloProvider client={apolloClient}>
          <BrowserRouter>
            <App />
          </BrowserRouter>
        </ApolloProvider>
      )

admin/src/App.tsx
  PURPOSE: Defines all application routes.
  ROUTES:
    Public:
      /login                → Login.tsx
      /auth/callback        → AuthCallback.tsx
      /signup               → Step1Email.tsx
      /signup/workspace     → Step2Workspace.tsx
      /signup/auth          → Step3Auth.tsx
    Protected (wrapped in ProtectedLayout):
      /dashboard            → Dashboard.tsx
      /remote-networks      → RemoteNetworks.tsx
      /remote-networks/:id/connectors → Connectors.tsx
      /remote-networks/:id/shields    → Shields.tsx
      /connectors           → AllConnectors.tsx
      /connectors/:id       → ConnectorDetail.tsx
      /settings             → Settings.tsx

admin/src/apollo/client.ts
  PURPOSE: Creates and exports the Apollo GraphQL client.
  CONTAINS:
    errorLink   → catches GraphQL errors, handles 401 → refresh → retry
    authLink    → attaches Authorization: Bearer {token} to all non-public ops
    httpLink    → points to /graphql
    apolloClient = new ApolloClient({
      link: from([errorLink, authLink, httpLink]),
      cache: new InMemoryCache(),
      defaultOptions: { query: { fetchPolicy: 'cache-and-network' } }
    })

admin/src/apollo/links/auth.ts
  PURPOSE: Attaches JWT to outgoing requests.
  CONTAINS:
    authLink = new ApolloLink((operation, forward) => {
      const PUBLIC_OPS = ['InitiateAuth', 'LookupWorkspace', 'LookupWorkspacesByEmail']
      if PUBLIC_OPS.includes(operation.operationName)) → skip auth header
      else → set headers: { Authorization: `Bearer ${useAuthStore.getState().accessToken}` }
      return forward(operation)
    })

admin/src/apollo/links/error.ts
  PURPOSE: Handles auth errors from the server.
  CONTAINS:
    errorLink = onError(({ graphQLErrors, operation, forward }) => {
      if any error has code UNAUTHENTICATED:
        → POST /auth/refresh (using httpOnly cookie)
        → on success: set new accessToken in store, retry operation
        → on failure: clearAccessToken(), navigate('/login')
    })

admin/src/store/auth.ts
  PURPOSE: Zustand store for authentication state.
  CONTAINS:
    interface AuthStore
      accessToken: string | null
      isRefreshing: boolean
      setAccessToken(token: string): void     → updates in-memory token
      clearAccessToken(): void                → called on logout or refresh failure
      setIsRefreshing(v: boolean): void
    const useAuthStore = create<AuthStore>(...)
  NOTE: Token is NEVER written to localStorage. Memory only → prevents XSS theft.
        Refresh token is in httpOnly cookie → browser sends it automatically.

admin/src/hooks/useRequireAuth.ts
  PURPOSE: Used by ProtectedLayout to guard all authenticated routes.
  CONTAINS:
    export function useRequireAuth()
      → if no accessToken in store:
            POST /auth/refresh
            on success → setAccessToken(newToken) → allow render
            on failure → navigate('/login')
      → returns { isReady: boolean }
  USED BY: ProtectedLayout component (wraps all protected routes in App.tsx)

admin/src/graphql/queries.graphql
  PURPOSE: All GraphQL read operations the UI performs.
  QUERIES:
    Me                → { id, email, role }
    GetWorkspace      → { name, slug, status }
    GetRemoteNetworks → RemoteNetwork { id, name, location, status, networkHealth,
                            connectors { id, name, status, ... },
                            shields { id, name, status, interfaceAddr, ... } }
    GetConnectors(remoteNetworkId) → Connector[]
    GetShields(remoteNetworkId)    → Shield[]
    GetRemoteNetwork(id)           → single RemoteNetwork
    LookupWorkspace(slug)          → { exists: bool }
    LookupWorkspacesByEmail(email) → [{ name, slug }]
  BUILD CMD: cd admin && npm run codegen  (regenerates generated/graphql.ts)

admin/src/graphql/mutations.graphql
  PURPOSE: All GraphQL write operations the UI performs.
  MUTATIONS:
    InitiateAuth(provider, workspaceName)           → { redirectUrl }
    CreateRemoteNetwork(name, location)             → RemoteNetwork
    DeleteRemoteNetwork(id)                         → Boolean
    GenerateConnectorToken(remoteNetworkId, name)   → { connectorId, installCommand }
    RevokeConnector(id)                             → Connector
    DeleteConnector(id)                             → Boolean
    GenerateShieldToken(remoteNetworkId, name)      → { shieldId, installCommand }
    RevokeShield(id)                                → Shield
    DeleteShield(id)                                → Boolean

admin/src/generated/graphql.ts
  PURPOSE: Auto-generated TypeScript types for all queries and mutations.
           DO NOT EDIT MANUALLY — regenerate with npm run codegen.
  CONTAINS: TypeScript interfaces for every GQL type, query variable types,
            query result types, React hook wrappers (useGetShieldsQuery etc.)
  BUILD CMD: cd admin && npm run codegen

admin/src/pages/Dashboard.tsx
  PURPOSE: Main landing page after login.
  SHOWS: Stats cards (active networks, total/active connectors),
         recent connectors list sorted by last_heartbeat_at,
         workspace info
  QUERIES: Me, GetWorkspace, GetRemoteNetworks
  POLLING: pollInterval: 30000 (30s)

admin/src/pages/RemoteNetworks.tsx
  PURPOSE: Lists all remote networks with health status.
  SHOWS: NetworkHealth badge (ONLINE/DEGRADED/OFFLINE), connector count, shield count
  QUERIES: GetRemoteNetworks (poll 30s)
  MUTATIONS: CreateRemoteNetwork, DeleteRemoteNetwork
  KEY LOGIC: NetworkHealth badge colors map from enum value in response

admin/src/pages/Connectors.tsx
  PURPOSE: Connectors list filtered by a specific remote network.
  SHOWS: Name, Status badge, Hostname, Public IP, Cert Expiry, Last Seen, Version
  QUERIES: GetConnectors(remoteNetworkId) (poll 30s)
  MUTATIONS: GenerateConnectorToken → opens InstallCommandModal
             RevokeConnector, DeleteConnector

admin/src/pages/Shields.tsx
  PURPOSE: Shields list filtered by a specific remote network.
  SHOWS: Name, Status badge, Interface IP (zecurity0 /32), Via (connector name),
         Last Seen, Version, Hostname
  QUERIES: GetShields(remoteNetworkId) (poll 30s)
  MUTATIONS: GenerateShieldToken → opens InstallCommandModal
             RevokeShield, DeleteShield

admin/src/pages/AllConnectors.tsx
  PURPOSE: Connector list across ALL networks (dashboard-level view).
  QUERIES: GetRemoteNetworks (flatten connectors from all networks)

admin/src/pages/ConnectorDetail.tsx
  PURPOSE: Single connector detail view.
  SHOWS: Full connector info, cert details, heartbeat history
  ROUTE: /connectors/:connectorId

admin/src/pages/Login.tsx
  PURPOSE: Login page for returning users.
  SHOWS: Google OAuth button, workspace selector if email has multiple workspaces
  MUTATIONS: InitiateAuth → redirect to Google OAuth

admin/src/pages/signup/Step1Email.tsx
  PURPOSE: Signup step 1 — enter email address.
  QUERIES: LookupWorkspacesByEmail (on submit, to detect returning users)

admin/src/pages/signup/Step2Workspace.tsx
  PURPOSE: Signup step 2 — choose workspace slug.
  QUERIES: LookupWorkspace (on-change, to check slug availability)

admin/src/pages/signup/Step3Auth.tsx
  PURPOSE: Signup step 3 — Google OAuth setup for new workspace.
  MUTATIONS: InitiateAuth

admin/src/components/InstallCommandModal.tsx
  PURPOSE: Two-step modal used for both Connector and Shield creation.
  STEP 1: Text input for name → "Generate" button
  STEP 2: Read-only textarea with install command:
            curl -fsSL .../install.sh | ENROLLMENT_TOKEN=<jwt> bash
          "Copy" button → copies to clipboard
  USED BY: Connectors.tsx (GenerateConnectorToken), Shields.tsx (GenerateShieldToken)

admin/src/components/layout/AppShell.tsx
  PURPOSE: Main layout wrapper for all protected pages.
  RENDERS: Header + Sidebar + <Outlet /> (child page content)

admin/src/components/layout/Sidebar.tsx
  PURPOSE: Left navigation bar.
  LINKS: Dashboard, Remote Networks, Connectors, Shields, Settings
  SHOWS: Active workspace name + slug

admin/src/components/layout/Header.tsx
  PURPOSE: Top bar.
  SHOWS: Logo, workspace name, user menu (logout)

admin/src/components/ui/
  PURPOSE: Shadcn/UI component library (built on Radix UI + Tailwind).
  COMPONENTS: Button, Card, Input, Badge, Dialog, Select, Tooltip, Table,
              DropdownMenu, Avatar, Skeleton, Separator, ScrollArea etc.
  NOTE: These are design primitives — do not add business logic here.

admin/codegen.yml
  PURPOSE: GraphQL Code Generator configuration.
  DOES: reads controller GraphQL schema + queries.graphql + mutations.graphql
        → outputs TypeScript types + React hooks to src/generated/
  BUILD CMD: cd admin && npm run codegen
  WHEN TO RUN: any time controller schema changes OR queries/mutations change

  8. Why does Shield need CAP_NET_ADMIN?
     → network.rs calls rtnetlink (kernel syscall) to create the zecurity0 TUN device
       and calls nftables to write firewall rules. Both require CAP_NET_ADMIN.
