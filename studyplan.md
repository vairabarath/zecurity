Zecurity — Full Codebase Study Plan
Updated: 2026-05-02 — Sprint 8 Phase 1 complete (Policy Engine: Groups, ACL Compiler, UI)

─────────────────────────────────────────────
WHAT THIS FILE IS
─────────────────────────────────────────────

A complete reading-order guide, function-level call trace, and code
comprehension aid for every major flow in the Zecurity ZTNA platform.

Read each section in order. Every concept builds on the previous one.
Do NOT skip to the code you are working on — read the proto files first.


─────────────────────────────────────────────
HOW TO READ THIS CODEBASE — QUICK GUIDE
─────────────────────────────────────────────

Before you open any file, internalize these three mental models:

  1. THE CONTRACT MODEL
     Every component communicates through proto-defined messages.
     proto/   → the contracts
     gen/go/  → auto-generated Go stubs (DO NOT edit)
     The Rust side compiles protos via build.rs using tonic-build.
     When you see an unknown type, look in the .proto file first,
     not in the generated code.

  2. THE IDENTITY MODEL
     Every agent (Connector, Shield, Client device) has a SPIFFE ID
     embedded in its X.509 cert as a URI SAN. Every mTLS call verifies
     that SPIFFE ID. "Who is this caller?" is always answered by checking
     the peer cert, never by a header or token. This is the core of ZTNA.

  3. THE LAYER MODEL
     UI  →  GraphQL  →  Controller  →  [Connector/Shield/Client over gRPC]
     Each layer only talks to the one directly below it.
     Shields never talk to the Controller; they only talk to their Connector.
     The Controller never calls agents — agents call in (pull model).

HOW TO TRACE ANY FLOW:
  a. Find the proto RPC that carries the action.
  b. Find the Go handler (controller/internal/<component>/).
  c. Find the Rust caller (enrollment.rs, control_stream.rs, etc.).
  d. Follow the DB writes in the handler to understand what changes.

HOW TO UNDERSTAND ANY RUST FILE:
  - Every Rust agent module follows the same shape:
      main.rs       → startup, spawn tokio tasks
      enrollment.rs → one-time token+cert exchange
      control_stream.rs → persistent bidirectional gRPC stream
      renewal.rs    → cert renewal triggered by re_enroll signal
      tls.rs        → mTLS channel builder
      config.rs     → env-var config struct
      appmeta.rs    → SPIFFE constants (must match Go)

HOW TO UNDERSTAND ANY GO FILE:
  - controller/internal/<component>/  one directory per agent type
  - Each directory: token.go, enrollment.go, control_stream.go, heartbeat.go, goodbye.go
  - Middleware runs before every handler: auth.go, workspace.go, role.go, session.go
  - All DB calls use pgxpool — QueryRow for single row, Query + rows.Next() for lists
  - All gRPC errors use google.golang.org/grpc/status.Errorf(codes.X, ...)

HOW TO UNDERSTAND THE POLICY ENGINE (Sprint 8):
  controller/internal/policy/
    store.go    → all DB queries (groups, members, access_rules, compiler queries)
    compiler.go → compiles ACL snapshot from DB
    cache.go    → in-memory per-workspace snapshot cache
    notifier.go → version bump + cache invalidation on policy mutation
  The flow: mutation → NotifyPolicyChange → Invalidate cache →
            next heartbeat/GetACLSnapshot → CompileACLSnapshot → cache.Set


─────────────────────────────────────────────
30-SECOND ARCHITECTURE
─────────────────────────────────────────────

  ┌──────────────────────────────────────────────────────────────────────┐
  │  Admin UI (React/TS)  ←→  Controller (Go) :8080 HTTP + :9090 gRPC   │
  │                                  ↕ mTLS                              │
  │              Connector (Rust)  :9090 → Controller  (Control stream)  │
  │                  ↑ mTLS :9091                                        │
  │              Shield (Rust)  →  Connector :9091  (Control stream)    │
  │                                                                      │
  │              Client CLI (Rust) → Controller :9090 (ClientService)   │
  └──────────────────────────────────────────────────────────────────────┘

  Admin UI     → talks to Controller via GraphQL over HTTPS
  Controller   → issues X.509 certs, stores state in Postgres + Redis/Valkey
  Connector    → Linux agent on network edge; relays Shield traffic to Controller
  Shield       → Linux agent on resource host; creates TUN + nftables firewall
  Client CLI   → end-user agent; authenticates, enrolls device, gets ACL snapshot


─────────────────────────────────────────────
FULL COMPONENT MAP (all source files)
─────────────────────────────────────────────

CONTROLLER (Go) — controller/
  cmd/server/main.go              startup wiring — creates all services
  internal/appmeta/identity.go    SPIFFE constants + helper functions
  internal/auth/
    service.go                    JWT issue + verify
    callback.go                   Google OAuth web callback handler
    exchange.go                   token exchange helpers
    refresh.go                    sliding refresh token
    session.go                    in-memory auth sessions
    valkey.go                     Redis/Valkey refresh token store
    oidc.go                       OIDC token verification
    idtoken.go                    Google ID token parsing
  internal/pki/
    root.go                       Root CA (self-signed, 10yr)
    intermediate.go               Intermediate CA (signed by Root, 5yr)
    workspace.go                  per-tenant Workspace CA + signing
    crypto.go                     AES-256-GCM key encryption helpers
    service.go                    PKI service interface
    controller.go                 Controller server TLS cert
  internal/db/
    pool.go                       pgxpool setup
    tenant.go                     tenant-scoped DB helpers
  internal/models/
    user.go                       User struct
    workspace.go                  Workspace struct
  internal/middleware/
    auth.go                       JWT extraction from HTTP header
    workspace.go                  workspace slug → tenantID context
    role.go                       RBAC role check
    session.go                    session context helpers
  internal/tenant/context.go      tenant context key helpers
  internal/connector/
    token.go                      generate + verify enrollment JWT
    token_handler.go              HTTP handler for token generation
    enrollment.go                 Enroll() gRPC handler
    control_stream.go             Control() bidirectional stream handler + ConnectorRegistry
    disconnect_watcher.go         goroutine: marks disconnected after 90s
    goodbye.go                    Goodbye() handler
    spiffe.go                     SPIFFE cert verification helpers
    ca_endpoint.go                GET /ca.crt HTTP handler
    config.go                     ConnectorConfig struct
  internal/shield/
    token.go                      generate + verify shield JWT
    token_handler.go              HTTP handler
    enrollment.go                 Enroll() gRPC handler (richer response: interface_addr, connector_addr)
    heartbeat.go                  UpdateShieldHealth() + disconnect watcher
    spiffe.go                     Shield SPIFFE helpers
    config.go                     ShieldConfig struct
  internal/resource/
    store.go                      CRUD for resources table
    config.go                     resource config
  internal/policy/
    store.go                      Group/member/rule CRUD + compiler queries
    compiler.go                   CompileACLSnapshot — rules→groups→users→SPIFFEs
    cache.go                      SnapshotCache — in-memory per-workspace
    notifier.go                   NotifyPolicyChange — version bump + cache invalidate
  internal/client/
    service.go                    ClientService gRPC: GetAuthConfig, InitiateAuth,
                                  TokenExchange, EnrollDevice, GetACLSnapshot
    store.go                      client_devices DB queries
    auth_session.go               in-memory PKCE auth session store
  internal/discovery/
    store.go                      DiscoveredService DB queries
    config.go                     discovery config
  internal/invitation/
    store.go                      invitation DB queries
    email.go                      email send helpers
    handler.go                    invitation HTTP handler
  internal/bootstrap/bootstrap.go  first-run workspace setup
  graph/
    resolver.go                   Resolver struct wiring all services
    model/model.go                GQL model types
    resolvers/
      resolver.go                 base Resolver type
      helpers.go                  shared resolver helpers
      auth.resolvers.go           Me, signup, OAuth resolvers
      workspace.resolvers.go      GetWorkspace, UpdateWorkspace
      connector.resolvers.go      GetConnectors, GenerateConnectorToken, Revoke, Delete
      shield.resolvers.go         GetShields, GenerateShieldToken, Revoke, Delete
      resource.resolvers.go       GetResources, CreateResource, UpdateResource, Delete
      policy.resolvers.go         Groups CRUD, AddMember, RemoveMember, AssignResource
      policy_helpers.go           groupRowToGQL, loadGroup, loadResourceWithGroups
      client.resolvers.go         ClientDevices, RevokeDevice
      client_helpers.go           client helper types
      discovery.resolvers.go      GetDiscoveredServices, RequestScan
      schema.resolvers.go         users list query

