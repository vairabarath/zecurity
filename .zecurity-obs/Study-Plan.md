---
type: study-plan
created: 2026-05-05
purpose: Full codebase walkthrough — understand every layer of Zecurity before Sprint 9 M4 work
---

# Zecurity Full Codebase Study Plan

Read in the order below. Each section ends with a **checkpoint** — a question to answer before moving on. By the end you should be able to trace any request from user device to resource and back without referring to docs.

---

## Part 1 — System Shape (30 min)

Understand the four components and how they fit together before reading any code.

| File | Why |
|------|-----|
| `agent.md` (repo root) | Build commands, code style, port map |
| `.zecurity-obs/Sprint9/path.md` | Full dependency graph and sprint goal |
| `.zecurity-obs/Decisions/ADR-001-Sprint8-ACL-Snapshot-Caching.md` | Why ACL is pushed to Connector, not checked per-request |
| `.zecurity-obs/Decisions/ADR-002-Client-Daemon-Required.md` | Why a daemon is required (TUN + CAP_NET_ADMIN) |
| `.zecurity-obs/Decisions/ADR-003-Client-TUN-Transparent-Proxy.md` | Why TUN transparent proxy instead of SOCKS/HTTP proxy |

**Checkpoint:** Draw (on paper or whiteboard) the four components with their ports, what protocol each pair speaks, and who initiates each connection.

---

## Part 2 — Protobuf Contracts (20 min)

The proto files are the source of truth for every cross-component message. Read these before any implementation code.

| File | Why |
|------|-----|
| `proto/shield/v1/shield.proto` | ShieldService gRPC (Enroll, Heartbeat, Renewal, Goodbye, Control stream); ShieldControlMessage fields 1–11 |
| `proto/connector/v1/connector.proto` | ConnectorService gRPC (Enroll, Control stream, RenewCert); ConnectorControlMessage variants; ACLSnapshot |
| `proto/client/v1/client.proto` | ClientService gRPC (Login, GetACLSnapshot, RefreshToken, RenewDeviceCert) |

**Checkpoint:** For each service, name the RPC that carries the long-lived bidirectional stream and what messages flow on it in each direction.

---

## Part 3 — Controller (Go) — Identity, Auth, PKI (45 min)

The controller is the trust anchor. Read identity and PKI before anything else.

### 3a — Entry point and middleware

| File | Why |
|------|-----|
| `controller/cmd/server/main.go` | How all services are wired: DB pool, PKI service, auth, gRPC, HTTP mux, GraphQL |
| `controller/internal/middleware/auth.go` | Session extraction from cookie/header |
| `controller/internal/middleware/workspace.go` | Tenant scoping — how workspace_id flows into every handler |
| `controller/internal/middleware/role.go` | ADMIN vs MEMBER enforcement |
| `controller/internal/middleware/session.go` | Session hydration |

### 3b — Auth (OIDC)

| File | Why |
|------|-----|
| `controller/internal/auth/service.go` | Top-level auth service: login, callback, refresh |
| `controller/internal/auth/oidc.go` | OIDC provider setup |
| `controller/internal/auth/session.go` | Session create/load/destroy |
| `controller/internal/auth/valkey.go` | Session storage in Valkey (Redis replacement) |
| `controller/internal/auth/idtoken.go` | ID token validation |

### 3c — PKI

| File | Why |
|------|-----|
| `controller/internal/pki/service.go` | `Service` interface — all PKI operations |
| `controller/internal/pki/root.go` | Root CA bootstrap (self-signed, stored in DB) |
| `controller/internal/pki/intermediate.go` | Intermediate CA (per-controller) |
| `controller/internal/pki/workspace.go` | Workspace CA (per-tenant); `SignConnectorCert`, `SignShieldCert`, `SignClientCert`, `GenerateClientCRL` |
| `controller/internal/pki/crypto.go` | EC key generation, cert signing helpers |
| `controller/internal/connector/ca_endpoint.go` | `GET /ca.crl?workspace_id=<uuid>` — DER CRL endpoint consumed by Connector |

