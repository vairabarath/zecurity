---
type: phase
member: M1-Frontend
sprint: 17
phase: 0
title: Enterprise IdP Connection Onboarding
status: implemented-unverified
depends_on: []
tags: [react, admin, idp, oidc, frontend, onboarding, pending-04, pending-05]
---

# Phase 0 (FE) — Enterprise IdP Connection Onboarding

> Bridges PENDING-04 / ADR-024 backend capability into the frontend, unblocking the Sprint 17 SCIM onboarding path (PENDING-05 / ADR-025). Full spec: [[ADR-024-Identity-Linking-and-Provider-Migration]] §0 · [[ADR-025-SCIM-Directory-Synchronization]] §3.

## Goal
Allow a workspace administrator (`ADMIN` role) to create an Enterprise OIDC Identity Provider connection directly from the **Identity Providers** page (`/idp-connections`), replacing the dead-end empty-state with an actionable onboarding flow that feeds directly into FE-1's SCIM configuration.

## Context & Problem
Sprint 17 SCIM frontend phases (FE-1 through FE-5) configure SCIM directory sync atop existing IdP connections. While the backend has supported connection creation since PENDING-04 via `createIdpConnection` (`controller/graph/idp.graphqls`), the admin frontend previously lacked a creation UI. Fresh workspaces saw a placeholder empty state pointing them to "Settings", where no IdP management existed.

## Expected Flow
```text
Identity Providers (/idp-connections)
    ↓
Click "Add Identity Provider" (Header action / Empty-state CTA)
    ↓
Dialog Form: Display Name, Provider Preset, Issuer, Client ID, Client Secret (Write-Only), Optional Advanced (Discovery URL, Scopes, Domain Hint)
    ↓
Submit createIdpConnection mutation
    ↓
Connection created in database (client secret encrypted at rest)
    ↓
Dialog closes + Toast success + Connection list refetches
    ↓
Admin clicks newly created connection → navigates to /idp-connections/:id
    ↓
Continues into FE-1 SCIM Configuration & Token Minting
```

## Backend Contract (Verified — Reused As-Is)
- **GraphQL Mutation:** `createIdpConnection(input: CreateIdpConnectionInput!): WorkspaceIdpConnection! @hasRole(roles: [ADMIN])` in `controller/graph/idp.graphqls:187`
- **GraphQL Input:** `CreateIdpConnectionInput` in `controller/graph/idp.graphqls:66-75`:
  - `provider: String!` — Resolver key (`"okta"`, `"entra"`, `"jumpcloud"`, `"keycloak"`, `"oidc"`)
  - `displayName: String!` — Human-readable label (e.g. "Corporate Okta")
  - `issuer: String!` — OIDC Issuer URL (immutable identity anchor, e.g. `https://acme.okta.com`)
  - `clientId: String!` — OAuth 2.0 Client ID
  - `clientSecret: String!` — OAuth 2.0 Client Secret (write-only; encrypted at rest via `pki.EncryptSecret`)
  - `discoveryUrl: String` — Optional OIDC discovery override (defaults to derivation from `issuer` + `/.well-known/openid-configuration`)
  - `scopes: String` — Optional scopes override (defaults server-side to `"openid email profile"`)
  - `domainHint: String` — Optional email-domain hint for candidate IdP routing
- **Backend Authorization & Security:**
  - Strict `@hasRole(roles: [ADMIN])` directive enforced.
  - Secret encryption: Handled automatically by `IdpStore.CreateWorkspaceConnection` using `pki.EncryptSecret(ctx, "idp-client-secret:"+tenantID, secret)`.
  - Secret redaction: `clientSecret` is never returned in `WorkspaceIdpConnection` queries or mutation results.

## Files
| File | Change |
| --- | --- |
| `admin/src/graphql/mutations.graphql` | **add** `CreateIdpConnection` mutation document. |
| `admin/src/components/idp/CreateIdpConnectionDialog.tsx` | **new** — dialog form collecting required & optional connection fields with security guards. |
| `admin/src/pages/IdpConnections.tsx` | **update** — add "Add Identity Provider" button in header and actionable CTA in `EmptyState` replacing the misleading Settings text. |
| `admin/src/generated/**` | regenerated via `npm run codegen`. |

