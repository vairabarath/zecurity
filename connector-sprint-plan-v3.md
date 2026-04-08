# Connector Sprint — Full Plan v3
## Rust Connector · Go gRPC Controller · Admin UI · systemd + Docker · Auto-Update

---

## Changelog v2 → v3

```
1. appmeta.go     All SPIFFE constants + trust domain helpers moved here.
                  No package ever hardcodes "zecurity.in", "ws-", "connector", etc.
                  Same pattern as existing PKIRootCACommonName constants.

2. ConnectorConfig  New struct in internal/connector/config.go.
                    All tunable values (TTLs, ports, intervals) read from .env
                    via mustDuration/envOr in main.go.
                    Same pattern as existing auth.Config.

3. Cert validity  Changed from 1 year → 7 days (GROK final instruction).
                  Controlled by CONNECTOR_CERT_TTL in .env — no recompile needed.

4. re_enroll      Added bool re_enroll = 3 to HeartbeatResponse proto.
                  Controller always returns false this sprint.
                  Field is plumbed now so next sprint renewal needs no proto change.

5. src/appmeta.rs New file in Rust connector.
                  Compile-time constants mirroring Go appmeta.go.
                  tls.rs uses appmeta::SPIFFE_CONTROLLER_ID — no string literals
                  in handlers.

6. .env additions CONNECTOR_CERT_TTL, CONNECTOR_ENROLLMENT_TOKEN_TTL,
                  CONNECTOR_HEARTBEAT_INTERVAL, CONNECTOR_DISCONNECT_THRESHOLD,
                  GRPC_PORT added to .env.example.

7. trust_domain   JWT payload uses appmeta.WorkspaceTrustDomain(slug) — not
   derivation     an inline string format. Single source of format truth.

8. Disconnect     Watcher interval driven by cfg.HeartbeatInterval from
   watcher        ConnectorConfig — not a hardcoded 30s literal.
```

---

## All Decisions Locked

```
CA security          Option C — enrollment token contains CA fingerprint (SHA-256)
                     Connector fetches CA cert, verifies fingerprint, aborts if mismatch

Identity             SPIFFE — every cert carries spiffe://<trust_domain>/<role>/<id>
                     All SPIFFE strings originate from appmeta.go (Go) / appmeta.rs (Rust)
                     No package hardcodes trust domain strings directly
                     Controller cert:  spiffe://zecurity.in/controller/global
                     Connector cert:   spiffe://ws-<slug>.zecurity.in/connector/<id>
                     Agent cert:       spiffe://ws-<slug>.zecurity.in/agent/<id>  (future)

Trust domain format  ws-<workspace_slug>.zecurity.in
                     Derived via appmeta.WorkspaceTrustDomain(slug) — never inline
                     Global/vendor domain: zecurity.in (appmeta.SPIFFEGlobalTrustDomain)

Cert validity        7 days — CONNECTOR_CERT_TTL in .env (default: 168h)
                     GROK final instruction — aligns with Zero Trust short-lived certs
                     Next sprint adds auto-renewal via re_enroll flag

Cert auto-renewal    Out of scope this sprint — re_enroll proto field added but
                     controller always returns false. Manual re-enrollment for now.

Auto-update          systemd timer — every 24 hours
                     Source: GitHub releases API (latest tag vs CARGO_PKG_VERSION)
                     Verify: SHA-256 checksum before replacing binary
                     Rollback: backup old binary, health-check after restart, restore if failed
                     Flag: AUTO_UPDATE_ENABLED=true (set false for air-gapped)

Deployment           systemd (primary) + Docker Compose (supported)
Heartbeat            30 seconds — CONNECTOR_HEARTBEAT_INTERVAL in .env
Disconnect threshold 90 seconds — CONNECTOR_DISCONNECT_THRESHOLD in .env
                     Must always be > 3× heartbeat interval (operator responsibility)
Connector keypair    EC P-384 — generated on-device, private key never leaves
Enrollment token TTL 24 hours — CONNECTOR_ENROLLMENT_TOKEN_TTL in .env
Admin UI             Remote Networks page + Connectors page + install command modal
Connector page       name + status + last seen + hostname + version + install button
Rust crates          rcgen, tokio-rustls, tonic, anyhow, tracing, semver, figment,
                     sha2, x509-parser, oid-registry
Connector version    Build-time constant via CARGO_PKG_VERSION — not a config file
Repo structure       Monorepo — connector/ folder inside existing zecurity repo
Distribution         GitHub Actions — musl static binaries on tag push (amd64 + arm64)
```

---

## What Is Hardcoded vs Configurable

```
Value                              Where          Why
─────────────────────────────────────────────────────────────────────────────
"zecurity.in"  (global domain)     appmeta.go     Product identity — a config
                                   appmeta.rs     file must never override this
"ws-" prefix                       appmeta.go     Format is product-defined
".zecurity.in" suffix              appmeta.go     Same
"connector" role string            appmeta.go     Part of identity schema
"agent" role string                appmeta.go     Future — plumbed now
"controller" role string           appmeta.go     Same
SPIFFE_CONTROLLER_ID full URI      appmeta.go     Derived constant, immutable
PKIConnectorCNPrefix "connector-"  appmeta.go     Cert CN format, product-defined

CONNECTOR_CERT_TTL     168h        .env           7 days in prod, 1h in tests
CONNECTOR_ENROLLMENT_TOKEN_TTL 24h .env           Operator may tighten
CONNECTOR_HEARTBEAT_INTERVAL   30s .env           Tunable per network conditions
CONNECTOR_DISCONNECT_THRESHOLD 90s .env           Must stay > 3× heartbeat
GRPC_PORT              9090        .env           Different in dev vs prod
```

---

## Repo Structure

```
zecurity/
  controller/
    appmeta/
      appmeta.go                   ← MODIFIED — add SPIFFE constants + helpers
    proto/
      connector.proto              ← NEW — written first, unblocks everyone
    internal/
      connector/
        config.go                  ← NEW — ConnectorConfig struct
        token.go                   ← NEW — enrollment token gen + Redis jti burn
        enrollment.go              ← NEW — gRPC Enroll handler
        heartbeat.go               ← NEW — gRPC Heartbeat handler + disconnect watcher
        spiffe.go                  ← NEW — parseSPIFFEID() + interceptor + validator
        ca_endpoint.go             ← NEW — HTTP GET /ca.crt
      auth/                        ← UNCHANGED (sprint 1)
      bootstrap/                   ← UNCHANGED (sprint 1)
      pki/
        service.go                 ← UNCHANGED
        crypto.go                  ← UNCHANGED
        root.go                    ← UNCHANGED
        intermediate.go            ← UNCHANGED
        workspace.go               ← MODIFIED — add SignConnectorCert method
    graph/
      schema.graphqls              ← MODIFIED — add connector + remote_network types
      resolvers/
        schema.resolvers.go        ← UNCHANGED (sprint 1)
        connector.resolvers.go     ← NEW — connector GraphQL resolvers
    migrations/
      001_schema.sql               ← UNCHANGED (sprint 1)
      002_connector_schema.sql     ← NEW — remote_networks + connectors tables
    cmd/server/
      main.go                      ← MODIFIED — add ConnectorConfig wiring + gRPC server
    docker-compose.yml             ← UNCHANGED

  connector/                       ← NEW — Rust binary
    Cargo.toml
    build.rs
    src/
      appmeta.rs                   ← NEW — compile-time constants (mirrors appmeta.go)
      main.rs
      config.rs
      enrollment.rs
      heartbeat.rs
      crypto.rs
      tls.rs
      updater.rs
    proto/
    systemd/
      zecurity-connector.service
      zecurity-connector-update.service
      zecurity-connector-update.timer
    scripts/
      connector-install.sh

  admin/                           ← existing React frontend (additions only)
    src/
      pages/
        RemoteNetworks.tsx         ← NEW
        Connectors.tsx             ← NEW
      components/
        InstallCommandModal.tsx    ← NEW
      graphql/
        connector-mutations.graphql ← NEW
        connector-queries.graphql   ← NEW
```

---

## Team Split

