---
type: planning
status: planned
sprint: 8
tags:
  - sprint8
  - dependencies
  - execution-path
  - team-coordination
  - policy-engine
  - acl
---

# Sprint 8 — Execution Path & Dependency Map

> **Read this before writing a single line of code.**
> This file is the source of truth for execution order. Following it prevents merge conflicts, broken builds, and blocked teammates.

---

## Sprint Goal

**Policy Engine: Groups, Resources, ACL Push** — Admins create groups, add users to groups, and assign resources to groups. The Controller compiles those rules into ACL snapshots and pushes them to both Connectors and Clients. Both sides enforce default-deny from local snapshots.

This sprint must land before RDE device tunneling. Sprint 9 can build the tunnel using local ACL snapshots instead of calling the Controller per connection.

---

## Key Design Decisions

| Decision | Detail |
|----------|--------|
| **Policy model** | `group + resource = access rule`. Users inherit resource access through group membership. |
| **Default-deny** | If a resource is missing from the local ACL snapshot, access is denied. |
| **ACL compiler** | Controller compiles per-workspace ACL snapshots: resource address/port/protocol plus allowed client device SPIFFE IDs. |
| **Connector push** | ACL snapshots ride the existing Connector heartbeat response, same pattern as Sprint 6 resource instructions. |
| **Client pull** | Client `GetACLSnapshot` runtime handling is Sprint 8.5 daemon work. Sprint 8 only defines the RPC and Controller handler. |
| **Snapshot shape** | `ACLSnapshot { version, workspace_id, generated_at, entries[] }`; each entry contains `resource_id`, address, port, protocol, and `allowed_spiffe_ids[]`. |
| **Controller snapshot cache** | In-memory per-workspace cache invalidated by `NotifyPolicyChange(workspace_id)`. See [[Decisions/ADR-001-Sprint8-ACL-Snapshot-Caching]]. |
| **Invalidation** | Group/member/rule changes call `NotifyPolicyChange(workspace_id)` so Connectors receive updated snapshots on their next heartbeat. |
| **RDE dependency** | Sprint 9 `device_tunnel.rs` checks the Connector's local snapshot before routing. No per-request Controller access check in the hot path. |
| **Client daemon timing** | M4 daemon foundation is Sprint 8.5, not a Day 1 Sprint 8 task. Daemon is required for active runtime/tunnel state; no direct-state fallback. See [[Decisions/ADR-002-Client-Daemon-Required]]. |

---

## Data Model

New migration: `controller/migrations/012_groups_acl.sql`

| Table | Fields |
|-------|--------|
| `groups` | `id`, `workspace_id`, `name`, `description`, timestamps |
| `group_members` | `group_id`, `user_id`, `joined_at` |
| `access_rules` | `id`, `workspace_id`, `resource_id`, `group_id`, `enabled`, timestamps |

The existing `resources` and `client_devices` tables are used by the compiler.

---

## Team Assignments

| Member | Role | Area |
|--------|------|------|
| **M1** | Frontend | Groups page, member management, resource assignment UI, resource access visibility |
| **M2** | Go (Proto + DB + GraphQL) | Migration 012, GraphQL schema, ClientService `GetACLSnapshot`, Connector heartbeat ACL field |
| **M3** | Go (Controller) | Group/member/rule CRUD, ACL compiler, policy change notification, ClientService ACL handler |
| **M4** | Rust (Client + Connector) | Connector heartbeat ACL receive/store, local default-deny helpers; client daemon foundation moves to Sprint 8.5 |

---

## Critical Rule: Conflict Zones

| File | Who Touches It | Rule |
|------|---------------|------|
| `controller/migrations/012_groups_acl.sql` | M2 | M2 commits first. Do not reuse migration number 012. |
| `proto/client/v1/client.proto` | M2 | Add `GetACLSnapshot`; never renumber existing fields. |
| `proto/connector/v1/connector.proto` | M2 | Add `ACLSnapshot acl_snapshot = 11` to `ConnectorControlMessage`; never renumber existing fields and never reuse field 11. |
| `controller/graph/client.graphqls` or new policy schema | M2 | M2 owns schema/codegen changes. |
| `controller/internal/policy/` | M3 | M3 owns compiler + store + notifier. |
| `controller/internal/client/service.go` | M3 | M3 adds `GetACLSnapshot` handler after proto lands. |
| `connector/src/policy/` | M4 | M4 owns local ACL snapshot/cache helpers. |
| `client/src/login.rs`, `client/src/runtime.rs`, client command files | M4 | Client daemon refactor is Sprint 8.5. Do not add a second direct-state ACL fallback path in Sprint 8. |
| `admin/src/pages/` group UI files | M1 | M1 owns frontend pages and operations. |