## Form Fields & UI Validation
### Required Fields:
1. **Provider (`provider`)**: Dropdown selector offering recognized backend presets:
   - `okta` ("Okta")
   - `entra` ("Microsoft Entra ID")
   - `jumpcloud` ("JumpCloud")
   - `keycloak` ("Keycloak")
   - `oidc` ("Generic OIDC")
2. **Display Name (`displayName`)**: Text input (e.g., "Corporate Okta", "Okta Workforce").
3. **Issuer URL (`issuer`)**: Valid HTTP/HTTPS URL (e.g., `https://dev-123456.okta.com`).
4. **Client ID (`clientId`)**: Non-empty text string from IdP app registration.
5. **Client Secret (`clientSecret`)**: Password-type input (write-only, masked).

### Optional / Advanced Fields (Collapsible / Accordion):
6. **Discovery URL (`discoveryUrl`)**: Optional URL override.
7. **Scopes (`scopes`)**: Text input pre-filled with `openid email profile`.
8. **Domain Hint (`domainHint`)**: Optional email domain (e.g. `acme.com`).

## Security Rules (Critical)
- **Zero Client-Side Persistence:** `clientSecret` must **never** be stored in Apollo cache, Zustand, localStorage, sessionStorage, or URL query params.
- **Form State Cleanup:** The form state containing `clientSecret` must be cleared immediately upon dialog close or successful submission.
- **No Console Logging:** Form submit handlers must never log the raw input object or secret.
- **No In-UI Secret Echo:** Mutation response returns `WorkspaceIdpConnection` (which does not expose `clientSecret`); the UI must never attempt to display or echo back the secret after creation.

## UX & Navigation
- On successful mutation:
  1. Trigger `toast.success("Identity provider connection created.")`.
  2. Close creation dialog and clear form state.
  3. Refetch connection list via Apollo `refetchQueries` or `onSuccess` callback.
  4. (Optional UX convenience) Option to remain on `/idp-connections` or immediately navigate to `/idp-connections/${created.id}`. Standard convention: refetch list so the new connection is visible in the table/card list.

## Out of Scope
- Modifying backend schema, resolvers, stores, or migration files (backend `createIdpConnection` is verified complete).
- Connection deletion or updates (handled separately; this phase is strictly onboarding/creation).
- SCIM configuration / SCIM tokens (owned by FE-1).
- Health monitoring badges (owned by FE-2).
- Conflicts queue (owned by FE-4).
- Identity provider login / authentication federation execution.

## Dependencies & Relationship to Sprint 17
```text
FE-0 (Enterprise IdP Onboarding)
  └── Creates identity_connections row
        ↓
FE-1 (SCIM Config & Tokens)
  └── Mounts on connection, configures SCIM provider preset, mapping, & token minting
        ↓
FE-2 (Identity Health Indicator)
  └── Evaluates sync health on the connection
        ↓
FE-3 / FE-4 / FE-5 (Read-only User fields, Conflict queue, Group origin labels)
```

## Build Gates
- `cd admin && npm run codegen && npm run build` green.
- `npx vitest run` passes without regression.
- `npx eslint` across touched files exits 0.

## Manual Acceptance Test
1. Access a workspace with 0 IdP connections at `/idp-connections`.
2. Verify empty state displays actionable "Add Identity Provider" button (no references to Settings).
3. Click "Add Identity Provider"; dialog opens with provider preset dropdown and masked secret input.
4. Attempt submit with missing required fields → client validation blocks submit.
5. Fill valid OIDC details (Provider: Okta, Display Name: "Test Okta", Issuer: `https://example.okta.com`, Client ID: `client_123`, Secret: `secret_abc`) and submit.
6. Verify toast notification appears, dialog closes, and "Test Okta" appears in the connection list.
7. Click the new connection row; verify navigation to `/idp-connections/:id` where FE-1's SCIM configuration cards render cleanly.
8. Verify client secret is nowhere in browser DOM, localStorage, or network response payload.

## Audit Notes
- **2026-08-26 — Implemented (Unverified).**
  - Added `CreateIdpConnection` mutation document to `admin/src/graphql/mutations.graphql` mapping to existing backend resolver `createIdpConnection`.
  - Created `admin/src/components/idp/CreateIdpConnectionDialog.tsx` with preset selector (`okta`, `entra`, `jumpcloud`, `keycloak`, `oidc`), required OIDC inputs, masked write-only client secret, and collapsible advanced options.
  - Updated `admin/src/pages/IdpConnections.tsx` to add "Add Identity Provider" header button and actionable `EmptyState` CTA replacing the misleading Settings text.
  - Created `admin/src/components/idp/CreateIdpConnectionDialog.test.tsx` (4 tests).
  - Automated gates: `npm run codegen` generated, `npm run build` (`tsc -b && vite build`) green, `npm test` **10 files / 50 tests pass** (FE-0 delta = **+1 file / +4 tests**), `npx eslint` on touched files exited 0.