```
Member 1   Frontend — React
           Remote Networks page
           Connectors page
           InstallCommandModal
           New GraphQL operation files + codegen
           NO SPIFFE changes — SPIFFE is invisible to the UI

Member 2   Go — Auth + enrollment infrastructure
           connector.proto ← Day 1, written first, unblocks Member 3 + 4
             ↳ includes re_enroll = 3 in HeartbeatResponse
           internal/connector/config.go ← ConnectorConfig struct
           internal/connector/token.go  ← JWT gen using appmeta.WorkspaceTrustDomain
             ↳ trust_domain from appmeta, TTL from cfg.EnrollmentTokenTTL
           internal/connector/ca_endpoint.go ← HTTP GET /ca.crt
           cmd/server/main.go additions:
             ↳ read .env vars → populate ConnectorConfig
             ↳ wire UnarySPIFFEInterceptor into grpc.NewServer
             ↳ start gRPC server on cfg.GRPCPort

Member 3   Go — PKI + gRPC handlers + SPIFFE core
           appmeta/appmeta.go ← add SPIFFE constants + WorkspaceTrustDomain + ConnectorSPIFFEID
           internal/connector/spiffe.go ← parseSPIFFEID + interceptor + validator
             ↳ written first (Day 1) — unblocks Member 2 wiring
           internal/connector/enrollment.go
             ↳ verifies CSR SPIFFE SAN against JWT claims
             ↳ calls pki.SignConnectorCert with trustDomain + cfg.CertTTL
           internal/connector/heartbeat.go
             ↳ reads identity from context (interceptor already parsed it)
             ↳ disconnect watcher uses cfg.HeartbeatInterval, cfg.DisconnectThreshold
           pki/workspace.go ← add SignConnectorCert method
             ↳ 7-day validity from cfg.CertTTL
             ↳ SPIFFE SAN via appmeta.ConnectorSPIFFEID(trustDomain, connectorID)

Member 4   Go — Schema + DB + Migrations
           migrations/002_connector_schema.sql
             ↳ workspaces: add trust_domain column + backfill
             ↳ connectors: new table with trust_domain column
           graph/schema.graphqls ← add connector + remote_network types
           graph/resolvers/connector.resolvers.go ← new resolvers
           Rust — Connector binary
           connector/src/appmeta.rs ← compile-time constants
           connector/src/config.rs  ← ConnectorConfig via figment
           connector/src/enrollment.rs
             ↳ CSR SAN uses appmeta::SPIFFE_ROLE_CONNECTOR
             ↳ trust_domain from JWT, not hardcoded
           connector/src/tls.rs
             ↳ verify_controller_spiffe uses appmeta::SPIFFE_CONTROLLER_ID
           connector/src/heartbeat.rs, crypto.rs, updater.rs, main.rs
           systemd units + connector-install.sh
           GitHub Actions CI
           Docker Compose
```

---

## appmeta/appmeta.go — Member 3 (Day 1)

Add to existing file alongside current constants. Do not remove anything.

```go
package appmeta

// ── Existing constants (sprint 1 — unchanged) ───────────────────────────────
const (
    ProductName               = "ZECURITY"
    ControllerIssuer          = "zecurity-controller"
    PKIPlatformOrganization   = ProductName + " Platform"
    PKIWorkspaceOrganization  = ProductName + " Workspace"
    PKIRootCACommonName       = ProductName + " Root CA"
    PKIIntermediateCommonName = ProductName + " Intermediate CA"
)

// ── SPIFFE identity constants (connector sprint) ─────────────────────────────
//
// These are product-level identity constants. They define what ZECURITY is,
// not how it is deployed. They must NOT be overridable via config files or
// environment variables — a compromised config must not be able to redirect
// identity trust to a rogue domain.
//
// All packages that need SPIFFE strings import these. No package writes
// "zecurity.in", "ws-", or "connector" as a string literal directly.
const (
    // SPIFFEGlobalTrustDomain is the vendor-level trust domain.
    // Used for the controller certificate and as the root of all workspace
    // trust domains.
    SPIFFEGlobalTrustDomain = "zecurity.in"

    // SPIFFEControllerID is the full SPIFFE URI embedded in the controller's
    // TLS certificate. The Rust connector verifies this on every mTLS handshake.
    SPIFFEControllerID = "spiffe://" + SPIFFEGlobalTrustDomain + "/controller/global"

    // SPIFFETrustDomainPrefix and SPIFFETrustDomainSuffix form workspace trust
    // domains. Use WorkspaceTrustDomain(slug) — never concatenate manually.
    SPIFFETrustDomainPrefix = "ws-"
    SPIFFETrustDomainSuffix = "." + SPIFFEGlobalTrustDomain

    // SPIFFE role path segments — verified by UnarySPIFFEInterceptor.
    SPIFFERoleConnector  = "connector"
    SPIFFERoleAgent      = "agent"       // future sprint — plumbed now
    SPIFFERoleController = "controller"

    // PKI cert subject CN prefixes — keeps cert naming consistent.
    PKIConnectorCNPrefix = "connector-" // CN = "connector-<connectorID>"
    PKIAgentCNPrefix     = "agent-"     // CN = "agent-<agentID>" — future
)

// WorkspaceTrustDomain derives the SPIFFE trust domain for a workspace.
//
// Example: slug "acme" → "ws-acme.zecurity.in"
//
// Every package that needs a workspace trust domain calls this function.
// The format is defined once here — nowhere else.
func WorkspaceTrustDomain(slug string) string {
    return SPIFFETrustDomainPrefix + slug + SPIFFETrustDomainSuffix
}

// ConnectorSPIFFEID builds the full SPIFFE URI for a connector certificate.
//
// Example: trustDomain "ws-acme.zecurity.in", connectorID "abc-123"
//       → "spiffe://ws-acme.zecurity.in/connector/abc-123"
//
// Used in SignConnectorCert (Go) and enrollment.rs (Rust, mirrored).
func ConnectorSPIFFEID(trustDomain, connectorID string) string {
    return "spiffe://" + trustDomain + "/" + SPIFFERoleConnector + "/" + connectorID
}
```

---

## internal/connector/config.go — Member 2

New file. Same pattern as `internal/auth/config.go` from sprint 1.
All tunable values live here. No handler or PKI function hardcodes durations.

```go
package connector

import "time"

// Config holds all tunable settings for the connector subsystem.
// Populated in main.go from environment variables.
// Passed into every service and handler that needs these values.
//
// Rule: if a value might differ between dev, staging, and prod,
// it belongs here — not hardcoded in a handler.
type Config struct {
    // CertTTL is the validity window for connector leaf certificates.
    // GROK final instruction: 7 days (168h). Operator can reduce for testing.
    // Env: CONNECTOR_CERT_TTL (default: 168h)
    CertTTL time.Duration

    // EnrollmentTokenTTL is the Redis TTL for single-use enrollment JWTs.
    // Env: CONNECTOR_ENROLLMENT_TOKEN_TTL (default: 24h)
    EnrollmentTokenTTL time.Duration

    // HeartbeatInterval is how often connectors are expected to heartbeat.
    // The disconnect watcher uses this to derive the check cadence.
    // Env: CONNECTOR_HEARTBEAT_INTERVAL (default: 30s)
    HeartbeatInterval time.Duration

    // DisconnectThreshold is how long without a heartbeat before a connector
    // is marked DISCONNECTED. Must always be > 3 × HeartbeatInterval.
    // Env: CONNECTOR_DISCONNECT_THRESHOLD (default: 90s)
    DisconnectThreshold time.Duration

    // GRPCPort is the port the gRPC server listens on.
    // Env: GRPC_PORT (default: "9090" dev / "8443" prod)
    GRPCPort string

    // JWTSecret is reused from the existing auth config — same secret,
    // used to sign and verify enrollment tokens.
    // Env: JWT_SECRET (already required from sprint 1)
    JWTSecret string
}
```

---

## cmd/server/main.go additions — Member 2

Follows the exact same `mustEnv` / `envOr` / `mustDuration` pattern
already used in sprint 1 to populate `auth.Config`.

