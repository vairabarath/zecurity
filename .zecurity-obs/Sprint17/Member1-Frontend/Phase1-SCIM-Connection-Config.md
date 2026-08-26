---
type: phase
member: M1-Frontend
sprint: 17
phase: 1
title: SCIM Connection Configuration UI
status: pending
depends_on: [M1-4, M1-2, M1-9]
tags: [react, admin, scim, frontend, pending-05]
---

# Phase 1 (FE) — SCIM Connection Configuration UI

> Depends on backend Phase 4 (profiles + mapping), Phase 2 (SCIM token auth), Phase 9 (connection lifecycle). Full spec: [[PENDING-05-SCIM-Implementation-Plan]] (Frontend item 1) · [[ADR-025-SCIM-Directory-Synchronization]] §3/§5.

## Goal
Let an admin configure SCIM for an IdP connection from the admin UI: pick the provider preset, review/set the identity-mapping fields, enable SCIM, mint a SCIM bearer token (shown once), and copy the SCIM base URL to paste into the IdP (Okta/Entra/JumpCloud/Keycloak).

## Backend prerequisites / gaps (MUST be closed before this phase is buildable)
The `WorkspaceIdpConnection` GraphQL type currently exposes **none** of the SCIM config fields — only `scimEnabledAllowed` (a computed gate), `identityHealth`, `lastSyncAt`. Verified missing:
- **No read** of `subjectClaim`, `scimIdentifier`, `scimEnabled` on `WorkspaceIdpConnection` (graph/idp.graphqls).
- **No write** of those fields: `UpdateIdpConnectionInput` has only `displayName/clientId/clientSecret/discoveryUrl/scopes/domainHint` (graph/idp.graphqls). `scim_enabled` is only set via `enableScimBreakGlass` (unproven-mapping override) or `testIdpConnection` — there is no clean "enable SCIM (mapping proven)" toggle nor a disable path.
- No provider-preset selection surface (presets live in `internal/scim/profiles.go`, not exposed to FE).

Until these are added (a small backend follow-up: expose the fields on the connection type + an `updateScimConfig` mutation covering `subjectClaim`, `scimIdentifier`, `scimEnabled`), the UI can only render the *token* and *base-URL* parts below. The phase is split accordingly.

## Files
| File | Change |
| --- | --- |
| `admin/src/pages/IdpConnections.tsx` | **new** — list of workspace IdP connections. **No connection query exists yet** — `admin/src/graphql/queries.graphql` contains zero `idpConnection*` operations, so this phase must author the list/detail queries from scratch against `idp.graphqls`. |
| `admin/src/pages/IdpConnectionDetail.tsx` | **new** — connection detail with a "SCIM" tab/section. |
| `admin/src/components/scim/ScimConfigCard.tsx` | **new** — provider preset select, mapping fields (subject claim / SCIM identifier), enable toggle, base-URL copy box, token mint/rotate/revoke panel. |
| `admin/src/graphql/queries.graphql` + `mutations.graphql` | add `IdpConnection.scimEnabled/scimIdentifier/subjectClaim` (after backend exposes them) + `updateScimConfig` mutation; `mintScimToken`/`rotateScimToken`/`revokeScimToken`/`scimTokens` already exist in schema. |
| `admin/src/generated/graphql.ts` | regenerated via `npm run codegen`. |

## Steps
- [ ] **Provider preset** — dropdown bound to the built-in profiles (okta/entra/jumpcloud/keycloak/generic) with per-connection override display. (Backend: expose preset/override.)
- [ ] **Mapping fields** — editable `subjectClaim` + `scimIdentifier` (OIDC subject claim + SCIM identifier that resolve to `external_identities.subject`). (Backend: expose + persist.)
- [ ] **Enable SCIM** — toggle gated by `scimEnabledAllowed`; when the mapping is unproven, surface the `enableScimBreakGlass` flow (mandatory reason input, audited) rather than a silent on-switch. Normal enable path requires a proven mapping (Phase 4 `MappingGate`). (Backend: add clean enable/disable mutation.)
  > ⚠️ **`scimEnabledAllowed` is `false` for every connection today.** `MappingGateResult.WithRoundTrip` (`internal/scim/validation.go:137`) has no production caller — the probe-user round-trip M1-4b deferred to Phase 5 was never wired — so the mapping can never reach `proven`. **In practice the break-glass branch is the only reachable path**, and the "normal enable" branch is currently unexercisable. Build both branches, but expect only break-glass to fire until the backend probe lands. Do **not** work around this by weakening the gate client-side; fail closed is the ADR-025 §3.1 intent.
- [ ] **SCIM base URL** — read-only copy box showing `<controller-origin>/scim/v2`. **The path is identical for every connection** — ADR-025 §7 scopes every SCIM request to `(workspace_id, connection_id)` derived from the presented bearer token, never from the URL. Do **not** build a per-connection path; the token is what distinguishes connections. Feasible now.
- [ ] **Token lifecycle** — `mintScimToken` shows `plaintext` **once** (with a "copied, will not be shown again" warning), list via `scimTokens`, `rotateScimToken` (new plaintext once), `revokeScimToken`. Feasible now.

## Rules
- Never let the UI imply ADMIN alone can force-enable SCIM; the break-glass path must collect a reason and call `enableScimBreakGlass`.
- Plaintext token is shown exactly once; never persisted in client state beyond the render that displays it.

## Deferred / blocked
- Persisting mapping fields + a clean enable/disable toggle is **blocked on a backend GraphQL gap** (documented above). Mark this phase done only after that backend work lands and the UI binds to it.

## Ordering note (blocks FE-2 and, optionally, FE-4)
There is **no IdP UI in the admin app today** — `admin/src/pages/` contains no `IdpConnections.tsx` or
`IdpConnectionDetail.tsx`, and the only match for `IdpConnection` under `admin/src` is dead codegen
output in `generated/graphql.ts`. This phase creates both host pages. **FE-2 (health badge) has no
page to mount on until the unblocked half of this phase lands** — see
[[Sprint17/Member1-Frontend/Phase2-Identity-Health-Indicator]]. FE-4 can avoid the dependency by
shipping its standalone `ScimConflicts.tsx` page.

Recommended split: land the **unblocked half first** (page shells + connection list/detail queries +
base-URL box + token lifecycle panel), then FE-2/FE-4, then return for the mapping/enable half once
the backend gap closes.

## Build gate
`cd admin && npm run codegen && npm run build` green; manual: open a connection, mint a token (plaintext shown once), copy base URL. Mapping/enable steps gated behind backend gap closure.
