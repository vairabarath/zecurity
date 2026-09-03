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
- [x] **F7-5** (implemented 2026-09-02). `admin/src/pages/Login.tsx` no longer hardcodes `provider: 'google'`: the login page now discovers the workspace's configured IdP connections and renders one button per connection, calling `initiateAuth(provider, connectionId, workspaceName)` with the exact selected connection. See "F7-5 implementation notes" below for the API boundary decision, the defects found in the first draft, and the deviation from the originally specified GraphQL signature.

### F7-5 implementation notes (2026-09-02)

**API boundary.** `idpConnections` is `@hasRole(roles: [ADMIN])` and the login page has no JWT, so a
separate public field was required. `idpConnections` was NOT broadened — its annotation is untouched.

**DEVIATION from the specified signature.** The task specified
`lookupIdpConnections(workspaceSlug: String!): [WorkspaceIdpConnection!]!`. Implemented as
`[PublicIdpConnection!]!` instead — a new 3-field type (`id`, `provider`, `displayName`) added to
`controller/graph/idp.graphqls`. Reason: on an unauthenticated field, returning
`WorkspaceIdpConnection` makes `clientId`, `issuer`, `discoveryUrl`, `domainHint`, `subjectClaim`,
`scimIdentifier`, `scimEnabled`, `lastSyncAt` and `identityHealth` readable by anyone who knows a
workspace slug, because field selection is client-controlled — which contradicts the task's own
"must not expose arbitrary workspace connection data" requirement. Zeroing those fields in the
resolver was rejected as the alternative: they are non-null `String!`, so redaction would return
empty-string *lies* on the same type the admin API returns truthfully, and Apollo's normalized cache
keys by `__typename:id`, so a public read and an admin read of the same connection would collide in
one cache entry. `WorkspaceIdpConnection` itself is unchanged. **To revert to the pinned signature:**
change the field's return type back in `idp.graphqls`, map through `idpConnToGQL` in the resolver, and
re-run `make gqlgen` + `npm run codegen`.

**THE PUBLIC-ROUTING ALLOWLIST — the defect that would have shipped this dead.** Omitting
`@hasRole` is NOT what makes a field public in this server. `controller/cmd/server/main.go` routes
`/graphql` through `routeGraphQL` → `isPublicGraphQLRequest` → `requestSelectsOnlyPublicFields`,
which is fail-closed against the `publicRootFields` allowlist (`main.go:713`); anything not listed
goes to the *protected* handler and gets `missing Authorization header`. The first draft added the
schema field and the resolver but never touched that map, so every login-page discovery call would
have been rejected with `UNAUTHORIZED` in production while passing every resolver-level test.
`"lookupIdpConnections"` is now in `publicRootFields`, with routing cases added to
`cmd/server/route_test.go` (public alone; still protected when smuggled beside `idpConnections`;
alias of `idpConnections` still protected).

**Backend** (`controller/graph/resolvers/idp.resolvers.go`, `LookupIdpConnections`):
- Workspace scoping is *derived*, never supplied: slug → `IdpStore.WorkspaceIDBySlug` → the resulting
  tenant id is the only value passed to the store. A client-supplied workspace/tenant id is never
  accepted.
- Reuses `IdpStore.ListWorkspaceConnections(ctx, tenantID)` as-is (already
  `tenant_id = $1 AND status != 'deleted'`). No new store method, no new query, no migration.
- **Active-only:** non-active connections are filtered out. `InitiateAuth` fails closed on a
  non-active connection (`internal/auth/oidc.go:47`), so listing a disabled connection would render a
  guaranteed-broken button.
- Unknown slug → empty list, not an error, matching `lookupWorkspace`'s `found:false` semantics.

**Defects fixed in the first (uncommitted) draft of this phase.** The draft was wired end-to-end but
broken in four ways, all now covered by failing-under-mutation tests:
1. `workspaceName` sent to `initiateAuth` was wrong. Endpoint mode derived it from `foundWorkspaces`,
   which is empty there, so it fell through to the raw **slug**; email mode picked
   `foundWorkspaces[0]`, so choosing the *second* workspace sent the *first* workspace's name. The
   resolved name was held in a `resolvedWorkspaceName` state that was never read. Replaced with a
   single `pendingWorkspace: {name, slug}` captured where the workspace is actually resolved, in both
   modes.
2. The IdP chooser was rendered only inside the endpoint-mode `<form>`. In email mode `startOAuth`
   loaded connections and rendered nothing — a dead end with the spinner stuck on. The chooser is now
   rendered once for both modes.
