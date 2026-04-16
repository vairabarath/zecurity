# Sprint 4 — Connector Solidification + Shield Deployment
## Rust Shield · Go Controller · Connector gRPC Server · Admin UI

---

## Sprint Goal

Deploy a Shield on any resource host, see it appear ACTIVE in the dashboard,
watch it go DISCONNECTED if the server goes offline, and have its zecurity0
interface + base nftables table set up automatically on enrollment.
Every identity uses SPIFFE-standard certificates.
The Shield heartbeats through the Connector — never directly to the Controller.

---

## All Decisions Locked

```
Shield binary name      zecurity-shield
Shield service          zecurity-shield.service
Shield install script   shield-install.sh
Shield CI tag           shield-v*
Shield SPIFFE role      shield  (appmeta.SPIFFERoleShield)
Shield SPIFFE SAN       spiffe://ws-<slug>.zecurity.in/shield/<id>
Shield cert CN          shield-<id>  (appmeta.PKIShieldCNPrefix)
Shield cert TTL         SHIELD_CERT_TTL (default 168h / 7 days)
Shield renewal window   SHIELD_RENEWAL_WINDOW (default 48h)
Shield heartbeat        to Connector :9091 (NOT Controller directly)
Shield interface        zecurity0 (tun, CGNAT 100.64.x.x)
Connector new port      :9091 — Shield-facing gRPC server
Connector improvement   Goodbye RPC + NetworkHealth + ShieldHealth aggregation
Proto new file          proto/shield/v1/shield.proto (same pattern as connector)
DB new migration        003_shield_schema.sql
```

---

## Dependency Map — What Blocks What

This is the most important section. Read before coding.

```
Day 1 — Two things must land before anyone else can start:

  Member 2 commits proto/shield/v1/shield.proto
    AND updates proto/connector/v1/connector.proto (Goodbye + ShieldHealth)
    → Member 3: buf generate → Go stubs → writes Shield handlers
    → Member 3: Go stubs for Goodbye → writes Goodbye handler on Connector
    → Member 4: build.rs picks up shield.proto → Rust stubs → writes Shield binary

  Member 2 commits appmeta additions (SPIFFERoleShield etc.)
    → Member 3: SignShieldCert uses appmeta.SPIFFERoleShield
    → Member 4: mirrors into shield/src/appmeta.rs

  Member 3 commits 003_shield_schema.sql + graph/shield.graphqls + codegen
    → Member 2: DB table exists → Shield handlers can write to it
    → Member 1: generated TypeScript hooks available → wires UI

  Member 1 can start immediately (no backend needed):
    → Shield page layout, routing, loading states
    → Wait only for codegen to wire real mutations/queries

  Member 4 can start immediately after proto lands:
    → Shield binary scaffolding, config.rs, appmeta.rs
    → enrollment.rs after Member 2's Enroll handler is live
    → heartbeat.rs after Member 3's Connector :9091 server is live
    → network.rs (zecurity0 + nftables) is fully independent
```

---

## Team Split

```
Member 1   Frontend — React
           graph/shield.graphqls additions (shield types + mutations + queries)
           src/pages/Shields.tsx
           src/components/InstallCommandModal.tsx (reuse, Shield token variant)
           RemoteNetworks.tsx (add NetworkHealth indicator)
           src/graphql/ (add Shield operations)
           codegen run

Member 2   Go — Proto + appmeta + Shield gRPC handlers + PKI
           proto/shield/v1/shield.proto           ← Day 1, written FIRST
           proto/connector/v1/connector.proto     ← Day 1, add Goodbye + ShieldHealth
           internal/appmeta/identity.go           ← Day 1, add Shield constants
           internal/shield/config.go              ← ShieldConfig struct
           internal/shield/token.go               ← JWT generation + Redis jti burn
           internal/shield/enrollment.go          ← gRPC Enroll + RenewCert handlers
           internal/shield/heartbeat.go           ← disconnect watcher (reads from DB)
           internal/shield/spiffe.go              ← reuse connector parseSPIFFEID
           internal/pki/workspace.go              ← add SignShieldCert + RenewShieldCert
           cmd/server/main.go                     ← wire Shield gRPC service + ShieldConfig

Member 3   Go — DB + GraphQL + Connector improvements
           migrations/003_shield_schema.sql       ← Day 1, written FIRST
           graph/shield.graphqls                  ← Day 1, written FIRST (unblocks Member 1)
           graph/resolvers/shield.resolvers.go    ← Shield CRUD + token generation
           graph/resolvers/connector.resolvers.go ← add NetworkHealth computation
           internal/connector/heartbeat.go        ← add ShieldHealth aggregation
           internal/connector/goodbye.go          ← new Goodbye RPC handler
           connector/src/agent_server.rs          ← NEW: Shield-facing gRPC server :9091

Member 4   Rust — Shield binary + CI
           shield/ (entire new crate)
             src/appmeta.rs
             src/config.rs
             src/main.rs
             src/enrollment.rs
             src/heartbeat.rs
             src/renewal.rs
             src/crypto.rs
             src/tls.rs
             src/network.rs                       ← zecurity0 + nftables
             src/updater.rs
             src/util.rs
           shield/systemd/ (3 unit files)
           shield/scripts/shield-install.sh
           shield/Cross.toml
           shield/Dockerfile
           shield/build.rs
           shield/Cargo.toml
           connector/src/main.rs                  ← start Shield gRPC server on :9091
           .github/workflows/shield-release.yml
```

---

## Proto Files — Member 2 (Day 1)

### proto/shield/v1/shield.proto (NEW)

Same pattern as connector proto. One proto file covers both:
- Shield → Connector (Connector runs ShieldService on :9091)
- Controller → also runs ShieldService on :9090 (for enrollment only)

The Shield doesn't know or care which it's talking to. Same RPCs, same messages.

