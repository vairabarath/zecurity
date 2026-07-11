---
type: adr
status: pending
id: PENDING-07a
domain: operator
priority: P1
created: 2026-07-03
related:
  - PENDING-07-Provider-Dashboard-Vision
  - PENDING-07b-Provider-Console-Packaging
  - PENDING-01-Authenticated-Relay-Provisioning
  - PENDING-04-Multiple-IdPs-Enterprise-SSO
tags: [pending, adr, operator, provider, identity, authz, rbac]
---

# Pending ADR 07a — Provider Identity & Authorization Tier

> **Status: PENDING — for team discussion.** Backend half of [[PENDING-07-Provider-Dashboard-Vision]].
> This is the load-bearing decision and the real prerequisite to doing PENDING-01/02 *correctly*.

## Context / Current State

Relays are provider-owned platform infrastructure (`spiffe://zecurity.in/relay/<uuid>`, global
trust domain; the `relays` table has **no `tenant_id`** — already provider-shaped). But the only
way to create one today is `POST /api/relays`, guarded by `RequireRole("admin")` with **no
`WorkspaceGuard`** (`controller/cmd/server/main.go:250-251`) — meaning **any tenant admin could
mint relay provisioning tokens**. That's a privilege gap: provider operations are exposed through
the tenant authorization model.

The rest of the system is tenant-scoped: `users.tenant_id`, `WorkspaceGuard` derives tenant from
the JWT, sessions are workspace-bound, and auth is a single Google OIDC.

## Problem — Decision Needed

Is "provider" a distinct identity + authorization domain, and how is it modeled and authenticated?

## Architectural Invariant

Provider identities and tenant identities are separate security domains. Provider authorization
must never depend on tenant membership, and tenant authorization must never grant access to
provider-owned infrastructure.

## Options

### Option A — Separate identity tier (recommended)
A `provider_users` store (not the tenant `users` table), authenticated via the **provider's
corporate SSO** with a **separate session audience/cookie**, gated by a new `RequireProvider`
middleware that never touches `WorkspaceGuard`. All provider operations live behind a `/provider`
API surface. Authorization decisions funnel through **one policy function** (`canManage(actor,
target)`), which is trivial in alpha and gains role/partner-scoping later.
- **Pros:** clean tenant/provider isolation; no "which workspace does a provider user belong to?"
  problem; blast-radius contained; partner-scoping + SoD roles slot into one chokepoint later.
- **Cons:** a second identity/session path to build (small — can reuse the existing Google OIDC
  with a different audience + an allowlist for alpha).

### Option B — Global role on the existing `users` table
Add `role="provider"` and special-case `WorkspaceGuard` to bypass for it.
- **Pros:** fastest; one identity path. **Cons:** provider users don't fit the tenant model;
  bypass-exceptions inside tenant authz are where isolation bugs live; provider ends up
  authenticatable via tenant IdPs (dangerous once PENDING-04 lands).

### Option C — Identity broker owns provider identity too
If PENDING-04 picks a broker (Keycloak/WorkOS), model provider staff as a separate realm there.
- **Pros:** one identity system. **Cons:** couples provider access to the broker decision + uptime.

## Recommendation (non-binding)
**Option A.** For alpha, reuse the existing Google OIDC but issue a provider-scoped session and
gate on a `provider_users` **allowlist** (seed with your team's emails) — no new IdP work, but the
separate tier is established. Keep one `canManage` chokepoint. Enforce MFA via corp Google.

## Open Questions
- Provider sub-roles for GA (`relay-ops`, `token-management`, `billing`, `support`, `auditor`) and
  **separation of duties** (issue-token ≠ decommission-relay)? Alpha = single role.
- **Partner/reseller scoping:** stamp a nullable `provider_org_id` on `provider_users`/`relays`
  seeded to a single "root" org now, or keep the model flat and add scoping only in `canManage`
  later? (Cheaper: flat model + chokepoint; add column when partners are real.)
- **Break-glass impersonation** of tenant admins for support — in scope? Guardrails (scoped,
  time-boxed, audited, maybe dual-control)?
- Does provider auth share the OIDC code path or get its own module to avoid tenant/provider drift?

## Rough Effort / Priority
**Alpha slice: S–M** (allowlist + `RequireProvider` + `/provider` route group + audit). Full RBAC
+ partner scoping: **L**, later. **P1** — unblocks correct PENDING-01/02.
