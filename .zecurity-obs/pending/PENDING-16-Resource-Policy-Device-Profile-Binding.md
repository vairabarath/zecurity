---
type: adr
status: pending
id: PENDING-16
domain: policy
priority: P1
created: 2026-08-10
related:
  - PENDING-08-Device-Posture-Health
  - PENDING-09-Continuous-Authorization
tags: [pending, adr, policy, resource-policy, device-profile, posture]
---

# Pending ADR 16 — Resource Policy to Device Profile Binding

> **Status: PENDING — for team discussion.** On adoption, promote to the next free `ADR-0NN`.

> **Verification note (2026-09-01):** still **PENDING** — confirmed unbuilt. `resource_profile_bindings`
> (`controller/migrations/030_device_posture.sql:106`), enforced in
> `controller/internal/policy/compiler.go` via `applyPosture` and exposed as
> `bindResourceToProfile` / `unbindResourceFromProfile` in `controller/graph/posture.graphqls`, is
> the **direct Resource ↔ Device Profile binding this ADR proposes to replace** — it is the current
> state, not the deliverable. There is no intermediate Resource Policy layer: `grep -r
> 'resource_policies\|ResourcePolicy' controller/` returns nothing, and `posture.Profile` still
> carries the audit/enforce `Mode` this ADR says should move to the policy layer.

## Context / Current State

The Zecurity posture-check system is now capable of collecting device posture, evaluating posture requirements, and propagating authorization changes toward the connector/data plane.

The current policy model contains:

```text
Policies
├── Resource Policies
├── Sign In Policy
└── Device Profiles
```

A Device Profile represents the conditions that define a trusted/compliant device, such as posture requirements.

The current implementation also contains a direct Resource ↔ Device Profile binding model through `resource_profile_bindings`.

The desired policy model is to introduce a Resource Policy as the intermediate policy layer:

```text
Resource
    ↓
Resource Policy
    ↓
Device Profile(s)
```

A Resource is assigned to exactly one Resource Policy.

The Resource Policy is intended to become an extensible policy layer. Device Profile requirements are the first policy condition being integrated; future policy conditions may include other access requirements.

The Device Profile itself should not contain an Audit/Enforce mode. Enforcement is a policy decision that is ultimately applied at the edge/connector when the resource-access decision is evaluated.

## Problem — Decision Needed

How should Zecurity replace the current direct Resource ↔ Device Profile relationship with a Resource Policy model where:

* each Resource has exactly one Resource Policy;
* a Resource Policy may reference zero or more Device Profiles;
* multiple selected Device Profiles use OR semantics;
* an empty Device Profile requirement means "Any Device";
* policy decisions are enforced at the connector/edge;
* the Resource Policy remains extensible for future access conditions;
* the existing `resource_profile_bindings` implementation is migrated or replaced safely?

The key architectural flow should become:

```text
Resource
    ↓
Resource Policy
    ↓
Device Profile(s)
    ↓
Posture Evaluation
    ↓
Connector / Edge Enforcement
```

## Intended Policy Semantics

### Resource → Resource Policy

Each Resource is assigned to exactly one Resource Policy.

```text
Resource A
    ↓
Resource Policy A
```

A Resource should not simultaneously reference multiple Resource Policies.

### Resource Policy → Device Profiles

A Resource Policy can reference zero or more Device Profiles.

When one or more profiles are selected, they are evaluated using **OR** semantics.

Example:

```text
Resource Policy
├── Corporate Linux
└── Corporate Windows
```

The device satisfies the policy when it matches either profile.

```text
Corporate Linux    → PASS → ALLOW
Corporate Windows  → PASS → ALLOW
Neither            → FAIL → DENY
```

### Empty Device Profile requirement

A Resource Policy with no selected Device Profiles is valid and deployable.

It represents:

```text
Any Device
```

Therefore:

```text
Resource Policy
└── Device Profiles: none
        ↓
    Any Device
        ↓
      ALLOW
```

This is an intentional policy state, not an invalid configuration.

## Enforcement