```go
// Add mustDuration helper alongside existing mustEnv / envOr:
func mustDuration(key string, fallback time.Duration) time.Duration {
    v := os.Getenv(key)
    if v == "" {
        return fallback
    }
    d, err := time.ParseDuration(v)
    if err != nil {
        log.Fatalf("env var %s is not a valid duration: %s", key, v)
    }
    return d
}

// ConnectorConfig — populated from .env, same pattern as auth.Config:
connectorCfg := connector.Config{
    CertTTL:             mustDuration("CONNECTOR_CERT_TTL",              7*24*time.Hour),
    EnrollmentTokenTTL:  mustDuration("CONNECTOR_ENROLLMENT_TOKEN_TTL",  24*time.Hour),
    HeartbeatInterval:   mustDuration("CONNECTOR_HEARTBEAT_INTERVAL",    30*time.Second),
    DisconnectThreshold: mustDuration("CONNECTOR_DISCONNECT_THRESHOLD",  90*time.Second),
    GRPCPort:            envOr("GRPC_PORT", "9090"),
    JWTSecret:           mustEnv("JWT_SECRET"), // reuses existing required env var
}

// gRPC server — wires SPIFFE interceptor:
grpcServer := grpc.NewServer(
    grpc.Creds(tlsCreds),
    grpc.UnaryInterceptor(
        connector.UnarySPIFFEInterceptor(
            connector.NewTrustDomainValidator(
                appmeta.SPIFFEGlobalTrustDomain, // from appmeta — not a string literal
                workspaceStore,
            ),
        ),
    ),
)
pb.RegisterConnectorServiceServer(grpcServer, connector.NewService(connectorCfg, db, pkiSvc, redis))
go grpcServer.Serve(grpcListener)
```

---

## .env additions (full updated .env.example)

Existing vars from sprint 1 are unchanged. New vars added at bottom.

```env
# ── Sprint 1 (unchanged) ────────────────────────────────────────────────────
DATABASE_URL=postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform
REDIS_URL=redis://localhost:6379
PORT=8080
ENV=development

JWT_SECRET=replace_with_32_plus_random_bytes
JWT_ISSUER=zecurity-controller

GOOGLE_CLIENT_ID=your_google_client_id
GOOGLE_CLIENT_SECRET=your_google_client_secret
GOOGLE_REDIRECT_URI=http://localhost:8080/auth/callback

PKI_MASTER_SECRET=replace_with_64_plus_random_bytes
ALLOWED_ORIGIN=http://localhost:5173

# ── Connector sprint additions ───────────────────────────────────────────────

# gRPC server port (9090 dev / 8443 prod)
GRPC_PORT=9090

# Connector certificate validity — 7 days per GROK final instruction.
# Set to 1h or 24h for faster expiry testing during development.
CONNECTOR_CERT_TTL=168h

# Enrollment JWT Redis TTL — single-use token lifetime.
CONNECTOR_ENROLLMENT_TOKEN_TTL=24h

# Heartbeat and disconnect — must satisfy: DISCONNECT_THRESHOLD > 3 × HEARTBEAT_INTERVAL
CONNECTOR_HEARTBEAT_INTERVAL=30s
CONNECTOR_DISCONNECT_THRESHOLD=90s
```

---

## The Proto File — Member 2 (Day 1)

Written first. Commits before anyone else starts.
SPIFFE lives in the X.509 certificate — proto messages are unchanged from v1
except the addition of `re_enroll` to `HeartbeatResponse`.

```protobuf
// controller/proto/connector.proto
syntax = "proto3";

package connector;
option go_package = "github.com/yourorg/zecurity/controller/proto/connector";

service ConnectorService {

  // Called once during enrollment.
  // Uses plain TLS — connector has no cert yet.
  // Connector presents enrollment JWT + PKCS#10 CSR.
  // CSR SAN must be: spiffe://<trust_domain>/connector/<connector_id>
  // Controller returns signed cert (7-day validity) + CA chain.
  rpc Enroll(EnrollRequest) returns (EnrollResponse);

  // Called every CONNECTOR_HEARTBEAT_INTERVAL seconds after enrollment.
  // Uses mTLS — connector presents its SPIFFE-certified cert.
  // Authoritative identity comes from the cert SPIFFE ID, not request body.
  // connector_id in request body is used for logging only.
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);
}

message EnrollRequest {
  string enrollment_token = 1;  // signed JWT — contains trust_domain + ca_fingerprint
  bytes  csr_der          = 2;  // DER-encoded PKCS#10 CSR (EC P-384)
                                // SAN: spiffe://<trust_domain>/connector/<id>
  string version          = 3;  // CARGO_PKG_VERSION
  string hostname         = 4;
}

message EnrollResponse {
  bytes  certificate_pem      = 1;  // signed leaf cert — 7-day validity, SPIFFE SAN
  bytes  workspace_ca_pem     = 2;  // WorkspaceCA cert (PEM)
  bytes  intermediate_ca_pem  = 3;  // Intermediate CA cert (PEM)
  string connector_id         = 4;  // confirmed connector UUID
}

message HeartbeatRequest {
  string connector_id = 1;  // for logging only — NOT authoritative identity
  string version      = 2;  // CARGO_PKG_VERSION
  string hostname     = 3;
  string public_ip    = 4;  // optional
}

message HeartbeatResponse {
  bool   ok             = 1;
  string latest_version = 2;  // controller informs connector of latest release
  bool   re_enroll      = 3;  // cert expiring soon → connector should re-enroll
                              // ALWAYS false this sprint — renewal logic next sprint
                              // Field is plumbed now so no proto change is needed later
}
```

After Member 2 commits this:
- Member 3 runs `protoc` → generates Go stubs → writes handlers
- Member 4 adds `tonic-build` to `build.rs` → generates Rust stubs

---

## Database — Member 4

New migration: `002_connector_schema.sql`

### workspaces table update

```sql
-- 002_connector_schema.sql
-- Part 1: extend workspaces (sprint 1 created this table)

-- Each workspace has its own SPIFFE trust domain.
-- Format enforced by appmeta.WorkspaceTrustDomain(slug) in Go.
-- Backfilled here to match: ws-<slug>.zecurity.in
ALTER TABLE workspaces
    ADD COLUMN IF NOT EXISTS trust_domain TEXT UNIQUE;

UPDATE workspaces
   SET trust_domain = 'ws-' || slug || '.zecurity.in'
 WHERE trust_domain IS NULL;

ALTER TABLE workspaces
    ALTER COLUMN trust_domain SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_workspaces_trust_domain
    ON workspaces (trust_domain);
```

### remote_networks table

```sql
-- Part 2: new tables

CREATE TABLE remote_networks (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    location    TEXT        NOT NULL
                            CHECK (location IN (
                              'home','office','aws','gcp','azure','other'
                            )),
    status      TEXT        NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active','deleted')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, name)
);

CREATE INDEX idx_remote_networks_tenant ON remote_networks (tenant_id);
```

### connectors table

```sql
CREATE TABLE connectors (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    remote_network_id    UUID        NOT NULL REFERENCES remote_networks(id) ON DELETE CASCADE,
    name                 TEXT        NOT NULL,

    -- Status lifecycle:
    --   pending      → token generated, not yet enrolled
    --   active       → enrolled and heartbeating
    --   disconnected → no heartbeat for CONNECTOR_DISCONNECT_THRESHOLD
    --   revoked      → rejected on next heartbeat attempt
    status               TEXT        NOT NULL DEFAULT 'pending'
                                     CHECK (status IN (
                                       'pending','active','disconnected','revoked'
                                     )),

    -- Single-use enrollment token handle.
    -- Stored so Enroll handler can look up which connector a token belongs to.
    -- Set NULL after enrollment completes.
    enrollment_token_jti TEXT,

    -- SPIFFE trust domain for this connector.
    -- Derived from workspace slug via appmeta.WorkspaceTrustDomain at token
    -- generation time. Written to DB at enrollment completion.
    -- Used by heartbeat handler to resolve tenant from SPIFFE ID.
    trust_domain         TEXT,

    -- Set after successful enrollment
    cert_serial          TEXT,
    cert_not_after       TIMESTAMPTZ,

    -- Updated on every heartbeat
    last_heartbeat_at    TIMESTAMPTZ,
    version              TEXT,
    hostname             TEXT,
    public_ip            TEXT,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_connectors_tenant         ON connectors (tenant_id);
CREATE INDEX idx_connectors_remote_network ON connectors (remote_network_id, tenant_id);
CREATE INDEX idx_connectors_token_jti      ON connectors (enrollment_token_jti);
CREATE INDEX idx_connectors_trust_domain   ON connectors (trust_domain);
```

---

## GraphQL Schema Update — Member 4

Add to existing `schema.graphqls`. Existing `me`, `workspace`, `initiateAuth` unchanged.