CONNECTOR (Rust) — connector/src/
  main.rs              startup: enrollment → spawn control_stream + agent_server
  appmeta.rs           SPIFFE constants (must match Go)
  config.rs            ConnectorConfig (env vars)
  enrollment.rs        Enroll RPC + save EnrollmentState to state_dir
  control_stream.rs    persistent bidirectional Control stream to Controller
                       handles: resource_instructions, re_enroll, scan_command, acl_snapshot, ping
  agent_server.rs      ShieldService gRPC server for incoming Shield connections
                       ShieldRegistry: tracks active Shield streams + buffers instructions
  controller_client.rs build mTLS gRPC channel to Controller
  tls.rs               TLS helpers (build server TLS for agent_server)
  renewal.rs           cert renewal on re_enroll signal
  crypto.rs            key generation helpers
  updater.rs           self-update logic
  util.rs              hostname, LAN IP detection
  build.rs             tonic-build compiles proto files

SHIELD (Rust) — shield/src/
  main.rs              startup: enrollment → setup network → spawn control_stream
  appmeta.rs           SPIFFE constants
  config.rs            ShieldConfig
  enrollment.rs        Enroll RPC to Connector + save ShieldState
  control_stream.rs    persistent bidirectional Control stream to Connector
                       handles: resource_instruction, re_enroll, ping
                       sends: health_report, resource_ack, discovery_report
  network.rs           TUN device creation (zecurity0), nftables setup via rtnetlink
  resources.rs         apply/remove resource nftables rules, port health check
  discovery.rs         service discovery: scan local ports, report changes
  tls.rs               mTLS helpers
  renewal.rs           cert renewal
  crypto.rs            key generation
  types.rs             ShieldState struct
  updater.rs           self-update
  util.rs              LAN IP detection

CLIENT CLI (Rust) — client/src/
  main.rs              CLI entry point (subcommands: login, enroll, acl)
  appmeta.rs           SPIFFE constants
  config.rs            ClientConfig (env vars + state_dir)
  state_store.rs       encrypted local state: JWT, device cert, device_id
                       (encrypted at rest; keys live in memory during active use)
  login.rs             login flow: GetAuthConfig → InitiateAuth → browser → TokenExchange
  grpc.rs              gRPC channel builder for ClientService
  runtime.rs           daemon bridge (Sprint 8.5) — not yet implemented
  error.rs             error types
  build.rs             tonic-build

ADMIN UI (React/TS) — admin/src/
  App.tsx              router: public routes (Login, Signup, Callback) + protected shell
  main.tsx             Vite entry, Apollo provider
  apollo/              Apollo client setup, auth links, error link (handles 401 refresh)
  stores/              Zustand auth store (user, workspace, token)
  pages/
    Dashboard.tsx       stats + recent connectors + workspace info
    RemoteNetworks.tsx  network list with health badge
    RemoteNetworkDetail.tsx  network detail with connectors + shields tabs
    Connectors.tsx      connector list + Generate/Revoke/Delete
    ConnectorDetail.tsx single connector: cert + heartbeat history
    AllConnectors.tsx   connector list across all networks
    Shields.tsx         shield list + Generate/Revoke/Delete
    ShieldDetail.tsx    single shield detail
    AllShields.tsx      shield list across all networks
    Resources.tsx       resources list + Create/Edit/Delete + groups with access
    ResourceDetail.tsx  single resource detail
    ResourceDiscovery.tsx  discovered services + scan request
    Groups.tsx          groups list + Create/Edit/Delete
    GroupDetail.tsx     group detail: members tab, resources tab
    TeamUsers.tsx       workspace users list + invite
    Settings.tsx        workspace settings
    Topology.tsx        network topology diagram
    ClientInstall.tsx   client CLI install guide
    Login.tsx           Google OAuth login
    InviteAccept.tsx    accept workspace invitation
    AuthCallback.tsx    OAuth callback handler
    signup/             3-step signup: email → workspace slug → OAuth
  components/
    InstallCommandModal.tsx   2-step modal: name → copy install command
    layout/AppShell.tsx       Header + Sidebar + <Outlet />
    layout/Sidebar.tsx        nav links + workspace name
    layout/Header.tsx         logo + user menu
    ui/                       Shadcn/UI primitives (Button, Card, Badge, Dialog, etc.)
  graphql/
    queries.graphql            all frontend GQL queries
    mutations.graphql          all frontend GQL mutations
  generated/graphql.ts         auto-generated TypeScript hooks — DO NOT EDIT

DATABASE — controller/migrations/
  001_schema.sql         workspaces, users, ca_root, ca_intermediate, workspace_ca_keys
  002_connector_schema.sql  remote_networks, connectors
  003_shield_schema.sql  shields
  004_connector_agent_addr.sql  add agent_addr column
  005_rename_agent_addr.sql     rename to public_ip
  006_shield_lan_ip.sql  add lan_ip to shields
  007_resources.sql      resources table
  008_add_unprotected_state.sql  unprotected status enum value
  009_streaming_states.sql       streaming/ACK tracking states
  010_discovery.sql      discovered_services table
  011_client.sql         client_devices table, auth sessions
  012_groups_acl.sql     groups, group_members, access_rules


─────────────────────────────────────────────
STEP 1 — CONTRACTS (proto files)  ~30 min
─────────────────────────────────────────────

Read these FIRST. Every other file implements or calls them.

  proto/connector/v1/connector.proto
  proto/shield/v1/shield.proto
  proto/client/v1/client.proto

CONNECTOR SERVICE (ConnectorService):
  rpc Enroll(EnrollRequest)       → EnrollResponse
  rpc Control(stream ...)         → bidirectional stream of ConnectorControlMessage
  rpc RenewCert(...)              → RenewCertResponse
  rpc Goodbye(...)                → GoodbyeResponse

  ConnectorControlMessage is a oneof — each message type is one direction:
    Controller → Connector: resource_instructions(1), re_enroll(2), ping(3),
                            scan_command(10), acl_snapshot(11)
    Connector → Controller: connector_health(4), shield_status(5), resource_acks(6),
                            pong(7), shield_discovery(8), scan_report(9)

SHIELD SERVICE (ShieldService — same RPC shape):
  rpc Enroll    → richer EnrollResponse: +interface_addr, +connector_addr, +connector_id
  rpc Control   → bidirectional stream of ShieldControlMessage
  rpc RenewCert → same as Connector
  rpc Goodbye   → same

  ShieldControlMessage oneof:
    Connector → Shield: resource_instruction(1), re_enroll(2), ping(3)
    Shield → Connector: health_report(4), resource_ack(5), pong(6), discovery_report(7)

CLIENT SERVICE (ClientService):
  rpc GetAuthConfig    → public, returns OAuth endpoint info
  rpc InitiateAuth     → registers PKCE session, returns Google auth URL
  rpc TokenExchange    → PKCE verify + JWT issue
  rpc EnrollDevice     → issue mTLS cert for client device
  rpc GetACLSnapshot   → returns workspace ACL snapshot (default-deny)

  ACLSnapshot shape:
    version, workspace_id, generated_at
    entries[]: resource_id, address, port, protocol, allowed_spiffe_ids[]

WHY READ PROTOS FIRST: Every handler, every Rust caller, every GraphQL mutation
ultimately produces or consumes one of these messages. Knowing the shape of
ConnectorControlMessage explains why the Connector's control_stream.rs has a
match arm for every message type. Knowing ACLSnapshot explains why the compiler
returns allowed_spiffe_ids per resource.


