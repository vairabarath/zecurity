---
type: planning
status: active
sprint: 19
tags:
  - sprint19
  - dependencies
  - execution-path
  - resource-policy
  - device-posture
  - ztna
  - pending-16
---

# Sprint 19 — Resource Policy → Device Profile Binding (PENDING-16)

> **Read this before writing a single line of code.**
> Source of truth: [[PENDING-16-Resource-Policy-Device-Profile-Binding]] plus the team decisions
> recorded during the PENDING-16 discussion.
>
> **This sprint is owned entirely by Member02. No other member is assigned to this task.**
>
> The sprint must deliver the **complete PENDING-16 path**, not only the database or frontend pieces.
> A Resource Policy must become a real working authorization path through the existing posture,
> ACL compilation, propagation, and Connector enforcement pipeline.
>
> **Linux is the minimum end-to-end supported Device Profile platform for this sprint.**
> The implementation is not considered complete if the UI can create a Linux profile but a real
> Linux device cannot report posture, satisfy the profile, become authorized through a Resource
> Policy, and successfully reach a protected Resource.

## Sprint Goal

Replace the direct Resource → Device Profile enforcement model with the policy-layer model:

```text
Resource
    ↓
Resource Policy
    ↓
Device Profile(s)
    ↓
Device Posture Evaluation
    ↓
ACL Compilation
    ↓
ACL Snapshot
    ↓
Connector Enforcement
    ↓
Shield / Resource
```

The Resource Policy is the access-policy layer. Device Profiles define trusted-device requirements
and remain observable even when no Resource Policy references them. Enforcement occurs because a
Resource Policy references the Device Profile; there is no new Audit/Enforce switch on the profile
or Resource Policy.

## Normative decisions

| Decision | Detail |
|---|---|
| Policy hierarchy | `Policies` is the common parent containing `Resource Policies`, `Sign In Policy`, and `Device Profiles`. |
| Resource relationship | A Resource can be assigned to **exactly one** Resource Policy. |
| Profile relationship | A Resource Policy can reference zero or more Device Profiles. |
| Multiple profiles | Device Profiles in one Resource Policy use **OR** semantics. A device satisfying any selected profile can satisfy the policy. |
| Empty profile set | A Resource Policy with no selected Device Profiles is valid and means **Any Device**; no posture gate is applied by that policy. |
| Enforcement location | Controller computes the authorization result; Connector is the identity-based enforcement point; Shield remains the network/firewall enforcement layer. |
| Profile mode | The new architecture has **no Audit/Enforce mode** on Device Profiles or Resource Policies. Do not invent a replacement toggle. Existing `device_profiles.mode` dependencies must be migrated/retired safely as part of this sprint. |
| Device Profile meaning | A Device Profile defines device trust requirements and provides posture visibility. It does not itself grant or deny resource access. |
| Visibility | Device posture/profile satisfaction can be observed even when a Device Profile is not bound to any Resource Policy. |
| Existing bindings | Keep `resource_profile_bindings` temporarily and migrate existing data safely; do not perform a destructive cutover that can silently change customer access. |
| ACL format | Preserve the existing ACL snapshot contract unless a code-level blocker is proven. The Connector should continue receiving the resolved authorization state, not Resource Policy internals. |
| Posture engine | Reuse the existing posture evaluation and `applyPosture()` behavior wherever possible. |
| Propagation | Preserve the existing policy-change path: invalidate → recompile → push; heartbeat/version reconciliation remains the fallback. |
| Platform support | Linux is the minimum required end-to-end supported Device Profile platform for Sprint 19. |
| Ownership | **Member02 only.** No work is assigned to other members. |
| Backend first | Backend/data/authorization behavior must be complete and tested before frontend completion is declared. |

## Before / after

### Current

```text
Resource
   ↓
resource_profile_bindings
   ↓
Device Profile(s)
   ↓
Mode == ENFORCE
   ↓
Posture evaluation
   ↓
ACL Snapshot
   ↓
Connector
```

### Target

```text
Resource
   ↓
Resource Policy
   ├── Device Profile A
   ├── Device Profile B
   └── Device Profile C
          ↓
     OR evaluation
          ↓
Posture evaluation
          ↓
ACL Snapshot
          ↓
Connector
```

## Non-goals / boundaries

- Do not redesign the Connector authorization protocol.
- Do not make Connector aware of Resource Policy IDs or Device Profile definitions.
- Do not redesign Shield.
- Do not create a second policy engine in the Connector.
- Do not introduce a new Audit/Enforce toggle.
- Do not silently delete or ignore existing `resource_profile_bindings`.
- Do not remove posture evaluation merely because ACL compilation already resolves posture.
- Do not claim Linux Device Profiles work based only on UI or unit tests; prove the complete path.
- Do not implement unrelated PENDING/ADR work.

## Dependency graph

```text
P1  Database model + migration
 ├── P2 Store/domain model
 │    └── P3 GraphQL API
 ├── P4 Safe legacy-binding migration/compatibility
 └── P5 ACL compiler integration
       └── P6 Policy-change propagation
             └── P8 End-to-end authorization

P7  Frontend Resource Policies + Device Profile usability
 └── P8 End-to-end verification

P9  Testing
 └── P10 Final verification / documentation / build gates
```

## Team assignment

| Member | Role | Area |
|---|---|---|
| **Member02** | Full-stack / ZTNA | Entire PENDING-16 implementation: database, Controller/store, GraphQL, compiler integration, propagation, frontend, Linux Device Profile end-to-end validation, tests, migration, and final verification. |

## Critical rule

**There is only one owner for Sprint 19: Member02.**

Do not create parallel work assignments for Member01, Member03, etc. If a shared file is touched,
Member02 owns the change and must keep it surgical and compatible with existing work.
