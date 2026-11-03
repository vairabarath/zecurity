---
type: phase
member: M1-Frontend
sprint: 17
phase: 5
title: Origin-Labelled Groups
status: implemented-unverified
depends_on: [M1-7]
tags: [react, admin, scim, frontend, groups, pending-05]
---

# Phase 5 (FE) — Origin-Labelled Groups

> Depends on backend Phase 7 (SCIM groups + origin). Full spec: [[PENDING-05-SCIM-Implementation-Plan]] (Frontend item 5) · [[ADR-025-SCIM-Directory-Synchronization]] §7.

## Goal
In group listings/detail, always show the group's origin so admins never mistake a directory-pushed group for a local one: `Engineering · SCIM` / `· Local` / `· System`. Display-name alone is forbidden.

## Backend prerequisite — CLOSED 2026-08-26 (commit b5c1bce)
The `Group` GraphQL type (`controller/graph/policy.graphqls`) now exposes the provenance this phase needs:
- `origin: String!` — `manual` | `scim` | `system`. Read-only; the directory owns scim-origin groups.
- `externalId: String` — SCIM group id for scim-origin groups; null for manual/system.
- `connectionId: String` — the source connection, which makes the `Engineering · SCIM (Okta)` form possible.

Resolved in `controller/graph/resolvers/policy_helpers.go:99` via `originOrManual(row.Origin)` (origin is NOT NULL in the DB; the helper defaults defensively). The `Group` model carries all three (`controller/graph/models_gen.go:120-131`).

**No backend follow-up is required for this phase.**

## Files
| File | Change |
| --- | --- |
| `admin/src/components/groups/GroupOriginLabel.tsx` | **new** — renders `SCIM` / `Local` / `System` suffix. |
| `admin/src/pages/Groups.tsx` + `GroupDetail.tsx` | show origin label next to every group name. |
| `admin/src/graphql/queries.graphql` | add `Group.origin` (after backend exposes it). |

## Steps
- [x] ~~Expose `origin` on `Group`~~ — done backend-side in b5c1bce (`origin`, `externalId`, `connectionId`). No longer a blocker.
- [ ] Select `origin` (and `connectionId` for the source-connection form) in `GetGroups` / group-detail queries; regenerate codegen.
- [ ] Render `name · <Origin>` everywhere a group name appears (list, detail, policy assignment, member rows).
- [ ] SCIM-origin groups may additionally show the source connection (e.g. `Engineering · SCIM (Okta)`) — `connectionId` is exposed, so resolve it to a display name via `idpConnections` (FE-1's `GetIdpConnections` already fetches `displayName`).

## Rules
- Never display a bare group name without its origin suffix.
- Origin is read-only (directory owns SCIM group names/metadata; Zecurity owns manual groups).

## Deferred / blocked
- **Nothing is blocked.** The `Group.origin` exposure landed in b5c1bce.

## Audit notes
- **2026-08-26 — backend gap CLOSED by b5c1bce.** Verified in `controller/graph/policy.graphqls` (`origin: String!`, `externalId: String`, `connectionId: String` on `Group`), `controller/graph/models_gen.go:120-131`, and the resolver `controller/graph/resolvers/policy_helpers.go:99`. The "exposes only id/name/description/members/resources/createdAt/updatedAt" finding is stale — do not re-raise it.
- **2026-08-26 — implemented (unverified).** Added `admin/src/components/groups/GroupOriginLabel.tsx` (pure presentational: `name · Local|System|SCIM`, plus `(connectionName)` for scim-origin when a resolved name is supplied — no Apollo query inside, so it stays testable) and `GroupOriginLabel.test.tsx` (6 tests). Wrapped every group-name render site: `Groups.tsx` (list row + delete-dialog confirm), `GroupDetail.tsx` (breadcrumb + title), `Resources.tsx` (protected-resource group pills, gated via `GetAllResources.groups` now selecting `origin`/`connectionId`). Each consumer fetches `GetIdpConnections` once (cache-and-network) and passes `connectionName` into the label — no redundant per-row lookups. `GetGroups` / `GetGroup` / `GetAllResources.groups` now select `origin` + `connectionId`; codegen regenerated. Gates: `codegen` + `build` (tsc -b) green, `test` **9 files / 46 tests** (FE-5 delta = **+1 file / +6 tests** — prior was 8/40 from FE-4), `lint` delta = 0. No secrets, no mutations, no backend changes — FE-1 `no-cache` rule does not apply (read-only display).
- **Manual gate NOT run.** Per the build-gate, acceptance needs a visual pass: SCIM/Local/System groups each labelled with no bare display names, and `Engineering · SCIM (Okta)` resolving when a SCIM group's `connectionId` matches an `idpConnections` entry. Marked `implemented-unverified` (not `done`) until that visual pass runs.

## Build gate
`cd admin && npm run codegen && npm run build` green; visual: SCIM/Local/System groups each labelled; no bare display names.
