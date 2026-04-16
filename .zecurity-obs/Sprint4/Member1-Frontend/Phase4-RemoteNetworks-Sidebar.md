---
type: task
status: pending
sprint: 4
member: M1
phase: 4
depends_on:
  - Phase2-GraphQL-Operations (NetworkHealth enum generated)
unlocks:
  - Sprint 4 frontend complete
tags:
  - frontend
  - react
  - network-health
---

# M1 · Phase 4 — RemoteNetworks Update + Sidebar

**Can run in parallel with Phase 3. Depends on codegen for NetworkHealth enum.**

---

## Goal

Add NetworkHealth indicator to the RemoteNetworks page and Shield count summary. Add Shields nav link to Sidebar.

---

## Files to Modify

| File | Action |
|------|--------|
| `admin/src/pages/RemoteNetworks.tsx` | Add NetworkHealth + shield count |
| `admin/src/components/layout/Sidebar.tsx` | Confirm Shields link (done in Phase 1) |

---

## Checklist

### RemoteNetworks.tsx

- [ ] Add `networkHealth` and `shields` fields to `GetRemoteNetworks` query (or create a new query variant)
- [ ] Run codegen to pick up `NetworkHealth` enum and `shields` field on `RemoteNetwork`
- [ ] Add NetworkHealth indicator next to each network name:
  - `ONLINE` → green dot or "🟢 Online" label (≥1 connector ACTIVE)
  - `DEGRADED` → amber dot or "🟡 Degraded" (connectors exist but none ACTIVE)
  - `OFFLINE` → red dot or "🔴 Offline" (no connectors)
- [ ] Add Shield count to each network card:
  ```
  "2 / 3 connectors active · 4 shields active"
  ```
  - Active connectors: count from `connectors` where `status == ACTIVE`
  - Active shields: count from `shields` where `status == ACTIVE`

### Sidebar.tsx

- [ ] Confirm "Shields" nav link is under "Connectors" (done in Phase 1)
- [ ] If network context is available, link to `/remote-networks/:id/shields`
- [ ] If no network selected, disable or show unlinked

---

## NetworkHealth Logic (Frontend)

The `networkHealth` field is computed server-side in the GraphQL resolver (Member 3). The frontend just renders it:

```typescript
const healthConfig = {
  ONLINE:   { label: 'Online',   color: 'green' },
  DEGRADED: { label: 'Degraded', color: 'amber' },
  OFFLINE:  { label: 'Offline',  color: 'red'   },
}
```

---

## Build Check

```bash
cd admin && npm run build
# NetworkHealth indicator renders on RemoteNetworks page
# Shield count visible on each network card
```

---

## Related

- [[Sprint4/Member3-Go-DB-GraphQL/Phase2-Shield-Resolvers]] — NetworkHealth computed in connector.resolvers.go
- [[Sprint4/Member1-Frontend/Phase3-Shields-Page]] — parallel work
