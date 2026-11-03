---
type: phase
member: M1
person: Barath   # reassigned 2026-09-28 (Sathiya on testing, Phase V)
sprint: 21
phase: 3
execution: R
title: Provider Read Actions + Read APIs
status: done   # 2026-10-06, branch sprint21/r-provider-read-apis (R1–R7)
depends_on: ["M2-Phase1", "M2-Phase2"]   # R1–R2 may start Day 1; R3 route wiring after M2-H and M2-U are merged (main.go order H → U → R3)
schema_change: false
tags:
  - go
  - provider
  - rest
  - authz
  - provider-dashboard
---

# Phase 3 (R) — Provider Read Actions + Read APIs

> **Decision Record:** D-04 (REST under `/provider/*`; query services independent of HTTP), D-05 (read actions, no role migration), D-06 (`tenant.*` super-admin only via the prefix rule), D-15 (cross-tenant read APIs only), D-23 (existing telemetry only). This covers Q15 prerequisite #7, "Relay read endpoints".
> **Open questions to settle before R3's tenant endpoints:** OQ-1 (audit tenant reads?) and OQ-2 (tenant admin PII in detail?). See `Sprint21/path.md`.

## Problem (verified)

- **The provider REST surface can't read anything useful.** Its routes are:
  - `POST /provider/relays`, `POST /provider/relays/{id}/revoke`, `DELETE /provider/relays/{id}` (`cmd/server/main.go:454-456`);
  - `GET /provider/me`, `GET /provider/users` (`main.go:355-356`).
  - Nothing lists or describes relays, tenants, provider audit or certificate expiry.
- **`decide()` has no read actions** except the unused `audit.view` / `CanViewProviderAudit` (`internal/provider/authz.go`).
- **The data exists:**
  - relays: `relays`, `relay_certificates`, `connector_relay_placement`, plus Valkey `relay:heartbeat:last:<id>`;
  - tenants: `workspaces`, `workspace_ca_keys`; counts from `connectors`, `shields`, `remote_networks`, `users` (`tenant_id`) and `client_devices` (`workspace_id`);
  - provider audit: `provider_audit_logs`, indexed on `created_at DESC` and `(target_type, target_id)`;
  - certificates: `ca_root`, `ca_intermediate`, `workspace_ca_keys`, relays and `relay_certificates`, `connectors`, `shields`, `client_devices`, plus the controller gRPC cert, held **only in memory** in `pki.ControllerCertRotator`.

## Goal

Correct, role-gated REST reads for relays, tenants, provider audit and certificate health, with no secrets in any response.

## Files

| File | Change |
|------|--------|
| `controller/internal/provider/authz.go` | Read actions + `Can…` methods (shared review) |
| `controller/internal/providerquery/` (new) | `relays.go`, `tenants.go`, `audit.go`, `certificates.go`: pgx queries plus DTOs, **no `net/http`** |
| `controller/internal/provider/read_handlers.go` (new) | HTTP handlers: parse params → authz → providerquery → JSON |
| `controller/internal/pki/controller_rotator.go` | Read-only `CurrentCertInfo() (serial string, notAfter time.Time)` |
| `controller/cmd/server/main.go` | Route wiring (after M2-H and M2-U merged) |
| Tests | `authz_test.go`, `providerquery/*_test.go` (DB), `read_handlers_test.go` |

## Steps

### R1 — Read actions (`authz.go`)

```go
ActionRelayRead  = "relay.read"   // relay-ops via the "relay." prefix; super-admin via all
ActionTenantRead = "tenant.read"  // super-admin only — prefix rule, no special case (D-06)
ActionCertRead   = "cert.read"    // super-admin only — spans every tenant's certificates
// ActionAuditView = "audit.view" — already defined; reuse it (don't rename)
func (a *Authz) CanReadRelays(actor Actor, target Target) error
func (a *Authz) CanReadTenants(actor Actor, target Target) error
func (a *Authz) CanReadCertificates(actor Actor) error
// CanViewProviderAudit already exists
```

**Don't change `decide()` itself.** The prefix rules already produce the D-05/D-06 matrix. Add a table test asserting the full matrix (role × action).