─────────────────────────────────────────────
STEP 2 — SHARED CONSTANTS (appmeta)  ~15 min
─────────────────────────────────────────────

These constants appear in every component. A mismatch = cert rejection.

  controller/internal/appmeta/identity.go
  connector/src/appmeta.rs
  shield/src/appmeta.rs
  client/src/appmeta.rs

KEY VALUES (must be identical in Go and Rust):

  PRODUCT_NAME                = "ZECURITY"
  SPIFFE_GLOBAL_TRUST_DOMAIN  = "zecurity.in"
  SPIFFE_CONTROLLER_ID        = "spiffe://zecurity.in/controller/global"
  SPIFFE_ROLE_CONNECTOR       = "connector"
  SPIFFE_ROLE_SHIELD          = "shield"
  SHIELD_INTERFACE_NAME       = "zecurity0"
  SHIELD_INTERFACE_CIDR_RANGE = "100.64.0.0/10"

HELPER FUNCTIONS (Go):
  WorkspaceTrustDomain(slug)        → "ws-acme.zecurity.in"
  ConnectorSPIFFEID(trustDomain, id) → "spiffe://ws-acme.zecurity.in/connector/<id>"
  ShieldSPIFFEID(trustDomain, id)    → "spiffe://ws-acme.zecurity.in/shield/<id>"
  ClientSPIFFEID(trustDomain, id)    → "spiffe://ws-acme.zecurity.in/client/<id>"


─────────────────────────────────────────────
STEP 3 — PKI (3-tier certificate chain)  ~45 min
─────────────────────────────────────────────

Read controller/internal/pki/ top to bottom:

  root.go          → Root CA (self-signed, 10yr, maxPathLen=2)
  intermediate.go  → Intermediate CA (signed by Root, 5yr, maxPathLen=1)
  workspace.go     → Workspace CA (per-tenant, 2yr) + signing functions
  crypto.go        → AES-256-GCM key encryption, PEM/DER helpers
  service.go       → Service interface (what callers use)
  controller.go    → ephemeral Controller server TLS cert

CERT CHAIN:   Root → Intermediate → WorkspaceCA → Connector/Shield/Client leaf cert

KEY FUNCTIONS:
  SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, ttl)
    → validates CSR self-signature
    → sets SPIFFE URI SAN = ConnectorSPIFFEID(trustDomain, connectorID)
    → signs with workspace CA private key (decrypted from DB with AES-GCM)
    → returns Certificate{PEM, chain, Serial, NotAfter}

  SignShieldCert(ctx, tenantID, shieldID, ...) — same flow, ShieldSPIFFEID
  SignClientCert(ctx, tenantID, deviceID, ...) — same flow, ClientSPIFFEID

IMPORTANT:
  - Every cert is 7 days. Renewal starts 48 hours before expiry.
  - Private keys NEVER leave the device they were generated on.
  - Workspace CA private key stored AES-256-GCM encrypted in DB (workspace_ca_keys).
  - mTLS peer cert SPIFFE verification happens in a gRPC interceptor (spiffe.go)
    before the handler ever runs.


─────────────────────────────────────────────
STEP 4 — DATABASE SCHEMA  ~30 min
─────────────────────────────────────────────

Read controller/migrations/ in numeric order (001 → 012).

CRITICAL TABLES AND COLUMNS:

  workspaces:
    id, slug, name, tenant_id (= workspace_id in JWTs)

  users:
    id, workspace_id, email, role ('admin'|'member'), google_sub

  connectors:
    status        ENUM('pending','active','disconnected','revoked')
    enrollment_token_jti  TEXT UNIQUE   ← stored in Redis too (single-use burn)
    last_heartbeat_at  TIMESTAMPTZ
    cert_not_after     TIMESTAMPTZ
    public_ip, hostname, version

  shields:
    connector_id  UUID REFERENCES connectors   ← which Connector owns this Shield
    interface_addr INET   ← /32 from 100.64.0.0/10 (unique per workspace)
    lan_ip         TEXT   ← detected LAN IP of the resource host
    (status, cert columns mirror connectors)

  resources:
    workspace_id, shield_id (optional), host, port_from, port_to, protocol
    status: 'pending'|'protected'|'failed'|'removed'|'unprotected'
    connector_id: which connector manages the shield

  groups:
    workspace_id, name, description

  group_members:
    group_id, user_id, joined_at

  access_rules:
    workspace_id, resource_id, group_id, enabled (bool)
    UNIQUE(resource_id, group_id)

  client_devices:
    user_id, workspace_id, device_name, os
    spiffe_id  TEXT   ← set after EnrollDevice, used by ACL compiler
    cert_not_after, cert_serial

  discovered_services:
    shield_id, protocol, port, bound_ip, service_name, first_seen_at

REDIS/VALKEY KEYS:
  "connector:jti:{jti}"  → connectorID   (single-use enrollment token burn)
  "shield:jti:{jti}"     → shieldID      (single-use enrollment token burn)
  "refresh:{token}"      → userID        (sliding refresh token store)


─────────────────────────────────────────────
STEP 5 — CONTROLLER (Go backend)  ~2.5 hours
─────────────────────────────────────────────

Read in this exact order:

① controller/internal/auth/service.go
    IssueAccessToken(userID, workspaceID, role) → signed JWT, 15min TTL
    VerifyAccessToken(tokenStr)                 → TokenClaims{UserID, TenantID, Role}
    IssueRefreshToken(ctx, userID)              → opaque token stored in Redis
    RefreshAccessToken(ctx, refreshToken)       → new access JWT, slides Redis TTL

② controller/internal/connector/token.go
    GenerateEnrollmentToken(connectorID, workspaceID, trustDomain, ttl)
      → HMAC-SHA256 JWT, claims: {sub: connectorID, jti: UUID, workspace_id, trust_domain}
      → Redis: SET "connector:jti:{jti}" connectorID EX {ttl}
    VerifyEnrollmentToken(tokenStr)
      → parses + verifies signature + expiry
    BurnEnrollmentJTI(ctx, redis, jti)
      → Redis: GET + DEL in a transaction (atomic single-use burn)

③ controller/internal/connector/enrollment.go
    Enroll(ctx, req *EnrollRequest) (*EnrollResponse, error)
      Step 1: VerifyEnrollmentToken(req.EnrollmentToken)
      Step 2: BurnEnrollmentJTI → if already burned: codes.AlreadyExists
      Step 3: SELECT connector WHERE id=claims.sub AND status='pending'
      Step 4: Parse req.CsrDer as PKCS#10
      Step 5: Verify CSR self-signature (proves key ownership)
      Step 6: Verify SPIFFE SAN == ConnectorSPIFFEID(trustDomain, connectorID)
      Step 7: pki.SignConnectorCert(...)
      Step 8: UPDATE connectors SET status='active', cert_serial, cert_not_after
      Step 9: return EnrollResponse{CertificatePem, WorkspaceCaPem, IntermediateCaPem}

④ controller/internal/connector/control_stream.go
    Control(stream pb.ConnectorService_ControlServer) error
      → read peer cert SPIFFE from mTLS context
      → register stream in ConnectorRegistry
      → pump inbound messages: match body type
          ConnectorHealthReport → UPDATE connectors SET public_ip, hostname, version
          ShieldStatusBatch     → UPDATE shields SET status, version, lan_ip, last_seen
          ResourceAckBatch      → store acks in DB
          ShieldDiscoveryBatch  → store discovered_services
          ScanReport            → store scan results
          Pong                  → record latency
      → on disconnect: remove from registry

    ConnectorRegistry.PushInstruction(row *resource.Row)
      → get stream client by connector_id → stream.Send(resource_instructions message)
      → if no active stream: no-op (instruction already in DB, delivered on reconnect)

    ConnectorRegistry.PushACLSnapshot(connectorID string, snap *clientv1.ACLSnapshot)
      → sends acl_snapshot(11) message to the connector's live stream
      → called after NotifyPolicyChange in policy mutations