```graphql
type RemoteNetwork {
  id:         ID!
  name:       String!
  location:   NetworkLocation!
  status:     RemoteNetworkStatus!
  connectors: [Connector!]!
  createdAt:  String!
}

enum NetworkLocation { HOME  OFFICE  AWS  GCP  AZURE  OTHER }
enum RemoteNetworkStatus { ACTIVE  DELETED }

type Connector {
  id:              ID!
  name:            String!
  status:          ConnectorStatus!
  remoteNetworkId: ID!
  lastSeenAt:      String
  version:         String
  hostname:        String
  publicIp:        String
  certNotAfter:    String
  createdAt:       String!
}

enum ConnectorStatus { PENDING  ACTIVE  DISCONNECTED  REVOKED }

# Returned once — installCommand is shown once, never stored.
type ConnectorToken {
  connectorId:    ID!
  installCommand: String!
}

# Add to existing Mutation type:
createRemoteNetwork(name: String!, location: NetworkLocation!): RemoteNetwork!
deleteRemoteNetwork(id: ID!): Boolean!
generateConnectorToken(remoteNetworkId: ID!, connectorName: String!): ConnectorToken!
revokeConnector(id: ID!): Boolean!
deleteConnector(id: ID!): Boolean!

# Add to existing Query type:
remoteNetworks: [RemoteNetwork!]!
remoteNetwork(id: ID!): RemoteNetwork
connectors(remoteNetworkId: ID!): [Connector!]!
```

---

## Enrollment Token Design — Member 2

### JWT payload

```json
{
  "jti":            "random-uuid",
  "connector_id":   "uuid-of-connector-row",
  "workspace_id":   "uuid-of-workspace",
  "trust_domain":   "ws-acme.zecurity.in",
  "ca_fingerprint": "sha256-hex-of-intermediate-ca-cert-DER",
  "iss":            "zecurity-controller",
  "exp":            "now + CONNECTOR_ENROLLMENT_TOKEN_TTL"
}
```

`trust_domain` is derived in `token.go` using `appmeta.WorkspaceTrustDomain(workspace.Slug)`.
Never concatenated inline. The `iss` claim must equal `appmeta.ControllerIssuer`.

### Single-use burn

```
generateConnectorToken mutation:
  1. INSERT connector row (status='pending', name, remote_network_id, tenant_id)
  2. Load workspace → trust_domain = appmeta.WorkspaceTrustDomain(workspace.Slug)
  3. Generate random jti (UUID v4)
  4. SET enrollment:jti:<jti> <connector_id> Redis TTL=cfg.EnrollmentTokenTTL
  5. Sign JWT:
       iss = appmeta.ControllerIssuer
       exp = now + cfg.EnrollmentTokenTTL
       all claims including trust_domain
  6. Build install command string
  7. Return ConnectorToken { connectorId, installCommand }
  JWT never stored anywhere — jti is the only handle in Redis

Enroll gRPC handler:
  1. Verify JWT signature (cfg.JWTSecret), exp, iss == appmeta.ControllerIssuer
  2. Extract jti, connector_id, workspace_id, trust_domain from claims
  3. GET+DEL jti from Redis atomically
     NOT FOUND → PERMISSION_DENIED ("token expired or already used")
  4. Load connector row: verify status='pending', tenant matches workspace_id
     FAIL → PERMISSION_DENIED
  5. Verify workspace status='active'
     FAIL → FAILED_PRECONDITION
  6. Parse + verify CSR (see Enroll Handler section)
```

### Install command format

```bash
curl -fsSL https://github.com/yourorg/zecurity/releases/latest/download/connector-install.sh | \
  sudo \
  CONTROLLER_ADDR=controller.example.com:8443 \
  ENROLLMENT_TOKEN=eyJhbGci... \
  bash
```

---

## SPIFFE Core — Member 3 (new file: `spiffe.go`, written Day 1)

All SPIFFE logic lives in this one file.
No other Go file parses SPIFFE IDs or duplicates trust domain validation.

```go
// internal/connector/spiffe.go
package connector

// parseSPIFFEID extracts trust domain, role, and entity ID from an X.509 cert.
//
// Expected URI SAN format: spiffe://<trust_domain>/<role>/<id>
// Example: spiffe://ws-acme.zecurity.in/connector/abc-123
//
// Returns an error if:
//   - cert has != 1 URI SAN
//   - URI scheme is not "spiffe"
//   - path does not have exactly two segments (/<role>/<id>)
func parseSPIFFEID(cert *x509.Certificate) (trustDomain, role, id string, err error) {
    if len(cert.URIs) != 1 {
        return "", "", "", fmt.Errorf(
            "expected exactly 1 URI SAN, got %d", len(cert.URIs))
    }
    uri := cert.URIs[0]
    if uri.Scheme != "spiffe" {
        return "", "", "", fmt.Errorf(
            "SAN URI scheme must be spiffe, got %s", uri.Scheme)
    }
    trustDomain = uri.Host
    parts := strings.Split(strings.TrimPrefix(uri.Path, "/"), "/")
    if len(parts) != 2 {
        return "", "", "", fmt.Errorf(
            "SPIFFE path must be /<role>/<id>, got %s", uri.Path)
    }
    return trustDomain, parts[0], parts[1], nil
}

// UnarySPIFFEInterceptor validates SPIFFE identity on every gRPC call except Enroll.
//
// For Enroll: skipped — connector has no cert yet at enrollment time.
// For all other RPCs:
//   1. Extracts peer cert from mTLS context
//   2. Parses SPIFFE ID via parseSPIFFEID
//   3. Validates trust domain against known workspaces + global domain
//   4. Injects trustDomain, role, and entityID into context
//
// Handlers read identity from context — they never re-parse the cert.
// When Agent RPCs are added next sprint, this interceptor covers them automatically.
func UnarySPIFFEInterceptor(validator TrustDomainValidator) grpc.UnaryServerInterceptor {
    return func(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo,
        handler grpc.UnaryHandler) (interface{}, error) {

        if info.FullMethod == "/connector.ConnectorService/Enroll" {
            return handler(ctx, req)
        }

        p, ok := peer.FromContext(ctx)
        if !ok {
            return nil, status.Error(codes.Unauthenticated, "no peer info")
        }
        tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
        if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
            return nil, status.Error(codes.Unauthenticated, "no peer certificate")
        }
        cert := tlsInfo.State.PeerCertificates[0]

        trustDomain, role, id, err := parseSPIFFEID(cert)
        if err != nil {
            return nil, status.Error(codes.Unauthenticated, err.Error())
        }
        if !validator(trustDomain) {
            return nil, status.Error(codes.PermissionDenied,
                "unknown trust domain: "+trustDomain)
        }

        ctx = context.WithValue(ctx, spiffeIDKey{},       cert.URIs[0].String())
        ctx = context.WithValue(ctx, spiffeRoleKey{},     role)
        ctx = context.WithValue(ctx, spiffeEntityIDKey{}, id)
        ctx = context.WithValue(ctx, trustDomainKey{},    trustDomain)
        return handler(ctx, req)
    }
}

// TrustDomainValidator is a function that returns true if a trust domain
// is known and valid for this controller.
type TrustDomainValidator func(domain string) bool

// NewTrustDomainValidator returns a validator that accepts:
//   - appmeta.SPIFFEGlobalTrustDomain ("zecurity.in") — the controller domain
//   - any active workspace's trust domain (looked up via WorkspaceStore)
//
// globalDomain should always be passed as appmeta.SPIFFEGlobalTrustDomain.
func NewTrustDomainValidator(globalDomain string, store WorkspaceStore) TrustDomainValidator {
    return func(domain string) bool {
        if domain == globalDomain {
            return true
        }
        _, err := store.GetByTrustDomain(domain)
        return err == nil
    }
}
```

---

## Controller — Enroll Handler (Member 3)

