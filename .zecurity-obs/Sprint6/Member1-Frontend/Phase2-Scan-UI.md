---
type: phase
status: pending
sprint: 6
member: M1
phase: Phase2-Scan-UI
depends_on:
  - M1-F1 (Phase1 codegen done)
  - M2-D1-D (discovery.graphqls)
tags:
  - frontend
  - react
  - graphql
  - scan
---

# M1 Phase 2 — Connector Network Scan UI

---

## What You're Building

A "Scan Network" button on the Remote Networks page. Opens a modal where the admin enters target IPs/CIDRs and ports. Results appear in a table with a "Create Resource" button per row.

---

## Files to Touch

### 1. `admin/src/graphql/mutations.graphql` (MODIFY)

Add:

```graphql
mutation TriggerScan($connectorId: ID!, $targets: [String!]!, $ports: [Int!]!) {
  triggerScan(connectorId: $connectorId, targets: $targets, ports: $ports)
}
```

### 2. `admin/src/graphql/queries.graphql` (MODIFY)

Add:

```graphql
query GetScanResults($requestId: String!) {
  getScanResults(requestId: $requestId) {
    requestId
    ip
    port
    protocol
    serviceName
    reachableFrom
    firstSeen
  }
}
```

### 3. Run codegen

```bash
cd admin && npm run codegen
```

### 4. `admin/src/components/ScanModal.tsx` (NEW)

Props: `connectorId: string`, `onClose: () => void`

**Step 1 — Scan form:**
- Textarea: "Target IPs / CIDRs (one per line)" — placeholder: `192.168.1.0/24`
- Text input: "Ports (comma-separated)" — placeholder: `22, 80, 443, 3306`
- Validation: at least one target, at least one port, max 16 ports
- "Start Scan" button → calls `useTriggerScanMutation()` → stores returned `requestId`

**Step 2 — Results (appears after mutation succeeds):**
- Shows spinner + "Scanning…" message
- Polls `useGetScanResultsQuery({ variables: { requestId }, pollInterval: 3000 })` every 3s
- Stops polling after 60s regardless (scan timeout guard)
- Results table: IP | Port | Protocol | Service Name | Via (connector) | Action
- Each row: "Create Resource" button → navigates to Resources page with pre-filled CreateResourceModal (pass state via router or open modal with defaults)
- Empty results after polling: "No live services found in the given scope."

### 5. `admin/src/pages/RemoteNetworks.tsx` (MODIFY)

Add a "Scan Network" button to each network card (or network detail view). Each network may have multiple connectors — show a connector selector dropdown if more than one, otherwise auto-select the single connector.

Button opens `<ScanModal connectorId={selectedConnectorId} onClose={() => setScanOpen(false)} />`.

---

## UI Notes

- Parse port input: split on commas + spaces, `parseInt`, dedupe, reject non-numbers
- Parse target input: split on newlines + commas, trim whitespace, skip empty lines
- Show validation errors inline (red text below the input)
- "Start Scan" button disabled while mutation is in flight
- Poll results only while modal is open — stop on `onClose()`

---

## Build Check

```bash
cd admin && npm run build
```