⑤ controller/internal/connector/disconnect_watcher.go
    RunDisconnectWatcher(ctx, pool, cfg)
      → goroutine: every 60s:
          UPDATE connectors SET status='disconnected'
          WHERE status='active' AND last_heartbeat_at < NOW() - 90s

⑥ controller/internal/shield/token.go
    GenerateShieldToken(ctx, pool, redis, cfg, remoteNetworkID, shieldName, workspaceID, trustDomain)
      → selectConnector: picks least-loaded active Connector in same network
      → assignInterfaceAddr: iterate 100.64.0.0/10, find unused /32
      → INSERT shields
      → HMAC JWT: {sub: shieldID, jti, workspace_id, trust_domain, connector_id, interface_addr}
      → Redis: SET "shield:jti:{jti}" shieldID

⑦ controller/internal/shield/enrollment.go
    Enroll(ctx, req *EnrollRequest) (*EnrollResponse, error)
      Steps 1-5: same token burn + pending-status check as Connector
      Step 6-8: CSR parse + verify SPIFFE SAN + sign cert
      Step 9:   UPDATE shields SET status='active', ...
      Step 10:  SELECT connector WHERE id=claims.connector_id
      Step 11:  return EnrollResponse{...plus InterfaceAddr, ConnectorAddr, ConnectorId}
      NOTE: ConnectorAddr is what Shield uses to dial its agent_server

⑧ controller/internal/shield/heartbeat.go
    UpdateShieldHealth(ctx, pool, shieldID, status, version, lastHeartbeatAt)
      → UPDATE shields SET status, version, last_heartbeat_at
    RunDisconnectWatcher: same pattern, 120s threshold for shields

⑨ controller/internal/policy/ — THE POLICY ENGINE
    store.go:
      CreateGroup / UpdateGroup / DeleteGroup / GetGroup / ListGroups
      AddGroupMember / RemoveGroupMember / ListGroupMembers
      AssignResourceToGroup / UnassignResourceFromGroup
      ListEnabledRulesWithResources  ← compiler query: rules joined with resource host/port
      ListActiveDeviceSPIFFEsForGroup ← compiler query: group members' device SPIFFEs
    compiler.go:
      CompileACLSnapshot(ctx, store, workspaceID) *clientv1.ACLSnapshot
        → ListEnabledRulesWithResources (one row per rule+resource)
        → for each rule: ListActiveDeviceSPIFFEsForGroup
        → aggregate by resource, deduplicate SPIFFEs
        → return ACLSnapshot{entries[]}
    cache.go:
      SnapshotCache — sync.RWMutex protected map[workspaceID]snapshot
      Get / Set / Invalidate
    notifier.go:
      NotifyPolicyChange(ctx, workspaceID)
        → increment atomic version counter
        → cache.Invalidate(workspaceID)
        → (future) push to connected Connectors via ConnectorRegistry.PushACLSnapshot

⑩ controller/internal/client/service.go
    GetACLSnapshot(ctx, req) (*GetACLSnapshotResponse, error)
      → VerifyAccessToken(req.AccessToken)
      → confirm device belongs to user+workspace in client_devices
      → cache.Get(workspaceID) → return cached snapshot if hit
      → on miss: CompileACLSnapshot + cache.Set
      → default-deny: return Internal error on any compile failure

⑪ controller/cmd/server/main.go  — startup wiring
    main()
      → db.Init()                            → *pgxpool.Pool
      → pki.Init()                           → pki.Service (loads or creates 3-tier CA)
      → auth.NewService()                    → JWT + refresh token service
      → connector.NewConfig() + shield.NewConfig()
      → redis.NewClient()
      → policy.NewStore() + policy.NewSnapshotCache() + policy.NewNotifier()
      → client.NewService() + connector.NewConnectorRegistry()
      → gqlgen.NewDefaultServer(NewSchema(resolver))
      → RegisterRoutes:
          POST /graphql             → gqlgen handler (JWT + workspace middleware)
          GET  /auth/callback       → Google OAuth web callback (admin login)
          POST /auth/refresh        → sliding refresh
          GET  /ca.crt              → public CA cert download
          GET  /api/clients/callback → client CLI OAuth callback
          POST /api/clients/invite  → accept invitation
      → grpc.NewServer() + register ConnectorServiceServer + ShieldServiceServer + ClientServiceServer
      → RunDisconnectWatcher (goroutine)


─────────────────────────────────────────────
STEP 6 — CONNECTOR (Rust)  ~1.5 hours
─────────────────────────────────────────────

Read in this order:

① connector/src/config.rs
    ConnectorConfig: CONTROLLER_ADDR, STATE_DIR, ENROLLMENT_TOKEN, CONNECTOR_LAN_ADDR
    Loaded from env vars at startup.

② connector/src/enrollment.rs
    enroll(cfg, token) → EnrollmentState
      → generate EC P-384 key pair
      → create PKCS#10 CSR with SPIFFE URI SAN
      → call ConnectorServiceClient::enroll(EnrollRequest)
      → save cert + key + connector_id to state_dir/
      → return EnrollmentState{connector_id, cert_pem, key_pem, ca_chain, trust_domain}
    NOTE: Enrollment uses plain TLS (no client cert yet). The CSR proves key ownership.

③ connector/src/tls.rs
    build_mtls_channel(state) → Channel
      → load cert + key from EnrollmentState
      → verify Controller SPIFFE SAN (UnarySPIFFEInterceptor equivalent)
      → returns tokio channel with mTLS Identity

④ connector/src/control_stream.rs  — THE MAIN LOOP
    run_control_stream(cfg, state, shield_registry, ack_rx) → never returns
      → exponential backoff reconnect loop
      → inner: run_once() — establishes bidirectional Control stream
          outbound goroutine:
            every 15s: send ConnectorHealthReport
            every 5s: flush pending discovery reports as ShieldDiscoveryBatch
            on ack_rx message: send ResourceAckBatch
          inbound loop: match msg.body
            resource_instructions → decode per-shield, push to ShieldRegistry
            re_enroll             → renewal::renew_cert()
            ping                  → send Pong
            scan_command          → spawn scan task → send ScanReport
            acl_snapshot          → store in local policy cache (Sprint 8 M4 work)

⑤ connector/src/agent_server.rs  — SHIELD-FACING SERVER
    ShieldRegistry: shared state across all Shield connections
      instruction_txs: map[shield_id → mpsc::Sender<ResourceInstruction>]
      resource_instructions: buffered instructions for offline shields
      ack_tx: unified sink — acks forwarded to control_stream
      health: map[shield_id → ShieldEntry{status, version, last_seen, lan_ip}]
      pending_discovery: map[shield_id → DiscoveryReport]
      controller_channel: for renewal proxying

    ShieldService implementation:
      Enroll(req) → proxy to Controller ShieldServiceClient::enroll()
                  → get interface_addr + connector_addr from response
                  → return to Shield
      Control(stream) → bidirectional stream
          register shield → drain buffered instructions
          inbound: health_report → update health map
                   resource_ack  → push to ack_tx (forwarded upstream)
                   discovery_report → merge into pending_discovery
                   pong → log latency
      RenewCert(req) → proxy to Controller ShieldServiceClient::renew_cert()
      Goodbye(req)   → remove from health map

⑥ connector/src/main.rs
    main()
      → load config
      → enrollment::enroll(cfg, token) OR load existing state
      → create ShieldRegistry + ack channel
      → tokio::spawn(agent_server::serve(...))   ← Shield-facing gRPC server on :9091
      → run_control_stream(...)                   ← blocks: Controller-facing stream


─────────────────────────────────────────────
STEP 7 — SHIELD (Rust)  ~1.5 hours
─────────────────────────────────────────────

① shield/src/config.rs
    ShieldConfig: CONNECTOR_ADDR (set from EnrollResponse), STATE_DIR, ENROLLMENT_TOKEN

