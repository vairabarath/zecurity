# Zecurity Codebase Overview

## What Is Zecurity

A Zero Trust Network Access (ZTNA) platform. Admins create remote networks, deploy connectors on Linux servers, and those connectors maintain secure mTLS tunnels back to the controller. Every identity in the system uses SPIFFE-standard X.509 certificates.

Traffic proxying is not yet implemented — that is the next sprint.

## Repository Structure

```
zecurity/
├── proto/               Shared protobuf definitions (single source of truth)
├── controller/          Go backend (GraphQL API + gRPC server + PKI)
├── connector/           Rust agent (enrollment + heartbeat + cert renewal + auto-update)
├── shield/              Rust agent (enrollment + heartbeat + cert renewal + resource protection)
├── admin/               React frontend (dashboard + remote networks + connectors)
├── buf.yaml             Buf lint/breaking config (repo root)
├── buf.gen.yaml         Buf codegen config → controller/gen/go
└── .github/workflows/   CI/CD (connector and shield binary releases)
```

## Controller (Go)

### Entry Point
- `cmd/server/main.go` — starts HTTP server (:8080), gRPC server (:9090), wires everything

### GraphQL API
- `graph/schema.graphqls` — sprint 1 types (User, Workspace, auth, lookups)
- `graph/connector.graphqls` — connector types (RemoteNetwork, Connector, mutations)
- `graph/shield.graphqls` — shield types
- `graph/resource.graphqls` — resource types (Resource, mutations for Create/Protect/Unprotect/Delete)
- `graph/resolvers/schema.resolvers.go` — auth + user + workspace resolvers
- `graph/resolvers/connector.resolvers.go` — connector CRUD + token generation
- `graph/resolvers/resource.resolvers.go` — resource CRUD and lifecycle management
- `graph/gqlgen.yml` — codegen config (follow-schema layout)

### PKI (3-tier certificate hierarchy)
- `internal/pki/root.go` — Root CA (self-signed, 10yr, MaxPathLen=2)
- `internal/pki/intermediate.go` — Intermediate CA (signed by Root, 5yr, MaxPathLen=1)
- `internal/pki/workspace.go` — Workspace CA (per-tenant, 2yr) + `SignConnectorCert()` (7-day leaf)
- `internal/pki/controller.go` — Controller TLS cert (ephemeral, SPIFFE SAN)
- `internal/pki/crypto.go` — Key encryption (AES-256-GCM via HKDF), PEM helpers
- `internal/pki/service.go` — Service interface + initialization

### Resource Protection
- `internal/resource/config.go` — `ResourceConfig` struct, `NewConfig()`, duration constants
- `internal/resource/store.go` — DB helpers for resource lifecycle: `CreateResource`, `GetPendingForShield`, `UpdateStatus`, `RecordAck`, `MarkRemoving`, `SoftDelete`

### Connector Subsystem
- `internal/connector/config.go` — ConnectorConfig struct (CertTTL, HeartbeatInterval, RenewalWindow, etc.)
- `internal/connector/token.go` — JWT enrollment token generation + Redis JTI burn
- `internal/connector/enrollment.go` — gRPC Enroll + RenewCert handlers (verify JWT, sign CSR, return cert)
- `internal/connector/heartbeat.go` — gRPC Heartbeat handler (sets re_enroll=true within renewal window) + disconnect watcher goroutine
- `internal/connector/spiffe.go` — SPIFFE ID parser + gRPC interceptor + cert verification
- `internal/connector/ca_endpoint.go` — HTTP GET /ca.crt endpoint

### Identity Constants
- `internal/appmeta/identity.go` — all SPIFFE constants, trust domain helpers, CN prefixes

### Auth
- `internal/auth/` — Google OAuth, JWT, Redis sessions (sprint 1, unchanged)

### Bootstrap
- `internal/bootstrap/` — first-user signup, workspace creation, CA generation

### Database
- `migrations/001_schema.sql` — users, workspaces (sprint 1)
- `migrations/002_connector_schema.sql` — remote_networks, connectors, trust_domain column
- `migrations/007_resources.sql` — resources table for shield resource protection

## Connector (Rust)