**Checkpoint:** Trace the PKI chain: root → intermediate → workspace CA → connector cert. What SPIFFE URI is in each cert? Who verifies it?

---

## Part 4 — Controller (Go) — Connector Lifecycle (30 min)

| File | Why |
|------|-----|
| `controller/internal/connector/enrollment.go` | `EnrollConnector` RPC — validates token, signs cert, inserts into DB |
| `controller/internal/connector/control_stream.go` | Bidirectional `Control` stream — heartbeat, ACL snapshot delivery, connector_log insert, resource ack |
| `controller/internal/connector/disconnect_watcher.go` | Marks connectors offline when stream drops |
| `controller/internal/connector/goodbye.go` | Graceful `Goodbye` RPC |
| `controller/internal/connector/token.go` | Enrollment token generation |
| `controller/internal/connector/spiffe.go` | SPIFFE ID construction for connectors |

**Checkpoint:** What happens when a Connector comes online? List the 5 steps from token validation to first heartbeat ACL push.

---

## Part 5 — Controller (Go) — Policy Engine (Sprint 8) (30 min)

| File | Why |
|------|-----|
| `controller/internal/policy/store.go` | DB reads for groups, members, resource rules |
| `controller/internal/policy/compiler.go` | `CompileACLSnapshot(workspace_id)` → flattens groups+rules into per-SPIFFE ACL entries |
| `controller/internal/policy/cache.go` | Per-workspace compiled snapshot cache; invalidated by `NotifyPolicyChange` |
| `controller/internal/policy/notifier.go` | Broadcasts invalidation to all connected connectors |
| `controller/graph/resolvers/policy.resolvers.go` | GraphQL resolvers: createGroup, addMember, createPolicy, deletePolicy |
| `controller/graph/resolvers/client.resolvers.go` | clientDevices, revokeDevice, myDevices |
| `controller/graph/resolvers/log.resolvers.go` | connectorLogs |

**Checkpoint:** If an admin adds a new member to a group, trace the exact sequence until the Connector has the updated ACL in memory.

---

## Part 6 — Controller (Go) — Shield and Resource (20 min)

| File | Why |
|------|-----|
| `controller/internal/shield/enrollment.go` | `EnrollShield` — sign Shield cert, record in DB |
| `controller/internal/shield/heartbeat.go` | Heartbeat handler — record last_seen, return resource instructions piggybacked |
| `controller/internal/resource/store.go` | Resource CRUD: create, update, delete, list by workspace |
| `controller/internal/discovery/store.go` | Discovered services: store scan results, promote to resource |

---

## Part 7 — Connector (Rust) — Startup and Enrollment (30 min)

| File | Why |
|------|-----|
| `connector/src/main.rs` | Full startup sequence — read top-to-bottom, it's the wiring diagram |
| `connector/src/config.rs` | All env vars: controller_addr, controller_http_addr, state_dir |
| `connector/src/appmeta.rs` | SPIFFE constants, product name, version |
| `connector/src/enrollment.rs` | POST to `/enroll`, write state.json, save certs |
| `connector/src/crypto.rs` | Key generation, CSR, PEM I/O |

**Checkpoint:** After enrollment, what files exist in `state_dir`? What does each contain?

---

## Part 8 — Connector (Rust) — Control Stream (30 min)

| File | Why |
|------|-----|
| `connector/src/control_stream.rs` | Bidirectional gRPC Control stream: sends heartbeat, receives heartbeat reply (ACL snapshot + resource instructions), dispatches connector_log, handles reconnect loop |
| `connector/src/controller_client.rs` | TLS channel to controller with SPIFFE verification |
| `connector/src/tls/mod.rs` | `verify_controller_spiffe` — post-handshake SPIFFE URI check |
| `connector/src/renewal.rs` | Certificate renewal flow (triggered by heartbeat reply) |
| `connector/src/policy/mod.rs` | `PolicyCache` — stores ACL snapshot in memory; `authorize(destination, port, protocol, spiffe_id)` |

