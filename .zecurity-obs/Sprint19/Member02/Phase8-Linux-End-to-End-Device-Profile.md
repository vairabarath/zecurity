---
type: phase
member: M02
sprint: 19
phase: 8
title: Linux Device Profile End-to-End Validation
status: planned
depends_on: [5, 6, 7]
tags: [linux, posture, device-profile, e2e, ztna, pending-16]
---

# Phase 8 — Linux Device Profile End-to-End Validation

> This phase is mandatory. Sprint 19 is not complete if Device Profiles only work at the schema/UI
> level. At least one real Linux path must work end-to-end.

## Goal

Prove the full path:

```text
Linux Device
   ↓
Client identity
   ↓
Posture collection/report
   ↓
Controller posture evaluation
   ↓
Device Profile satisfaction
   ↓
Resource Policy
   ↓
ACL compilation
   ↓
ACL snapshot propagation
   ↓
Connector authorization
   ↓
ALLOW / DENY
```

## Required validation

### Profile creation

- [ ] Create a Linux Device Profile.
- [ ] Configure at least the currently supported Linux checks.
- [ ] Confirm requirements persist and revisions change correctly.

### Posture reporting

- [ ] Real Linux client can report posture.
- [ ] Controller receives and stores the report.
- [ ] Report is associated with the correct device/workspace.
- [ ] Supported checks produce real observations.
- [ ] Unsupported checks are handled according to the existing project semantics, not silently treated as passing.

### Profile evaluation

- [ ] Passing Linux device satisfies the profile.
- [ ] Failing Linux device does not satisfy the profile.
- [ ] Stale/missing posture is handled according to existing posture semantics.
- [ ] Evaluation uses the current profile revision.

### Resource Policy enforcement

- [ ] Attach the Linux profile to a Resource Policy.
- [ ] Assign the Resource Policy to a Resource.
- [ ] Passing Linux device is represented in the compiled allowed identity set.
- [ ] Failing Linux device is absent/denied.
- [ ] Remove the profile and verify the policy becomes Any Device.
- [ ] Add two profiles and verify OR behavior.

### Live propagation

- [ ] Change the Resource Policy while the Connector is running.
- [ ] Verify ACL invalidation/recompile/push.
- [ ] Verify the Connector receives the new ACL.
- [ ] Verify access changes without requiring a manual service restart.

## Evidence

The phase must leave reproducible test/e2e evidence showing:

```text
Linux posture PASS → Resource Policy → ACL → Connector → ALLOW

Linux posture FAIL → Resource Policy → ACL → Connector → DENY
```

Do not mark this phase done from mocked posture data alone.