### R2 — Query services (`internal/providerquery/`)

All functions take `context.Context` plus a pool or `pgx` querier and return DTOs. There are no HTTP types. Limits are clamped server-side (default 50, max 200). Lists use **keyset pagination** (`created_at, id`) with an opaque `next` cursor.

- **`ListRelays(filter{status})`**, one row per relay:
  - `id, name, status, version, hostname, public_addr, address_scope, capacity_label, connection_count, max_connections, cert_serial, cert_not_after, created_at`;
  - `last_heartbeat_at` = the **fresher** of the DB value and Valkey `relay:heartbeat:last:<id>` (D-23, Sprint 20 Phase A);
  - `attached_connectors` = count from `connector_relay_placement`.
  - Excludes `deleted` unless `status=deleted` is asked for.
- **`GetRelay(id)`:** the list fields, plus:
  - `dns_allowlist, ip_allowlist`;
  - cert history from `relay_certificates` (`serial, issued_at, not_after, revoked_at, revocation_reason, is_current`);
  - attachments: `connector_id, workspace_id, attached_at, last_confirmed, source`.
  - **Never** `enrollment_token_jti`.
- **`ListTenants(filter{status})`:**
  - `id, slug, name, status, trust_domain, created_at`;
  - `ca_not_after` from `workspace_ca_keys.not_after`;
  - counts: connectors by status, shields by status, remote networks, users, client devices.
  - One query with `LEFT JOIN LATERAL` counts, or grouped subqueries; **no N+1**.
- **`GetTenant(id)`:** the list fields, plus remote networks (`id, name, status`), connectors (`id, name, status, revoked_at, cert_not_after, last_heartbeat_at, version, remote_network_id`) and shields (`id, name, status, cert_not_after, last_heartbeat_at, connector_id`).
  - **Excluded:** user emails and identities (OQ-2), `encrypted_*`, `ca_cert_pem` bodies, any tokens.
- **`QueryProviderAudit(filter{action, target_type, target_id, provider_email, since, until})`:** rows from `provider_audit_logs`, newest first, keyset-paginated. `details` is returned as JSON.
- **`CertificateExpiry(filter{within, kind, tenant_id})`:** a normalized row set:
  - Each row is `{kind, tenant_id?, entity_id, entity_name, serial?, not_after, status}`.
  - `kind` is one of `root`, `intermediate`, `workspace_ca`, `controller_grpc`, `relay`, `connector`, `shield`, `client_device`.
  - Revoked or deleted entities are excluded. `controller_grpc` comes from `CurrentCertInfo()` (process-local, D-02).
  - A **summary** gives counts per bucket: `expired`, `<24h`, `<7d`, `<30d`, `ok`.

### R3 — Handlers + routes (after M2-H and M2-U merged)

| Route | Action | Notes |
|-------|--------|-------|
| `GET /provider/relays` | `relay.read` | `?status=&limit=&cursor=` |
| `GET /provider/relays/{id}` | `relay.read` | 404 if unknown |
| `GET /provider/tenants` | `tenant.read` | `?status=&limit=&cursor=` |
| `GET /provider/tenants/{id}` | `tenant.read` | **OQ-1:** if audited, `InsertAudit` action `tenant.read`, target `tenant/<id>` |
| `GET /provider/audit` | `audit.view` | filters as in R2 |
| `GET /provider/certificates` | `cert.read` | `?within=720h&kind=&tenant_id=` |

- All routes go behind `RequireProvider`, matching the existing `mux.Handle("GET /provider/…", requireProvider(…))` pattern.
- Status codes: 403 on `ErrForbidden`, 400 on bad params, 404 on a missing id, 500 otherwise.
- JSON field names are `snake_case`, and timestamps are RFC 3339 UTC.

### R4 — Tests

See the table below. The role matrix and "no secret fields" tests get shared review with M2.

## Invariants

1. **Read-only:** no handler in this phase writes, except the OQ-1 audit insert if that's decided.
2. **Role matrix:**
   - relay-ops → relays only (403 on tenants, audit and certificates);
   - super-admin → everything;
   - a tenant JWT → 401 (M2-H).