```protobuf
syntax = "proto3";

package shield.v1;

option go_package = "github.com/vairabarath/zecurity/gen/go/proto/shield/v1;shieldv1";

// ShieldService is implemented by BOTH the Controller (for enrollment)
// and the Connector (for post-enrollment heartbeat + renewal).
//
// Enrollment:   Shield → Controller :9090 (plain TLS, no cert yet)
// Heartbeat:    Shield → Connector  :9091 (mTLS, SPIFFE cert)
// RenewCert:    Shield → Connector  :9091 (mTLS, SPIFFE cert)
// Goodbye:      Shield → Connector  :9091 (mTLS, SPIFFE cert)
service ShieldService {

  // Called once during enrollment.
  // Plain TLS — Shield has no cert yet.
  // Shield presents enrollment JWT + PKCS#10 CSR.
  // CSR SAN: spiffe://ws-<slug>.zecurity.in/shield/<id>
  // Controller returns 7-day cert signed by WorkspaceCA.
  rpc Enroll(EnrollRequest) returns (EnrollResponse);

  // Called every SHIELD_HEARTBEAT_INTERVAL seconds after enrollment.
  // mTLS to Connector :9091 — Shield presents its SPIFFE cert.
  // Connector aggregates Shield health and reports to Controller.
  // re_enroll=true when cert is within renewal window.
  rpc Heartbeat(HeartbeatRequest) returns (HeartbeatResponse);

  // Called when heartbeat response has re_enroll=true.
  // mTLS to Connector :9091 — Shield keeps its existing keypair.
  // Shield sends CSR (proof of key possession) for same SPIFFE ID.
  // Connector forwards to Controller which issues fresh cert.
  rpc RenewCert(RenewCertRequest) returns (RenewCertResponse);

  // Called on clean shutdown (SIGTERM).
  // mTLS to Connector :9091.
  // Connector immediately marks Shield DISCONNECTED in its state.
  // Best-effort — crash without Goodbye is handled by disconnect watcher.
  rpc Goodbye(GoodbyeRequest) returns (GoodbyeResponse);
}

message EnrollRequest {
  string enrollment_token = 1;  // signed JWT from controller
  bytes  csr_der          = 2;  // DER-encoded PKCS#10 CSR (EC P-384)
                                // SAN: spiffe://ws-<slug>.zecurity.in/shield/<id>
  string version          = 3;  // CARGO_PKG_VERSION
  string hostname         = 4;
}

message EnrollResponse {
  bytes  certificate_pem     = 1;  // 7-day leaf cert, SPIFFE SAN
  bytes  workspace_ca_pem    = 2;  // WorkspaceCA cert
  bytes  intermediate_ca_pem = 3;  // Intermediate CA cert
  string shield_id           = 4;  // confirmed shield UUID
  string interface_addr      = 5;  // assigned zecurity0 IP (e.g. "100.64.0.1/32")
  string connector_addr      = 6;  // Connector address for post-enrollment comms
  string connector_id        = 7;  // which Connector to heartbeat through
}

message HeartbeatRequest {
  string shield_id = 1;   // logging only — identity from SPIFFE cert
  string version   = 2;   // CARGO_PKG_VERSION
  string hostname  = 3;
  string public_ip = 4;   // optional
}

message HeartbeatResponse {
  bool   ok             = 1;
  string latest_version = 2;
  bool   re_enroll      = 3;  // cert expiring soon — call RenewCert
}

message RenewCertRequest {
  string shield_id = 1;  // logging only
  bytes  csr_der   = 2;  // DER-encoded CSR — proves key possession
}

message RenewCertResponse {
  bytes certificate_pem     = 1;
  bytes workspace_ca_pem    = 2;
  bytes intermediate_ca_pem = 3;
}

message GoodbyeRequest {
  string shield_id = 1;  // logging only
}

message GoodbyeResponse {
  bool ok = 1;
}
```

### proto/connector/v1/connector.proto (MODIFY)

Add two things:

**1. Goodbye RPC:**

```protobuf
// Called by Connector on clean shutdown (SIGTERM).
// Controller immediately marks Connector DISCONNECTED.
rpc Goodbye(GoodbyeRequest) returns (GoodbyeResponse);

message GoodbyeRequest {
  string connector_id = 1;
}
message GoodbyeResponse {
  bool ok = 1;
}
```

**2. ShieldHealth in HeartbeatRequest:**

```protobuf
// MODIFY existing HeartbeatRequest — add shields field:
message HeartbeatRequest {
  string connector_id              = 1;
  string version                   = 2;
  string hostname                  = 3;
  string public_ip                 = 4;
  repeated ShieldHealth shields    = 5;  // NEW — Shield health summary
}

message ShieldHealth {
  string shield_id          = 1;
  string status             = 2;  // "active" or "disconnected"
  string version            = 3;
  int64  last_heartbeat_at  = 4;  // unix timestamp
}
```

### buf.gen.yaml (MODIFY)

Add shield proto to output:

```yaml
version: v1
plugins:
  - plugin: go
    out: controller/gen/go
    opt: paths=source_relative
  - plugin: go-grpc
    out: controller/gen/go
    opt: paths=source_relative
```

The `buf.yaml` at repo root already has `roots: [proto]` so it picks up
`proto/shield/v1/shield.proto` automatically. No change needed to `buf.yaml`.

After Member 2 commits both proto files:

```bash
# From repo root — regenerates ALL Go stubs
buf generate

# Rust — each binary's build.rs handles its own proto
# connector/build.rs already reads ../proto/connector/v1/connector.proto
# shield/build.rs will read ../proto/shield/v1/shield.proto
```

---

## appmeta additions — Member 2 (Day 1)

Add to `controller/internal/appmeta/identity.go` alongside existing constants.
Do NOT remove anything.

```go
// ── Shield identity constants (sprint 4) ────────────────────────────────────

const (
    // SPIFFERoleShield is the SPIFFE role path segment for Shield binaries.
    // Shield SPIFFE URI: spiffe://ws-<slug>.zecurity.in/shield/<id>
    SPIFFERoleShield = "shield"

    // PKIShieldCNPrefix is the certificate CN prefix for Shield leaf certs.
    // CN = "shield-<shieldID>"
    PKIShieldCNPrefix = "shield-"

    // ShieldInterfaceName is the tun interface created by Shield on the resource host.
    ShieldInterfaceName = "zecurity0"

    // ShieldInterfaceCIDR is the CGNAT range used for zecurity0 interface addresses.
    // Controller assigns a unique /32 from this range to each Shield at enrollment.
    ShieldInterfaceCIDR = "100.64.0.0/10"
)

// ShieldSPIFFEID builds the full SPIFFE URI for a Shield certificate.
// Example: trustDomain "ws-acme.zecurity.in", shieldID "abc-123"
//       → "spiffe://ws-acme.zecurity.in/shield/abc-123"
func ShieldSPIFFEID(trustDomain, shieldID string) string {
    return "spiffe://" + trustDomain + "/" + SPIFFERoleShield + "/" + shieldID
}
```

---

## DB Migration — Member 3 (Day 1)

New file: `controller/migrations/003_shield_schema.sql`

```sql
-- 003_shield_schema.sql
-- Shield table — mirrors connectors table with two additions:
--   connector_id: which Connector this Shield heartbeats through
--   interface_addr: the zecurity0 IP assigned to this Shield

CREATE TABLE shields (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    remote_network_id    UUID        NOT NULL REFERENCES remote_networks(id) ON DELETE CASCADE,
    connector_id         UUID        NOT NULL REFERENCES connectors(id) ON DELETE CASCADE,
    name                 TEXT        NOT NULL,

    -- Status lifecycle (same as connectors):
    -- pending      → token generated, not yet enrolled
    -- active       → enrolled, heartbeating via Connector
    -- disconnected → no heartbeat report from Connector for > threshold
    -- revoked      → rejected on next heartbeat
    status               TEXT        NOT NULL DEFAULT 'pending'
                                     CHECK (status IN (
                                       'pending','active','disconnected','revoked'
                                     )),

    -- Single-use enrollment token handle. Cleared after enrollment.
    enrollment_token_jti TEXT,

    -- SPIFFE trust domain — derived from workspace slug at token generation.
    -- e.g. "ws-acme.zecurity.in"
    trust_domain         TEXT,

    -- zecurity0 interface address assigned by Controller at enrollment.
    -- Unique per shield within the workspace. From CGNAT range 100.64.0.0/10.
    interface_addr       TEXT,

    -- Set after successful enrollment
    cert_serial          TEXT,
    cert_not_after       TIMESTAMPTZ,

    -- Updated when Connector reports Shield health in its heartbeat
    last_heartbeat_at    TIMESTAMPTZ,
    version              TEXT,
    hostname             TEXT,
    public_ip            TEXT,

    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_shields_tenant          ON shields (tenant_id);
CREATE INDEX idx_shields_remote_network  ON shields (remote_network_id, tenant_id);
CREATE INDEX idx_shields_connector       ON shields (connector_id);
CREATE INDEX idx_shields_token_jti       ON shields (enrollment_token_jti);
CREATE INDEX idx_shields_trust_domain    ON shields (trust_domain);
CREATE UNIQUE INDEX idx_shields_interface_addr
    ON shields (tenant_id, interface_addr)
    WHERE interface_addr IS NOT NULL;
```

---

## GraphQL Schema — Member 3 (Day 1)

New file: `controller/graph/shield.graphqls`

