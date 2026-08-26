---
type: phase
member: M1-Frontend
sprint: 17
phase: 2
title: Identity Health Indicator
status: pending
depends_on: [M1-9, FE-1]
tags: [react, admin, scim, frontend, health, pending-05]
---

# Phase 2 (FE) — Identity Health Indicator

> Depends on backend Phase 9 (connection lifecycle + health). This is the "Phase 12" health badge referenced in path.md §M1-9 (delivered backend-surface-only there). Full spec: [[PENDING-05-SCIM-Implementation-Plan]] (Frontend item 2) · [[ADR-025-SCIM-Directory-Synchronization]] §12.

## Goal
Show a sync-health badge on each SCIM-enabled IdP connection so an admin can see at a glance whether directory sync is healthy, delayed, or broken.

## Backend: no gap. Frontend: blocked on FE-1.
**Backend surface is complete** — `WorkspaceIdpConnection` already exposes `identityHealth: String!`
(Healthy | Delayed | Disconnected | Disabled) and `lastSyncAt: Time`, derived in
`DirectoryService.IdentityHealth` (path.md M1-9b). No backend gap.

**But this phase is NOT standalone-buildable.** Both files it writes into —
`IdpConnectionDetail.tsx` (connection header) and `IdpConnections.tsx` (connection list) — are
marked **new** in [[Sprint17/Member1-Frontend/Phase1-SCIM-Connection-Config]], and neither exists in
`admin/src/pages/` today. There is no IdP UI in the admin app at all. The badge component itself
(`IdentityHealthBadge.tsx`) can be written and unit-tested in isolation, but it cannot be *mounted*
until FE-1's page shells land.

**Order:** ship FE-1's unblocked half (page shells + connection queries + base URL + token panel)
first, then this phase.

## Files
| File | Change |
| --- | --- |
| `admin/src/components/scim/IdentityHealthBadge.tsx` | **new** — colored badge mapping the four states; tooltip/title shows `lastSyncAt`. |
| `admin/src/pages/IdpConnectionDetail.tsx` | embed `IdentityHealthBadge` in the connection header. |
| `admin/src/pages/IdpConnections.tsx` | show the badge per row in the connection list. |

## Steps
- [ ] Badge states: Healthy (≤24h) green, Delayed (≤72h) amber, Disconnected (>72h or null) red, Disabled grey.
- [ ] Render `lastSyncAt` ("last synced 3h ago" / "never") next to the badge.
- [ ] Only show the badge for SCIM-capable connections (hide when `scimEnabledAllowed` is false and no sync has occurred).

## Rules
- Pure presentation of the backend-derived `identityHealth`; never recompute thresholds client-side (single source of truth is `DirectoryService.IdentityHealth`).

## Build gate
`cd admin && npm run codegen && npm run build` green; visual check of all four states against seeded `lastSyncAt` values.
