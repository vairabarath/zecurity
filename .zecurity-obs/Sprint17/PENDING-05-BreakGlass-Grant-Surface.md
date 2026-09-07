---
type: brief
status: pending
sprint: 17
tags: [pending-05, scim, identity, break-glass, permissions, admin-ui, fe]
related:
  - Sprint17/Member1-Frontend/Phase4-Provisioning-Conflicts-Queue
  - Sprint17/Member1-Go/Phase3-BreakGlass-Permission
  - Decisions/ADR-025-SCIM-Directory-Synchronization
---

# PENDING — Break-Glass Grant Surface for Provisioning-Conflict Resolution

> **Scope:** implementation work against the already-accepted **ADR-025**. This does **not**
> redesign the backend, does not alter the identity-conflict workflow, and does not weaken
> `identity.mapping.break_glass`.

---

## 0. Correction to the brief that produced this document

This work was originally framed as *"the Provisioning Conflicts resolution workflow has no
frontend."* **That framing is stale and is corrected here.**

`path.md` (≈line 388, "Also still open") states:

> `admin/src/pages/ScimConflicts.tsx` is read-only (0 `useMutation`;
> `acceptScimConflict`/`rejectScimConflict`/`reopenScimConflict` have no frontend)

That was true when written. It is **no longer true**. Commit **`79c009f`**
*(`feat(admin): SCIM provisioning-conflicts queue (Sprint 17 FE-4)`)* added both
`admin/src/pages/ScimConflicts.tsx` and `admin/src/components/scim/ConflictRow.tsx`; the latter
holds all three `useMutation` calls. `path.md` line 218 still shows `- [ ] FE-4` — the checkbox
was simply never ticked, and the "Also still open" note was never revised.

Per the task constraint, **`path.md` is not modified by this document.** The correction is recorded
here, in the same style as the dated verification callouts in `.zecurity-obs/pending/README.md`.

The residual gap is real but much narrower than the original framing, and is the subject of §4.

> **Parent document:** `Sprint17/PENDING-05-Implementation-Brief.md` — the broader PENDING-05 / ADR-025
> SCIM brief. This file is a narrow follow-on under the same roadmap ID, not a replacement for it.

---

## 1. Problem

An administrator can see and act on provisioning conflicts, but in the **only workspace state in
which conflicts actually occur** — SCIM enabled and pushing — there is **no reachable UI path to
obtain the `identity.mapping.break_glass` permission** that `AcceptLink` requires.

The result: **Accept link** returns `403 FORBIDDEN`, the UI correctly explains that the permission
is required, and the admin has nowhere in the product to go and get it. The conflict is
unresolvable without direct database access.

---

## 2. Current confirmed state

Verified by targeted inspection on branch `feat/sprint17-scim`:

| Claim | Status | Evidence |
|---|---|---|
| Conflicts page renders the queue | **shipped** | `admin/src/pages/ScimConflicts.tsx` |
| Accept / Reject / Reopen mutations wired in the UI | **shipped** | `admin/src/components/scim/ConflictRow.tsx:65-67` |
| `grantPermission` exists in the schema | **shipped** | `controller/graph/idp.graphqls:267` |
| `GrantWorkspacePermission` document exists | **shipped** | `admin/src/graphql/mutations.graphql:378` |
| A grant **button** exists somewhere in the UI | **shipped, but misplaced** | `admin/src/components/scim/ScimConfigCard.tsx:152, 236, 398-414` |
| Grant button reachable while SCIM is enabled | **NO — the gap** | render guard `!readOnly && !connection.scimEnabled` (`ScimConfigCard.tsx:392`) |
| Any query to **read** who holds a permission | **absent** | `WorkspacePermission` type exists (`idp.graphqls:310`); no `Query` field returns it |
| Permission management on the Team/Users page | **absent** | `admin/src/pages/TeamUsers.tsx` has no permission surface |
| Dev workspace has 0 `workspace_permissions` rows | **to verify** | asserted in `path.md` prose; not re-queried here |

---

