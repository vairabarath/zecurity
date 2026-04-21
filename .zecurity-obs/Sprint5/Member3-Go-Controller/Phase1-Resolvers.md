---
type: task
status: done
sprint: 5
member: M3
phase: 1
priority: normal
depends_on:
  - M2-D1-D (graph/resource.graphqls)
  - M2-A2 (resource store.go)
  - buf generate + go generate done
unlocks:
  - M1 can test mutations against real backend
tags:
  - go
  - graphql
  - resolvers
---

# M3 · Phase 1 — Resource GraphQL Resolvers

---

## Files to Create / Modify

| File | Action |
|------|--------|
| `controller/graph/resolvers/resource.resolvers.go` | CREATE |
| `controller/graph/resolvers/helpers.go` | MODIFY — add toResourceGQL() |

---

## Checklist

### 1. Create `controller/graph/resolvers/resource.resolvers.go`

#### `CreateResource(ctx, input CreateResourceInput) (*model.Resource, error)`
```go
// 1. Get tenantID from JWT claims (same pattern as other resolvers)
// 2. Call resource.CreateResource(ctx, db, input)
//    → internally calls AutoMatchShield(host) → sets shield_id
//    → returns error "no shield installed on this host" if no match
// 3. Return toResourceGQL(r)
```

#### `ProtectResource(ctx, id string) (*model.Resource, error)`
```go
// 1. Fetch resource, verify tenant ownership
// 2. Check status is "pending" or "failed" — reject if already managing/protected
// 3. resource.MarkManaging(ctx, db, id)
// 4. Return updated resource
```

#### `UnprotectResource(ctx, id string) (*model.Resource, error)`
```go
// 1. Fetch resource, verify tenant ownership
// 2. Check status is "protected" — reject if not protected
// 3. resource.MarkRemoving(ctx, db, id)
// 4. Return updated resource
// Note: Shield will remove nftables rule on next heartbeat delivery
```

#### `DeleteResource(ctx, id string) (bool, error)`
```go
// 1. Fetch resource, verify tenant ownership
// 2. If status is "protected" → MarkRemoving first, return error
//    "unprotect resource before deleting"
// 3. resource.SoftDelete(ctx, db, id)
// 4. Return true
```

#### `Resources(ctx, remoteNetworkID string) ([]*model.Resource, error)`
```go
// resource.GetByRemoteNetwork(ctx, db, remoteNetworkID)
// → map with toResourceGQL()
```

#### `AllResources(ctx) ([]*model.Resource, error)`
```go
// resource.GetAll(ctx, db, tenantID)
// → map with toResourceGQL()
```

### 2. Add `toResourceGQL()` in `helpers.go`

```go
func toResourceGQL(r resource.Resource) *model.Resource {
    return &model.Resource{
        ID:             r.ID.String(),
        Name:           r.Name,
        Description:    nullableString(r.Description),
        Host:           r.Host,
        Protocol:       r.Protocol,
        PortFrom:       r.PortFrom,
        PortTo:         r.PortTo,
        Status:         r.Status,
        ErrorMessage:   nullableString(r.ErrorMessage),
        AppliedAt:      nullableTime(r.AppliedAt),
        LastVerifiedAt: nullableTime(r.LastVerifiedAt),
        CreatedAt:      r.CreatedAt.Format(time.RFC3339),
    }
}
```

- [ ] All 6 resolver functions implemented
- [ ] `CreateResource` returns clear error if no shield on host
- [ ] `ProtectResource` rejects invalid status transitions
- [ ] `DeleteResource` blocks delete if currently protected
- [ ] `toResourceGQL` maps all fields including nullable ones

---

## Build Check

```bash
cd controller && go build ./...     # must pass
```

---

## Related

- [[Sprint5/Member3-Go-Controller/Phase2-Heartbeat-Relay]] — next phase
- [[Sprint5/Member2-Go-Proto-DB/Phase2-Resource-Package]] — depends on this
