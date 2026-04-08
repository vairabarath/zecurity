# Member 4 — Go Schema + DB + Migrations + Rust Connector + CI

## Role

You own the widest surface area: database migrations, GraphQL schema extensions, connector resolvers on the Go side, and the entire Rust connector binary plus its deployment infrastructure (systemd, install script, Docker Compose, GitHub Actions). Your Day 1 deliverable (migration + schema.graphqls) unblocks Member 1 (codegen) and Member 3 (DB queries in handlers).

---

## Your Files (CREATE or MODIFY only these)

### New files you create — Go side

```
controller/migrations/002_connector_schema.sql
controller/graph/resolvers/connector.resolvers.go
```

### Files you modify — Go side

```
controller/graph/schema.graphqls    ← add connector + remote_network types, queries, mutations
```

### New files you create — Rust connector (entire directory)

```
connector/Cargo.toml
connector/build.rs
connector/src/appmeta.rs
connector/src/main.rs
connector/src/config.rs
connector/src/enrollment.rs
connector/src/heartbeat.rs
connector/src/crypto.rs
connector/src/tls.rs
connector/src/updater.rs
connector/proto/                    ← symlink or copy of controller/proto/connector.proto
connector/systemd/zecurity-connector.service
connector/systemd/zecurity-connector-update.service
connector/systemd/zecurity-connector-update.timer
connector/scripts/connector-install.sh
```

### New files you create — CI

```
.github/workflows/connector-release.yml
```

### Optional — Docker

```
connector/Dockerfile                ← if providing a container image
connector/docker-compose.yml        ← connector-side compose example (NOT controller's)
```

---

## DO NOT TOUCH — Conflict Boundaries

- **`controller/internal/appmeta/identity.go`** — Member 3 owns all SPIFFE constants. You MIRROR them into Rust `appmeta.rs` but never modify the Go source.
- **`controller/proto/connector.proto`** — Member 2 writes this. You consume it via `tonic-build` in `build.rs`.
- **`controller/internal/connector/config.go`** — Member 2 owns the Config struct.
- **`controller/internal/connector/token.go`** — Member 2 owns token generation/burn.
- **`controller/internal/connector/spiffe.go`** — Member 3 owns SPIFFE parsing + interceptor.
- **`controller/internal/connector/enrollment.go`** — Member 3 owns the Enroll gRPC handler.
- **`controller/internal/connector/heartbeat.go`** — Member 3 owns the Heartbeat handler + watcher.
- **`controller/internal/pki/*`** — Member 3 adds SignConnectorCert; you do not touch PKI code.
- **`controller/cmd/server/main.go`** — Member 2 wires everything. Do not edit.
- **`controller/internal/auth/*`** — Sprint 1 auth code. Do not modify.
- **`controller/internal/bootstrap/*`** — Sprint 1 bootstrap code. Do not modify.
- **`controller/migrations/001_schema.sql`** — Sprint 1 schema. Do not modify.
- **`controller/docker-compose.yml`** — Sprint 1 dev infra (Postgres + Redis). Do not modify.
- **`admin/`** — Member 1 owns all frontend code.
- **`Makefile`** — Shared; do not modify without coordination.

---

## Phase-by-Phase Plan

### Phase 1 — Database Migration (DAY 1 — COMMIT FIRST)

**Create: `controller/migrations/002_connector_schema.sql`**

This unblocks Member 3 (DB queries in enrollment/heartbeat handlers) and Member 1 (after codegen).

**Part 1 — Extend workspaces table:**

```sql
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

**Part 2 — remote_networks table:**

```sql
CREATE TABLE remote_networks (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id   UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name        TEXT        NOT NULL,
    location    TEXT        NOT NULL CHECK (location IN ('home','office','aws','gcp','azure','other')),
    status      TEXT        NOT NULL DEFAULT 'active' CHECK (status IN ('active','deleted')),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, name)
);

