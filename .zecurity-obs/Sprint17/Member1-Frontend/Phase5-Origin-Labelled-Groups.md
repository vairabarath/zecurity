---
type: phase
member: M1-Frontend
sprint: 17
phase: 5
title: Origin-Labelled Groups
status: pending
depends_on: [M1-7]
tags: [react, admin, scim, frontend, groups, pending-05]
---

# Phase 5 (FE) — Origin-Labelled Groups

> Depends on backend Phase 7 (SCIM groups + origin). Full spec: [[PENDING-05-SCIM-Implementation-Plan]] (Frontend item 5) · [[ADR-025-SCIM-Directory-Synchronization]] §7.

## Goal
In group listings/detail, always show the group's origin so admins never mistake a directory-pushed group for a local one: `Engineering · SCIM` / `· Local` / `· System`. Display-name alone is forbidden.

## Backend prerequisite / gap (MUST close first)
The `Group` GraphQL type (graph/policy.graphqls) exposes only `id/name/description/members/resources/createdAt/updatedAt`. It does **not** expose `origin` (or `connectionId`/`externalId`), even though the `groups` table carries `origin` (`manual`/`scim`/`system`) and `external_id` (per migration 034/035). The UI cannot label origin without this field.

## Files
| File | Change |
| --- | --- |
| `admin/src/components/groups/GroupOriginLabel.tsx` | **new** — renders `SCIM` / `Local` / `System` suffix. |
| `admin/src/pages/Groups.tsx` + `GroupDetail.tsx` | show origin label next to every group name. |
| `admin/src/graphql/queries.graphql` | add `Group.origin` (after backend exposes it). |

## Steps
- [ ] Expose `origin` on `Group` (backend gap — block).
- [ ] Render `name · <Origin>` everywhere a group name appears (list, detail, policy assignment, member rows).
- [ ] SCIM-origin groups may additionally show the source connection (e.g. `Engineering · SCIM (Okta)`) when `connectionId` is also exposed.

## Rules
- Never display a bare group name without its origin suffix.
- Origin is read-only (directory owns SCIM group names/metadata; Zecurity owns manual groups).

## Deferred / blocked
- Blocked on the backend `Group.origin` exposure (documented above).

## Build gate
`cd admin && npm run codegen && npm run build` green; visual: SCIM/Local/System groups each labelled; no bare display names.