3. `startOAuth` never cleared `loading` on the success-with-connections path (no `finally`), so the
   button stayed "Authenticating...".
4. After a failed enterprise `initiateAuth`, `selectedConnection` stayed set, hiding the choices; the
   user could see the error but not retry. Selection is now cleared on failure so the list returns.

**Google fallback:** reached *only* when discovery returns an empty successful list. A discovery
failure raises `IdpDiscoveryError` and shows "Could not load the identity providers for this
network." — never Google. A failed enterprise sign-in shows
"Sign-in with `<label>` failed. Select a provider to try again." and never retries through Google.

**CORRECTION to an earlier claim in this section's drafting.** The draft was *suspected* of silently
falling back to Google when the discovery query errored (`data?.lookupIdpConnections ?? []`). Probed
empirically against this repo's Apollo version (`@apollo/client` v4, `useLazyQuery` from
`@apollo/client/react`): the execute promise **rejects** for both GraphQL errors and network errors —
it never resolves with `{error, data: undefined}`. So the draft's rejection propagated to the caller's
`catch` and *did* produce an error state, just with the misleading message "Authentication failed.
Check the endpoint and try again." The explicit `if (result.error || !result.data)` guard now in
`discoverConnections` is therefore **defensive only** (it covers `errorPolicy` changes and future
Apollo behavior), not a live bug fix. The load-bearing guard is the `try/catch` → `IdpDiscoveryError`.

**Verified over real HTTP, unauthenticated** (new binary on `PORT=18080` against the `ztna_postgres`
dev container, no `Authorization` header):
| Probe | Result |
|---|---|
| `lookupIdpConnections(workspaceSlug:"no-such-workspace")` | `{"data":{"lookupIdpConnections":[]}}` — public, no auth error, unknown slug is empty |
| `idpConnections { id }` | `UNAUTHORIZED / missing Authorization header` — admin field still gated |
| `lookupIdpConnections { id clientId issuer subjectClaim scimEnabled }` | `GRAPHQL_VALIDATION_FAILED: Cannot query field "clientId" on type "PublicIdpConnection"` (×4) — admin metadata is **schema-impossible** here, which is the concrete payoff of the type deviation; with `WorkspaceIdpConnection` these would have returned values |
| `lookupIdpConnections { id } idpConnections { clientId }` | `UNAUTHORIZED` — no smuggling past the allowlist |

**Verified by mutation testing** (reintroduce the bug, confirm a test fails):
- old `workspaceName` derivation → 3 frontend tests fail;
- chooser restricted to endpoint mode → 1 frontend test fails;
- `status != "active"` filter removed → `TestLookupIdpConnections_ActiveOnly` fails.
The `data ?? []` swallow could NOT be made to fail a test, which is what led to the correction above.
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

---

## Post-Phase Fixes

### Fix: F7-5 login discovery dropped the platform (Google) tier — 2026-09-03
**Issue:** After F7-5 landed, a workspace with an enterprise IdP configured (Okta) showed ONLY that
provider on `Login.tsx`. There was no way to reach the shared platform (Google) login path, and the
workspace-level `platform_login_enabled` toggle had no effect on the login page at all.

**Root Cause:** Two public login-discovery paths existed and silently disagreed.

| | store call | SQL scope | platform tier? | honors `platform_login_enabled`? |
|---|---|---|---|---|
| `GET /workspaces/{slug}/auth` (pre-existing, ADR-024 §0, `internal/auth/discovery.go`) | `ListForWorkspace` | `tenant_id IS NULL OR tenant_id = $1` | yes | yes |
| `lookupIdpConnections` (F7-5) | `ListWorkspaceConnections` | `tenant_id = $1` | **no** | **no** |

The shared Google connection is seeded by `migrations/031_identity_federation.sql` with
`tenant_id = NULL` (unique per provider via `idx_idp_conn_platform`), so the tenant-scoped query could
never return it. F7-5 also never read `platform_login_enabled`, so ADR-024 §5 was unenforced on the
GraphQL path.

**Fix Applied — backend** (`controller/graph/resolvers/idp.resolvers.go`, `LookupIdpConnections`):
```go
// BEFORE:
conns, err := r.IdpStore.ListWorkspaceConnections(ctx, tenantID)
// ...
out = append(out, &graph.PublicIdpConnection{ID: c.ID, Provider: c.Provider, DisplayName: c.DisplayName})

// AFTER:
conns, err := r.IdpStore.ListForWorkspace(ctx, tenantID)          // + platform tier
platformEnabled, err := r.IdpStore.PlatformLoginEnabled(ctx, tenantID)  // ADR-024 §5
// c.TenantID == nil -> tier "bootstrap" (skipped when !platformEnabled); else "enterprise"
// enterprise ordered first
```
`PublicIdpConnection` gained a 4th field, `tier: String!` (`"enterprise" | "bootstrap"`) in
`controller/graph/idp.graphqls`. Still no admin metadata — the type remains id/provider/displayName/tier,
so the F7-5 API-boundary decision is intact.

