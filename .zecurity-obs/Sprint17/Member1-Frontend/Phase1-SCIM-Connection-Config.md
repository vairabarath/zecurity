---
type: phase
member: M1-Frontend
sprint: 17
phase: 1
title: SCIM Connection Configuration UI
status: implemented-unverified
depends_on: [M1-4, M1-2, M1-9]
tags: [react, admin, scim, frontend, pending-05]
---

# Phase 1 (FE) — SCIM Connection Configuration UI

> Depends on backend Phase 4 (profiles + mapping), Phase 2 (SCIM token auth), Phase 9 (connection lifecycle). Full spec: [[PENDING-05-SCIM-Implementation-Plan]] (Frontend item 1) · [[ADR-025-SCIM-Directory-Synchronization]] §3/§5.

## Goal
Let an admin configure SCIM for an IdP connection from the admin UI: pick the provider preset, review/set the identity-mapping fields, enable SCIM, mint a SCIM bearer token (shown once), and copy the SCIM base URL to paste into the IdP (Okta/Entra/JumpCloud/Keycloak).

## Backend prerequisites — CLOSED 2026-08-26 (commit b5c1bce)
The backend gap this phase was originally blocked on is **closed**. `WorkspaceIdpConnection` now exposes `subjectClaim`, `scimIdentifier` and `scimEnabled` for read (`graph/idp.graphqls`), and the write path exists as a dedicated `updateScimConfig(connectionId, UpdateScimConfigInput)` mutation covering all three (resolver: `controller/graph/resolvers/idp.resolvers.go:275`). The provider presets are exposed as `scimProviderProfiles: [ScimProviderProfile!]!` (resolver `:734`), carrying `defaultSubjectClaim`, `defaultScimIdentifier`, capability flags and `quirks`.

**No backend follow-up is required for this phase.** The whole phase — mapping fields, enable/disable, preset picker, base URL, token lifecycle — is buildable.

## Files
| File | Change |
| --- | --- |
| `admin/src/pages/IdpConnections.tsx` | **new** — list of workspace IdP connections; row shows status, SCIM on/off and last-sync, links to detail. |
| `admin/src/pages/IdpConnectionDetail.tsx` | **new** — connection header + the three SCIM cards. Selects its connection from `idpConnections` (no single-connection query exists). |
| `admin/src/components/scim/ScimConfigCard.tsx` | **new** — provider preset dropdown, mapping fields, SCIM enable toggle, mapping-change confirm dialog. |
| `admin/src/components/scim/ScimBaseUrlBox.tsx` | **new** — read-only `<origin>/scim/v2` + copy, with the "same URL for every connection; the token scopes it" note. |
| `admin/src/components/scim/ScimTokenPanel.tsx` | **new** — token list + mint/rotate/revoke, and the show-once plaintext dialog. |
| `admin/src/components/scim/BreakGlassDialog.tsx` | **new** — mandatory-reason override dialog; shows the server's verbatim refusal. |
| `admin/src/graphql/queries.graphql` | **+3 operations** — `GetIdpConnections` (incl. `subjectClaim`/`scimIdentifier`/`scimEnabled`), `GetScimTokens`, `GetScimProviderProfiles`. |
| `admin/src/graphql/mutations.graphql` | **+5 operations** — `UpdateScimConfig`, `MintScimToken`, `RotateScimToken`, `RevokeScimToken`, `EnableScimBreakGlass`. |
| `admin/src/App.tsx` | routes `/idp-connections` and `/idp-connections/:id` inside `AdminLayout`. |
| `admin/src/components/layout/Sidebar.tsx` | "Identity Providers" nav item in the ADMIN-only block. |
| `admin/src/generated/**` | regenerated via `npm run codegen`. |

## Steps
- [x] **Provider preset** — dropdown bound to `scimProviderProfiles` (okta/entra/jumpcloud/keycloak/generic), showing each preset's defaults and `quirks` so an admin can see what the connection's values deviate FROM.
- [x] **Mapping fields** — editable `subjectClaim` + `scimIdentifier` (OIDC subject claim + SCIM identifier that resolve to `external_identities.subject`), persisted via `updateScimConfig`. The UI confirms before saving a changed mapping on a SCIM-enabled connection, because the server force-disables SCIM in the same write.
- [x] **Enable SCIM** — the toggle is **not** pre-gated client-side. Attempt `updateScimConfig{scimEnabled:true}`; when the server refuses, surface the `enableScimBreakGlass` flow (mandatory reason input, audited) rather than a silent on-switch. The normal enable path requires a proven mapping (`MappingGate`). Disabling is always permitted — it is the fail-closed direction.
  > **Why no client-side gate:** `scimEnabledAllowed` exists only on `IdpTestResult`, never on `WorkspaceIdpConnection`, and it is deliberately **not** being added there. While `WithRoundTrip` has no production caller it would be a constant `false` for every connection — a field that looks like state but is a hardcoded no — and it would invite a client gate that has to be torn out when the probe lands. Obtaining it via `testIdpConnection` on render is also wrong: that runs a live discovery probe and is a user-initiated action, not a render dependency. The gate is server-side and fail-closed; the UI must not duplicate it.
  > ⚠️ **SCIM cannot reach the normal enable path today.** `MappingGateResult.WithRoundTrip` (`internal/scim/validation.go:137`) has no production caller — the probe-user round-trip M1-4b deferred to Phase 5 was never wired — so the mapping can never reach `proven`. **In practice the break-glass branch is the only reachable path**, and the "normal enable" branch is currently unexercisable. Build both branches, but expect only break-glass to fire until the backend probe lands. Do **not** work around this by weakening the gate client-side; fail closed is the ADR-025 §3.1 intent.
