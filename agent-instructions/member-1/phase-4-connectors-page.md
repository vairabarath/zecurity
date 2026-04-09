# Phase 4 — Connectors Page

## Objective

Build the connectors page for a selected remote network, using the existing routing structure and Apollo patterns already present in the admin app.

---

## Prerequisites

- Phase 2 complete
- Phase 5 modal file planned or scaffolded
- GraphQL operation files present

---

## Files to Create

```
None
```

## Files to Modify

```
admin/src/pages/Connectors.tsx
```

---

## Implementation

The route is:

```txt
/remote-networks/:id/connectors
```

Read the route parameter using `react-router-dom`.

### Required page behavior

- show the selected network context in the page header
- include breadcrumb-style context:
  - Remote Networks
  - current network
- include `Add Connector` action
- show connector list in table-like or row-based layout

### Required connector fields

- name
- status
- last seen
- hostname
- version

Optional fields in detail area if needed:

- public IP
- certificate expiry

### Status display

Map statuses to clear visual badges:

- `PENDING` -> gray
- `ACTIVE` -> green
- `DISCONNECTED` -> amber
- `REVOKED` -> red

### Actions

- revoke connector
- delete connector

### Data behavior

Use Apollo polling for connector freshness:

```txt
pollInterval: 30000
```

Do not add WebSocket logic.

Use the same data access style as existing pages:

- `useQuery`
- `useMutation`
- generated docs/types from `@/generated/graphql` once available

Use placeholder/local types until codegen is available if needed.

---

## Verification

- route param is used correctly
- connectors render in a stable list/table layout
- status badges map correctly
- revoke and delete actions are present
- Apollo poll interval is 30000 once real query wiring is added
- page matches existing app shell and page spacing patterns

---

## Do Not Touch

- Apollo client setup
- backend files
- generated files by hand

---

## After This Phase

Proceed to Phase 5: install command modal.