**Fix Applied — frontend** (`admin/src/pages/Login.tsx`):
- The Google auto-fallback now keys off the **enterprise subset** being empty, not the whole list.
  With the platform tier included, `discovered.length === 0` would no longer be true for a bare
  workspace, and it would have rendered a pointless one-button "chooser" containing only Google
  instead of redirecting straight there (the pre-F7-5 behavior).
- Google is rendered behind an **"Other sign-in options"** disclosure, not as a primary button.

**Why a disclosure and not an admin-only gate.** The original ask was "show Google only if the user is
an admin". That is not implementable at this point in the flow: login discovery runs before any JWT
exists, so the server cannot know who the visitor is, and answering "is this email an admin?"
unauthenticated is an account-enumeration oracle. The risk it was meant to mitigate also does not
exist — per `internal/bootstrap/bootstrap.go` `Provision`, a never-before-seen identity signing in via
the platform IdP either matches a pending invite or gets a **brand-new workspace**; it cannot join this
one. The real hazard is a rank-and-file member accidentally provisioning a junk workspace, which the
disclosure addresses without an oracle.

**Related files also fixed:** `admin/src/graphql/queries.graphql` (+`tier`), regenerated
`admin/src/generated/*` and `controller/graph/{generated,models_gen}.go`.

**Tests:** `TestLookupIdpConnections_IncludesPlatformTier`, `_PlatformTierSuppressedWhenDisabled`,
`_PlatformToggleIsPerWorkspace` (new); the 3 pre-existing scoping/active-only/bare-workspace tests were
updated for tiering (a bare workspace now correctly returns the bootstrap tier, not an empty list).
Frontend: 4 new cases covering collapsed-by-default, reveal, exact platform `connectionId`, and
straight-to-Google when only the platform tier exists. **7/7 backend (live PG), 85/85 frontend.**

### Fix: F7-5 integration-test teardown leaked a scratch database per test — 2026-09-03
**Issue:** `controller/graph/resolvers/idp_lookup_connections_test.go` left one
`resolvers_lookupidp_*` database behind per test, per run. 32 had accumulated on the dev Postgres.

**Root Cause:** teardown issued `DROP DATABASE` on `h.pool` — a pool connected to the very database
being dropped. Postgres always refuses this, and the error was discarded (`_, _ =`), so the failure was
silent.

**Fix Applied:**
```go
// BEFORE:
_, _ = h.pool.Exec(ctx, "DROP DATABASE IF EXISTS "+h.dbName)
h.pool.Close()

// AFTER: close the pool FIRST, then drop from a separate admin connection
h.pool.Close()
adminPool, err := pgxpool.New(ctx, h.adminDSN)   // adminDSN now stored on the harness
defer adminPool.Close()
if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+h.dbName); err != nil {
    h.t.Logf("teardown: drop %s: %v", h.dbName, err)   // no longer swallowed
}
```
**Verified:** 14 test executions after the fix added zero new databases. The 32 pre-existing leftovers
are still present and need a manual sweep.

### Note: codegen commands in `CLAUDE.md` are wrong for this repo — 2026-09-03
- `cd controller && go generate ./graph/...` is a **no-op** — there is no `go:generate` directive
  anywhere in the repo. Real command: `gqlgen generate --config graph/gqlgen.yml`, run from
  `controller/` (the paths in `gqlgen.yml` are relative to the controller root, NOT to `graph/`).
- The `~/go/bin/gqlgen` on this machine is **v0.17.89** while `go.mod` pins **v0.17.90**. Generating
  with the stale binary rewrites the version stamp in ten `*.resolvers.go` files — pure diff noise.
  Use `GOBIN=<tmp> go install github.com/99designs/gqlgen@v0.17.90` and run that binary.
- Running gqlgen from the wrong cwd creates a stray `controller/graph/graph/` tree. One already exists
  and is **committed** (`generated.go`, `models_gen.go`, `resolvers/resolver.go`) from a previous
  occurrence — it is dead weight and a cleanup candidate.