```graphql
# Shield types — mirrors connector types

type Shield {
  id:             ID!
  name:           String!
  status:         ShieldStatus!
  remoteNetworkId: ID!
  connectorId:    ID!
  lastSeenAt:     String
  version:        String
  hostname:       String
  publicIp:       String
  interfaceAddr:  String
  certNotAfter:   String
  createdAt:      String!
}

enum ShieldStatus {
  PENDING
  ACTIVE
  DISCONNECTED
  REVOKED
}

# Returned once when token is generated.
# installCommand shown once — never stored in DB.
type ShieldToken {
  shieldId:       ID!
  installCommand: String!
}

# Add to existing Mutation type:
extend type Mutation {
  generateShieldToken(remoteNetworkId: ID!, shieldName: String!): ShieldToken!
  revokeShield(id: ID!): Boolean!
  deleteShield(id: ID!): Boolean!
}

# Add to existing Query type:
extend type Query {
  shields(remoteNetworkId: ID!): [Shield!]!
  shield(id: ID!): Shield
}
```

Modify `controller/graph/connector.graphqls` — add NetworkHealth to RemoteNetwork:

```graphql
# Add to existing RemoteNetwork type:
type RemoteNetwork {
  id:            ID!
  name:          String!
  location:      NetworkLocation!
  status:        RemoteNetworkStatus!
  networkHealth: NetworkHealth!    # NEW
  connectors:    [Connector!]!
  shields:       [Shield!]!        # NEW — all shields in this network
  createdAt:     String!
}

enum NetworkHealth {
  ONLINE     # ≥1 connector ACTIVE
  DEGRADED   # connectors exist but none ACTIVE
  OFFLINE    # no connectors at all
}
```

After committing the schema files, run codegen:

```bash
cd controller && go generate ./graph/...
cd admin && npm run codegen
```

Member 1 can now build the UI using generated hooks.

---

## internal/shield/config.go — Member 2

New file. Same pattern as `internal/connector/config.go`.

```go
package shield

import "time"

// Config holds all tunable values for the Shield subsystem.
// Populated in main.go from environment variables.
// Pattern is identical to connector.Config.
type Config struct {
    // CertTTL is the validity window for Shield leaf certificates.
    // Env: SHIELD_CERT_TTL (default: 168h / 7 days)
    CertTTL time.Duration

    // RenewalWindow is how early before expiry re_enroll=true is returned.
    // Env: SHIELD_RENEWAL_WINDOW (default: 48h)
    RenewalWindow time.Duration

    // EnrollmentTokenTTL is the Redis TTL for single-use enrollment JWTs.
    // Env: SHIELD_ENROLLMENT_TOKEN_TTL (default: 24h)
    EnrollmentTokenTTL time.Duration

    // DisconnectThreshold is how long without a heartbeat report from
    // a Connector before a Shield is marked DISCONNECTED.
    // Env: SHIELD_DISCONNECT_THRESHOLD (default: 120s)
    // Note: Shield heartbeats go to Connector which aggregates and reports
    // to Controller every HeartbeatInterval. So threshold must be >
    // Connector's HeartbeatInterval + some buffer.
    DisconnectThreshold time.Duration

    // JWTSecret is reused from auth config — same secret for all JWTs.
    // Env: JWT_SECRET (already required from sprint 1)
    JWTSecret string
}
```

---

## .env additions

Add to `controller/.env` and `.env.example`:

```env
# ── Shield (Sprint 4) ────────────────────────────────────────────────────────
SHIELD_CERT_TTL=168h
SHIELD_RENEWAL_WINDOW=48h
SHIELD_ENROLLMENT_TOKEN_TTL=24h
SHIELD_DISCONNECT_THRESHOLD=120s
# Note: SHIELD_GRPC_PORT is not needed — Shield uses same :9090 as Connector
# Shield-facing gRPC on Connector runs on :9091 (hardcoded in Connector binary)
```

---

## internal/shield/token.go — Member 2

JWT payload for Shield enrollment token:

```json
{
  "jti":            "random-uuid",
  "shield_id":      "uuid-of-shield-row",
  "remote_network_id": "uuid",
  "workspace_id":   "uuid",
  "trust_domain":   "ws-acme.zecurity.in",
  "ca_fingerprint": "sha256-hex-of-intermediate-ca-cert-DER",
  "connector_id":   "uuid-of-selected-connector",
  "connector_addr": "192.168.1.10:9091",
  "interface_addr": "100.64.0.1/32",
  "iss":            "zecurity-controller",
  "exp":            "now + SHIELD_ENROLLMENT_TOKEN_TTL"
}
```

Three new fields vs Connector token:
- `connector_id` + `connector_addr` — which Connector to heartbeat through
- `interface_addr` — the zecurity0 IP assigned by Controller

**Connector selection logic** (inside token.go):

```go
func (s *service) selectConnector(ctx context.Context, remoteNetworkID, tenantID string) (*Connector, error) {
    // 1. Get all ACTIVE connectors for this remote network
    connectors, err := s.db.GetActiveConnectors(ctx, remoteNetworkID, tenantID)
    if err != nil || len(connectors) == 0 {
        return nil, ErrNoActiveConnectors
    }

    // 2. Count shields per connector (load balancing)
    // Pick connector with fewest shields
    // Tiebreaker: most recent last_heartbeat_at

    type scored struct {
        connector   *Connector
        shieldCount int
    }
    scores := make([]scored, len(connectors))
    for i, c := range connectors {
        count, _ := s.db.CountShieldsByConnector(ctx, c.ID, tenantID)
        scores[i] = scored{c, count}
    }
    sort.Slice(scores, func(i, j int) bool {
        if scores[i].shieldCount != scores[j].shieldCount {
            return scores[i].shieldCount < scores[j].shieldCount
        }
        // Tiebreaker: more recent heartbeat wins
        return scores[i].connector.LastHeartbeatAt.After(
            scores[j].connector.LastHeartbeatAt)
    })
    return scores[0].connector, nil
}
```

**Interface address assignment** (inside token.go):

```go
func (s *service) assignInterfaceAddr(ctx context.Context, tenantID string) (string, error) {
    // Find the next available IP in 100.64.0.0/10 for this tenant
    // Check shields table: SELECT interface_addr WHERE tenant_id = ?
    // Pick next unused /32 from the range
    // Store reserved address atomically
    // Return e.g. "100.64.0.1/32"
}
```

---

## internal/shield/enrollment.go — Member 2

Enroll handler on the Controller. Same pattern as `internal/connector/enrollment.go`.

```
Receive EnrollRequest (from Shield, plain TLS):
  1. Verify JWT signature (cfg.JWTSecret), exp > now,
     iss == appmeta.ControllerIssuer
  2. Extract jti, shield_id, workspace_id, trust_domain,
     connector_id, interface_addr from claims
  3. GET+DEL jti from Redis atomically
     NOT FOUND → PERMISSION_DENIED ("token expired or already used")
  4. Load shield row: verify status='pending', tenant matches workspace_id
     FAIL → PERMISSION_DENIED
  5. Verify workspace status='active'
     FAIL → FAILED_PRECONDITION
  6. Verify connector exists and is ACTIVE
     FAIL → FAILED_PRECONDITION ("assigned connector is not active")
  7. Parse CSR from request.csr_der
  8. Verify CSR self-signature
  9. Verify CSR SPIFFE SAN:
       expected = appmeta.ShieldSPIFFEID(trust_domain, shield_id)
       actual   = first URI SAN in CSR
       MISMATCH → PERMISSION_DENIED
 10. Call pki.SignShieldCert(ctx, tenantID, shieldID, trustDomain, csr, cfg.CertTTL)
 11. UPDATE shields SET
       status='active',
       trust_domain=trust_domain,
       interface_addr=interface_addr,
       connector_id=connector_id,
       cert_serial=<hex>,
       cert_not_after=<expiry>,
       hostname=request.hostname,
       version=request.version,
       last_heartbeat_at=NOW(),
       enrollment_token_jti=NULL
     WHERE id=shield_id AND tenant_id=workspace_id
 12. Return EnrollResponse:
       certificate_pem
       workspace_ca_pem
       intermediate_ca_pem
       shield_id
       interface_addr    ← Shield uses this to configure zecurity0
       connector_addr    ← Shield uses this for all future comms
       connector_id
```