```
Receive EnrollRequest:
  1. Verify JWT signature (cfg.JWTSecret), exp > now,
     iss == appmeta.ControllerIssuer
  2. Extract jti, connector_id, workspace_id, trust_domain from claims
  3. GET+DEL jti from Redis atomically
     NOT FOUND → PERMISSION_DENIED ("token expired or already used")
  4. Load connector row:
     verify status='pending', tenant_id matches workspace_id
     FAIL → PERMISSION_DENIED
  5. Verify workspace status='active'
     FAIL → FAILED_PRECONDITION
  6. Parse CSR from request.csr_der
  7. Verify CSR self-signature (proves connector holds the private key)
  8. Verify CSR SPIFFE SAN:
       expected = appmeta.ConnectorSPIFFEID(trust_domain, connector_id)
       actual   = first URI SAN in CSR
       MISMATCH → PERMISSION_DENIED ("SPIFFE ID in CSR does not match token")
       This ensures the connector built its CSR from the correct trust_domain
       and connector_id — not a forged or replayed identity.
  9. Call pki.SignConnectorCert(ctx, tenantID, connectorID, trustDomain, csr, cfg.CertTTL)
       → WorkspaceCA signs a leaf cert
       → SAN:          appmeta.ConnectorSPIFFEID(trustDomain, connectorID)
       → CN:           appmeta.PKIConnectorCNPrefix + connectorID
       → KeyUsage:     DigitalSignature
       → ExtKeyUsage:  ClientAuth
       → IsCA:         false
       → Validity:     cfg.CertTTL (7 days / 168h from CONNECTOR_CERT_TTL)
 10. UPDATE connectors SET
       status='active',
       trust_domain=trust_domain,
       cert_serial=<hex>,
       cert_not_after=<expiry>,
       hostname=request.hostname,
       version=request.version,
       last_heartbeat_at=NOW(),
       enrollment_token_jti=NULL
     WHERE id=connector_id AND tenant_id=workspace_id
 11. Return EnrollResponse:
       certificate_pem      ← 7-day cert with SPIFFE SAN
       workspace_ca_pem
       intermediate_ca_pem
       connector_id
```

---

## Controller — Heartbeat Handler (Member 3)

The interceptor has already validated the SPIFFE ID before this runs.
Handler reads from context — zero cert parsing duplication.

```
Receive HeartbeatRequest (mTLS — interceptor already ran):
  1. Read from context (injected by UnarySPIFFEInterceptor):
       trustDomain  = ctx.Value(trustDomainKey{})
       role         = ctx.Value(spiffeRoleKey{})
       connectorID  = ctx.Value(spiffeEntityIDKey{})
  2. Verify role == appmeta.SPIFFERoleConnector
     FAIL → PERMISSION_DENIED ("not a connector cert")
  3. Resolve tenantID:
       SELECT tenant_id FROM connectors
        WHERE id = connectorID AND trust_domain = trustDomain
       NOT FOUND → PERMISSION_DENIED
  4. Verify not revoked:
       SELECT status FROM connectors
        WHERE id = connectorID AND tenant_id = tenantID
       status = 'revoked' → PERMISSION_DENIED ("connector has been revoked")
  5. UPDATE connectors SET
       last_heartbeat_at = NOW(),
       version           = request.version,
       hostname          = request.hostname,
       public_ip         = request.public_ip,
       status            = 'active',
       updated_at        = NOW()
     WHERE id = connectorID AND tenant_id = tenantID
  6. Return HeartbeatResponse:
       ok             = true
       latest_version = <cached from GitHub API or env var>
       re_enroll      = false   ← always false this sprint
                                   next sprint: true when cert_not_after < now + renewal_window

Note: request.connector_id is used for tracing/logging only.
      It is never used as an authoritative identity claim.
```

---

## Controller — Disconnect Watcher (Member 3)

Background goroutine. Starts with gRPC server. Uses `cfg` values — no literals.

```go
// Runs every cfg.HeartbeatInterval.
// Marks connectors DISCONNECTED after cfg.DisconnectThreshold without a heartbeat.
func (s *service) runDisconnectWatcher(ctx context.Context) {
    ticker := time.NewTicker(s.cfg.HeartbeatInterval)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            _, err := s.db.Exec(ctx, `
                UPDATE connectors
                   SET status     = 'disconnected',
                       updated_at = NOW()
                 WHERE status = 'active'
                   AND last_heartbeat_at < NOW() - $1
                   AND tenant_id IN (
                       SELECT id FROM workspaces WHERE status = 'active'
                   )
            `, s.cfg.DisconnectThreshold)
            if err != nil {
                tracing.Error("disconnect watcher query failed", err)
            }
        }
    }
}
```

---

## PKI Extension — Member 3 (`pki/workspace.go`)

New method added to existing `pki.Service`. Same WorkspaceCA key loading
pattern as `GenerateWorkspaceCA` from sprint 1.

```go
// SignConnectorCert issues a 7-day leaf certificate for a connector.
//
// The certificate carries a single SPIFFE URI SAN built via
// appmeta.ConnectorSPIFFEID. This is the connector's cryptographic identity
// for all mTLS connections to the controller.
//
// Parameters:
//   tenantID    — workspace UUID (used to load + decrypt the WorkspaceCA key)
//   connectorID — connector UUID (embedded in SPIFFE ID and CN)
//   trustDomain — SPIFFE trust domain (from enrollment JWT, via appmeta.WorkspaceTrustDomain)
//   csr         — parsed PKCS#10 CSR (self-signature already verified by caller)
//   certTTL     — validity window, from ConnectorConfig.CertTTL (default 7 days)
func (s *service) SignConnectorCert(
    ctx         context.Context,
    tenantID    string,
    connectorID string,
    trustDomain string,
    csr         *x509.CertificateRequest,
    certTTL     time.Duration,
) (*ConnectorCertResult, error) {

    // Build the SPIFFE URI — single source of format via appmeta.
    spiffeID := appmeta.ConnectorSPIFFEID(trustDomain, connectorID)
    uri, err := url.Parse(spiffeID)
    if err != nil {
        return nil, fmt.Errorf("pki: invalid SPIFFE ID %q: %w", spiffeID, err)
    }

    now := time.Now().UTC()
    cert := &x509.Certificate{
        SerialNumber: newSerial(),
        Subject: pkix.Name{
            CommonName:   appmeta.PKIConnectorCNPrefix + connectorID,
            Organization: []string{appmeta.PKIWorkspaceOrganization},
        },
        URIs:      []*url.URL{uri},      // single SPIFFE SAN — no custom URI:connector: SANs
        NotBefore: now,
        NotAfter:  now.Add(certTTL),     // 7 days from ConnectorConfig.CertTTL
        KeyUsage:  x509.KeyUsageDigitalSignature,
        ExtKeyUsage: []x509.ExtKeyUsage{
            x509.ExtKeyUsageClientAuth,  // mTLS client identity
        },
        IsCA: false,
    }

    // Load + decrypt WorkspaceCA key (same pattern as GenerateWorkspaceCA).
    // Key is zeroed from memory after signing.
    workspaceCA, caKey, err := s.loadWorkspaceCA(ctx, tenantID)
    if err != nil {
        return nil, fmt.Errorf("pki: load workspace CA: %w", err)
    }
    defer zeroKey(caKey)

    certDER, err := x509.CreateCertificate(rand.Reader, cert, workspaceCA, csr.PublicKey, caKey)
    if err != nil {
        return nil, fmt.Errorf("pki: sign connector cert: %w", err)
    }

    return &ConnectorCertResult{
        CertificatePEM: pemEncode("CERTIFICATE", certDER),
        Serial:         cert.SerialNumber.Text(16),
        NotBefore:      cert.NotBefore,
        NotAfter:       cert.NotAfter,
    }, nil
}
```

---

## Rust Connector — Member 4

### connector/src/appmeta.rs (NEW — written first)

Mirrors `appmeta.go`. All identity strings originate here.
No `tls.rs`, `enrollment.rs`, or any other file contains literal strings
for SPIFFE domains or roles.

```rust
// src/appmeta.rs
//
// Compile-time identity constants for the ZECURITY connector.
// Mirrors controller/appmeta/appmeta.go exactly.
//
// Rule: no other src/ file writes "zecurity.in", "ws-", "connector",
// or any SPIFFE string as a literal. Import from here instead.

/// Global SPIFFE trust domain for the ZECURITY controller.
/// The controller certificate carries spiffe://zecurity.in/controller/global.
pub const SPIFFE_GLOBAL_TRUST_DOMAIN: &str = "zecurity.in";

/// Full SPIFFE URI embedded in the controller's TLS certificate.
/// tls.rs verifies this on every mTLS heartbeat handshake.
/// A cert signed by the right CA but with a different SPIFFE ID is rejected.
pub const SPIFFE_CONTROLLER_ID: &str =
    "spiffe://zecurity.in/controller/global";

/// SPIFFE role path segment for connectors.
/// enrollment.rs uses this when building the CSR SAN URI.
pub const SPIFFE_ROLE_CONNECTOR: &str = "connector";

/// Product name — used in log messages and service descriptions.
pub const PRODUCT_NAME: &str = "ZECURITY";
```

### Cargo.toml

