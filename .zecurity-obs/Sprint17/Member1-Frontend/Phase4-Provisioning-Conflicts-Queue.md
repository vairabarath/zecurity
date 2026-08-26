---
type: phase
member: M1-Frontend
sprint: 17
phase: 4
title: Provisioning-Conflicts Queue
status: pending
depends_on: [M1-8]
tags: [react, admin, scim, frontend, conflict, pending-05]
---

# Phase 4 (FE) — Provisioning-Conflicts Queue

> Depends on backend Phase 8 (identity conflict workflow). Full spec: [[PENDING-05-SCIM-Implementation-Plan]] (Frontend item 4) · [[ADR-025-SCIM-Directory-Synchronization]] §4.1/§9.

## Goal
Surface the SCIM provisioning-conflicts queue for a connection and let an admin Accept-Link (link directory claim to existing user), Reject, or Reopen — each requiring a reason; Accept additionally requires the `identity.mapping.break_glass` permission (ADMIN alone is denied — `DirectoryService.AcceptLink` returns `403` server-side). The denial reaches the client as `extensions.code === "FORBIDDEN"` with the server's message intact — see gap 3 below.

## Feasible now (core surface exists) — with three known data gaps
- Query `scimConflicts(connectionId: ID!): [ScimConflict!]!` (admin-scoped).
- Mutations `acceptScimConflict` / `rejectScimConflict` / `reopenScimConflict` (each take `connectionId`, `canonicalKey`, `reason`).
- `ScimConflict` exposes `id/workspaceId/connectionId/userId/canonicalKey/scimExternalId/status/resolutionReason/createdAt/resolvedAt`.
- Break-glass enforcement is real and server-side: `DirectoryService.AcceptLink` (`internal/scim/conflict.go`) does an explicit `HasPermission(workspace, actor, identity.mapping.break_glass)` check and returns `403` when absent — ADMIN role alone is insufficient, exactly as ADR-025 §3.2 requires.

**This phase is standalone-buildable** — ship `ScimConflicts.tsx` as its own page rather than a panel
in `IdpConnectionDetail.tsx`, which does not exist yet (see
[[Sprint17/Member1-Frontend/Phase1-SCIM-Connection-Config]] ordering note).

### Known gaps (do not treat this phase as gap-free)

1. **No human-readable context on a conflict row (backend GraphQL gap).**
   Migration 034 stores `scim_username_snapshot` and `scim_email_snapshot` on
   `scim_identity_conflicts` — the columns exist *precisely* for this screen (ADR-025 §4.1 lists them
   as required conflict-record fields) — but **neither is exposed on the GraphQL `ScimConflict`
   type**. With only `userId: ID!`, the queue renders raw UUIDs and an admin has no way to tell which
   human a conflict is about. **Backend follow-up: expose `scimUsernameSnapshot` /
   `scimEmailSnapshot` (and ideally resolve `userId` → `User`).**
   This does **not** conflict with the "never show email as the identity key" rule below: a snapshot
   shown as *context* is not the same as email used as a *matching key*.

2. **`resolutionReason` — FIXED 2026-08-26.**
   Migration `034_scim_directory_sync.sql` now carries the column (in the `CREATE TABLE` **and** a matching `ALTER TABLE ... ADD COLUMN IF NOT EXISTS`, so databases that already ran 034 pick it up on re-apply), and
   Accept-Link / Reject now persist the mandatory reason on the row (both read paths select it).
   **Render the Reason column — it carries data.** One nuance: **Reopen clears it**, along with
   `resolved_at`/`resolved_by`, because a row returned to `pending` has no resolution to describe.
   The reopen reason is not lost — `scim.user.conflict_reopened` carries it in the same transaction.
   So expect a reason on `approved`/`rejected` rows and none on `pending` ones.

3. **The `403` is machine-readable — FIXED 2026-08-26.**
   `ErrorPresenter` (`controller/graph/resolvers/presenter.go`) now recognises `scim.SCIMError` and
   surfaces the **client-actionable** ones verbatim with structured extensions:

   ```
   extensions.code    BAD_REQUEST | FORBIDDEN | NOT_FOUND | CONFLICT
   extensions.status  400 | 403 | 404 | 409
   extensions.scimType  RFC 7644 type, when set (e.g. "identity_conflict")
   ```

   **Branch on `extensions.code`, never on the message.** A break-glass denial is
   `code: "FORBIDDEN"`. User-actionable resolver errors (missing reason, connection not found,
   platform-managed connection) now return `apperr.UserError` and also surface verbatim.

   Still masked to `"an unexpected error occurred"` / `code: "INTERNAL"`, by design: every `5xx`
   (their `Detail` embeds raw Postgres text), a zero-status `SCIMError`, and `401` (its message is
   deliberately generic to prevent token enumeration). Treat `INTERNAL` as "something broke", never
   as a denial.

## Files
| File | Change |
| --- | --- |
| `admin/src/pages/ScimConflicts.tsx` (or a panel in `IdpConnectionDetail.tsx`) | **new** — conflicts table for the connection. |
| `admin/src/components/scim/ConflictRow.tsx` | **new** — row with Accept/Reject/Reopen + reason dialog. |
| `admin/src/graphql/queries.graphql` + `mutations.graphql` | `scimConflicts` query + the three conflict mutations (already in schema; ensure codegen picks them up). |

## Steps
- [ ] List pending conflicts for the connection (`scimConflicts(connectionId)`); show `canonicalKey` + `scimExternalId` + existing user.
- [ ] **Accept-Link** — reason required; calls `acceptScimConflict`. Detect the break-glass denial via `extensions.code === "FORBIDDEN"` (gap 3 above) and show "requires the identity.mapping.break_glass permission"; the server message is safe to display verbatim. Do **not** string-match. Treat `code: "INTERNAL"` as an unexpected failure, not a denial.
- [ ] **Reject** / **Reopen** — reason required; calls `rejectScimConflict` / `reopenScimConflict`; reflected in `status` (pending → approved/rejected → pending).
- [ ] **Handle all four statuses.** Migration 034's CHECK is `pending | approved | rejected | expired`. `expired` is a valid terminal state no step here covers — render it (read-only, no actions) rather than falling through to an unknown-status blank.
- [ ] **Ignore the stale schema comment.** `controller/graph/idp.graphqls` line ~101 comments `# pending | linked | rejected`. That is **wrong** — the backend uses `approved` (`internal/scim/conflict.go:44`, and migration 034's CHECK). Do not code against `linked`; the comment should be corrected backend-side.
- [ ] Poll/refresh after each action; never auto-takeover — every resolution is an explicit admin act.

## Rules
- Reason is mandatory for all three transitions (audited server-side).
- Never show email as the identity key; use `canonicalKey` / `scimExternalId`.
- Never retry a failed Accept silently. Branch on `extensions.code`: `FORBIDDEN` is the break-glass denial (guide the admin to obtain `identity.mapping.break_glass`), `CONFLICT`/`NOT_FOUND` mean the conflict moved under you (refresh the queue), `INTERNAL` is an unexpected failure — surface it and stop, never infer a permission problem from it.

## Build gate
`cd admin && npm run codegen && npm run build` green; manual: create a conflict (backend test fixture), Accept/Reject/Reopen with/without break-glass. Verify the denial surfaces in the UI as `extensions.code === "FORBIDDEN"` with a readable message, and that the `scim.user.conflict_approved` / `_rejected` audit rows and the persisted `resolution_reason` are written server-side.
