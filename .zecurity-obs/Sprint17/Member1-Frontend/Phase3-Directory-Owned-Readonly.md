---
type: phase
member: M1-Frontend
sprint: 17
phase: 3
title: Directory-Owned Fields Read-Only in Admin
status: implemented-unverified
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
- [x] Select `provisioningOwner` (and `provisionedBy` where the origin is worth showing) in the user queries; regenerate codegen.
- [x] When `provisioningOwner == "scim"`, show "Managed by <provider>" and disable editing of directory-owned fields (email/name/title/department — note: `users` has no name/title/department columns yet per Phase 5 known gap, so today this is limited to email/status display).
- [x] Zecurity-owned fields (role, manual group membership, policies) stay editable for directory users.

## Rules
- Read-only is derived from the backend ownership signal, never guessed from `provider`.
- Do not hide the user; show the management source transparently.

## Deferred / blocked
- **Nothing is blocked.** The `User` provisioning-source exposure landed in b5c1bce. Still true: the Phase 5 known gap means `users` lacks `name`/`title`/`department` columns, so directory-owned *attribute* editing is currently moot; the badge still applies to the row.
- **Known gap (recorded, NOT an oversight): `UserOwnershipBadge.tsx` shipped with no unit test at FE-3 close.** FE-3's gate delta was 0 new test files / 0 new tests — the inherited `21/21 (6 files)` total came entirely from FE-2 (`IdentityHealthBadge.test.tsx`). No test step was in the original phase spec; the badge is a thin presentational `StatusPill` keyed off `provisioningOwner === "scim"` over a small provider→label map. Recorded here so a future auditor does not mistake it for missing coverage.
- **2026-08-26 — gap CLOSED post-hoc.** Added `UserOwnershipBadge.test.tsx` (6 tests): known-provider label map, non-scim owners (incl. `unmanaged`) render nothing, badge keys off `provisioningOwner` not `provider`, unknown-provider capitalize fallback, case-insensitive label resolution, StatusPill container. Full suite now **7 files / 27 tests** — the FE-3 *delta* is +1 file / +6 tests over FE-2's 6/21.

## Audit notes
- **2026-08-26 — backend gap CLOSED by b5c1bce.** Verified against code (not just the spec header): `controller/graph/schema.graphqls:80-81` exposes `provisionedBy: String!` + `provisioningOwner: String!` on `User`, and `provider: String!` (line 63) is also available for the badge label. The "User exposes only id/email/role/provider/createdAt" finding is stale — do not re-raise it.
- Two fields, not one: `provisionedBy` is immutable origin, `provisioningOwner` is current authority. Read-only enforcement keys off `provisioningOwner == "scim"`; `unmanaged` is an explicit value and must not be inferred from `scim_enabled=false` (confirmed in schema.graphqls:73).
- **2026-08-26 — implemented (unverified).** Added `provider`/`provisioningOwner`/`provisionedBy` to `GetUsers` (queries.graphql:243) and ran codegen → `GetUsersQuery` now carries all three (`graphql.ts:1302`). New `admin/src/components/users/UserOwnershipBadge.tsx`: returns null unless `provisioningOwner === "scim"`; then renders a `StatusPill` (tone `info`, the same blue as "SCIM on") labelled "Managed by {Provider}" using a small provider→label map (okta→Okta, entra→Microsoft Entra, google→Google Workspace, …) with capitalize fallback. Mounted in `TeamUsers.tsx` under each user's email in the Name cell; local `User` type extended. Gates: `codegen` regenerated (real diff), `build` green, `test` 21/21 (6 files), `lint` delta = 0 (18 pre-existing problems, byte-identical to stashed baseline; new file not flagged).
- **"Disable editing of directory-owned fields" is MOOT on the current page.** Verified: `TeamUsers.tsx` is a display-only table — there is no inline editor for email/name/role (the `MoreHorizontal` "Activity" button at line ~241 has no `onClick` handler). Combined with the Phase 5 known gap (`users` has no name/title/department columns), there is no directory-owned-attribute editor to disable. The badge is the full deliverable; no edit dialog was invented. Role/membership stay editable by design and remain so (no editor exists here anyway). If a future `UserDetail`/edit surface lands, it must key read-only off `provisioningOwner == "scim"`, not `provider`.
- **2026-08-26 — test-count provenance CORRECTION.** The gate line above reads "21/21 (6 files)". That total was reached at **FE-2** (commit 711839a added `admin/src/components/scim/IdentityHealthBadge.test.tsx` — the 6th file, +5 tests: 16→21). This phase (FE-3) added `UserOwnershipBadge.tsx` with **0 test files / 0 tests** at close — no test referenced it. The "21/21 (6 files)" passing gate is the *inherited* total at FE-3 time, not a delta this change produced. Rule for the rest of Sprint 17: report the delta your change caused, not the absolute count you inherited. Do not let a future auditor credit FE-3 with tests it did not write.
- **2026-08-26 — gap CLOSED (post-hoc, same day).** The "0 tests" above was a recorded gap, not an oversight; it has since been filled with `UserOwnershipBadge.test.tsx` (6 tests). Suite is now 7 files / 27 tests — FE-3's true *delta* over FE-2 is +1 file / +6 tests.

## Build gate
`cd admin && npm run codegen && npm run build && npm run test && npm run lint` green.

Manual check: a SCIM-provisioned user (`provisioningOwner == "scim"`) shows "Managed by <Provider>"; a `manual` / `jit` / `unmanaged` user shows no badge.

**Do not look for disabled directory-owned fields** — there are none to disable. `TeamUsers.tsx` is display-only (the row's overflow button has no handler; the only dialog is the invite flow), and per the Phase 5 known gap `users` has no `name`/`title`/`department` columns. The badge is the whole deliverable for this phase. This line previously asked a verifier to confirm disabled fields, which would have been re-raised as a gap that does not exist.
