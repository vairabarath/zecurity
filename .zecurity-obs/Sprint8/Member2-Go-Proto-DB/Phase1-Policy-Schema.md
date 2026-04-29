---
type: phase
status: pending
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
