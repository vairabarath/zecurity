---
type: code-study
flow: remote-network-create
created: 2026-05-05
---

# Code Study 03 — Remote Network Creation Flow

> Trace creating a Remote Network: an admin types "Production VPC", picks a location, clicks Create. One DB row inserted, list refreshes. No PKI, no email, no transactions — the simplest mutation in the system.

---

## What is a "Remote Network"?

A **logical grouping** for connectors and shields — "Production VPC", "Office HQ", "AWS us-east-1". Just a row in `remote_networks` with a name + location enum. No infrastructure is provisioned by creating one. Connectors/Shields later carry a `remote_network_id` foreign key linking them in.

URL: `/remote-networks` ([admin/src/pages/RemoteNetworks.tsx](admin/src/pages/RemoteNetworks.tsx)).

---

## High-Level Flow

```
[ADMIN]                          [BACKEND]
admin → RemoteNetworks.tsx           │
clicks "Add Network", fills form     │
  ─CreateRemoteNetwork mutation─────▶│
                                     │ resolver (no role check)
                                     │ INSERT remote_networks RETURNING ...
                                     │ scanRemoteNetwork → upcast enums
  ◀──────── { id, name, ... } ──────│
                                     │
  Apollo refetchQueries fires        │
  ─GetRemoteNetworks query──────────▶│
                                     │
  ◀──── refreshed list ──────────────│
new card renders in grid
```

---

# Stage 1 — Admin Opens Page, Clicks "Add Network"