---

## internal/shield/heartbeat.go — Member 2

**Disconnect watcher only** — the Controller does NOT receive heartbeats
directly from Shield. Shield heartbeats go to the Connector.
The Controller learns about Shield health from the Connector's HeartbeatRequest
(the `shields` repeated field).

```go
// runShieldDisconnectWatcher marks Shields DISCONNECTED when the Connector
// stops reporting them alive in its heartbeat's ShieldHealth list.
//
// The Connector reports Shield health every HeartbeatInterval (30s).
// If a Shield's last_heartbeat_at is older than DisconnectThreshold (120s),
// it means the Connector hasn't seen it in 4+ heartbeat cycles.
func (s *service) runShieldDisconnectWatcher(ctx context.Context) {
    ticker := time.NewTicker(s.cfg.DisconnectThreshold / 2)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            s.db.Exec(ctx, `
                UPDATE shields
                   SET status     = 'disconnected',
                       updated_at = NOW()
                 WHERE status = 'active'
                   AND last_heartbeat_at < NOW() - $1
                   AND tenant_id IN (
                       SELECT id FROM workspaces WHERE status = 'active'
                   )
            `, s.cfg.DisconnectThreshold)
        }
    }
}
```

The Connector's HeartbeatRequest now contains `shields []ShieldHealth`.
In `internal/connector/heartbeat.go`, the Controller reads this list and
updates the shields table:

```go
// In existing Heartbeat handler — add after updating connector row:
for _, sh := range req.Shields {
    s.shieldSvc.UpdateShieldHealth(ctx, sh.ShieldId, sh.Status,
        sh.Version, sh.LastHeartbeatAt, connectorTenantID)
}
```

---

## internal/connector/goodbye.go — Member 3 (NEW)

New file in the connector package. Implements the Goodbye RPC on the Controller.

```go
// Goodbye marks the Connector DISCONNECTED immediately.
// Called by the Connector on clean shutdown (SIGTERM).
// This prevents the 90-second disconnect watcher delay.
//
// Best-effort: if the Connector crashes without calling Goodbye,
// the disconnect watcher still catches it after 90s.
func (s *service) Goodbye(
    ctx context.Context,
    req *pb.GoodbyeRequest,
) (*pb.GoodbyeResponse, error) {

    // Identity from SPIFFE interceptor context
    connectorID := ctx.Value(spiffeEntityIDKey{}).(string)
    trustDomain := ctx.Value(trustDomainKey{}).(string)

    err := s.db.Exec(ctx, `
        UPDATE connectors
           SET status     = 'disconnected',
               updated_at = NOW()
         WHERE id         = $1
           AND trust_domain = $2
    `, connectorID, trustDomain)
    if err != nil {
        return nil, status.Error(codes.Internal, "failed to mark disconnected")
    }

    tracing.Info("connector goodbye",
        "connector_id", connectorID,
        "trust_domain", trustDomain,
    )

    return &pb.GoodbyeResponse{Ok: true}, nil
}
```

---

## internal/connector/heartbeat.go — Member 3 (MODIFY)

Add NetworkHealth computation. Add ShieldHealth processing from request.

```go
// NetworkHealth is computed in the RemoteNetworks GraphQL resolver,
// not in the heartbeat handler. No changes needed here for that.

// What DOES change: process shields from HeartbeatRequest.
// After updating the connector row, process each ShieldHealth:
for _, sh := range req.Shields {
    // Update shield's last_heartbeat_at and status in DB
    s.db.Exec(ctx, `
        UPDATE shields
           SET last_heartbeat_at = to_timestamp($1),
               status            = $2,
               version           = $3,
               updated_at        = NOW()
         WHERE id        = $4
           AND connector_id = $5
    `, sh.LastHeartbeatAt, sh.Status, sh.Version,
       sh.ShieldId, connectorID)
}
```

---

## internal/connector/spiffe.go — Member 2 (MODIFY)

The SPIFFE interceptor already parses roles generically.
Add `shield` as a valid role alongside `connector`:

```go
// In NewTrustDomainValidator or the interceptor's role check:
// The interceptor does NOT check role — it just parses and injects.
// Role checking happens in each handler.
// So actually NO CHANGE NEEDED to spiffe.go.
// The interceptor works for Shield RPCs automatically.
```

The Shield gRPC service on the Controller registers on the same gRPC server
as the Connector service. The interceptor covers it automatically because
it validates any valid trust domain + SPIFFE cert — it doesn't check role.
Role checking is in each handler (`role == appmeta.SPIFFERoleShield`).

---

## internal/pki/workspace.go — Member 2 (MODIFY)

Add alongside existing `SignConnectorCert` and `RenewConnectorCert`:

```go
// SignShieldCert issues a 7-day leaf certificate for a Shield.
//
// Identical to SignConnectorCert except:
//   - SPIFFE SAN uses appmeta.ShieldSPIFFEID (role = "shield")
//   - CN uses appmeta.PKIShieldCNPrefix
//   - certTTL comes from ShieldConfig.CertTTL
func (s *serviceImpl) SignShieldCert(
    ctx         context.Context,
    tenantID    string,
    shieldID    string,
    trustDomain string,
    csr         *x509.CertificateRequest,
    certTTL     time.Duration,
) (*ShieldCertResult, error) {

    spiffeID := appmeta.ShieldSPIFFEID(trustDomain, shieldID)
    uri, _   := url.Parse(spiffeID)

    now := time.Now().UTC()
    cert := &x509.Certificate{
        SerialNumber: newSerial(),
        Subject: pkix.Name{
            CommonName:   appmeta.PKIShieldCNPrefix + shieldID,
            Organization: []string{appmeta.PKIWorkspaceOrganization},
        },
        URIs:        []*url.URL{uri},
        NotBefore:   now,
        NotAfter:    now.Add(certTTL),
        KeyUsage:    x509.KeyUsageDigitalSignature,
        ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
        IsCA:        false,
    }

    workspaceCA, caKey, err := s.loadWorkspaceCA(ctx, tenantID)
    if err != nil {
        return nil, fmt.Errorf("pki: load workspace CA: %w", err)
    }
    defer zeroKey(caKey)

    certDER, err := x509.CreateCertificate(rand.Reader, cert, workspaceCA, csr.PublicKey, caKey)
    if err != nil {
        return nil, fmt.Errorf("pki: sign shield cert: %w", err)
    }

    return &ShieldCertResult{
        CertificatePEM:    pemEncode("CERTIFICATE", certDER),
        WorkspaceCAPEM:    s.workspaceCACertPEM(ctx, tenantID),
        IntermediateCAPEM: s.intermediateCertPEM(),
        Serial:            cert.SerialNumber.Text(16),
        NotBefore:         cert.NotBefore,
        NotAfter:          cert.NotAfter,
    }, nil
}

// RenewShieldCert is identical to RenewConnectorCert but for Shield.
// Shield sends a CSR (proof-of-possession), Controller re-signs with fresh validity.
func (s *serviceImpl) RenewShieldCert(
    ctx         context.Context,
    tenantID    string,
    shieldID    string,
    trustDomain string,
    csr         *x509.CertificateRequest,
    certTTL     time.Duration,
) (*ShieldCertResult, error) {
    // Identical to SignShieldCert — CSR already has correct public key
    // Controller signs fresh cert for same SPIFFE ID + same public key
    return s.SignShieldCert(ctx, tenantID, shieldID, trustDomain, csr, certTTL)
}
```

