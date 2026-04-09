# Phase 3 — Connector Resolvers

Implement Go-side GraphQL resolvers for remote networks and connectors. All resolvers use tenant-scoped queries (explicit `tenant_id` in WHERE clauses).

---

## File to Create

```
controller/graph/resolvers/connector.resolvers.go
```

---

## Query Resolvers

### `remoteNetworks`

```sql
SELECT * FROM remote_networks WHERE tenant_id = $1 AND status != 'deleted'
```

### `remoteNetwork(id)`

```sql
SELECT * FROM remote_networks WHERE id = $1 AND tenant_id = $2
```

### `connectors(remoteNetworkId)`

```sql
SELECT * FROM connectors WHERE remote_network_id = $1 AND tenant_id = $2
```

### `RemoteNetwork.connectors`

Nested resolver — loads connectors for a network:

```sql
SELECT * FROM connectors WHERE remote_network_id = $1 AND tenant_id = $2
```

---

## Mutation Resolvers

### `createRemoteNetwork`

INSERT into `remote_networks` with `tenant_id` from context.

### `deleteRemoteNetwork`

Soft delete (set `status='deleted'`), only if zero non-deleted connectors exist.

### `generateConnectorToken`

1. INSERT connector row (status='pending')
2. Call Member 2's `GenerateEnrollmentToken` from `controller/internal/connector/token.go`
3. Build install command string
4. Return `ConnectorToken`

**Install command format:**

```
curl -fsSL https://github.com/yourorg/zecurity/releases/latest/download/connector-install.sh | \
  sudo CONTROLLER_ADDR=<controller_host>:<grpc_port> ENROLLMENT_TOKEN=<jwt> bash
```

### `revokeConnector`

```sql
UPDATE connectors SET status='revoked' WHERE id = $1 AND tenant_id = $2
```

### `deleteConnector`

```sql
DELETE FROM connectors WHERE id = $1 AND tenant_id = $2 AND status IN ('pending', 'revoked')
```

---

## Field Resolvers (Enum Conversions)

- `Connector.status` → map DB lowercase to GraphQL enum
- `RemoteNetwork.status` → map DB lowercase to GraphQL enum
- `RemoteNetwork.location` → map DB lowercase to GraphQL enum
- Timestamp fields → RFC3339 string format

Follow the same patterns as existing resolvers in `schema.resolvers.go` for enum conversion and time formatting.

---

## Important Rules

1. **Every query includes explicit `tenant_id` conditions.** No implicit tenant scoping.
2. **Follow existing resolver patterns** from `schema.resolvers.go`.
3. **Coordinate with Member 2** on the `GenerateEnrollmentToken` function signature.

---

## Phase 3 Checklist

```
✓ connector.resolvers.go created
✓ All query resolvers implemented with tenant_id scoping
✓ All mutation resolvers implemented with tenant_id scoping
✓ Enum conversion field resolvers added
✓ Timestamp formatting matches RFC3339
✓ generateConnectorToken calls Member 2's token function
✓ Install command string built correctly
✓ Committed and pushed
```

---

## After This Phase

Then proceed to Phase 4 (Rust connector foundation).
