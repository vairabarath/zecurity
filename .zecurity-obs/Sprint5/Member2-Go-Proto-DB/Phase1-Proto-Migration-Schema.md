---
type: task
status: pending
sprint: 5
member: M2
phase: 1
priority: DAY1-CRITICAL
depends_on: []
unlocks:
  - Everyone (buf generate unblocks M3 + M4)
  - M1 codegen (needs graph/resource.graphqls)
  - M3 resource package (needs migration 007)
tags:
  - go
  - proto
  - migration
  - graphql
  - day1
---

# M2 · Phase 1 — Proto + Migration + GraphQL Schema (DAY 1 — COMMIT FIRST)

> **Most critical commit in Sprint 5.**
> Nothing else can proceed until these files are committed and `buf generate` + `go generate` run clean.

---

## Files to Create / Modify

| File | Action |
|------|--------|
| `proto/shield/v1/shield.proto` | MODIFY — add ResourceInstruction, ResourceAck, heartbeat fields |
| `proto/connector/v1/connector.proto` | MODIFY — add ShieldResourceInstructions, heartbeat fields |
| `controller/migrations/007_resources.sql` | CREATE |
| `controller/graph/resource.graphqls` | CREATE |

---

## Checklist

### 1. Modify `proto/shield/v1/shield.proto`

- [ ] Add `ResourceInstruction` message:
  ```protobuf
  message ResourceInstruction {
    string resource_id = 1;
    string host        = 2;  // must match shield's lan_ip
    string protocol    = 3;  // "tcp", "udp", "any"
    int32  port_from   = 4;
    int32  port_to     = 5;
    string action      = 6;  // "apply" or "remove"
  }
  ```
- [ ] Add `ResourceAck` message:
  ```protobuf
  message ResourceAck {
    string resource_id      = 1;
    string status           = 2;  // "protected", "failed", "removed"
    string error            = 3;  // set only on failure
    int64  verified_at      = 4;  // unix timestamp of last port check
    bool   port_reachable   = 5;
  }
  ```
- [ ] Add to `HeartbeatResponse`:
  ```protobuf
  repeated ResourceInstruction resources = 4;  // pending instructions from Controller
  ```
- [ ] Add to `HeartbeatRequest`:
  ```protobuf
  repeated ResourceAck resource_acks = 5;  // Shield reports back results
  ```
- [ ] **Never change existing field numbers.** Current HeartbeatRequest max = 4 (lan_ip). Use 5.

### 2. Modify `proto/connector/v1/connector.proto`

- [ ] Add `ShieldResourceInstructions` wrapper message:
  ```protobuf
  message ShieldResourceInstructions {
    repeated ResourceInstruction instructions = 1;
  }
  ```
  > Re-use `ResourceInstruction` from shield.proto OR define inline — keep in connector.proto for connector-side codegen.

- [ ] Add to `HeartbeatResponse` (Connector ↔ Controller):
  ```protobuf
  // keyed by shield_id → list of pending resource instructions for that shield
  map<string, ShieldResourceInstructions> shield_resources = N;
  ```
  > Check current max field number in HeartbeatResponse and use next available.

- [ ] Add to `HeartbeatRequest` (Connector → Controller):
  ```protobuf
  repeated ResourceAck resource_acks = N;  // forwarded from all shields
  ```
  > Check current max field in HeartbeatRequest (currently: connector_id=1, status=2, version=3, hostname=4, public_ip=5, lan_addr=6, shields=7 — use 8).

- [ ] **Never change or reuse existing field numbers.**

### 3. Create `controller/migrations/007_resources.sql`