- [x] **SCIM base URL** — read-only copy box showing `<controller-origin>/scim/v2`. **The path is identical for every connection** — ADR-025 §7 scopes every SCIM request to `(workspace_id, connection_id)` derived from the presented bearer token, never from the URL. Do **not** build a per-connection path; the token is what distinguishes connections. Feasible now.
- [x] **Token lifecycle** — `mintScimToken` shows `plaintext` **once** (with a "copied, will not be shown again" warning), list via `scimTokens`, `rotateScimToken` (new plaintext once), `revokeScimToken`. Feasible now.

## Rules
- Never let the UI imply ADMIN alone can force-enable SCIM; the break-glass path must collect a reason and call `enableScimBreakGlass`.
- Plaintext token is shown exactly once; never persisted in client state beyond the render that displays it.

## Deferred / blocked
- Nothing is blocked. The backend gap closed in b5c1bce; see "Backend prerequisites — CLOSED" above.

## Audit notes
- **2026-08-26 — original backend gap CLOSED by b5c1bce.** Re-verified directly against `controller/graph/idp.graphqls` and `controller/graph/resolvers/idp.resolvers.go`: `subjectClaim`/`scimIdentifier`/`scimEnabled` are readable on `WorkspaceIdpConnection`, `updateScimConfig` exists (resolver `:275`), `scimProviderProfiles` exists (resolver `:734`). The "no read / no write / no preset surface" findings in the original spec text are stale — do not re-raise them.
- **`scimEnabledAllowed` is NOT a field on `WorkspaceIdpConnection` — by design, not by omission.** It lives only on `IdpTestResult`. A previous read of this spec treated the "toggle gated by `scimEnabledAllowed`" step as implementable against the connection type; it is not, and the decision (recorded 2026-08-26) is to keep it that way rather than expose a permanently-false field. No client-side pre-gate.
- **Still true (not a stale finding): `MappingGateResult.WithRoundTrip` (`controller/internal/scim/validation.go:137`) has no production caller** — only comments in `validation.go` and `validation_test.go` reference it. The mapping therefore never reaches `proven`, so break-glass remains the only reachable enable path. Do not "fix" this client-side.

## Ordering note (unblocks FE-2 and FE-4)
This phase created the host pages FE-2 needs: `admin/src/pages/IdpConnections.tsx` (list, route
`/idp-connections`) and `admin/src/pages/IdpConnectionDetail.tsx` (detail, route
`/idp-connections/:id`), both registered in `App.tsx` inside `AdminLayout` and reachable from the
ADMIN-only sidebar block as "Identity Providers". **FE-2 (health badge) now has pages to mount on.**
FE-4 remains standalone-buildable as its own `ScimConflicts.tsx` page.

Delivered here: page shells + `GetIdpConnections`/`GetScimTokens`/`GetScimProviderProfiles` queries,
`UpdateScimConfig`/`Mint`/`Rotate`/`RevokeScimToken`/`EnableScimBreakGlass` mutations, the
mapping+preset+enable card, the base-URL box, and the token lifecycle panel.

Note: the schema has no single-connection query, so the detail page selects its connection from the
(small, workspace-scoped) `idpConnections` list rather than adding a backend field this phase does
not own.

## Build gate
Automated gates, run 2026-08-26 — all green:
- `npm run codegen` — 8 new documents generated (`GetIdpConnections`, `GetScimTokens`, `GetScimProviderProfiles`, `UpdateScimConfig`, `MintScimToken`, `RotateScimToken`, `RevokeScimToken`, `EnableScimBreakGlass`).
- `npm run build` — `tsc -b && vite build` green, 2439 modules.
- `npm run lint` — 18 problems (12 errors, 6 warnings), byte-identical to the pre-change baseline measured by stashing this work; **zero introduced**, and no new file is flagged.
- `npm run test` — 5 files / 16 tests passed.

**Status is `implemented-unverified`, not `done`.** The manual half of the gate has NOT been run: no
live IdP was wired up, so minting a token and pasting the base URL into Okta/Entra was not exercised,
and the break-glass enable path was not fired against a real refusal. Marking this `done` requires
that manual pass.
