# Phase 3 — Remote Networks Page

## Objective

Build the `RemoteNetworks` page for listing, creating, and deleting remote networks using the existing admin UI patterns.

This page should visually and structurally match the current compact card-based frontend style used by the existing pages.

---

## Prerequisites

- Phase 1 complete
- Phase 2 complete
- GraphQL operation files present

---

## Files to Create

```
None
```

## Files to Modify

```
admin/src/pages/RemoteNetworks.tsx
```

---

## Implementation

Build the page around the `GetRemoteNetworks` query and the related create/delete mutations.

### Required UI behavior

- show page title
- show an `Add Network` action
- list remote networks as cards or compact sections
- each network shows:
  - name
  - location
  - status
  - connector count
  - navigation action to connectors page

### Add network UX

Add inline form or panel behavior for creating a network.

Required fields:

- `name`
- `location`

Location options must be:

- `HOME`
- `OFFICE`
- `AWS`
- `GCP`
- `AZURE`
- `OTHER`

Use the existing `DropdownMenu` primitive rather than assuming a nonexistent shared dialog component.

### Delete behavior

- allow delete only when the network has zero connectors
- call `deleteRemoteNetwork`
- clearly disable or hide delete when connectors exist

### Data/loading style

Use the same frontend patterns already present in the repo:

- `useQuery` from `@apollo/client/react`
- generated docs/types from `@/generated/graphql` once available
- `Skeleton` placeholders during loading
- compact `Card` layout
- badges for visual status/location labeling

If schema/codegen is not ready yet, local placeholder types are acceptable temporarily.

Recommended Apollo fetch policy:

```txt
cache-and-network
```

---

## Verification

- page renders a loading state using `Skeleton`
- page renders list/cards for remote networks
- location options match the allowed enum values
- add-network flow includes name and location
- delete is restricted when connector count is not zero
- page style matches the existing dashboard/settings visual pattern

---

## Do Not Touch

- backend schema or resolver files
- generated GraphQL files by hand
- auth/apollo infrastructure

---

## After This Phase

Proceed to Phase 4: connectors page.
