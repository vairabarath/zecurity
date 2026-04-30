---
type: phase
status: done
sprint: 8
member: M2
phase: Phase1-Policy-Schema
depends_on: []
tags:
  - go
  - proto
  - db
  - graphql
  - policy-engine
---

# M2 Phase 1 — Policy Schema

> Day 1 work. M1, M3, and M4 are blocked until this lands and codegen is complete.

---

## What You're Building

Create the schema foundation for group-based access policy and ACL snapshots.

---

## Files to Touch

### `controller/migrations/012_groups_acl.sql`

Add:

- `groups`: workspace-scoped group records.
- `group_members`: group-to-user membership.
- `access_rules`: group-to-resource grants.

Rules:

- Use UUID primary keys.
- Enforce workspace scoping.
- Add uniqueness constraints for `(workspace_id, name)`, `(group_id, user_id)`, and `(resource_id, group_id)`.
- Add useful indexes for compiler queries.

### `proto/client/v1/client.proto`

Add `GetACLSnapshot` to `ClientService`.

Suggested messages:

```proto
rpc GetACLSnapshot(GetACLSnapshotRequest) returns (GetACLSnapshotResponse);

message GetACLSnapshotRequest {
  string access_token = 1;
  string device_id = 2;
}

message GetACLSnapshotResponse {
  ACLSnapshot snapshot = 1;
}

message ACLSnapshot {
  uint64 version = 1;
  string workspace_id = 2;
  int64 generated_at = 3;
  repeated ACLEntry entries = 4;
}

message ACLEntry {
  string resource_id = 1;
  string address = 2;
  uint32 port = 3;
  string protocol = 4;
  repeated string allowed_spiffe_ids = 5;
}
```

### `proto/connector/v1/connector.proto`

Add ACL snapshot payload to `ConnectorControlMessage.oneof body` without changing existing field numbers.

Field number decision:

```proto
message ConnectorControlMessage {
  oneof body {
    // existing fields 1-10 unchanged

    // Controller -> Connector
    ACLSnapshot acl_snapshot = 11;
  }
}
```

Rules:

- Use field number `11` for `acl_snapshot`.
- Never use field number `11` for anything else in `ConnectorControlMessage`.
- This does not conflict with Sprint 9 ShieldControlMessage fields 8-11 because that is a different proto message in `proto/shield/v1/shield.proto`.

### GraphQL schema

Add group CRUD, group membership, resource assignment, and resource group visibility.

---

## Build Check

```bash
buf generate
cd controller && go generate ./graph/...
cd controller && go build ./...
cd admin && npm run codegen
```

---

## Post-Phase Fixes

### Fix: Missing `users` GraphQL Query (added by M1 on 2026-04-30)

**Issue:** M2's GraphQL schema added group types and mutations but did not add a `users` query. M1's GroupDetail "Add Member" flow needs to list all workspace users to populate the user picker dropdown. Without this query, the Add Member UI had no way to fetch users.

**Root Cause:** M2's phase doc mentioned group CRUD, membership, and resource assignment in the GraphQL schema section but did not explicitly call out a `users: [User!]!` list query. The `me` query already existed (Sprint 1) but returns only the current user.

**Fix Applied:**

`controller/graph/schema.graphqls` — added to `Query`:
```graphql
# Returns all active users in the current workspace.
# Scoped to tenant_id from JWT — never crosses workspaces.
users: [User!]!
```

`controller/graph/resolvers/schema.resolvers.go` — added resolver:
```go
func (r *queryResolver) Users(ctx context.Context) ([]*models.User, error) {
    tc := tenant.MustGet(ctx)
    // queries all active users for the workspace ordered by email
    ...
}
```

`admin/src/graphql/queries.graphql` — added `GetUsers` query used by GroupDetail Add Member picker.