**Checkpoint:** What does the Connector do when it receives an ACL snapshot that's newer than the one it has? What happens if the Control stream drops?

---

## Part 9 — Connector (Rust) — Shield-Facing gRPC Server (30 min)

| File | Why |
|------|-----|
| `connector/src/agent_server.rs` | `ShieldRegistry`: serves `ShieldService` gRPC on `:9091`; Enroll, Heartbeat (relay + piggyback), RenewCert proxy; **Sprint 9:** Control stream for tunnel messages |
| `connector/src/agent_tunnel.rs` | `AgentTunnelHub`: routes `TunnelOpen/Opened/Data/Close` between device connections and Shield instances; `RelaySession::relay_stream()` |

**Key insight:** The Connector's `:9091` acts as a mini-controller proxy for Shields. Shields never talk directly to the Controller — all cert operations and resource instructions flow through the Connector.

**Checkpoint:** When a Shield sends a `TunnelOpened` message on the Control stream, trace the exact call path until the data reaches the waiting `open_relay_session` future.

---

## Part 10 — Connector (Rust) — Device Tunnel Layer (Sprint 9) (30 min)

| File | Why |
|------|-----|
| `connector/src/tls/cert_store.rs` | `CertStore` — PEM material for device tunnel TLS |
| `connector/src/tls/server_cfg.rs` | `build_device_tunnel_tls` — mTLS with workspace CA, `WebPkiClientVerifier`, ALPN `ztna-tunnel-v1` |
| `connector/src/net_util.rs` | `lan_ip()` — OS routing trick to find outbound LAN IP |
| `connector/src/crl.rs` | `CrlManager` — fetch/parse DER CRL, `is_revoked(serial)`, 5-min background refresh |
| `connector/src/quic_listener.rs` | QUIC/UDP on `:9092` — wraps each bidir stream, delegates to `handle_stream` |
| `connector/src/device_tunnel.rs` | **M4 stub** — will become: TLS/TCP listener, JSON handshake, CRL check, ACL check, direct vs Shield relay routing |
| `connector/src/watchdog.rs` | systemd `READY=1` + `WATCHDOG=1` keepalive |

**Key interface note for M4:** `CrlManager` is passed as bare value (not `Arc<CrlManager>`) — it already holds `Arc<RwLock<...>>` internally. `open_relay_session` is called as a method: `hub.open_relay_session(shield_id, dest, port, protocol).await?`.

**Checkpoint:** A device connects over TLS to `:9092`. List every check that happens before a byte of application data is relayed. Which checks are M3's? Which are M4's to implement?

---

## Part 11 — Shield (Rust) — Full Lifecycle (40 min)

| File | Why |
|------|-----|
| `shield/src/main.rs` | Startup: config, enrollment, heartbeat loop, network setup |
| `shield/src/config.rs` | Config: connector_addr, state_dir |
| `shield/src/enrollment.rs` | POST `/enroll` to Connector `:9091` |
| `shield/src/control_stream.rs` | Bidirectional Control stream to Connector: sends heartbeat, receives resource instructions + **Sprint 9 TunnelOpen** messages |
| `shield/src/resources.rs` | Apply resource instructions: add/remove protected resources |
| `shield/src/network.rs` | TUN interface `zecurity0`, nftables `chain resource_protect` — atomic flush+rebuild per heartbeat |
| `shield/src/discovery.rs` | LAN scan on request from Connector |
| `shield/src/tls.rs` | TLS setup for Connector-facing gRPC |
| `shield/src/renewal.rs` | Cert renewal via Connector proxy |

**Checkpoint:** When a resource is added to a Shield, trace what happens in nftables. Why must the chain be flushed and rebuilt atomically rather than appended?

---

## Part 12 — Client (Rust) — Daemon and State (30 min)

