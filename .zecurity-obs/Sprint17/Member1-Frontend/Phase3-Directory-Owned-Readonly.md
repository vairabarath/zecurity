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

## Backend prerequisite — CLOSED 2026-08-26 (commit b5c1bce)
The `User` GraphQL type now exposes the provisioning provenance this phase needs (`controller/graph/schema.graphqls`), as **two deliberately separate fields**:
- `provisionedBy: String!` — **immutable** origin recorded at creation: `jit | manual | scim`.
- `provisioningOwner: String!` — **mutable** current authority over directory-owned attributes: `jit | manual | scim | unmanaged`.

Both are populated in the resolvers (`controller/graph/resolvers/schema.resolvers.go:45` and `:81`, plus `policy_helpers.go:27` for group member rows).

`unmanaged` is an explicit state, **not** inferred from `scim_enabled=false` — a user whose SCIM connection was removed is `unmanaged`, and removing a connection never deletes the user. Derive "managed by the directory" from `provisioningOwner == "scim"` and nothing else.

**No backend follow-up is required for this phase.**

## Files
| File | Change |
| --- | --- |
| `admin/src/components/users/UserOwnershipBadge.tsx` | **new** — "Managed by <provider>" badge when directory-owned. |
| `admin/src/pages/TeamUsers.tsx` (or `UserDetail`) | render the badge; disable edition of directory-owned fields. |
| `admin/src/graphql/queries.graphql` | add `User.provisioningOwner` / `provisionedBy` (after backend exposes them). |

## Steps
- [x] ~~Expose provisioning source on `User`~~ — done backend-side in b5c1bce (`provisionedBy` + `provisioningOwner`). No longer a blocker.
- [ ] Select `provisioningOwner` (and `provisionedBy` where the origin is worth showing) in the user queries; regenerate codegen.
- [ ] When `provisioningOwner == "scim"`, show "Managed by <provider>" and disable editing of directory-owned fields (email/name/title/department — note: `users` has no name/title/department columns yet per Phase 5 known gap, so today this is limited to email/status display).
- [ ] Zecurity-owned fields (role, manual group membership, policies) stay editable for directory users.

## Rules
- Read-only is derived from the backend ownership signal, never guessed from `provider`.
- Do not hide the user; show the management source transparently.

## Deferred / blocked
- **Nothing is blocked.** The `User` provisioning-source exposure landed in b5c1bce. Still true: the Phase 5 known gap means `users` lacks `name`/`title`/`department` columns, so directory-owned *attribute* editing is currently moot; the badge still applies to the row.

## Audit notes
- **2026-08-26 — backend gap CLOSED by b5c1bce.** Verified in `controller/graph/schema.graphqls` (`provisionedBy: String!`, `provisioningOwner: String!` on `User`) and in the resolvers (`schema.resolvers.go:45`, `:81`, `policy_helpers.go:27`). The "User exposes only id/email/role/provider/createdAt" finding is stale — do not re-raise it.
- Two fields, not one: `provisionedBy` is immutable origin, `provisioningOwner` is current authority. Read-only enforcement keys off `provisioningOwner == "scim"`; `unmanaged` is an explicit value and must not be inferred from `scim_enabled=false`.

## Build gate
`cd admin && npm run codegen && npm run build` green; visual: SCIM-provisioned user shows badge + disabled directory fields; manual user editable.
