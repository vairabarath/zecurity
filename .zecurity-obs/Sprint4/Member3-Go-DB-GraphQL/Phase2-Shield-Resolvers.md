---
type: task
status: pending
sprint: 4
member: M3
phase: 2
depends_on:
  - Phase1-DB-GraphQL-Schema (schema + codegen done)
  - "M2 Phase 2: token.go (GenerateShieldToken called from resolver)"
unlocks:
  - M1 Phase 3 (Shields page can test against live endpoints)
tags:
  - go
  - graphql
  - resolvers
---

# M3 · Phase 2 — GraphQL Resolvers

**Depends on: Day 1 schema committed + go generate run. M2's token.go must exist for GenerateShieldToken.**

---

## Goal

Implement all GraphQL resolvers for the Shield feature plus the `NetworkHealth` computation for `RemoteNetwork`.

---

## Files to Create / Modify

| File | Action |
|------|--------|
| `controller/graph/resolvers/shield.resolvers.go` | CREATE |
| `controller/graph/resolvers/connector.resolvers.go` | MODIFY (add NetworkHealth) |

---

## Checklist

### shield.resolvers.go — Mutations

- [ ] `GenerateShieldToken(ctx, remoteNetworkID, shieldName)`:
  1. Authenticate caller (JWT middleware — workspace from context)
  2. Create shield row in DB: `INSERT INTO shields (tenant_id, remote_network_id, name, status) VALUES (?, ?, ?, 'pending') RETURNING id`
  3. Call `shieldSvc.GenerateShieldToken(ctx, remoteNetworkID, workspaceID, tenantID, shieldID, shieldName)`
  4. Return `ShieldToken{ ShieldId: shieldID, InstallCommand: installCmd }`
  - **IMPORTANT:** `installCommand` is returned here and **never stored in DB**

- [ ] `RevokeShield(ctx, id)`:
  1. Verify caller owns the shield (tenant_id check)
  2. `UPDATE shields SET status='revoked' WHERE id=$1 AND tenant_id=$2`
  3. Return `true`

- [ ] `DeleteShield(ctx, id)`:
  1. Verify caller owns the shield
  2. Only allow delete for `status IN ('pending', 'revoked')` — return error for active/disconnected
  3. `DELETE FROM shields WHERE id=$1 AND tenant_id=$2`
  4. Return `true`

### shield.resolvers.go — Queries

- [ ] `Shields(ctx, remoteNetworkID)`:
  - `SELECT * FROM shields WHERE remote_network_id=$1 AND tenant_id=$2 ORDER BY created_at DESC`
  - Map DB rows to `model.Shield` structs

- [ ] `Shield(ctx, id)`:
  - `SELECT * FROM shields WHERE id=$1 AND tenant_id=$2`
  - Return `nil` if not found (not an error)

### connector.resolvers.go — NetworkHealth

- [ ] Add `NetworkHealth` field resolver on `RemoteNetwork`:
  ```go
  func (r *remoteNetworkResolver) NetworkHealth(ctx context.Context, obj *model.RemoteNetwork) (model.NetworkHealth, error) {
      // Query: SELECT COUNT(*), COUNT(CASE WHEN status='active' THEN 1 END)
      //        FROM connectors WHERE remote_network_id=$1
      // Logic:
      //   total == 0      → OFFLINE
      //   active_count > 0 → ONLINE
      //   else             → DEGRADED
  }
  ```

- [ ] Add `Shields` field resolver on `RemoteNetwork`:
  ```go
  func (r *remoteNetworkResolver) Shields(ctx context.Context, obj *model.RemoteNetwork) ([]*model.Shield, error) {
      // SELECT * FROM shields WHERE remote_network_id=$1 AND tenant_id=$2
  }
  ```

---

## Build Check

```bash
cd controller && go build ./...
# All resolver methods implemented (gqlgen will error on unimplemented)
```

---

## Notes

- `installCommand` in `GenerateShieldToken` is the entire `curl ... | sudo bash` command including the JWT. It's shown once in the UI modal and never persisted.
- The `DeleteShield` guard (only PENDING/REVOKED) prevents accidental deletion of active shields. Active shields must be revoked first.
- `NetworkHealth` is computed in the resolver (not a DB column) — this keeps it always current without requiring update triggers.

---

## Related

- [[Sprint4/Member2-Go-Proto-Shield/Phase2-Shield-Package]] — shieldSvc.GenerateShieldToken called here
- [[Sprint4/Member1-Frontend/Phase3-Shields-Page]] — consumes these resolvers
- [[Sprint4/Member1-Frontend/Phase4-RemoteNetworks-Sidebar]] — consumes NetworkHealth
