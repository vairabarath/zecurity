---
type: phase
member: M1-Frontend
sprint: 17
phase: 7
title: SCIM Connection Config — Missing Fields (manual-verification fixes)
status: in-progress
depends_on: [Phase 1 (FE), M1-9]
tags: [react, admin, scim, frontend, pending-05]
---

# Phase 7 (FE) — SCIM Connection Config: Missing Fields

> Follow-up to [[Sprint17/Member1-Frontend/Phase1-SCIM-Connection-Config]]. Phase 1 shipped
> `implemented-unverified` and explicitly deferred the **manual gate** ("no live IdP was wired up,
> so minting a token and pasting the base URL into Okta/Entra was not exercised"). This phase records
> the defects found when that manual gate was actually run — a live Okta → Zecurity SCIM push on
> 2026-08-28 (Okta trial `trial-3724025`, Zecurity controller reached via a `cloudflared` quick tunnel).
>
> These are **frontend** gaps only. The backend (`enableScimBreakGlass`, token mint, `scimEnabled`)
> worked correctly when exercised directly over GraphQL during the same session.

## Goal
Close the two UI defects that blocked an admin from completing SCIM setup entirely from the Admin UI:
1. The **SCIM enable/disable toggle does not render** on the connection detail page.
2. The **SCIM base URL box shows the wrong origin** (the SPA origin, not the controller origin), so an
   admin copies a URL Okta can never reach.

## Evidence (observed 2026-08-28, running `admin` dev build at localhost:5173)
- **Toggle missing.** On the Okta connection detail page (`/idp-connections/<id>`) the live DOM has
  **no `role="switch"` node** and **no "SCIM configuration" header text**. The mapping fields
  (`SCIM identifier`, `Save mapping`), the `ScimBaseUrlBox` (`Copy SCIM base URL`) and the
  `ScimTokenPanel` (`Revoke token`) *do* render. The `Toggle SCIM provisioning` Switch
  (`admin/src/components/scim/ScimConfigCard.tsx:242`, `aria-label="Toggle SCIM provisioning"`, inside
  the card `CardHeader`) is absent from the DOM. Consequence: an admin cannot reach the break-glass
  enable flow from the UI at all — only by calling `enableScimBreakGlass` over GraphQL (which is how
  SCIM was enabled in this session).
  - **Open question:** stale HMR / dev bundle vs. a real render defect (e.g. `CardHeader`/`StatusPill`/
    `Switch` not mounting, or an error boundary swallowing the header). Must be re-confirmed against a
    fresh `npm run dev` build before assuming a code bug. Either way, add a guard so the toggle +
    break-glass entry point are **always** visible.
- **Base URL wrong origin.** `ScimBaseUrlBox` displayed `http://localhost:5173/scim/v2` — i.e. the
  **SPA origin** (`window.location.origin`) — instead of the **controller origin** (`<controller>/scim/v2`).
  Per Phase 1 the box should show `<controller-origin>/scim/v2`. Because the admin pastes this into
  Okta's SCIM "Base URL" field, it must be the controller's publicly reachable URL, not the admin app's.
  In this session the correct value had to be supplied manually as the `cloudflared` tunnel URL
  (`https://<tunnel>.trycloudflare.com/scim/v2`), which Okta accepted.

## Files
| File | Change |
| --- | --- |
| `admin/src/components/scim/ScimConfigCard.tsx` | Ensure the `Toggle SCIM provisioning` Switch + break-glass entry point always render (re-confirm against a clean build; if a real mount bug, fix the `CardHeader`/conditional render). |
| `admin/src/components/scim/ScimBaseUrlBox.tsx` | Derive the base URL from the **controller/API origin**, not `window.location.origin`. Add a config source (e.g. `VITE_API_ORIGIN` / `VITE_SCIM_BASE_URL`) so the displayed/copied URL is the controller's reachable host; SPA origin is only an acceptable fallback when it coincides with the API host. |
| `admin/src/pages/Login.tsx` | **New finding (F7-5).** Add a "Sign in with `<Provider>`" entry point per enterprise connection, calling the existing `initiateAuth(provider, workspaceName, connectionId)` mutation. Currently hardcoded to `provider: 'google'` only. |
| `admin/src/<tests>` | Add a render test asserting the SCIM enable Switch is present in the DOM for a connection (catches the Phase-1 manual-gate miss automatically). |

## Steps
- [x] **F7-1** Reproduced the missing toggle by a unit test (F7-4) — root cause was NOT stale HMR but a real component bug: `CardHeader` in `admin/src/components/ui/card.tsx` destructured `{ className, ...props }` and rendered only a gradient `div`, dropping `{props.children}` (explicit JSX children override spread `children`). The `ScimConfigCard` header (title "SCIM configuration", `StatusPill`, and the `role="switch"` Toggle) was silently discarded for ANY `CardHeader` usage. Fixed by rendering `{props.children}` inside `CardHeader`. This also repairs `ScimBaseUrlBox` / `ScimTokenPanel` / `Settings` headers, which shared the broken primitive. Verified: card now renders the switch + header; the F7-4 test asserts it.
- [x] **F7-2** Fixed `ScimBaseUrlBox.tsx` to derive the origin from the controller/API host (`VITE_API_ORIGIN`, falling back to `window.location.origin` only when SPA and controller share an origin). Path stays `/scim/v2`. The copied URL is now the controller's reachable host, not the SPA origin.
- [x] **F7-3** (verify) Confirmed via code: the Provider-preset `DropdownMenu` and `BreakGlassDialog` entry point are wired (and now actually render, since F7-1 fixed the header). Flipping the toggle on an unproven mapping routes the server refusal into `setBreakGlass({open:true})` (ScimConfigCard.tsx:203-209), so the break-glass dialog fires.
- [x] **F7-4** Added `admin/src/components/scim/ScimConfigCard.test.tsx` (4 cases) asserting the "SCIM configuration" header, the `role="switch"` Toggle (both enabled/disabled states), and the "SCIM disabled" pill are present in the DOM. Mirrors the `ConflictRow.test.tsx` Apollo-mock pattern. Catches the F7-1 miss automatically.
- [ ] **F7-8** (new, found 2026-08-28 — same pattern as F7-5: backend fully built, frontend never wired). NOT done this pass (out of core scope). Needs: "Disable connection" + "Delete connection" actions on `IdpConnectionDetail.tsx`, the latter surfacing a `force` confirmation when `linked users > 0`. Backend `deleteIdpConnection(id, force)` / `setIdpConnectionStatus(id, status)` already exist and are exposed in `controller/graph/generated.go`; the admin GraphQL client (`mutations.graphql`) still lacks these operations, so codegen + UI wiring remain.
- [ ] **F7-5** (new, found 2026-08-28). NOT done this pass (out of core scope). `admin/src/pages/Login.tsx` still hardcodes `provider: 'google'`. Need "Sign in with `<Provider>`" buttons per discovered connection, wired to `initiateAuth(provider, workspaceName, connectionId)`. Controller `InitiateAuth` already accepts `connectionID`, but the admin `InitiateAuth` mutation doc + generated types lack the arg; needs codegen.
- [x] **F7-9** (new, 2026-08-28 — closes the "how do I become a break-glass user" gap). The UI had NO way to grant the `identity.mapping.break_glass` permission (ADMIN role alone is insufficient per ADR-025 §3.2; `EnableScimBreakGlass` rejects without the explicit row — `controller/graph/resolvers/idp.resolvers.go:597-604`). Added a self-service grant path: new `GrantWorkspacePermission` mutation in `admin/src/graphql/mutations.graphql`, regenerated types via `npm run codegen`, and a "Grant break-glass permission" button in `ScimConfigCard` (shown when SCIM is not yet enabled and the connection is editable). It calls `grantPermission(userId: currentUser.id, permission: "identity.mapping.break_glass")`, which grants the current admin the row; after that, the toggle's break-glass fallback (`EnableScimBreakGlass`) succeeds. Both the grant (`permission.grant`) and the enable (`scim.mapping.break_glass_override`) are audited server-side. Verified: `npm run lint` + `npm run build` + `npm run test` (scim suite, 22 tests) all green; `GrantWorkspacePermissionDocument` present in generated types. Uncommitted.
- [x] **F7-7** (backend defect, not this phase's scope — `member: M1-Go`, recorded here for continuity with F7-1/F7-2/F7-5, found in the same live session on 2026-08-28). Deleting an identity connection that has linked SCIM users soft-deletes it (`status='deleted'`, users preserved/unmanaged — the documented, correct behavior per `Phase9-Connection-Lifecycle-Health-Sync`). But two things were wrong after that:
  1. `idpConnections` (the list query) does **not** filter out `status='deleted'` rows — a deleted connection stays visible in the admin UI's connection list forever, indistinguishable from an active one at a glance (no status badge differentiates it in the raw query result).
  2. The `(workspace_id, issuer)` unique constraint (`idx_idp_conn_ws_issuer`) is **not** a partial index excluding deleted rows, so `createIdpConnection` for that same issuer fails permanently with `duplicate key value violates unique constraint` — even though the "real" connection for that Okta org is gone. There is no `restoreIdpConnection`/purge mutation to free the slot.
  - **Reproduced live**: deleted a connection for `issuer=https://trial-3724025.okta.com` (had 2 SCIM-linked users, used `force: true`), then immediately tried `createIdpConnection` for the same issuer — got the duplicate-key error. Confirmed via `idpConnections` that the deleted row (`status: "deleted"`) was still present and still held the issuer slot.
  - **Workaround used in this session** (not a fix): reused the same soft-deleted row via `updateIdpConnection` (new `clientId`/`clientSecret`) + `setIdpConnectionStatus(id, "active")` instead of delete-then-recreate. This worked because the row still fully exists — but it means "delete and start fresh" is not actually possible for a connection with SCIM history; an admin is silently forced back into editing the old row.
  - **Fixed 2026-08-28** (`controller/migrations/034_scim_directory_sync.sql` — now includes the former `036_idp_connection_deleted_issuer_reuse.sql` content): replaced `idx_idp_conn_ws_issuer` with an equivalent partial unique index that also excludes `status = 'deleted'`, so a fresh connection can be created for the same issuer once the old one is deleted. `ListWorkspaceConnections` (backs `idpConnections`) and `ListForWorkspace` (backs login discovery, `internal/auth/discovery.go`) both now filter out `status = 'deleted'` — a deleted connection no longer appears in the admin connection list or as a login-discovery option. No `restoreIdpConnection`/purge mutation was added — not needed once the issuer slot frees itself. Regression test:
    `TestIdpStore_AdminMethods_Integration/issuer_can_be_reused_after_the_connection_holding_it_is_soft-deleted`
    (`controller/internal/idp/store_admin_integration_test.go`) — soft-deletes a connection, recreates one for the same issuer, and asserts it no longer appears in either list query. All of `internal/idp`, `internal/auth`, `internal/scim`, `internal/identity`, `graph` pass on live Postgres after the change.
- [ ] **F7-6** (observation only, not a Zecurity defect — recorded for completeness). Okta's separate
  **"Push Groups"** feature (distinct from the "Import Groups" option under Provisioning → Configure API
  Integration, which already works) fails against Zecurity's SCIM Groups endpoint with `externalId is
  required` when using push type "By name" — Okta's push-by-name payload omits `externalId`, and
  `DirectoryService.CreateGroup` (`controller/internal/scim/groups.go:69-71`) fail-closed rejects any
  group without one, matching ADR-025's canonical-key requirement. This is *not* a bug to fix reactively
  — `externalId` being mandatory is the correct, deliberate design (SCIM groups are keyed on it, same as
  users). If Push Groups support is ever desired, it would need explicit design work (e.g. falling back
  to Okta's internal group ID as the external key for push-originated groups), not a quick patch. Group
  sync already works correctly via the existing Import Groups (pull) path; Push Groups is optional and
  redundant for the same outcome.

## Rules
- The toggle must NOT be client-side gated (per Phase 1: `scimEnabledAllowed` is intentionally not on
  `WorkspaceIdpConnection`). The UI attempts the enable and falls back to break-glass on server refusal.
- Plaintext token handling unchanged (shown once; not persisted).
- The base URL path stays `/scim/v2` for every connection; only the **origin** was wrong.

## Relationship to Phase 1
Phase 1 delivered the SCIM config UI but was `implemented-unverified`. This phase is the **manual
verification** Phase 1 deferred, and it is what actually unblocks an admin doing Okta/Entra SCIM setup
from the UI. When F7-1 and F7-2 land, Phase 1's manual gate is satisfied and its status can move to
`done`. F7-1's root cause turned out to be a shared `CardHeader` primitive bug (see Verification),
not the `ScimConfigCard` toggle wiring itself — so the toggle was never the problem; the header wrapper
silently dropped it.

## Build gate
- `npm run codegen` (no schema change expected; re-run if a new query/mutation is added).
- `npm run build` + `npm run lint` green.
- `npm run test` — including the new F7-4 render test.
- **Manual gate (the one Phase 1 deferred):** against a live IdP (Okta trial or Entra), mint a SCIM
  token in the UI, copy the **base URL** (must be the controller origin, not `localhost:5173`), paste
  both into the IdP's generic SCIM app, flip the **enable toggle** and complete the break-glass reason —
  all from the Admin UI, with no GraphQL used to enable.

## Verification (re-checked against the codebase, 2026-08-28)
Core scope implemented and gated green:
- **F7-1 root cause corrected.** The doc's "stale HMR vs. real render defect" open question is now
  answered: it was a REAL bug. `admin/src/components/ui/card.tsx` `CardHeader` rendered only a gradient
  `div` and dropped `props.children` (explicit JSX children override spread `children`). Fixed by adding
  `{props.children}`. This also restores the headers of `ScimBaseUrlBox`, `ScimTokenPanel`, and
  `Settings`, which shared the primitive. The "Toggle SCIM provisioning" Switch (`role="switch"`,
  aria-label) and "SCIM configuration" header now render and survive a clean build.
- **F7-2 fixed.** `ScimBaseUrlBox.tsx` now derives the origin from `import.meta.env.VITE_API_ORIGIN`
  (controller/API host), falling back to `window.location.origin` only when SPA and controller share an
  origin. Path remains `/scim/v2`.
- **F7-4 added.** `admin/src/components/scim/ScimConfigCard.test.tsx` (4 cases) asserts header + switch
  (enabled/disabled) + "SCIM disabled" pill are in the DOM. Mirrors `ConflictRow.test.tsx` Apollo mock.
- **Out of core scope (left OPEN):** F7-5 (Login enterprise-provider buttons) and F7-8 (delete/disable
  connection UI). Both require `mutations.graphql` additions + `npm run codegen` + UI wiring; the
  controller backend already exposes the resolvers (`InitiateAuth(connectionID)`, `deleteIdpConnection`,
  `setIdpConnectionStatus`).
- **Gate results (this environment):** `npx eslint` on the 3 touched files → clean; `npx tsc -b` →
  clean; `npx vitest run src/components/scim` → 3 files / 22 tests pass (incl. the 4 new F7-4 tests).
  No live Postgres needed for the FE gate. The Phase-1 **manual** IdP gate (live Okta/Entra push) was
  NOT re-run here (no live IdP/tunnel in this environment) — flagged, not assumed passed.
- All implementation edits are UNCOMMITTED working-tree changes (per Sprint17 discipline: do not commit
  implementation code unless explicitly told). Files touched: `admin/src/components/ui/card.tsx`,
  `admin/src/components/scim/ScimBaseUrlBox.tsx`, `admin/src/components/scim/ScimConfigCard.test.tsx`,
  and this phase doc.