```toml
[package]
name = "zecurity-connector"
version = "0.1.0"
edition = "2021"

[dependencies]
tokio              = { version = "1",    features = ["full"] }
tonic              = { version = "0.11", features = ["tls"] }
prost              = "0.12"
rcgen              = "0.13"
tokio-rustls       = "0.25"
rustls             = "0.23"
x509-parser        = "0.16"
oid-registry       = "0.7"
sha2               = "0.10"
hex                = "0.4"
anyhow             = "1"
tracing            = "1"
tracing-subscriber = "0.3"
figment            = { version = "0.10", features = ["env", "toml"] }
serde              = { version = "1",    features = ["derive"] }
serde_json         = "1"
semver             = "1"
reqwest            = { version = "0.12", features = ["json", "rustls-tls"] }
tokio-retry        = "0.3"

[build-dependencies]
tonic-build = "0.11"
```

### config.rs

Reads env vars first, then `/etc/zecurity/connector.conf`.
`figment` merges both — env overrides file.

Required:
```
CONTROLLER_ADDR      gRPC host:port  e.g. controller.example.com:8443
ENROLLMENT_TOKEN     required only on first run — removed from config after enrollment
```

Optional with defaults:
```
AUTO_UPDATE_ENABLED  default: true
LOG_LEVEL            default: info
HEARTBEAT_INTERVAL_SECS   default: 30
UPDATE_CHECK_INTERVAL_SECS default: 86400
```

State directory: `/var/lib/zecurity-connector/`
```
connector.key      EC P-384 private key (PEM, 0600, never leaves device)
connector.crt      signed leaf cert (7-day validity, SPIFFE SAN)
workspace_ca.crt   WorkspaceCA + Intermediate CA chain (PEM)
state.json         { connector_id, trust_domain, enrolled_at, cert_not_after }
                   trust_domain is required for controller SPIFFE verification in tls.rs
```

### main.rs — startup logic

```
1. Init tracing (LOG_LEVEL from config)
2. Load config via figment (env → file merge)
3. Check /var/lib/zecurity-connector/state.json:
   NOT EXISTS → run enrollment flow
   EXISTS     → load state (connector_id + trust_domain), go to heartbeat loop

── Enrollment flow (enrollment.rs) ────────────────────────────────────────────
  a. Parse JWT payload (base64-decode middle segment, no signature verify —
     connector has no JWT_SECRET; authenticity proven via ca_fingerprint check)
  b. Extract: connector_id, workspace_id, trust_domain, ca_fingerprint, jti
  c. GET http://<CONTROLLER_ADDR>/ca.crt  (plain HTTP — bootstrap trust problem)
  d. SHA-256 of fetched CA cert DER bytes
  e. Compare hex against ca_fingerprint from JWT
     MISMATCH → tracing::error!("CA fingerprint mismatch — aborting (possible MITM)")
                exit(1)
     MATCH    → trust this cert for the Enroll TLS connection
  f. Generate EC P-384 keypair
     Save private key to connector.key (mode 0600)
  g. Build CSR:
       CN:      format!("{}{}", appmeta::PKI_CONNECTOR_CN_PREFIX, connector_id)
                (defined in appmeta.rs — not a string literal here)
       SAN URI: format!("spiffe://{}/{}/{}", trust_domain,
                        appmeta::SPIFFE_ROLE_CONNECTOR, connector_id)
                (trust_domain from JWT, role from appmeta::SPIFFE_ROLE_CONNECTOR)
  h. Connect to controller gRPC — plain TLS, trust root = fetched Intermediate CA
     (not mTLS yet — connector has no cert)
  i. Call Enroll { enrollment_token, csr_der, version, hostname }
  j. Receive EnrollResponse { certificate_pem, workspace_ca_pem, intermediate_ca_pem }
  k. Save:
       connector.crt    ← 7-day cert with SPIFFE SAN
       workspace_ca.crt ← workspace_ca_pem + "\n" + intermediate_ca_pem
       state.json       ← { connector_id, trust_domain, enrolled_at, cert_not_after }
  l. Remove ENROLLMENT_TOKEN from /etc/zecurity/connector.conf
     Write CONNECTOR_ID=<id> to config
  m. tracing::info!("enrollment complete connector_id={}", connector_id)
─────────────────────────────────────────────────────────────────────────────

4. tokio::spawn(heartbeat_loop(state, cfg))
5. If AUTO_UPDATE_ENABLED: tokio::spawn(update_loop(cfg))
6. Wait for SIGTERM / ctrl_c → graceful shutdown
```

### enrollment.rs — CSR SAN (SPIFFE)

```rust
use crate::appmeta;

// Build SPIFFE SAN using appmeta constants — no string literals here.
// trust_domain comes from the JWT payload (placed there by the controller
// via appmeta.WorkspaceTrustDomain on the Go side).
let spiffe_id = format!(
    "spiffe://{}/{}/{}",
    trust_domain,                    // from JWT claim
    appmeta::SPIFFE_ROLE_CONNECTOR,  // "connector" — from appmeta.rs
    connector_id                     // from JWT claim
);

let mut params = rcgen::CertificateParams::default();
params.distinguished_name.push(
    rcgen::DnType::CommonName,
    format!("{}{}", appmeta::PKI_CONNECTOR_CN_PREFIX, connector_id),
);
params.subject_alt_names = vec![
    rcgen::SanType::URI(spiffe_id),
];
let key_pair = rcgen::KeyPair::generate_for(&rcgen::PKCS_ECDSA_P384)?;
let csr = params.serialize_request(&key_pair)?;
```

### tls.rs — controller SPIFFE verification

```rust
use crate::appmeta;

/// Verifies the controller's TLS certificate carries the expected SPIFFE ID.
///
/// Expected: appmeta::SPIFFE_CONTROLLER_ID = "spiffe://zecurity.in/controller/global"
///
/// Called after every mTLS handshake for Heartbeat connections.
/// Prevents a rogue server (e.g. a connector cert signed by the same WorkspaceCA)
/// from impersonating the controller — the SPIFFE role path "controller" would
/// not be present in any connector or agent cert.
///
/// cert_der: DER bytes of the controller's peer certificate from the TLS handshake.
fn verify_controller_spiffe(cert_der: &[u8]) -> anyhow::Result<()> {
    let (_, cert) = x509_parser::parse_x509_certificate(cert_der)?;

    let san_ext = cert
        .get_extension_unique(&oid_registry::OID_X509_EXT_SUBJECT_ALT_NAME)?
        .ok_or_else(|| anyhow::anyhow!("controller cert has no SAN extension"))?;

    let san = match san_ext.parsed_extension() {
        x509_parser::extensions::ParsedExtension::SubjectAlternativeName(s) => s,
        _ => anyhow::bail!("SAN extension could not be parsed"),
    };

    for name in &san.general_names {
        if let x509_parser::extensions::GeneralName::URI(uri) = name {
            // Compare the full SPIFFE URI against the appmeta constant.
            // Any deviation — wrong domain, wrong role, wrong id — fails here.
            if *uri == appmeta::SPIFFE_CONTROLLER_ID {
                return Ok(());
            }
            if uri.starts_with("spiffe://") {
                // It's a SPIFFE URI but wrong value — log for debugging.
                anyhow::bail!(
                    "controller SPIFFE ID mismatch: got {}, want {}",
                    uri, appmeta::SPIFFE_CONTROLLER_ID
                );
            }
        }
    }
    anyhow::bail!("no matching SPIFFE URI found in controller cert")
}
```

### heartbeat.rs

```
Build mTLS config:
  client_cert: connector.crt   (7-day SPIFFE cert)
  client_key:  connector.key
  trust_root:  workspace_ca.crt
  post_handshake: verify_controller_spiffe(peer_cert_der)
                  ← uses appmeta::SPIFFE_CONTROLLER_ID — no string literal

Create tonic Channel with mTLS credentials
Create ConnectorServiceClient

interval = tokio::time::interval(Duration::from_secs(cfg.heartbeat_interval_secs))
consecutive_failures = 0

loop:
  interval.tick().await
  req = HeartbeatRequest {
    connector_id: state.connector_id,   // logging only
    version:      env!("CARGO_PKG_VERSION"),
    hostname:     gethostname(),
    public_ip:    get_public_ip().unwrap_or_default(),
  }

  match client.heartbeat(req).await:
    Ok(resp) →
      consecutive_failures = 0
      if resp.re_enroll:
        // next sprint — cert expiring soon, trigger re-enrollment
        // this sprint: controller always sends false, branch never taken
        tracing::warn!("controller requested re-enrollment — not yet implemented")
      if resp.latest_version != env!("CARGO_PKG_VERSION"):
        tracing::info!("new version available: {}", resp.latest_version)

    Err(e) →
      consecutive_failures += 1
      backoff = min(5 * 2^(failures-1), 60)
      tracing::warn!("heartbeat failed attempt={} backoff={}s", failures, backoff)
      sleep(backoff).await
```