### Fix: swapped client ID / client secret was accepted and only failed at login — 2026-09-03
**Issue:** Entering the Okta client secret in the **Client ID** field (and vice versa) created the
connection successfully and the wizard advanced to Step 2. The mistake only surfaced later as an
opaque `HTTP 400` from Okta's `/authorize`:
`{"errorCode":"invalid_client","errorSummary":"Invalid value for 'client_id' parameter."}`.
Observed live against `trial-3724025.okta.com`, where `identity_connections.client_id` held a
64-char base64url value (Okta's client-SECRET shape; real client IDs are ~20 chars, `0oa…`).

**Root Cause:** nothing anywhere verified the credentials. `createIdpConnection` called only
`validateOIDCDiscovery`, whose own error text conceded *"this check does not validate the client ID or
client secret"* — discovery is an unauthenticated endpoint and sends no credential.
`testIdpConnection` looked like a real check but constructed its provider with an **empty secret**
(`idp.resolvers.go:269`), so it only re-probed discovery. Field mapping was correct at every layer —
this was pure absence of validation, not a swap in code.

**Design — one shared primitive.** `providers.(*OIDCProvider).VerifyClientCredentials` is the single
verification path; create, credential-changing update and test all route through
`resolvers.verifyOIDCCredentials`.

It POSTs to the **freshly discovered** `token_endpoint` (`fetchDiscovery(ctx, false)` — cache-neutral,
so the endpoint tested is the one the configured issuer/discoveryURL currently advertises, and an
admin probe can neither seed nor refresh the login path's cache) with
`grant_type=authorization_code` and a deliberately invalid code:

| response | verdict |
|---|---|
| `invalid_client` (either envelope), or 401 with no code | **invalid** |
| any other OAuth error (`invalid_grant`, `unsupported_grant_type`, …), or 200 | **valid** |
| unreachable / unparseable / no OAuth code | **inconclusive** |

**Two findings that shaped this, both established empirically against the live Okta org, not assumed:**
1. **`client_credentials` is unusable as the probe.** Okta's org authorization server does not list
   it in `grant_types_supported` at all, so probing with it would fail for a perfectly valid login
   connection. `authorization_code` is necessarily enabled for a connection whose purpose is login.
2. **Okta's error envelope is non-standard** — `{"errorCode":…}` with **HTTP 400**, not RFC 6749's
   `{"error":…}` with 401. A classifier keyed only on the spec shape misreads it. `oauthErrorCode`
   accepts both.

The soundness rests on one measured fact: **client authentication is evaluated before the grant.** A
request carrying *both* a bogus code and a bad client ID returned `invalid_client`, not
`invalid_grant` — so `invalid_client` means the credentials are wrong, and anything else means the
client authenticated.

**Policy — block unless proven correct.** Invalid *and* inconclusive both fail. An unverified pair
must not be presented to an admin as a working connection; that is exactly the failure mode above.

**Applied:**
- **create** — verified after discovery, before the insert. Nothing persists unless proven.
- **update** — verified only when `clientId`/`clientSecret` actually change, on the **effective** pair
  (supplied value, else stored — either field may change alone; `GetByID` returns the decrypted
  secret). Runs before the single `UPDATE`, so a refusal cannot leave a partially updated row. A
  metadata-only update (e.g. `displayName`) is **not** probed.
- **test** — now built with the connection's real secret and runs the same check. `Ok=false` unless
  positively verified; an inconclusive result is never worded as verified. Mutates nothing.

**Secret hygiene:** the surfaced reason carries only the IdP's bounded OAuth error code or a locally
constructed transport description — never the response body, never a credential. Nothing is logged.
Regression-tested by `TestVerifyClientCredentials_ReasonNeverLeaksTheSecret`.

**Frontend:** the create dialog previously carried a deliberate honesty guard asserting the UI must
*never* claim credentials were verified. That premise is now half-false, so it was re-aimed rather
than deleted: the copy and toast must now state credentials **are** verified (under-claiming would
send an admin hunting a problem the server already ruled out), while the overclaim guard now protects
the **redirect URI**, which genuinely remains unproven — verifying it needs a real authorization
request.

**Tests:** `internal/auth/providers/oidc_verify_credentials_test.go` (17 subtests — both envelopes,
the 401 case, post-auth errors, inconclusive shapes, secret non-leakage, and that the probe sends
`client_secret_post` + the sentinel code + the real redirect URI) and
`graph/resolvers/idp_credential_verification_test.go` (swapped pair persists nothing; refused
credential change leaves the row untouched including a rename riding along in the same mutation;
metadata-only update is not probed; test reports `Ok=false`). Existing fixtures had to start serving a
token endpoint and a decryptable secret, since a connection without credentials is now correctly
inconclusive.

**Still not verified (deliberately):** the redirect URI, and that any particular user can sign in.
Note the live 400 may also involve `redirect_uri` registration or the org-vs-`default` authorization
server — see the F7-5 platform-tier entry's sibling notes.

**Gates:** `go build ./...`, `go vet ./...` clean; full backend suite green except the 7 pre-existing
`TestGroupOrigin_*` fixture failures (`workspaces_status_check`, unrelated). Frontend: `tsc --noEmit`
clean, **85/85** tests, `pnpm build` clean. Uncommitted.

### Fix: enterprise Okta login — non-member self-provisioned a workspace; callback errors were invisible — 2026-09-03
**Requirement:** a user who exists in the workspace but is NOT an admin signs in and lands on the
client-installation page; a user who is NOT in the workspace is told they are not invited and offered
the deploy-your-own-network path.

**Already working (no change needed):** the non-admin landing. `AuthCallback.tsx:83-88` already
role-redirects (`ADMIN → /dashboard`, else `/install`) and `App.tsx`'s `AdminLayout` default-denies
non-admins to `/install`. `ClientInstall.tsx` sits under `ProtectedLayout`, not `AdminLayout`.

**Issue 1 — an uninvited enterprise user got their own workspace, as its ADMIN.**
`bootstrap.Provision` treated every first-seen identity as a signup: no pending invite ⇒
`runBootstrapTransaction` ⇒ **new workspace, role `admin`**. That is correct for the platform tier
(Google first-time signup IS workspace creation) but wrong for an enterprise connection, which
already belongs to exactly one workspace. Anyone in the customer's Okta directory could sign in and
receive a fresh Zecurity workspace. Authentication proves identity; it does not grant access.

**Fix:** made provisioning tier-aware. `ProvisionInput` gained `ConnectionTenantID *string` — the
workspace owning the connection, non-nil for enterprise, nil for the platform tier — threaded from
`callback.go` (`conn.TenantID`) through `identity.Service.Authenticate`. On an enterprise connection
with no invite, `Provision` now returns the new `identity.ErrNotInvited` instead of creating a
workspace. The platform signup path is untouched.

**Issue 1b — the invite lookup was not workspace-scoped (security).** It matched
`WHERE email = $1 AND status = 'invited'` across ALL workspaces, so an invite pending in workspace B
would let someone signing in through workspace A's IdP join B. Email is an invite-matching hint, not
an identity key and not workspace proof. The enterprise path now scopes the lookup with
`AND workspace_id = $2`.

**Issue 2 — every OAuth callback failure was silent.** `callback.go`'s `fail()` redirects to
`/login?error=<reason>`, but `Login.tsx` never read the param — it only rendered its own form-validation
state. So a refused sign-in returned the user to a blank login form with no explanation, which is why
the Okta failure felt opaque.

**Fix:** `Login.tsx` now decodes `?error=` through a `CALLBACK_ERRORS` map and renders a
`role="alert"` banner. `not_invited` gets an actionable message plus a "Deploy one" link (the path the
person actually controls); other reasons get a specific message without that CTA. Unknown or crafted
codes fall back to a generic message rather than echoing the raw value onto the page.

**Result:** existing member (any role) → signs in, non-admins land on `/install`. Uninvited identity →
`not_invited` banner + deploy CTA, and **no workspace is created**. SCIM-provisioned and invited users
are unaffected: `scim/provisioner.go` writes `external_identities` in the same transaction as the
`users` row (ADR-024 invariant #4), so they resolve before provisioning is ever reached — the refusal
only applies to genuinely first-seen identities.

**Tests:** `internal/identity/enterprise_provisioning_test.go` (owning workspace threaded on the
enterprise tier; nil kept on the platform tier with the signup name preserved; `ErrNotInvited` reaches
the caller unwrapped so the callback can distinguish it; a real provisioning error is NOT reported as
not-invited; an existing member never reaches the provisioner). `Login.test.tsx` +5: the not_invited
message and its deploy CTA, a generic failure WITHOUT the CTA, no banner absent the param, an unknown
code not echoed, and the banner clearing on retry.

**Gates:** `go build`/`go vet ./...` clean; full backend suite green except the 7 pre-existing
`TestGroupOrigin_*` fixture failures (`workspaces_status_check`, unrelated). Frontend **90/90**,
`tsc --noEmit` clean, `pnpm build` clean. Uncommitted.

**Not addressed:** whether an enterprise user should instead be AUTO-provisioned as a plain member
(common in ZTNA federation). That is a product choice; the requirement here was explicitly to refuse.
Flipping it later is a one-line change at the `ErrNotInvited` return.