② shield/src/enrollment.rs
    enroll(cfg, token) → ShieldState
      → generate EC P-384 key pair
      → CSR with SPIFFE URI SAN = ShieldSPIFFEID(trustDomain, shieldID)
      → call ShieldServiceClient::enroll(EnrollRequest)  ← to Connector, not Controller
      → read EnrollResponse:
          interface_addr → will be assigned to zecurity0 TUN
          connector_addr → where to connect for heartbeats
          connector_id   → used for SPIFFE verification
      → save state to state_dir/
    NOTE: Shield always talks to Connector on :9091, never directly to Controller.

③ shield/src/network.rs  — KERNEL-LEVEL NETWORKING
    setup(interface_addr, connector_id) → TUN device + nftables
      create_tun_device("zecurity0")
        → rtnetlink: create TUN, assign interface_addr/32
        → ip link set zecurity0 up
      init_nftables()
        → nft: create table + chain "resource_protect" (always flushed atomically, never appended)
    add_resource_rule(host, port_from, port_to, protocol)
        → nft add rule: accept traffic matching host+port
    remove_resource_rule(host, port_from, port_to, protocol)
        → nft delete rule
    IMPORTANT: requires CAP_NET_ADMIN capability (systemd unit: AmbientCapabilities)

④ shield/src/resources.rs
    apply_resource(instruction)
      → validate host == detect_lan_ip()  ← security: never apply foreign host rules
      → network::add_resource_rule(...)
      → check_port(host, port)           ← verify port is reachable (tries 127.0.0.1, ::1, host)
      → return ResourceAck{status: "protected"}
    remove_resource(instruction)
      → network::remove_resource_rule(...)
      → return ResourceAck{status: "removed"}

⑤ shield/src/control_stream.rs  — THE MAIN LOOP
    run_once(state, cfg) → establishes ShieldService::Control() stream to Connector
      outbound: every 60s: send ShieldHealthReport
                on resource_ack available: send ResourceAck
      inbound: match msg.body
        resource_instruction → resources::apply_resource or remove_resource
                             → send ResourceAck
        re_enroll            → renewal::renew_cert()
        ping                 → send Pong

⑥ shield/src/discovery.rs
    run_discovery(state, cfg) → goroutine: scan local ports every 60s
      → collect TCP listeners (from /proc/net/tcp + tcp6)
      → compute fingerprint (hash of port set)
      → if changed: send DiscoveryReport with added/removed services

⑦ shield/src/main.rs
    main()
      → enroll(cfg, token) OR load existing state
      → network::setup(interface_addr, connector_id)
      → tokio::spawn(control_stream::run(...))
      → tokio::spawn(discovery::run_discovery(...))


─────────────────────────────────────────────
STEP 8 — CLIENT CLI (Rust)  ~45 min
─────────────────────────────────────────────

① client/src/state_store.rs
    Encrypted at-rest local state: access_token, refresh_token, device_cert, device_id
    Key derivation: machine-specific secret → AES-256-GCM
    IMPORTANT: decrypted private key and active tokens live only in process memory.

② client/src/login.rs
    login(cfg) — the full auth flow:
      → ClientServiceClient::get_auth_config(workspace_slug)
      → ClientServiceClient::initiate_auth(code_challenge, local_redirect_uri)
        NOTE: CLI generates its own PKCE pair. Controller generates a second PKCE pair
              for Google. Two independent PKCE exchanges in parallel.
      → open browser at auth_url
      → spawn local HTTP server on 127.0.0.1:<random port>
      → wait for Google callback → ctrl_code arrives at local server
      → ClientServiceClient::token_exchange(ctrl_code, code_verifier, session_id)
      → save access_token + refresh_token to state_store

③ client/src/grpc.rs
    build_channel(cfg, state) → gRPC channel to Controller
    Enrollment uses plain TLS; post-enrollment uses mTLS with device cert.

④ client/src/main.rs
    Subcommands:
      login    → login::login(cfg)
      enroll   → ClientServiceClient::enroll_device(access_token, csr_pem, device_name, os)
      acl      → ClientServiceClient::get_acl_snapshot(access_token, device_id) → print table
    NOTE: daemon/runtime subcommands are Sprint 8.5 work.


─────────────────────────────────────────────
STEP 9 — GRAPHQL + ADMIN UI  ~1.5 hours
─────────────────────────────────────────────

① controller/graph/resolver.go
    Resolver struct: holds ALL service deps (pool, pki, auth, connector, shield,
                     resource, policy, discovery, client svc, invitation)
    Every resolver file embeds this via the mutationResolver/queryResolver wrappers.
    When you see "r.resolver" in a resolver file, it's accessing this struct.

② controller/graph/resolvers/helpers.go
    Shared conversion helpers: dbStatusToGQL, toTimePtr, nullStringToPtr, etc.
    Read this before reading any resolver — it defines the vocabulary.

③ controller/graph/resolvers/policy.resolvers.go + policy_helpers.go
    CreateGroup / UpdateGroup / DeleteGroup → store.CreateGroup / ... → NotifyPolicyChange
    AddGroupMember / RemoveGroupMember      → store.AddGroupMember / ... → NotifyPolicyChange
    AssignResourceToGroup / UnassignResourceFromGroup → store.Assign... → NotifyPolicyChange
    groupRowToGQL(row) → model.Group
    loadGroup(ctx, id)  → store.GetGroup + load members + resources
    loadResourceWithGroups(ctx, id) → store.GetResource + load group IDs

④ admin/src/graphql/queries.graphql
    GetGroups, GetGroup, GetUsers, GetResources, GetResourceWithGroups
    All queries: variables → gRPC GraphQL → PostgreSQL → JSON response

⑤ admin/src/graphql/mutations.graphql
    CreateGroup, UpdateGroup, DeleteGroup
    AddGroupMember, RemoveGroupMember
    AssignResourceToGroup, UnassignResourceFromGroup

⑥ admin/src/pages/Groups.tsx
    useGetGroupsQuery() → list of groups
    CREATE button → useCreateGroupMutation()
    DELETE row → useDeleteGroupMutation()
    Click row → navigate to GroupDetail

⑦ admin/src/pages/GroupDetail.tsx
    Two tabs: Members | Resources
    Members tab:
      useGetGroupQuery(groupId) → group.members[]
      "Add Member" picker → useGetUsersQuery() → dropdown of workspace users
      useAddGroupMemberMutation() / useRemoveGroupMemberMutation()
    Resources tab:
      group.resources[] (from GetGroup query)
      useUnassignResourceFromGroupMutation()

⑧ admin/src/pages/Resources.tsx
    useGetResourcesQuery() → each resource shows "Groups with access" section
    Row expand → list of group names with access
    useAssignResourceToGroupMutation() — from resource side

⑨ admin/apollo/ — Auth link chain
    Links (in order): authLink → errorLink → httpLink
    authLink: injects "Authorization: Bearer <token>" from Zustand store
    errorLink: on 401 response → call /auth/refresh → retry original request
              on persistent 401 → logout (clear store + redirect to Login)
    SPRINT 8 FIX: errorLink now checks statusCode === 401 on network errors
    (Apollo v4 surfaces HTTP 401 as a network error, not a GraphQL error)


─────────────────────────────────────────────
STEP 10 — MIDDLEWARE CHAIN  ~20 min
─────────────────────────────────────────────

Every HTTP request to /graphql runs through these middleware in order:

  1. CORS middleware        → allow admin UI origin
  2. auth.Middleware        → extract JWT from Authorization header
                           → set user claims in context (ctx.Value(auth.ClaimsKey))
                           → unauthenticated = continue (resolvers check claims)
  3. workspace.Middleware   → extract workspace-slug header
                           → lookup tenant_id → set in context
  4. gqlgen handler        → resolvers run
     → resolvers call requireAuth(ctx) helper → returns error if no claims

Every gRPC call to ConnectorService/ShieldService runs through:
  UnarySPIFFEInterceptor → extracts peer TLS cert → verifies SPIFFE URI SAN
  → on mismatch: codes.Unauthenticated

ClientService is EXEMPT from the SPIFFE interceptor — clients have no workspace
cert until after EnrollDevice.