---

## cmd/server/main.go — Member 2 (MODIFY)

Add ShieldConfig wiring + register ShieldService on gRPC server:

```go
// Add mustDuration calls alongside existing ConnectorConfig:
shieldCfg := shield.Config{
    CertTTL:             mustDuration("SHIELD_CERT_TTL",              7*24*time.Hour),
    RenewalWindow:       mustDuration("SHIELD_RENEWAL_WINDOW",        48*time.Hour),
    EnrollmentTokenTTL:  mustDuration("SHIELD_ENROLLMENT_TOKEN_TTL",  24*time.Hour),
    DisconnectThreshold: mustDuration("SHIELD_DISCONNECT_THRESHOLD",  120*time.Second),
    JWTSecret:           mustEnv("JWT_SECRET"),
}

shieldSvc := shield.NewService(shieldCfg, db, pkiSvc, redis)

// Register on same gRPC server as ConnectorService:
shieldpb.RegisterShieldServiceServer(grpcServer, shieldSvc)

// Start disconnect watcher:
go shieldSvc.RunDisconnectWatcher(ctx)
```

---

## connector/src/agent_server.rs — Member 3 (NEW)

The Connector runs a second gRPC server on `:9091` that Shields connect to.
This server implements the ShieldService — same proto, same RPCs.
But the Connector only handles Heartbeat, RenewCert, and Goodbye here.
Enroll is handled by the Controller directly (Shield enrolls with Controller, not Connector).

```rust
// connector/src/agent_server.rs
//
// Shield-facing gRPC server. Runs on :9091.
// Shields connect here for post-enrollment communication.
//
// This server:
//   - Verifies Shield mTLS cert (SPIFFE role = "shield", same WorkspaceCA trust)
//   - Handles Heartbeat: updates local Shield health map
//   - Handles RenewCert: forwards to Controller, returns fresh cert to Shield
//   - Handles Goodbye: removes Shield from local health map
//
// Shield health is reported to Controller via the Connector's own
// heartbeat (ShieldHealth repeated field in HeartbeatRequest).

pub struct ShieldServer {
    // Local in-memory map of alive shields
    // shield_id → last_seen timestamp
    shields: Arc<Mutex<HashMap<String, ShieldEntry>>>,
    // mTLS channel to Controller for forwarding RenewCert
    controller_channel: Channel,
    // Connector's own trust domain (to validate Shield certs)
    trust_domain: String,
}

impl ShieldService for ShieldServer {

    async fn heartbeat(&self, request: Request<HeartbeatRequest>)
        -> Result<Response<HeartbeatResponse>, Status>
    {
        // 1. Extract Shield SPIFFE ID from mTLS peer cert
        //    Verify: role = "shield", trust_domain matches connector's workspace
        // 2. Update local shields map: shield_id → now()
        // 3. Check cert_not_after from peer cert
        //    If within renewal window → re_enroll = true
        // 4. Return HeartbeatResponse { ok: true, re_enroll }
        // Note: Shield health is batched and reported to Controller
        //       in the Connector's own heartbeat loop
    }

    async fn renew_cert(&self, request: Request<RenewCertRequest>)
        -> Result<Response<RenewCertResponse>, Status>
    {
        // 1. Verify Shield SPIFFE identity from mTLS cert
        // 2. Forward RenewCert to Controller via existing mTLS channel
        // 3. Return Controller's response to Shield
        // The Connector is a transparent proxy here — no PKI work itself
    }

    async fn goodbye(&self, request: Request<GoodbyeRequest>)
        -> Result<Response<GoodbyeResponse>, Status>
    {
        // 1. Verify Shield SPIFFE identity
        // 2. Remove Shield from local health map
        //    (will be missing from next ShieldHealth batch to Controller)
        // 3. Return ok
    }

    // Enroll is NOT implemented here — Shield enrolls with Controller directly
    async fn enroll(&self, _: Request<EnrollRequest>)
        -> Result<Response<EnrollResponse>, Status>
    {
        Err(Status::unimplemented(
            "Shield enrolls directly with Controller, not through Connector"
        ))
    }
}
```

**How ShieldHealth gets to the Controller:**

The Connector's existing heartbeat loop (in `heartbeat.rs`) already sends
a `HeartbeatRequest` to the Controller every 30s. After this sprint it also
includes the Shield health map:

```rust
// In heartbeat.rs — build HeartbeatRequest:
let shield_health: Vec<ShieldHealth> = self.shield_server
    .get_alive_shields()
    .iter()
    .map(|(id, entry)| ShieldHealth {
        shield_id:         id.clone(),
        status:            entry.status.clone(),
        version:           entry.version.clone(),
        last_heartbeat_at: entry.last_seen.timestamp(),
    })
    .collect();

let req = HeartbeatRequest {
    connector_id: state.connector_id.clone(),
    version:      env!("CARGO_PKG_VERSION").to_string(),
    hostname:     util::read_hostname(),
    public_ip:    get_public_ip().unwrap_or_default(),
    shields:      shield_health,  // NEW
};
```

---

## connector/src/main.rs — Member 4 (MODIFY)

Start the Shield-facing gRPC server alongside the existing heartbeat loop:

```rust
// In main.rs, after loading state and starting heartbeat:
let shield_server = ShieldServer::new(
    controller_channel.clone(),  // for forwarding RenewCert
    state.trust_domain.clone(),
);

// Start Shield gRPC server on :9091
tokio::spawn(async move {
    Server::builder()
        .add_service(ShieldServiceServer::new(shield_server))
        .serve("0.0.0.0:9091".parse().unwrap())
        .await
        .expect("Shield gRPC server failed");
});
```

---

## Shield Binary — Member 4 (NEW CRATE: shield/)

### shield/Cargo.toml

```toml
[package]
name    = "zecurity-shield"
version = "0.1.0"
edition = "2021"

[dependencies]
tokio              = { version = "1",    features = ["full"] }
tonic              = { version = "0.14", features = ["tls"] }
prost              = "0.14"
rcgen              = "0.13"
tokio-rustls       = "0.26"
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
rtnetlink          = "0.14"   # for zecurity0 tun interface creation
nftables           = "0.4"    # for writing nftables rules

[build-dependencies]
tonic-build = "0.14"
```

### shield/build.rs

```rust
fn main() -> Result<(), Box<dyn std::error::Error>> {
    tonic_build::compile_protos("../proto/shield/v1/shield.proto")?;
    Ok(())
}
```

### shield/src/appmeta.rs

Mirrors `connector/src/appmeta.rs` exactly, adds Shield-specific constants:

```rust
pub const SPIFFE_GLOBAL_TRUST_DOMAIN: &str = "zecurity.in";
pub const SPIFFE_CONTROLLER_ID: &str = "spiffe://zecurity.in/controller/global";
pub const SPIFFE_ROLE_SHIELD: &str = "shield";
pub const SPIFFE_ROLE_CONNECTOR: &str = "connector";
pub const PKI_SHIELD_CN_PREFIX: &str = "shield-";
pub const PRODUCT_NAME: &str = "ZECURITY";
pub const SHIELD_INTERFACE_NAME: &str = "zecurity0";
pub const SHIELD_INTERFACE_CIDR_RANGE: &str = "100.64.0.0/10";
```

