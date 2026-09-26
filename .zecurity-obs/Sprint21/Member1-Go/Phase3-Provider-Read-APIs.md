---
type: phase
member: M1
person: Sathiya
sprint: 21
phase: 3
execution: R
title: Provider Read Actions + Read APIs
status: planned
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

- [ ] AT-R.1 … AT-R.8 in [[Sprint21/Acceptance-Test-Plan]] pass.

## Build Check

```bash
cd controller && go build ./... && go vet ./... && go test ./internal/provider/... ./internal/providerquery/... ./internal/pki/...
```

## Implementation Checklist

- [ ] R1 read actions + matrix test
- [ ] R2 providerquery: relays, tenants, audit, certificates (+ rotator accessor)
- [ ] OQ-1 / OQ-2 answered and recorded here
- [ ] R3 handlers + routes (rebased on M2-H and M2-U)
- [ ] R4 tests; build gate

## Post-Phase Fixes

_None yet._
