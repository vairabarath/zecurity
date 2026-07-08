---
type: adr
status: pending
id: PENDING-06
domain: identity
priority: P2
created: 2026-07-03
related:
  - PENDING-04-Multiple-IdPs-Enterprise-SSO
  - PENDING-09-Continuous-Authorization
tags: [pending, adr, identity, mfa]
---

# Pending ADR 06 — MFA & Step-Up Authentication

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.

## Context / Current State

There is no first-party MFA and no step-up (re-auth for sensitive resources). Today MFA, if any,
is whatever Google enforces upstream — invisible and uncontrollable by Zecurity. There is no
notion of "this resource requires a fresh/strong auth" in the policy engine.

## Problem — Decision Needed

Do we own MFA, delegate it to the IdP, or both — and do we need step-up per resource?

## Options

### Option A — Delegate MFA to the IdP (rely on OIDC `acr`/`amr`)
Require and verify authentication-context claims from the IdP; no first-party authenticators.
- **Pros:** least work; enterprises already run MFA in their IdP; composes with PENDING-04.
- **Cons:** depends on IdP emitting/honoring `acr`; weak story for Google-consumer tenants.

### Option B — First-party MFA (TOTP/WebAuthn)
Zecurity enrolls a second factor directly.
- **Pros:** works regardless of IdP; enables device-bound WebAuthn. **Cons:** we own authenticator
  lifecycle, recovery, support burden.

### Option C — Step-up authorization tied to policy
Per-resource/-group "requires step-up"; on access to a sensitive resource, force fresh strong auth
(via A or B) before the ACL grants it.
- **Pros:** true zero-trust posture; high-value for crown-jewel resources. **Cons:** requires
  policy-engine + client-daemon changes; interacts with continuous authz (PENDING-09).

## Recommendation (non-binding)
Option A as the baseline (cheap, enterprise-aligned) once PENDING-04 lands, with **Option C**
step-up as the differentiator when continuous authz (PENDING-09) is tackled. Defer B unless a
segment needs IdP-independent MFA.

## Open Questions
- Do we need step-up granularity per resource/group, or just a global MFA requirement?
- Session/token model for "recently re-authenticated" (interacts with 11.3 token refresh)?

## Rough Effort / Priority
**A: S · C: M–L**, P2. Sequence after PENDING-04.