---

## Execution Timeline

### DAY 1 — Unblocking Work

- [x] **M2-D1-A** `controller/migrations/012_groups_acl.sql` — Add groups, group_members, access_rules.
- [x] **M2-D1-B** `proto/client/v1/client.proto` — Add `GetACLSnapshot` RPC and ACL snapshot messages.
- [x] **M2-D1-C** `proto/connector/v1/connector.proto` — Add ACL snapshot payload to heartbeat response.
- [x] **M2-D1-D** GraphQL schema — Group CRUD, membership, resource assignment, resource group visibility.
- [x] **TEAM** Run `buf generate` from repo root.
- [x] **TEAM** Run `cd controller && go generate ./graph/...`.
- [x] **TEAM** Run `cd admin && npm run codegen`.

> After Day 1: M3 can implement policy services, M1 can build UI, and M4 can wire client/connector snapshot handling.

---

### PHASE A — M2 Proto + DB + GraphQL

> See [[Sprint8/Member2-Go-Proto-DB/Phase1-Policy-Schema]].

- [x] **M2-A1** Migration 012
- [x] **M2-A2** Client `GetACLSnapshot` proto
- [x] **M2-A3** Connector heartbeat ACL proto
- [x] **M2-A4** GraphQL schema/codegen

> Build check: `buf generate` clean + `cd controller && go build ./...` passes.

---

### PHASE B — M3 Policy CRUD + Compiler

> Depends on: M2-A complete.
> See [[Sprint8/Member3-Go-Controller/Phase1-Policy-Compiler]].

- [x] **M3-B1** Group/member/access-rule store and CRUD.
- [x] **M3-B2** `compile_acl_snapshot(workspace_id)` — resources → groups → users → client device SPIFFE IDs.
- [x] **M3-B3** `NotifyPolicyChange(workspace_id)` version bump/cache invalidation.
- [x] **M3-B4** `SnapshotCache` — in-memory per-workspace cache. Cache miss compiles; policy mutation invalidates.
- [x] **M3-B5** GraphQL/HTTP resolvers call notifier after mutations.
- [x] **M3-B6** ClientService `GetACLSnapshot` validates JWT/device context and returns snapshot.

> Build check: `cd controller && go build ./...` passes.

---

### PHASE C — M4 Connector ACL Handling

> Depends on: M2-A proto complete + M3 Compiler Output Contract documented. M4 does not need to wait for the M3 implementation.
> See [[Sprint8/Member4-Rust-Client-Connector/Phase1-ACL-Snapshot-Handling]].

- [ ] **M4-C1** Connector receives ACL snapshot from heartbeat response.
- [ ] **M4-C2** Connector keeps local in-memory snapshot with default-deny helper APIs.
- [ ] **M4-C3** Add test/helper proving unknown resource and missing SPIFFE are denied.
> Build check: `cd connector && cargo build` passes. Client build remains required for Sprint 8.5 daemon work.

---

### PHASE D — M1 Frontend Policy UI

> Depends on: M2-A GraphQL codegen and M3-B CRUD.
> See [[Sprint8/Member1-Frontend/Phase1-Groups-Policy-UI]].

- [x] **M1-D1** Groups page: create/edit/delete.
- [x] **M1-D2** Members tab: add/remove users from group.
- [x] **M1-D3** Resources tab: assign/unassign resources to group.
- [x] **M1-D4** Resources page: show groups with access.
- [x] **M1-D5** Empty/error/loading states for policy operations.

> Build check: `cd admin && npm run build` passes.

---

## Final Verification Checklist

- [ ] `buf generate` — clean, no errors
- [ ] `cd controller && go build ./...` — clean
- [ ] `cd client && cargo build` — clean
- [ ] `cd connector && cargo build` — clean
- [ ] `cd admin && npm run build` — clean
- [ ] Admin creates a group and adds a user.
- [ ] Admin assigns a resource to that group.
- [ ] Controller compiles ACL snapshot containing that user's client device SPIFFE ID.
- [ ] Connector receives updated ACL snapshot via heartbeat response.
- [ ] Client daemon ACL fetch contract is documented for Sprint 8.5.
- [ ] Unknown resource is denied by local snapshot.
- [ ] Known resource with missing client SPIFFE ID is denied.
- [ ] Known resource with matching client SPIFFE ID is allowed.
- [ ] Policy mutation triggers `NotifyPolicyChange(workspace_id)`.