─────────────────────────────────────────────
STEP 11 — END-TO-END FLOWS  ~2 hours
─────────────────────────────────────────────

═══════════════════════════════════════════
FLOW A: Shield Protection (20 steps)
═══════════════════════════════════════════

Precondition: Connector "xyz" is enrolled and active. No shields yet.
Admin creates a shield "my-shield" for remote network "acme-net".

1. Admin UI (Shields.tsx)
   → useGenerateShieldTokenMutation({ input: { name: "my-shield", remoteNetworkId: ... }})

2. controller/graph/resolvers/shield.resolvers.go → GenerateShieldToken resolver
   → calls shieldSvc.GenerateShieldToken(ctx, pool, redis, cfg, remoteNetworkID, "my-shield", workspaceID, trustDomain)

3. controller/internal/shield/token.go → GenerateShieldToken()
   → selectConnector: picks "xyz" (only active connector)
   → assignInterfaceAddr: finds "100.64.0.5" (first unused /32)
   → INSERT shields(id="abc", status='pending', connector_id="xyz", interface_addr="100.64.0.5")
   → generate JWT{sub:"abc", jti:"<uuid>", connector_id:"xyz", interface_addr:"100.64.0.5"}
   → Redis: SET "shield:jti:<uuid>" "abc" EX 3600
   → return (shieldID="abc", token=JWT)

4. Admin UI
   → InstallCommandModal shows: curl ... | ENROLLMENT_TOKEN=<jwt> bash
   → admin runs this on the resource host machine

5. [On resource host] shield binary starts
   shield/src/main.rs → load config (ENROLLMENT_TOKEN from env)
   → shield/src/enrollment.rs → enroll(cfg, token)

6. shield/src/enrollment.rs
   → generate EC P-384 key pair
   → generate CSR with SPIFFE SAN = "spiffe://ws-acme.zecurity.in/shield/abc"
   → ShieldServiceClient::enroll(EnrollRequest{ token, csr_der }) to Connector :9091

7. connector/src/agent_server.rs → Enroll()
   → proxy: ShieldServiceClient (Controller-side) → Controller :9090

8. controller/internal/shield/enrollment.go → Enroll()
   → VerifyShieldToken(token) → claims{sub:"abc", connector_id:"xyz", interface_addr:"100.64.0.5"}
   → BurnShieldJTI → atomic Redis GET+DEL
   → SELECT shield WHERE id="abc" AND status='pending'
   → SELECT connector WHERE id="xyz" AND status='active'
   → verify CSR SPIFFE SAN
   → pki.SignShieldCert → 7-day cert with ShieldSPIFFEID SAN
   → UPDATE shields SET status='active', cert_serial, cert_not_after
   → return EnrollResponse{ cert, ca_chain, interface_addr:"100.64.0.5",
                             connector_addr:"192.168.1.10:9091", connector_id:"xyz" }

9. connector/src/agent_server.rs → relay response to Shield

10. shield/src/enrollment.rs → save {cert, key, ca_chain, interface_addr, connector_addr, connector_id}

11. shield/src/main.rs → network::setup("100.64.0.5", "xyz")
    → create zecurity0 TUN device with IP 100.64.0.5/32
    → init nftables table + empty resource_protect chain
    → ip link set zecurity0 up

12. shield/src/main.rs → spawn control_stream::run(state, cfg)

13. shield/src/control_stream.rs → run_once()
    → mTLS channel to "192.168.1.10:9091" (shield.crt + workspace_ca.crt)
    → verify Connector SPIFFE (ws-acme.zecurity.in/connector/xyz)
    → establish bidirectional ShieldService::Control() stream
    → every 60s: send ShieldHealthReport{version, hostname, lan_ip:"192.168.1.10"}

14. connector/src/agent_server.rs → inbound ShieldHealthReport
    → health["abc"] = ShieldEntry{status:"active", version, last_seen:now, lan_ip:"192.168.1.10"}

15. connector/src/control_stream.rs → every 5s: flush pending discovery + health
    → sends ShieldStatusBatch{[{shield_id:"abc", status:"active", lan_ip:"192.168.1.10"}]}

16. controller/internal/connector/control_stream.go → inbound ShieldStatusBatch
    → shieldSvc.UpdateShieldHealth("abc", "active", ...)
    → UPDATE shields SET status='active', last_heartbeat_at, lan_ip WHERE id='abc'

17. Admin UI (Shields.tsx)
    → 30s poll fires → Shield "my-shield" shows Status: ONLINE, Interface: 100.64.0.5
    ✓ Shield enrolled and visible

18. [Admin creates a Resource]
    → CreateResource mutation → INSERT resources(host:"192.168.1.10", port:443, ...)
    → resource resolver calls ConnectorRegistry.PushInstruction(row)
    → PushInstruction sends ResourceInstructionBatch over Control stream to Connector "xyz"

19. connector/src/control_stream.rs → inbound resource_instructions
    → ShieldRegistry.push_instruction("abc", ResourceInstruction{host:"192.168.1.10", port:443})

20. connector/src/agent_server.rs → deliver via mpsc to Shield "abc"'s Control stream
    → Shield receives ResourceInstruction → resources::apply_resource()
    → nft add rule: accept 192.168.1.10:443
    → port reachability check (check_port) → success
    → send ResourceAck{status:"protected"} → upstream to Controller
    ✓ Resource protected + firewall rule active


═══════════════════════════════════════════
FLOW B: Policy ACL (Sprint 8) — ACL compiled and pushed
═══════════════════════════════════════════

Precondition: user "alice" exists. Resource "web-server" (192.168.1.10:443) exists.
Admin creates a Group, adds Alice, assigns web-server to that Group.

1. Admin creates group:
   CreateGroup mutation → policy.store.CreateGroup(workspaceID, "DevTeam")
   → notifier.NotifyPolicyChange(workspaceID) → cache.Invalidate(workspaceID)

2. Admin adds Alice to group:
   AddGroupMember mutation → store.AddGroupMember(groupID, alice.userID)
   → notifier.NotifyPolicyChange(workspaceID) → cache.Invalidate(workspaceID)

3. Admin assigns resource:
   AssignResourceToGroup mutation → store.AssignResourceToGroup(workspaceID, resourceID, groupID)
   → notifier.NotifyPolicyChange(workspaceID) → cache.Invalidate(workspaceID)

4. [Next heartbeat cycle] Connector "xyz" sends ConnectorHealthReport upstream.
   Controller handler → checks if ACL snapshot available for workspace
   → cache.Get(workspaceID) → miss (was invalidated)
   → CompileACLSnapshot:
       ListEnabledRulesWithResources → [{resource_id:"web-server", address:"192.168.1.10",
                                          port:443, protocol:"tcp", group_id:"DevTeam"}]
       ListActiveDeviceSPIFFEsForGroup("DevTeam")
         → alice's client device SPIFFE: "spiffe://ws-acme.zecurity.in/client/device-1"
       → ACLSnapshot.entries = [{resource_id:"web-server", address:"192.168.1.10", port:443,
                                  protocol:"tcp", allowed_spiffe_ids:["spiffe://...client/device-1"]}]
   → cache.Set(workspaceID, snapshot)
   → ConnectorRegistry.PushACLSnapshot("xyz", snapshot)
     → sends acl_snapshot(11) message on live Control stream

5. connector/src/control_stream.rs → inbound acl_snapshot(11)
   → policy::store local snapshot in memory (Sprint 8 M4)
   → Connector can now default-deny incoming client connections
     that lack a matching SPIFFE in the snapshot

6. [Client requests ACL snapshot]
   client CLI: get_acl_snapshot(access_token, device_id)
   → controller/internal/client/service.go → GetACLSnapshot()
   → cache.Get(workspaceID) → HIT → return snapshot
   → Client stores snapshot locally (Sprint 8.5 daemon: used for tunnel access control)


═══════════════════════════════════════════
FLOW C: Certificate Auto-Renewal
═══════════════════════════════════════════

Trigger: cert_not_after is within 48 hours of expiry.