### shield/src/config.rs

```
Required fields:
  CONTROLLER_ADDR       Controller host:port for enrollment gRPC
  CONTROLLER_HTTP_ADDR  Controller HTTP for /ca.crt bootstrap
  ENROLLMENT_TOKEN      JWT — required only on first run

Optional with defaults:
  AUTO_UPDATE_ENABLED   default: false
  LOG_LEVEL             default: info
  SHIELD_HEARTBEAT_INTERVAL_SECS  default: 30
```

State directory: `/var/lib/zecurity-shield/`

```
shield.key          EC P-384 private key (PEM, 0600, never leaves device)
shield.crt          Signed leaf cert (7-day validity, SPIFFE SAN)
workspace_ca.crt    WorkspaceCA + Intermediate CA chain (PEM)
state.json          {
                      shield_id,
                      trust_domain,
                      connector_id,
                      connector_addr,    ← heartbeat destination
                      interface_addr,    ← zecurity0 IP
                      enrolled_at,
                      cert_not_after
                    }
```

### shield/src/main.rs — startup logic

```
1. Init tracing (LOG_LEVEL from config)
2. Load config via figment
3. Check state.json:
   NOT EXISTS → run enrollment flow
   EXISTS     → load state, go to heartbeat loop

── Enrollment flow (enrollment.rs) ─────────────────────────────────────────
  a. Parse JWT payload (base64 decode, no signature verify)
  b. Extract: shield_id, workspace_id, trust_domain, ca_fingerprint,
              connector_id, connector_addr, interface_addr
  c. GET http://<CONTROLLER_HTTP_ADDR>/ca.crt (plain HTTP bootstrap)
  d. SHA-256 of CA cert DER bytes
  e. Compare against ca_fingerprint
     MISMATCH → error! exit(1)
     MATCH    → proceed
  f. Generate EC P-384 keypair → save shield.key (0600)
  g. Build CSR:
       CN:      format!("{}{}", appmeta::PKI_SHIELD_CN_PREFIX, shield_id)
       SAN URI: format!("spiffe://{}/{}/{}", trust_domain,
                        appmeta::SPIFFE_ROLE_SHIELD, shield_id)
  h. Call Controller gRPC Enroll (plain TLS to CONTROLLER_ADDR)
  i. Receive EnrollResponse:
       certificate_pem, workspace_ca_pem, intermediate_ca_pem,
       shield_id, interface_addr, connector_addr, connector_id
  j. Save shield.crt, workspace_ca.crt
  k. Write state.json (includes connector_addr + interface_addr)
  l. Remove ENROLLMENT_TOKEN from config
  m. Write SHIELD_ID=<id> to config
  n. Call network::setup(interface_addr, connector_addr) ← zecurity0 + nftables
  o. info!("enrollment complete shield_id={}", shield_id)
─────────────────────────────────────────────────────────────────────────────

4. tokio::spawn(heartbeat::run(state, cfg))
5. If AUTO_UPDATE_ENABLED: tokio::spawn(updater::run(cfg))
6. Wait for SIGTERM → call Goodbye on Connector → shutdown
```

### shield/src/tls.rs

Shield verifies the **Connector's** SPIFFE ID on every mTLS heartbeat connection.
NOT the Controller's — Shield talks to Connector post-enrollment.

```rust
/// Verifies the Connector's SPIFFE cert during mTLS handshake.
///
/// Expected format: spiffe://ws-<slug>.zecurity.in/connector/<connector_id>
///
/// The connector_id comes from state.json (saved at enrollment).
/// This ensures the Shield only heartbeats to the exact Connector
/// it enrolled with — not any connector that happens to have a valid cert.
pub fn verify_connector_spiffe(
    cert_der: &[u8],
    expected_connector_spiffe_id: &str,  // from state.json
) -> anyhow::Result<()> {
    // Parse cert, extract SPIFFE URI SAN
    // Compare full URI against expected_connector_spiffe_id
    // Reject if mismatch
}
```

The `expected_connector_spiffe_id` is built from state.json:
```rust
let expected = format!(
    "spiffe://{}/{}/{}",
    state.trust_domain,
    appmeta::SPIFFE_ROLE_CONNECTOR,
    state.connector_id
);
```

### shield/src/heartbeat.rs

Heartbeats go to **Connector :9091**, not Controller.

```
Build mTLS config:
  client_cert: shield.crt
  client_key:  shield.key
  trust_root:  workspace_ca.crt
  post_handshake: tls::verify_connector_spiffe(peer_cert, expected_connector_spiffe_id)

Connect to state.connector_addr (e.g. "192.168.1.10:9091")
Create ShieldServiceClient

Every SHIELD_HEARTBEAT_INTERVAL_SECS (default 30s):
  req = HeartbeatRequest {
    shield_id: state.shield_id,   // logging only on Connector side
    version:   env!("CARGO_PKG_VERSION"),
    hostname:  util::read_hostname(),
    public_ip: get_public_ip().unwrap_or_default(),
  }

  match client.heartbeat(req).await:
    Ok(resp) →
      consecutive_failures = 0
      if resp.re_enroll → call renewal::renew_cert()
    Err(e) →
      consecutive_failures += 1
      backoff = min(5 * 2^(failures-1), 60)
      warn!("heartbeat to connector failed attempt={}", failures)
      sleep(backoff).await
```

### shield/src/network.rs (NEW — unique to Shield)

Called once after successful enrollment. Sets up `zecurity0` + nftables.
Requires `CAP_NET_ADMIN` (set in systemd unit).

```rust
/// Sets up the zecurity0 tun interface and base nftables table.
///
/// Called ONCE after enrollment completes.
/// Creates the network infrastructure that resource protection rules
/// will be added to in Sprint 5 when admin clicks "Protect".
///
/// After this function:
///   - zecurity0 interface exists with interface_addr IP
///   - nftables table "zecurity" exists with chain "input"
///   - base rules: ACCEPT loopback + ACCEPT connector_ip
///   - default: DROP on zecurity0 (no resource rules yet)
pub async fn setup(interface_addr: &str, connector_addr: &str) -> anyhow::Result<()> {
    setup_tun_interface(interface_addr).await?;
    setup_nftables(connector_addr).await?;
    Ok(())
}

async fn setup_tun_interface(interface_addr: &str) -> anyhow::Result<()> {
    // 1. Create TUN interface named "zecurity0" using rtnetlink
    // 2. Assign interface_addr (e.g. 100.64.0.1/32)
    // 3. Bring interface UP
    // Shell equivalent:
    //   ip tuntap add dev zecurity0 mode tun
    //   ip addr add 100.64.0.1/32 dev zecurity0
    //   ip link set zecurity0 up
}

async fn setup_nftables(connector_addr: &str) -> anyhow::Result<()> {
    // Extract just the IP from connector_addr (strip port)
    // e.g. "192.168.1.10:9091" → "192.168.1.10"

    // Create nftables table and chain:
    //
    // table inet zecurity {
    //   chain input {
    //     type filter hook input priority 0; policy accept;
    //
    //     # Always allow loopback
    //     iif "lo" accept
    //
    //     # Always allow traffic from Connector IP
    //     # This is the only thing that can talk to resources via zecurity0
    //     ip saddr <connector_ip> accept
    //
    //     # Drop everything else coming in on zecurity0
    //     # (no resource rules yet — added in Sprint 5 when admin clicks Protect)
    //     iif "zecurity0" drop
    //   }
    // }
    //
    // Use nftables crate to write these rules programmatically
}
```