---

## Dependency Graph

```text
M2-A (migration + proto + GraphQL)
  |
  +--> M3-B (CRUD + compiler + GetACLSnapshot)
  |       |
  |       +--> M4-C (connector snapshot handling)
  |       +--> Sprint 8.5 M4 daemon foundation (client runtime snapshot handling)
  |
  +--> M1-D (groups and access UI)
```

---

## Notes for AI Agents Working on This Sprint

1. Always check this file first. Confirm dependency checkboxes before touching files.
2. Do not implement RDE tunnel routing in Sprint 8. RDE is Sprint 9 and should consume this local ACL snapshot.
3. Default-deny is mandatory. Missing snapshot, missing resource, disabled rule, or missing SPIFFE ID means deny.
4. The Controller is the source of truth for policy compilation. Connector and Client only enforce snapshots.
5. Proto field numbers are permanent. Add fields only at new numbers.
6. Client active runtime state is daemon-required. Do not create optional direct-state fallback paths.
7. Build gates are not optional.

See individual phase files:
- [[Sprint8/Member2-Go-Proto-DB/Phase1-Policy-Schema]]
- [[Sprint8/Member3-Go-Controller/Phase1-Policy-Compiler]]
- [[Sprint8/Member4-Rust-Client-Connector/Phase1-ACL-Snapshot-Handling]]
- [[Sprint8/Member1-Frontend/Phase1-Groups-Policy-UI]]

---

## Post-Sprint Fixes

### Fix: Makefile GQLGEN_VERSION mismatch (fixed by M1 on 2026-04-30)

**File:** `Makefile`

**Issue:** `make gqlgen` failed — `Makefile` had `v0.17.89` but `controller/go.mod` pinned `v0.17.90`.

**Fix:** `GQLGEN_VERSION := v0.17.90`.

---

### Fix: Apollo Client v4 HTTP 401 not caught (fixed by M1 on 2026-04-30)

**File:** `admin/src/apollo/links/error.ts`

**Issue:** Active users were logged out mid-session. Apollo v4 surfaces HTTP 401 as a network error, not a GraphQL error — `CombinedGraphQLErrors.is()` returned false so the refresh/logout logic never ran.

**Fix:** Extended `isUnauthorizedError` to also check `error.statusCode === 401` and `error instanceof Response`.

See full details in [[Sprint8/Member1-Frontend/Phase1-Groups-Policy-UI]] → Post-Phase Fixes.

---

### Fix: Refresh token TTL not sliding (fixed by M1 on 2026-04-30)

**File:** `controller/internal/auth/refresh.go`

**Issue:** Redis TTL set only at login, never extended. Active users logged out after 7 days regardless of activity.

**Fix:** After issuing new access token, call `SetRefreshToken` with the configured TTL to slide the Redis expiry on every refresh.

See full details in [[Sprint8/Member1-Frontend/Phase1-Groups-Policy-UI]] → Post-Phase Fixes.

---

### Fix: M2 Missing `users` GraphQL Query (added by M1 on 2026-04-30)

**File:** `controller/graph/schema.graphqls`, `controller/graph/resolvers/schema.resolvers.go`, `admin/src/graphql/queries.graphql`

**Issue:** M2's schema had no `users` list query. M1's GroupDetail "Add Member" picker needed to list all workspace users to show a dropdown — without it the feature was unusable.

**Fix:** M1 added `users: [User!]!` to the Query type, wired the resolver (scoped by tenant_id from JWT, ordered by email), and added `GetUsers` to the frontend queries.

See full fix details in [[Sprint8/Member2-Go-Proto-DB/Phase1-Policy-Schema]] → Post-Phase Fixes.

---

### Fix: M3 Missing GraphQL Resolver Helpers (fixed by M1 on 2026-04-30)

**File:** `controller/graph/resolvers/policy_helpers.go`

**Issue:** Controller build broken after M3's PR merged. `policy.resolvers.go` called `groupRowToGQL`, `r.loadGroup`, and `r.loadResourceWithGroups` which were never implemented.

**Fix:** M1 added the three missing functions to `policy_helpers.go`. Both `loadGroup` and `loadResourceWithGroups` are defined on `*Resolver` (the base struct) so both `mutationResolver` and `queryResolver` can access them via embedding.

See full fix details in [[Sprint8/Member3-Go-Controller/Phase1-Policy-Compiler]] → Post-Phase Fixes.