[RemoteNetworks.tsx line 55](admin/src/pages/RemoteNetworks.tsx#L55):

```tsx
const [showComposer, setShowComposer] = useState(false)
const [name, setName] = useState('')
const [location, setLocation] = useState<NetworkLocation>(NetworkLocation.Office)

const { data, loading } = useQuery(GetRemoteNetworksDocument, {
  fetchPolicy: 'cache-and-network',
  pollInterval: 30000,
})
```

- `showComposer` — toggles the inline composer (NOT a modal — embedded in the page)
- `name`, `location` — form fields
- `pollInterval: 30000` — refetches every 30s so network health (online/offline) stays fresh without WebSockets

Click "Add Network" → `setShowComposer(true)` → composer appears.

---

# Stage 2 — Admin Fills Form, Clicks Create

Three fields:
- **Name input** — controlled `<Input>` with `onKeyDown` shortcut (Enter triggers create)
- **Location dropdown** — `Home | Office | AWS | GCP | Azure | Other`
- **Create button**

[Line 77](admin/src/pages/RemoteNetworks.tsx#L77):
```tsx
async function handleCreate() {
  if (!name.trim()) return
  await createNetwork({
    variables: { name: name.trim(), location } as CreateRemoteNetworkMutationVariables,
  })
  setName('')
  setLocation(NetworkLocation.Office)
  setShowComposer(false)
}
```

`useMutation` at [line 67](admin/src/pages/RemoteNetworks.tsx#L67):
```tsx
const [createNetwork, { loading: creating }] = useMutation(CreateRemoteNetworkDocument, {
  refetchQueries: [{ query: GetRemoteNetworksDocument }],
})
```

**`refetchQueries`** — after the mutation succeeds, Apollo automatically re-runs `GetRemoteNetworks`. Simpler than manually patching the cache.

---

# Stage 3 — Apollo Sends the Mutation

[admin/src/graphql/mutations.graphql line 8](admin/src/graphql/mutations.graphql#L8):
```graphql
mutation CreateRemoteNetwork($name: String!, $location: NetworkLocation!) {
  createRemoteNetwork(name: $name, location: $location) {
    id name location status createdAt
  }
}
```

`NetworkLocation` is a GraphQL enum sent as `"OFFICE"` etc.

Wire request through the same `errorLink → authLink → httpLink` chain:
```
POST /graphql
Authorization: Bearer <admin's JWT>
{ "operationName": "CreateRemoteNetwork", "variables": { "name": "Production VPC", "location": "OFFICE" } }
```

No `X-Public-Operation` header → server routes through `protected` chain.

---

# Stage 4 — Middleware → gqlgen → Resolver

Same chain: `routeGraphQL → AuthMiddleware → WorkspaceGuard → gqlgen`.

gqlgen dispatches to [connector.resolvers.go line 25](controller/graph/resolvers/connector.resolvers.go#L25):

```go
func (r *mutationResolver) CreateRemoteNetwork(ctx context.Context, name string, location graph.NetworkLocation) (*graph.RemoteNetwork, error) {
    tc := tenant.MustGet(ctx)

    rn, err := scanRemoteNetwork(r.TenantDB.QueryRow(ctx,
        `INSERT INTO remote_networks (tenant_id, name, location)
         VALUES ($1, $2, $3)
         RETURNING id, name, location, status, created_at`,
        tc.TenantID, name, strings.ToLower(string(location)),
    ))
    if err != nil {
        return nil, fmt.Errorf("create remote network: %w", err)
    }
    return rn, nil
}
```

Three things to notice:
- **No role check** — any authenticated workspace member can create a network. Differs from invite which is admin-only. Looks like a deliberate trust model — networks are collaborative metadata, not destructive
- **`tc.TenantID`** scopes the row — frontend cannot choose tenant
- **`strings.ToLower`** — GraphQL sends `"OFFICE"`, DB stores `"office"` (enum constraint in migration is lowercase)

Single `INSERT ... RETURNING` — no transaction needed since there's only one statement.

---

# Stage 5 — scanRemoteNetwork Materializes the Result

[helpers.go line 36](controller/graph/resolvers/helpers.go#L36):

```go
func scanRemoteNetwork(s scanner) (*graph.RemoteNetwork, error) {
    var (
        rn        graph.RemoteNetwork
        location  string
        status    string
        createdAt time.Time
    )
    if err := s.Scan(&rn.ID, &rn.Name, &location, &status, &createdAt); err != nil {
        return nil, err
    }
    rn.Location = graph.NetworkLocation(strings.ToUpper(location))
    if !rn.Location.IsValid() {
        return nil, fmt.Errorf("invalid network location: %q", location)
    }
    rn.Status = graph.RemoteNetworkStatus(strings.ToUpper(status))
    if !rn.Status.IsValid() {
        return nil, fmt.Errorf("invalid remote network status: %q", status)
    }
    rn.CreatedAt = fmtTime(createdAt)
    rn.Connectors = []*graph.Connector{}
    rn.Shields = []*graph.Shield{}
    rn.NetworkHealth = graph.NetworkHealthOffline
    return &rn, nil
}
```

### The `scanner` interface (line 32)

```go
type scanner interface { Scan(dest ...any) error }
```

Both `*pgx.Row` (from `QueryRow`) and `pgx.Rows` (from `Query`, looped) satisfy this. Lets the same helper scan single rows AND loop rows — used here for the `RETURNING` row, and elsewhere in `GetRemoteNetworks` inside `for rows.Next()`.

### Temporary variables for type-mismatched fields

| DB column | DB type | Struct type | Direct? |
|-----------|---------|-------------|---------|
| `id`, `name` | text | `string` | ✅ |
| `location` | text `"office"` | `graph.NetworkLocation` `"OFFICE"` | ❌ case mismatch |
| `status` | text `"active"` | `graph.RemoteNetworkStatus` `"ACTIVE"` | ❌ case mismatch |
| `created_at` | timestamp | `string` (ISO8601) | ❌ time→string |

The three mismatched columns scan into raw vars first, then get converted.

### Scan args order MUST match RETURNING column order

```sql
RETURNING id, name, location, status, created_at
```
maps positionally to:
```go
s.Scan(&rn.ID, &rn.Name, &location, &status, &createdAt)
```
Reordering one without the other = silent bug.

### Enum upcasing + validation

```go
rn.Location = graph.NetworkLocation(strings.ToUpper(location))
if !rn.Location.IsValid() { return nil, fmt.Errorf(...) }
```

- The cast `NetworkLocation("OFFICE")` would accept ANY string — Go doesn't validate enum values at conversion
- `.IsValid()` is auto-generated by gqlgen on every enum type — checks against known constants
- Defense in depth: if DB ever has a garbage value (migration bug, manual edit), the resolver errors loud instead of leaking a broken value to the frontend

### Time formatting

```go
rn.CreatedAt = fmtTime(createdAt)
```

`fmtTime` formats `time.Time` → ISO8601 string. GraphQL declares `createdAt: String!`, not a `DateTime` scalar, so we format on the way out.

### Defaults for relations + health

```go
rn.Connectors = []*graph.Connector{}
rn.Shields = []*graph.Shield{}
rn.NetworkHealth = graph.NetworkHealthOffline
```

Schema declares `connectors: [Connector!]!` (non-null list of non-null connectors). gqlgen errors on `nil` — must be at least empty slices. A brand-new network has zero connectors → trivially `Offline`.

The list resolver `GetRemoteNetworks` calls a separate batch query to populate connectors and runs `computeNetworkHealth` per network. The create path stubs them empty since they don't exist yet.

---

# Stage 6 — Response → Apollo Cache → UI Update

gqlgen serializes the `*graph.RemoteNetwork`:
```json
{
  "data": {
    "createRemoteNetwork": {
      "id": "01HX...",
      "name": "Production VPC",
      "location": "OFFICE",
      "status": "ACTIVE",
      "createdAt": "2026-05-05T10:23:00Z"
    }
  }
}
```

Apollo:
1. Resolves the `await createNetwork(...)` promise
2. Updates `InMemoryCache` — new `RemoteNetwork:01HX...` entity added
3. `refetchQueries` fires → `GetRemoteNetworks` re-runs → list refreshed
4. React re-renders with the new card

Back in `handleCreate`:
```tsx
setName('')
setLocation(NetworkLocation.Office)
setShowComposer(false)
```

Form resets and closes.

---

# Bonus — Delete

[connector.resolvers.go line 42](controller/graph/resolvers/connector.resolvers.go#L42):
```go
UPDATE remote_networks
   SET status = 'deleted', updated_at = NOW()
 WHERE id = $1 AND tenant_id = $2 AND status = 'active'
   AND NOT EXISTS (
       SELECT 1 FROM connectors
        WHERE remote_network_id = $1 AND tenant_id = $2
          AND status NOT IN ('pending', 'revoked')
   )
RETURNING id
```

Two protections:
- **Soft delete** — `status='deleted'`, not `DELETE FROM`. Preserves audit trail and foreign-key referents
- **`NOT EXISTS` clause** — refuses to delete if active/pending connectors still exist. UI mirrors this by disabling the trash button when `network.connectors.length > 0` ([RemoteNetworks.tsx:189](admin/src/pages/RemoteNetworks.tsx#L189))

---

# Files Touched

### Frontend
- [admin/src/pages/RemoteNetworks.tsx](admin/src/pages/RemoteNetworks.tsx) — page + composer + grid
- [admin/src/graphql/mutations.graphql](admin/src/graphql/mutations.graphql) — `CreateRemoteNetwork`, `DeleteRemoteNetwork`
- [admin/src/graphql/queries.graphql](admin/src/graphql/queries.graphql) — `GetRemoteNetworks`
- [admin/src/generated/graphql.ts](admin/src/generated/graphql.ts) — auto-generated `Document` + types
- [admin/src/apollo/links/auth.ts](admin/src/apollo/links/auth.ts) — Bearer attachment

### Backend
- [controller/graph/connector.graphqls](controller/graph/connector.graphqls) — schema declaration
- [controller/graph/resolvers/connector.resolvers.go](controller/graph/resolvers/connector.resolvers.go) — `CreateRemoteNetwork`, `DeleteRemoteNetwork` resolvers
- [controller/graph/resolvers/helpers.go](controller/graph/resolvers/helpers.go) — `scanRemoteNetwork`, `computeNetworkHealth`, `fmtTime`, `scanner` interface
- [controller/internal/middleware/auth.go](controller/internal/middleware/auth.go) — JWT verification
- [controller/internal/middleware/workspace.go](controller/internal/middleware/workspace.go) — workspace guard
- [controller/internal/tenant/context.go](controller/internal/tenant/context.go) — tenant context

### Database
- `remote_networks` table — `(id, tenant_id, name, location, status, created_at, updated_at)`

---

# Key Invariants

| Invariant | Where enforced |
|-----------|---------------|
| Network is scoped to caller's tenant | `tc.TenantID` from JWT, never from request body |
| Location must be a valid enum value (input) | GraphQL enum validation by gqlgen |
| Location must be a valid enum value (output) | `IsValid()` check in `scanRemoteNetwork` |
| DB stores location lowercase, GraphQL exposes uppercase | `strings.ToLower` on insert, `strings.ToUpper` in scan |
| Non-null list fields never return nil | `[]*Connector{}` / `[]*Shield{}` initialization |
| Deletion is soft (`status='deleted'`) | UPDATE, not DELETE |
| Cannot delete a network with active connectors | `NOT EXISTS` subquery + UI disable |
| Any workspace member can create networks | No role check in resolver |

---

# Quick-Reference Call Chain

```
RemoteNetworks.tsx → handleCreate
  → createNetwork mutation (Apollo, refetchQueries=GetRemoteNetworks)
  → main.go: routeGraphQL → protected chain
  → AuthMiddleware → WorkspaceGuard → gqlgen
  → connector.resolvers.go: CreateRemoteNetwork
      → tenant.MustGet
      → INSERT remote_networks (tenant_id, name, location='office')
      → scanRemoteNetwork
          → s.Scan(...)
          → upper-case enums + IsValid() checks
          → fmtTime
          → empty Connectors/Shields, Offline health
      → return *graph.RemoteNetwork
  → JSON response
  → Apollo cache update + refetch GetRemoteNetworks
  → handleCreate resets form
  → new card renders
```

---

## Next Flows to Study

- **Connector enrollment** — `generateConnectorToken` mutation, install command, gRPC `Enroll`, controller signs cert with workspace CA, certificate hits state.json on the connector host
- **Shield enrollment** — similar to connector but Shield enrolls into the Connector (not Controller); SPIFFE workspace CA chain
- **Resource creation + ACL push** — admin defines a resource, policy compiles, ACL snapshot pushed to connectors via heartbeat piggyback
- **Network discovery** — connector-side TCP scan returns discovered services that can be promoted to resources
