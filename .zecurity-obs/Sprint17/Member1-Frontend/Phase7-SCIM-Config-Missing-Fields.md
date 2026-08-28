---
type: phase
member: M1-Frontend
sprint: 17
phase: 7
title: SCIM Connection Config — Missing Fields (manual-verification fixes)
status: open
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
- [ ] **F7-1** Reproduce the missing toggle on a clean `npm run dev` build; determine stale-bundle vs. real render defect; fix so the `Toggle SCIM provisioning` Switch (+ `SCIM enabled`/`SCIM disabled` `StatusPill`) always mounts in `ScimConfigCard`.
- [ ] **F7-2** Fix `ScimBaseUrlBox` to show `<controller-origin>/scim/v2` (config-driven origin, SPA origin only as fallback). Verify the copied URL is reachable by Okta (public HTTPS host), not `localhost`/SPA origin.
- [ ] **F7-3** (verify) Confirm the Provider preset dropdown and the `BreakGlassDialog` entry point are reachable once the toggle renders, and that flipping the toggle on an unproven mapping surfaces the break-glass dialog (already built — confirm it fires).
- [ ] **F7-4** Add a component test that fails if the enable Switch is absent from the `ScimConfigCard` DOM, so the manual-gate miss cannot recur silently.
- [ ] **F7-5** (new, found 2026-08-28 while setting up a second, separate Okta OIDC app for Zecurity
  login — same live session as F7-1/F7-2, not related to the SCIM app). `admin/src/pages/Login.tsx`
  only wires `useMutation(InitiateAuthDocument)` with a hardcoded `provider: 'google'` on every call
  site (both the workspace-lookup and the direct-login paths). The backend fully supports enterprise
  IdP login today — `InitiateAuth(ctx, provider, workspaceName, connectionID)`
  (`internal/auth/oidc.go`) accepts a `connectionID` and Phase 11 (this same sprint) wired the
  connection's `subjectClaim` all the way through — but there is **no UI control** on the login page to
  pick a non-Google connection and pass its `connectionId`. Consequence: creating an Okta (or any
  enterprise) OIDC connection via FE-0/the Phase-6 wizard lets an admin *configure* the connection, but
  there is currently no click-through path for an end user to actually *log in* with it — only a direct
  `initiateAuth` GraphQL call exercises that path. Needs a "Sign in with `<Provider>`" button per
  discovered connection (the `discovery` query/endpoint already used to list available IdPs for a
  workspace is the natural data source), wired to `initiateAuth(provider, workspaceName, connectionId)`.
- [ ] **F7-7** (backend defect, not this phase's scope — `member: M1-Go`, recorded here for continuity
  with F7-1/F7-2/F7-5, found in the same live session on 2026-08-28). Deleting an identity connection
  that has linked SCIM users soft-deletes it (`status='deleted'`, users preserved/unmanaged — the
  documented, correct behavior per `Phase9-Connection-Lifecycle-Health-Sync`). But two things are
  wrong after that:
  1. `idpConnections` (the list query) does **not** filter out `status='deleted'` rows — a deleted
     connection stays visible in the admin UI's connection list forever, indistinguishable from an
     active one at a glance (no status badge differentiates it in the raw query result).
  2. The `(workspace_id, issuer)` unique constraint (`idx_idp_conn_ws_issuer`) is **not** a partial
     index excluding deleted rows, so `createIdpConnection` for that same issuer fails permanently
     with `duplicate key value violates unique constraint` — even though the "real" connection for
     that Okta org is gone. There is no `restoreIdpConnection`/purge mutation to free the slot.
  - **Reproduced live**: deleted a connection for `issuer=https://trial-3724025.okta.com` (had 2
    SCIM-linked users, used `force: true`), then immediately tried `createIdpConnection` for the same
    issuer — got the duplicate-key error. Confirmed via `idpConnections` that the deleted row
    (`status: "deleted"`) was still present and still held the issuer slot.
  - **Workaround used in this session** (not a fix): reused the same soft-deleted row via
    `updateIdpConnection` (new `clientId`/`clientSecret`) + `setIdpConnectionStatus(id, "active")`
    instead of delete-then-recreate. This worked because the row still fully exists — but it means
    "delete and start fresh" is not actually possible for a connection with SCIM history; an admin is
    silently forced back into editing the old row.
  - **Fix would need**: either (a) a partial unique index (`WHERE status != 'deleted'`) so a new
    connection can be created for the same issuer once the old one is deleted, or (b) filter
    `idpConnections` to exclude `deleted` by default (with an explicit include-deleted flag for
    audit/history views), or both.
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
`done`.

## Build gate
- `npm run codegen` (no schema change expected; re-run if a new query/mutation is added).
- `npm run build` + `npm run lint` green.
- `npm run test` — including the new F7-4 render test.
- **Manual gate (the one Phase 1 deferred):** against a live IdP (Okta trial or Entra), mint a SCIM
  token in the UI, copy the **base URL** (must be the controller origin, not `localhost:5173`), paste
  both into the IdP's generic SCIM app, flip the **enable toggle** and complete the break-glass reason —
  all from the Admin UI, with no GraphQL used to enable.
