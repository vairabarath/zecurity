# Provider Dashboard — Architecture Decision Analysis

> **Status:** Analysis only. No code, migrations, protobufs or schema were changed.
> **Date:** 2026-09-23 · **Companion to:** [`provider-dashboard-architecture-discovery.md`](provider-dashboard-architecture-discovery.md)
> **Scope:** Answers Q1–Q15 raised after discovery. Each answer separates **Existing** behaviour (verified in code) from **Proposed/possible** behaviour (not built). Options are analysed, **not chosen**; the human decisions are collected at the end.
>
> **Update 2026-09-23:** product/architecture decisions have since been made. See **[Decision record](#decision-record--2026-09-23)** at the end of this document. The analysis sections below are unchanged.

**Conventions**

- **Existing** means verified in the current tree (branch `fix/permission-test-shared-db-race`, HEAD `edc4d3f`).
- **Direction** means stated in ADRs or `.zecurity-obs/pending/*`. These are product intent: ADRs marked *accepted* are binding, while `PENDING-*` documents are non-binding.
- **UNCERTAIN** means the repository cannot confirm it.
- Paths are repo-relative; `controller/` is Go, while `relay/`, `connector/`, `shield/` and `client/` are Rust.

**Product-direction documents consulted**

| Document | Status | Relevance |
|---|---|---|
| `.zecurity-obs/Decisions/ADR-021-Provider-Identity-and-Authorization.md` | **accepted**, implemented Sprint 12 | Provider tier, `decide()` chokepoint, open questions on partner scoping, sub-roles, break-glass |
| `.zecurity-obs/pending/PENDING-07-Provider-Dashboard-Vision.md` | pending (vision) | Functionality catalogue, phased roadmap (Alpha/Beta/GA), "multi-party ready" |
| `.zecurity-obs/pending/PENDING-07b-Provider-Console-Packaging.md` | pending | CLI-first alpha, separate app for Beta/GA |
| `.zecurity-obs/pending/PENDING-06-MFA-Step-Up-Auth.md` | pending | MFA delegation via `acr`/`amr`, step-up |
| `.zecurity-obs/pending/PENDING-10-Observability.md` | pending | Prometheus + OTel direction |
| `.zecurity-obs/pending/PENDING-11-Audit-Logging-SIEM.md` | pending | Tamper-evidence, SIEM export |
| `.zecurity-obs/pending/PENDING-12-Controller-HA-Multi-Region.md` | pending | HA options A/B/C |
| ADR-016 (controller-labelled, connector-probed relay), ADR-017 (transport propagation), ADR-020 (relay provisioning), ADR-027 (serial CRLs) | accepted / implemented | Relay selection, transport plane, PKI revocation |

---

## Q1 — Provider model

### Existing

- **One flat provider tier.** `provider_users(id, email UNIQUE, role CHECK {super-admin, relay-ops}, disabled_at)` (`controller/migrations/025_provider_users.sql`) has no organisation column and no relation to `workspaces` or `users`.
- **Provider audit is global.** `provider_audit_logs` (`026`) has no organisation or tenant scope.
- **`workspaces` has no owner column** (`001`, `002`, `032`). Every workspace is implicitly owned by "the platform".
- **Relays are global.** `relays` has no `tenant_id` and no `provider_org_id` (`019`–`028`).
- **Authorization carries an unused target.** `provider.Target{Type, ID}` is passed into `decide()` but ignored. The code comment says so explicitly: *"Fields are unused by the alpha policy but carried NOW so partner-scoping … becomes a decide() change later, not a signature change"* (`controller/internal/provider/authz.go:37-44`).
- No code, table, proto or GraphQL anywhere mentions MSP, reseller, partner or `provider_org` (grep across `controller`, `relay`, `connector`, `proto`, `admin/src`).

### 1. What the current architecture implies

It implies **Model A**: one Zecurity provider operating every tenant and every relay. There is exactly one provider identity pool, one global relay fleet broadcast to all connectors (`BroadcastRelayList`, `controller/internal/connector/control_stream.go:274`), one platform Intermediate CA, and no ownership edge from any tenant to any provider entity.

### 2. Is multi-provider clearly intended?

**It is intended as future direction, but it is not decided and not built.**

- **ADR-021 (accepted), Open Questions:** *"Partner/reseller scoping: stamp a nullable `provider_org_id` on `provider_users`/`relays` seeded to a single 'root' org now, or keep the model flat and add scoping only in `canManage` later? (Cheaper: flat model + chokepoint; add column when partners are real.)"* The accepted decision implemented the flat model and left this open.
- **PENDING-07 (vision, non-binding):**
  - Principle 6: *"Multi-party ready. Provider + partners/resellers as first-class scoped orgs with delegated admin — designed-for now, built later."*
  - Catalogue §6 lists "Partner / reseller management".
  - The roadmap places partner multi-tenancy at **GA**. Alpha is a "single provider org"; Beta is "still internal (+ trusted early partners maybe)".
- **PENDING-07 open questions:**
  - *"Is the alpha internal-only, or do partners touch it during alpha?"*
  - *"BYO-relay? … decide before the data model sets."*

### 3. Abstractions to preserve if we start with one provider

These seams already exist or are cheap to keep:

| Seam | Existing? | Why it matters for Model B later |
|---|---|---|
| Single `decide(actor, action, target)` chokepoint | ✅ `authz.go:79-91` | Partner scoping becomes a change in one function |
| `Target{Type, ID}` passed at every call site | ✅ | Ownership checks need the target |
| Provider identity separate from tenant identity | ✅ ADR-021 invariant | A partner admin is a provider-tier actor, not a tenant user |
| Every provider route behind `RequireProvider` | ✅ `middleware/provider.go` | One place to attach an org to `provider.Actor` |
| Dotted action namespaces | ✅ | Partner roles grant namespaces |
| **Tenant-listing queries parameterised by an "owner scope"** | ❌ (no provider tenant queries exist yet) | Cheap to design now; expensive to retrofit into every query later |
| **No hard-coded assumption that "provider" equals "all tenants"** in new read models | n/a | Aggregations (tenant counts, fleet views) must be filterable by owner |

### 4. Entities that would eventually need provider ownership (under Model B)

| Entity | Why |
|---|---|
| `provider_users` | A partner operator belongs to one partner org |
| `workspaces` | The "tenant belongs to MSP" edge |
| `relays` | Only if partners operate their own relays (ties into the PENDING-07 BYO-relay question) |
| Relay pools / regions (new) | If pools can be partner-scoped |
| `provider_audit_logs` | So partners can read their own trail without seeing others' |
| Quotas / plans / billing (new) | Partner-level limits (PENDING-07 §5, §6) |
| Support-access grants (new) | A partner supporting its own tenants |

Would **not** need provider ownership: `ca_root`, `ca_intermediate` (platform-owned), and all tenant-internal tables (they inherit ownership through `workspaces`).

### 5. Is `provider_org` necessary now?

| Choice | Consequences |
|---|---|
| **Add now** (nullable `provider_org_id`, seeded to a single "root" org) | Every new provider table and read model is org-aware from day one. The cost is one table plus nullable columns and `decide()` owner checks that are trivially true. Risk: designing an org model before partner requirements are known. |
| **Wait** (flat model, as ADR-021 recommended) | Cheapest now. The retrofit touches `provider_users`, `workspaces`, `relays`, pools, audit, and every cross-tenant query written in the meantime. The seams in §3 limit the damage to the data model and queries, not the authorisation call sites. |

The trigger ADR-021 names is *"when partners are real"*. PENDING-07 ties the urgency to whether partners touch the Alpha or Beta. That is a product decision (see the list at the end).

---

## Q2 — Controller HA requirement

### 1. Process-local state (existing)

| State | Location | Shared? | Notes |
|---|---|---|---|
| Policy version counters (`versions map[string]*atomic.Uint64`) | `controller/internal/policy/notifier.go:15-16,57-76` | **No** | Reset to 0 on restart. Drive the connector heartbeat gate and the client `known_version` check |
| Policy ACL snapshot cache + epoch map + singleflight | `internal/policy/cache.go:39-58` | **No** | Epoch CAS (ADR-013) is intra-process only |
| `pushHook` → `ACLPusher` per-workspace coalescing (`inflight map`) | `policy/notifier.go:27,75`; `internal/connector/acl_push.go:41-62` | **No** | Pushes only to streams held by this process |
| Transport version counters | `internal/transport/notifier.go:57` (comment at `:22-36` states the single-controller limitation) | **No** | No push hook; clients poll |
| Transport snapshot cache + epoch | `internal/transport/cache.go:22-28` | **No** | |
| `ConnectorRegistry` (live control streams) | `internal/connector/control_stream.go:62-143`; `ClientsForWorkspace`, `PushInstruction`, `PushScanCommand`, `BroadcastRelayList` | **No, inherently** | A gRPC stream lives in one process |
| Shield reconciler drift/absent counters | `internal/connector/reconcile.go:26-33` | **No** | Tied to the stream |
| `RelayRevocationChecker` revoked-serial set | `internal/connector/relay_revocation.go:19,65`; reloaded every 60s from the DB (`main.go:217-233`) | Reloaded from shared DB | Replicas converge within ≤60s; `OnRelayRevoked` refreshes **only the local** instance |
| Controller gRPC server certificate | Generated in memory at startup (`main.go:~457`) | Per process | Each replica would mint its own cert (all chain to the Intermediate) |
| OIDC discovery / JWKS caches | `internal/auth/providers/oidc.go:356-394` | No | Harmless to duplicate |
| Outbox handler registry | `internal/outbox/handler.go:26-33` | Static config | Harmless |

### Shared state that already exists

| State | Store |
|---|---|
| PKCE `pkce:*`, refresh sessions `refresh:*` | Valkey |
| Enrollment/provisioning JTIs | Valkey (atomic `GETDEL`) |
| Relay heartbeat liveness/metadata/throttle | Valkey `relay:heartbeat:*` |
| Relay capacity-label hysteresis | Postgres (`pending_capacity_label`, `pending_label_since`) with `FOR UPDATE` (`relay/store.go:551-609`) |
| Outbox claims | Postgres (`lease_id`, `claimed_at`, reaper). Designed for concurrent workers |
| Relay CRL, workspace CRL | Generated from Postgres per request |

### 2. State that must become shared before multiple instances are safe

1. **Version counters (policy and transport).** Different replicas would hand out unrelated version numbers. When a client or connector compares `known_version` by equality (`internal/client/service.go:~752,~805`, heartbeat gate `control_stream.go:~719`), two failures follow: it can get a false "unchanged" when counters collide, and it can churn needlessly. This is already a latent single-instance issue across restarts.
2. **Change notification fan-out.** `NotifyPolicyChange`, `NotifyTopologyChange`, `BroadcastRelayList` and `OnRelayRevoked` act only on the local process. A change handled on replica 1 must reach connectors whose streams are on replica 2. PENDING-12 calls this "the crux".
3. **Cache invalidation.** Invalidation of each replica's snapshot cache must follow from (2).
4. **Directed commands to one connector.** `PushScanCommand` (GraphQL `triggerScan`) and future drain/suspend teardowns need routing to the replica that holds the stream.
5. **Revocation propagation.** Tolerable as-is (60s DB reload), unless a stricter bound is required.

Loops that are idempotent under replication and need no sharing: disconnect watchers, relay expiry loop, posture retention, discovery purge, outbox (lease-based). Their notifications still fan out locally only, which is issue (2).

### 3. Can the dashboard be built safely before HA?

On the current single-instance deployment, yes. HA is a question of how many replicas run, not of whether the dashboard can exist. What matters is which dashboard operations assume a single process:

| Operation class | Single instance | Multiple instances (without the §2 work) |
|---|---|---|
| Read-only views (relay fleet, tenant list, cert inventory, audit search) | Safe (DB reads) | Safe. DB and Valkey are shared |
| Relay create / token issue | Safe | Safe (DB + Valkey) |
| Relay revoke / delete | Safe | **Degraded.** DB state and checker converge within ≤60s, but `broadcastRelayList` and topology notifications reach only connectors on the handling replica. Other connectors still hold the old list until a local trigger fires, and the relay CRL still protects them. Whether the expiry sweep rebroadcasts when nothing was evicted is **UNCERTAIN** |
| Relay drain (new) | Needs design | Same fan-out problem |
| Tenant suspend with stream teardown (new) | Needs design | Must reach every replica's streams |
| Tenant-scoped actions from the dashboard (connector revoke, `triggerScan`) | Works | Revoke: the next heartbeat on the owning replica closes the stream (≤15s). Scan: fails if the stream is on another replica |

### 4. Dashboard operations affected by multiple instances

Relay revoke/delete, relay drain, relay config or label changes, tenant suspend/resume, forced connector/shield disconnect, scan/diagnostic commands, and anything that expects immediate ACL or transport propagation.

### 5. Minimum architecture for HA (descriptive, not a design)

From the state inventory, the minimum set is:

1. A shared, monotonic version source per workspace for the policy and transport planes.
2. A cross-replica notification bus carrying workspace policy change, workspace topology change, relay-list change and relay revoked.
3. A way to deliver a directed command to the replica holding a given connector's stream: stream ownership recorded somewhere shared, or a broadcast that each replica filters.
4. A decision on controller server certificate issuance per replica (SANs, load-balancer identity).

PENDING-12 Option A already sketches "N replicas, control streams shard across replicas, Postgres + Valkey HA" and names cross-replica fan-out as the crux.

### 6. Is HA a dashboard prerequisite or a separate milestone?

The repository does not settle this. Facts relevant to the decision:

- PENDING-12 is **P2** and pending. PENDING-07 does not list HA as an Alpha or Beta prerequisite.
- Read-only dashboard functionality is replica-agnostic.
- Mutating operations that fan out to live streams (revoke, drain, suspend) are exactly the ones that break under multi-replica without §2. If HA lands after the dashboard, those operations need rework or a documented "single replica" constraint.

---

## Q3 — Tenant suspension semantics

### Current enforcement points of `workspaces.status` (existing)

The status value `suspended` exists in the CHECK constraint (`001_schema.sql`). **No code path writes it.**

| # | Surface | Checks workspace `active`? | Evidence |
|---|---|---|---|
| E1 | Tenant GraphQL (authenticated) + `/api/*` | **Yes**, per request | `WorkspaceGuard` (`internal/middleware/workspace.go:17-57`) |
| E2 | Web login (OAuth callback → JWT) | **No** | `identity.Resolver.Resolve` has no workspace predicate (`internal/identity/resolver.go:29-50`); `callback.go` checks IdP connection status only (`:73`). A suspended tenant's user still receives a JWT but is blocked by E1 |
| E3 | Token refresh | **No** | `internal/auth/refresh.go` checks user status/generation only |
| E4 | CLI auth start | **Yes** | `GetAuthConfig`, `InitiateAuth` → `lookupWorkspaceBySlug … status='active'` (`internal/client/store.go:38-50`, `service.go:122,155`) |
| E5 | CLI `TokenExchange`, `EnrollDevice`, `RenewCert`, `GetACLSnapshot`, `GetTransportSnapshot` | **No workspace check found** | `internal/client/service.go:336-812`. `deviceGate` checks device state only (`:671-712`). Clients of a suspended tenant keep receiving ACL and transport snapshots. That TokenExchange completes after a blocked InitiateAuth is unlikely because PKCE starts at E4 (**UNCERTAIN** on edge cases) |
| E6 | Connector/shield **new** gRPC calls and streams | **Yes** | SPIFFE interceptor `NewTrustDomainValidator` returns `ws.Status == "active"` (`internal/connector/spiffe.go:88-94`); chain CA lookup requires `w.status='active'` (`cmd/server/main.go:~964-972`). New control streams, `RenewCert` and shield renew via the connector are rejected |
| E7 | Connector **existing** control stream | **No** | Interceptor runs only at stream start; `handleConnectorHealth` does not check workspace status. An open stream keeps receiving ACL pushes and relay lists |
| E8 | Connector / shield enrollment | **Yes** | `connector/enrollment.go:119`, `shield/enrollment.go:77` |
| E9 | Disconnect watchers | Only active workspaces swept | `connector/disconnect_watcher.go:48`, `shield/heartbeat.go:107`. Connectors of a suspended tenant would **never be marked disconnected** |
| E10 | ACL / transport compilers | **No** workspace check found | `internal/policy`, `internal/transport` |
| E11 | Relay | **No knowledge of workspace status** | Pairs by trust domain (`relay/src/spiffe.rs:120-132`), chains to Intermediate, checks workspace CRL |
| E12 | Workspace CRL `/ca.crl` | No status check found | Served for any workspace |
| E13 | SCIM `/scim/v2/*` | No workspace-status check found (**UNCERTAIN**) | `internal/scim/middleware.go` |
| E14 | Outbox processor | No status check | Continues processing |

### Semantics evaluated

For each option: current support, what is missing, and per-component effects. Tunnels are client↔connector direct (`:9092`) or via relay; the connector enforces the ACL locally (`connector/src/device_tunnel.rs`).

#### S1 — Administrative suspension (block login and admin; tunnels continue)

- **Supported now:** admin GraphQL/REST blocked (E1); CLI login start blocked (E4); new connector/shield enrollment blocked (E8).
- **Not supported:** web login still mints JWTs (E2). No writer for `suspended`.
- **Changes required:** a status writer with audit; optionally an E2 check.
- **Unintended effects already in code:**
  - New connector control streams are rejected (E6), so a connector that restarts or reconnects **cannot come back**. S1 would silently degrade into partial S3 for any connector that reconnects.
  - Connector cert renewal is blocked (E6), which is moot today because renewal is never triggered.
- **Connectors:** existing streams continue (E7); reconnects fail (E6); never marked disconnected (E9).
- **Shields:** heartbeats flow via the connector. Shield renewal via the connector is blocked by E6 when the connector proxies `RenewCert`.
- **Clients:** existing devices keep fetching ACL/transport snapshots (E5) and tunnelling.
- **Relays:** unaffected (E11).
- **Cached snapshots:** unchanged; no invalidation.

#### S2 — Access suspension (block new tunnels; existing continue; connector online)

- **Supported now:** nothing specific.
- **Required:** the ACL must stop granting new sessions. Mechanically, the existing lever is the ACL snapshot: a policy change produces a snapshot with no allowed identities, pushed via `NotifyPolicyChange`.
- **Conflict with existing behaviour:** the connector cancels sessions for removed (spiffe, resource) pairs on snapshot update (`PolicyCache.update_and_revoked`; `SessionRegistry.cancel_all`). "Existing tunnels continue" **contradicts current connector behaviour**. An emptied ACL tears down existing sessions, which is S3's tunnel effect. Keeping existing sessions would need a new "no new sessions" flag in the ACL or connector.
- **Connectors:** must stay online, but E6 rejects reconnects if `workspaces.status` is used. S2 would therefore need a status or flag distinct from the one E6 checks.
- **Shields:** unaffected.
- **Clients:** continue polling (E5); new tunnels denied by the connector ACL.
- **Relays:** unaffected; the relay does no ACL.
- **Snapshots:** ACL must be recompiled and pushed; transport can remain.

#### S3 — Full isolation

- **Supported now:** E1, E4, E6, E8 give partial isolation once status is `suspended`.
- **Missing:**
  - E2 web login check.
  - E5 client RPC checks.
  - E7 teardown of existing streams (requires the `ConnectorRegistry`, which is process-local; see Q2).
  - E10: compilers returning empty snapshots, or connectors refusing sessions.
  - Terminating live tunnel sessions (ACL empty → connector cancels; or stream close).
  - Stopping shields (no controller→shield channel exists; shields follow their connector).
  - Blocking SCIM (E13).
- **Connectors:** stream closed; reconnect rejected (E6); remain "active" in the DB unless the watcher is fixed (E9).
- **Shields:** lose their connector; no direct controller control.
- **Clients:** RPCs must reject; sessions die when the connector cancels them or loses the ACL.
- **Relays:** will still pair a connector and client of that tenant if both are connected and not on the CRL. Relays have no workspace-status input.
- **Snapshots:** both caches must be invalidated or return empty.

#### S4 — Certificate-level suspension

- **Supported now:** per-entity revocation for connectors (`revoked_at` → workspace CRL) and client devices (`revoked_at` → workspace CRL). The shield has status only, with no CRL entry. The **Workspace CA itself cannot be revoked**: no CA-level revocation exists, and the Root/Intermediate CRL does not list Workspace CAs.
- **Missing:**
  - Bulk revocation.
  - Shield cryptographic revocation.
  - A re-enrollment flow on reactivation. New enrollment tokens are required, since certificates are CSR-based and tokens are single-use.
  - Handling of renewal overwriting `cert_serial` (older valid certs are not on the CRL; discovery §8).
  - Known status-resurrection defects for connectors and shields (discovery §15).
- **Connectors:** relays reject them via the workspace CRL (relays check the CRL). The controller-side interceptor does **not** check connector revocation; it relies on DB status (resurrection defect).
- **Shields:** no cryptographic path.
- **Clients:** CRL + ACL removal + `deviceGate` directive (REVOKED → wipe key).
- **Relays:** effective via CRL for connectors and clients.
- **Snapshots:** `NotifyPolicyChange` removes revoked devices from the ACL.
- **Reactivation:** full re-enrollment of every agent and device. The tenant admin must redistribute install commands.

### Compatibility with the existing architecture (not a ranking)

- Most of the existing enforcement points (E1, E4, E6, E8) already key off `workspaces.status='active'`. Setting `suspended` today yields **an inconsistent blend of S1 and partial S3**:
  - admin and CLI login blocked, but web login allowed;
  - reconnecting connectors locked out, but connected ones still active;
  - clients still served.
- Any chosen semantic must reconcile this blend. The pure form closest to what the checks already do is S1 plus reconnect lockout. The mechanism that most naturally implements "no new tunnels" is the ACL snapshot, which currently also kills existing tunnels.
- S4 aligns with the ADR-027 serial-CRL model but inherits its gaps: no shield CRL, and serial overwrite on renewal.

---

## Q4 — Tenant deletion semantics

### Current FK / cascade facts (existing)

- `DELETE FROM workspaces` cascades to: `users`, `workspace_members`, `invitations`, `workspace_ca_keys`, `remote_networks`, `connectors`, `shields`, `resources`, `client_devices`, `groups`, `access_rules`, `connector_logs`, `audit_logs`, `identity_connections` (tenant rows), `external_identities`, `device_*`, `resource_profile_bindings`, `scim_*`, `workspace_permissions`.
- It is **blocked** by `outbox_events.workspace_id … ON DELETE RESTRICT` (`033`) whenever any outbox row exists for the tenant, including `done` rows (no pruning found).
- `connector_relay_placement` cascades via `connectors`.
- `device_posture_reports.device_id` and `device_profile_evaluations.device_id` reference `client_devices` **without** cascade. Whether these block the chain when the workspace cascade reaches `client_devices` depends on cascade ordering. Both tables also cascade directly from `workspaces`. **UNCERTAIN** whether Postgres resolves this without error; it has not been tested.
- `connector_scan_results.connector_id` has no FK, so rows would be orphaned.
- Global references that do not cascade: `provider_audit_logs.target_id` (TEXT) may reference the workspace. Relay certificates are unaffected.
- Valkey keys referencing tenant entities (`refresh:<userID>`, enrollment JTIs) expire by TTL only.
- In-memory state (caches, registry, version counters) is not cleaned.

### Options

| | **D1 Hard delete** | **D2 Soft delete, retain** | **D3 Soft delete + scheduled purge** | **D4 Crypto/data destruction, minimal metadata** |
|---|---|---|---|---|
| **Database** | Requires removing or relaxing `outbox_events` RESTRICT (or purging outbox rows first); cascade removes everything; posture FK ordering must be verified | Uses existing `status='deleted'`; all rows retained; every query and uniqueness constraint must tolerate deleted tenants. The `slug`/`trust_domain` UNIQUE constraints keep names reserved | D2, plus a purge job (no scheduler framework exists beyond ad-hoc loops like posture retention, `internal/posture/retention.go`) | Delete or overwrite sensitive rows and keys; keep a tombstone row (id, slug, timestamps, deletion actor) |
| **Audit** | `audit_logs` rows are **cascade-deleted**, losing the tenant's own trail. The provider audit entry for the deletion survives (`provider_audit_logs` is global) | Retained | Retained until purge; retention window is a policy decision | Tenant audit must be exported or retained separately, or it is destroyed with the data |
| **PKI** | `workspace_ca_keys` deleted, so the Workspace CA key is gone and no further CRLs can be signed for it. Outstanding leaf certs remain cryptographically valid until expiry (≤7d; Workspace CA 2y) but chain to a CA the controller no longer recognises (E6 lookup fails). Relays still trust anything chaining to the Intermediate and **cannot fetch a CRL** for a deleted workspace. Behaviour of `WorkspaceCrlManager` on 404 is fail-closed per discovery §8 | CA key retained; CRLs still servable; certs expire naturally | Same as D2 until purge, then D1 | Destroying the CA key is effectively crypto-shredding the tenant's issuance authority. It does not revoke existing certs (same relay caveat as D1) |
| **Connectors / shields / devices** | Rows vanish; live streams keep running until reconnect (registry is in memory; interceptor then fails); agents keep retrying forever | Agents are rejected on reconnect (E6); must be uninstalled out-of-band | Same | Same as D1 |
| **Outbox** | RESTRICT blocks deletion; pending device-trust events would be lost | Events keep processing unless handlers check tenant status (they do not) | Purge must drain or discard outbox first | Same as D1 |
| **Compliance** | Satisfies erasure requests; loses tenant-side audit evidence | Retains personal data (emails, IdP subjects, device names, access logs in `connector_logs`), which may conflict with erasure obligations | Supports a retention window, then erasure | Supports erasure with minimal proof-of-deletion metadata; audit retention must be solved separately |
| **Restoration** | Impossible | Possible (flip status; agents need valid certs, since expired certs require re-enrollment) | Possible until purge | Impossible for data; tombstone only |

Additional existing fact relevant to all options: **there is no deletion API and no suspension API** (discovery §5). Whatever is chosen is new code.

---

## Q5 — Relay pool semantics

### Existing baseline

- **Controller side.** `relay.Store.BuildLabelledRelayList` (`controller/internal/relay/store.go:455-513`) filters global relays by `status='active'`, `capacity_label IN (high, medium)` and a public address, then fingerprints the list (`relayListVersion`, FNV-64a).
- **Delivery.**
  - `ConnectorRegistry.BroadcastRelayList(list)` sends one message to all connectors (`control_stream.go:274-289`).
  - Stream open sends the same list (`:414-425`).
  - `ConnectorRegistry.ClientsForWorkspace(workspaceID)` already exists (`:143`), so per-workspace delivery is mechanically available.
- **Proto.** `LabelledRelayList{relays[], version}`, where `LabelledRelayInfo{relay_id, relay_addr, spiffe_id, capacity label HIGH=0/MEDIUM=1}` (`proto/connector/v1/connector.proto:~169-178`). There is no priority, pool or region field.
- **Connector selector** (`connector/src/relay_selector.rs`):
  - Tier-1 (HIGH), then Tier-2 (MEDIUM), then backoff.
  - RTT probes.
  - Migrates when >15% **and** >10ms better, or when the active relay leaves the list.
  - Warm ranking persisted in `relay_ranking.json`.
  - Failover through ranked entries.
- **Placement.** `connector_relay_placement` (PK `connector_id`) is written from connector reports.

### Model analysis

| | **P1 Hard constraint** | **P2 Preferred + failover pool** | **P3 Ordered pool set** | **P4 Connector/network-level assignment** |
|---|---|---|---|---|
| **Database** | Pools, pool membership, tenant→pool (1:1) | Pools, membership, tenant→{primary, failover} | Pools, membership, tenant→pool with a priority ordinal (1:N) | Pools, membership, and assignment on `remote_networks` or `connectors` (with optional tenant default) |
| **Relay-list generation** | `BuildLabelledRelayList` filtered by the tenant's pool, so it becomes per-tenant rather than global. Version fingerprint becomes per-tenant | Either (a) send only the primary pool and switch to the failover pool server-side when the primary has no eligible relays, or (b) send both pools with a priority marker | Send all assigned pools with priority markers, or only the highest non-empty priority | Per network/connector list; highest cardinality |
| **`BroadcastRelayList`** | Replaced by per-workspace fan-out via `ClientsForWorkspace` | Same | Same | Per-connector send (`get(connectorID)`), or per-network grouping |
| **Proto** | No change needed (list content changes only) | (a) none; (b) new field on `LabelledRelayInfo` (priority/pool) | New priority field (additive field number) | None, unless priority is added |
| **Connector selector** | Unchanged. It already chooses from whatever list it gets | (a) unchanged; (b) must understand pool priority on top of HIGH/MEDIUM tiers | Must implement a priority → tier → RTT ordering; migration rules must not migrate to a lower-priority pool just for RTT | Unchanged if the list is pre-filtered |
| **Failover** | Only within the pool. An empty or exhausted pool leaves the connector in backoff (no relay path; direct path unaffected) | (a) controller decides, with latency = eviction interval (90s) + broadcast; (b) connector decides immediately on session failure | Connector-driven across priorities; fastest | As P1/P2, per network |
| **Migration** | Changing a tenant's pool changes the list. The active relay "left the list" triggers make-before-break migration (existing behaviour) | Returning from failover to primary: in (a) a server-side list change triggers migration; in (b) connector rules are needed to fail back | Fail-back rule needed (the existing >15%/>10ms rule is RTT-based, not priority-based) | Same mechanics, finer grain |
| **Region interaction** | A pool can be defined as "the region", so hard pools give residency enforcement for relay traffic (R2) | Failover pool may cross the residency boundary; only compatible with R2 if the failover pool is in-region | Same as P2, per priority | Allows different regions per network (e.g. a branch office in another country) |
| **Existing capacity labels** | Apply within the pool | Apply within each pool; interaction with "primary exhausted" (all LOW) must be defined. LOW relays are already excluded | Same | Same |

Cross-cutting fact: `connector_relay_placement` records **observed** placement, not assignment. Every model needs a separate assignment concept; placement stays as the observed truth.

---

## Q6 — Assignment granularity

### Existing structure

```text
workspaces 1─< remote_networks 1─< connectors 1─1? connector_relay_placement >─1 relays
                  (location enum: home/office/aws/gcp/azure/other — descriptive only)
```

- **Control stream is per connector.** The registry is keyed by connector id, and `ClientsForWorkspace` filters by tenant.
- **Relay list is identical for all connectors today**; the connector chooses.
- **Transport snapshot is compiled per workspace** (`transport/store.go` `GetWorkspaceConnectors`) and lists each connector's current relay.

| Granularity | Data model | Delivery | Operational consequences |
|---|---|---|---|
| **Tenant** | One assignment per workspace | Per-workspace list via `ClientsForWorkspace` | Simplest to operate and explain. All sites of a multi-site tenant use the same pools; a tenant with offices in two countries cannot pin each site. Pool changes affect every connector of the tenant at once (migration storm bounded by connector count). Aligns with the tenant-level suspension, quota and residency concepts. |
| **Remote network** | Assignment on `remote_networks` (optionally inheriting a tenant default) | Group connectors by `remote_network_id` (not a registry index today; derivable) | Matches the physical-site model: `remote_networks` is already the unit shields and resources attach to. Per-site region pinning is possible. More configuration surface; needs inheritance and override semantics. |
| **Connector** | Assignment on `connectors` | Per-connector send (`get(connectorID)` exists) | Maximum control; highest operational burden. Assignments must be re-established on re-enrollment (a new connector id). Placement and assignment tables become near-duplicates. Useful mainly for debugging or canarying. |

Common consequences:

- Any granularity finer than "global" turns the single broadcast into targeted sends. Relay-list versions become per-scope.
- The single-replica assumption in Q2 applies to all three.

---

## Q7 — Region semantics

### Existing

**No region concept exists anywhere**: schema, code, protos and config (discovery §7). The nearest artefacts are:

- `remote_networks.location` (a descriptive enum, not used in routing);
- `relays.observed_ip` / `address_scope` / `public_addr` (network facts, not geography).

The deployment is a single Postgres, a single Valkey and a single controller.

| Interpretation | What it constrains | Additional systems required (beyond today) |
|---|---|---|
| **R1 Network/latency region** | Relay candidate ordering only | Region attribute on relays (operator-set, or derived from IP geolocation); pool↔region mapping; optional region hint on connectors/networks. Connector RTT probing already optimises latency, so R1 mainly narrows or orders the candidate set. No change to data storage. |
| **R2 Data-residency boundary (relay traffic)** | Which relays may carry a tenant's traffic | Everything in R1, plus: **hard** filtering (P1-style) with no out-of-region failover; a residency attribute on tenants (and possibly networks); evidence and audit that the constraint held; a policy for when no in-region relay is available (fail closed means no relay path). Relay placement reports are connector-supplied and not validated against relay status (discovery §6), so enforcement would need controller-side validation of reported placement. The direct client→connector path is unaffected. |
| **R3 Full infrastructure residency** | Where all tenant data lives and is processed | Everything in R2, plus: regional controllers (or partitioned tenancy), regional Postgres and Valkey (or partitioning), regional log storage (`connector_logs`, `audit_logs`, posture reports), regional PKI decisions (Workspace CA keys are in the single Postgres; Root/Intermediate are global), cross-region routing of admin logins (tenant lookup by slug/email is global today), and a global directory mapping tenant → region. This is PENDING-12 Option B (active-active multi-region), rated **XL**, and depends on HA (Q2). |

---

## Q8 — Provider RBAC

### Existing actions in `provider.Authz` (`controller/internal/provider/authz.go`)

| Constant | Action string | `Can*` method | Call site | super-admin | relay-ops |
|---|---|---|---|---|---|
| `ActionRelayCreate` | `relay.create` | `CanCreateRelay` | **None** (unused) | ✅ | ✅ |
| `ActionRelayIssueToken` | `relay.issue_token` | `CanIssueProvisioningToken` | `POST /provider/relays` (`relay/admin_handler.go:62`) | ✅ | ✅ |
| `ActionRelayDelete` | `relay.delete` | `CanDeleteRelay` | `DELETE /provider/relays/{id}` (`:159`) | ✅ | ✅ |
| `ActionRelayRevoke` | `relay.revoke` | `CanRevokeRelay` | `POST /provider/relays/{id}/revoke` (`:221`) | ✅ | ✅ |
| `ActionProviderUserManage` | `provider_user.manage` | `CanManageProviderUser` | `GET /provider/users` (`provider/handler.go:50`) (read is gated by a manage action) | ✅ | ❌ |
| `ActionAuditView` | `audit.view` | `CanViewProviderAudit` | **None** (no route) | ✅ | ❌ |

`decide()` logic: `super-admin` → allow all; `relay-ops` → allow if the action has prefix `relay.`; otherwise `ErrForbidden`. There are no read actions for relays, because no relay read endpoint exists. `GET /provider/me` has no action check.

### Candidate matrix (Owner / Operator / Read-only / Support)

Legend: **E** = exists as an action today · **N** = new action · cells are **options to decide**, shown as the permission each role *could* have, with the main consideration. This is a template for the decision, not a recommendation.

| Action | Status | Owner | Operator | Read-only | Support | Consideration |
|---|---|---|---|---|---|---|
| `provider.read` | N | ✓ | ✓ | ✓ | ✓ | Baseline for any console access |
| `tenant.read` | N | ✓ | ✓ | ✓ | ✓ / scoped | Cross-tenant read is itself sensitive (customer list, user counts) |
| `tenant.create` | N | ✓ | ? | ✗ | ✗ | Today tenants self-create via JIT signup |
| `tenant.suspend` | N | ✓ | ? | ✗ | ✗ | Semantics undecided (Q3); may need step-up |
| `tenant.delete` | N | ✓ | ✗? | ✗ | ✗ | Irreversible under D1/D4; dual control? |
| `relay.read` | N | ✓ | ✓ | ✓ | ✓ | No endpoint exists yet |
| `relay.create` | E (unused) | ✓ | ✓ | ✗ | ✗ | Currently subsumed by `relay.issue_token` |
| `relay.issue_token` | E | ✓ | ✓ | ✗ | ✗ | ADR-021 SoD question: issue ≠ decommission |
| `relay.drain` | N | ✓ | ✓ | ✗ | ✗ | Capability doesn't exist yet |
| `relay.revoke` | E | ✓ | ✓? | ✗ | ✗ | Global blast radius |
| `relay.delete` | E | ✓ | ✓? | ✗ | ✗ | Terminal |
| `relay.config` | N | ✓ | ✓ | ✗ | ✗ | Capacity thresholds, pool membership, region |
| `pki.read` | N | ✓ | ✓ | ✓ | ? | CA metadata, cert inventory, CRL contents |
| `pki.revoke` | N | ✓ | ? | ✗ | ✗ | Tenant-scoped revocation crosses the tenant boundary |
| `audit.read` (= existing `audit.view`) | E (unrouted) | ✓ | ? | ✓? | ? | PENDING-07 proposes a distinct `auditor` role |
| `support.access` | N | ✓? | ✗? | ✗ | ✓ | See Q9 |
| `provider_user.manage` | E | ✓ | ✗ | ✗ | ✗ | Currently also gates listing users |

### Missing action categories

1. **Read actions** for every namespace. Today only one read (`provider_user.manage` for listing) is gated.
2. **Tenant namespace** (`tenant.*`), plus tenant-scoped sub-resources (`tenant.connector.*`, `tenant.device.*`, `tenant.user.*`).
3. **Pool/region namespace** (`pool.*`, `region.*`).
4. **PKI namespace** (`pki.*`, including CA rotation planning).
5. **Support/impersonation namespace** (`support.*`).
6. **Billing/quota/plan namespace** (PENDING-07 §5).
7. **Provider-user lifecycle split** (list vs create/disable/role change).
8. **Step-up markers:** a way for an action to declare that it requires recent strong authentication (Q10).
9. **Target scoping** (`Target` ignored today). Needed for partner orgs (Q1) and support grants (Q9).

Implementation facts for any matrix:

- `provider_users.role` CHECK allows only two values (`025`). New roles need a migration.
- The prefix rule assumes one role per user (single `role` column).
- PENDING-07 lists different candidate roles: `super-admin`, `relay-ops`, `token-management`, `billing`, `support`, `auditor`.

---

## Q9 — Support access

### Existing building blocks

- **`RequireProvider`** (`controller/internal/middleware/provider.go`) injects `provider.Actor{UserID, Email, Role}` and never calls `WorkspaceGuard`.
- **`TenantContext`** (`controller/internal/tenant/context.go`) holds `{TenantID, UserID, Role, Email}`.
  - It is set only by `middleware.AuthMiddleware` from a tenant JWT.
  - Resolvers call `tenant.MustGet`, and `@hasRole` compares `tc.Role`.
  - `UserID` is assumed to be a tenant `users.id`, and audit writes use it as `actor_user_id`.
- **`WorkspaceGuard`** requires `workspaces.status='active'`.
- **Audit:** `audit_logs.actor_user_id` has no FK (so a non-tenant id could be recorded) and `actor_email` is NOT NULL. `provider_audit_logs` is global.
- There is **no** impersonation, grant, consent or support-session code. ADR-021 lists break-glass impersonation as an open question. PENDING-07 places it at GA with "scoped, time-boxed, fully audited" guardrails.

| | **A Dedicated cross-tenant read APIs** | **B Read-only impersonation** | **C Time-boxed support session** | **D Tenant-approved grant + time-boxed** |
|---|---|---|---|---|
| **Identity** | Provider actor stays a provider actor; tenant identity never assumed | A provider actor is presented to tenant code as a tenant principal. The identity question is whether to fabricate a `TenantContext` or add a distinct principal type | Same as B, plus a session identity with expiry | Same as C, plus a grant entity linking tenant, provider user, scope and approval |
| **Tenant context** | None. Provider queries take an explicit `tenant_id` parameter, a new query class bypassing the `tenant_id`-from-JWT convention (discovery risk #1) | `TenantContext` must be synthesised. `TenantContext.UserID` has no valid tenant user → audit and resolver code that assumes `users.id` break or mis-attribute. `@hasRole` would need a role for the impersonator (read-only is not expressible today; all guarded fields are `ADMIN`) | As B, bounded in time | As C, bounded by grant scope |
| **Authorization** | New provider actions (`tenant.read`, …) in `decide()`; tenant GraphQL untouched | Read-only enforcement is not supported by `@hasRole` (no read/write distinction; only `ADMIN` guards). It would need a mutation block at the router or directive level | As B plus expiry checks | As C plus grant scope checks; tenant admins need an approval surface (new tenant-side API) |
| **Session** | Provider JWT (15 min, no revocation) | Needs a derived token or context carrying both identities; the tenant JWT format has no "acting as" claim | Needs an explicit session store (Valkey or DB) with expiry and revocation | Grant plus session store; grant revocation by the tenant must terminate the session |
| **Audit** | Provider audit only (unless tenant visibility is required: Q20 in discovery) | Must write **both** provider audit (who entered) and tenant audit (what was seen/done), with the provider identity. `audit_logs` has no field distinguishing an impersonated actor | Same as B, plus session start/stop events | Same as C, plus grant request/approve/revoke events visible to the tenant |
| **`WorkspaceGuard`** | Not involved; provider reads must decide whether suspended tenants are readable (probably yes for support) | Blocks access to suspended tenants unless bypassed, and bypasses inside tenant authz are where ADR-021 says isolation bugs live (its Option B rejection rationale) | Same | Same |
| **ADR-021 invariant** ("provider authz must never depend on tenant membership, and tenant authz must never grant access to provider infrastructure") | Preserved | Strained: provider enters tenant authz | Strained | Strained, but gated by tenant consent |

---

## Q10 — Provider authentication / MFA

### 1. What the current authentication proves (existing)

Flow: `GET /provider/auth/initiate` → Google PKCE → `/provider/auth/callback` (`controller/internal/auth/provider_auth.go`) → `VerifyIDToken` → `provider.Store.GetByEmail` → `IssueProviderToken`.

It proves that:

- The caller completed a Google OAuth authorization-code + PKCE flow with the platform `GoogleClientID`.
- Google issued a valid ID token whose `email` is verified (`email_verified` enforced, `internal/auth/idtoken.go:119`).
- That email matches an **active** (`disabled_at IS NULL`) `provider_users` row at login **and on every request** (`RequireProvider` re-reads the row).
- The resulting token is ≤15 minutes old, HS256-signed with `JWT_SECRET`, `aud=["provider"]` (`internal/provider/session.go:52-73`).

### 2. What it does not prove

- **MFA.** Google's adapter notes that Google does not populate `amr`/`acr` (`internal/auth/google_provider.go:61`). The provider path reads neither.
- **Corporate account.** There is no `hd` (hosted domain) check, so a consumer Google account with a matching verified email passes.
- **Stable identity.** Matching is by email, not Google `sub`, so an email reassigned at the IdP maps to the same provider user.
- **Recency of authentication** for dangerous operations: no `auth_time` or `max_age` handling on the provider path.
- **Device or network posture:** no network allowlist in code (PENDING-07 requires network isolation).
- **Token non-reuse / revocation:** no `jti` tracking and no logout.

### Existing `amr`/`acr` support

The generic tenant OIDC adapter parses `acr`, `amr` and `auth_time` into `providers.AuthenticationContext` (`internal/auth/providers/oidc.go:214-216,265-266`; struct at `providers/provider.go:32-34`). **No code consumes these fields** (grep finds no reader). The break-glass MFA hook `RequireBreakGlassMFA` (`graph/resolvers/permission_helpers.go:14-22`) is a documented no-op, tenant-side only.

### 3. Requirements to enforce MFA at the provider layer (options, not a choice)

- **IdP-asserted:** use a provider IdP that emits `amr`/`acr` (Google consumer does not; Google Workspace/Cloud Identity behaviour is **UNCERTAIN** from the repo) and reject tokens lacking the required values. This needs a provider OIDC path that can target a non-Google issuer (today it is hard-coded to `accounts.google.com`) and a check in the provider callback.
- **Organisational policy only:** enforce MFA in Google Workspace and pin `hd` to the corporate domain. Zecurity then trusts the domain policy and can't verify MFA per login.
- **First-party:** TOTP/WebAuthn owned by Zecurity for provider users (PENDING-06 Option B). This brings a new authenticator store and a recovery process.

### 4. Requirements for step-up before dangerous operations

- A notion of **authentication time/strength inside the provider session**: claim(s) such as `auth_time`/`amr` carried into the provider JWT, or server-side session attributes.
- A re-authentication flow (OIDC `prompt=login`/`max_age`, or a WebAuthn assertion) that upgrades the session.
- Per-action metadata in `decide()`, or a wrapper, to require recent step-up (missing category 8 in Q8).
- Audit of step-up events.

### 5. Separate signing key / issuer for provider tokens?

Existing: tenant access JWTs, provider JWTs, connector/shield enrollment JWTs and relay provisioning JWTs are **all HS256 with the same `JWT_SECRET`** and the same issuer (`appmeta.ControllerIssuer`). They are separated only by claim checks: `aud=provider`, `aud=relay-provisioning`, and the presence of `tenant_id`. Connector and shield enrollment tokens have no `aud`.

| Choice | Consequences |
|---|---|
| Keep shared key | No new secret management. Compromising `JWT_SECRET` forges any token type, including provider super-admin. Isolation depends on every verifier checking `aud` correctly; tenant middleware rejects provider tokens only because `tenant_id` is missing. |
| Separate key (HS256) | Contains the blast radius of a leak; one more secret to distribute and rotate; provider verification code already separate (`provider.VerifyProviderToken`), so the change is localised. |
| Asymmetric (e.g. ES256) with separate issuer | Verifiers can't forge; enables external verification (e.g. a separately deployed console, PENDING-07b). Requires key management and rotation (no rotation mechanism exists for any key today). |

### 6. Requirements for session revocation

- **Existing:** provider tokens can't be revoked. Disabling a user takes effect immediately because `RequireProvider` re-reads `provider_users` per request, but a role downgrade also takes effect only through that re-read. There is no refresh token.
- **Requirements:**
  - a per-user generation counter (mirroring tenant `identity_generation`, `internal/identity/revocation.go`) stamped into the token and compared in `RequireProvider`; or
  - a `jti` denylist in Valkey; or
  - server-side sessions.

  Each also needs a logout endpoint, "revoke all sessions of user X", and audit events. If refresh tokens are introduced for longer sessions, the ADR-006 rotation pattern exists to mirror.

---

## Q11 — PKI boundary

### Existing

- **Hierarchy.** Root (`ca_root`, 10y) → **one** Intermediate (`ca_intermediate`, 5y, MaxPathLen=1).
- **What the Intermediate signs:**
  - relay leaves (`internal/pki/relay.go:34-110`);
  - the controller gRPC leaf (`pki/controller.go`);
  - Workspace CAs (`pki/workspace.go:18-83`);
  - the relay CRL (`pki/relay_crl.go`).
- **Trust anchors in use:**
  - Relays trust only the Intermediate for peers (`relay/src/tls.rs:21-77`).
  - The controller verifies relays against the Intermediate (`internal/connector/spiffe.go` `verifyRelayCertificate`).
  - Bootstrap pins the Intermediate fingerprint (`RELAY_CA_FINGERPRINT`; enrollment-token `ca_fingerprint`).
  - `SELECT … FROM ca_intermediate LIMIT 1` is hard-coded.
- No CA rotation of any kind.

### Proposed topology under evaluation

```text
Root
 └── Platform Intermediate
      ├── Controller leaf
      ├── Workspace CAs (…)            ← placement of Workspace CAs is itself a design choice
      └── Provider/Relay Intermediate
           ├── Relay leaves
           └── Relay CRL
```

| Dimension | Current (single Intermediate) | With a Provider/Relay Intermediate |
|---|---|---|
| **Blast radius** | Compromise of the Intermediate key lets an attacker mint relays, a controller identity **and Workspace CAs for any tenant**. The key is held decrypted in controller memory for the process lifetime | A relay-intermediate compromise is confined to relay identities, provided peers verify relays against the relay intermediate specifically, not just "chains to Root" |
| **Issuance authority** | Controller process signs everything | Allows the relay signer to live elsewhere (a separate service or console process, HSM) with a different operational owner (provider ops) |
| **Revocation** | Relay CRL signed by the Intermediate; the Intermediate itself cannot be revoked by anything (no Root CRL) | Relay CRL signed by the relay intermediate; the relay intermediate becomes revocable via a Root- or Platform-signed CRL, **which does not exist today** |
| **CA rotation** | Rotating the Intermediate re-roots every tenant CA, every relay and the controller at once | Relay CA rotation is independent of tenant PKI. Still requires rotation machinery that doesn't exist (multi-intermediate support, overlap windows, pin updates) |
| **Provider isolation** | Provider operations (relay issuance) share a key with tenant CA issuance | Supports ADR-021's separation principle at the cryptographic layer; natural home for partner relay CAs under Model B (Q1) |
| **Operational complexity** | One key, one pin | Another key to generate, encrypt (new `PKI_MASTER_SECRET` HKDF context), load at startup, and cover in `auditCAConstraints` (`internal/pki/audit.go`, which today checks only Root and Intermediate). Another fingerprint pin to distribute |
| **Migration impact** | n/a | **Verifier changes:** relays (`tls.rs` trust store), connectors and clients (verify relay chain, relay CRL issuer check against the new CA), controller (`verifyRelayCertificate`), bootstrap pins (`RELAY_CA_FINGERPRINT`, `/ca.crt` content). **Existing relay certs** (30d TTL, no renewal) must be re-provisioned, and relay renewal doesn't exist, so migration means new relays or a new renewal path. **Path length:** Root MaxPathLen=2 and Intermediate MaxPathLen=1. A relay intermediate under the Platform Intermediate needs Platform MaxPathLen≥1 (satisfied), with the relay intermediate at MaxPathLen=0. Putting it directly under Root as a sibling is also possible with the current Root constraint. **Transition:** a period where both chains are accepted |

---

## Q12 — Dashboard API architecture

### Existing

- **Tenant GraphQL.**
  - One gqlgen schema set (`controller/graph/*.graphqls`, `graph/gqlgen.yml`).
  - Auth via route-level `AuthMiddleware` + `WorkspaceGuard`, then the `@hasRole` directive against `TenantContext` (`graph/resolvers/directives.go:23-34`).
  - Deny-by-default test `graph/schema_authz_test.go` (which misses two schema files).
  - Public-field routing `publicRootFields` (`cmd/server/main.go:713-717`).
  - Introspection only in development.
- **Provider REST.**
  - `/provider/*` on the same HTTP listener.
  - Hand-written handlers (`internal/relay/admin_handler.go`, `internal/provider/handler.go`), `RequireProvider`, `Authz.Can*` per handler.
  - JSON request/response; no OpenAPI spec found.
- **Frontend.**
  - `admin/` uses Apollo Client 4 with codegen over the tenant schemas (`admin/codegen.yml`). No REST client layer for `/provider/*`.
  - PENDING-07b direction: CLI first, then a **separate** app.

| | **API-A Extend `/provider/*` REST** | **API-B Dedicated provider GraphQL** | **API-C Hybrid** |
|---|---|---|---|
| **Auth reuse** | Direct: `RequireProvider` + `Authz.Can*` per handler (existing pattern) | Needs a provider directive (e.g. an `@providerAction`) reading `provider.Actor`, and a **separate executable schema** so tenant `@hasRole` / `TenantContext` never mixes with provider actors. Needs its own deny-by-default test | Both |
| **Isolation from tenant API** | Strong: different handlers, different middleware | Strong **only** if it is a separate gqlgen server/endpoint; sharing the tenant schema would violate the ADR-021 invariant | As its components |
| **Dashboard query fit** | Cross-entity views (tenant detail with connectors, devices, certs) need multiple calls or bespoke aggregate endpoints | Natural for nested, cross-entity reads; field-level authorisation per action | Queries in GraphQL, mutations in REST |
| **Mutation semantics** | Explicit verbs (`/revoke`, `/drain`); easy per-endpoint audit and step-up | Mutations in GraphQL; audit and step-up must be wired per resolver | Operational mutations stay REST (existing relay routes unchanged) |
| **CLI (alpha)** | Easiest (curl/HTTP client); matches PENDING-07b alpha | Workable but heavier | CLI uses REST for ops; reads via GraphQL or REST |
| **Frontend** | Separate app would need its own REST client/types (no generator in repo) | Apollo + codegen pattern already in `admin/` can be reused in a separate app | Two client stacks |
| **Tooling** | No schema/codegen; risk of drift | gqlgen (now a `go.mod` tool, commit `9cd2351`) and codegen workflow exist | Both toolchains |
| **Security surface** | Small, explicit | Introspection, query depth/complexity (ADR-009 GraphQL DoS hardening applies to tenant GraphQL, **UNCERTAIN** how it's configured for a second server) | Both |
| **Existing code impact** | Additive | New server wiring in `main.go`; new schema directory | Additive |

---

## Q13 — Read model vs direct DB queries

Assessment is against the current schema and indexes (discovery §13). "Direct" means SQL on existing tables at query time.

| View | Data needed | Direct query feasible today? | Pressure toward a read model |
|---|---|---|---|
| **Provider Overview** | Tenant counts by status; relay counts by status/label; connector online counts; cert expiries | Yes: `workspaces`, `relays`, `connectors` are small aggregates; `idx_workspaces_active` and `idx_relays_status` exist. `connectors` has only `tenant_id`-leading indexes, so a global status count is a sequential scan | Low at current scale. Rises with connector count. Accuracy is limited by source defects (relay status flapping, connector status resurrection) regardless of read model |
| **Relay Fleet** | `relays` row + placements count + capacity | Yes: `relays` plus `connector_relay_placement` (`idx_crp_relay`) | **Freshness, not query cost:** `connection_count` and `last_heartbeat_at` are DB-written only every ~5 min (Valkey throttle). A fleet view wanting live numbers must read Valkey (`relay:heartbeat:*`) or change the write cadence. Uptime and `registered_connectors` are not stored at all |
| **Tenant List** | Per-tenant counts (users, connectors, shields, devices, resources) | Yes, with per-table `GROUP BY tenant_id`; indexes are tenant-leading, which suits this | Becomes costly as tenants × tables grow; a materialised per-tenant summary (or incremental counters) is the typical relief |
| **Tenant Detail** | One tenant's entities | Yes: existing tenant-scoped queries (the same SQL resolvers use) with an explicit tenant id | Low |
| **Certificate Inventory** | Serial, expiry, status across `connectors`, `shields`, `client_devices`, `relays`, `relay_certificates`, CA tables | Yes via `UNION` across tables; **no `cert_not_after` indexes** exist (expiry sort/filter = scans) | Moderate. Also a **data gap**, not just a query gap: no per-cert history for connectors/clients (serial overwritten on renewal), so "all certs still valid" can't be answered from any query |
| **Audit Search** | Provider audit; optionally tenant audit across tenants | `provider_audit_logs`: indexes on `created_at DESC` and `(target_type, target_id)`; no actor index; `details` JSONB has no GIN index. `audit_logs` cross-tenant search: indexes are `tenant_id`-leading, so a global search by actor/action is a scan | Moderate to high for cross-tenant search and free-text details. PENDING-11 points toward export or SIEM rather than in-DB search |
| **Active sessions** | Live tunnel sessions | **No source exists** (connector in-memory `SessionRegistry` only) | Requires new telemetry (Q14) before any read model |
| **Future metrics** | Time series (bandwidth, sessions, CPU) | Not in Postgres | A time-series store, not a relational read model |

Summary of existing constraints: relational aggregates are feasible directly at current scale. The blockers are **freshness** (relay heartbeat throttle), **missing data** (sessions, bytes, cert history) and **source correctness**, not query shape.

---

## Q14 — Metrics architecture

### Current compatibility

| Mechanism | Existing today |
|---|---|
| **Heartbeat telemetry** | Relay `HeartbeatRequest` carries `connection_count`, `max_connections`, `registered_connectors`, `uptime_seconds`, `version` (proto `relay.proto:43-51`). Connector `ConnectorHealthReport` every 15s carries version, IPs, `acl_version`, relay placement. Shield health via connector batch. Persisted selectively (relay DB write throttled to 5 min) |
| **Prometheus** | Controller private registry with reconcile metrics + Go/process collectors on `127.0.0.1:9102` (`internal/metrics/metrics.go`). **No Rust component exposes metrics** |
| **Controller event aggregation** | Per-tunnel allow/deny/error events already flow controller-ward as `ConnectorLog` (control-stream field 12) into `connector_logs` (`migrations/014`, `021`), with no retention job and no bytes/duration columns |
| **Dedicated pipeline** | None (no OTel, no collector, no TSDB) |

### Per-signal analysis

| Signal | Where the truth originates | M1 Heartbeat | M2 Prometheus | M3 Controller event aggregation | M4 Dedicated pipeline |
|---|---|---|---|---|---|
| Relay sessions | Relay (`ACTIVE_STREAMS`) | **Partially exists** (`connection_count`) | Needs relay `/metrics` + scrape reachability to relays (relays are remote, provider-operated) | n/a (relay doesn't emit events) | Relay agent/exporter |
| Relay bandwidth | Relay pipe loop (`session.rs pipe_streams`) | New fields; coarse (30s) | Natural (counters) | n/a | Natural |
| Connector sessions | Connector `SessionRegistry` | New field in health report | Connector exporter; connectors are in customer networks, so scrape reachability is a problem | Derivable from open/close events (close events don't exist today) | Push-based agent |
| Tenant tunnel sessions | Sum over connectors | Aggregate of connector heartbeats | Aggregation across tenant label; per-tenant metric isolation question (PENDING-10) | From events | Natural |
| Bytes transferred | Connector `copy_bidirectional` (counts **discarded** today, `device_tunnel.rs`) | New counters | Natural | Needs a close event with byte counts | Natural |
| CPU / memory | Each process | New fields | Natural (process collectors) | n/a | Natural |
| Tunnel establishment success rate | Connector allow/deny/error | Counters in heartbeat | Natural | **Closest to existing**: `connector_logs.action` already records allow/deny/error per attempt | Natural |

### Tradeoffs

| | Strengths | Weaknesses | Compatibility with current code |
|---|---|---|---|
| **M1 Heartbeat** | Works through existing authenticated channels (mTLS relay heartbeat, connector control stream), with no inbound reachability to customer networks or relays. Already carries relay capacity | Coarse granularity (15–30s); grows proto messages; controller becomes a metrics sink (Postgres write load; already throttled for relays); no history unless stored | **High.** Additive proto fields; handlers exist |
| **M2 Prometheus** | Standard; PENDING-10 direction; counters/histograms; existing controller registry | Pull model needs network reachability to relays and connectors (connectors are behind NAT in customer networks); per-tenant isolation of metric labels; cardinality | **Medium.** Controller has it; Rust components don't |
| **M3 Controller event aggregation** | Reuses `ConnectorLog` path; exact per-event data; tenant-scoped naturally | Event volume on the control stream and Postgres (`connector_logs` unbounded, no retention); bounded mailbox (128) and backpressure issue noted in discovery (connector `emit_access_log` can block tunnel handlers when disconnected); not suited to gauges (CPU/mem) | **Medium-high** for success/failure; low for gauges |
| **M4 Dedicated pipeline** (e.g. OTel push to a collector/TSDB) | Decouples telemetry from control plane; push works from NAT; scales; PENDING-10 mentions OTel | New infrastructure to operate; agent auth to the collector (a new trust relationship); data residency (Q7 R3) implications | **Low.** Nothing exists |

---

## Q15 — Final dependency graph

```text
Provider Dashboard
       │
       ├── Provider Identity ............ PARTIALLY EXISTS
       │     ├── provider_users + Google PKCE + aud=provider JWT ...... EXISTS
       │     ├── sub/hd binding, MFA, step-up, session revocation ..... NEW
       │     └── separate signing key ................................ NEW (decision)
       │
       ├── Provider RBAC ................ PARTIALLY EXISTS
       │     ├── decide() chokepoint, Target plumbing ................. EXISTS
       │     ├── role set / action taxonomy / read actions ............ BLOCKED BY DECISION (Q8)
       │     └── partner scoping ...................................... BLOCKED BY DECISION (Q1)
       │
       ├── Tenant Scope ................. PARTIALLY EXISTS
       │     ├── workspaces.status incl. 'suspended'/'deleted' values . EXISTS
       │     ├── provider tenant read APIs ............................ NEW
       │     ├── suspend semantics & propagation ...................... BLOCKED BY DECISION (Q3)
       │     ├── deletion semantics ................................... BLOCKED BY DECISION (Q4)
       │     └── support access ....................................... BLOCKED BY DECISION (Q9)
       │
       ├── Relay Fleet .................. PARTIALLY EXISTS
       │     ├── create/token/revoke/delete ........................... EXISTS
       │     ├── list/detail APIs ..................................... NEW
       │     ├── accurate liveness (heartbeat/expiry alignment) ....... PREREQUISITE FIX
       │     ├── renewal / token re-issue ............................. NEW (decision: Q6 discovery)
       │     └── drain ................................................ NEW (depends on command channel decision)
       │
       ├── Relay Pools .................. NEW
       │     ├── pool semantics ........................................ BLOCKED BY DECISION (Q5)
       │     ├── granularity ........................................... BLOCKED BY DECISION (Q6)
       │     └── region meaning ........................................ BLOCKED BY DECISION (Q7)
       │
       ├── PKI .......................... PARTIALLY EXISTS
       │     ├── hierarchy, CRLs, relay cert history .................. EXISTS
       │     ├── connector renewal trigger, controller cert reload .... PREREQUISITE FIX
       │     ├── revocation correctness (connector/shield) ............ PREREQUISITE FIX
       │     ├── cert inventory / expiry view ......................... NEW
       │     └── relay intermediate, CA rotation ...................... BLOCKED BY DECISION (Q11)
       │
       ├── Audit ........................ PARTIALLY EXISTS
       │     ├── provider_audit_logs write path ....................... EXISTS
       │     ├── query API ............................................ NEW
       │     └── immutability / retention / tenant-visibility ......... BLOCKED BY DECISION
       │
       ├── Telemetry .................... PARTIALLY EXISTS (relay capacity, connector status only)
       │     └── source of truth per signal ........................... BLOCKED BY DECISION (Q14)
       │
       └── (cross-cutting) Controller HA . NEW; whether prerequisite is BLOCKED BY DECISION (Q2)
```

### HARD BLOCKERS (must be decided before implementation)

1. Single-provider vs partner model, and whether `provider_org` is introduced now (Q1).
2. Provider role set and action matrix (Q8).
3. Tenant suspension semantics (Q3). The current checks already produce an inconsistent partial state if `suspended` is ever written.
4. Tenant deletion semantics, including audit retention and outbox RESTRICT handling (Q4).
5. Relay pool semantics, assignment granularity and region meaning (Q5–Q7), if pools/regions are in the first dashboard scope.
6. Provider API style (Q12) and console packaging (PENDING-07b).
7. Whether mutating dashboard operations must work multi-replica from day one (Q2).
8. Provider authentication strength requirements: MFA source, `hd`/`sub` binding, signing key separation (Q10).

### IMPLEMENTATION PREREQUISITES (backend defects/gaps to fix before dashboard work)

Each of these would otherwise make the dashboard display false state (details in discovery §4, §8, §15):

1. Relay liveness: `EvictExpiredRelays` reads DB `last_heartbeat_at` while DB writes are throttled to 5 min (`internal/relay/store.go:408-431` vs `internal/relay/heartbeat.go:178-209`).
2. Connector revocation resurrection: unguarded status writes at stream open/close (`internal/connector/control_stream.go:330-347,363-381`).
3. Shield revocation resurrection: `UpdateShieldHealth` overwrites status (`internal/shield/heartbeat.go:34-51`).
4. Connector certificate renewal trigger (no `ReEnroll` sent; `CONNECTOR_RENEWAL_WINDOW` unused).
5. Controller gRPC server certificate: 7-day TTL with no reload (likely; `cmd/server/main.go:~457-473`).
6. Relay SAN allowlist enforcement (stored `dns_allowlist`/`ip_allowlist` ignored; `internal/relay/provision.go:102-106,170`).
7. Relay read endpoints (list/detail). No provider read surface exists.
8. Disconnect watcher does not notify the transport plane (`cmd/server/main.go:~565`).

### CAN BE DEFERRED (don't block an initial dashboard)

- Partner/reseller delegated administration (PENDING-07 places it at GA), unless the Q1 decision pulls it forward.
- Billing, quotas, metering (PENDING-07 §5; bytes/sessions not measured).
- Full observability pipeline (M2/M4); an initial fleet view can use existing heartbeat data.
- Relay rollout/version targeting (no relay release workflow exists).
- Tenant migration between pools/regions/controllers.
- Region R3 (full infrastructure residency) and multi-region controllers.
- Separate relay intermediate CA and CA rotation (unless Q11 is decided as a prerequisite).
- SIEM export (PENDING-11).
- Break-glass impersonation (Q9 options B–D), since option A (dedicated read APIs) needs no impersonation.
- OCSP.

---

## Decisions required from product/architecture

1. Is the provider model single-provider (Model A) for the foreseeable roadmap, or must partner/MSP scoping (Model B) be supported in Alpha or Beta?
2. Should `provider_org` (or an owner column on tenants/relays) be introduced now, or deferred until partners exist?
3. Can tenants ever bring their own relays?
4. Is controller HA required before the dashboard ships, or can dashboard mutations be documented as single-replica until PENDING-12 lands?
5. What must "Suspend Tenant" do (S1/S2/S3/S4 or a combination), and should existing tunnels survive it?
6. What must "Delete Tenant" do (D1/D2/D3/D4), what is the retention period, and must tenant audit logs survive tenant deletion?
7. Are relay pools a hard constraint, a preference with failover, or an ordered set (P1/P2/P3)?
8. At what granularity is pool assignment made: tenant, remote network or connector?
9. Does "region" mean latency placement (R1), relay-traffic residency (R2), or full infrastructure residency (R3)?
10. What is the provider role set, and the permission matrix per action, including separation of duties (e.g. token issuance vs decommission)?
11. Which actions require step-up authentication or dual control?
12. Is support access in scope, and if so which model (A/B/C/D)? Must tenants consent?
13. When a provider acts on tenant data, must the event appear in the tenant's audit log, the provider log, or both?
14. What is the provider MFA requirement, and must it be verified by Zecurity (IdP `amr`/`acr` or first-party) or delegated to corporate IdP policy?
15. Must provider users be bound to a corporate domain (`hd`) and to IdP subject rather than email?
16. Should provider tokens use a separate signing key/issuer (and symmetric vs asymmetric)?
17. Is a separate Provider/Relay intermediate CA required, and is CA rotation a prerequisite for exposing PKI actions?
18. Must relays renew certificates in-band, or is re-provisioning with a new token acceptable?
19. Should relays receive commands (drain, shutdown) via heartbeat-response directives or a new push channel?
20. Provider API style: REST, provider GraphQL, or hybrid?
21. Console packaging: CLI-first alpha, separate app, or shared app (PENDING-07b)?
22. What audit immutability level and retention are required (convention, DB-enforced append-only, hash chain, WORM/SIEM)?
23. Which telemetry signals are required for the first dashboard release, and which source of truth (M1–M4) is adopted per signal?
24. Is cross-tenant audit search a dashboard feature, or is audit consumed via export/SIEM?

---

## Decisions already supported by the codebase

These are constrained by accepted ADRs or implemented code. Revisiting them would mean reversing existing work.

1. **Provider identity is a separate tier, not tenant `users`.** ADR-021 accepted and implemented: `provider_users`, `aud=provider` JWT, `RequireProvider` without `WorkspaceGuard`. The ADR's invariant forbids provider authz depending on tenant membership.
2. **All provider authorization goes through one `decide(actor, action, target)` chokepoint** with dotted action namespaces and a `Target` already plumbed (`internal/provider/authz.go`).
3. **Provider operations live under `/provider/*`** behind `RequireProvider`. Existing relay operations are REST there.
4. **Relays are provider-owned, global platform infrastructure.** No `tenant_id`; SPIFFE `spiffe://zecurity.in/relay/<uuid>`; signed by the platform Intermediate; provisioning only via provider-minted one-time tokens (ADR-020 implemented).
5. **Relay selection model: controller filters and labels, connector chooses** (ADR-016 implemented). Any pool/region design shapes the **list** the connector receives rather than assigning relays directly; the connector selector and make-before-break migration already react to list changes.
6. **One active relay per connector** (`connector_relay_placement` PK `connector_id`), and placement is connector-reported (observed), not controller-assigned.
7. **Relay changes propagate to clients through the transport plane** (`NotifyTopologyChange` / `TransportSnapshot`, ADR-017, Sprint 13 complete), not through the ACL plane.
8. **Revocation uses serial-based CRLs, not OCSP** (ADR-027). Relay revocation has per-certificate history (`relay_certificates`), and relay deletion is soft by design (no cascade from `relay_certificates`).
9. **Tenant equals workspace** everywhere, with no separate organisation/account entity. Tenant-scoped PKI (one Workspace CA and trust domain per workspace) is fixed in schema and code.
10. **Workspace lifecycle states already defined in schema:** `provisioning / active / suspended / deleted`. Suspension/deletion designs can use these values without a new column, but must reconcile the existing enforcement points listed in Q3.
11. **Provider audit is a separate, global table** (`provider_audit_logs`) written in-transaction for destructive relay operations. The in-tx audit pattern is established.
12. **Leaf private keys are generated on the agent** (CSR-based issuance for connector, shield, client and relay). A dashboard never handles agent private keys.

Product direction (non-binding, `PENDING-*`), noted for context:

- CLI-first alpha, separate network-locked console for Beta/GA (PENDING-07/07b).
- Mandatory MFA and network isolation for the console (PENDING-07).
- Prometheus + OTel as the observability standard (PENDING-10).
- First-party audit with export, separate from ops telemetry (PENDING-11).
- Multi-replica single-region HA as the production target (PENDING-12).

---

## Decision record — 2026-09-23

Decisions made by the product/architecture owner after reviewing this analysis. These **supersede** the open framing of the matching questions above. Nothing has been implemented yet.

### Decided

| # | Topic | Decision | Ref |
|---|---|---|---|
| D-01 | Provider model | **Single provider, flat.** No `provider_org` for now; keep the `decide()` / `Target` seams for later partner scoping. | Q1 |
| D-02 | Controller HA | **Out of scope.** Zecurity runs a single controller instance. Dashboard reads **and** mutations may rely on process-local state and the live-stream registry. Multi-replica HA is a separate later initiative (PENDING-12) and is **not** a dashboard prerequisite. | Q2 |
| D-03 | Relay pools / regions | **Not in the first release.** Global relay fleet only. Q5–Q7 are deferred. | Q5–Q7 |
| D-04 | Provider API style | **REST under `/provider/*`** for both dashboard reads and operational mutations. Provider domain/query services stay independent of the HTTP layer so a provider-specific GraphQL read API can be added later. No second GraphQL stack in the first release. | Q12 |
| D-05 | Provider RBAC | **Keep `super-admin` / `relay-ops`** and add read actions (e.g. `relay.read`, `audit.read`, `tenant.read`) to `decide()`. No role migration. | Q8 |
| D-06 | Tenant read access | **super-admin only** for all `tenant.*` actions (read, suspend, resume, delete). This follows from the existing prefix rule with no special case. | Q8 |
| D-07 | Suspend Tenant | **Immediate full tenant access isolation.** Active tunnels and sessions are terminated immediately. Blocks tenant login, configuration mutations, new sessions and enrollment. Tenant data and PKI are retained. Provider operators keep read, resume and delete. | Q3 |
| D-08 | Suspended agents | **Connectors and shields stay connected** and receive a deny-all ACL (the connector cancels sessions and refuses new ones). Suspended tenants' agents must be exempt from the SPIFFE active-workspace check so reconnects and renewals keep working. Resume = push the normal ACL. | Q3 |
| D-09 | Delete Tenant | **D4: crypto/data destruction** with a minimal tombstone (id, slug, actor, timestamps). Not restorable after destruction. | Q4 |
| D-10 | Deletion timing | **Only suspended tenants can be deleted.** Destruction runs after a **30-day grace period**, during which deletion can be cancelled. | Q4 |
| D-11 | Tenant audit on deletion | **Export, then destroy.** Tenant `audit_logs` are exported before destruction and destroyed with the tenant. | Q4 |
| D-12 | Audit immutability | **DB-enforced append-only** for the application role on both audit tables: insert and query only, no update or delete. Destructive provider operations and their audit events remain transactional. Hash chaining, WORM storage and SIEM export are deferred to the audit/SIEM phase (PENDING-11). | Q13 (discovery), PENDING-11 |
| D-13 | Audit vs D4 conflict | **Privileged purge path.** The D4 purge runs through a separate privileged DB role/function that may delete one tenant's `audit_logs`, only after export. `provider_audit_logs` is never deletable. | D-09, D-12 |
| D-14 | Provider actions on tenants | Audited in **`provider_audit_logs` only.** Tenant admins don't see provider actions in their own audit log. | Q9 |
| D-15 | Support access | **Option A: cross-tenant read APIs only** (super-admin via `tenant.read`). No impersonation and no entry into tenant context. Grants and impersonation are deferred. | Q9 |
| D-16 | Provider authentication | First release: **bind provider users to Google `sub` and the corporate hosted domain (`hd`)**; **dedicated provider JWT signing key/issuer**, separate from tenant and enrollment tokens; **provider session revocation and logout via a generation counter**. **Zecurity-verified MFA is deferred** until the provider identity/IdP MFA contract is finalised. | Q10 |
| D-17 | Destructive-action safeguards | **Mandatory reason (stored in provider audit) plus typed confirmation** (e.g. tenant slug or relay name) for tenant suspend/delete and relay revoke/delete. No dual approval or step-up in the first release. | Q8, Q10 |
| D-18 | Console packaging | **Separate React app** (own build and domain, network-locked; may share components with `admin/`). | Q12, PENDING-07b |
| D-19 | Relay certificate renewal | **In-band `RelayService.RenewCert` RPC** over the existing mTLS channel (same key, CSR proof of possession), scheduled by the relay before expiry and recorded in `relay_certificates`. | discovery Q6 |
| D-20 | Expired / lost-key relay | **Replace the relay** (revoke/delete it, create a new relay id and token). No provisioning-token re-issue. | discovery Q6 |
| D-21 | Relay drain | **Not in the first release.** | Q15 |
| D-22 | Relay intermediate CA | **Deferred.** Relays stay under the platform Intermediate; revisit with CA rotation work. | Q11 |
| D-23 | Telemetry scope | **Existing data only** for the first release (status, version, capacity label, `connection_count`/`max_connections`, last heartbeat, cert expiry), after the relay-liveness fix. | Q14 |

### Implementation consequences derived from the decisions

These follow from the decisions above combined with current code. They are facts to plan for, not new decisions.

1. **D-07/D-08 (suspension)** touches every enforcement point in Q3:
   - add a workspace-status check to web login (`identity.Resolve` / `auth/callback.go`) and refresh (`auth/refresh.go`);
   - reject the client RPCs `TokenExchange`, `EnrollDevice`, `RenewCert`, `GetACLSnapshot` and `GetTransportSnapshot` for suspended tenants (`internal/client/service.go`);
   - compile a deny-all ACL for suspended workspaces (`internal/policy`);
   - split the SPIFFE trust-domain check so suspended tenants' **agents** are still accepted (`internal/connector/spiffe.go:88-94`, `cmd/server/main.go` CA lookup `w.status='active'`);
   - include suspended workspaces in the disconnect watchers (`connector/disconnect_watcher.go:48`, `shield/heartbeat.go:107`);
   - block SCIM for suspended tenants, which is a configuration mutation under D-07 (`internal/scim/middleware.go`);
   - `WorkspaceGuard` already blocks tenant GraphQL and REST.
2. **D-08** relies on existing connector behaviour: `PolicyCache.update_and_revoked` cancels sessions for removed (spiffe, resource) pairs. Sessions tunnelled via relays also end, because the connector ends them. Resume re-pushes via `NotifyPolicyChange` (process-local; acceptable under D-02).
3. **D-09/D-10/D-11/D-13 (deletion)** needs:
   - a "deletion scheduled" state or timestamp (`workspaces.status` has `deleted` but no scheduled/grace representation; a schema change);
   - a scheduled purge job;
   - an audit export format;
   - draining or purging `outbox_events` (FK `ON DELETE RESTRICT`);
   - verified cascade ordering for the `device_posture_reports` / `device_profile_evaluations` FKs to `client_devices` (no cascade);
   - Workspace CA key destruction;
   - releasing or reserving the slug and `trust_domain` in the tombstone (**not yet decided**, see below).
4. **D-12/D-13** require **separate Postgres roles**: an application role without UPDATE/DELETE on audit tables and a privileged purge role. Today a single DB user is used (`controller/docker-compose.yml`, `DATABASE_URL`), and there is no migration runner (discovery §1.3).
5. **D-16** needs:
   - a new provider-signing-key configuration;
   - provider-specific verification in `provider.VerifyProviderToken` / `RequireProvider`;
   - a generation column on `provider_users` (schema change);
   - `sub` and `hd` columns or checks;
   - a logout endpoint.

   `PROVIDER_BOOTSTRAP_EMAILS` seeding must be reconciled with `sub` binding, because `sub` is only known after first login.
6. **D-19** adds `RenewCert` to `proto/relay/v1/relay.proto` (additive), a relay-side scheduler (`relay/src`), and a controller handler writing `relay_certificates`. The interceptor must accept the renewal call from a relay whose current cert is valid.
7. **D-05/D-06** need no migration. `decide()` gains read actions; `relay-ops` gets `relay.read` through the existing `relay.*` prefix.
8. **Implementation prerequisites (Q15) remain in force:**
   - relay liveness;
   - connector/shield revocation guards;
   - connector renewal trigger;
   - controller cert reload;
   - relay SAN allowlist enforcement;
   - disconnect watcher → transport notify.

   D-08 makes the connector/shield revocation guards and the SPIFFE check split especially important.

### Still open (not decided in this round)

1. **Tombstone and names:** after D4 destruction, are the tenant's `slug` and `trust_domain` permanently reserved or released for reuse?
2. **Audit export:** format and destination for the pre-deletion tenant audit export (to tenant admins, to a provider archive, or both).
3. **Resume after long suspension:** confirm the SCIM, outbox and IdP-connection behaviour during suspension and on resume (e.g. queued SCIM changes, pending device-trust outbox events).
4. **BYO relays:** can tenants ever bring their own relays? Deferred with pools, but it affects the `relays` data model.
5. **Long-term telemetry source of truth** (M1–M4) beyond the first release.
6. **Cross-tenant audit search** vs export/SIEM consumption (first release has provider audit query only).
7. **CA rotation requirements** (deferred together with the relay intermediate CA).
8. **Provider MFA contract** (deferred by D-16).

---

## Decision record amendment — 2026-09-26: provider authentication

Made by the product/architecture owner before Sprint 21 Phase H was implemented. It **supersedes the authentication-method parts of D-16**. Everything else in the 2026-09-23 record stands.

**Rationale:**
- There are two different trust boundaries.
  - The **tenant dashboard** is customer-facing: external organisations authenticate through OIDC, SCIM and their own identity providers.
  - The **provider dashboard** is an internal operator control plane, used only by InkYank employees. It has no customer, partner or MSP logins.
- The provider plane should own its identity instead of depending on an external login provider ("front office" vs "server room").
- **Authentication is separate from authorization.** Authentication answers "who are you?" (password today; OIDC, Google, Azure AD or Okta later). Authorization answers "what can you do?" (`super-admin`, `relay-ops`).
  - The console trusts Zecurity's own **Provider Identity Service**, never an external IdP directly.
  - The service issues the provider JWT and owns roles and `session_generation`. Authentication methods plug into it.
  - This is the model mature infrastructure products use (GitLab, Keycloak, Vault): a local admin account always exists, and external IdPs are pluggable rather than foundational.
- The earlier options were both too extreme. "Google is mandatory" makes the control plane depend on an external provider. "Google is removed forever" leaves no path to SSO. The Provider Identity Service avoids both.

### Decided

| # | Topic | Decision | Supersedes |
|---|---|---|---|
| D-24 | Provider authentication method | A **Provider Identity Service** owned by Zecurity. Its first authentication method is **local accounts**: email + password, hashed with **Argon2id**. The current direct Google OAuth provider login is replaced: no Google `sub` or `hd` binding, no `PROVIDER_ALLOWED_HD`, no provider OAuth callback or redirect flow in the first release. | D-16 "bind provider users to Google `sub` and the corporate hosted domain (`hd`)" |
| D-25 | Retained from D-16 | **Dedicated provider JWT signing key and issuer**, separate from tenant and enrollment tokens (startup refuses a reused tenant secret). **Session revocation and logout via a per-user generation counter** (`session_generation`), incremented on logout, disable, password reset and role change. | — (reaffirms D-16) |
| D-26 | Bootstrap | The first `super-admin` is created from `PROVIDER_BOOTSTRAP_EMAIL` + `PROVIDER_BOOTSTRAP_PASSWORD`, **only if that account does not exist**. It must change its password at first login, and the bootstrap password is then removed from the environment. After that, only a super-admin creates operators. Normal onboarding needs no SQL. | D-16 consequence "reconcile `PROVIDER_BOOTSTRAP_EMAILS` with `sub` binding" |
| D-27 | Operator lifecycle | A super-admin adds operators, changes roles, disables and re-enables them, and **resets passwords** (the replacement for account recovery). Nobody can disable or demote themselves, the last active super-admin can't be removed, and every action is audited in `provider_audit_logs`. | — (extends D-05, D-14) |
| D-28 | MFA and deferred items | **TOTP MFA is mandatory for `super-admin` before Sprint 22's dangerous mutation features.** It takes priority over any external IdP. Backup recovery codes, password rotation policies and refresh tokens are deferred. | D-16 "MFA deferred" |
| D-29 | Future external IdPs | Local authentication is the **bootstrap, break-glass and baseline** identity. External providers (OIDC, Google Workspace, Azure AD, Okta, Keycloak federation) may be added later as **optional authentication sources plugged into the Provider Identity Service**, alongside local accounts. They don't change provider roles, JWT issuance or `session_generation`. Not part of the first release, but it must not block them: a single session-issuing path, an `amr` claim, a nullable password hash, and the email as the stable operator key. | — |

### Consequences for Sprint 21

- **Phase H becomes "Provider Identity Foundation":**
  - the Provider Identity Service seam: an `Authenticator` interface (local password first) and one session-issuing path that is the only place provider JWTs are minted, with an `amr` claim;
  - Argon2id password storage;
  - `POST /provider/auth/login` (JSON credentials → provider JWT), forced password change, logout;
  - a login rate limiter (Valkey);
  - the provider JWT key and issuer, and `session_generation`;
  - bootstrap per D-26;
  - removal of the provider Google OAuth routes and the required `PROVIDER_GOOGLE_REDIRECT_URI`.
- **Phase U** adds password reset and drops the "bootstrap accounts pinned" rule: create-only bootstrap no longer re-activates accounts on restart.
- **Phase C:** the console logs in with a credentials form and a change-password screen, instead of an OAuth redirect.
- **The tenant Google/OIDC login is unchanged.**
- **Before Sprint 22:** mandatory TOTP for `super-admin` (D-28), a separate follow-up after Sprint 21.