## 3. What is already implemented (do not rebuild)

**Backend (ADR-025, Sprint 17 M1-3b / M1-8b) — complete:**

- `acceptScimConflict` / `rejectScimConflict` / `reopenScimConflict`, all `@hasRole(roles: [ADMIN])`
  at the boundary (`controller/graph/idp.graphqls:286-306`).
- `AcceptLink` performs an explicit `perm.HasPermission(..., permission.BreakGlassMapping)` and
  returns `403` when the row is absent. ADMIN role alone is denied — intentionally.
- Mandatory `reason` on every transition; audited (`scim.user.conflict_approved`,
  `scim.user.conflict_reopened`).
- `grantPermission(userId, permission)` → `WorkspacePermission`, ADMIN-gated, grant itself audited.
- `ErrorPresenter` surfaces `FORBIDDEN` in `extensions.code` so the frontend can branch on the code
  rather than on message text.

**Frontend (Sprint 17 FE-4, commit `79c009f`) — complete:**

- `ScimConflicts.tsx`: connection picker, `?connectionId=` deep link, loading / error / empty states,
  refetch-on-resolve.
- `ConflictRow.tsx`: Accept / Reject for `pending`, Reopen for `rejected`, read-only for
  `approved` / `expired`; mandatory-reason dialog with Confirm disabled until non-empty; directory
  snapshot (`scimUsernameSnapshot` / `scimEmailSnapshot`) with `canonicalKey` fallback.
- `admin/src/lib/conflictError.ts`: whitelist code classification, unknown code collapses to
  `INTERNAL` and is **never** inferred as a denial; `conflictGuidance` gives `FORBIDDEN` a
  permission-specific message and the dialog deliberately stays open on `FORBIDDEN`.
- `admin/src/components/scim/ConflictRow.test.tsx`: 16 tests covering the above.
- `ScimConfigCard.tsx`: a working "Grant break-glass permission" button + `enableScimBreakGlass`.

---

## 4. What is missing

**4.1 The grant control is unreachable from the state that needs it.** The alert in
`ScimConfigCard.tsx:392` renders only when `!readOnly && !connection.scimEnabled`. Identity
conflicts are raised by directory pushes, which only happen when SCIM **is** enabled — so by
construction, at conflict time the only grant button in the product is hidden. There is no grant
affordance anywhere on `ScimConflicts.tsx`.

**4.2 Possession is unobservable.** No GraphQL query returns `workspace_permissions` rows. The
frontend cannot tell whether the current admin holds `identity.mapping.break_glass`, so it cannot
pre-empt the 403, cannot label the Accept button, and cannot show who in the workspace can resolve
conflicts. `ScimConfigCard`'s `breakGlassGranted` is local optimistic state that resets on remount.

**4.3 No workspace-level permission administration.** Granting to *another* user is only possible
via the raw mutation; `TeamUsers.tsx` exposes no permission column or control.

**4.4 Bootstrap.** A fresh workspace has zero grant rows *(to verify)*, so the first conflict in any
new deployment is unresolvable through the UI.

---

## 5. Required frontend changes

1. **Recovery affordance on the FORBIDDEN path.** In `ConflictRow.tsx`, when
   `parsed.code === 'FORBIDDEN'`, render — alongside the existing `conflictGuidance` alert, inside
   the still-open dialog — an explicit **"Grant myself identity.mapping.break_glass"** action using
   `GrantWorkspacePermissionDocument` with the current user's id from `useAuthStore` (`import { useAuthStore } from '@/store/auth'`; `useAuthStore((s) => s.user)` — as used at `ScimConfigCard.tsx:32,153`). On success,
   re-enable Confirm so the admin can retry the accept without leaving the dialog. Do **not** retry
   automatically — the accept must remain a deliberate, reason-bearing act.
2. **Do not weaken the guidance text.** `conflictGuidance('FORBIDDEN')` already states that ADMIN is
   insufficient; keep that wording and add the action beneath it.