The Resource Policy does not expose a separate Audit/Enforce toggle.

The intended decision flow is:

```text
Resource
    ↓
Resource Policy
    ↓
Device Profile requirement
    ↓
Posture evaluation
    ↓
YES → Allow
NO  → Deny / revoke
```

The actual enforcement occurs at the **edge/connector**.

The Device Profile therefore remains responsible for defining device requirements rather than deciding whether those requirements should be enforced.

## Policy Structure

The Policy area remains the common parent:

```text
Policies
├── Resource Policies
├── Sign In Policy
└── Device Profiles
```

Resource Policies are intended to become an extensible policy layer.

The initial implementation focuses on:

```text
Resource Policy
└── Device Profile requirements
```

The architecture should not prevent future Resource Policy conditions from being introduced, such as other authentication, identity, location, or access requirements.

Those future conditions are outside the immediate implementation scope of this ADR.

## Options — Existing Binding Migration

The current code already contains a direct Resource ↔ Device Profile relationship through `resource_profile_bindings`. The exact migration strategy remains a decision for implementation planning.

### Option A — Replace the existing binding model

Replace the direct binding model with a Resource Policy model:

```text
Resource
    ↓
Resource Policy
    ↓
Device Profile(s)
```

The existing `resource_profile_bindings` representation would be removed or replaced.

* **Pros:** clean final architecture; directly represents the intended policy hierarchy.
* **Cons:** larger migration touching database, GraphQL, controller/ACL compilation, and frontend behavior.

### Option B — Keep the existing binding temporarily

Keep the current `resource_profile_bindings` implementation while introducing Resource Policies above it.

```text
Resource
    ↓
Resource Policy
    ↓
existing resource_profile_bindings
    ↓
Device Profile
```

* **Pros:** smaller incremental migration; reduces immediate changes to existing posture/ACL behavior.
* **Cons:** introduces an intermediate compatibility model and may leave two policy representations temporarily.

### Option C — Extend the existing binding model

Modify the existing binding representation so that it becomes the underlying implementation of Resource Policy → Device Profile relationships.

* **Pros:** reuses existing database and code where possible.
* **Cons:** the existing model may impose constraints that do not fit the intended extensible Resource Policy architecture.

## Recommendation

**Non-binding:** The target architecture should be:

```text
Resource
    ↓
Resource Policy
    ↓
Device Profile(s)
```

with:

* exactly one Resource Policy per Resource;
* zero or more Device Profiles per Resource Policy;
* OR semantics between selected Device Profiles;
* zero Device Profiles meaning Any Device;
* no Audit/Enforce toggle on Device Profiles or Resource Policies;
* enforcement performed at the connector/edge;
* Resource Policy designed as an extensible policy layer.

The migration strategy for the existing `resource_profile_bindings` should be selected after evaluating the three options above against the current database, GraphQL, ACL compiler, and frontend implementation.

## Open Questions

* Should the existing `resource_profile_bindings` table be replaced, retained temporarily, or extended?
* What database schema should represent exactly one Resource Policy per Resource?
* Should the Default Policy be a special persisted policy object or a system-level fallback?
* When a Resource Policy is deleted, how exactly is the Resource automatically reassigned to the Default Policy?
* What permissions should be required to create, modify, assign, and delete Resource Policies?
* How should Resource Policy changes trigger `NotifyPolicyChange` and subsequent ACL recomputation?
* How should the existing ACL compiler consume the new Resource Policy → Device Profile relationship?
* How should existing Resource ↔ Device Profile bindings be migrated without changing current authorization unexpectedly?
* How should the frontend expose Device Profile selection inside Resource Policy management?
* How should policy evaluation and OR semantics be represented in the ACL snapshot/protobuf model?
* How should future Resource Policy conditions be added without requiring another redesign of the Resource → Policy relationship?

## Rough Effort / Priority

**L–XL, P1.**

This is an architectural policy-model change touching the database, GraphQL API, controller policy/ACL compilation, frontend policy management, migration of existing bindings, and edge enforcement behavior.
