---
type: adr
status: implemented
id: PENDING-04
domain: identity
priority: P1
created: 2026-07-03
related:
  - ADR-005-Email-Normalization
  - ADR-006-Refresh-Token-Rotation
tags: [pending, adr, identity, sso, oidc, saml]
---

# Pending ADR 04 — Multiple IdPs & Enterprise SSO

> ~~**Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.~~
> *(superseded — see the correction below)*

> **Correction (2026-09-01, verified against source):** this is **IMPLEMENTED** — the frontmatter
> said `pending` long after the work landed. Per-workspace IdP configuration lives in
> `controller/internal/idp/store.go` on the `identity_connections` table
> (`controller/migrations/031_identity_federation.sql`), the Google hardwiring described below is
> gone (provider abstraction under `controller/internal/auth/providers/`), and
> `controller/internal/auth/discovery.go` returns a workspace's connections with a
> `platformFallback` flag. The resolved design is [[ADR-023-Identity-Philosophy]] /
> [[ADR-024-Identity-Linking-and-Provider-Migration]] / [[ADR-026-Identity-Governance-and-Identity-Linking]].
> SAML is still not implemented — OIDC only. The "Context / Current State" below is the
> 2026-07-03 pre-implementation snapshot; read it as history.

## Context / Current State

Authentication is **hardwired to Google**. The `AuthService` interface is Google-shaped
(`controller/internal/auth/service.go`: `ExchangeCode` = "Google OAuth", `VerifyIDToken` =
"Google ID token", `GoogleClaims`, `GoogleTokenResponse`), config carries `GoogleClientID` /
`GoogleClientSecret`, and `idtoken.go` calls `VerifyGoogleIDToken`. There is no provider
abstraction, no per-workspace IdP configuration, and no SAML.

This blocks selling to any org not fully on Google Workspace — enterprise buyers expect Okta,
Entra ID (Azure AD), Ping, or generic OIDC/SAML, usually configured **per tenant**.

## Problem — Decision Needed

What identity-federation model do we support, and how is it scoped (global vs per-workspace)?

## Options

### Option A — Generic OIDC provider abstraction
Refactor `AuthService` into a provider interface; support any OIDC IdP via discovery
(`/.well-known/openid-configuration`); store per-workspace IdP config (issuer, client id/secret,
claims mapping).
- **Pros:** covers Google, Okta, Entra, Auth0, Ping in one abstraction; incremental from today.
- **Cons:** doesn't cover SAML-only orgs; per-tenant secret storage needed.

### Option B — OIDC + SAML 2.0
Add SAML SP support alongside OIDC.
- **Pros:** covers the enterprise long tail (many orgs are SAML-first). **Cons:** SAML is heavier
  (metadata, signing, assertion parsing); more attack surface.

### Option C — Delegate to an identity broker (Keycloak / Dex / WorkOS / Auth0)
Front all IdPs with a broker; controller trusts one OIDC.
- **Pros:** offloads SAML/OIDC breadth + MFA + SCIM to a mature component; fastest breadth.
- **Cons:** new infra dependency (or vendor cost/lock-in); less control.

## Recommendation (non-binding)
Option A first (generic per-workspace OIDC — unblocks most enterprise buyers with moderate
effort), then evaluate B vs C for SAML/long-tail. A broker (C) is worth costing early if it also
solves PENDING-05 (SCIM) and PENDING-06 (MFA) in one move.

## Open Questions
- Global vs per-workspace IdP config (multi-tenant almost certainly needs per-workspace)?
- Account linking / JIT provisioning rules; claim→role/group mapping; email as the join key
  (interacts with ADR-005 email normalization)?
- Secret storage for per-tenant client secrets (see PENDING-07 / secrets management)?

## Rough Effort / Priority
**M–L, P1.** Foundational for enterprise sales; pairs with PENDING-05/06.
