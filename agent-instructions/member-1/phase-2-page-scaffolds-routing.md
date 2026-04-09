# Phase 2 — Page Scaffolds and Routing

## Objective

Create the initial page scaffolds for the connector UI and wire them into the existing protected routing and sidebar structure.

This phase should follow the current app shell layout:

- `ProtectedLayout` in `admin/src/App.tsx`
- `AppShell` in `admin/src/components/layout/AppShell.tsx`
- child pages rendered via `<Outlet />`

---

## Prerequisites

- Phase 1 complete or in progress
- Existing admin app structure unchanged

---

## Files to Create

```
admin/src/pages/RemoteNetworks.tsx
admin/src/pages/Connectors.tsx
```

## Files to Modify

```
admin/src/App.tsx
admin/src/components/layout/Sidebar.tsx
```

---

## Implementation

### Route wiring

Update `admin/src/App.tsx` to add these protected routes:

```txt
/remote-networks
/remote-networks/:id/connectors
```

These routes must live inside the existing protected route block so they render through:

```txt
ProtectedLayout -> AppShell -> Outlet
```

Match the style already used for:

- `/dashboard`
- `/settings`

### Sidebar wiring

Update `admin/src/components/layout/Sidebar.tsx` to add:

```txt
Remote Networks
```

Place it between Dashboard and Settings.

### Page scaffolds

Create the two page files as simple initial scaffolds:

- page title
- placeholder container
- loading/empty state friendly layout

Match the visual structure already used in:

- `admin/src/pages/Dashboard.tsx`
- `admin/src/pages/Settings.tsx`

Use existing primitives such as:

- `Card`
- `Skeleton`
- `Button`
- `Badge`

Do not implement full data loading logic yet if codegen/schema is not ready. Placeholder data or lightweight local placeholder state is acceptable at this phase.

---

## Verification

- `/remote-networks` route exists in `App.tsx`
- `/remote-networks/:id/connectors` route exists in `App.tsx`
- `Sidebar.tsx` shows `Remote Networks`
- new pages render inside the existing `AppShell`
- no existing login/signup/dashboard/settings behavior is broken

---

## Do Not Touch

- Apollo client setup
- auth store
- backend files
- generated GraphQL files

---

## After This Phase

Proceed to Phase 3: build the Remote Networks page behavior.
