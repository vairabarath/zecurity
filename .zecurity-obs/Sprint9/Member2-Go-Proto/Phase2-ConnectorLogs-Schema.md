---
type: phase
status: pending
sprint: 9
member: M2
phase: Phase2-ConnectorLogs-Schema
depends_on:
  - Phase1-Tunnel-Proto (Day 1 buf generate done)
  - Sprint 8 DB migrations applied
tags:
  - go
  - graphql
  - migration
  - connector-logs
  - device-management
---

# M2 Phase 2 — Connector Logs + Device Management Schema

> **M1 is blocked on this phase.** Land it before M1 starts frontend work. Can run in parallel with M3-B and M4-C.

---

## What You're Building

Three things M1 needs to build the Access Log Viewer and Device Management pages:

1. **DB migration** — `connector_logs` table to store RDE access events emitted by `device_tunnel.rs`
2. **Controller handler** — receive `connector_log` ControlMessage from Connector → insert into DB
3. **GraphQL schema + resolver stubs** — `ConnectorLog`, `ClientDevice` types; `connectorLogs`, `clientDevices` queries; `revokeDevice` mutation

After you commit, run codegen so M1's types are up to date.

---

## Files to Touch

### 1. `controller/migrations/013_connector_logs.sql` (NEW)

```sql
CREATE TABLE connector_logs (
    id           UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID        NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    connector_id TEXT        NOT NULL,
    message      TEXT        NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_connector_logs_workspace ON connector_logs(workspace_id, created_at DESC);
```

---

### 2. Controller: handle `connector_log` ControlMessage (MODIFY existing handler)

In whatever file handles incoming ControlMessages from the Connector (look for the heartbeat/message dispatch), add a case for `connector_log`:

```go
case "connector_log":
    var payload struct {
        ConnectorID string `json:"connector_id"`
        Message     string `json:"message"`
    }
    if err := json.Unmarshal(msg.Payload, &payload); err != nil {
        log.Warn().Err(err).Msg("bad connector_log payload")
        continue
    }
    _, err = db.ExecContext(ctx,
        `INSERT INTO connector_logs (workspace_id, connector_id, message)
         VALUES ($1, $2, $3)`,
        workspaceID, payload.ConnectorID, payload.Message,
    )
    if err != nil {
        log.Error().Err(err).Msg("insert connector_log")
    }
```

---

### 3. `controller/graph/schema.graphqls` (MODIFY)

Add these types and operations:

```graphql
type ConnectorLog {
  id:           ID!
  workspaceId:  ID!
  connectorId:  String!
  message:      String!
  createdAt:    String!
}

type ClientDevice {
  id:         ID!
  userId:     ID!
  spiffeId:   String!
  commonName: String!
  createdAt:  String!
  revokedAt:  String
}

extend type Query {
  connectorLogs(limit: Int): [ConnectorLog!]!
  clientDevices:             [ClientDevice!]!
}

extend type Mutation {
  revokeDevice(deviceId: ID!): Boolean!
}
```

---

### 4. Resolver stubs (MODIFY `controller/graph/resolver.go` or split files)

Add resolver implementations (or stubs that return empty slices while DB work is in progress):

```go
func (r *queryResolver) ConnectorLogs(ctx context.Context, limit *int) ([]*model.ConnectorLog, error) {
    l := 100
    if limit != nil {
        l = *limit
    }
    rows, err := r.DB.QueryContext(ctx,
        `SELECT id, workspace_id, connector_id, message, created_at
           FROM connector_logs
          WHERE workspace_id = $1
          ORDER BY created_at DESC
          LIMIT $2`,
        workspaceIDFromCtx(ctx), l,
    )
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var logs []*model.ConnectorLog
    for rows.Next() {
        var cl model.ConnectorLog
        if err := rows.Scan(&cl.ID, &cl.WorkspaceID, &cl.ConnectorID, &cl.Message, &cl.CreatedAt); err != nil {
            return nil, err
        }
        logs = append(logs, &cl)
    }
    return logs, nil
}

func (r *queryResolver) ClientDevices(ctx context.Context) ([]*model.ClientDevice, error) {
    rows, err := r.DB.QueryContext(ctx,
        `SELECT id, user_id, spiffe_id, common_name, created_at, revoked_at
           FROM client_devices
          WHERE workspace_id = $1
          ORDER BY created_at DESC`,
        workspaceIDFromCtx(ctx),
    )
    if err != nil {
        return nil, err
    }
    defer rows.Close()
    var devices []*model.ClientDevice
    for rows.Next() {
        var d model.ClientDevice
        if err := rows.Scan(&d.ID, &d.UserID, &d.SpiffeID, &d.CommonName, &d.CreatedAt, &d.RevokedAt); err != nil {
            return nil, err
        }
        devices = append(devices, &d)
    }
    return devices, nil
}

func (r *mutationResolver) RevokeDevice(ctx context.Context, deviceID string) (bool, error) {
    result, err := r.DB.ExecContext(ctx,
        `UPDATE client_devices SET revoked_at = now() WHERE id = $1 AND workspace_id = $2`,
        deviceID, workspaceIDFromCtx(ctx),
    )
    if err != nil {
        return false, err
    }
    n, _ := result.RowsAffected()
    if n == 0 {
        return false, fmt.Errorf("device not found")
    }
    // Trigger CRL regeneration so Connector picks up revocation on next 5-min refresh
    r.CAService.RegenerateCRL(workspaceIDFromCtx(ctx))
    return true, nil
}
```

> **Note:** `client_devices` table was created in Sprint 8. Check the migration for exact column names. `revoked_at` is `TIMESTAMPTZ NULL`.
> **CRL regeneration:** `revokeDevice` should trigger CRL regeneration. Connector fetches `/ca.crl` on a 5-min cycle — the revoked device will be blocked on its next connection attempt.

---

## After Your Commit

Notify M1. They run:

```bash
cd controller && go generate ./graph/...
cd admin && npm run codegen
```

---

## Build Check

```bash
cd controller && go build ./...
```

Verify:
- `go generate ./graph/...` produces no errors
- `connectorLogs` and `clientDevices` queries return results from DB
- `revokeDevice` sets `revoked_at` on the `client_devices` row
