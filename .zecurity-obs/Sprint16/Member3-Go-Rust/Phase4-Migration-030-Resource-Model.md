---
type: phase
sprint: 16
stage: 2
phase: 4
title: Migration 030 + Resource Model
owner: M3
depends_on:
  - Sprint16/Member3-Go-Rust/Phase3-Connector-Requires-ResourceId
status: done
tags: [sprint16, controller, go, postgres, migration, fqdn, resource-model, shield-ha]
---

# Sprint 16 · Phase 4 — Migration 030 + Resource Model

> Goal: stop the `resources` table from conflating three different concepts in one `host` column, so a
> resource can be addressed by **name** with no pinned IP.
> Depends on **Phase 3** (Stage 1 must be green — it is the contract Stage 2 rests on).

## Why

Until now **a resource *was* an IP**: `host` was `NOT NULL`, and the client keyed its transports on it.
A resource that is only reachable by name, or whose backend IP moves (cloud DNS, load balancers, k8s
Services), could not be expressed at all — the client silently dropped any ACL entry whose address
didn't parse as IPv4.

`host` was carrying three unrelated jobs. The migration separates them:

| Concept | Column | Who reads it |
|---|---|---|
| Client-facing **name** (what an app / DNS / TLS uses) | `hostname` | client (synthetic-IP binding), connector (resolution input) |
| Pinned **IP** | `host` — now **nullable** | client (route), connector (dial target) |
| **How** to find the current endpoint | `resolver` (jsonb) | connector only |
| Which **local** address a shield dials | `local_target` | **Shield only** — see Phase 8 |

## Tasks

### 4.1 — `controller/migrations/030_fqdn_resources.sql` (new)
> 027–029 are taken by Sprint 14. **030 is the next free number.**

- [x] `ADD COLUMN hostname TEXT`, `resolver JSONB`, `local_target TEXT` — all nullable, so existing
      rows are untouched and compile into an identical ACL entry.
- [x] `ALTER COLUMN host DROP NOT NULL`.
- [x] `resources_addressable_check`: `CHECK (host IS NOT NULL OR hostname IS NOT NULL)` — a row with
      neither is meaningless and would compile into an unroutable entry.
- [x] `resources_resolver_shape_check`: `CHECK (resolver IS NULL OR (jsonb_typeof(resolver) = 'object'
      AND resolver ? 'type'))` — the connector dispatches on `type`; a typeless blob would fail closed
      at dial time with no way to diagnose it.
- [x] **Replace the unique index.** The old `UNIQUE (tenant_id, remote_network_id, host, name)`
      silently stops enforcing anything once `host IS NULL`, because Postgres treats NULLs as
      distinct — two FQDN resources could share a name in one network. Replaced with:
      ```sql
      CREATE UNIQUE INDEX resources_workspace_network_addr_name_key
        ON resources (tenant_id, remote_network_id, COALESCE(host, hostname), name);
      ```
      This preserves the previous semantics exactly for IP resources.
- [x] `idx_resources_hostname ON resources (tenant_id, hostname) WHERE hostname IS NOT NULL` — lookup
      support for the ACL compiler.
- [x] `route_type` is **NOT** stored. It stays derived from `(status, shield_id)` in
      `policy.routeTypeForResource`. Nothing in this migration changes that.

### 4.2 — `internal/resource/store.go`
- [x] `CreateInput` / `UpdateInput` carry `Hostname`, `Resolver`, `LocalTarget`.
- [x] ⚠️ **Making `host` nullable breaks every `Scan` into a non-nullable Go `string`.** All read sites
      now `COALESCE(host, '')`. This is the single highest-risk edit in the phase — a missed site is a
      runtime scan error, not a compile error, so it will not show up in `go build`.
- [x] `AutoMatchShield` applies **only to IP-hosted resources**. A shield protects the host it runs on
      and validates `resource.host == detect_lan_ip()`; a name with no pinned IP has nothing to match
      against. Consequence: **FQDN resources are always `route_type == "connector"`**, which is what
      makes connector-side resolution the only path that needs a resolver.

### 4.3 — `shield_ids[]` shape — decided: **join table**
- [x] `resources.shield_id` (singular) cannot express a service replicated across hosts, each running
      a shield. Added `resource_shields (resource_id, shield_id, created_at)` + backfill from the
      singular column, so both agree from day one.
- [x] `resources.shield_id` is **kept and still authoritative** for existing readers (policy compiler,
      reconciler). The join table is written in parallel; readers migrate in a later phase.
      *Rationale: HA is expensive to retrofit, cheap to add now.*

## Build gate

```bash
cd controller && go build ./...     # PASS
```

Test coverage: `internal/resource/create_addressing_test.go`, DB-gated on
`RESOURCE_TEST_DATABASE_URL`.

## Verify

- [x] Existing IP resources round-trip unchanged; the ACL compiles identically for them.
- [x] A row with neither `host` nor `hostname` is rejected by the DB.
- [x] Two FQDN resources with the same name in one remote network are rejected by the new index.

## Post-Phase Fixes

### Fix: unique index silently stopped enforcing once `host` became nullable
**Issue:** dropping `NOT NULL` on `host` would have allowed unlimited duplicate FQDN resources.

**Root cause:** Postgres treats NULLs as distinct in a unique index, so
`UNIQUE (tenant_id, remote_network_id, host, name)` becomes a no-op for FQDN rows.

**Fix applied:** replaced with a unique index on `COALESCE(host, hostname)`. Caught while writing the
migration, not in review — worth remembering that nullability changes can silently disarm an existing
constraint.

## Notes

- ⚠️ **Known gap, carried into Phase 6 (`6.0`):** the DB check is *at-least-one*
  (`host IS NOT NULL OR hostname IS NOT NULL`), while the **exactly-one** rule is enforced only at the
  GraphQL layer by `validateAddressing` (Phase 5). A row inserted directly by SQL — which Phase 5's own
  solo tip recommends — can legally have **both**, and nothing downstream defines which wins. The
  migration's own header comment ("a resource may have a name AND a pinned IP") contradicts
  `validateAddressing`; treat `validateAddressing` as the rule and make the connector fail closed.
- `local_target` is added by this migration but **not delivered anywhere** until Phase 8. It is a
  Shield-only field and deliberately never reaches an `ACLEntry` — see the design correction in
  [[Sprint16/Member3-Go-Rust/Phase5-Proto-ACL-Emission-GraphQL]].
