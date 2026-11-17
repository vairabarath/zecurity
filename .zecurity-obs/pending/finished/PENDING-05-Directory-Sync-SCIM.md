---
type: adr
status: implemented
verified: 2026-11-14
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

> **Status (verified against `main` 25a0cfb5, 2026-11-14): IMPLEMENTED.** [[ADR-025-SCIM-Directory-Synchronization]]
> is accepted and is the authoritative SCIM design; the options below are the original framing.
>
> **Built:**
> - The SCIM engine `controller/internal/scim/*`, with migrations `034_scim_directory_sync.sql` and
>   `035_groups_sync_instance.sql`.
> - The durable outbox sink: `scim.NewDurableOutboxSink(outboxStore)` in the controller's `main.go` (plan step P10;
>   no interim sink is left).
> - The SCIM conflicts page (`admin/src/pages/ScimConflicts.tsx`).
> - Sprint 17 FE Phase 7: per-connection sign-in buttons on `Login.tsx` (F7-5, 2026-09-02), and disable/delete on
>   `IdpConnectionDetail.tsx` (F7-8, 2026-09-05, `dangerzone.test.tsx`).
>
> **Known gaps:**
> - No SCIM conformance suite exists (`Sprint17/path.md`: "NOT RUN").
> - Live Okta/Entra interop is still waiting on tenant access.
> - Break-glass MFA is a stub that allows everything (`RequireBreakGlassMFA`, `graph/resolvers/permission_helpers.go`) until
>   PENDING-06.
>
> The 2026-09-01 correction (FE Phase 7 still open) is superseded: F7-5 and F7-8 are done.

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
