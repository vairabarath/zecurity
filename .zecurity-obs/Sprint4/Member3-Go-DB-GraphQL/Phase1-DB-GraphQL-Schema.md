---
type: task
status: pending
sprint: 4
member: M3
phase: 1
priority: DAY1-CRITICAL
depends_on: []
unlocks:
  - M1 codegen (shield.graphqls + connector.graphqls)
  - M2 token.go DB calls (003_shield_schema.sql)
  - M3 Phase 2 resolvers
tags:
  - go
  - sql
  - graphql
  - day1
---

# M3 · Phase 1 — DB Migration + GraphQL Schema (DAY 1 — COMMIT FIRST)

> **This is the second critical Day 1 commit** (alongside M2's proto files).
> The GraphQL schema unblocks M1's codegen. The DB migration unblocks M2's token.go.
> Commit all three files together.

---

## Files to Create / Modify

| File | Action |
|------|--------|
| `controller/migrations/003_shield_schema.sql` | CREATE |
| `controller/graph/shield.graphqls` | CREATE |
| `controller/graph/connector.graphqls` | MODIFY |

---

## Checklist

### 003_shield_schema.sql

- [ ] Create `shields` table with columns:
  - `id UUID PRIMARY KEY DEFAULT gen_random_uuid()`
  - `tenant_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE`
  - `remote_network_id UUID NOT NULL REFERENCES remote_networks(id) ON DELETE CASCADE`
  - `connector_id UUID NOT NULL REFERENCES connectors(id) ON DELETE CASCADE`
  - `name TEXT NOT NULL`
  - `status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','active','disconnected','revoked'))`
  - `enrollment_token_jti TEXT`
  - `trust_domain TEXT`
  - `interface_addr TEXT`
  - `cert_serial TEXT`
  - `cert_not_after TIMESTAMPTZ`
  - `last_heartbeat_at TIMESTAMPTZ`
  - `version TEXT`
  - `hostname TEXT`
  - `public_ip TEXT`
  - `created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`
  - `updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()`
- [ ] Create indexes:
  - `idx_shields_tenant ON shields (tenant_id)`
  - `idx_shields_remote_network ON shields (remote_network_id, tenant_id)`
  - `idx_shields_connector ON shields (connector_id)`
  - `idx_shields_token_jti ON shields (enrollment_token_jti)`
  - `idx_shields_trust_domain ON shields (trust_domain)`
  - `UNIQUE INDEX idx_shields_interface_addr ON shields (tenant_id, interface_addr) WHERE interface_addr IS NOT NULL`
- [ ] Test: `psql $DATABASE_URL -f migrations/003_shield_schema.sql` on clean DB — no errors

### shield.graphqls (NEW)

- [ ] Create `controller/graph/shield.graphqls`
- [ ] Define `Shield` type with all fields: `id`, `name`, `status`, `remoteNetworkId`, `connectorId`, `lastSeenAt`, `version`, `hostname`, `publicIp`, `interfaceAddr`, `certNotAfter`, `createdAt`
- [ ] Define `ShieldStatus` enum: `PENDING`, `ACTIVE`, `DISCONNECTED`, `REVOKED`
- [ ] Define `ShieldToken` type: `shieldId`, `installCommand`
- [ ] Extend `Mutation` type:
  - `generateShieldToken(remoteNetworkId: ID!, shieldName: String!): ShieldToken!`
  - `revokeShield(id: ID!): Boolean!`
  - `deleteShield(id: ID!): Boolean!`
- [ ] Extend `Query` type:
  - `shields(remoteNetworkId: ID!): [Shield!]!`
  - `shield(id: ID!): Shield`

### connector.graphqls (MODIFY)

- [ ] Add `NetworkHealth` enum:
  ```graphql
  enum NetworkHealth {
    ONLINE     # ≥1 connector ACTIVE
    DEGRADED   # connectors exist but none ACTIVE
    OFFLINE    # no connectors at all
  }
  ```
- [ ] Modify `RemoteNetwork` type — add two fields:
  - `networkHealth: NetworkHealth!`
  - `shields: [Shield!]!`
- [ ] **Do NOT remove existing fields** on `RemoteNetwork`

### Run Go codegen

- [ ] Run: `cd controller && go generate ./graph/...`
- [ ] Verify `controller/graph/generated.go` updated without errors
- [ ] Stub resolver methods generated for Shield mutations/queries
- [ ] `cd controller && go build ./...` must pass (even with empty resolver stubs)

---

## Build Check

```bash
# DB migration
psql $DATABASE_URL -f controller/migrations/003_shield_schema.sql

# GraphQL codegen
cd controller && go generate ./graph/...
cd controller && go build ./...

# Frontend codegen (run after M2 + M3 Day 1 both committed)
cd admin && npm run codegen
```

---

## Notes

- `interface_addr` has a partial UNIQUE index (`WHERE interface_addr IS NOT NULL`) — allows multiple NULLs but enforces uniqueness once assigned.
- The `shields` field on `RemoteNetwork` in GraphQL is computed by the resolver — it's not a DB join in the migration.
- `ShieldToken.installCommand` is returned once and never stored in the DB. The resolver generates it and returns it directly.

---

## Related

- [[Sprint4/Member3-Go-DB-GraphQL/Phase2-Shield-Resolvers]] — implements the resolver stubs
- [[Sprint4/Member2-Go-Proto-Shield/Phase2-Shield-Package]] — uses shields table for token.go
- [[Sprint4/Member1-Frontend/Phase2-GraphQL-Operations]] — consumes this schema via codegen
