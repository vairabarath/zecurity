---
type: task
status: done
sprint: 4
member: M1
phase: 1
completed: 2026-04-17
commit: deb908d
depends_on: []
unlocks:
  - Phase2-GraphQL-Operations
  - Phase3-Shields-Page
  - Phase4-RemoteNetworks-Sidebar
tags:
  - frontend
  - react
  - routing
---

# M1 · Phase 1 — Layout & Routing Scaffold

**Can start immediately — no backend dependency.**

---

## Goal

Set up all routing, page stubs, and loading states for the Shields feature before the backend is wired. This unblocks all subsequent M1 phases.

---

## Files to Create / Modify

| File | Action | Notes |
|------|--------|-------|
| `admin/src/pages/Shields.tsx` | CREATE | Page stub with skeleton/loading state |
| `admin/src/components/layout/Sidebar.tsx` | MODIFY | Add Shields nav link |
| `admin/src/App.tsx` (or router file) | MODIFY | Add `/remote-networks/:id/shields` route |

---

## Checklist

- [x] Create `admin/src/pages/Shields.tsx` as a stub:
  - Route: `/remote-networks/:id/shields`
  - Skeleton table with column headers: Name, Status, Interface, Via Connector, Last Seen, Version
  - "Add Shield" button (opens modal placeholder)
  - Loading spinner component reused from Connectors page
- [x] Add "Shields" nav link to `admin/src/components/layout/Sidebar.tsx`
  - Place it under "Connectors" in the sidebar
  - Icon: use a shield or lock icon consistent with existing icon set
  - Link: `/remote-networks` (sidebar has no network context — deep-link arrives in Phase 4)
- [x] Add route in router config for `Shields` page
- [x] Verify: `cd admin && npm run dev` starts without errors and Shields page renders (empty state OK)

---

## Status Badges

Same color scheme as Connectors (reuse the `StatusBadge` component if it exists):

| Status | Color |
|--------|-------|
| PENDING | Gray |
| ACTIVE | Green |
| DISCONNECTED | Amber |
| REVOKED | Red |

---

## Build Check

```bash
cd admin && npm run dev
# Navigate to /remote-networks/<any-id>/shields
# Should render without console errors
```

---

## What This Unlocks

- Phase 2 (GraphQL operations) can be merged into this stub once codegen runs
- Phase 3 (full Shields page) builds on this scaffold
- Phase 4 (RemoteNetworks + Sidebar) can proceed in parallel with this phase

---

## Related

- [[Sprint4/path.md]] — full dependency map
- [[Sprint4/Member3-Go-DB-GraphQL/Phase1-DB-GraphQL-Schema]] — schema that unblocks codegen
