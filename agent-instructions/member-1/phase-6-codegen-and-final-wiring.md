# Phase 6 — Codegen and Final Wiring

## Objective

Connect the frontend pages and modal to the real generated GraphQL types and documents once the backend schema is ready.

This is the final integration phase for Member 1.

---

## Prerequisites

- Member 4 schema changes merged into `controller/graph/schema.graphqls`
- Phase 1 GraphQL operation files created
- Pages/components from Phases 2–5 present

---

## Files to Create

```
None
```

## Files to Modify

```
admin/src/generated/*
admin/src/pages/RemoteNetworks.tsx
admin/src/pages/Connectors.tsx
admin/src/components/InstallCommandModal.tsx
```

---

## Implementation

The repo already has frontend codegen configured in:

- `admin/codegen.yml`

Current config:

- schema source: `../controller/graph/schema.graphqls`
- document source: `src/graphql/**/*.graphql`
- output: `src/generated/`

### Codegen step

Run:

```bash
cd admin && npm run codegen
```

### Final wiring step

After codegen succeeds:

- replace placeholder/local types with generated types
- import generated documents and types from `@/generated/graphql`
- wire `useQuery` / `useMutation` to real generated documents
- keep the existing Apollo client setup unchanged

### Existing repo pattern to follow

Match the style already used in:

- `admin/src/pages/Dashboard.tsx`
- `admin/src/pages/Settings.tsx`

Those pages already show the correct pattern for:

- `useQuery`
- generated document imports
- loading state with `Skeleton`

---

## Verification

- `npm run codegen` completes successfully
- `admin/src/generated/*` updates via codegen, not manual edits
- pages/components import generated types and documents from `@/generated/graphql`
- no placeholder local data types remain where generated types are available
- no backend files are modified during this phase

---

## Do Not Touch

- Apollo client/auth link/error link setup
- backend Go code
- generated files manually

---

## After This Phase

Member 1 frontend work for the connector sprint is complete.