3. **No secrets:** no `encrypted_*`, private keys, `enrollment_token_jti`, tokens or `ca_cert_pem` bodies in any response.
4. Relay liveness in reads never reports a live relay as stale because of the 5-minute DB write throttle (it uses the Valkey fallback).
5. `internal/providerquery` imports no `net/http` (D-04).
6. Bounded queries: every list is paginated and clamped, with no unbounded scans or N+1.

## Tests

| Test | Kind | Asserts |
|------|------|---------|
| `TestDecide_ReadMatrix` | unit | the role × action table for all read actions |
| `TestListRelays_LivenessPrefersFreshValkey` | DB + Valkey | stale DB + fresh Valkey → fresh `last_heartbeat_at` |
| `TestGetRelay_CertHistoryAndAttachments` | DB | history ordered, `is_current` correct, attachments listed; no `enrollment_token_jti` |
| `TestListTenants_Counts` | DB | seeded counts per status match; one query (no N+1) |
| `TestGetTenant_NoSecrets` | DB | JSON has no `encrypted`, `private`, `token` or PEM bodies |
| `TestQueryProviderAudit_FiltersAndCursor` | DB | each filter; stable keyset pagination across pages |
| `TestCertificateExpiry_BucketsAndKinds` | DB | every kind appears; buckets correct; revoked excluded; the controller cert included |
| `TestReadHandlers_RoleMatrix` | HTTP | relay-ops 403 on tenants/audit/certificates; super-admin 200; tenant JWT 401 |
| `TestReadHandlers_BadParams400_NotFound404` | HTTP | as named |

DB-backed tests use the existing throwaway-DB harness pattern (e.g. `PKI_TEST_DATABASE_URL`). **A skipped DB test fails acceptance.**

## Acceptance Criteria