### shield/src/renewal.rs

Identical to `connector/src/renewal.rs`. RenewCert goes to Connector :9091.
Connector forwards to Controller. Shield gets fresh cert back.

```rust
pub async fn renew_cert(state: &ShieldState, cfg: &ShieldConfig) -> anyhow::Result<ShieldState> {
    // 1. Read shield.key from disk
    // 2. Build new CSR with same SPIFFE SAN (proof of key possession)
    // 3. Call RenewCert on Connector :9091 (mTLS)
    // 4. Save new shield.crt
    // 5. Update state.json cert_not_after
    // 6. Return updated state
}
```

### shield/src/updater.rs

Identical to `connector/src/updater.rs`. Checks `shield-v*` releases.
Replaces `/usr/local/bin/zecurity-shield`.

---

## shield/systemd/ — Member 4

### zecurity-shield.service

```ini
[Unit]
Description=Zecurity Shield — Resource Host Protection
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
ExecStart=/usr/local/bin/zecurity-shield
EnvironmentFile=/etc/zecurity/shield.conf
User=zecurity
Group=zecurity

# Same hardening as zecurity-connector.service
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
SystemCallFilter=@system-service @network-io ~@privileged

# Shield needs NET_ADMIN for:
#   - creating zecurity0 tun interface
#   - writing nftables rules
CapabilityBoundingSet=CAP_NET_ADMIN CAP_NET_RAW CAP_NET_BIND_SERVICE
AmbientCapabilities=CAP_NET_ADMIN CAP_NET_RAW

StateDirectory=zecurity-shield
RuntimeDirectory=zecurity-shield
WorkingDirectory=/var/lib/zecurity-shield

Restart=on-failure
RestartSec=3
TimeoutStartSec=30

[Install]
WantedBy=multi-user.target
```

### zecurity-shield-update.service + zecurity-shield-update.timer

Identical pattern to connector update units. Change names and binary path.

---

## shield/scripts/shield-install.sh — Member 4

Mirrors `connector/scripts/connector-install.sh` exactly.
Key differences:
- Binary: `/usr/local/bin/zecurity-shield`
- Config: `/etc/zecurity/shield.conf` (0660 root:zecurity)
- State: `/var/lib/zecurity-shield/`
- Service: `zecurity-shield.service`
- User: `zecurity` (same system user — already created by connector install if run first)

---

## shield/Cross.toml — Member 4

```toml
[build.pre-build]
cmd = ["apt-get", "install", "-y", "protobuf-compiler"]
```

Identical to `connector/Cross.toml`.

---

## .github/workflows/shield-release.yml — Member 4

```yaml
name: Release Shield
on:
  push:
    tags: ['shield-v*']

jobs:
  release:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4

      - name: Install Rust
        uses: dtolnay/rust-toolchain@stable

      - name: Install cross
        run: cargo install cross --git https://github.com/cross-rs/cross

      - name: Build amd64
        run: cross build --manifest-path shield/Cargo.toml
             --release --target x86_64-unknown-linux-musl

      - name: Build arm64
        run: cross build --manifest-path shield/Cargo.toml
             --release --target aarch64-unknown-linux-musl

      - name: Rename binaries
        run: |
          cp shield/target/x86_64-unknown-linux-musl/release/zecurity-shield \
             shield-linux-amd64
          cp shield/target/aarch64-unknown-linux-musl/release/zecurity-shield \
             shield-linux-arm64

      - name: Checksums
        run: sha256sum shield-linux-amd64 shield-linux-arm64 > checksums.txt

      - name: Release
        uses: softprops/action-gh-release@v1
        with:
          files: |
            shield-linux-amd64
            shield-linux-arm64
            checksums.txt
            shield/scripts/shield-install.sh
            shield/systemd/zecurity-shield.service
            shield/systemd/zecurity-shield-update.service
            shield/systemd/zecurity-shield-update.timer
```

---

## Admin UI — Member 1

### graph/shield.graphqls (already written by Member 3 — Member 1 consumes it)

Run codegen after Member 3 commits the schema:
```bash
cd admin && npm run codegen
```

### src/graphql/ additions

```graphql
# mutations.graphql — add:
mutation GenerateShieldToken($remoteNetworkId: ID!, $shieldName: String!) {
  generateShieldToken(remoteNetworkId: $remoteNetworkId, shieldName: $shieldName) {
    shieldId
    installCommand
  }
}

mutation RevokeShield($id: ID!) { revokeShield(id: $id) }
mutation DeleteShield($id: ID!) { deleteShield(id: $id) }

# queries.graphql — add:
query GetShields($remoteNetworkId: ID!) {
  shields(remoteNetworkId: $remoteNetworkId) {
    id name status lastSeenAt version hostname
    publicIp interfaceAddr certNotAfter createdAt
    connectorId
  }
}
```

### src/pages/Shields.tsx (NEW)

Route: `/remote-networks/<id>/shields`

Identical structure to `Connectors.tsx`. Differences:
- Extra column: `Interface` showing `interfaceAddr` (the zecurity0 IP)
- Extra column: `Via` showing which Connector this Shield heartbeats through
- Status badges: same colors (PENDING=gray, ACTIVE=green, DISCONNECTED=amber, REVOKED=red)
- "Add Shield" button → `InstallCommandModal` (same component, Shield token variant)
- 30s auto-poll

### src/pages/RemoteNetworks.tsx (MODIFY)

Add NetworkHealth indicator next to each network name:
- 🟢 ONLINE (≥1 connector ACTIVE)
- 🟡 DEGRADED (connectors exist but none ACTIVE)
- 🔴 OFFLINE (no connectors)

Add Shield count to each network card:
```
"2 / 3 connectors active · 4 shields active"
```

### src/components/layout/Sidebar.tsx (MODIFY)

Add "Shields" nav link under "Connectors" in the sidebar.

---

## Repo Structure After Sprint 4

```
zecurity/
├── proto/
│   ├── connector/v1/connector.proto    MODIFIED (Goodbye + ShieldHealth)
│   └── shield/v1/shield.proto         NEW
├── controller/
│   ├── internal/
│   │   ├── appmeta/identity.go        MODIFIED (Shield constants)
│   │   ├── connector/
│   │   │   ├── goodbye.go             NEW
│   │   │   └── heartbeat.go           MODIFIED (ShieldHealth processing)
│   │   ├── shield/                    NEW PACKAGE
│   │   │   ├── config.go
│   │   │   ├── token.go
│   │   │   ├── enrollment.go
│   │   │   ├── heartbeat.go           (disconnect watcher only)
│   │   │   └── spiffe.go              (thin wrapper, reuses connector logic)
│   │   └── pki/workspace.go           MODIFIED (SignShieldCert + RenewShieldCert)
│   ├── graph/
│   │   ├── connector.graphqls         MODIFIED (NetworkHealth + shields field)
│   │   ├── shield.graphqls            NEW
│   │   └── resolvers/
│   │       ├── connector.resolvers.go MODIFIED (NetworkHealth computation)
│   │       └── shield.resolvers.go    NEW
│   ├── migrations/
│   │   └── 003_shield_schema.sql      NEW
│   ├── gen/go/proto/shield/v1/        NEW (buf generate output)
│   └── cmd/server/main.go             MODIFIED (ShieldConfig + ShieldService)
├── connector/
│   └── src/
│       ├── agent_server.rs            NEW (Shield-facing gRPC :9091)
│       ├── heartbeat.rs               MODIFIED (ShieldHealth in request)
│       └── main.rs                    MODIFIED (start agent_server)
├── shield/                            NEW CRATE
│   ├── Cargo.toml
│   ├── build.rs
│   ├── Cross.toml
│   ├── Dockerfile
│   ├── src/
│   │   ├── appmeta.rs
│   │   ├── config.rs
│   │   ├── main.rs
│   │   ├── enrollment.rs
│   │   ├── heartbeat.rs
│   │   ├── renewal.rs
│   │   ├── crypto.rs
│   │   ├── tls.rs
│   │   ├── network.rs                 UNIQUE — zecurity0 + nftables
│   │   ├── updater.rs
│   │   └── util.rs
│   ├── systemd/
│   │   ├── zecurity-shield.service
│   │   ├── zecurity-shield-update.service
│   │   └── zecurity-shield-update.timer
│   └── scripts/
│       └── shield-install.sh
├── admin/
│   └── src/
│       ├── pages/
│       │   ├── Shields.tsx            NEW
│       │   └── RemoteNetworks.tsx     MODIFIED (NetworkHealth + shield count)
│       ├── components/layout/
│       │   └── Sidebar.tsx            MODIFIED (Shields nav link)
│       └── graphql/
│           ├── mutations.graphql      MODIFIED
│           └── queries.graphql        MODIFIED
└── .github/workflows/
    └── shield-release.yml             NEW
```

