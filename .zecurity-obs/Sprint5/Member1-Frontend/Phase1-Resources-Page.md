---
type: task
status: pending
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
tags:
  - react
  - typescript
  - graphql
  - frontend
---

# M1 · Phase 1 — Resources Page + UI

---

## Files to Create / Modify

| File                                           | Action                                     |
| ---------------------------------------------- | ------------------------------------------ |
| `admin/src/pages/Resources.tsx`                | CREATE                                     |
| `admin/src/components/CreateResourceModal.tsx` | CREATE                                     |
| `admin/src/graphql/queries.graphql`            | MODIFY — add GetAllResources, GetResources |
| `admin/src/graphql/mutations.graphql`          | MODIFY — add resource mutations            |
| `admin/src/App.tsx`                            | MODIFY — add /resources route              |
| `admin/src/components/layout/Sidebar.tsx`      | MODIFY — add Resources nav link            |

---

## Checklist

### 1. Add GraphQL operations

#### `admin/src/graphql/queries.graphql` — add:
```graphql
query GetAllResources {
  allResources {
    id
    name
    description
    host
    protocol
    portFrom
    portTo
    status
    errorMessage
    appliedAt
    lastVerifiedAt
    createdAt
    shield {
      id
      name
      status
      lanIp
    }
    remoteNetwork {
      id
      name
    }
  }
}

query GetResources($remoteNetworkId: String!) {
  resources(remoteNetworkId: $remoteNetworkId) {
    id
    name
    host
    protocol
    portFrom
    portTo
    status
    errorMessage
    lastVerifiedAt
    shield { id name status }
  }
}
```

#### `admin/src/graphql/mutations.graphql` — add:
```graphql
mutation CreateResource($input: CreateResourceInput!) {
  createResource(input: $input) {
    id name host protocol portFrom portTo status
    shield { id name }
  }
}

mutation ProtectResource($id: ID!) {
  protectResource(id: $id) { id status }
}

mutation UnprotectResource($id: ID!) {
  unprotectResource(id: $id) { id status }
}

mutation DeleteResource($id: ID!) {
  deleteResource(id: $id)
}
```

- [ ] All queries + mutations added
- [ ] Run `cd admin && npm run codegen` — generates TypeScript hooks

### 2. Create `admin/src/components/CreateResourceModal.tsx`

Form fields:
- **Name** (text, required)
- **Description** (text, optional)
- **Host IP** (text, required) — tooltip: "IP of the resource host. A shield must be installed on this machine."
- **Protocol** (select: tcp / udp / any, default: tcp)
- **Port From** (number, 1–65535)
- **Port To** (number, ≥ Port From, defaults to same as Port From)

On submit:
```tsx
createResource({ variables: { input: { remoteNetworkId, name, description, host, protocol, portFrom, portTo } } })
```

Error handling:
- Show error toast if mutation returns "no shield installed on this host"

- [ ] Modal created following existing modal patterns (see `InstallCommandModal` or similar)
- [ ] No shield selector — host IP is all that's needed
- [ ] Port To defaults to Port From value
- [ ] Error toast on "no shield on this host" response

### 3. Create `admin/src/pages/Resources.tsx`

```tsx
// Route: /resources
// useQuery(GET_ALL_RESOURCES, { pollInterval: 30000 })
```

**Table columns:**
| Column | Detail |
|--------|--------|
| Name | resource name |
| Host IP | resource.host |
| Protocol | tcp / udp / any badge |
| Port | portFrom === portTo ? portFrom : `portFrom–portTo` |
| Shield | shield.name if exists, else "No shield ⚠️" |
| Status | badge: pending / managing / protecting / protected / failed / removing |
| Last Active | lastVerifiedAt formatted, or "—" |
| Actions | context-dependent buttons (see below) |

**Action button logic:**
```tsx
// Show Protect button if:
shield != null && (status === 'pending' || status === 'failed')

// Show Unprotect button if:
status === 'protected'

// Show spinner if:
status === 'managing' || status === 'protecting' || status === 'removing'

// Show "No shield" (greyed, no button) if:
shield == null

// Show shield offline badge if:
shield != null && shield.status === 'disconnected'
```

**Status badge colors:**
- `pending` → grey
- `managing` / `protecting` → yellow + spinner
- `protected` → green
- `failed` → red + error tooltip
- `removing` → orange + spinner
- `deleted` → grey strikethrough

- [ ] `Resources.tsx` page created
- [ ] 30s poll interval
- [ ] All status badge states handled
- [ ] Protect/Unprotect buttons show/hide correctly
- [ ] "No shield" greyed out with tooltip
- [ ] Shield offline shows badge on resource row
- [ ] "Add Resource" button → opens `CreateResourceModal`
- [ ] Delete button with confirmation dialog

### 4. Modify `admin/src/App.tsx`

- [ ] Add route: `<Route path="/resources" element={<Resources />} />`
- [ ] Import `Resources` page

### 5. Modify `admin/src/components/layout/Sidebar.tsx`

- [ ] Add "Resources" nav link pointing to `/resources`
- [ ] Position: after "Shields" in nav order

---

## Build Check

```bash
cd admin && npm run build   # must pass, no TypeScript errors
```

---

## Notes

- Mirror the `AllShields.tsx` pattern for global view — same polling, same table structure.
- `lastVerifiedAt` shown as relative time ("2 min ago") or absolute — pick whichever matches existing date formatting in the codebase.
- Port display: if `portFrom === portTo` show single port, else show range `80–443`.
- The `shield.lanIp` field in the query is for display only (matches resource.host visually confirming the assignment).

---

## Related

- [[Sprint5/path.md]] — dependency map
- [[Sprint5/Member2-Go-Proto-DB/Phase1-Proto-Migration-Schema]] — graphqls schema this depends on
