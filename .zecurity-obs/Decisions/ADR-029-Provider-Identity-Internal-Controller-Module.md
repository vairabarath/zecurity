---
type: adr
status: accepted
id: ADR-029
domain: operator
priority: P1
created: 2026-09-28
decided: 2026-09-28
related:
  - ADR-021-Provider-Identity-and-Authorization
  - PENDING-07b-Provider-Console-Packaging
  - PENDING-12-Controller-HA-Multi-Region
amends:
  - ADR-021-Provider-Identity-and-Authorization   # authentication mechanism only
decision_record: docs/provider-dashboard-architecture-decisions.md — amendment 2026-09-26 (D-24…D-29)
tags: [adr, operator, provider, identity, authn, architecture]
---

# ADR-029 — Provider Identity Remains an Internal Controller Module

> **Status: ACCEPTED (2026-09-28).** Recorded before Sprint 21 Phase H ("Provider Identity Foundation") is implemented.
> It **amends [[ADR-021-Provider-Identity-and-Authorization]] in one respect only**: provider authentication is no longer the Google OIDC exchange. It is the Provider Identity Service with local accounts (Decision Record amendment 2026-09-26, D-24).
> The rest of ADR-021 stands: the separate `provider_users` identity tier, the audience-scoped provider JWT, and the `decide()` authorization chokepoint.

## Context

- The Decision Record amendment of 2026-09-26 (D-24…D-29) gives the provider plane its **own** identity: a Provider Identity Service with a pluggable `Authenticator` (local email + password first), a single session-issuing path, a dedicated provider JWT key and issuer, and `session_generation` revocation.
- That raises a packaging question: should Provider Identity be a **separate service** (its own binary, its own deployment, maybe an off-the-shelf IdP such as Keycloak), or a **module inside the controller**?
- **Facts that bear on it:**
  - Zecurity runs a **single controller instance** (D-02), and multi-replica HA is a separate later initiative (PENDING-12).
  - Provider operators are a handful of InkYank employees. There are no customer, partner or MSP logins (D-01, D-24).
  - Provider users, audit (`provider_audit_logs`) and the rate-limit state already live in the controller's Postgres and Valkey.
  - Every provider API (`/provider/*`) is served by the controller. The provider console (D-18) is a separate *frontend*, not a separate backend.
  - Zecurity is pre-production and the team has two engineers.

## Decision

**Provider Identity remains an internal module of the controller for now.**

- **Code:** `controller/internal/provider/`, covering the identity service, authenticators, password hashing, provider JWT issuing and verification, the login rate limiter, the store, and the `RequireProvider` middleware (`internal/middleware/provider.go`).
- **Runtime:** it runs in the controller process. It uses the controller's Postgres (`provider_users`, `provider_audit_logs`) and Valkey, and serves its endpoints under `/provider/*` on the controller's HTTP port.
- **Not** a separate binary, container, deployment or network service, and **not** an external IdP product. Nothing outside the controller mints or verifies provider tokens.

### Principle: authentication and authorization are independent

**Provider Identity owns authentication. Provider authorization (`super-admin`, `relay-ops`) stays independent of the authentication method.**

- Every authentication method produces the same `ProviderUser`: local password today; later Google Workspace, Azure AD, Okta or others as optional `Authenticator`s (D-29).
- Role, email and `session_generation` are always read from `provider_users`, never from the authentication method or an upstream IdP's claims. The existing authorization pipeline (`RequireProvider` → `Actor` → `Authz`/`decide()`) runs unchanged whatever method was used.
- Adding a login method must never require changing roles, the role matrix, `decide()`, the provider JWT format or revocation.
- An upstream IdP can prove *who* someone is. It never decides *what* they may do.

### Boundaries inside the controller (so the module stays extractable)

1. **One way in:** provider tokens are minted **only** by `IdentityService.IssueSession` and verified only by `VerifyProviderToken` / `RequireProvider`.
2. **Separate key material:** the provider JWT uses `PROVIDER_JWT_SECRET` and the issuer `zecurity-provider`, never the tenant `JWT_SECRET`. Startup refuses a reused tenant secret.
3. **No tenant coupling:**
   - provider identity code doesn't import tenant session or auth code for its sessions (`internal/auth` stays tenant-only);
   - tenant code never accepts provider tokens (the existing `aud` wall plus the separate issuer);
   - provider identity has no `tenant_id`, ever (ADR-021).
4. **Narrow surface:** other controller packages use only the middleware (`RequireProvider`, the `Actor` in context) and `Authz`/`decide()`. They don't read `provider_users` or password data directly.
5. **Data ownership:** `provider_users` and the password and session columns are read and written only through `provider.Store`.

## Consequences

- **Good:**
  - no new service to deploy, secure, monitor or keep available;
  - no service-to-service trust or token-verification network hop;
  - no second secret-distribution path;
  - it fits D-02 and the team's size.
- **Good:** the `Authenticator` seam and the single minting path (D-29) mean external IdPs, and a later extraction, don't change roles, token format or revocation.
- **Accepted costs:**
  - provider login availability is tied to controller availability;
  - a controller compromise is also a provider-identity compromise;
  - the controller's attack surface includes the provider login endpoint, which is mitigated by rate limiting, no account enumeration, a network-locked console origin (D-18), and mandatory super-admin TOTP before Sprint 22 (D-28).
- **Bootstrap and recovery** follow from being in-process:
  - **Bootstrap:** first-run bootstrap is **create-only**. It creates the first super-admin from `PROVIDER_BOOTSTRAP_EMAIL` / `PROVIDER_BOOTSTRAP_PASSWORD` and is ignored once any super-admin exists (D-26). No environment flag changes an existing account.
  - **Recovery:** break-glass recovery is planned as a **controller CLI subcommand** (`zecurity-controller provider recover-admin --email …`), run on the controller host with the controller's database access. Shell access to the host is the trust boundary. It's documented in Sprint 21 Phase H and not built yet; target: before production, at the latest with the super-admin TOTP follow-up.

## When to revisit

Reopen this ADR if any of these become true:

1. **Multi-replica controller / HA** (PENDING-12): the rate limiter and session checks already use shared stores (Valkey, Postgres), but ownership and failover need re-evaluation.
2. **Federation or SSO requirements** outgrow the `Authenticator` seam: e.g. the provider plane must itself act as an IdP for other internal tools, or needs SAML or SCIM for operators.
3. **Partner or MSP access** (a change to D-01), which brings delegated administration and a much larger operator population.
4. A **compliance or security requirement** for a physically separated admin identity plane.
5. **Independent scaling or availability** of provider login becomes necessary.

## Alternatives considered

- **A separate Provider Identity microservice now.** Rejected for now: it adds deployment, inter-service trust and operational load with no current need (single instance, few operators, pre-production).
- **An off-the-shelf IdP (e.g. Keycloak) as the provider identity plane.** Rejected for now: it's another critical system to run and secure. It stays possible later as an optional upstream `Authenticator` (D-29) without changing roles or the provider JWT.
- **Keep the direct Google OIDC exchange (ADR-021's original mechanism).** Superseded by D-24: the internal operator plane shouldn't depend directly on an external login provider.