- **Manual Gate NOT Run:** Verification requires creating a connection against a running backend instance in the browser and confirming that navigation into `/idp-connections/:id` exposes FE-1 SCIM configuration as expected. Status marked `implemented-unverified` per project conventions.

---

## Post-Phase Fixes

### Fix: "Create Connection" saved an unverified configuration and reported success (2026-08-31)

**Issue:** This phase's `CreateIdpConnectionDialog` collected Okta/OIDC details and, on submit,
reported `"Identity provider connection created."` — but `createIdpConnection` performed **no network
call of any kind**. `IdpStore.CreateWorkspaceConnection` encrypted the secret, inserted the row and
wrote an audit entry, nothing more. A mistyped domain or a revoked secret saved cleanly, and the
guided wizard (FE Phase 6) advanced straight into SCIM on top of a connection that had never been
contacted. The operator could not tell *configuration saved* from *IdP reachable*.

**Root cause:** A correct discovery client existed (`OIDCProvider.Probe`,
`internal/auth/providers/oidc.go`) but its only caller was the `testIdpConnection` resolver, which
has no GraphQL document in `admin/src/graphql/` and no UI affordance — so no create-time or
post-create verification was reachable from the admin UI at all.

**Fix applied — backend validates before persisting (Option A):**

```go
// graph/resolvers/idp.resolvers.go — CreateIdpConnection
// BEFORE:
tc := tenant.MustGet(ctx)
created, err := r.IdpStore.CreateWorkspaceConnection(ctx, tc.TenantID, idp.CreateInput{...})

// AFTER:
tc := tenant.MustGet(ctx)
if err := validateOIDCDiscovery(ctx, input.Provider, input.Issuer,
    deref(input.DiscoveryURL), deref(input.Scopes)); err != nil {
    return nil, err // nothing is persisted
}
created, err := r.IdpStore.CreateWorkspaceConnection(ctx, tc.TenantID, idp.CreateInput{...})
```

**Two traps, both load-bearing — do not undo them:**

1. **Do not wire `testIdpConnection` to a "Test Connection" button.** It also runs `ProbeMapping` and
   then unconditionally calls `SetSCIMEnabled(..., false)` (the Phase 13 / C1 invariant), so clicking
   "Test" on a healthy SCIM-enabled connection would **silently force-disable SCIM**. A real Test
   Connection button needs a new discovery-only resolver.
2. **`ProbeFresh`, not `Probe`.** `discoveryCache` is keyed on the **issuer alone**, process-global,
   1h TTL, while `discoveryEndpoint()` prefers an explicit `discoveryUrl` override — so a
   cache-consulting check can pass with no request at all on an issuer already warmed by a login or by
   *another workspace's* connection, and never fetches a bogus override.

**UI wording (this phase's dialog) — honest scope only:**

```tsx
// BEFORE:
toast.success('Identity provider connection created.')
{loading ? 'Creating…' : 'Create Connection'}

// AFTER:
toast.success('Connection created — OIDC discovery verified.')
{loading ? 'Verifying…' : 'Create Connection'}
// plus a note: on save Zecurity verifies the domain serves a valid OpenID Connect
// discovery document and will not create the connection otherwise; the client ID,
// client secret and redirect URI are NOT verified until the first sign-in.
```

**Verified at create:** issuer reachability + OIDC discovery validity (200, `issuer` match, required
endpoints). **NOT verified:** client ID, client secret, redirect URI, actual authentication, SCIM
connectivity. Never word this as "credentials verified" — discovery is unauthenticated and the
provider is deliberately constructed with **empty** client ID and secret, so no credential is sent.

**Related files also changed:** `internal/auth/providers/oidc.go` (cache-neutral `ProbeFresh`),
`graph/resolvers/idp_helpers.go` (`validateOIDCDiscovery`), `graph/resolvers/idp.resolvers.go`
(`UpdateIdpConnection` validates a `discoveryUrl` override too). No schema change, no migration, no
GraphQL schema change. Full write-up: `Sprint17/path.md` → **Finding D**.
