---
type: adr
status: in-progress
id: PENDING-05
domain: identity
priority: P2
created: 2026-07-03
related:
  - PENDING-04-Multiple-IdPs-Enterprise-SSO
  - ADR-025-SCIM-Directory-Synchronization
tags: [pending, adr, identity, scim, provisioning]
---

# Pending ADR 05 — Directory Sync (SCIM)

> **Status: PENDING — proposed design captured in [[ADR-025-SCIM-Directory-Synchronization]] (PROPOSED, 2026-08-06).**
> Awaiting team ratification. **Implementation does not begin until ADR-025 is ACCEPTED** — at which
> point ADR-025 becomes the authoritative SCIM design (exactly as ADR-024 is authoritative for identity
> federation). The options/recommendation below are the original framing; the resolved decisions live in ADR-025.

> **Correction (2026-09-01, verified against source):** ADR-025 was ACCEPTED and the work is **largely
> built** as Sprint 17 — the frontmatter said `pending` throughout. The SCIM engine is
> `controller/internal/scim/*` with migration `034` (which now includes the former 036 idempotent index guard); all 13 backend phases in
> `.zecurity-obs/Sprint17/path.md` are checked. **Still open:** FE Phase 7 (`status: in-progress`) —
> per-connection sign-in buttons on `Login.tsx` (F7-5) and disable/delete connection actions on
> `IdpConnectionDetail.tsx` (F7-8); FE Phases 0–6 are `implemented-unverified`; and **no SCIM
> conformance suite exists** — live Okta/Entra interop is still gated on tenant access. Hence
> `in-progress`, not `implemented`.

## Context / Current State

Users land in the system JIT at first Google login; **groups are managed manually in-app**
(the Sprint 8 policy engine: `groups`, `group_members`, `access_rules`). There is no automated
provisioning/deprovisioning from an external directory. When an employee is offboarded in
Okta/Entra, nothing removes their access here automatically — an admin must do it by hand. For a
ZTNA product this is both a security gap (stale access) and an admin-toil gap.

## Problem — Decision Needed

How do we keep users and groups in sync with customers' directories?

## Options

### Option A — SCIM 2.0 server
Implement a SCIM endpoint so IdPs (Okta/Entra) push users + groups.
- **Pros:** industry standard; near-real-time deprovisioning; maps cleanly onto the existing
  `groups`/`group_members` schema. **Cons:** SCIM spec surface (Users, Groups, PATCH semantics);
  per-tenant bearer tokens.

### Option B — Pull-based directory sync
Controller periodically pulls from the directory API (Google Directory / MS Graph).
- **Pros:** no inbound endpoint; full control of cadence. **Cons:** per-provider integrations;
  polling lag; API scopes/consent.

### Option C — Broker-provided (if PENDING-04 Option C chosen)
Let the identity broker (WorkOS/Auth0/Keycloak) own SCIM; controller consumes normalized events.
- **Pros:** one integration. **Cons:** depends on the broker decision.

## Recommendation (non-binding)
If PENDING-04 lands generic OIDC, add **SCIM 2.0 (Option A)** — it's what enterprise admins expect
and directly powers the policy engine. If a broker is chosen in PENDING-04, prefer C.

## Open Questions
- Group-name → Zecurity group mapping and conflict handling with manually-created groups?
- Deprovisioning semantics: disable vs delete; revoke device certs on deprovision (ties to
  PENDING-02 revocation + PENDING-13 device lifecycle)?

## Rough Effort / Priority
**M, P2.** High admin-value; depends on PENDING-04 direction.
