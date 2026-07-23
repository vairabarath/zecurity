---
type: phase
member: M4
sprint: 15
phase: 1
title: Device Profile Admin UI
status: planned
depends_on: [Member1-Go/Phase3-GraphQL-Admin-and-ACL-Hook]
tags: [frontend, admin-ui, posture, pending-08]
---

# Phase 1 — Device Profile Admin UI

> Depends on M1's Phase 3 GraphQL schema (`posture.graphqls`). Independent of M2/M3 —
> can start as soon as the schema is stable, without waiting on client collectors or
> connector work.

## Goal

GraphQL admin mutations alone are not a usable administration surface — this was an
identified gap in the original plan, confirmed against the codebase: every existing
page (`Resources.tsx`, etc.) requires an explicit `<Route>` in `App.tsx:85-87` to be
reachable at all. Page components with no routing/navigation wiring are unreachable in
practice, not just incomplete — this phase must ship both, not just the former.

## Files

| File | Change |
|------|--------|
| `admin/src/pages/DeviceProfiles.tsx` | **new** — list view, mirrors `admin/src/pages/Resources.tsx` |
| `admin/src/components/CreateDeviceProfileModal.tsx` | **new**, mirrors `CreateResourceModal.tsx` |
| `admin/src/components/EditDeviceProfileModal.tsx` | **new**, mirrors `EditResourceModal.tsx` |
| `admin/src/pages/DeviceProfileDetail.tsx` | **new** — requirements editor, resource-binding picker, audit/enforce toggle, per-device posture visibility |
| `admin/src/graphql/*` | new queries/mutations against M1's schema |
| `admin/src/App.tsx` | **new `<Route>` entries** for `DeviceProfiles` and `DeviceProfileDetail` — pages are unreachable without this |
| navigation/sidebar config (wherever `admin/src`'s existing nav entries are declared) | new nav item linking to the Device Profiles list |

## Scope

- **List view**: all Device Profiles in the workspace, name, mode (audit/enforce), bound-resource count.
- **Create/Edit modal**: name, mode toggle.
- **Detail page**:
  - Requirements editor: add/remove `check_id` + `allow_unsupported` rows. **Source the
    check-ID options from the `supportedPostureChecks` GraphQL query (M1-E1), not a
    hardcoded frontend list** — a fixed list would drift out of sync the moment a new
    check is added server-side, requiring a synchronized frontend release just to
    expose it. Use each descriptor's `platform` field to filter/label options
    appropriately, and surface whether `allowUnsupported` is meaningful for a given check.
  - Surface the server-side rejection clearly if `removeProfileRequirement` is blocked
    because it would empty an already-enforced profile (M1's store-layer guard) — this
    is a real possible response from the mutation, not just a hypothetical.
  - Resource-binding picker: multi-select against existing resources (reuse whatever
    resource-picker component already exists for policy rule binding, if one does).
  - Audit/enforce toggle, with a visible warning when flipping to enforce (this is the
    point where the profile starts actually gating access) — the UI warning is
    supplementary; the controller (M1-E1) enforces the actual "≥1 requirement required
    to enforce" rule server-side, this page must surface that rejection clearly if hit.
  - Per-device posture visibility table: device, satisfied/unsatisfied, failure reason,
    observation time, report age, collector error — **never raw collector output**
    (the GraphQL schema already excludes it; the UI must not attempt to surface
    anything beyond what the schema exposes).
- **Routing + navigation:** add `<Route>` entries for `DeviceProfiles` (list) and
  `DeviceProfileDetail` (detail) in `App.tsx`, and a sidebar/nav entry linking to the
  list — matching how every other admin page is wired in. Without this, the pages exist
  in the bundle but no operator can reach them through the UI.
- **Permission-based visibility:** the nav entry and routes should only render/be
  reachable for users with admin-equivalent permission, consistent with how other
  admin-only pages (e.g. Resources) gate their own visibility.

## Codegen

Run `cd admin && npm run codegen` after M1's schema lands, before writing queries
against the generated types.

## Tests

- Component tests for `DeviceProfiles.tsx`, `DeviceProfileDetail.tsx`, and the create/edit modals (render, basic interaction) — not just a manual/E2E pass.
- Test: a non-admin user does not see the nav entry and the routes are not reachable for them.
- Manual/E2E: create a profile, add requirements, bind to a resource, flip to enforce,
  confirm the detail page reflects the change and per-device posture table populates
  after a client reports.
- Manual/E2E: attempt to flip an empty (zero-requirement) profile to enforce — confirm
  the controller's rejection (M1-E1) surfaces as a clear error in the UI, not a silent
  failure.

## Build Check
```bash
cd admin && npm run codegen && npm run build
```

## Implementation Checklist
- [ ] **M4-A1** `DeviceProfiles.tsx` list view.
- [ ] **M4-A2** Create/Edit modals.
- [ ] **M4-A3** `DeviceProfileDetail.tsx` — requirements editor, resource-binding picker, audit/enforce toggle, posture visibility table.
- [ ] **M4-A4** `App.tsx` routes + sidebar/nav entry + permission-gated visibility — **required for the pages to be reachable at all**.
- [ ] **M4-A5** Component tests for all new pages/modals.
- [ ] **Build gate:** `cd admin && npm run codegen && npm run build`

## Post-Phase Fixes
_None yet._
