---
type: phase
member: M1-Frontend
sprint: 17
phase: 3
title: Directory-Owned Fields Read-Only in Admin
status: pending
depends_on: [M1-5]
tags: [react, admin, scim, frontend, ownership, pending-05]
---

# Phase 3 (FE) — Directory-Owned Fields Read-Only in Admin

> Depends on backend Phase 5 (provision/update + ownership). Full spec: [[PENDING-05-SCIM-Implementation-Plan]] (Frontend item 3) · [[ADR-025-SCIM-Directory-Synchronization]] §5 (directory owns directory attrs; Zecurity owns role/policies/manual groups).

## Goal
On user/team views, mark directory-provisioned users' directory-owned attributes as read-only and label them "Managed by Google Workspace / Microsoft Entra / <provider>" so admins don't try to edit fields the IdP owns.

## Backend prerequisite / gap (MUST close first)
The `User` GraphQL type exposes only `id/email/role/provider/createdAt` (graph/schema.graphqls). There is **no provisioning-source signal** (`provisioned_by`, `provisioning_owner`, or a derived `managedByDirectory` boolean) exposed to the frontend. Without it the UI cannot tell a SCIM-provisioned user from a JIT/manual one, so read-only enforcement is impossible. This needs a small backend addition (expose e.g. `provisioningOwner` / `provisionedBy` on `User`, or a `directoryManaged: Boolean!`).

## Files
| File | Change |
| --- | --- |
| `admin/src/components/users/UserOwnershipBadge.tsx` | **new** — "Managed by <provider>" badge when directory-owned. |
| `admin/src/pages/TeamUsers.tsx` (or `UserDetail`) | render the badge; disable edition of directory-owned fields. |
| `admin/src/graphql/queries.graphql` | add `User.provisioningOwner` / `provisionedBy` (after backend exposes them). |

## Steps
- [ ] Expose provisioning source on `User` (backend gap — block).
- [ ] When `provisioningOwner == "scim"`, show "Managed by <provider>" and disable editing of directory-owned fields (email/name/title/department — note: `users` has no name/title/department columns yet per Phase 5 known gap, so today this is limited to email/status display).
- [ ] Zecurity-owned fields (role, manual group membership, policies) stay editable for directory users.

## Rules
- Read-only is derived from the backend ownership signal, never guessed from `provider`.
- Do not hide the user; show the management source transparently.

## Deferred / blocked
- Blocked on the backend `User` provisioning-source exposure (documented above). Also note the Phase 5 known gap: `users` lacks `name`/`title`/`department` columns, so directory-owned *attribute* editing is currently moot; the badge still applies to the row.

## Build gate
`cd admin && npm run codegen && npm run build` green; visual: SCIM-provisioned user shows badge + disabled directory fields; manual user editable.
