---
type: adr
status: accepted
id: ADR-023
domain: identity
priority: P1
created: 2026-07-27
decided: 2026-07-27
related:
  - PENDING-04-Multiple-IdPs-Enterprise-SSO
  - ADR-024-Identity-Linking-and-Provider-Migration
  - ADR-021-Provider-Identity-and-Authorization
tags: [adr, identity, philosophy, boundaries, federation]
---

# ADR-023 — Identity Philosophy (Boundaries)

> **Status: ACCEPTED (2026-07-27).** Foundational, written before PENDING-04 code.
> This is a **constitution, not a design** — it fixes definitions and plane walls so every
> downstream ADR (PENDING-04/05/06, ADR-024, and the zero-trust cluster 08/09) falls out cleanly
> instead of drifting. No schema or code is specified here.

## Context

PENDING-04 turns authentication from "hardwired Google" into a federation layer, which makes the
identity subsystem the **root of trust** for the whole platform. Root-of-trust subsystems are
optimized for "survives five years of feature growth," not "works today." Before writing that code
we lock the vocabulary and the boundaries, because they are extremely expensive to change once
customers are onboarded.

## Decision

### 1. Definitions

- **Identity** — *who* a subject is. A stable, issuer-scoped external identifier
  `(connection_id, issuer, subject)` mapped to a canonical **Principal**. Never email.
- **Authentication** — proving an identity *right now*, producing an `AuthenticationContext`
  (issuer, subject, `amr`, `acr`, `auth_time`, raw claims). Nothing more.
- **Authorization** — evaluating policy for a Principal against a resource. Owned by the policy
  engine. Authentication never decides access.
- **Trust** — a dynamic signal (device posture, risk, MFA freshness, network). Owned by
  PENDING-08/09. Not computed at authentication time.
- **Session** — a time-bounded reference to a Principal plus a revocation generation. Deliberately
  "dumb": it points at a Principal, it is not the Principal.
- **Federation** — mapping an *external* authentication to an *internal* Principal.
- **Principal** — the canonical actor Zecurity authorizes. Today a human user; later also
  workloads/service accounts. Everything downstream references the Principal, never the IdP.

### 2. The four planes (walls that must not be crossed)

| Plane | Owns | Must NOT |
|-------|------|----------|
| **Human identity federation** (PENDING-04) | external→Principal, linking, lifecycle | compute trust, decide authz |
| **Workload identity** (existing SPIFFE/SVID, per-workspace CA) | connector/relay/shield identity | merge into the human plane |
| **Authorization** (Sprint 8 policy engine) | ACL, access rules, groups | authenticate, or trust-score |
| **Trust / posture** (PENDING-08/09) | device trust, risk, continuous re-eval | own identity |

### 3. Consequences (the rules these boundaries impose)

- **Identity ≠ authorization.** IdP-supplied groups are a *transient hint*, never the effective
  authorization set (which lives in the internal group model / SCIM). Nothing derived from an ID
  token is fed to the policy engine.
- **Human plane ≠ workload plane.** OIDC humans and SPIFFE workloads keep separate trust roots,
  lifecycles, and revocation paths; they converge only as abstract *Principals*, later.
- **Authentication emits signals; trust consumes them.** `amr`/`acr`/`auth_time` are recorded at
  login for 08/09 to score — the federation layer itself assigns no trust level.
- **The Principal is the pivot.** Sessions, device trust, and continuous authz all attach to the
  Principal, so new auth sources (CLI, API keys, workloads) and new signals slot in without
  reshaping sessions or the authz path.

## Alternatives considered

- **Skip this ADR, define terms ad hoc per feature.** Rejected — guarantees drift across
  PENDING-04/05/06/08/09 and expensive retrofits after onboarding.
- **One unified identity graph now (humans + workloads).** Deferred to a vision doc — a
  multi-quarter effort that would sink the near-term goal; the boundaries here keep the door open.

## Status / follow-ups

Accepted as the baseline for PENDING-04. The linking/migration mechanics live in
[[ADR-024-Identity-Linking-and-Provider-Migration]]. The identity-graph north-star remains a
non-binding vision item.