### updater.rs

No SPIFFE changes. Contacts GitHub API over plain HTTPS.

```
If AUTO_UPDATE_ENABLED=false → return
Random startup delay 0–3600s (prevents thundering herd)
Every UPDATE_CHECK_INTERVAL_SECS (default 86400):
  1. GET github.com/releases/latest → parse tag_name
  2. semver compare: latest > current?  NO → continue
  3. Download connector-linux-<arch> + checksums.txt
  4. Verify SHA-256 checksum — MISMATCH → abort, binary unchanged
  5. Backup /usr/bin/connector → replace → systemctl restart
  6. Sleep 10s → systemctl is-active?
     YES → remove backup, log success
     NO  → restore backup, restart, log rollback
```

---

## systemd Units — Member 4 (unchanged from v1)

### zecurity-connector.service

```ini
[Unit]
Description=Zecurity Connector — Zero Trust Relay
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/bin/connector
EnvironmentFile=/etc/zecurity/connector.conf
User=zecurity
Group=zecurity
NoNewPrivileges=yes
ProtectSystem=strict
ProtectHome=tmpfs
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectHostname=yes
ProtectClock=yes
ProtectProc=invisible
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictSUIDSGID=yes
MemoryDenyWriteExecute=yes
LockPersonality=yes
RestrictRealtime=yes
RemoveIPC=yes
SystemCallFilter=@system-service @mount ~@privileged
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW
StateDirectory=zecurity-connector
RuntimeDirectory=zecurity-connector
WorkingDirectory=/var/lib/zecurity-connector
Restart=always
RestartSec=3
TimeoutStartSec=30

[Install]
WantedBy=multi-user.target
```

### zecurity-connector-update.service

```ini
[Unit]
Description=Zecurity Connector Auto-Updater (oneshot)
After=network-online.target

[Service]
Type=oneshot
ExecStart=/usr/bin/connector --check-update
User=root
```

### zecurity-connector-update.timer

```ini
[Unit]
Description=Zecurity Connector Daily Update Check
Requires=zecurity-connector-update.service

[Timer]
OnCalendar=daily
RandomizedDelaySec=3600
Persistent=true

[Install]
WantedBy=timers.target
```

---

## connector-install.sh — Member 4

```
Creates 'zecurity' system user
Fetches /ca.crt from CONTROLLER_HTTP_ADDR
Installs all three systemd units
Enables zecurity-connector + zecurity-connector-update.timer
chown -R zecurity:zecurity /var/lib/zecurity-connector
Config at /etc/zecurity/connector.conf (0600 root:zecurity)
State at /var/lib/zecurity-connector/
-f flag: force overwrite config + remove stale state
```

Config written by script:
```conf
CONTROLLER_ADDR=<from env>
ENROLLMENT_TOKEN=<from env>
AUTO_UPDATE_ENABLED=true
LOG_LEVEL=info
```

Post-enrollment, connector rewrites config:
- Removes ENROLLMENT_TOKEN
- Adds CONNECTOR_ID=<uuid>

### Docker Compose alternative

```yaml
services:
  zecurity-connector:
    image: ghcr.io/yourorg/zecurity/connector:latest
    restart: unless-stopped
    network_mode: host
    cap_add: [NET_ADMIN, NET_RAW]
    volumes:
      - /var/lib/zecurity-connector:/var/lib/zecurity-connector
      - /etc/zecurity:/etc/zecurity:ro
    environment:
      - CONTROLLER_ADDR=controller.example.com:8443
      - ENROLLMENT_TOKEN=eyJ...
      - AUTO_UPDATE_ENABLED=false
```

---

## Admin UI — Member 1 (no SPIFFE changes)

### GraphQL operation files

```graphql
# src/graphql/connector-mutations.graphql

mutation CreateRemoteNetwork($name: String!, $location: NetworkLocation!) {
  createRemoteNetwork(name: $name, location: $location) {
    id name location status createdAt
  }
}

mutation GenerateConnectorToken($remoteNetworkId: ID!, $connectorName: String!) {
  generateConnectorToken(remoteNetworkId: $remoteNetworkId, connectorName: $connectorName) {
    connectorId
    installCommand
  }
}

mutation RevokeConnector($id: ID!) { revokeConnector(id: $id) }
mutation DeleteConnector($id: ID!) { deleteConnector(id: $id) }
```

```graphql
# src/graphql/connector-queries.graphql

query GetRemoteNetworks {
  remoteNetworks {
    id name location status createdAt
    connectors { id name status lastSeenAt version hostname }
  }
}

query GetConnectors($remoteNetworkId: ID!) {
  connectors(remoteNetworkId: $remoteNetworkId) {
    id name status lastSeenAt version hostname publicIp certNotAfter createdAt
  }
}
```

Run `npm run codegen` after adding these files.

### Remote Networks page (`/remote-networks`)

- "Add Network" button → inline modal (name + location dropdown) → `createRemoteNetwork`
- Network cards: name, location badge, active connector count, "View Connectors" link
- Delete button visible only when zero connectors exist

### Connectors page (`/remote-networks/<id>/connectors`)

- Breadcrumb: Remote Networks → [Network Name]
- "Add Connector" → `InstallCommandModal`
- Table: name, status badge, last seen (relative), hostname, version, Revoke/Delete
- Status badges: PENDING=gray, ACTIVE=green, DISCONNECTED=amber, REVOKED=red
- Polls `GetConnectors` every 30 seconds — no manual refresh needed

### InstallCommandModal

Step 1: connector name input → `generateConnectorToken` mutation
Step 2: monospace code block + one-click copy + warning banner
        "This token expires in 24 hours and works only once."

---

## GitHub Actions — Member 4

```yaml
# .github/workflows/connector-release.yml
# Trigger: push tag matching connector-v*

steps:
  1. Checkout
  2. Install Rust stable
  3. Install musl tools
  4. rustup target add x86_64-unknown-linux-musl aarch64-unknown-linux-musl
  5. cargo build --release --target x86_64-unknown-linux-musl
     cargo build --release --target aarch64-unknown-linux-musl
  6. Rename → connector-linux-amd64, connector-linux-arm64
  7. sha256sum connector-linux-amd64 connector-linux-arm64 > checksums.txt
  8. Create GitHub release from tag
  9. Upload: connector-linux-amd64, connector-linux-arm64,
             checksums.txt, connector/scripts/connector-install.sh
```

---

## Dependency Map — What Blocks What

```
Day 1 — two things must land before anyone else can start:

  Member 2 commits connector.proto
    → Member 3: protoc generates Go stubs → writes handlers
    → Member 4: tonic-build in build.rs generates Rust stubs

  Member 3 commits appmeta.go additions + spiffe.go
    → Member 2: wires UnarySPIFFEInterceptor in main.go
    → Member 3: uses parseSPIFFEID in heartbeat.go (no duplication)
    → Member 4: mirrors constants into appmeta.rs

  Member 4 commits 002_connector_schema.sql + schema.graphqls update + codegen
    → Member 3: DB queries in enrollment.go / heartbeat.go
    → Member 1: generated TypeScript hooks available

  Member 1 starts immediately — no backend needed:
    Layout, routing, loading states for RemoteNetworks + Connectors pages
    InstallCommandModal structure
    Waits only for codegen output before wiring real mutations

  Member 2 needs from Member 3:
    spiffe.go (UnarySPIFFEInterceptor) before wiring gRPC server in main.go
    ConnectorConfig agreed before either implements JWT/token code

  Member 4 (Rust) needs:
    connector.proto → generate tonic stubs
    appmeta.rs constants confirmed → enrollment.rs SPIFFE SAN
    /ca.crt endpoint live → test enrollment end-to-end
    state.json trust_domain field → tls.rs controller verification
```

---

## Integration Checklist

