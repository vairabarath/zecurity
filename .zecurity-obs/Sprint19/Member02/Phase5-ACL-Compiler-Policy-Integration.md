---
type: phase
member: M02
sprint: 19
phase: 5
title: ACL Compiler / Resource Policy Integration
status: planned
depends_on: [2, 4]
tags: [resource-policy, acl, posture, compiler, pending-16]
---

# Phase 5 — ACL Compiler / Resource Policy Integration

## Goal

Change only the Controller-side policy source so the compiler resolves:

```text
Resource
   ↓
Resource Policy
   ↓
Device Profiles
   ↓
Posture
   ↓
allowed_spiffe_ids
```

while preserving the existing ACL output contract.

## Required work

- [ ] Change the profile lookup path from direct Resource → Profile binding to Resource → Resource Policy → Profile.
- [ ] Preserve existing group/resource authorization behavior.
- [ ] Preserve `applyPosture()` OR semantics.
- [ ] Preserve Any Device semantics for zero selected Device Profiles.
- [ ] Remove the production authorization dependency on `DeviceProfile.mode`.
- [ ] Do not replace `mode` with a new policy-level toggle.
- [ ] Keep posture visibility/evaluation independent from whether a profile is bound.
- [ ] Ensure current profile revision/evaluation freshness checks remain correct.
- [ ] Preserve final `allowed_spiffe_ids` generation.
- [ ] Preserve ACL versioning.
- [ ] Preserve route/resource/shield/connector information in the ACL snapshot.
- [ ] Ensure a policy with Profile A + Profile B authorizes a device satisfying either profile.
- [ ] Ensure a policy with no profiles does not accidentally produce an empty-deny ACL.

## Critical rule

The Connector must continue receiving the resolved authorization state. Do not add Resource Policy or
Device Profile concepts to the Connector authorization protocol unless a verified existing contract
requires it.

## Verification

Add tests proving:

```text
zero profiles → Any Device
one profile + pass → allow
one profile + fail → deny
two profiles + A pass/B fail → allow
two profiles + A fail/B pass → allow
two profiles + both fail → deny
```

Also prove the same behavior survives ACL snapshot generation.