CREATE INDEX idx_remote_networks_tenant ON remote_networks (tenant_id);
```

**Part 3 — connectors table:**

```sql
CREATE TABLE connectors (
    id                   UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id            UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    remote_network_id    UUID        NOT NULL REFERENCES remote_networks(id) ON DELETE CASCADE,
    name                 TEXT        NOT NULL,
    status               TEXT        NOT NULL DEFAULT 'pending'
                                     CHECK (status IN ('pending','active','disconnected','revoked')),
    enrollment_token_jti TEXT,
    trust_domain         TEXT,
    cert_serial          TEXT,
    cert_not_after       TIMESTAMPTZ,
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

### Phase 2 — GraphQL Schema Update (DAY 1)

**Modify: `controller/graph/schema.graphqls`**

Add to the existing schema. Do NOT remove or modify existing `me`, `workspace`, or `initiateAuth` definitions.

**New types:**

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

type ConnectorToken {
  connectorId:    ID!
  installCommand: String!
}
```

**Add to existing Query type:**

```graphql
remoteNetworks: [RemoteNetwork!]!
remoteNetwork(id: ID!): RemoteNetwork
connectors(remoteNetworkId: ID!): [Connector!]!
```

**Add to existing Mutation type:**

```graphql
createRemoteNetwork(name: String!, location: NetworkLocation!): RemoteNetwork!
deleteRemoteNetwork(id: ID!): Boolean!
generateConnectorToken(remoteNetworkId: ID!, connectorName: String!): ConnectorToken!
revokeConnector(id: ID!): Boolean!
deleteConnector(id: ID!): Boolean!
```

After committing, run `make gqlgen` to regenerate Go code and tell Member 1 to run `npm run codegen`.

### Phase 3 — Connector Resolvers

**Create: `controller/graph/resolvers/connector.resolvers.go`**

All resolvers use tenant-scoped queries (explicit `tenant_id` in WHERE clauses), matching the pattern in `schema.resolvers.go`.

**Query resolvers:**

- `remoteNetworks` — `SELECT * FROM remote_networks WHERE tenant_id = $1 AND status != 'deleted'`
- `remoteNetwork(id)` — `SELECT * FROM remote_networks WHERE id = $1 AND tenant_id = $2`
- `connectors(remoteNetworkId)` — `SELECT * FROM connectors WHERE remote_network_id = $1 AND tenant_id = $2`
- `RemoteNetwork.connectors` — nested resolver, loads connectors for a network

**Mutation resolvers:**

- `createRemoteNetwork` — INSERT into remote_networks with tenant_id from context
- `deleteRemoteNetwork` — Soft delete (set status='deleted'), only if zero non-deleted connectors exist
- `generateConnectorToken` — INSERT connector row (status='pending'), call Member 2's `GenerateEnrollmentToken`, build install command string, return `ConnectorToken`
- `revokeConnector` — UPDATE status='revoked' WHERE tenant_id matches
- `deleteConnector` — DELETE connector row WHERE tenant_id matches (only if status is 'pending' or 'revoked')

**Field resolvers (enum conversions):**

- `Connector.status` → map DB lowercase to GraphQL enum
- `RemoteNetwork.status` → map DB lowercase to GraphQL enum
- `RemoteNetwork.location` → map DB lowercase to GraphQL enum
- Timestamp fields → RFC3339 string format

Follow the same patterns as existing resolvers in `schema.resolvers.go` for enum conversion and time formatting.

**Install command format** (built in `generateConnectorToken`):

```
curl -fsSL https://github.com/yourorg/zecurity/releases/latest/download/connector-install.sh | \
  sudo CONTROLLER_ADDR=<controller_host>:<grpc_port> ENROLLMENT_TOKEN=<jwt> bash
```

### Phase 4 — Rust Connector: Foundation

**Create the `connector/` directory with all source files.**

**`connector/src/appmeta.rs`** — Mirrors `controller/internal/appmeta/identity.go` EXACTLY:

```rust
pub const SPIFFE_GLOBAL_TRUST_DOMAIN: &str = "zecurity.in";
pub const SPIFFE_CONTROLLER_ID: &str = "spiffe://zecurity.in/controller/global";
pub const SPIFFE_ROLE_CONNECTOR: &str = "connector";
pub const PRODUCT_NAME: &str = "ZECURITY";
pub const PKI_CONNECTOR_CN_PREFIX: &str = "connector-";
```

These values MUST match Member 3's `appmeta.go` constants character-for-character. If Member 3 changes a constant, you must update `appmeta.rs` to match.

**`connector/Cargo.toml`** — Dependencies as specified in the plan (tokio, tonic, rcgen, tokio-rustls, x509-parser, sha2, figment, semver, reqwest, etc.)

**`connector/build.rs`** — Uses `tonic-build` to compile Member 2's `connector.proto` into Rust stubs.

**`connector/src/config.rs`** — Uses `figment` to read env vars + `/etc/zecurity/connector.conf`. Required: `CONTROLLER_ADDR`, `ENROLLMENT_TOKEN` (first run only). Optional with defaults: `AUTO_UPDATE_ENABLED`, `LOG_LEVEL`, `HEARTBEAT_INTERVAL_SECS`, `UPDATE_CHECK_INTERVAL_SECS`.

### Phase 5 — Rust Connector: Enrollment Flow

**`connector/src/enrollment.rs`**

1. Parse JWT payload (base64-decode middle segment — no signature verification, connector has no JWT_SECRET; trust is established via CA fingerprint)
2. Extract `connector_id`, `workspace_id`, `trust_domain`, `ca_fingerprint`, `jti`
3. `GET http://<CONTROLLER_ADDR>/ca.crt` — fetch Intermediate CA cert
4. SHA-256 of fetched cert DER → compare hex against `ca_fingerprint` from JWT
   - Mismatch → `exit(1)` with clear MITM warning
5. Generate EC P-384 keypair, save private key to `connector.key` (mode 0600)
6. Build CSR:
   - CN: `format!("{}{}", appmeta::PKI_CONNECTOR_CN_PREFIX, connector_id)`
   - SAN URI: `format!("spiffe://{}/{}/{}", trust_domain, appmeta::SPIFFE_ROLE_CONNECTOR, connector_id)`
7. Connect to controller gRPC — plain TLS (not mTLS), trust root = fetched CA
8. Call `Enroll { enrollment_token, csr_der, version, hostname }`
9. Save: `connector.crt`, `workspace_ca.crt` (workspace + intermediate chain), `state.json`
10. Remove `ENROLLMENT_TOKEN` from config, write `CONNECTOR_ID`

**`connector/src/crypto.rs`** — EC P-384 key generation, PEM read/write, CSR building via `rcgen`.

### Phase 6 — Rust Connector: Heartbeat + TLS

**`connector/src/heartbeat.rs`**

- Build mTLS config: client cert + key, trust root = workspace CA chain
- Post-handshake: call `verify_controller_spiffe(peer_cert_der)` from `tls.rs`
- Create tonic channel + `ConnectorServiceClient`
- Loop every `HEARTBEAT_INTERVAL_SECS`:
  - Send `HeartbeatRequest { connector_id, version, hostname, public_ip }`
  - On success: reset failure counter, log if `re_enroll` is true (no action yet), log if new version available
  - On failure: exponential backoff (5s, 10s, 20s, 40s, 60s cap)

**`connector/src/tls.rs`**

- `verify_controller_spiffe(cert_der)` — parse X.509, find SAN URI matching `appmeta::SPIFFE_CONTROLLER_ID`
- Reject if SPIFFE ID doesn't match — prevents a rogue server signed by the same CA from impersonating the controller

### Phase 7 — Rust Connector: Auto-Updater

**`connector/src/updater.rs`**

- If `AUTO_UPDATE_ENABLED=false` → return immediately
- Random startup delay 0–3600s (prevent thundering herd)
- Every `UPDATE_CHECK_INTERVAL_SECS` (default 86400):
  1. GET GitHub releases API → parse `tag_name`
  2. semver compare: latest > `env!("CARGO_PKG_VERSION")`? No → continue
  3. Download binary + `checksums.txt`
  4. Verify SHA-256 — mismatch → abort, binary unchanged
  5. Backup old binary → replace → `systemctl restart`
  6. Health check after 10s → success: remove backup. Failure: restore backup, restart, log rollback

### Phase 8 — Rust Connector: Main Entry

**`connector/src/main.rs`**

1. Init tracing with `LOG_LEVEL`
2. Load config via figment
3. Check `state.json`:
   - Not exists → run enrollment flow (Phase 5)
   - Exists → load state, go to heartbeat loop
4. `tokio::spawn(heartbeat_loop(...))`
5. If `AUTO_UPDATE_ENABLED`: `tokio::spawn(update_loop(...))`
6. Wait for SIGTERM / ctrl_c → graceful shutdown

### Phase 9 — Deployment Infrastructure

**systemd units:**

- `zecurity-connector.service` — main daemon with security hardening (NoNewPrivileges, ProtectSystem, PrivateTmp, etc.)
- `zecurity-connector-update.service` — oneshot updater
- `zecurity-connector-update.timer` — daily trigger with random delay

**`connector/scripts/connector-install.sh`:**

- Creates `zecurity` system user
- Fetches `/ca.crt` from `CONTROLLER_HTTP_ADDR`
- Downloads binary from GitHub releases
- Installs systemd units + enables them
- Writes config to `/etc/zecurity/connector.conf` (0600)
- State directory: `/var/lib/zecurity-connector/`
- `-f` flag: force overwrite for re-installation

**Docker Compose alternative** (in connector directory, NOT controller's):

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

### Phase 10 — GitHub Actions CI

**Create: `.github/workflows/connector-release.yml`**

Trigger: push tag matching `connector-v*`

Steps:
1. Checkout
2. Install Rust stable + musl tools
3. `rustup target add x86_64-unknown-linux-musl aarch64-unknown-linux-musl`
4. Build release binaries for both targets
5. Rename to `connector-linux-amd64`, `connector-linux-arm64`
6. Generate `checksums.txt` with SHA-256
7. Create GitHub release from tag
8. Upload: binaries + checksums + `connector-install.sh`

---

## Dependency Timeline

```
Day 1:  Phase 1 (migration) — COMMIT FIRST, unblocks Member 3 + Member 1
        Phase 2 (schema.graphqls) — COMMIT WITH MIGRATION, unblocks Member 1 codegen
        Phase 4 (Rust foundation) — start immediately
          appmeta.rs can be written once Member 3's appmeta.go constants are committed
          build.rs needs Member 2's connector.proto committed

Day 2:  Phase 3 (connector resolvers) — needs gqlgen regeneration after schema commit
        Phase 5 (enrollment.rs) — needs proto stubs + appmeta.rs
        Phase 6 (heartbeat + tls) — needs enrollment working first

Day 3+: Phase 7 (updater) — independent, can be done anytime
        Phase 8 (main.rs) — needs enrollment + heartbeat done
        Phase 9 (systemd/install) — independent, can be done anytime
        Phase 10 (CI) — independent, do last
```

---

## Special Instructions

1. **Your migration + schema commit unblocks two people.** Member 1 can't run codegen without `schema.graphqls`. Member 3 can't write DB queries without the table definitions. Commit these Day 1.

2. **`appmeta.rs` must mirror `appmeta.go` exactly.** Character-for-character match on all string values. If Member 3 changes a constant in Go, you update Rust immediately. There is no automated sync — this is a manual contract.

3. **You consume the proto, you don't write it.** Member 2 writes `connector.proto`. You reference it via `tonic-build` in `build.rs`. If you need a proto change, ask Member 2 — don't modify the proto yourself.

4. **Your resolvers call Member 2's token function.** The `generateConnectorToken` resolver calls `connector.GenerateEnrollmentToken(...)` from Member 2's `token.go`. You build the install command string and return the `ConnectorToken` response. Coordinate the function signature with Member 2.

5. **Do NOT modify `controller/docker-compose.yml`.** That's the sprint 1 dev infrastructure (Postgres + Redis). Your Docker Compose file for the connector goes in `connector/docker-compose.yml` — a separate file for connector deployment.

6. **Do NOT modify `001_schema.sql`.** Sprint 1 schema is immutable. All your changes go in `002_connector_schema.sql`. Use `ALTER TABLE` and `ADD COLUMN IF NOT EXISTS` for safe idempotent changes to existing tables.

7. **The Rust connector never has `JWT_SECRET`.** It cannot verify the enrollment JWT signature. Trust is established by verifying the CA certificate fingerprint embedded in the JWT against the actual fetched certificate. This is intentional — the secret never leaves the controller.

8. **State directory permissions matter.** `connector.key` must be 0600 owned by `zecurity`. `connector.conf` must be 0600. The install script sets these permissions. The systemd unit runs as user `zecurity`.

9. **`re_enroll` handling in Rust:** The `HeartbeatResponse.re_enroll` field will always be `false` this sprint. Your Rust code should read it, log a warning if `true`, but take no action. This prepares for next sprint's auto-renewal without requiring a proto change.

10. **Resolver SQL must include explicit `tenant_id` conditions.** Follow the pattern from `schema.resolvers.go` — every query includes `AND tenant_id = $N`. No implicit tenant scoping.