- [x] AT-R.1 … AT-R.8 in [[Sprint21/Acceptance-Test-Plan]] pass. *(Verified in development: unit and HTTP tests plus the real-binary live check; see "Closeout" below. Formal acceptance is M1's.)*

## Build Check

```bash
cd controller && go build ./... && go vet ./... && go test ./internal/provider/... ./internal/providerquery/... ./internal/pki/...
```

## Implementation Checklist

- [x] R1 read actions + matrix test
- [x] R2 providerquery: relays, tenants, audit, certificates (+ rotator accessor)
- [x] OQ-1 / OQ-2 answered and recorded here
- [x] R3 handlers + routes (rebased on M2-H and M2-U)
- [x] R4 tests; build gate

## Closeout (2026-10-06)

### Commits (branch `sprint21/r-provider-read-apis`)
1. `33d0287` R1: read actions (`relay.read`, `tenant.read`, `cert.read`; `audit.view` reused) and the matrix tests. `decide()` is unchanged.
2. `87f8f2b` `providerquery` base (read-only `Querier`, pagination) and relay list/detail.
3. `8556274` Tenant list/detail.
4. `77992ae` Provider audit query.
5. `4d17b3d` Certificate expiry and `pki.ControllerCertRotator.CurrentCertInfo()`.
6. `f7215e5` The six HTTP routes and the fail-closed tenant detail audit.
7. This closeout (docs only).

### Locked decisions
- **OQ-1, tenant detail only, fail-closed.**
  - **Flow:** `RequireProvider` → authorize → validate UUID → `BEGIN REPEATABLE READ` → `GetTenant(tx)` → (404: rollback, no audit) → marshal → `InsertAuditTx` in the same transaction → `COMMIT` → first response byte.
  - **Audit row:** action `tenant.read`, target `tenant/<uuid>`, the actor's id and email, client IP (host only), details exactly `{"view":"detail"}`.
  - **Failures:** an insert or commit failure rolls back and returns 500 with no tenant data. A client that disconnects after the commit leaves an audit row behind, so errors over-record and never under-record.
  - **Not audited:** tenant lists, relay, audit and certificate reads, and 400/401/403/404 responses.
- **OQ-2, no tenant admin identities.**
  - No tenant user rows in any response; users appear as status counts only.
  - Never selected: email, name, `provider_sub`, role, `last_login_at`, device owner.
  - No identity lookup endpoint.

### Choices made in review (accepted)
- **Tenant detail** keeps soft-deleted remote networks and revoked connectors/shields, each with its status (or `revoked_at`). The counts block counts only active remote networks, per plan.
- **Audit time filter:** the half-open window `[since, until)`. `since > until` → 400 `invalid_range`.
- **`provider_email` filter:** case-insensitive exact match.
- **Client device certificate rows:** `entity_name` is null (device names are user-chosen and may identify a person).
- **`controller_grpc` row:** `entity_id` is `"controller"` (single controller, D-02). Its data comes from the process-local `CurrentCertInfo()`; nothing is persisted.
- **Certificate serials:** lowercase hex (`SerialNumber.Text(16)`, the form stored for relay certificates). `openssl` prints uppercase and zero-pads, so compare case-insensitively and ignore leading zeros.
- **Order of checks:** authorization runs before validation, so relay-ops with a malformed tenant id gets 403.
- **Within the read-only `providerquery` package:**
  - relay status is read from the database as-is;
  - liveness uses the single-key `relay.Service.LastHeartbeat` (one Valkey read per row, bounded by the page size).

### Spec vs database: mismatches and how they were resolved
- **`relay_certificates.is_current`** doesn't exist. Derived as `serial = relays.cert_serial`.
- **`connector_relay_placement.workspace_id`** doesn't exist. The tenant is joined through `connectors.tenant_id`.
- **`ca_root`, `ca_intermediate` and `workspace_ca_keys`** store no serial. It's parsed on the server from the **public** `certificate_pem`, which is never returned. Key columns (`encrypted_*`, `nonce`) are never selected.
- **`pki.ControllerCertRotator`** had no accessor. Added the read-only `CurrentCertInfo()` (serial, notBefore, notAfter), which parses the DER when `Leaf` is unset.
- **Shields** have no `revoked_at`. `status='revoked'` is used.
- **"Deleted" for relays and workspaces** is a status value, not a column. It's excluded by default in lists, and readable by id.
- **Tenant-scoped tables** use `tenant_id`, except `client_devices`, which uses `workspace_id`.
- **The Valkey liveness key** `relay:heartbeat:last:<id>` holds unix seconds (TTL 330s by default). Reads report the fresher of it and the database value.

### Test-only import cycle (resolved in R6)
- **Cause:** once `provider` imported `providerquery`, the real-Valkey liveness test (an internal `providerquery` test importing `internal/relay`, which imports `provider`) formed an import cycle.
- **Fix:** that test moved to an external test package (`providerquery_test`), with helpers exposed through `export_test.go`. Package code is unchanged.
- **Check still holds:** the `go list -deps` test still shows no `net/http`, `relay` or `provider` in `providerquery`.

### Live check (2026-10-06, real binary)
- **Setup:** throwaway Postgres DB and throwaway Valkey container. Operators: a super-admin (bootstrap) and relay-ops (created through the operator API). A tenant JWT signed with the real `JWT_SECRET`.
- **Auth matrix, all six routes × five callers (30 checks), all as expected:**
  - super-admin: 200;
  - relay-ops: 200 on relays, 403 `forbidden` on tenants, audit and certificates;
  - no token, tenant JWT and malformed token: 401.
- **Relay liveness:**
  - The database heartbeat was 4 minutes old and Valkey's fresh. List and detail both reported the Valkey time with status `active`.
  - After one controller eviction pass (~70s) the relay was still `active`.
  - Attachments were listed, and there was no `jti` in the response.
- **Tenant detail audit, measured as deltas:**
  - the tenant list added 0 rows; one detail read added exactly 1;
  - 400, 403, 404 and 401 responses, and relay, audit and certificate reads, added 0;
  - every `tenant.read` row had the right actor id and email, target, IP `::1` (no port) and exactly `{"view":"detail"}`;
  - the audit query endpoint found the rows and wasn't itself audited.
- **No identities or secrets:** canaries seeded in tenant user, device, connector and CA columns (email, `provider_sub`, device owner, hostname, public IP, jti, encrypted key, nonce) were absent from both tenant responses. Users appeared only as counts.
- **Certificates:**
  - the controller_grpc serial and expiry matched `openssl s_client -connect localhost:19090` (case and leading zeros ignored);
  - the workspace_ca serial matched the PEM's serial;
  - client device `entity_name` was null;
  - no PEM, private-key, encrypted, nonce or owner text appeared;
  - the summary was identical for `within=24h` and `within=8760h`.
- **Kinds seen:** six of the eight appeared as items (client_device, connector, controller_grpc, relay, shield, workspace_ca). `root` (10-year) and `intermediate` (5-year) were present in the summary's `ok` count but outside the 8760h maximum window. A `kind=root` / `kind=intermediate` query returned 0 items with `ok: 1`. This is the locked behaviour (see the open question below). All eight kinds appear as items in the DB tests.
- **Bad input:**
  - `invalid_cursor`, `invalid_limit`, `invalid_id`, `invalid_kind`, `invalid_within` (9000h) and `invalid_range` returned 400;
  - an unknown relay or tenant returned 404;
  - `Cache-Control: no-store` was present on errors.
- **Cleanup:** DB dropped, Valkey container removed, no leftover processes, files or dev-Valkey keys.

### Final test numbers (R6 code, unchanged since)
- `go build ./...` and `go vet ./...`: clean. `gofmt` is clean on every file R touched.
- Package results:

  | Package | Passed | Failed | Skipped |
  |---|---|---|---|
  | `internal/providerquery` | 71 | 0 | 0 |
  | `internal/provider` | 86 (incl. 14 HTTP tests) | 0 | 0 |
  | `internal/middleware` | 18 | 0 | 0 |
  | `internal/pki` | 36 | 0 | 0 |
  | `internal/relay` | 118 | 0 | 0 |

- **Full CI-style run** `go test -count=1 ./...` (throwaway DBs, isolated Valkey): 25 packages, 1,023 pass, 0 fail, 33 skip. The skips are the pre-existing set, all outside R's packages: `RESOURCE_TEST_SHIELD_ID` tests, K6, one connector test.
- **Mutation checks:**
  - R1: 4;
  - R2/relays: 7;
  - R3/tenants: 10;
  - R4/audit: 12;
  - R5/certificates: 19, plus 2 on `CurrentCertInfo`;
  - R6/handlers: 15, including every fail-closed case: audit removed, audit after commit, response before commit, commit error ignored, action/target/details changed, list audited, unauthorized detail.

  All were caught. The harness counts a mutation that fails to compile as invalid rather than as "0 failing".

### Acceptance (verified in development)

| Case | Evidence |
|---|---|
| AT-R.1 Role matrix | Unit, HTTP and live (30 checks) |
| AT-R.2 Relay list liveness | DB+Valkey test through `relay.Service` and live: fresh time, `status=active` |
| AT-R.3 Relay detail | Allowlists, history with `is_current` and `revoked_at`, attachments, no `enrollment_token_jti` (tests and live) |
| AT-R.4 Tenant list/detail | Seeded counts per status, three sublists, canary scans for secrets and identities (tests and live) |
| AT-R.5 Tenant detail audit | One `tenant.read` per detail, none for lists; fail-closed on insert or commit failure (tests and live) |
| AT-R.6 Provider audit query | Each filter, combinations, stable keyset, limit clamp 200 (tests and live) |
| AT-R.7 Certificate expiry | All eight kinds, buckets, revoked/deleted excluded (tests); controller cert matches `openssl s_client` (live) |
| AT-R.8 Bad input | 400 on malformed cursor, limit or `within`; 404 on unknown id (tests and live) |

### Open question
- **Root and intermediate in the item list:** with `within` capped at 8760h, the multi-year root (10y) and intermediate (5y) can never appear in the item list; they show only in the summary's `ok` count. That follows the locked design. If operators should see the platform CAs' expiry dates directly, the options are:
  1. always list `root` and `intermediate` regardless of `within`;
  2. raise the maximum `within`.

  Either is a behaviour change for review. It was not changed in closeout.

## Post-Phase Fixes

_None yet._