```
appmeta + Config
  ✓ appmeta.go SPIFFE constants committed Day 1
  ✓ appmeta.rs mirrors Go constants exactly
  ✓ No package contains "zecurity.in", "ws-", or "connector" as a string literal
  ✓ WorkspaceTrustDomain(slug) used everywhere — never inline concatenation
  ✓ ConnectorSPIFFEID used in SignConnectorCert — not fmt.Sprintf inline
  ✓ ConnectorConfig populated in main.go from .env — no hardcoded durations
  ✓ CONNECTOR_CERT_TTL=168h in .env — changing it requires no recompile

Proto + DB
  ✓ connector.proto committed before any other work starts
  ✓ HeartbeatResponse has re_enroll bool field (always false this sprint)
  ✓ Go stubs generated cleanly (protoc)
  ✓ Rust stubs generated cleanly (tonic-build in build.rs)
  ✓ 002_connector_schema.sql runs on fresh DB without error
  ✓ workspaces.trust_domain column exists and backfilled for existing rows
  ✓ connectors.trust_domain column exists with index
  ✓ All FK constraints valid

Enrollment flow
  ✓ generateConnectorToken creates connector row (status=pending)
  ✓ trust_domain derived via appmeta.WorkspaceTrustDomain(slug)
  ✓ JWT: iss=appmeta.ControllerIssuer, exp=now+cfg.EnrollmentTokenTTL
  ✓ JWT contains: jti, connector_id, workspace_id, trust_domain, ca_fingerprint
  ✓ jti stored in Redis TTL=cfg.EnrollmentTokenTTL
  ✓ Install command correct and complete
  ✓ Connector reads trust_domain from JWT payload
  ✓ Connector fetches /ca.crt over HTTP
  ✓ SHA-256 fingerprint mismatch → exit(1) with clear error
  ✓ EC P-384 keypair generated on device, private key 0600
  ✓ CSR SAN: spiffe://<trust_domain>/connector/<id> built via appmeta constants
  ✓ Controller verifies CSR SPIFFE SAN == appmeta.ConnectorSPIFFEID(trust_domain, id)
  ✓ SignConnectorCert called with cfg.CertTTL (7 days)
  ✓ Signed cert SAN: spiffe://<trust_domain>/connector/<id>
  ✓ Signed cert validity: 7 days (cfg.CertTTL / CONNECTOR_CERT_TTL)
  ✓ Second Enroll with same token → rejected (jti burned)
  ✓ Connector row: status=active, trust_domain, cert_serial, cert_not_after set
  ✓ state.json: connector_id + trust_domain + enrolled_at + cert_not_after
  ✓ ENROLLMENT_TOKEN removed from config post-enrollment

Heartbeat + SPIFFE
  ✓ mTLS: connector presents 7-day SPIFFE cert
  ✓ UnarySPIFFEInterceptor fires before Heartbeat handler
  ✓ Interceptor uses appmeta.SPIFFEGlobalTrustDomain — not a string literal
  ✓ Interceptor rejects unknown trust domains
  ✓ Interceptor injects trustDomain + role + entityID into context
  ✓ Heartbeat handler reads role from context, compares to appmeta.SPIFFERoleConnector
  ✓ tenantID resolved via trust_domain lookup — not from request body
  ✓ last_heartbeat_at, version, hostname, public_ip updated
  ✓ Disconnect watcher uses cfg.HeartbeatInterval + cfg.DisconnectThreshold
  ✓ Kill connector → DISCONNECTED after cfg.DisconnectThreshold
  ✓ Restart connector → ACTIVE on next heartbeat
  ✓ Workspace A cert rejected on Workspace B controller (trust domain mismatch)
  ✓ HeartbeatResponse.re_enroll always false this sprint — Rust handles field gracefully
  ✓ Rust connector verifies appmeta::SPIFFE_CONTROLLER_ID on every heartbeat connection

Auto-update
  ✓ AUTO_UPDATE_ENABLED=false → no GitHub calls, no updates
  ✓ Already on latest → no download, no restart
  ✓ Checksum mismatch → abort, binary unchanged
  ✓ Checksum match → replace → restart → health check
  ✓ Health check fails → rollback + restore + log
  ✓ Random startup delay 0–3600s working

Security
  ✓ Private key never sent over network
  ✓ Single SPIFFE SAN — no custom URI:connector: or URI:tenant: remnants
  ✓ Trust domain validated on every RPC (not just enrollment)
  ✓ Role verified in handler against appmeta constant
  ✓ Controller identity verified by Rust on every mTLS connection
  ✓ connector.key 0600 owned by zecurity
  ✓ connector.conf 0600
  ✓ ENROLLMENT_TOKEN removed from config after enrollment

Docker
  ✓ Enrolls via ENROLLMENT_TOKEN env var
  ✓ state.json + certs persist across restart via volume mount
  ✓ Without volume mount: re-enrollment on restart (expected, documented)
  ✓ AUTO_UPDATE_ENABLED=false in compose example

End-to-end demo
  ✓ Create Remote Network in dashboard
  ✓ Add Connector → copy install command
  ✓ Run on Linux VM → connector appears ACTIVE within 30s
  ✓ Kill connector → DISCONNECTED within cfg.DisconnectThreshold (90s)
  ✓ Restart → ACTIVE on next heartbeat
  ✓ Revoke in dashboard → next heartbeat rejected → stays REVOKED
  ✓ Workspace A cert rejected on Workspace B controller
```

---

## What Is NOT in This Sprint

```
Certificate auto-renewal     re_enroll proto field is plumbed (always false).
                             Renewal logic is next sprint.
Resource definitions         IP, port, protocol — next sprint
ACL delivery                 Controller → Connector policy push — next sprint
Traffic proxying             TCP/UDP — next sprint
Agent binary                 Resource host enforcement — next sprint
Access policies              Policy engine — next sprint
SPIFFE federation            Cross-workspace trust — out of scope
SPIRE integration            Rolling our own SPIFFE-compatible PKI — out of scope
CRL / OCSP                   Revocation via DB status flag only this sprint
```

At the end of this sprint an admin can:
create a remote network, deploy a connector on any Linux server,
see it appear ACTIVE in the dashboard within 30 seconds,
and watch it go DISCONNECTED if the server goes offline.

Every identity in the system is SPIFFE-standard and centrally defined
in `appmeta.go` / `appmeta.rs`. No magic strings in handlers.
When Agent is added next sprint, the interceptor and trust domain
validator cover it with zero changes to existing connector code.

Traffic does not flow yet. That is the next sprint.

---

## Summary

```
Member 1  RemoteNetworks.tsx + Connectors.tsx + InstallCommandModal.tsx
          connector-mutations.graphql + connector-queries.graphql + codegen
          NO SPIFFE changes

Member 2  connector.proto — Day 1, unblocks Member 3 + 4
            ↳ HeartbeatResponse includes re_enroll = false (always this sprint)
          internal/connector/config.go — ConnectorConfig struct
          internal/connector/token.go — JWT uses appmeta.WorkspaceTrustDomain
            ↳ TTL from cfg.EnrollmentTokenTTL, iss from appmeta.ControllerIssuer
          internal/connector/ca_endpoint.go — HTTP GET /ca.crt
          cmd/server/main.go — mustDuration helper + ConnectorConfig wiring
            ↳ UnarySPIFFEInterceptor wired with appmeta.SPIFFEGlobalTrustDomain

Member 3  appmeta/appmeta.go — SPIFFE constants + WorkspaceTrustDomain + ConnectorSPIFFEID
            ↳ Day 1, written before anything else
          internal/connector/spiffe.go — parseSPIFFEID + interceptor + validator
            ↳ Day 1, unblocks Member 2 wiring
          internal/connector/enrollment.go — CSR SPIFFE SAN verified against JWT
            ↳ SignConnectorCert called with cfg.CertTTL
          internal/connector/heartbeat.go — identity from context, role vs appmeta const
            ↳ disconnect watcher uses cfg.HeartbeatInterval + cfg.DisconnectThreshold
          pki/workspace.go — SignConnectorCert method
            ↳ 7-day validity, SPIFFE SAN via appmeta.ConnectorSPIFFEID

Member 4  Go: 002_connector_schema.sql — trust_domain on workspaces + connectors
          Go: schema.graphqls additions + connector.resolvers.go
          Rust: connector/src/appmeta.rs — mirrors appmeta.go constants
          Rust: config.rs — ConnectorConfig via figment
          Rust: enrollment.rs — SPIFFE SAN via appmeta constants
          Rust: tls.rs — verify_controller_spiffe uses appmeta::SPIFFE_CONTROLLER_ID
          Rust: heartbeat.rs — handles re_enroll field (logs warning, no action yet)
          Rust: main.rs, crypto.rs, updater.rs
          systemd units + connector-install.sh
          GitHub Actions CI (musl static builds + SHA-256 checksums)
          Docker Compose
```