3. **Reflect possession when a read exists** (see §6). If a `myWorkspacePermissions`-style query is
   added, use it to (a) render a passive "you hold break-glass" indicator on `ScimConflicts.tsx` and
   (b) pre-label rather than pre-disable **Accept link** — a stale read must never be the reason an
   authorized admin is blocked. The server stays the only authority.
4. **Extract the grant control** currently inlined in `ScimConfigCard.tsx` into a shared component
   (e.g. `admin/src/components/scim/GrantBreakGlassButton.tsx`) so the config card and the conflicts
   dialog share one implementation and one success/error contract. Behaviour on the config card must
   not change.
5. **Optional, only if §6 adds a read:** a permissions column or per-user grant action on
   `TeamUsers.tsx`, so break-glass can be delegated rather than self-granted.

---

## 6. Required permission / authorization UX or bootstrap handling

- **Self-grant is the sanctioned bootstrap.** `grantPermission` is already ADMIN-gated and audited;
  an ADMIN self-granting break-glass is an explicit, recorded act and remains materially different
  from ADMIN implicitly satisfying the check. Nothing in this scope changes that distinction.
- **A read query is needed and does not exist.** Exposing possession requires a new ADMIN-scoped
  `Query` field returning `[WorkspacePermission!]!` (self-scoped, or workspace-scoped for §5.5).
  Whether this is in scope is a **decision for the implementer**; every §5 item except 3 and 5 works
  without it. Marked **to verify** — no such field was found, but the search was targeted, not
  exhaustive.
- **No auto-grant.** The permission must never be issued as a side effect of opening a page, loading
  the queue, or clicking Accept. It is always a separate, explicit, user-initiated action.
- **No seeding.** Do not add a migration or fixture that pre-inserts break-glass rows into any
  workspace, dev included. Bootstrap is a UI action, not a data default.

---

## 7. Security boundaries / invariants (unchanged — restate and preserve)

1. `AcceptLink` keeps its explicit `perm.HasPermission(..., permission.BreakGlassMapping)` check.
   **Do not** relax it to accept ADMIN role.
2. `@hasRole(roles: [ADMIN])` on the mutations is a boundary gate, **not** the authorization
   decision. The permission-row check stays.
3. Possession is a row in `workspace_permissions`. Never implied by role, never cached client-side
   as authority.
4. `reason` stays mandatory on accept / reject / reopen and continues to be audited.
5. Identity is keyed on the canonical key. **Never** on email. Snapshots stay display-only context.
6. No silent auto-linking; no new identity created while a canonical identity awaits resolution.
7. Frontend branches on `extensions.code` only. A missing or unrecognized code stays `INTERNAL` and
   must never be rendered as a permission problem.
8. Client-side permission state is advisory UX. The server remains the sole authority.

Nothing in §5 or §6 touches invariants 1–8.

---

## 8. Expected user flow

1. A directory push collides with an existing identity → backend returns `409 identity_conflict` and
   writes a pending conflict row.
2. Admin opens **Provisioning conflicts** (directly, or deep-linked from the connection detail page).
3. Admin clicks **Accept link**, enters a reason, confirms.
4. **Holds break-glass:** the link is made, ownership flips to `scim`, the action is audited, the
   queue refetches. *(Already works today.)*
5. **Does not hold break-glass:** `403 FORBIDDEN`. The dialog stays open showing "Permission
   required". **New:** a "Grant myself identity.mapping.break_glass" action appears; the admin
   clicks it, the grant is audited, Confirm re-enables, and the admin deliberately re-confirms with
   the reason still in the field. Step 4 then proceeds.
6. Reject and Reopen are unaffected and never return `FORBIDDEN`.

---

## 9. Acceptance criteria

- [ ] An ADMIN with no break-glass row can, starting from a pending conflict and **without leaving
      the admin UI or touching the database**, obtain the permission and complete an Accept link.
