# Provider Dashboard — Architecture Discovery Report

> **Status:** Discovery only. No code, migrations or APIs were changed.
> **Date:** 2026-09-23 · **Branch inspected:** `fix/permission-test-shared-db-race` (HEAD `edc4d3f`, based on `fixed-pendings`)
> **Method:** Direct reading of all 36 SQL migrations, plus targeted tracing of controller (Go), relay / connector / shield / client (Rust), protobufs, GraphQL schemas and the admin UI. Where ADRs disagree with code, **code is reported**. Claims marked **UNCERTAIN** could not be fully confirmed from the repository. Claims marked **(code-path analysis)** were derived by reading code, not by running the system.

All paths are relative to the repository root. `controller/…` paths are Go; `relay/src`, `connector/src`, `shield/src`, `client/src` are Rust.

---

## Table of contents

1. Repository structure
2. Current tenancy model
3. Current relay architecture
4. Relay lifecycle
5. Tenant lifecycle
6. Relay pool / placement architecture
7. Region and data residency
8. PKI architecture
9. Identity and provider authentication
10. Audit architecture
11. Monitoring and telemetry
12. Existing API surface
13. Database relationship map
14. Existing provider-level abstraction
15. Security boundaries
16. Feature feasibility matrix
17. Architectural risks
18. Blocking questions
19. Recommended provider-dashboard domain model
20. Executive summary

---

## 1. Repository structure

### 1.1 Top level

