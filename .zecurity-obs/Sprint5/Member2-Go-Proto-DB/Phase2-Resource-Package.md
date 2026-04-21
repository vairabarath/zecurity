---
type: task
status: pending
sprint: 5
member: M2
phase: 2
priority: normal
depends_on:
  - M2-D1-C (007_resources.sql migration)
  - buf generate done
unlocks:
  - M3 resolvers (need resource store functions)
tags:
  - go
  - resource
  - db
---

# M2 · Phase 2 — Resource Package

---

## Files to Create

| File | Action |
|------|--------|
| `controller/internal/resource/config.go` | CREATE |
| `controller/internal/resource/store.go` | CREATE |

---

## Checklist

### 1. Create `controller/internal/resource/config.go`

```go
package resource

import "github.com/jackc/pgx/v5/pgxpool"

type Config struct {
    DB *pgxpool.Pool
}

func NewConfig(db *pgxpool.Pool) Config {
    return Config{DB: db}
}
```

- [ ] File created
- [ ] `Config` struct with `DB *pgxpool.Pool`
- [ ] `NewConfig()` constructor

### 2. Create `controller/internal/resource/store.go`

Implement these functions:

#### `AutoMatchShield(ctx, db, host, tenantID) (shieldID uuid.UUID, remoteNetworkID uuid.UUID, err error)`
```go
// Looks up shield by lan_ip matching resource host
// SELECT id, remote_network_id FROM shields
//   WHERE lan_ip = $1 AND tenant_id = $2 AND deleted_at IS NULL
// Returns error "no shield installed on this host" if not found
```

#### `CreateResource(ctx, db, input) (*Resource, error)`
```go
// 1. Call AutoMatchShield(host) → get shield_id + remote_network_id
// 2. INSERT INTO resources (...) VALUES (...) RETURNING *
// Returns error if host has no shield
```

#### `GetPendingForShield(ctx, db, shieldID) ([]Resource, error)`
```go
// SELECT * FROM resources
//   WHERE shield_id = $1
//     AND status IN ('managing', 'removing')
//     AND deleted_at IS NULL
```

#### `UpdateStatus(ctx, db, resourceID, status, errorMessage) error`
```go
// UPDATE resources SET status=$2, error_message=$3, updated_at=NOW()
//   WHERE id=$1
```

#### `RecordAck(ctx, db, ack ResourceAck) error`
```go
// UPDATE resources SET
//   status = $2,
//   error_message = $3,
//   last_verified_at = to_timestamp($4),  -- ack.VerifiedAt
//   applied_at = CASE WHEN $2='protected' THEN NOW() ELSE applied_at END,
//   updated_at = NOW()
// WHERE id = $1
```

#### `MarkManaging(ctx, db, resourceID) error`
```go
// UPDATE resources SET status='managing', updated_at=NOW() WHERE id=$1
```

#### `MarkRemoving(ctx, db, resourceID) error`
```go
// UPDATE resources SET status='removing', updated_at=NOW() WHERE id=$1
```

#### `SoftDelete(ctx, db, resourceID) error`
```go
// UPDATE resources SET deleted_at=NOW(), updated_at=NOW() WHERE id=$1
```

#### `GetByShield(ctx, db, shieldID) ([]Resource, error)`
```go
// SELECT * FROM resources WHERE shield_id=$1 AND deleted_at IS NULL
```

#### `GetByRemoteNetwork(ctx, db, remoteNetworkID) ([]Resource, error)`
```go
// SELECT r.*, s.lan_ip, s.name as shield_name
//   FROM resources r
//   LEFT JOIN shields s ON s.id = r.shield_id
//   WHERE r.remote_network_id=$1 AND r.deleted_at IS NULL
```

#### `GetAll(ctx, db, tenantID) ([]Resource, error)`
```go
// SELECT r.*, s.lan_ip, s.name as shield_name, rn.name as network_name
//   FROM resources r
//   LEFT JOIN shields s ON s.id = r.shield_id
//   LEFT JOIN remote_networks rn ON rn.id = r.remote_network_id
//   WHERE r.tenant_id=$1 AND r.deleted_at IS NULL
```

- [ ] All functions implemented
- [ ] `AutoMatchShield` returns descriptive error when no shield found
- [ ] `RecordAck` updates `last_verified_at` from Shield-reported timestamp
- [ ] `applied_at` set only when status transitions to `protected`

### 3. Wire into Resolver struct

In `controller/graph/resolvers/resolver.go`:
- [ ] Add `ResourceCfg resource.Config` to `Resolver` struct
- [ ] Wire in `cmd/server/main.go`: `ResourceCfg: resource.NewConfig(pool)`

---

## Build Check

```bash
cd controller && go build ./...     # must pass
```

---

## Related

- [[Sprint5/Member2-Go-Proto-DB/Phase1-Proto-Migration-Schema]] — depends on migration
- [[Sprint5/Member3-Go-Controller/Phase1-Resolvers]] — consumes this package
