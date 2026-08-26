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

## Backend: no gap. Frontend: UNBLOCKED 2026-08-26.
**Backend surface is complete** — `WorkspaceIdpConnection` already exposes `identityHealth: String!`
(Healthy | Delayed | Disconnected | Disabled) and `lastSyncAt: Time`, derived in
`DirectoryService.IdentityHealth` (path.md M1-9b). No backend gap.

**The FE-1 blocker is cleared.** Both host files now exist —
`admin/src/pages/IdpConnections.tsx` (route `/idp-connections`) and
`admin/src/pages/IdpConnectionDetail.tsx` (route `/idp-connections/:id`) — created by
[[Sprint17/Member1-Frontend/Phase1-SCIM-Connection-Config]]. Both already select `identityHealth`,
`lastSyncAt` and `scimEnabled` in the shared `GetIdpConnections` query, so this phase can mount the
badge without touching the GraphQL layer.

This phase is now buildable end to end.

## Files
| File | Change |
| --- | --- |
| `admin/src/components/scim/IdentityHealthBadge.tsx` | **new** — colored badge mapping the four states; tooltip/title shows `lastSyncAt`. |
| `admin/src/pages/IdpConnectionDetail.tsx` | embed `IdentityHealthBadge` in the connection header. |
| `admin/src/pages/IdpConnections.tsx` | show the badge per row in the connection list. |

## Steps
- [ ] Badge states: Healthy (≤24h) green, Delayed (≤72h) amber, Disconnected (>72h or null) red, Disabled grey.
- [ ] Render `lastSyncAt` ("last synced 3h ago" / "never") next to the badge.
- [ ] Only show the badge for SCIM-capable connections — gate on **`scimEnabled`** (present on `WorkspaceIdpConnection`), plus `lastSyncAt` being non-null for the "has ever synced" case. Do **not** use `scimEnabledAllowed`: it exists only on `IdpTestResult`, not on the connection type (see FE-1 Audit notes).

## Rules
- Pure presentation of the backend-derived `identityHealth`; never recompute thresholds client-side (single source of truth is `DirectoryService.IdentityHealth`).

## Build gate
`cd admin && npm run codegen && npm run build` green; visual check of all four states against seeded `lastSyncAt` values.

## Audit notes
- **2026-08-26 — FE-1 dependency cleared.** `IdpConnections.tsx` and `IdpConnectionDetail.tsx` exist and are routed; the "there is no IdP UI in the admin app at all" finding is stale.
- **2026-08-26 — `scimEnabledAllowed` correction.** The original step 3 gated visibility on `scimEnabledAllowed`, which is not a field on `WorkspaceIdpConnection` (only on `IdpTestResult`) — the step was unimplementable as written and has been rewritten to use `scimEnabled`. Do not re-raise as a backend gap: exposing `scimEnabledAllowed` on the connection was considered and deliberately rejected (it would be a constant `false` while `WithRoundTrip` has no production caller).
- Backend has never had a gap for this phase; `identityHealth` + `lastSyncAt` were complete as of M1-9b.