### Entry Point
- `src/main.rs` — load config, check state, enroll or heartbeat, shutdown handling
  - Supports `--check-update` flag for systemd oneshot update service

### Modules
- `src/appmeta.rs` — SPIFFE constants (mirrors Go appmeta exactly)
- `src/config.rs` — ConnectorConfig via figment (env vars + TOML)
- `src/enrollment.rs` — 10-step enrollment flow (JWT parse, CA fetch, fingerprint verify, keygen, CSR, gRPC enroll, save certs, config cleanup)
- `src/heartbeat.rs` — mTLS heartbeat loop (SPIFFE preflight, tonic channel, 30s interval, exponential backoff, triggers renewal on re_enroll=true)
- `src/renewal.rs` — cert renewal flow (read key, extract public key DER, call RenewCert RPC, save new cert, rebuild mTLS channel)
- `src/crypto.rs` — EC P-384 keygen, PEM I/O, CSR building, public key extraction, cert NotAfter parsing
- `src/tls.rs` — controller SPIFFE SAN verification
- `src/updater.rs` — GitHub release checker, SHA-256 verify, atomic binary replace, rollback
- `src/util.rs` — shared utilities (read_hostname)

### Proto
- `proto/connector/v1/connector.proto` — ConnectorService (Enroll + Heartbeat + RenewCert RPCs), package `connector.v1`
- `build.rs` — tonic-prost-build proto compilation (`../proto/connector/v1/connector.proto`)

### Deployment
- `scripts/connector-install.sh` — Linux installer (user, dirs, binary, systemd, config)
- `systemd/zecurity-connector.service` — main daemon (hardened, runs as zecurity)
- `systemd/zecurity-connector-update.service` — oneshot update check
- `systemd/zecurity-connector-update.timer` — daily update trigger
- `Cross.toml` — cross-compilation config (installs protoc via pre-build apt-get)
- `Dockerfile` — multi-stage build for containerized deployment

## Shield (Rust)

### Entry Point
- `src/main.rs` — load config, check state, enroll or heartbeat, shutdown handling

### Modules
- `src/appmeta.rs` — SPIFFE constants
- `src/config.rs` — ShieldConfig via figment
- `src/enrollment.rs` — Enrollment flow (similar to connector)
- `src/heartbeat.rs` — mTLS heartbeat loop, sends `resource_acks`, receives `resource_instructions`
- `src/renewal.rs` — Certificate renewal flow
- `src/crypto.rs` — Key generation, CSRs
- `src/tls.rs` — Controller SPIFFE SAN verification
- `src/updater.rs` — GitHub release checker
- `src/resources.rs` — Manages protected resources, applies `nftables` rules, and performs health checks.
- `src/network.rs` — Network utilities, including LAN IP detection.

### Proto
- `proto/shield/v1/shield.proto` — ShieldService (Enroll + Heartbeat + RenewCert RPCs), package `shield.v1`
- `build.rs` — tonic-prost-build proto compilation

### Deployment
- `scripts/shield-install.sh` — Linux installer
- `systemd/zecurity-shield.service` — Main daemon
- `systemd/zecurity-shield-update.service` + `zecurity-shield-update.timer` — Auto-updater
- `Cross.toml` — Cross-compilation configuration
- `Dockerfile` — Multi-stage build

## Admin Frontend (React + TypeScript)

### Pages
- `src/pages/Dashboard.tsx` — overview with workspace info and connector stats
- `src/pages/RemoteNetworks.tsx` — create/delete networks, location picker, connector count
- `src/pages/Connectors.tsx` — list connectors, status badges, revoke/delete, 30s auto-poll
- `src/pages/Resources.tsx` — list resources, status, and manage protection state.
- `src/pages/Login.tsx` — Google OAuth login flow
- `src/pages/Settings.tsx` — workspace settings

### Components
- `src/components/InstallCommandModal.tsx` — two-step modal (name input -> copy install command)
- `src/components/CreateResourceModal.tsx` — Form for creating new resources.
- `src/components/layout/Sidebar.tsx` — navigation sidebar

