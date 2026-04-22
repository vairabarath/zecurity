---
type: task
status: done
sprint: 5
member: M1
phase: 1
priority: normal
depends_on:
  - M2-D1-D (graph/resource.graphqls committed)
  - go generate done (gqlgen regenerated)
  - npm run codegen done
unlocks:
  - Full end-to-end UI test
  - M3 must implement updateResource resolver (see M3 action below)
tags:
  - react
  - typescript
  - graphql
  - frontend
---

# M1 · Phase 1 — Resources Page + UI

---

## ⚠️ Action Required — M3 (Go Controller)

> M1 has added `updateResource` to the GraphQL schema and wired up the full frontend Edit flow.
> **M3 must implement the backend resolver and store function** before the Edit modal saves successfully.

### What M3 needs to do:

**1. `controller/graph/resource.graphqls`** — already updated by M1, no changes needed.

**2. `controller/internal/resource/store.go`** — add an `Update` function:

```go
// UpdateInput holds fields that can be changed on an existing resource.
type UpdateInput struct {
    RemoteNetworkID *string
    Name            *string
    Description     *string
    Protocol        *string
    PortFrom        *int
    PortTo          *int
}

func Update(ctx context.Context, db *pgxpool.Pool, tenantID, id string, input UpdateInput) (*Row, error) {
    // Build dynamic SET clause from non-nil fields
    // Only allow update if status = 'pending' (optional enforcement)
    // UPDATE resources SET ... WHERE id = $n AND tenant_id = $n AND deleted_at IS NULL
    // Return updated row via GetByID
}
```

**3. `controller/graph/resolvers/resource.resolvers.go`** — replace the stub:

```go
func (r *mutationResolver) UpdateResource(ctx context.Context, id string, input graph.UpdateResourceInput) (*graph.Resource, error) {
    claims := middleware.ClaimsFromCtx(ctx)
    row, err := resource.Update(ctx, r.DB, claims.TenantID, id, resource.UpdateInput{
        RemoteNetworkID: input.RemoteNetworkID,
        Name:            input.Name,
        Description:     input.Description,
        Protocol:        input.Protocol,
        PortFrom:        input.PortFrom,
        PortTo:          input.PortTo,
    })
    if err != nil {
        return nil, err
    }
    return toResourceGQL(row), nil
}
```

**4. Run build gate:** `cd controller && go build ./...` must pass.

---

## Files Created / Modified

| File | Action |
|------|--------|
| `admin/src/pages/Resources.tsx` | CREATE — global resources table with three-dot Actions dropdown |
| `admin/src/components/CreateResourceModal.tsx` | CREATE — create form (Name, Host IP, Protocol, Port From/To, Remote Network) |
| `admin/src/components/EditResourceModal.tsx` | CREATE — edit form (Remote Network, Name, Description, Protocol, Port From/To) |
| `admin/src/graphql/queries.graphql` | MODIFY — add GetAllResources, GetResources |
| `admin/src/graphql/mutations.graphql` | MODIFY — add CreateResource, UpdateResource, ProtectResource, UnprotectResource, DeleteResource |
| `controller/graph/resource.graphqls` | MODIFY — add UpdateResourceInput + updateResource mutation |
| `admin/src/App.tsx` | MODIFY — add /resources route |
| `admin/src/components/layout/Sidebar.tsx` | MODIFY — add Resources nav link |

---

## What Was Built

### Resources Table (`/resources`)

- Columns: Name, Host IP, Protocol, Port, Shield, Status, Last Verified, **Actions**
- 30s poll interval
- Status badges: `pending` (grey), `managing`/`protecting` (yellow + spinner), `protected` (green), `failed` (red), `removing` (orange + spinner), `deleted` (strikethrough)
- Shield column shows online (green wifi), offline (amber wifi-off), or "No shield" (alert icon)

### Actions — Three-dot Dropdown (⋯)

Each row has a `MoreHorizontal` icon button. Clicking opens a dropdown with:

| Option | Shown when |
|--------|-----------|
| **Edit** | resource is not `deleted` |
| **Protect** | shield online + status is `pending` or `failed` |
| **Unprotect** | status is `protected` |
| **Delete** | shield exists + status is not `deleted` (red, separated) |

In-progress states (`managing`, `protecting`, `removing`) show a spinner instead of the menu.

### Edit Modal (`EditResourceModal.tsx`)

Fields (all pre-populated from current resource):
- **Remote Network** (select, required)
- **Name** (text, required)
- **Description** (text, optional)
- **Protocol** (select: tcp / udp / any)
- **Port From** (number, 1–65535)
- **Port To** (number, ≥ Port From)

Host IP is intentionally not editable (tied to shield auto-match).

Calls `updateResource(id, input)` mutation — **requires M3 backend implementation to function**.

### Bug Fixed (store.go)

`AutoMatchShield` queried `shields` table with `AND deleted_at IS NULL` but `shields` has no `deleted_at` column — removed that condition. Shields use `status NOT IN ('revoked', 'deleted')` instead.

---

## GraphQL Schema additions (resource.graphqls)

```graphql
input UpdateResourceInput {
  remoteNetworkId: String
  name:            String
  description:     String
  protocol:        String
  portFrom:        Int
  portTo:          Int
}

# Added to Mutation:
updateResource(id: ID!, input: UpdateResourceInput!): Resource!
```

---

## Build Checks

```bash
cd admin && npm run codegen     # regenerate TS types
cd admin && npm run build       # must pass, no TypeScript errors
cd controller && go build ./... # must pass after gqlgen re-run
```

---

## Notes

- `lastVerifiedAt` shown as relative time ("2 min ago").
- Port display: single port if `portFrom === portTo`, else range `80–443`.
- `shield.lanIp` in query confirms auto-match visually.

---

## Related

- [[Sprint5/path.md]] — dependency map
- [[Sprint5/Member2-Go-Proto-DB/Phase1-Proto-Migration-Schema]] — graphqls schema this depends on
- [[Sprint5/Member3-Go-Controller/Phase1-Resolvers]] — M3 must add updateResource resolver
