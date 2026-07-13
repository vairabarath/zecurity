---
type: vision
status: pending
id: PENDING-07
domain: operator
priority: P1
created: 2026-07-03
supersedes: PENDING-07-Provider-Operator-Plane (folded into this + 07a + 07b)
related:
  - ADR-021-Provider-Identity-and-Authorization
  - PENDING-07b-Provider-Console-Packaging
  - ADR-020-Authenticated-Relay-Provisioning
  - PENDING-02-Certificate-Revocation-Enforcement
tags: [pending, vision, operator, provider, multi-tenant, roadmap]
---

# Provider Dashboard — Production Vision

> **Status: PENDING — vision doc for team alignment.** This is the north-star for the
> provider/operator plane. The two concrete decisions under it are
> [[ADR-021-Provider-Identity-and-Authorization]] (backend identity + authz tier) and
> [[PENDING-07b-Provider-Console-Packaging]] (separate app vs shared, deployment). Alpha scope is
> defined at the bottom.

## What this is

An internal **control plane for the ZTNA provider** — the company operating Zecurity — to manage
**shared platform infrastructure and tenants across all workspaces**. It is *not* the tenant admin
app (`admin/`), which manages a single workspace. Relays, the Platform Intermediate CA, tenant
lifecycle, and (eventually) delegated partner administration all live here.

There is no public reference architecture for this — a vendor's own operator console is internal
crown-jewel tooling and nobody documents it. The right mental model is **"internal control plane
for a multi-tenant SaaS"** (Tailscale/Cloudflare-style ops), not anything ZTNA-specific.

## Design philosophy (the non-negotiables)

1. **Highest-privilege surface in the product.** It signs relay certs (mints platform identity),
   revokes certs, suspends tenants, and sees across all customers. Design it like privileged
   production infrastructure access, not like a web app.
2. **Separate identity tier.** Provider staff are your employees, authenticated via *your* corporate
   SSO with mandatory MFA — never workspace users, never via tenant IdPs. (See 07a.)
3. **Single authorization chokepoint, least privilege, separation of duties.** One place decides
   "can this provider actor do this," so roles/partner-scoping slot in without touching every
   endpoint.
4. **Network-isolated.** Not internet-facing — VPN / IP-allowlist / separate domain.
5. **Everything audited.** Immutable trail of every provider action (who signed a relay, who
   suspended a tenant, who impersonated).
6. **Multi-party ready.** Provider + partners/resellers as first-class scoped orgs with delegated
   admin — designed-for now, built later.
7. **Build boundaries now, defer surface area.** Identity tier, authz chokepoint, audit, and
   network isolation are expensive to retrofit; RBAC granularity, partner self-service, and UI
   polish are cheap to add once the seams exist.

## Functionality catalog (production-grade)

### 1. Relay fleet management *(alpha core)*
- Register a relay; issue single-use, TTL'd provisioning tokens; distribute securely.
- Relay inventory: status, version, hostname, observed address, cert serial + **expiry runway**,
  capacity (`connection_count`/`max_connections`, fill ratio, tier label).
- Revoke / decommission a relay (→ CRL, PENDING-02).
- Placement observability: which connectors are on which relay, migration/probe/RTT health,
  failover convergence (ADR-016 Phase 3D).
- Region/pool grouping; capacity planning.

### 2. Tenant (workspace) lifecycle
- Create / provision / suspend / resume / delete workspaces.
- Tenant inventory: plan, status, usage, connector/shield/client counts, health.
- Feature flags / entitlements per tenant.
- **Break-glass support access** — scoped, time-boxed, fully audited impersonation of a tenant
  admin (a major security decision; see 07a open questions).

### 3. Fleet health & observability (cross-tenant)
- Global health of relays, connectors, shields, clients.
- Fleet-wide cert-expiry runway and renewal status.
- Alerting: relay outage, capacity exhaustion, failover non-convergence, cert expiry.
- Backed by PENDING-10 (observability).

### 4. PKI & trust management
- Intermediate CA visibility + rotation planning.
- Relay cert signing policy + TTLs; renewal triggers.
- Revocation management — trigger + view CRL (PENDING-01/02).

### 5. Billing, plans & quotas
- Plan definitions + tier limits (max connectors/clients/relays/bandwidth).
- Usage metering + **quota enforcement**.
- Invoicing (Stripe) / usage export. *(Start at quotas + metering; defer invoicing — see 07 alpha.)*

### 6. Partner / reseller management (multi-party plane)
- Provider-orgs / partners as first-class entities.
- Delegated administration: a partner manages a **scoped subset** of relays/tenants and sees only
  those.
- Partner-level roles and limits.

### 7. Provider RBAC & separation of duties
- Provider roles, e.g.: `super-admin`, `relay-ops` (build/decommission relays), `token-management`
  (issue provisioning tokens), `billing`, `support` (read-only + break-glass), `auditor` (read
  audit only).
- Separation of duties — e.g., token issuance separated from relay decommission; destructive
  actions require step-up. *(This is exactly the "dedicated access for the token / relay-building
  teams" requirement.)*

### 8. Audit & compliance
- Immutable audit log of every provider action; per-actor, per-target.
- SIEM/export (PENDING-11). Retention + tamper-evidence.

### 9. Console security controls
- Provider IdP (corp SSO) + mandatory MFA / hardware keys.
- Network isolation (VPN/allowlist/separate domain); session policies; step-up for destructive ops.
- Documented break-glass procedure.

## Phased roadmap

| Phase | Scope | Identity / RBAC | UI |
|-------|-------|-----------------|-----|
| **Alpha** *(now, internal)* | Relay lifecycle only: create / issue token / list+health / revoke. Audit every action. | Single provider org, single role, corp-SSO allowlist, network-locked | **CLI** against a secured `/provider` API (React deferred) |
| **Beta** | + Fleet health views, tenant lifecycle (create/suspend), cert-expiry runway, a few provider roles | Provider RBAC (3–4 roles), still internal (+ trusted early partners maybe) | **Separate React app**, network-locked |
| **GA / production** | + Partner/reseller multi-tenancy w/ delegated scoped admin, billing/quotas, full observability + alerting, PKI/CA management, break-glass impersonation, SIEM export, SoD-enforced roles | Full provider RBAC + partner-org scoping + SoD | Hardened separate app |

## Alpha decision (what we build first)

> A `provider_users` allowlist authenticated via corporate Google (separate session/audience) →
> a `/provider` API tier behind `RequireProvider` (no `WorkspaceGuard`) → relay create/token/
> list/revoke → **every action audited** → driven by a **CLI** for the internal alpha →
> network-locked → with authz kept in **one policy chokepoint** so partner-scoping and roles slot
> in later.

This ships the two P0 fixes ([[ADR-020-Authenticated-Relay-Provisioning]],
[[PENDING-02-Certificate-Revocation-Enforcement]]) *correctly*, establishes the provider plane's
hard boundaries, and defers the partner/RBAC/billing machinery until it's actually being designed.

## Open questions for the team
- Is the alpha **internal-only**, or do partners touch it during alpha? (If partners are in alpha,
  org-scoping becomes real work now, not deferred.)
- **CLI-first vs React-first** for alpha — optimize for "prove the secure model fast" or "something
  clickable to demo"?
- Meterable units + plan tiers (drives billing scope)?
- **BYO-relay?** If tenants ever run their own relays, `relays` gains a `tenant_id` and the
  "purely provider-owned" assumption shifts — decide before the data model sets.
- Break-glass impersonation in scope for GA, and what audit/authorization guardrails?