### GraphQL
- `src/graphql/mutations.graphql` — all mutations (auth, network, connector)
- `src/graphql/queries.graphql` — all queries (me, workspace, networks, connectors)
- `src/generated/` — codegen output (TypeScript types + Apollo hooks)
- `codegen.yml` — reads both schema.graphqls + connector.graphqls

## Proto

- **Location:** `proto/` (repo root — single source of truth)
- **Packages:**
  - `connector.v1`: Connector service. `HeartbeatResponse` now includes `shield_resources` to relay resource instructions. `HeartbeatRequest` includes `resource_acks`.
  - `shield.v1`: Shield service. Defines `ResourceInstruction` and `ResourceAck` messages.
- **RPCs:** `Enroll`, `Heartbeat`, `RenewCert` in both services.
- **Go generated:** `controller/gen/go/proto/` (via Buf)
- **Rust:** `build.rs` in `connector` and `shield` projects read from `proto/` directly.
- **Regenerate:** `buf generate` or `make generate-proto` from repo root

## CI/CD

### `.github/workflows/connector-release.yml` and `.github/workflows/shield-release.yml`
- Triggers on `connector-v*` or `shield-v*` tags
- Uses `cross` (Docker-based) for musl static binaries (amd64 + arm64)
- Runs from repo root: `cross build --manifest-path <component>/Cargo.toml`
- `Cross.toml` installs protoc via `pre-build` apt-get inside the cross container
- Uploads: binaries, checksums.txt, install script, systemd units
- Creates GitHub Release via softprops/action-gh-release

## Key Architecture Decisions

- **SPIFFE identity**: Every cert carries `spiffe://<trust-domain>/<role>/<id>` as URI SAN
- **Multi-tenancy**: All queries scoped by `tenant_id`, trust domain validated live (no cache)
- **CA fingerprint enrollment**: Connector verifies CA cert SHA-256 matches JWT claim (no shared secret needed)
- **Private key never leaves device**: EC P-384 keypair generated on connector, only CSR sent to controller
- **7-day cert validity + auto-renewal**: Short-lived certs, renewed automatically before expiry via RenewCert RPC
- **Proof-of-possession renewal**: Connector sends self-signed CSR (not raw public key) — proves key ownership
- **Zero-downtime renewal**: Connector rebuilds mTLS channel after renewal without dropping heartbeat loop
- **Disconnect detection**: Background goroutine marks connectors DISCONNECTED after 90s without heartbeat
- **Repo-root proto**: `proto/` at repo root — neither Go nor Rust "owns" the contract
- **Resource instruction piggybacking**: No new RPCs for resource management. Instructions are piggybacked on the existing Connector <-> Controller heartbeat.
- **Atomic nftables updates**: The shield agent flushes and rebuilds its `nftables` chain atomically on each update, preventing intermediate broken states.
- **Controller-led, ack-driven delivery**: The controller repeatedly sends resource instructions until the shield acknowledges receipt, ensuring eventual consistency.

## Environment Variables

### Controller (.env)
```
DATABASE_URL, REDIS_URL, PORT, ENV
JWT_SECRET, JWT_ISSUER
GOOGLE_CLIENT_ID, GOOGLE_CLIENT_SECRET, GOOGLE_REDIRECT_URI
PKI_MASTER_SECRET, ALLOWED_ORIGIN
GRPC_PORT, CONNECTOR_CERT_TTL, CONNECTOR_ENROLLMENT_TOKEN_TTL
CONNECTOR_HEARTBEAT_INTERVAL, CONNECTOR_DISCONNECT_THRESHOLD
CONNECTOR_RENEWAL_WINDOW
```

### Connector (/etc/zecurity/connector.conf)
```
CONTROLLER_ADDR, CONTROLLER_HTTP_ADDR, ENROLLMENT_TOKEN
AUTO_UPDATE_ENABLED, LOG_LEVEL, STATE_DIR, CONNECTOR_ID
```

### Shield (/etc/zecurity/shield.conf)
```
CONTROLLER_ADDR, CONTROLLER_HTTP_ADDR, ENROLLMENT_TOKEN
AUTO_UPDATE_ENABLED, LOG_LEVEL, STATE_DIR, SHIELD_ID
RESOURCE_CHECK_INTERVAL_SECS
```