---

## Integration Checklist

```
Proto + DB
  ✓ proto/shield/v1/shield.proto committed Day 1
  ✓ connector.proto Goodbye + ShieldHealth added Day 1
  ✓ buf generate runs cleanly (Go stubs generated)
  ✓ shield/build.rs compiles Rust stubs cleanly
  ✓ 003_shield_schema.sql runs on fresh DB without error
  ✓ shields.interface_addr UNIQUE per tenant
  ✓ shields.connector_id FK to connectors table

appmeta
  ✓ SPIFFERoleShield = "shield" in identity.go
  ✓ PKIShieldCNPrefix = "shield-" in identity.go
  ✓ ShieldSPIFFEID() helper in identity.go
  ✓ shield/src/appmeta.rs mirrors Go constants exactly

Enrollment
  ✓ generateShieldToken picks least-loaded ACTIVE connector
  ✓ interface_addr assigned from 100.64.0.0/10, unique per tenant
  ✓ JWT contains: jti, shield_id, workspace_id, trust_domain,
                  ca_fingerprint, connector_id, connector_addr, interface_addr
  ✓ jti stored in Redis TTL=SHIELD_ENROLLMENT_TOKEN_TTL
  ✓ Shield fetches /ca.crt from CONTROLLER_HTTP_ADDR
  ✓ CA fingerprint verified
  ✓ CSR SAN: spiffe://ws-<slug>.zecurity.in/shield/<id>
  ✓ Controller verifies CSR SPIFFE SAN matches JWT
  ✓ SignShieldCert: 7-day validity, SPIFFE SAN, ClientAuth
  ✓ EnrollResponse includes interface_addr + connector_addr + connector_id
  ✓ Shield saves state.json with all fields
  ✓ network::setup() called after enrollment
  ✓ zecurity0 interface created with interface_addr
  ✓ nftables table "zecurity" created
  ✓ Base rules: ACCEPT lo, ACCEPT connector_ip, DROP zecurity0
  ✓ ENROLLMENT_TOKEN removed from shield.conf

Heartbeat (Shield → Connector → Controller)
  ✓ Shield connects to connector_addr :9091 (from state.json)
  ✓ Shield verifies Connector SPIFFE ID: spiffe://.../connector/<connector_id>
  ✓ Connector's agent_server receives Shield heartbeat
  ✓ Connector updates local shields map
  ✓ Connector includes ShieldHealth in its HeartbeatRequest to Controller
  ✓ Controller updates shields.last_heartbeat_at from ShieldHealth
  ✓ Shield disconnect watcher fires on DisconnectThreshold (120s)
  ✓ Shield shows ACTIVE in dashboard within 30s
  ✓ Kill Shield → DISCONNECTED within 120s
  ✓ Restart Shield → ACTIVE on next Connector heartbeat cycle

Graceful Shutdown
  ✓ Connector SIGTERM → calls Goodbye RPC on Controller
  ✓ Controller marks Connector DISCONNECTED immediately (not after 90s)
  ✓ Shield SIGTERM → calls Goodbye RPC on Connector :9091
  ✓ Connector removes Shield from local health map

Cert Renewal
  ✓ Connector's agent_server returns re_enroll=true when cert < 48h remaining
  ✓ Shield calls RenewCert on Connector :9091
  ✓ Connector forwards RenewCert to Controller
  ✓ Controller issues fresh 7-day cert (same SPIFFE ID)
  ✓ Shield saves new cert, rebuilds mTLS channel
  ✓ Shield stays ACTIVE throughout renewal

NetworkHealth
  ✓ RemoteNetwork.networkHealth computed in resolver (not DB column)
  ✓ ONLINE when ≥1 connector ACTIVE
  ✓ DEGRADED when connectors exist but none ACTIVE
  ✓ OFFLINE when no connectors
  ✓ Frontend shows colored indicator on RemoteNetworks page

CI/CD
  ✓ shield-release.yml triggers on shield-v* tags
  ✓ cross build --manifest-path shield/Cargo.toml from repo root
  ✓ amd64 + arm64 musl static binaries
  ✓ checksums.txt generated
  ✓ install script + systemd units uploaded to release

End-to-end demo
  ✓ Deploy connector on network (already working from Sprint 3)
  ✓ Generate Shield token in dashboard (picks active connector)
  ✓ Run install command on resource host
  ✓ Shield appears ACTIVE in dashboard within 30s
  ✓ zecurity0 interface visible on resource host (ip link show zecurity0)
  ✓ nftables table visible (nft list ruleset)
  ✓ Kill Shield → DISCONNECTED within 120s
  ✓ Restart Shield → ACTIVE on next heartbeat cycle
  ✓ Revoke Shield → next heartbeat rejected by Connector → stays REVOKED
```

---

## What Is NOT in This Sprint

```
RDE                        Sprint 5 — Connector scans network, reports services
Resource definitions       Sprint 5 — Admin adds IP/port/protocol resources
nftables per-resource      Sprint 5 — per-resource DROP rules via Shield
ACL delivery               Sprint 7
Traffic proxying           Sprint 9
Client enrollment          Sprint 6
Access policies            Sprint 7
```

---

## Summary

```
Member 1  Shields.tsx + RemoteNetworks.tsx update + Sidebar
          GraphQL operations + codegen
          NO proto/DB work

Member 2  proto/shield/v1/shield.proto       ← Day 1 (unblocks everyone)
          proto/connector/v1/connector.proto  ← Day 1 (Goodbye + ShieldHealth)
          internal/appmeta/identity.go        ← Day 1 (unblocks Member 3 + 4)
          internal/shield/ (5 files)
          internal/pki/workspace.go           (SignShieldCert + RenewShieldCert)
          cmd/server/main.go                  (ShieldConfig + ShieldService)

Member 3  migrations/003_shield_schema.sql   ← Day 1 (unblocks Member 2 + 1)
          graph/shield.graphqls              ← Day 1 (unblocks Member 1 codegen)
          graph/resolvers/shield.resolvers.go
          graph/resolvers/connector.resolvers.go (NetworkHealth)
          internal/connector/goodbye.go
          internal/connector/heartbeat.go    (ShieldHealth processing)
          connector/src/agent_server.rs      (Shield-facing gRPC :9091)

Member 4  shield/ entire new crate
          shield/src/network.rs              (zecurity0 + nftables — unique)
          connector/src/main.rs              (start agent_server on :9091)
          .github/workflows/shield-release.yml
```