| Path | Responsibility |
|---|---|
| `controller/` | Go control plane: GraphQL + REST (HTTP `:8080`), gRPC (`:9090`, TLS 1.3), Prometheus (`127.0.0.1:9102`). Owns Postgres + Valkey. |
| `relay/` | Rust QUIC relay (`zecurity-relay`), UDP `:9093`, ALPN `ztna-relay-v1`. Bridges client ↔ connector when direct path fails. |
| `connector/` | Rust connector deployed per remote network. Control stream to controller; QUIC tunnel listener `:9092`; shield-facing gRPC `:9091`; relay selection. |
| `shield/` | Rust agent on resource hosts ("tunneler" in the brief's terms). nftables `resource_protect` chain; control stream terminates at **connector** `:9091`. |
| `client/` | Rust end-user CLI + daemon (TUN, device cert, posture). |
| `proto/{connector,shield,client,relay}/v1` | gRPC contracts; Go stubs in `controller/gen`. |
| `admin/` | React 19 / Vite / Apollo tenant admin UI. |
| `scripts/` | Local install/uninstall scripts incl. `relay-local-install.sh`, `run-relay-local.sh`. |
| `.zecurity-obs/` | Obsidian vault: ADRs (`Decisions/ADR-001..028`), sprint plans, findings. |
| `.github/workflows/` | `ci.yml` (Go build/vet/test w/ Postgres+Valkey; Rust matrix client/connector/shield/relay; admin test+build) and release workflows for client/connector/shield (**no relay, controller or admin release/deploy workflow**). |

### 1.2 Controller (`controller/`)

| Package | Responsibility / key entry points |
|---|---|
| `cmd/server/main.go` | Composition root. Env config, `pki.Init`, route table (lines ~341–450), gRPC server (~451–479), background loops (~529–624), `publicRootFields` (713–717), `seedProviderUsers` (~1088). |
| `graph/*.graphqls`, `graph/resolvers/` | Tenant GraphQL API. `@hasRole` directive (`resolvers/directives.go:23`). |
| `internal/auth` | Tenant OAuth/OIDC login, PKCE (Valkey), JWT issue, refresh/logout, **provider** Google login (`provider_auth.go`). |
| `internal/identity` | Identity resolution (`resolver.go`), lifecycle gate, `identity_generation` revocation (`revocation.go`), audit sink. |
| `internal/idp` | Per-workspace IdP connections (`identity_connections`). |
| `internal/scim` | SCIM 2.0 server, SCIM tokens, identity conflicts. |
| `internal/bootstrap` | JIT workspace creation on first login (`bootstrap.go`). |
| `internal/tenant`, `internal/db` | `TenantContext` in ctx; `TenantDB` (asserts context only). |
| `internal/middleware` | `AuthMiddleware`, `WorkspaceGuard`, `RequireRole`, `RequireProvider`. |
| `internal/permission` | Fine-grained `workspace_permissions` (one permission constant). |
| `internal/provider` | Provider users store, provider JWT (`session.go`), `Authz.decide` (`authz.go`), actor ctx. |
| `internal/relay` | Relay admin REST (`admin_handler.go`), `Provision`/`Heartbeat` gRPC, store, capacity labels, expiry loop, relay CRL handler. |
| `internal/pki` | Root / Intermediate / Workspace CAs, leaf signing, CRLs, secret encryption. |
| `internal/spiffe`, `internal/appmeta` | SPIFFE helpers & identity constants (mirrored in Rust `appmeta.rs`). |
| `internal/connector` | Connector enrollment, control stream, ACL pusher, SPIFFE interceptors, relay revocation checker, disconnect watcher, CA/CRL endpoints. |
| `internal/shield` | Shield enrollment, renewal, heartbeat (via connector), disconnect watcher. |
| `internal/client` | `ClientService` gRPC (device enroll/renew, ACL + transport snapshot, posture), device-trust outbox handlers. |
| `internal/policy` | ACL compiler, snapshot cache (epoch CAS), `Notifier.NotifyPolicyChange`. |
| `internal/transport` | Sprint 13 `TransportSnapshot` compiler, cache, `NotifyTopologyChange`. |
| `internal/resource`, `internal/discovery`, `internal/posture` | Resources + reconciler, discovery/scans, device posture. |
| `internal/outbox` | Durable transactional outbox (`outbox_events`); processor + reaper. |
| `internal/audit` | `audit.Record` / `RecordTx` → `audit_logs`. |
| `internal/metrics` | Private Prometheus registry (reconcile metrics only). |
| `migrations/` | 36 SQL files (001–036, with duplicate prefixes 016/031/034). |

### 1.3 Migrations

Applied **only** via `controller/docker-compose.yml` mounting `./migrations` into `/docker-entrypoint-initdb.d`. There is **no migration runner and no schema-version table** in the code. Postgres runs these scripts only on first initialisation of an empty data volume, in lexical order. Duplicate numeric prefixes (`016_audit_logs`/`016_email_lowercase`, `031_device_profile_manual_trust`/`031_identity_federation`, `034_device_status`/`034_scim_directory_sync`) rely on lexical ordering. How production/staging schemas are migrated is **UNCERTAIN** (not in repo).

### 1.4 Configuration

- Controller: environment variables only (optional `.env`). Required: `DATABASE_URL`, `JWT_SECRET` (≥32 bytes), `VALKEY_URL`, `PKI_MASTER_SECRET`, Google OAuth (tenant + client + `PROVIDER_GOOGLE_REDIRECT_URI`), `SCIM_TOKEN_HASH_KEY`, `CONTROLLER_HOST`, `CONTROLLER_HTTP_URL`. Relevant optional: `PROVIDER_BOOTSTRAP_EMAILS`, `RELAY_CERT_TTL` (30d), `RELAY_HEARTBEAT_DB_WRITE_INTERVAL` (5m), `CONNECTOR_CERT_TTL` (7d), `CONNECTOR_DISCONNECT_THRESHOLD` (90s), `SHIELD_DISCONNECT_THRESHOLD` (120s), outbox/posture tunables.
- Relay (`relay/src/config.rs`): `RELAY_ID`, `CONTROLLER_ADDR`, `RELAY_CA_FINGERPRINT` required; `RELAY_PROVISIONING_TOKEN[_FILE]`, `RELAY_DNS_SANS`, `RELAY_IP_SANS`, `RELAY_BIND`, `RELAY_MAX_CONNECTIONS` (1024), probe/bridge limits. **No region, no public address config** (public address is observed by the controller).
- Connector (`connector/src/config.rs`): `CONTROLLER_ADDR`, `ENROLLMENT_TOKEN`, relay tuning (reprobe 300s, drain 120s). **No relay address config** (pushed by controller).

### 1.5 Frontend

`admin/` — React 19, Vite 8, TS, Tailwind 4, Radix, Apollo Client 4, react-router 7, zustand. Routes in `admin/src/App.tsx:62-102`; all management routes behind `AdminLayout` (role `ADMIN`). **There are no provider or relay pages; no code calls `/provider/*`.** Codegen: `admin/codegen.yml` over the tenant `.graphqls` files.

### 1.6 Tests relevant to this report

- Relay: `controller/internal/relay/*_test.go` (admin handler, provision, heartbeat, expiry, capacity label, list version, revoke integration); Rust inline tests in `relay/src/*`.
- Provider: `internal/provider/{authz,session}_test.go`, `internal/middleware/provider_test.go`.
- PKI: `internal/pki/*_test.go` (root/intermediate/workspace integration, chain audit, relay CSR, relay CRL, secret crypto); `connector/spiffe_test.go`; `connector/relay_revocation_test.go`.
- Transport/policy: `internal/transport/*_test.go`, `internal/policy/*_test.go`.
- Authz: `graph/schema_authz_test.go` (`TestAllFieldsAuthorized` — **omits `idp.graphqls` and `posture.graphqls`** from `schemaFiles`, lines 43–52); `cmd/server/route_test.go` (public-field routing).
- **Not tested:** `AuthMiddleware`, `WorkspaceGuard`, `RequireRole`, `HasRole`, `bootstrap.Provision`; revoked connector/shield status resurrection; relay heartbeat/expiry interaction.

---

## 2. Current tenancy model

| Question | Answer (with evidence) |
|---|---|
| Tenant entity | `workspaces` (`001_schema.sql`). |
| Primary identifier | `workspaces.id UUID`. Also `slug UNIQUE` and `trust_domain UNIQUE` (`ws-<slug>.zecurity.in`, migration 002). |
| tenant / workspace / organization / account | **"tenant" and "workspace" are exact aliases** (column named `tenant_id` in older tables, `workspace_id` in newer; both FK `workspaces(id)`). **No organization or account entity exists.** |
| Tables with a tenant column | `tenant_id`: `users`, `workspace_ca_keys`, `remote_networks`, `connectors`, `shields`, `resources`, `audit_logs`, `identity_connections` (nullable → platform IdP), `external_identities`. `workspace_id`: `invitations`, `client_devices`, `groups`, `access_rules`, `workspace_members`, `connector_logs`, `device_posture_reports`, `device_profiles`, `resource_profile_bindings`, `device_profile_evaluations`, `outbox_events`, `scim_sync_instances`, `scim_tokens`, `scim_identity_conflicts`, `workspace_permissions`. Scoped via parent FK only: `group_members`, `shield_discovered_services`, `connector_scan_results` (no FK at all), `device_posture_observations`, `device_profile_requirements`. |
| Global (non-tenant) tables | `ca_root`, `ca_intermediate`, `relays`, `relay_certificates`, `connector_relay_placement` (tenant only via `connectors`), `provider_users`, `provider_audit_logs`. |
| Users ↔ tenants | One `users` row **per workspace** (`users.tenant_id NOT NULL`, `UNIQUE(tenant_id, provider_sub)`). Membership/invite lifecycle also in `workspace_members` (`UNIQUE(workspace_id,email)`). JWT role comes from `users.role`. |
| Connectors ↔ tenants | `connectors.tenant_id` + `remote_network_id` (both CASCADE). SPIFFE trust domain per workspace. |
| Policies ↔ tenants | `groups.workspace_id`, `access_rules.workspace_id` (+ resource/group FKs), device profiles `workspace_id`. |
| Devices/shields ↔ tenants | `client_devices.workspace_id` + `user_id`; `shields.tenant_id` + `connector_id`. |
| Multiple tenants per user | Schema allows (row per workspace). **Login effectively prevents it for the platform Google IdP**: `identity.Service.Authenticate` calls `Resolve(..., tenantID="")` (`internal/identity/service.go:62`) which does `ORDER BY ei.created_at LIMIT 1` (`resolver.go:38-39`) → always lands in the oldest workspace; JIT/invite-join only runs if nothing resolves. No workspace switcher. Workspace-scoped enterprise IdPs select their own tenant. |
| Provider/global scope above tenant | **Yes, partially** — a separate provider identity tier exists (§9, §14), but it only governs relays. No entity groups workspaces. |
| Tenant A vs B isolation | (1) Tenant JWT carries `tenant_id` (HS256, no `aud`); (2) `middleware.AuthMiddleware` injects `TenantContext`; `WorkspaceGuard` requires `workspaces.status='active'`; (3) **every SQL statement manually filters by `tenant_id`/`workspace_id`** — `db.TenantDB` only asserts a context exists and many stores use the raw pool; (4) **no Postgres RLS** (grep: none), no per-tenant DB role; (5) FK + composite UNIQUE constraints, but **no composite FKs** enforcing child/parent same-tenant (e.g. `access_rules(resource_id, group_id)` are separate FKs); (6) agent plane: per-workspace CA + trust domain, relay enforces same-trust-domain pairing. |
| Where enforced | Middleware (identity/context) + application SQL convention (repository/resolver layer) + PKI trust domains. **Not** at DB level. |

Relationships (tenant core):

```text
workspaces 1─┬─< users >─┬─< external_identities >── identity_connections (tenant_id NULL = platform IdP)
             │           ├─< group_members >── groups
             │           └─< client_devices ─< device_posture_reports
             ├─< workspace_members (email-keyed invite lifecycle)
             ├─1 workspace_ca_keys
             ├─< remote_networks ─< connectors ─< shields ─< resources ─< access_rules >── groups
             └─< audit_logs, outbox_events(RESTRICT), connector_logs, scim_*, workspace_permissions, device_profiles
```

---

## 3. Current relay architecture

**What a relay is.** A provider-operated, **global (not tenant-scoped)** QUIC bridge. It holds a registry of connectors that registered with it (in memory, `relay/src/state.rs`) and, on a client `Lookup{connector_id}`, opens a bidi stream to that connector and byte-pipes (`relay/src/session.rs`). It enforces only identity, role and **same trust domain** between client and connector (`relay/src/spiffe.rs:120-132`); it performs **no ACL** — the tunnel is end-to-end client↔connector inside the relay stream.

**DB entity: `relays`** (migrations 019, 020, 023, 024, 028):

| Column group | Columns |
|---|---|
| Identity | `id UUID PK` (canonical lowercase enforced), `name` |
| Lifecycle | `status` CHECK `pending/active/inactive/deleted/revoked`, `enrollment_token_jti`, `created_at`, `updated_at` |
| Operator intent (unused) | `dns_allowlist TEXT[]`, `ip_allowlist TEXT[]` — **written at create, never enforced** (see below) |
| Cert (current) | `cert_serial`, `cert_not_after` |
| Heartbeat | `version`, `hostname`, `last_heartbeat_at` |
| Addressing (observed) | `public_addr`, `observed_ip INET`, `observed_port`, `address_scope` (public/private/loopback/link_local/unknown) |
| Capacity | `connection_count`, `max_connections`, `capacity_label` (high/medium/low), `pending_capacity_label`, `pending_label_since`, `last_label_changed_at` |

No `tenant_id`, `region`, `pool`, `provider_org_id`, or drain columns. Go mirror: `RelayRow` (`controller/internal/relay/store.go:28-44`, omits capacity columns).

Related: `relay_certificates` (027: per-cert history, `serial UNIQUE`, `revoked_at`, `revocation_reason`, never deleted, FK without cascade); `connector_relay_placement` (022: PK `connector_id` → one relay per connector).

| Question | Answer |
|---|---|
| Enrollment | Provider operator `POST /provider/relays` → `AdminHandler.Create` (`internal/relay/admin_handler.go:48-136`) inserts `pending` row and returns a provisioning token. Relay runs `ensure_provisioned` (`relay/src/provision.rs:26-57`) → `RelayService.Provision`. |
| Bootstrap token | **Yes.** HS256 JWT signed with the shared `JWT_SECRET`, `aud=relay-provisioning`, `sub=relay_id`, 24h TTL (`internal/relay/token.go:29-49`). |
| Token storage | JTI in Valkey `relay:provisioning:jti:<jti>` (24h TTL) and plaintext in `relays.enrollment_token_jti`. Token itself never stored. |
| One-time use | **Yes.** Atomic Valkey `GETDEL` in `Provision` (`provision.go`), relay-ID match rechecked. Burned **before** signing (a failure after burn consumes the token). **No re-issue endpoint** — `ActionRelayIssueToken` only checked within Create. |
| Cert issuance | `pki.SignRelayCert` (`controller/internal/pki/relay.go:34-110`), **signed by the platform Intermediate CA**. P-384 only, CSR self-signature, exactly one URI SAN = relay SPIFFE ID, EKU Server+Client, TTL `RELAY_CERT_TTL` (30d). Recorded in `relays.cert_serial` and `relay_certificates` by `MarkProvisioned` (`store.go:103-143`). |
| Private key generation | **On the relay** (`relay/src/csr.rs:23-56`, rcgen P-384). Stored `relay.key` 0600 in `RELAY_STATE_DIR`. |
| DNS/IP SANs | Taken from the **relay's own request** (`req.DnsSans`/`req.IpSans`, `provision.go:102-106,170`) and passed to `SignRelayCert` as the "allowed" list. **The operator-registered `relays.dns_allowlist/ip_allowlist` are never read during Provision** (only by `store.go:82` select). A token holder can obtain arbitrary DNS/IP SANs. |
| Provider CA | **Not implemented.** No provider-specific or relay-specific CA; relays chain directly to the platform Intermediate. |
| Identity | `spiffe://zecurity.in/relay/<uuid>`, CN `relay-<uuid>` (`internal/appmeta/identity.go`). |
| Controller authenticates relay | gRPC TLS 1.3 with `RequestClientCert`; `UnarySPIFFEInterceptor` (`internal/connector/spiffe.go`) verifies role=relay, trust domain `zecurity.in`, chain to Intermediate with ClientAuth EKU (`verifyRelayCertificate`, ~234–253), and `RelayRevocationChecker` (in-memory revoked serials, refreshed 60s, fail-closed). `Heartbeat` re-checks SAN = `RelaySPIFFEID(id)`. |
| Persistent connection | **None.** Unary `Heartbeat` polling every 30s (server-directed `next_heartbeat_seconds`), 10s timeout, reconnect after 5s forever. **No controller→relay push channel.** |
| Heartbeat message | `relay.v1.HeartbeatRequest{version, hostname, uptime_seconds, registered_connectors, listen_port, connection_count, max_connections}` → `HeartbeatResponse{server_time_unix, next_heartbeat_seconds}` (`proto/relay/v1/relay.proto:43-51`). |
| Health/status available | `relays.status`, `last_heartbeat_at`, `capacity_label`, observed address fields. Liveness also in Valkey `relay:heartbeat:last:<id>` (**not read by anything**). |
| Version | `relays.version` (EXISTS, DB only). |
| Region | **Not stored anywhere.** |
| Capacity | `connection_count`/`max_connections` persisted (throttled, §11), `capacity_label` hysteresis. Note unit mismatch: `connection_count` counts bridged lookup streams (`ACTIVE_STREAMS`), `max_connections` is the QUIC connection semaphore. |
| Active sessions | Only `connection_count` (active bridged streams). No per-session records. `registered_connectors` and `uptime_seconds` received and **discarded**. |
| Bandwidth | **Not tracked.** No relay metrics endpoint. |
| Relay pools | **Do not exist.** |
| Placement | Connector-reported, recorded in `connector_relay_placement` (§6). |
| Primary/failover | **No persistent relationship.** Connector-side dynamic failover only (§6). |

---

## 4. Relay lifecycle

| Transition | What exists today | What would need to be added for a provider dashboard |
|---|---|---|
| **Bootstrap** | `POST /provider/relays` (RequireProvider + `CanIssueProvisioningToken`) creates `pending` row + 24h token; audited `relay.create` (best-effort). | Token re-issue for existing relay; enforce stored SAN allowlists; list/get endpoints; pool/region assignment at creation. |
| **Enrollment** | `RelayService.Provision` (interceptor-skipped; token-authenticated), relay pins `/ca.crt` by `RELAY_CA_FINGERPRINT`. | Rate limiting; burn-after-success semantics or retry path. |
| **Certificate issuance** | Intermediate-signed, 30d, recorded in `relay_certificates`. | SANs bound to operator-registered row; optional dedicated relay/provider CA. |
| **Registration** | `MarkProvisioned` → `active`, `enrollment_token_jti=NULL`. | — |
| **Heartbeat** | Unary 30s; Valkey-throttled DB writes (every 5 min unless metadata changes). | Richer telemetry (CPU/mem/bandwidth/sessions/region); command channel. |
| **Active** | Eligible for global `LabelledRelayList` if `active` + label high/medium + public address. | Pool/region-scoped eligibility. |
| **Stale → inactive** | `relay.RunExpiryLoop` (60s sweep, 90s threshold, hard-coded, `main.go:579`); `inactive → active` on next DB-written heartbeat. **Likely flapping bug (code-path analysis):** eviction reads DB `last_heartbeat_at` (`store.go:408-431`) but the DB is only written ~every 5 min when metadata is unchanged (`heartbeat.go:178-209`), so a healthy relay can be marked `inactive` ~90–150s after each write. | Base liveness on Valkey key or align write interval with expiry threshold. |
| **Draining** | **Does not exist** as status or operator action. Connectors have their own migration drain (`relay_drain_timeout_secs`). Relay process has no shutdown/drain handling. | New status + controller behaviour (remove from list, keep existing sessions) + relay-side awareness (needs a command channel or heartbeat response field). |
| **Renewal** | **Does not exist.** No `RenewCert` RPC; relay skips provisioning if cert files exist; `RecordIssuedCert` has no callers. After 30 days the relay fails mTLS; recovery = new relay (no token re-issue). | Renewal RPC + scheduler; or token re-issue + re-provision flow. |
| **Revoked** | `POST /provider/relays/{id}/revoke?reason=` → `Store.RevokeRelay` (tx, `FOR UPDATE`, revokes all `relay_certificates` rows, status `revoked`, audit in-tx) → `OnRelayRevoked` (refresh checker, rebroadcast list, notify topology per affected workspace). `/relay.crl` (Intermediate-signed, 10-min nextUpdate) consumed by connectors (1s monitor closes sessions) and clients. | Relay process is **not told**: it keeps retrying heartbeats (PermissionDenied) every 5s and keeps its QUIC listener up; enforcement relies on peers checking the CRL. Relays do **not** consume `/relay.crl` themselves. |
| **Decommissioned** | `DELETE /provider/relays/{id}` = revoke + `status='deleted'`. No hard delete (by design, CRL history). | Self-shutdown on revocation; decommission confirmation. |
| **Reconnect after revocation** | Controller interceptor denies revoked serial (fail-closed); `MarkProvisioned` rejects `revoked`/`deleted`; re-provisioning with same ID impossible. | — |

---

## 5. Tenant lifecycle

| Aspect | Current state |
|---|---|
| Creation API | **No explicit API.** JIT on first OAuth login: public GraphQL `initiateAuth(provider, workspaceName, connectionId)` → `/auth/callback` (`internal/auth/callback.go`) → `bootstrap.Service.Provision` (`internal/bootstrap/bootstrap.go:33-197`). |
| Records created (single tx) | `workspaces` (`provisioning`), `users` (admin), `external_identities`, Workspace CA (`workspace_ca_keys`), `workspaces.status='active'` + `ca_cert_pem`, `workspace_members` (admin). |
| Slug collision | UNIQUE violation fails login; no suffixing. |
| Default configuration | Only CA + admin. **No default groups, policies, resources, relay placement.** `groups.origin='system'` exists but nothing seeds it. |
| Default relay placement | None — relays are global; every connector sees the same list. |
| Status fields | `workspaces.status` CHECK `provisioning/active/suspended/deleted`; `platform_login_enabled`. |
| Suspension | Enforced on read (`WorkspaceGuard` 403s non-active; enrollment checks `active`; disconnect watcher limited to active workspaces). **No code path sets `suspended`.** |
| Deletion | **No API.** No soft-delete machinery. A raw `DELETE FROM workspaces` would cascade nearly everything **including `audit_logs`**, and would be **blocked by `outbox_events` (ON DELETE RESTRICT)**. |
| Connector enrollment | Admin GraphQL `generateConnectorToken` → REST `POST /api/connectors/{id}/token` → gRPC `ConnectorService.Enroll`. |
| User enrollment | Invitations (`createInvitation`, REST accept; always role `member`), JIT via IdP, SCIM provisioning. |
| Policy creation | Admin GraphQL groups / access rules / device profiles. |

**Verdict:** "Suspend tenant" is **half-built**: the state value and read-side guards exist, but there is no writer, no audit, no defined effect on live agent sessions (connectors with open control streams, cached ACL/transport snapshots, relays) and no un-suspend. It requires a defined state machine and propagation, not just a new mutation.

---

## 6. Relay pool / placement architecture

**No Provider → Region → Relay Pool → Relay hierarchy exists.** Regions, pools and primary/secondary assignment are absent from schema, code and protos. ADR-015's "Placement Engine" (leader lease, `placement_generation`) was **not built**; it was superseded by ADR-016.

**What currently determines Tenant → Connector → Relay (ADR-016, implemented):**

1. **Controller labels** (`internal/relay/store.go:455-513` `BuildLabelledRelayList`): all relays with `status='active'`, `capacity_label IN ('high','medium')`, and a public address. Capacity label = fill ratio with hard-coded hysteresis thresholds (`capacity_label.go`; 60s hold-down; `WithLabelHoldDown` never wired, ADR-016 `RELAY_TIER*` env vars do not exist). List version = FNV-64a content fingerprint.
2. **Controller pushes the same global list to every connector in every tenant** — at control-stream open and via `ConnectorRegistry.BroadcastRelayList` on label change, address change, eviction, revocation (`internal/connector/control_stream.go:274-289, 414-425`). Proto: `LabelledRelayList` (connector control message field 17).
3. **Connector chooses** (`connector/src/relay_selector.rs`, `relay_probe.rs`, `relay_ranking.rs`): warm start from persisted top-5 ranking, else random Tier-1 → Tier-2 → backoff; RTT probes on list change and every 300s; migrate if >15% **and** >10 ms better or active relay left the list; make-before-break with drain; failover through ranked list.
4. **Connector reports placement** via `ConnectorRelayState` events and health-report fields → `UpsertPlacement`/`DeletePlacement` → `NotifyTopologyChange`. The controller does not validate the reported relay's status.
5. **Clients learn the relay** per connector from `TransportSnapshot` (`ClientService.GetTransportSnapshot`, 60s poll), joined `LEFT JOIN relays … status='active'` (`internal/transport/store.go`).

| Question | Answer |
|---|---|
| Static selection? | No (the old static `RELAY_ADDR` was removed per ADR-018). |
| Dynamic? | Yes — connector-side, RTT-based among controller-labelled candidates. |
| Relay address in connector config? | No — pushed via `LabelledRelayList.relay_addr`. |
| Controller chooses? | Controller only filters/labels; does not assign. |
| Connector chooses? | Yes. |
| Scheduling algorithm | Capacity hysteresis (controller) + RTT ranking/migration thresholds (connector). |
| Tenant uses multiple relays? | Yes, implicitly (different connectors may sit on different relays). |
| Connector uses multiple relays simultaneously? | No — one placement row; two sessions only briefly during migration. |
| Connector failover? | Yes, connector-side (ranked retry, CRL monitor triggers failover). |
| Transitional leftovers | ACL snapshot still carries per-connector `relay_addr`/`relay_spiffe_id` and a **global** `ACLSnapshot.RelayAddr` from `policy.GetActiveRelay` (`LIMIT 1`, not workspace-scoped; `policy/store.go:479-499`) — the deferred ADR-018 Phase 4 removal. |

**Where relay-pool assignment would logically belong:** the filter step in `BuildLabelledRelayList` — today one global list is broadcast. A pool abstraction implies the list becomes per-tenant (or per-connector), which changes `BroadcastRelayList` from "one message to all" into per-workspace fan-out, and changes the list-version semantics. The connector selector can remain unchanged if it simply receives a narrower list. (Documented as observation, not a proposal.)

---

## 7. Region and data residency

Searched for region, geography, residency, location, jurisdiction, locality, deployment region across Go, Rust, SQL, proto, GraphQL.

| Attached to | Status |
|---|---|
| Tenants | **Absent.** |
| Connectors | **Absent.** (`remote_networks.location` exists but is a descriptive enum `home/office/aws/gcp/azure/other` — not a geographic region and not used for routing.) |
| Relays | **Absent.** Only observed IP / address scope. |
| Relay pools | Entity does not exist. |
| Resources | **Absent.** |
| Sessions | Entity does not exist. |

Data residency: single Postgres, single Valkey, single controller (in-process caches assume one instance). **No residency concept exists.**

---

## 8. PKI architecture

### 8.1 Hierarchy (actual)

```text
Zecurity Root CA                  ca_root (singleton) · EC P-384 · 10y · MaxPathLen=2      pki/root.go
└── Zecurity Intermediate CA      ca_intermediate (singleton) · P-384 · 5y · MaxPathLen=1  pki/intermediate.go
    │   ("platform" CA; key decrypted into memory at startup)
    ├── Controller gRPC server leaf   in-memory only · spiffe://zecurity.in/controller/global · TTL = CONNECTOR_CERT_TTL (7d)
    ├── Relay leaves                  spiffe://zecurity.in/relay/<uuid> · 30d · CSR from relay
    ├── Relay CRL (/relay.crl)        nextUpdate +10 min
    └── Workspace CA (per tenant)     workspace_ca_keys · P-384 · 2y · MaxPathLen=0 · URI SAN tenant:<id>
        ├── Connector leaves          spiffe://ws-<slug>.zecurity.in/connector/<id> · 7d
        ├── Shield leaves             spiffe://ws-<slug>.zecurity.in/shield/<id> · 7d
        ├── Client device leaves      spiffe://ws-<slug>.zecurity.in/client/<id> · 7d (TPM keys P-256)
        └── Workspace CRL (/ca.crl?workspace_id=)  revoked client_devices + connectors · nextUpdate +24h
```

**There is no Provider CA.** Relays and the controller hang directly off the platform Intermediate.

### 8.2 Properties

| Property | Current implementation |
|---|---|
| Root CA location | Postgres `ca_root`, key AES-256-GCM encrypted with HKDF(`PKI_MASTER_SECRET`, info=`root-ca`). |
| Intermediate | Single row; `SELECT … LIMIT 1` hard-coded throughout. |
| Workspace CA | Generated in bootstrap tx; key decrypted **on every signing and CRL call**. |
| CA storage | Postgres, env-derived key; **no KMS/HSM**. |
| CA rotation | **Not supported** (root, intermediate, workspace). Rotating `PKI_MASTER_SECRET` breaks all keys. Startup auto-regenerates root/intermediate only if no workspaces exist. |
| Issuance | Always CSR-based (keys on device) except the controller's own key. Connector/shield via one-time Valkey-JTI JWT; client via user access token; relay via provider-minted provisioning JWT. |
| Renewal | Client devices: **implemented** (ADR-028; key-fingerprint pinned, daemon scheduler). Shields: **implemented** (connector triggers `ReEnroll` at 48h, proxies to controller). Connectors: **RPC exists but the controller never sends `ReEnroll`** since the streaming cutover (commit `372744d`) — `CONNECTOR_RENEWAL_WINDOW` is read but unused → connector certs appear to expire after 7 days. Relays: **none**. Controller server cert: generated once at startup with 7-day TTL, static `tls.Config.Certificates`, no reload → **UNCERTAIN / likely** expired cert after ~7 days uptime. |
| Revocation | Client (`revoked_at`), connector (`revoked_at`, `revocation_reason`), relay (`relay_certificates.revoked_at`). **Shields: status only, no CRL entry.** |
| CRL | Workspace CRL and relay CRL, generated per request, unauthenticated, uncached, signed by Workspace CA / Intermediate. |
| OCSP | **Does not exist** (ADR-027 defers). |
| Serial tracking | Current serial only on `connectors`, `shields`, `client_devices`, `relays`; **full history only in `relay_certificates`**. Renewal overwrites the serial for clients/connectors → an older, still-valid cert of a revoked entity is not on the CRL. |
| Expiry tracking | `cert_not_after` columns; **no sweeper, metric or alert**. CA `not_after` never read. |
| Public-key fingerprint | `client_devices.public_key_fingerprint` only (036). |
| Status fields | See §13. `client_devices.status='renew_pending'` is never written. |
| Inventory | **No certificate inventory API.** GraphQL exposes `certNotAfter` on Connector, Shield, ClientDevice; nothing for relays or CAs. |
| Scope | Workspace CA + connector/shield/client leaves = **tenant-scoped**; Root, Intermediate, relay, controller = **platform-scoped**. |
| Interceptor verification | Chain verification only for roles **connector** (vs Workspace CA) and **relay** (vs Intermediate + revocation checker). Other roles reach handlers with unverified certs (ADR-015-SPIFFE not implemented); handlers gate on role/JWT. Interceptor does **not** check connector revocation. |
| Enrollment tokens | HS256 JWTs signed with the **same `JWT_SECRET` as user sessions**. Connector/shield tokens have no `aud`. ADR-011 Fix C (invalidate old JTI on regenerate) not implemented. |

---

## 9. Identity and provider authentication

### 9.1 Tenant plane

| Aspect | Current state |
|---|---|
| Login | OIDC/OAuth only; no passwords. Platform Google IdP (global `identity_connections` row, `tenant_id NULL`) + per-workspace enterprise OIDC (google/github/okta/entra/generic). SAML reserved, not built. |
| Session | Access JWT HS256 (`JWT_SECRET`), 15 min; refresh token in Valkey `refresh:<userID>` (168h idle, 720h absolute), httpOnly cookie or CLI header. **One refresh session per user** (web and CLI overwrite each other). |
| Invalidation | `users.identity_generation` (`gen` claim) bumped by `identity.Revoker`; enforced **only at `/auth/refresh`** — access tokens live ≤15 min after revocation. Logout deletes refresh session. No JTI denylist. |
| RBAC | Roles `admin/member/viewer`; `@hasRole` — **every guarded field uses `[ADMIN]` only**; VIEWER ≡ MEMBER. Invitations always `member`; **no role-change path**. Refresh re-stamps role from old claims. |
| Permissions | `workspace_permissions` with a single constant `identity.mapping.break_glass`. |
| MFA | **Does not exist** (`RequireBreakGlassMFA` is a no-op stub); delegated to IdP. |
| API auth | No API keys/service tokens. SCIM bearer tokens (HMAC-hashed). Agents via mTLS/SPIFFE. |

### 9.2 Provider plane (exists — ADR-021, alpha)

| Aspect | Current state |
|---|---|
| Identity store | `provider_users(id, email UNIQUE, role CHECK super-admin/relay-ops, disabled_at)` — no link to `users`/`workspaces`. Seeded from `PROVIDER_BOOTSTRAP_EMAILS` as super-admin at every startup (`seedProviderUsers`). |
| Authentication | `GET /provider/auth/initiate` + `/provider/auth/callback` (`internal/auth/provider_auth.go`): Google PKCE (hard-coded `accounts.google.com`), `VerifyIDToken` (checks `email_verified`, `idtoken.go:119`), then `provider.Store.GetByEmail`. **Matched by email, not Google `sub`; no hosted-domain (`hd`) check.** |
| Token | `provider.ProviderClaims{Role, Email}` HS256 with the **same `JWT_SECRET` and issuer** as tenant tokens, distinguished by `aud=["provider"]`; 15 min; returned as JSON. **No refresh, no logout, no revocation/generation.** |
| Middleware | `middleware.RequireProvider` verifies token, re-reads `provider_users` by email per request (disable/role change is immediate), injects `provider.Actor`. |
| Authorization | `provider.Authz.decide(actor, action, target)` (`internal/provider/authz.go:47-91`). Actions: `relay.create`, `relay.issue_token`, `relay.delete`, `relay.revoke`, `provider_user.manage`, `audit.view`. super-admin = all; relay-ops = `relay.*`. `Target` accepted but ignored. |
| MFA | Delegated to corporate Google (ADR-021); not enforced in code. |

### 9.3 Can it support Owner / Operator / Read-only / Support?

Structurally the pieces are right (separate tier, DB-authoritative role per request, single `decide()` chokepoint, reserved `Target`). Missing abstractions:

1. Role CHECK permits only two roles (migration needed).
2. `decide()` is a hard-coded switch + prefix rule; no read/write action split (`relay.read` etc.) and no read endpoints exist to authorize.
3. No tenant-facing provider actions (`workspace.*`, `tenant_user.*`, `support.*`) — provider identity has **no path into tenant data at all**.
4. No support-access model: no impersonation, no time-boxed/consented grant, no break-glass.
5. No provider user management routes (`Store.Create/Disable` exist but unrouted).
6. No provider session revocation, no step-up/MFA assertion (`amr`/`acr`) check.
7. Shared signing key with tenant sessions — isolation rests solely on claim checks.

---

## 10. Audit architecture

| Question | `audit_logs` (tenant) | `provider_audit_logs` (provider) |
|---|---|---|
| Exists | Yes (016) | Yes (026) |
| Append-only | **Convention only** (migration comment). No trigger, no REVOKE, no hash chain. | Same. |
| Actor | `actor_user_id` (nullable, **no FK**), `actor_email` | `provider_user_id` (FK, nullable), `provider_email`, `ip_address` (raw `RemoteAddr`, no XFF handling) |
| Target | `target_type`, `target_id TEXT` | same |
| Before/after | Free-form `details JSONB` only | same |
| Timestamps | DB `NOW()` default (app never supplies) — trustworthy relative to DB clock | same |
| Modifiable/deletable | Yes by any DB role with privileges; **cascade-deleted with workspace** | Yes by DB role; not cascaded |
| Scope | Tenant | Global |
| Query API | **None** (no GraphQL/REST) | **None** (`audit.view` action defined, no route) |

Write paths: `audit.Record` (best-effort, post-commit, errors discarded with `_ =` at most call sites) / `audit.RecordTx`; `provider.Store.InsertAudit[Tx]`.

**Audited today (tenant):** `resource.force_delete`; IdP connection create/update/status/delete; SCIM config/token mint/rotate/revoke; `permission.grant`; break-glass override; `identity.user.provisioned` (JIT/SCIM); `session.generation.bump`; SCIM conflict lifecycle; `device.cert.renewed` / `renew_denied`; `device.revoked` / `re_enroll_required` (outbox handlers); `outbox.recover`.
**Audited today (provider):** `relay.create` (best-effort), `relay.revoke` and `relay.delete` (in-tx).

**Not audited:** login success/failure, logout, refresh; invitations; connector/shield token generation, enrollment, revocation, deletion; remote network CRUD; resource CRUD (except force delete); all group/access-rule/policy mutations; device profile changes; admin `revokeDevice` (UNCERTAIN); provider login; provider user seeding.

`outbox_events` (033) is a durable side-effect queue (SCIM device-trust events), **not** an audit log.

---

## 11. Monitoring and telemetry

Controller Prometheus (`internal/metrics/metrics.go`): only `reconcile_*` counters/gauges + Go/process collectors. No HTTP/gRPC/request metrics. **No Rust component exposes metrics.** `/health` pings Postgres only.

### Relay

| Metric | Status | Evidence |
|---|---|---|
| Heartbeat | EXISTS | `RelayService.Heartbeat` 30s |
| Online/offline | PARTIALLY EXISTS | `status` active/inactive via expiry loop; likely flapping (§4); no read API |
| Active sessions | PARTIALLY EXISTS | `connection_count` = active bridged streams, persisted with ≤5 min lag; no API |
| Bandwidth | DOES NOT EXIST | — |
| CPU | DOES NOT EXIST | — |
| Memory | DOES NOT EXIST | — |
| Connection counts | PARTIALLY EXISTS | `connection_count`/`max_connections` persisted; `registered_connectors` discarded |
| Version | EXISTS (DB only) | `relays.version`; no read API |

### Tenant

| Metric | Status | Evidence |
|---|---|---|
| Active connectors | EXISTS | `connectors.status`, GraphQL |
| Online/offline connectors | EXISTS | status + disconnect watcher (90s); `RemoteNetwork.networkHealth` |
| Active sessions | DOES NOT EXIST | Only connector in-memory `SessionRegistry`; no table/counter |
| Tunnel establishment success/failure | PARTIALLY EXISTS | Per-event `connector_logs.action` allow/deny/error; no aggregate; no retention job |
| Bytes transferred | DOES NOT EXIST | `copy_bidirectional` byte counts discarded (`connector/src/device_tunnel.rs`) |

### Control plane

| Metric | Status | Evidence |
|---|---|---|
| API health | PARTIALLY EXISTS | `/health` + Go/process metrics |
| DB health | PARTIALLY EXISTS | `/health` ping |
| PKI health | DOES NOT EXIST | (startup chain audit only) |
| Policy store health | DOES NOT EXIST | in-memory caches, no health |
| Background worker health | DOES NOT EXIST | logs only |
| Valkey health | DOES NOT EXIST at runtime | version check at startup only |

---

## 12. Existing API surface

Auth scopes: **PUB** = unauthenticated; **T** = any tenant user (JWT + WorkspaceGuard); **TA** = tenant ADMIN; **PRV** = provider JWT + `RequireProvider`; **mTLS** = SPIFFE agent; **TOK** = one-time token.

### 12.1 Provider REST (the only provider-scope APIs)

| API | Purpose | Auth | Request → Response | Entities | Caller | Reusable | Required changes |
|---|---|---|---|---|---|---|---|
| `GET /provider/auth/initiate`, `/provider/auth/callback` | Provider login | PUB (OAuth) | → auth URL / `{token, expires_in}` | provider_users | none (no UI) | YES | refresh/logout/revocation; `sub` binding; `hd` check; MFA assertion |
| `GET /provider/me` | Current provider identity | PRV | → actor | provider_users | none | YES | — |
| `GET /provider/users` | List provider users | PRV (super-admin) | → list | provider_users | none | YES | add create/disable/role-change routes |
| `POST /provider/relays` | Create relay + provisioning token | PRV + `CanIssueProvisioningToken` | `{name, dns_allowlist, ip_allowlist}` → `{relay_id, provisioning_token, expires_at}` | relays, Valkey, provider_audit_logs | none (curl/scripts) | YES | enforce allowlists; pool/region fields |
| `POST /provider/relays/{id}/revoke?reason=` | Revoke relay | PRV + `CanRevokeRelay` | → 2xx | relays, relay_certificates, provider_audit_logs | none | YES | notify relay process |
| `DELETE /provider/relays/{id}` | Soft delete (revoke + deleted) | PRV + `CanDeleteRelay` | → 2xx | same | none | YES | — |
| *(missing)* `GET /provider/relays`, relay detail, token re-issue, drain, audit query | — | — | — | — | — | — | **Must be added** |

### 12.2 Public/infra HTTP

| API | Purpose | Auth | Reusable | Notes |
|---|---|---|---|---|
| `GET /health` | Liveness | PUB | PARTIAL | DB ping only |
| `GET /ca.crt` | Intermediate CA PEM | PUB | YES | CA inventory would need more |
| `GET /ca.crl?workspace_id=` | Workspace CRL | PUB | NO (data plane) | uncached, decrypts CA key per hit |
| `GET /relay.crl` | Relay CRL | PUB | NO (data plane) | — |
| `/metrics` (`127.0.0.1:9102`) | Prometheus | loopback | PARTIAL | reconcile only |

### 12.3 Tenant GraphQL (all **workspace-scoped via JWT `tenant_id`**; none reusable by a provider identity without a new scope)

| Domain | Operations | Auth | Reusable by provider? | Required changes |
|---|---|---|---|---|
| Workspace | `me`, `workspace`, `users` | T / TA | NO | provider cross-tenant read APIs |
| Public auth | `initiateAuth`, `lookupWorkspace`, `lookupWorkspacesByEmail` | PUB | NO | (note: email→workspace enumeration) |
| Connectors / networks | `remoteNetworks`, `remoteNetwork`, `connectors`, `connector`, `createRemoteNetwork`, `deleteRemoteNetwork`, `generateConnectorToken`, `revokeConnector`, `deleteConnector` | TA | NO (query logic reusable) | provider read variant with explicit `tenant_id` + audit |
| Shields | `shields`, `shield`, `generateShieldToken`, `revokeShield`, `deleteShield` | TA | NO | same |
| Resources | `resources`, `allResources`, create/update/protect/unprotect/delete/forceDelete | TA | NO | — |
| Policies | groups CRUD, members, resource assignment | TA | NO | — |
| Devices | `myDevices` (T), `clientDevices`, `revokeDevice` (TA) | T/TA | NO | — |
| Invitations | `invitation`, `createInvitation` | T/TA | NO | — |
| Discovery | `getDiscoveredServices`, `getScanResults`, `promoteDiscoveredService`, `triggerScan` | TA | NO | — |
| Logs | `connectorLogs(limit)` | TA | NO | — |
| Posture | device profile queries/mutations | TA | NO | — |
| IdP / SCIM / permissions | idp connections, SCIM tokens/conflicts, `grantPermission`, break-glass | TA | NO | — |
| Relays / provider / audit | **none** | — | — | — |

Tenant REST: `POST /api/invitations` (TA), `GET /api/invitations/{token}` (PUB), `POST /api/invitations/{token}/accept` (T), `POST /api/connectors/{id}/token`, `POST /api/shields/{id}/token` (TA), `/auth/refresh`, `/auth/logout`, `GET /workspaces/{slug}/auth` (PUB), `/scim/v2/*` (SCIM token), `/api/clients/callback`.

### 12.4 gRPC (agent plane; not dashboard APIs, but the data sources)

| Service / RPC | Auth | Relevance to provider dashboard |
|---|---|---|
| `RelayService.Provision` | TOK (provisioning JWT) | Enrollment |
| `RelayService.Heartbeat` | mTLS relay + revocation checker | Source of relay telemetry |
| `ConnectorService.Enroll` / `Control` (stream) / `RenewCert` / `Goodbye` | TOK / mTLS | Connector health (15s), relay placement, relay list push |
| `ShieldService.Enroll`, `RenewCert` (controller); `Control` at connector `:9091` | TOK / mTLS | Shield health via connector batch |
| `ClientService.*` (`GetAuthConfig`, `InitiateAuth`, `TokenExchange`, `EnrollDevice`, `RenewCert`, `GetACLSnapshot`, `GetTransportSnapshot`, `RevokeDevice`, `ReportDevicePosture`) | JWT in request (interceptor-skipped) | Transport snapshot shows relay per connector |

**Session management APIs:** none for tunnels (no session entity). Tenant sessions: refresh/logout only; no "list sessions" or admin "revoke user sessions" API (generation bump happens only as side effect of IdP disable / SCIM).

---

## 13. Database relationship map (current)

```text
Provider                ── NOT CURRENTLY IMPLEMENTED as an organisation entity
  provider_users        (id PK, email UNIQUE, role CHECK{super-admin,relay-ops}, disabled_at)  [global]
  provider_audit_logs   (id PK, provider_user_id FK→provider_users NULL, …)                    [global, no cascade]

Region                  ── NOT CURRENTLY IMPLEMENTED
Relay Pool              ── NOT CURRENTLY IMPLEMENTED
Session (tunnel)        ── NOT CURRENTLY IMPLEMENTED (only connector_logs events; connector in-memory registry)
Policy (single entity)  ── NOT A TABLE: groups + group_members + access_rules + device_profiles(+requirements, bindings)

CA
  ca_root               (id PK) singleton by convention                                         [global]
  ca_intermediate       (id PK) singleton by convention                                         [global]
  workspace_ca_keys     (id PK, tenant_id UNIQUE FK→workspaces CASCADE)                         [tenant]

Tenant/Workspace
  workspaces            (id PK, slug UNIQUE, trust_domain UNIQUE, status CHECK{provisioning,active,suspended,deleted},
                         platform_login_enabled)

User
  users                 (id PK, tenant_id FK CASCADE, UNIQUE(tenant_id,provider_sub), role, status{active,suspended,deleted,locked},
                         identity_generation, provisioned_by, provisioning_owner, sync_instance_id FK SET NULL)
  workspace_members     (id PK, workspace_id FK CASCADE, user_id FK CASCADE NULL, UNIQUE(workspace_id,email), role, status)
  external_identities   (tenant_id, user_id, connection_id FKs CASCADE; UNIQUE(tenant_id,connection_id,subject))
  identity_connections  (tenant_id NULL=platform; partial UNIQUE(provider) WHERE tenant_id IS NULL;
                         partial UNIQUE(tenant_id,issuer) WHERE status<>'deleted'; status{active,disabled,deleted})
  invitations, workspace_permissions(PK ws,user,permission), scim_tokens, scim_sync_instances, scim_identity_conflicts

Connector
  remote_networks       (tenant_id FK CASCADE, UNIQUE(tenant_id,name), status{active,deleted})
  connectors            (tenant_id, remote_network_id FKs CASCADE, status{pending,active,disconnected,revoked},
                         cert_serial, cert_not_after, revoked_at, revocation_reason, last_heartbeat_at, version, …)
                         idx: tenant, (remote_network,tenant), token_jti, trust_domain, partial revoked

Tunneler/Device
  shields               (tenant_id, remote_network_id, connector_id FKs CASCADE, status{pending,active,disconnected,revoked},
                         UNIQUE(tenant_id,interface_addr) partial, snapshot_generation, cert_serial, …)
  client_devices        (user_id, workspace_id FKs CASCADE, status{active,re_enroll_required,renew_pending},
                         revoked_at, cert_serial, cert_not_after, spiffe_id, public_key_fingerprint)
  device_posture_reports / device_profile_evaluations → client_devices (FK, **no cascade**)

Relay                                                                                             [global]
  relays                (id PK, status{pending,active,inactive,deleted,revoked}, capacity & address columns)
                         idx: token_jti, status
  relay_certificates    (id PK, relay_id FK **no cascade**, serial UNIQUE, revoked_at) idx: relay_id, partial revoked
  connector_relay_placement (connector_id PK FK→connectors CASCADE, relay_id FK→relays CASCADE, source{event,heartbeat})

Certificate (generic)   ── NOT CURRENTLY IMPLEMENTED as one table; serial/expiry columns per entity + relay_certificates

Resources / policy
  resources             (tenant_id, remote_network_id CASCADE; shield_id SET NULL; UNIQUE(tenant_id,remote_network_id,host,name))
  groups (partial UNIQUEs by origin), group_members, access_rules (UNIQUE(resource_id,group_id))

Audit Event
  audit_logs            (tenant_id FK **CASCADE**, actor_user_id no FK) idx: (tenant_id,created_at DESC), (tenant_id,target_type,target_id)
  provider_audit_logs   (see above)

Other
  outbox_events         (workspace_id FK **RESTRICT**), connector_logs (workspace_id CASCADE, connector_id TEXT no FK)
```

**Tenant-isolation constraints at DB level:** only FKs to `workspaces` + composite uniques. **No RLS, no composite FKs** ensuring cross-table tenant consistency.
**Cascade behaviour of note:** deleting a workspace cascades to users, connectors, shields, resources, audit logs, CA key; blocked by `outbox_events`. Deleting a relay row cascades placements but is blocked by `relay_certificates` (no cascade) — deletion is soft by design.
**Soft-delete/status columns:** `workspaces.status`, `users.status`, `workspace_members.status`, `remote_networks.status`, `connectors/shields.status` (+`revoked_at`), `relays.status`, `client_devices.revoked_at/status`, `identity_connections.status`, `provider_users.disabled_at`, `resources.status` (`deleting` tombstone; `deleted_at` dropped in 017).

---

## 14. Existing provider-level abstraction

The codebase **already distinguishes a provider plane from a tenant plane — but only for relays**:

```text
Provider plane (exists, narrow)                Tenant plane (full)
  provider_users, provider_audit_logs            workspaces + all tenant tables
  /provider/* REST, RequireProvider              /graphql, /api/*, AuthMiddleware + WorkspaceGuard
  provider JWT (aud=provider)                    tenant JWT (tenant_id claim)
  Authz.decide (relay.*, provider_user.*, audit.view)   @hasRole(ADMIN)
  relays, relay_certificates, Intermediate CA    Workspace CAs, connectors, shields, clients
```

There is **no bridge** between them: a provider identity cannot read or act on any workspace, and there is no provider org entity grouping workspaces.

| Introducing full provider scope requires… | Needed? | Notes |
|---|---|---|
| New database entities | **Yes** | pools/regions/assignments, support grants, possibly provider org; tenant lifecycle audit fields |
| New authorization scope | **Partially** | `decide()` exists; needs new actions, read/write split, target scoping |
| New API namespace | **Partially** | `/provider/*` REST exists; needs read endpoints and tenant-facing routes (REST or a provider GraphQL schema) |
| New middleware | **Mostly no** | `RequireProvider` is reusable; support-access needs a distinct, audited path into tenant context |
| New identity claims | **Yes (small)** | session generation/revocation for provider tokens; MFA/`amr` assertion; possibly separate signing key/audience hardening |
| New audit scope | **Partially** | `provider_audit_logs` exists; needs query API, append-only enforcement, and cross-reference when provider acts on a tenant |

---

## 15. Security boundaries

| Boundary | Current state | Risk for a provider dashboard |
|---|---|---|
| Provider ↔ tenant tokens | Same HS256 `JWT_SECRET`, same issuer; separated by `aud` vs `tenant_id` claim checks. Enrollment/provisioning JWTs use the same key. | Compromise of one secret = forge any token type (tenant admin, provider super-admin, enrollment). A dashboard concentrates power in provider tokens. |
| Provider identity binding | Email match against `provider_users`; no `sub`, no `hd` check; auto-seeded super-admins from env on every boot. | Any Google account with a matching verified email becomes provider. Env change silently re-grants super-admin. |
| Provider sessions | 15-min JWT, no revocation, no MFA enforcement in code. | Disable is immediate (DB re-read) — good; but no step-up for dangerous ops. |
| Tenant isolation | Application-level SQL filters only; no RLS; no composite FKs. | Any provider cross-tenant query path is a new, unguarded class of query — must be designed explicitly. |
| CA boundaries | Root + Intermediate keys in DB, encrypted with one env secret; Intermediate decrypted in memory; Workspace CA keys decrypted per operation. | Provider dashboard must never expose CA material; no rotation means a leak is unrecoverable. |
| Relay trust | Global relays authenticate peers from all tenants; enforce same trust domain. Relay SANs are relay-chosen. Relay not told of its own revocation. | A compromised relay sees all tenants' (end-to-end encrypted) traffic metadata; revocation depends on every peer's CRL refresh. |
| Bootstrap tokens | One-time JTIs (good). No re-issue; burned before signing; connector/shield tokens lack `aud`; ADR-011 old-JTI invalidation missing. | Dashboard token re-issue must invalidate prior JTIs. |
| Unauthenticated endpoints | `/ca.crl` decrypts Workspace CA key per request, uncached; `lookupWorkspacesByEmail` enumerates tenants by email. | DoS / enumeration surface. |
| Interceptor | Chain verification only for connector & relay roles. | Adding provider-plane gRPC would inherit this gap. |
| Audit | Mutable, best-effort, cascade-deleted, no read API. | Provider actions against tenants must not be silently lost or deletable by tenant deletion. |

**Dangerous operations already reachable:** relay revoke/delete (global blast radius: affects every tenant whose connectors are placed on that relay); PKI master-secret mismatch auto-truncation of root/intermediate when no workspaces exist (`pki/service.go:203-257`).

**Existing designs that would make a provider dashboard dangerous if built on unchanged:**
1. **Revoked connector/shield status resurrection (code-path analysis, not reproduced):**
   - Connector: the control-stream close handler sets `status='disconnected'` without a `revoked` guard (`internal/connector/control_stream.go:363-381`), and stream open sets `active` unguarded (`:330-347`). The open-time check only rejects `status='revoked'`, so after revoke → stream close → reconnect the connector is `active` again. `revoked_at` stays set, so the connector remains on the workspace CRL (relays reject it), but the controller shows it active and `RevokeConnector` (guarded by `revoked_at IS NULL`) cannot re-revoke it. Whether the direct client→connector path is affected is **UNCERTAIN** (client does not consume the workspace CRL).
   - Shield: `UpdateShieldHealth` sets `status` from the connector's report (always `active`) without checking current status (`internal/shield/heartbeat.go:34-51`); a revoked shield returns to `active` within ~15s. Shields have no CRL entry.
   A dashboard showing "revoked" state or offering revoke buttons would present false guarantees.
2. **Relay status flapping** (§4) would make fleet health displays unreliable.
3. **Connector (7d) and controller server (7d) certificate expiry without renewal** would present as fleet-wide outages.

---

## 16. Feature feasibility matrix

| Feature | Existing support | Partial | Missing | Main backend changes |
|---|---|---|---|---|
| Relay enrollment | ✅ `POST /provider/relays` + `Provision` | | | Token re-issue; enforce stored SAN allowlists; rate limit |
| Relay certificate issuance | ✅ Intermediate-signed, 30d | | | Bind SANs to operator row |
| Relay revoke | ✅ revoke/delete + relay CRL + checker | | | Notify relay process; relay self-check |
| Relay fleet | | ◐ `relays` table has status/version/capacity | No list/get API, no UI | Read APIs; fix heartbeat/expiry flapping |
| Relay capacity | | ◐ `connection_count`, `max_connections`, labels | CPU/mem/bandwidth | Extend heartbeat; configurable thresholds; persist timely |
| Relay drain | | | ❌ | New status + list exclusion + relay awareness (command channel) |
| Tenant CRUD | | ◐ JIT create; status column + guard | Provider list/suspend/delete | Provider tenant APIs; suspension state machine + propagation; deletion semantics (outbox RESTRICT, audit cascade) |
| Tenant → relay pool | | | ❌ | Pool entities; per-tenant relay list; broadcast refactor |
| Region constraints | | | ❌ | Region on relays/pools/tenants; filter in list builder |
| Failover pool | | ◐ connector-side dynamic failover | No designated secondary pool | Pool priority semantics |
| Tenant quotas | | | ❌ | Quota entities + enforcement points |
| Certificate inventory | | ◐ per-entity serial/expiry columns; `relay_certificates` | Unified view, history for connectors/clients | Inventory read model; per-cert history tables |
| Expiry alerting | | | ❌ | Expiry sweeper/metrics; fix connector renewal trigger & controller cert reload first |
| CRL/OCSP | | ◐ Workspace + relay CRLs | OCSP; shield revocation; historical serials | Shield CRL entries; per-cert history; CRL caching |
| CA hierarchy | | ◐ Root → Intermediate → Workspace | Provider CA; rotation | Rotation design; multi-intermediate support |
| Provider audit | | ◐ `provider_audit_logs` written for relays | Query API; immutability; broader coverage | Read API; DB-level append-only; audit all provider actions |
| Provider RBAC | | ◐ two roles + `decide()` | Owner/Operator/RO/Support; read actions | Role migration; action taxonomy; target scoping |
| MFA | | | ❌ (IdP-delegated only) | Enforce `amr`/step-up or provider-owned MFA |
| Monitoring | | ◐ status/heartbeat columns; reconcile metrics | Relay/tenant/control-plane metrics | Metrics in relay/connector; health endpoints |
| Relay rollout | | | ❌ | Version targeting; no relay release workflow exists |
| Tenant migration | | | ❌ | No concept (between pools/regions/controllers) |
| Support access | | | ❌ | Consent/time-boxed grants; audited tenant-context entry |
| Metering | | | ❌ | Bytes/sessions not measured anywhere |

---

## 17. Architectural risks

| # | Area | Risk (evidence) |
|---|---|---|
| 1 | Tenant isolation | Isolation is SQL-convention only (no RLS, no composite FKs). Provider cross-tenant queries would be the first deliberate bypass of the `tenant_id` convention; `TenantDB` offers no protection. |
| 2 | Provider/global authz | Shared `JWT_SECRET` across tenant, provider and enrollment tokens; provider identity bound by email only; env-driven super-admin re-seeding; no provider session revocation. |
| 3 | Relay lifecycle | No renewal (30d cliff), no drain, no token re-issue; relay not informed of revocation. |
| 4 | Relay pool abstraction | Relay list is global and broadcast identically to all connectors; placement is connector-chosen. Pools require per-tenant lists and change version semantics. |
| 5 | Certificate lifecycle | Connector renewal trigger missing since `372744d`; controller server cert 7d without reload (likely); no CA rotation; single env master secret. |
| 6 | Certificate revocation | Renewal overwrites serial (old certs of revoked clients/connectors stay valid ≤7d at relays); shields have no crypto revocation; connector/shield status resurrection (§15). |
| 7 | Audit immutability | Convention-only append-only; tenant audit cascade-deleted with workspace; most writes best-effort post-commit. |
| 8 | Telemetry | Almost no metrics; relay telemetry persisted with ≤5 min lag; no bytes/sessions anywhere. |
| 9 | Concurrent admin ops | Relay revoke uses `FOR UPDATE` (good). Tenant-side connector revoke vs control-stream updates race (TOCTOU at stream open). No optimistic concurrency on admin mutations generally. |
| 10 | Relay state races | Heartbeat DB writes vs expiry sweeper (flapping); capacity label hold-down effectively stretched to DB-write cadence; placement delete on migration "disconnected" event after "switched" (transient no-relay in transport snapshot ≤15s). |
| 11 | Stale heartbeat state | Valkey holds true liveness but eviction reads DB; `uptime_seconds`/`registered_connectors` dropped. |
| 12 | Failover | Connector failover exists but no controller-directed failover; relays evicted/revoked keep placement rows (hidden by join). |
| 13 | Transaction boundaries | Relay provisioning token burned before signing; audit writes mostly outside business tx; bootstrap is one tx (good). |
| 14 | Eventual consistency | Policy/transport version counters are **in-memory** and reset on restart → possible version collisions serving stale "up to date"; all caches single-process → **controller cannot be horizontally scaled** today. |
| 15 | Command propagation | Controller → connector via bounded (128) mailbox, drop on full; connector → shield `try_send` drop on full; repair via reconciler/heartbeat. No controller → relay channel at all. Disconnect watcher notifies policy but **not transport**. |
| 16 | Reconciler architecture | Shield reconciler counters in memory; no worker health/metrics. |
| 17 | Heartbeat architecture | Three different models: connector stream (15s), shield via connector batch (15s), relay unary poll (30s, throttled). Offline thresholds 90s/120s/90s. |
| 18 | Schema migration | No migration runner/version table; init-scripts only; duplicate numeric prefixes. Every provider-dashboard schema change inherits this. |

---

## 18. BLOCKING QUESTIONS

### Provider architecture

**Q1. Is "provider" a single operator (Zecurity itself) or can there be multiple providers/partners (MSP/reseller) each owning tenants and relays?**
- Why it matters: determines whether a `provider_org` entity and `provider_org_id` on workspaces/relays are needed; ADR-021 reserved `Target` scoping but nothing implements it.
- Code: `internal/provider/authz.go` (`Target` ignored); ADR-021.
- Tables: `provider_users`, `relays`, `workspaces`.

**Q2. Will the controller need to run as multiple instances (HA) before or alongside the dashboard?**
- Why it matters: policy/transport caches, version counters, connector registry and revocation list are in-process; dashboard actions (drain, revoke, suspend) that must reach all connectors depend on this.
- Code: `internal/policy/notifier.go`, `internal/transport/notifier.go:22-36`, `internal/connector/control_stream.go` (registry).
- Tables: none (in-memory).

### Tenant model

**Q3. What should "suspend tenant" do to live traffic?** (block admin UI only? stop new tunnels? tear down existing sessions? revoke agent certs? keep audit/SCIM running?)
- Why it matters: status value exists but no semantics; enforcement points differ (WorkspaceGuard, enrollment, ACL compiler, relays).
- Code: `internal/middleware/workspace.go`, `internal/connector/enrollment.go`, `internal/connector/disconnect_watcher.go`.
- Tables: `workspaces.status`.

**Q4. What is the required tenant deletion semantics — hard delete, soft delete with retention, crypto-shred?** And must audit logs survive tenant deletion?
- Why it matters: current FKs cascade audit logs away and `outbox_events` RESTRICT blocks deletion entirely.
- Code: none (no deletion path).
- Tables: `workspaces`, `audit_logs`, `outbox_events`.

**Q5. Should one human be able to belong to multiple workspaces via the platform IdP?**
- Why it matters: provider "view tenants of user X" and support flows depend on it; current resolver pins to oldest workspace.
- Code: `internal/identity/service.go:62`, `resolver.go:38-39`.
- Tables: `users`, `external_identities`.

### Relay architecture

**Q6. Is relay certificate renewal expected to be in-band (renew RPC) or via re-provisioning with a new token?**
- Code: `proto/relay/v1/relay.proto`, `internal/relay/store.go` (`RecordIssuedCert` unused).
- Tables: `relays`, `relay_certificates`.

**Q7. Should the controller gain a push/command channel to relays (drain, shutdown, config), or stay poll-only with directives in `HeartbeatResponse`?**
- Code: `relay/src/heartbeat.rs`, `internal/relay/heartbeat.go`.
- Tables: `relays`.

**Q8. Are relays always provider-operated, or could a tenant bring its own relay?**
- Why it matters: relays are global and chain to the platform Intermediate; tenant-owned relays would need tenant scoping and a different CA path.
- Code: `internal/pki/relay.go`, `relay/src/tls.rs`.
- Tables: `relays`.

### Relay pools

**Q9. Is pool assignment a hard constraint (tenant may only use pool X) or a preference (prefer X, fall back to global)?**
- Why it matters: decides whether `BuildLabelledRelayList` filters or ranks, and what happens when a pool is empty/drained.
- Code: `internal/relay/store.go:455-513`, `connector/src/relay_selector.rs`.
- Tables: `relays`, `connector_relay_placement`.

**Q10. What is the assignment granularity: tenant, remote network, or connector?**
- Code: `internal/connector/control_stream.go` (`BroadcastRelayList`).
- Tables: `workspaces`, `remote_networks`, `connectors`.

**Q11. Does "region" imply data residency (legal constraint on where traffic/metadata may flow/store) or only latency placement?**
- Why it matters: residency would affect Postgres/Valkey/controller placement and logs, not just relays; nothing regional exists today.
- Code/tables: none.

### PKI

**Q12. Is a separate Provider/Relay intermediate CA required (to isolate relay issuance from the Intermediate that also signs Workspace CAs and the controller)?**
- Code: `internal/pki/relay.go`, `internal/pki/intermediate.go`.
- Tables: `ca_intermediate`.

**Q13. What CA rotation and master-secret rotation guarantees are required before exposing PKI in a dashboard?**
- Code: `internal/pki/service.go`, `crypto.go`, `secret.go`.
- Tables: `ca_root`, `ca_intermediate`, `workspace_ca_keys`.

**Q14. Should the dashboard be allowed to perform tenant-scoped PKI actions (revoke a tenant's connector/device) or only platform-scoped ones?**
- Tables: `connectors`, `client_devices`, `shields`.

### Identity/RBAC

**Q15. What is the provider IdP of record (corporate Google only? any OIDC?) and must provider users be bound to IdP `sub` / hosted domain?**
- Code: `internal/auth/provider_auth.go`.
- Tables: `provider_users`.

**Q16. Exact permission matrix for Owner / Operator / Read-only / Support, and does Support require tenant consent and time-boxing?**
- Code: `internal/provider/authz.go`.
- Tables: `provider_users`.

**Q17. Is MFA to be enforced by Zecurity (e.g. `amr` claim check, step-up for dangerous ops) or accepted as IdP policy?**
- Code: `graph/resolvers/permission_helpers.go` (stub), ADR-021.

**Q18. Should provider tokens get a separate signing key / issuer from tenant and enrollment tokens?**
- Code: `internal/provider/session.go`, `internal/auth/session.go`, `internal/relay/token.go`, `internal/connector/token.go`.

### Audit

**Q19. What immutability level is required for provider audit (DB-enforced append-only, hash chaining, external sink/WORM)? What retention?**
- Tables: `provider_audit_logs`, `audit_logs`.

**Q20. When a provider acts inside a tenant (support access), should the event appear in the tenant's `audit_logs`, the provider log, or both?**
- Tables: `audit_logs`, `provider_audit_logs`.

### Monitoring

**Q21. Which metrics are required for v1 of the dashboard (bytes, sessions, CPU/mem, tunnel success rates), and is Prometheus the intended pipeline (pull from relays) or heartbeat-carried telemetry?**
- Code: `internal/metrics/metrics.go`; relay has no metrics endpoint.

### Database

**Q22. How are schema migrations applied in non-dev environments?** (No runner or version table is in the repository.)
- Code: `controller/docker-compose.yml`.

### API

**Q23. Should the provider dashboard API be REST (extending `/provider/*`) or a separate provider GraphQL schema?**
- Why it matters: tenant GraphQL's `@hasRole` + `TenantContext` model cannot carry provider actors; `schema_authz_test.go` would need a provider equivalent.
- Code: `cmd/server/main.go:341-350,448-450`, `graph/`.

---

## 19. Recommended provider-dashboard domain model

*(Minimum additions only; derived from the gaps above. Not implemented.)*

### Already exists

- `provider_users` (identity tier), `RequireProvider` middleware, `provider.Authz.decide` chokepoint.
- `provider_audit_logs` (write path).
- `relays`, `relay_certificates` (per-cert history + revocation), `connector_relay_placement`.
- `/relay.crl`, `RelayRevocationChecker`, relay revoke/delete flows.
- `workspaces.status` including `suspended`/`deleted` values; `WorkspaceGuard`.
- Per-entity `cert_serial` / `cert_not_after` columns.

### Can be reused

- `/provider/*` REST namespace and provider JWT (`aud=provider`).
- `BuildLabelledRelayList` + `BroadcastRelayList` as the injection point for pool/region filtering.
- `TransportNotifier.NotifyTopologyChange` for propagating relay changes to clients.
- Tenant GraphQL resolver query logic (connectors/shields/devices) as a basis for provider read models — behind a new scope.
- `audit.RecordTx` / `InsertAuditTx` patterns (in-tx audit).

### Needs modification

- `provider_users.role` CHECK → Owner / Operator / Read-only / Support (+ bind `sub`, domain).
- `provider.Authz` → read/write action split, tenant-facing actions, `Target` scoping.
- Provider sessions → revocation/generation, MFA/step-up assertion; consider distinct signing key.
- `relays` → region / pool reference, drain state (`status` CHECK), enforce `dns_allowlist`/`ip_allowlist`.
- Relay heartbeat → liveness from Valkey or aligned DB writes; persist `registered_connectors`/uptime; extended telemetry fields.
- `workspaces` → suspension/deletion state machine with reason, actor, timestamps.
- `provider_audit_logs` / `audit_logs` → DB-enforced append-only; tenant audit survives tenant deletion.
- Connector & shield revocation guards (status resurrection), connector renewal trigger, controller cert reload — **prerequisites** for trustworthy fleet/cert views.

### Completely new

- **Region** (if Q11 confirms).
- **Relay Pool** and pool membership (relay ↔ pool).
- **Tenant ↔ Pool assignment** (with priority for failover pool, per Q9/Q10).
- **Provider support-access grant** (tenant, provider user, scope, expiry, consent/reason).
- **Relay command/directive** mechanism (drain, shutdown) — heartbeat-response directive or new channel (Q7).
- **Certificate inventory read model** (unified view; per-cert history for connectors/clients).
- **Telemetry/metrics** for relay and tenant usage (and, later, metering/quotas).
- **Provider audit query API**.
- *(Conditional on Q1)* **Provider organisation** entity.

---

## 20. Executive summary

**1. Current architecture.** A single-instance Go controller (GraphQL + REST + gRPC, Postgres + Valkey, in-memory caches) manages tenant-scoped workspaces. Each workspace has its own Workspace CA and trust domain; connectors, shields and client devices chain to it. Relays are global, provider-operated QUIC bridges chained to the platform Intermediate CA. The controller labels relays by capacity and broadcasts one global list; connectors choose relays by RTT and report placement; clients learn relays through the Sprint-13 `TransportSnapshot`. A narrow **provider plane already exists** (ADR-021): `provider_users` (super-admin / relay-ops), Google login with a separate `aud=provider` JWT, `RequireProvider`, `Authz.decide`, `provider_audit_logs`, and REST `/provider/relays` create/revoke/delete.

**2. What we already have.** Relay enrollment with one-time provisioning tokens; Intermediate-signed relay certs with per-cert history; relay revocation with relay CRL and a fail-closed controller checker; relay status, version, observed address, connection counts and capacity labels in the DB; connector-side relay failover; tenant status column and guard; provider identity tier, authz chokepoint and audit write path.

**3. What is missing.** Relay list/detail APIs and UI; relay renewal, drain and token re-issue; relay pools, regions and tenant→pool assignment; tenant suspend/delete flows; provider RBAC beyond two roles; support access; MFA enforcement; provider session revocation; audit query APIs and immutability; certificate inventory and expiry alerting; CA rotation and a provider CA; almost all telemetry (bandwidth, CPU/mem, sessions, bytes, worker/PKI health); metering and quotas.

**4. Biggest architectural risks.**
- Tenant isolation is application-SQL convention only (no RLS). Provider cross-tenant reads would be a new, unprotected query class.
- One `JWT_SECRET` signs tenant, provider and enrollment tokens. Provider identity is bound by email only, with super-admins re-seeded from env on every boot.
- Correctness bugs found by reading the code (not reproduced) would make a dashboard misleading:
  - revoked connectors and shields flip back to `active`;
  - relays likely flap between active and inactive (heartbeat DB throttle vs 90s expiry);
  - connector certs are never renewed after the streaming cutover (7-day cliff);
  - the controller gRPC cert is 7 days with no reload;
  - relay certs have no renewal (30-day cliff).
- The controller can't be horizontally scaled (in-memory caches and version counters, which also reset on restart).
- Audit is mutable, mostly best-effort, and tenant audit is cascade-deleted with the workspace.
- There's no migration runner: schema is applied only by Postgres init scripts.

**5. Blocking questions (top of §18).**
- Single provider or multi-provider (MSP)?
- Must the controller be HA first?
- What does "suspend tenant" do to live traffic?
- What are the tenant deletion and audit retention rules?
- Are pools a hard constraint or a preference, and at what granularity?
- Does "region" mean legal residency or only latency?
- Is a separate provider/relay CA required, and what CA rotation guarantees?
- What is the provider role matrix, and does support access need consent?
- Is MFA enforced by Zecurity or delegated to the IdP?
- What audit immutability level is required?
- Should the provider API be REST or GraphQL?
- How are migrations applied in production?

**6. Recommended next design step.**
1. Get answers to Q1, Q2, Q3, Q9/Q10 and Q11. They decide the shape of every new entity.
2. In parallel, raise a **"dashboard prerequisites" fix track** so the dashboard never displays false state:
   - connector/shield revocation guards;
   - connector renewal trigger;
   - controller cert reload;
   - relay heartbeat/expiry alignment;
   - enforcement of the stored relay SAN allowlist.
3. Then write an ADR for the provider domain model (§19): pools/regions/assignment, extended provider RBAC with support grants, and immutable queryable provider audit.
4. Build read-only APIs (relay fleet, tenant list, cert inventory) before any mutating provider operations.