| File | Why |
|------|-----|
| `client/src/main.rs` | CLI entry: subcommands dispatch to daemon IPC or run directly |
| `client/src/daemon.rs` | Long-running daemon: IPC socket, RuntimeState, command dispatch |
| `client/src/ipc.rs` | IPC message enum — commands and replies between CLI and daemon |
| `client/src/state_store.rs` | Encrypted durable state (device cert, tokens) — decrypted only in-process |
| `client/src/runtime.rs` | `RuntimeState` — decrypted active state in memory only |
| `client/src/grpc.rs` | gRPC client to Controller ClientService |
| `client/src/login.rs` | PKCE OAuth flow: browser open → callback → token exchange |
| `client/src/config.rs` | Client config: controller URL, state path |

### Commands (read in this order)

| File | Why |
|------|-----|
| `client/src/cmd/login.rs` | Login flow trigger |
| `client/src/cmd/setup.rs` | Device enrollment: generate key, get cert signed |
| `client/src/cmd/resources.rs` | `zecurity resources` — list ACL snapshot |
| `client/src/cmd/status.rs` | Daemon status |
| `client/src/cmd/logout.rs` | Token revoke + state wipe |

**Sprint 9 additions (M4 to implement):**
- `client/src/tun.rs` — TUN device `zecurity0`, route management
- `client/src/net_stack.rs` — smoltcp loop: intercept app packets, open QUIC stream per flow
- `client/src/tunnel_pool.rs` — QUIC connection pool (one conn per Connector, many streams)
- `client/src/cmd/up.rs` / `client/src/cmd/down.rs` — `zecurity up` / `zecurity down`

**Checkpoint:** Where does the device private key live after `zecurity setup`? When is it decrypted, and when is it wiped from memory?

---

## Part 13 — Admin UI (React) (20 min)

| File | Why |
|------|-----|
| `admin/src/App.tsx` | All routes: which pages exist, which require ADMIN |
| `admin/src/apollo/client.ts` | Apollo setup: auth link, error link, token refresh |
| `admin/src/components/layout/Sidebar.tsx` | Nav links — role-gated |
| `admin/src/pages/Dashboard.tsx` | Entry point after login |
| `admin/src/pages/Connectors.tsx` + `ConnectorDetail.tsx` | Connector list + install command modal |
| `admin/src/pages/Resources.tsx` + `ResourceDiscovery.tsx` | Resource management + LAN scan |
| `admin/src/pages/Groups.tsx` + `GroupDetail.tsx` | Group and policy management (Sprint 8) |

**Sprint 9 additions (M1 to implement):**
- `admin/src/pages/AccessLog.tsx` — connector_logs table, 10s poll
- `admin/src/pages/DeviceManagement.tsx` — enrolled devices, revoke flow

**Checkpoint:** How does the frontend know if the logged-in user is ADMIN or MEMBER? Where is that stored and how does it gate navigation?

---

## Part 14 — End-to-End Data Flows (45 min)

Read these cross-cutting flows after studying the individual components. Trace them line-by-line through the code.

### Flow 1: Shield creation (full lifecycle)
1. Admin creates Shield in UI → GraphQL `createShield` mutation
2. `zecurity-shield install` command → `/enroll` POST to Connector `:9091`
3. Connector proxies CSR to Controller → Controller signs cert → returns workspace CA
4. Shield connects Control stream to Connector `:9091`
5. Shield sends first Heartbeat → Connector returns resource instructions + piggybacks ACL
6. Shield applies nftables rules