- [ ] The grant is a distinct, explicit click. No code path issues the permission implicitly.
- [ ] After a successful grant the accept is **not** auto-retried; the admin re-confirms.
- [ ] The reason entered before the 403 survives the grant step (not silently cleared).
- [ ] `AcceptLink`'s server-side permission check is unmodified; an ADMIN without the row still
      receives `403`, verified by the existing backend test.
- [ ] `conflictGuidance('FORBIDDEN')` still states that ADMIN role is insufficient.
- [ ] A non-`FORBIDDEN` failure (`INTERNAL`, `CONFLICT`, `NOT_FOUND`) shows **no** grant affordance.
- [ ] The `ScimConfigCard` grant path behaves exactly as before the shared-component extraction.
- [ ] No new grant rows are created by any migration, seed, or fixture.
- [ ] `cd admin && npm run build` and the existing SCIM test suites pass.

---

## 10. Suggested tests

**Frontend** (`admin/src/components/scim/ConflictRow.test.tsx`, extending the existing 16):

- `FORBIDDEN` on accept → guidance **and** grant action rendered; dialog stays open.
- `INTERNAL` / `CONFLICT` / `NOT_FOUND` on accept → **no** grant action.
- Grant succeeds → Confirm re-enabled, accept **not** fired until a second explicit confirm.
- Grant fails → error surfaced, accept still blocked, dialog still open.
- Reason text persists across the grant step.
- Reject / Reopen never render the grant action.
- New test file for the extracted `GrantBreakGlassButton` (success, failure, no-authenticated-user).

**Backend** — no new tests required; `controller/internal/scim/conflict_integration_test.go:176`
("Accept-Link requires identity.mapping.break_glass (ADMIN alone denied, nothing commits)") is the
regression guard for invariant 1 and must keep passing untouched.

**Manual** — in the dev workspace: provoke a conflict, confirm Accept 403s, grant via the new
affordance, confirm accept succeeds and both the grant and the approval appear in the audit log.

---

## 11. Relevant files to inspect / modify

| File | Role |
|---|---|
| `admin/src/components/scim/ConflictRow.tsx` | **modify** — add the FORBIDDEN-path grant affordance |
| `admin/src/components/scim/ConflictRow.test.tsx` | **modify** — extend coverage per §10 |
| `admin/src/components/scim/ScimConfigCard.tsx` | **modify** — extract the grant control; behaviour unchanged |
| `admin/src/components/scim/GrantBreakGlassButton.tsx` | **new** — shared grant control |
| `admin/src/lib/conflictError.ts` | **inspect** — keep the FORBIDDEN wording; no code-classification change |
| `admin/src/pages/ScimConflicts.tsx` | **modify (light)** — optional possession indicator |
| `admin/src/graphql/mutations.graphql` | **inspect** — `GrantWorkspacePermission` already present |
| `admin/src/graphql/queries.graphql` | **modify only if** §6 adds a permissions read |
| `admin/src/store/auth.ts` | **inspect only** — `useAuthStore((s) => s.user)` supplies the grantee id |
| `admin/src/pages/TeamUsers.tsx` | **optional** — delegation surface (§5.5) |
| `controller/graph/idp.graphqls` | **inspect**; add a read-only `Query` field **only if** §6 is taken |
| `controller/internal/idp/store.go` (`:68`, `:291`) | **inspect only** — permission semantics |
| `controller/internal/scim/conflict_integration_test.go` | **inspect only** — must keep passing |

Run `cd admin && npm run codegen` after any `.graphql` change.

---

## 12. Non-goals

- Redesigning the backend conflict workflow, or any change to ADR-025.
- Weakening, bypassing, or role-implying `identity.mapping.break_glass`.
- Auto-linking identities, or creating a new identity while a canonical identity awaits resolution.
- Auto-retrying the accept after a grant.
- Seeding permission rows via migration or fixture.
- A general-purpose RBAC/permission-management console — only what §5 requires.
- Touching `enableScimBreakGlass` semantics or the mapping-proven gate.
- MFA step-up on break-glass (tracked separately as PENDING-06).
- Updating `path.md`'s FE-4 checkbox or its stale note (see §0 — deliberately out of scope here).