```sql
CREATE TABLE resources (
  id                UUID PRIMARY KEY DEFAULT gen_random_uuid(),
  tenant_id         UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
  remote_network_id UUID NOT NULL REFERENCES remote_networks(id) ON DELETE CASCADE,
  shield_id         UUID NOT NULL REFERENCES shields(id) ON DELETE CASCADE,
  name              TEXT NOT NULL,
  description       TEXT,
  protocol          TEXT NOT NULL DEFAULT 'tcp'
                    CHECK (protocol IN ('tcp','udp','any')),
  host              TEXT NOT NULL,
  port_from         INT  CHECK (port_from BETWEEN 1 AND 65535),
  port_to           INT  CHECK (port_to BETWEEN 1 AND 65535),
  CHECK (port_to >= port_from),
  status            TEXT NOT NULL DEFAULT 'pending'
                    CHECK (status IN ('pending','managing','protecting',
                                      'protected','failed','removing','deleted')),
  error_message     TEXT,
  applied_at        TIMESTAMPTZ,
  last_verified_at  TIMESTAMPTZ,
  deleted_at        TIMESTAMPTZ,
  created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  updated_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
  UNIQUE (shield_id, name)
);

CREATE INDEX idx_resources_shield
  ON resources (shield_id)
  WHERE deleted_at IS NULL;

CREATE INDEX idx_resources_managing
  ON resources (shield_id, status)
  WHERE status IN ('managing','removing') AND deleted_at IS NULL;
```

- [ ] File created at `controller/migrations/007_resources.sql`
- [ ] `UNIQUE (shield_id, name)` constraint present
- [ ] Both partial indexes created
- [ ] `last_verified_at` column present (updated every heartbeat port check)

### 4. Create `controller/graph/resource.graphqls`

```graphql
type Resource {
  id              ID!
  name            String!
  description     String
  host            String!
  protocol        String!
  portFrom        Int!
  portTo          Int!
  status          String!
  errorMessage    String
  appliedAt       String
  lastVerifiedAt  String
  createdAt       String!
  shield          Shield         # auto-matched — null if no shield on this host
  remoteNetwork   RemoteNetwork!
}

input CreateResourceInput {
  remoteNetworkId String!
  name            String!
  description     String
  host            String!       # IP of the resource host — must match a shield's lan_ip
  protocol        String!       # "tcp", "udp", "any"
  portFrom        Int!
  portTo          Int!
}

extend type Query {
  resources(remoteNetworkId: String!): [Resource!]!
  allResources: [Resource!]!
}

extend type Mutation {
  createResource(input: CreateResourceInput!): Resource!
  protectResource(id: ID!): Resource!
  unprotectResource(id: ID!): Resource!
  deleteResource(id: ID!): Boolean!
}
```

- [ ] File created at `controller/graph/resource.graphqls`
- [ ] `shield` field is nullable (null when no shield on host)
- [ ] `createResource` input has no `shieldId` — auto-matched by Controller

### 5. Run codegen (team step)

```bash
# From repo root
buf generate

# From controller/
go generate ./graph/...
```

- [ ] `buf generate` runs cleanly
- [ ] `cd controller && go build ./...` passes (new stubs compile)
- [ ] gqlgen `generated.go` regenerated with Resource resolvers

---

## Build Check

```bash
buf generate                        # from repo root
cd controller && go build ./...     # must pass
```

---

## Notes

- `ResourceInstruction` and `ResourceAck` are new messages — no existing field numbers to worry about.
- For connector.proto, check current HeartbeatRequest max field before assigning. Current known: `lan_addr=6`, `shields=7`. Use field 8 for `resource_acks`.
- The `shield_resources` map in HeartbeatResponse is keyed by `shield_id` (UUID string) → allows Connector to look up instructions per-shield efficiently.
- `last_verified_at` is set by Controller when it processes a `ResourceAck` with `verified_at` timestamp.

---

## Related

- [[Sprint5/path.md]] — dependency map
- [[Sprint5/Member2-Go-Proto-DB/Phase2-Resource-Package]] — next phase
- [[Sprint5/Member3-Go-Controller/Phase1-Resolvers]] — unblocked after this
- [[Sprint5/Member4-Rust-Shield/Phase1-Resources-Module]] — unblocked after buf generate