### Flow 2: RDE device tunnel (Sprint 9 goal)
1. `zecurity up` → daemon creates TUN `zecurity0`, installs routes for ACL resource IPs
2. App opens TCP to resource IP → OS routes to `zecurity0`
3. smoltcp intercepts → daemon opens QUIC stream to Connector `:9092`
4. JSON `TunnelRequest` sent with device cert SPIFFE ID + destination + port
5. Connector: CRL check → ACL check → route decision
6. **Protected path:** `AgentTunnelHub.open_relay_session(shield_id)` → `TunnelOpen` sent to Shield on Control stream → Shield opens local TCP to resource → `TunnelOpened` reply → data relayed via `TunnelData`
7. **Direct path:** `TcpStream::connect(resource)` → `copy_bidirectional`
8. `TunnelResponse { ok: true, quic_addr }` sent to client
9. Data flows; on close, `TunnelClose` sent to Shield

### Flow 3: Certificate revocation check
1. Admin revokes device in UI → `revokeDevice` mutation → sets `revoked_at` in DB
2. Connector fetches `GET /ca.crl?workspace_id=<uuid>` every 5 min → `CrlManager` parses DER, caches serials
3. Next device connection → `handle_stream` extracts cert serial from TLS peer cert → `crl_manager.is_revoked(serial)` → deny if revoked

---

## Part 15 — Migrations and Schema (15 min)

Read the migrations in order — they tell the story of how the schema evolved.

```
controller/migrations/
  001_*.sql   — workspaces, users, oauth sessions
  002_*.sql   — connectors
  003_*.sql   — shields
  004_*.sql   — resources
  005_*.sql   — discovery scans
  006_*.sql   — resource_scan_results
  007_*.sql   — workspace_members + roles
  008_*.sql   — invitations
  009_*.sql   — connector_certs / pki tables
  010_*.sql   — groups
  011_*.sql   — group_members
  012_*.sql   — policy_rules + acl_snapshots (Sprint 8)
  013_*.sql   — workspace_members extended (Sprint 8.5)
  014_*.sql   — connector_logs + client_devices (Sprint 9 M2)
```

**Checkpoint:** Which tables are partitioned or keyed by `workspace_id`? Why does every cross-tenant query need that column even when querying by primary key?

---

## Study Order Summary

```
1. System shape (ADRs + path.md)         → mental model
2. Proto contracts                        → message vocabulary
3. Controller: PKI + auth                 → trust anchor
4. Controller: Connector lifecycle        → enrollment + control
5. Controller: Policy engine              → ACL compilation
6. Controller: Shield + resources         → resource delivery
7. Connector: startup + enrollment        → Rust side of enrollment
8. Connector: control stream + policy     → ACL push + application
9. Connector: agent_server + hub          → Shield relay infrastructure
10. Connector: device tunnel layer        → Sprint 9 RDE entry point
11. Shield: full lifecycle               → nftables + relay (M4 to extend)
12. Client: daemon + state               → end-user side
13. Admin UI                             → frontend wiring
14. End-to-end flows                     → synthesis
15. Migrations                           → schema history
```

**Total estimated time: 6–7 hours for a thorough first pass. 2–3 hours for a fast skim.**

---

## Key Invariants to Internalize

| Rule | Where enforced |
|------|---------------|
| Every cross-tenant query scoped by `workspace_id` | Middleware → resolver → all DB queries |
| Missing ACL/resource/SPIFFE = default-deny | `policy.PolicyCache.authorize()` in Connector |
| Shield heartbeats to Connector `:9091` only — never Controller | Shield `control_stream.rs` |
| nftables `chain resource_protect` always flushed+rebuilt atomically | `shield/src/network.rs` |
| Connector receives ACL via heartbeat piggyback — Controller not in tunnel hot path | ADR-001 |
| Client private key decrypted only in daemon memory, never written to disk unencrypted | `state_store.rs` + ADR-002 |
| Proto field numbers are permanent — never reuse or renumber | `proto/shield/v1/shield.proto` comments |
| Max 16 KB per `TunnelData` frame — enforced both sides | `agent_tunnel.rs:MAX_CHUNK`, Shield tunnel |
| CrlManager takes bare `CrlManager` (not `Arc<CrlManager>`) — it is already Arc-wrapped internally | `crl.rs`, `quic_listener.rs`, `main.rs` |