1. controller/internal/connector/control_stream.go
   → cert expiry check in health report handler
   → if cert_not_after < now + cfg.RenewalWindow:
       stream.Send(ConnectorControlMessage{body: ReEnrollSignal{}})

2. connector/src/control_stream.rs → inbound re_enroll
   → renewal::renew_cert(&state, &cfg)

3. connector/src/renewal.rs
   → read existing key (private key stays local, never sent anywhere)
   → ConnectorServiceClient::renew_cert(RenewCertRequest{connector_id, public_key_der})

4. controller/internal/connector/ → RenewCert handler
   → verify mTLS (existing cert still valid)
   → pki.SignConnectorCert (same SPIFFE, new serial, new 7-day window)
   → return RenewCertResponse{certificate_pem, ca_chain}

5. connector/src/renewal.rs
   → write new cert to state_dir/connector.crt
   → update EnrollmentState in memory
   → rebuild mTLS channel with new cert

Same flow for Shield: re_enroll signal from Connector → renewal via proxy through Connector.
Same flow for Client device: detected by client CLI before expiry.


═══════════════════════════════════════════
FLOW D: Client Login + Device Enrollment
═══════════════════════════════════════════

1. client/src/main.rs → "zecurity login --workspace acme"

2. client/src/login.rs → login()
   → generate CLI PKCE pair: code_verifier (32 random bytes), code_challenge = SHA256(verifier)
   → spawn local HTTP server on 127.0.0.1:<random_port>
   → ClientServiceClient::initiate_auth(workspace_slug, code_challenge, local_redirect_uri)

3. controller/internal/client/service.go → InitiateAuth()
   → generate Controller PKCE pair (separate from CLI pair)
   → build Google OAuth URL with controller's code_challenge + state=sessionID
   → store authSession{workspaceID, cliCodeChallenge, googleCodeVerifier, ...} in memory
   → return {auth_url, session_id}

4. client/src/login.rs
   → open browser at auth_url → user logs in with Google

5. Google → redirect to controller's /api/clients/callback?code=...&state=sessionID

6. controller/internal/client/service.go → AuthCallbackHandler()
   → exchange Google code using controller's PKCE verifier (server-side, browser never sees this)
   → verify Google ID token
   → generate short-lived ctrl_code
   → redirect browser to local_redirect_uri?code=ctrl_code

7. client/src/login.rs local server
   → receives ctrl_code
   → ClientServiceClient::token_exchange(ctrl_code, code_verifier, session_id)

8. controller/internal/client/service.go → TokenExchange()
   → consume session (single-use)
   → verify ctrl_code
   → verify CLI PKCE: SHA256(code_verifier) == cliCodeChallenge stored in session
   → upsertUser (create if first login, error if no invite + not already member)
   → IssueAccessToken + IssueRefreshToken
   → return {access_token, refresh_token, expires_in, email}

9. client/src/login.rs
   → state_store.save(access_token, refresh_token)

10. "zecurity enroll --device-name laptop"
    → generate device EC P-384 key pair
    → CSR with ClientSPIFFEID SAN
    → ClientServiceClient::enroll_device(access_token, csr_pem, device_name, os)

11. controller → EnrollDevice()
    → verify access_token
    → parse + verify CSR
    → insertClientDevice → device_id
    → pki.SignClientCert(tenantID, deviceID, trustDomain, csr, 7d)
    → updateClientDeviceCert: set spiffe_id = ClientSPIFFEID(trustDomain, deviceID)
    → return {certificate_pem, ca_chain, spiffe_id, device_id}

12. client saves device cert + device_id to state_store (encrypted at rest)


─────────────────────────────────────────────
STEP 12 — KEY PATTERNS TO RECOGNIZE  ~30 min
─────────────────────────────────────────────

These patterns repeat everywhere. Learn them once, recognize them anywhere.

1. SINGLE-USE TOKEN BURN (Go)
     jti := uuid.New()
     redis.SetEX(ctx, "connector:jti:"+jti, connectorID, ttl)
     // later, at enrollment:
     val := redis.GetDel(ctx, "connector:jti:"+jti) // atomic get-and-delete
     // if val is empty → already burned → reject with AlreadyExists
   WHY: Prevents replay attacks. Enrollment token can only succeed once.

2. EXPONENTIAL BACKOFF RECONNECT (Rust)
     let mut backoff_secs = 2;
     loop {
         match run_once(...).await {
             Ok(()) => { backoff_secs = 2; } // clean shutdown, reset
             Err(e) => {
                 sleep(Duration::from_secs(backoff_secs)).await;
                 backoff_secs = (backoff_secs * 2).min(60);
             }
         }
     }
   SEEN IN: connector/control_stream.rs, shield/control_stream.rs

3. BIDIRECTIONAL STREAM PUMP (Rust + Go)
   Rust sender side (tokio):
     tokio::spawn(async move { loop { send message every N seconds } });
   Rust receiver side:
     while let Some(msg) = stream.message().await? { match msg.body { ... } }
   Go sender side:
     client.sendMu.Lock(); stream.Send(msg); client.sendMu.Unlock()
   WHY: Streams are concurrent. The send mutex prevents interleaved writes.

4. DEFAULT-DENY (Go policy)
     snap, ok := cache.Get(workspaceID)
     if !ok {
         snap, err = CompileACLSnapshot(ctx, store, workspaceID)
         if err != nil {
             return nil, status.Errorf(codes.Internal, ...) // deny on error
         }
         cache.Set(workspaceID, snap)
     }
   WHY: Missing snapshot = not yet compiled = deny. Never default-allow.

5. CACHE INVALIDATE AFTER MUTATION (Go policy)
     err = store.AssignResourceToGroup(ctx, ...)
     if err != nil { return err }
     notifier.NotifyPolicyChange(ctx, workspaceID)  // always after successful commit
   WHY: Ensures Connectors get updated snapshots on next heartbeat.

6. PROXY PATTERN (Connector → Controller for Shield)
   Shield calls Connector's agent_server. Connector proxies to Controller.
     // In agent_server.rs, Enroll():
     let mut controller_client = ShieldServiceClient::new(self.controller_channel.clone());
     let response = controller_client.enroll(request).await?;
     return Ok(Response::new(response.into_inner()));
   WHY: Shields don't have direct network access to Controller.
        The Connector is the network bridge.

7. NFTABLES ATOMIC FLUSH (Rust, network.rs)
     // NEVER append rules — always flush + rebuild atomically:
     nft flush chain ip zecurity resource_protect
     nft add rule ... accept  // for each resource
   WHY: Appending risks stale rules surviving across restarts.
        Flushing guarantees the chain reflects exactly current resources.

8. SPIFFE VERIFICATION (Go gRPC interceptor)
     peer, _ := peer.FromContext(ctx)
     tlsInfo := peer.AuthInfo.(credentials.TLSInfo)
     cert := tlsInfo.State.PeerCertificates[0]
     // walk SAN URIs, find spiffe:// prefix, match expected pattern
   WHY: Identity is in the cert, not in a token. This runs before every handler.

9. GRAPHQL MUTATION PATTERN (Go)
     func (r *mutationResolver) CreateGroup(ctx context.Context, input model.CreateGroupInput) (*model.Group, error) {
         claims := requireAuth(ctx)           // extract JWT claims
         row, err := r.policyStore.CreateGroup(ctx, claims.TenantID, input.Name, input.Description)
         if err != nil { return nil, err }
         r.policyNotifier.NotifyPolicyChange(ctx, claims.TenantID)
         return groupRowToGQL(row), nil
     }

10. REACT QUERY + MUTATION PATTERN (TypeScript)
      const [createGroup] = useCreateGroupMutation({
          refetchQueries: [{ query: GetGroupsDocument }],
      });
      // on submit:
      await createGroup({ variables: { input: { name, description } } });
    WHY: refetchQueries triggers a fresh GET_GROUPS fetch after mutation succeeds.


─────────────────────────────────────────────
STEP 13 — GRAPHQL SCHEMA OVERVIEW  ~20 min
─────────────────────────────────────────────

Read controller/graph/schema.graphqls (and policy.graphqls if split).

