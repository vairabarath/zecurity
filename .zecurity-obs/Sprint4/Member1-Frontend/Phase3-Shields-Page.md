---
type: task
status: done
sprint: 4
member: M1
phase: 3
depends_on:
  - Phase1-Layout-Routing (stub exists)
  - Phase2-GraphQL-Operations (hooks generated)
unlocks:
  - Sprint 4 frontend complete
tags:
  - frontend
  - react
  - shields
---

# M1 · Phase 3 — Shields Page (Full Implementation)

**Depends on: Phase 1 stub + Phase 2 codegen hooks.**

---

## Goal

Complete the `Shields.tsx` page with live data, token generation modal, revoke/delete actions, and 30-second auto-poll.

---

## File

`admin/src/pages/Shields.tsx`

Route: `/remote-networks/:remoteNetworkId/shields`

---

## Checklist

### Page Structure

- [ ] Load `remoteNetworkId` from route params
- [ ] Use `useGetShieldsQuery({ variables: { remoteNetworkId }, pollInterval: 30000 })`
- [ ] Show loading skeleton while fetching
- [ ] Show empty state: "No shields enrolled. Click 'Add Shield' to get started."

### Table Columns

- [ ] **Name** — shield name from DB
- [ ] **Status** — `StatusBadge` component (PENDING=gray, ACTIVE=green, DISCONNECTED=amber, REVOKED=red)
- [ ] **Interface** — `interfaceAddr` (zecurity0 IP, e.g. `100.64.0.1/32`)
- [ ] **Via Connector** — `connectorId` (show connector name if available, else ID truncated)
- [ ] **Last Seen** — `lastSeenAt` formatted as relative time (e.g. "2 minutes ago")
- [ ] **Version** — shield binary version
- [ ] **Actions** — Revoke + Delete buttons (with confirmation dialog)

### Add Shield Flow

- [ ] "Add Shield" button in page header
- [ ] Opens `InstallCommandModal` with Shield variant:
  - Input: Shield name
  - On submit: calls `useGenerateShieldTokenMutation`
  - Shows `installCommand` in a copy-to-clipboard code block
  - Warning: "Copy this command now — it cannot be shown again."
  - Modal stays open until user explicitly closes

### Actions

- [ ] **Revoke**: confirmation dialog → `useRevokeShieldMutation` → refetch
- [ ] **Delete**: confirmation dialog (only for PENDING/REVOKED) → `useDeleteShieldMutation` → refetch

### Reuse from Connectors page

- [ ] `StatusBadge` component (or equivalent)
- [ ] `InstallCommandModal` component (check if it supports a `variant="shield"` prop or needs a second instance)
- [ ] Confirmation dialog component
- [ ] Relative time formatter

---

## Build Check

```bash
cd admin && npm run build
# No TypeScript errors
# Navigate to /remote-networks/<id>/shields in dev server
# Table loads with live data from backend
```

---

## Related

- [[Sprint4/Member1-Frontend/Phase4-RemoteNetworks-Sidebar]] — parallel work
- [[Sprint4/Member2-Go-Proto-Shield/Phase2-Shield-Package]] — token.go generates the installCommand
