---
type: phase
status: done
sprint: 6
member: M1
phase: Phase1-Discovery-Tab
depends_on:
  - M2-D1-D (discovery.graphqls)
  - npm run codegen
tags:
  - frontend
  - react
  - graphql
  - discovery
---

# M1 Phase 1 — Shield Discovery Tab (Frontend)

> **Layout can start immediately on Day 1.** Only the GraphQL hook wiring needs codegen.

---

## What You're Building

A "Discovered Services" expandable panel on the Shields page — shows all services Shield found running on the host. Each row has a "Promote to Resource" button that opens a confirmation modal.

---

## Files to Touch

### 1. `admin/src/graphql/queries.graphql` (MODIFY)

Add:

```graphql
query GetDiscoveredServices($shieldId: ID!) {
  getDiscoveredServices(shieldId: $shieldId) {
    shieldId
    protocol
    port
    boundIp
    serviceName
    firstSeen
    lastSeen
  }
}
```

### 2. `admin/src/graphql/mutations.graphql` (MODIFY)

Add:

```graphql
mutation PromoteDiscoveredService($shieldId: ID!, $protocol: String!, $port: Int!) {
  promoteDiscoveredService(shieldId: $shieldId, protocol: $protocol, port: $port) {
    id
    name
    status
  }
}
```

### 3. Run codegen

```bash
cd admin && npm run codegen
```

### 4. `admin/src/components/DiscoveredServicesPanel.tsx` (NEW)

Props: `shieldId: string`

Behaviour:
- Calls `useGetDiscoveredServicesQuery({ variables: { shieldId }, pollInterval: 30000 })`
- Shows a table: Protocol | Port | Service Name | Bound IP | First Seen | Last Seen | Action
- Each row has a "Promote" button → opens `PromoteServiceModal`
- Empty state: "No services discovered yet. Shield scans every 60s."
- Loading state: skeleton rows

### 5. `admin/src/components/PromoteServiceModal.tsx` (NEW)

Props: `shieldId: string`, `protocol: string`, `port: number`, `serviceName: string`, `onClose: () => void`

Behaviour:
- Confirmation modal: "Promote **SSH (port 22/tcp)** to a resource?"
- Body text: "A new resource will be created on this shield's host, auto-matched by LAN IP. Status will be set to *pending* — click Protect to activate."
- Calls `usePromoteDiscoveredServiceMutation()` on confirm
- On success: shows success toast, calls `onClose()`
- On error: shows error message inline

### 6. `admin/src/pages/Shields.tsx` (MODIFY)

Add expandable "Discovered Services" section per shield row:

```tsx
// Below the shield row, add a collapsible panel:
<DiscoveredServicesPanel shieldId={shield.id} />
```

Use a toggle button "Show discovered services ▾" / "Hide ▴" per shield row. Panel is collapsed by default.

---

## UI Notes

- Reuse existing badge styles for `serviceName` (grey pill)
- Reuse existing table styles from `Resources.tsx`
- "Promote" button: small secondary button, same style as "Protect" on Resources page
- Auto-poll every 30s — same pattern as Shields page existing polling

---

## Build Check

```bash
cd admin && npm run build
```

---

## Post-Phase Fixes (Applied After Sprint 6)

### Fix: Added ResourceDiscovery Page
**New File Added:** `admin/src/pages/ResourceDiscovery.tsx` (367 lines)
- New dedicated page for resource discovery UI
- Displays discovered services across all shields
- Provides scan controls and result visualization

### Related Updates
- `admin/src/components/ScanModal.tsx` - Enhanced scan modal functionality
- `admin/src/components/DiscoveredServicesPanel.tsx` - New component for displaying discovered services
- `admin/src/components/PromoteServiceModal.tsx` - Modal for promoting services to resources
- `admin/src/generated/graphql.ts` - Updated GraphQL types