KEY TYPES:
  RemoteNetwork { id, name, connectors[], shields[], resources[], status }
  Connector     { id, name, status, publicIp, hostname, version, certNotAfter, lastSeenAt }
  Shield        { id, name, status, interfaceAddr, connectorId, lanIp, version, lastSeenAt }
  Resource      { id, name, host, portFrom, portTo, protocol, status, groups[] }
  Group         { id, name, description, members[], resources[] }
  User          { id, email, role }
  ClientDevice  { id, deviceName, os, spiffeId, certNotAfter }

KEY QUERIES:
  me, workspace, remoteNetworks, connectors, shields, resources, groups, users,
  clientDevices, discoveredServices

KEY MUTATIONS:
  createRemoteNetwork, createConnector, generateConnectorToken, revokeConnector
  createShield, generateShieldToken, revokeShield
  createResource, updateResource, deleteResource
  createGroup, updateGroup, deleteGroup
  addGroupMember, removeGroupMember
  assignResourceToGroup, unassignResourceFromGroup
  inviteUser, revokeInvitation, enrollDevice, revokeDevice

HOW CODEGEN WORKS:
  1. You write/change controller/graph/*.graphqls
  2. Run: cd controller && go generate ./graph/...  → regenerates models_gen.go
  3. Run: cd admin && npm run codegen               → regenerates src/generated/graphql.ts
  4. Now TypeScript hooks (useGetGroupsQuery etc.) reflect the new schema.


─────────────────────────────────────────────
STEP 14 — ADMIN UI PAGES REFERENCE
─────────────────────────────────────────────

admin/src/pages/Dashboard.tsx
  SHOWS: active networks, total/active connectors, recent connector list
  QUERIES: Me, GetWorkspace, GetRemoteNetworks
  POLLING: 30s

admin/src/pages/RemoteNetworks.tsx
  SHOWS: network list with health badge (ONLINE/DEGRADED/OFFLINE)
  MUTATIONS: CreateRemoteNetwork, DeleteRemoteNetwork

admin/src/pages/Connectors.tsx
  SHOWS: connector list by network — Status, Hostname, IP, Cert Expiry, Version
  MUTATIONS: GenerateConnectorToken → InstallCommandModal

admin/src/pages/Shields.tsx
  SHOWS: shield list — Status, Interface IP (zecurity0/32), Via (connector), Last Seen
  MUTATIONS: GenerateShieldToken → InstallCommandModal

admin/src/pages/Resources.tsx
  SHOWS: resource list with "groups with access" visible per row
  MUTATIONS: CreateResource, UpdateResource, DeleteResource, AssignResourceToGroup

admin/src/pages/Groups.tsx
  SHOWS: group list — name, member count, resource count
  MUTATIONS: CreateGroup, DeleteGroup

admin/src/pages/GroupDetail.tsx
  TABS: Members (add/remove users), Resources (unassign resources)
  QUERIES: GetGroup(id), GetUsers (for add member picker)

admin/src/pages/ResourceDiscovery.tsx
  SHOWS: discovered services per shield (protocol, port, service name)
  ACTION: RequestScan → triggers scan via ConnectorRegistry → ScanCommand on stream

admin/src/pages/Topology.tsx
  SHOWS: visual diagram of network → connectors → shields → resources

admin/src/components/InstallCommandModal.tsx
  STEP 1: name input → Generate button
  STEP 2: readonly textarea with: curl -fsSL .../install.sh | ENROLLMENT_TOKEN=<jwt> bash
  USED BY: Connectors.tsx, Shields.tsx

admin/src/generated/graphql.ts
  DO NOT EDIT. Regenerate with: cd admin && npm run codegen


─────────────────────────────────────────────
STEP 15 — BUILD + DEV COMMANDS  ~10 min
─────────────────────────────────────────────

  # Proto → Go stubs
  buf generate                                        # from repo root

  # GraphQL codegen
  cd controller && go generate ./graph/...            # → models_gen.go
  cd admin && npm run codegen                         # → src/generated/graphql.ts

  # Build all
  cd controller && go build ./...
  cd connector && cargo build
  cargo build --manifest-path shield/Cargo.toml
  cd client && cargo build
  cd admin && npm run build

  # Run tests
  cd controller && go test ./...

  # Deploy Shield (after build)
  sudo systemctl stop zecurity-shield
  sudo cp shield/target/release/zecurity-shield /usr/local/bin/zecurity-shield
  sudo systemctl start zecurity-shield
  # Note: requires tun kernel module: sudo modprobe tun

  # Local dev
  cd admin && npm run dev        # Vite dev server on :5173
  # Controller: set env vars (DB_URL, REDIS_URL, GOOGLE_CLIENT_ID, etc.)
  cd controller && go run ./cmd/server/


─────────────────────────────────────────────
STEP 16 — FREQUENTLY ASKED "WHY" QUESTIONS
─────────────────────────────────────────────

Q: Why does Shield connect to Connector (:9091), not Controller?
A: Shields are on LAN hosts behind NAT/firewalls. The Connector sits on the
   network edge with a public IP. Shield → Connector is LAN-local. Connector
   relays everything upstream. Controller never needs a path into the LAN.

Q: Why does Shield get connector_addr from the Controller, not from config?
A: The Connector is selected at token-generation time (least-loaded active
   Connector in the network). The Shield must use exactly that Connector.
   Hardcoding in config would break load balancing.

Q: Why two PKCE exchanges in the client login flow?
A: CLI ↔ Controller PKCE: proves the CLI that started the auth is the same
   CLI that completes the token exchange (prevents auth code interception).
   Controller ↔ Google PKCE: standard PKCE for the Google OAuth code exchange.
   Both happen in parallel; neither can see the other's verifier.

Q: Why is the ACL snapshot cached in memory, not in Redis?
A: Policy changes are infrequent and the snapshot is workspace-scoped.
   In-memory is fast and avoids Redis serialization overhead. On restart
   the cache is cold — first miss compiles from DB. ADR-001 documents this.

Q: Why does nftables chain get flushed on every Shield restart?
A: Shield may restart with a different resource set than before. Appending
   to a stale chain would leave orphan rules. Flush + rebuild = correct.

Q: Why is CAP_NET_ADMIN needed by Shield?
A: network.rs calls rtnetlink (kernel syscall) to create the zecurity0 TUN
   device. This is a privileged operation. The systemd unit grants it via
   AmbientCapabilities=CAP_NET_ADMIN.

Q: Why is the Client daemon separate from the CLI (Sprint 8.5)?
A: The daemon holds the active TUN tunnel + live ACL snapshot in memory.
   These cannot exist in a short-lived CLI invocation. ADR-002 explains why
   there is no direct-state fallback — the daemon is required.

Q: Why are enrollment tokens single-use (JTI burn)?
A: A stolen or leaked token should not allow a second enrollment. Once burned,
   the same JWT cannot enroll again even if leaked later.

Q: Why do Connectors not talk to Shields directly?
A: Connector is a relay, not a peer. Shield connects to Connector's
   agent_server. Connector pushes instructions DOWN to Shields and relays
   health reports UP to Controller.


─────────────────────────────────────────────
SPRINT HISTORY QUICK REFERENCE
─────────────────────────────────────────────

Sprint 1-4:  Proto contracts, PKI, enrollment, heartbeat, basic Admin UI
Sprint 5:    Resources + nftables protection, health check, unprotected state
Sprint 6:    Control stream refactor (bidirectional, persistent gRPC stream)
             Resource instructions delivered via stream, not HTTP
Sprint 7:    Client CLI: login, PKCE, device enrollment; discovery service;
             scan command; topology view
Sprint 8:    Policy Engine: Groups, ACL snapshot compiler, cache + notifier,
             ClientService GetACLSnapshot, Frontend Groups UI,
             Connector ACL receive/store (M4 — Phase C in progress)

Upcoming:
Sprint 8.5:  Client daemon foundation (M4) — runtime/tunnel state management
Sprint 9:    RDE (Remote Desktop / data plane) tunnel using local ACL snapshots
