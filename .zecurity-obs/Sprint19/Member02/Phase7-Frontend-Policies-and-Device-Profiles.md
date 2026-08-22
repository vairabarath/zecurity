---
type: phase
member: M02
sprint: 19
phase: 7
title: Frontend Policies / Device Profiles
status: planned
depends_on: [3, 5]
tags: [frontend, resource-policy, device-profile, linux, pending-16]
---

# Phase 7 — Frontend Policies / Device Profiles

## Goal

Deliver the complete administrative workflow for the new policy model.

The UI must represent:

```text
Policies
├── Resource Policies
├── Sign In Policy
└── Device Profiles
```

## Resource Policies

- [ ] Resource Policy list page.
- [ ] Create Resource Policy.
- [ ] Edit Resource Policy.
- [ ] Delete Resource Policy safely.
- [ ] Show which Resource is assigned to each policy.
- [ ] Enforce the one-policy-per-Resource constraint in the UI while preserving backend enforcement.
- [ ] Select zero or more Device Profiles.
- [ ] Add/remove Device Profiles.
- [ ] Clearly show that multiple profiles are OR.
- [ ] Clearly show that no selected profiles means Any Device.
- [ ] Do not show an Audit/Enforce toggle.

## Device Profiles

- [ ] Keep Device Profile management available independently of Resource Policies.
- [ ] Device Profile requirements remain editable.
- [ ] Device Profile posture satisfaction remains visible independently of Resource Policy binding.
- [ ] Linux is fully supported.
- [ ] Do not present unsupported platform checks as working merely because the UI can render them.
- [ ] Remove the old Audit/Enforce control from the Device Profile experience.
- [ ] Ensure profile creation/editing works with the current supported Linux posture checks.

## UX requirements

The UI must not imply:

```text
Device Profile → directly grants/denies Resource access
```

It must communicate:

```text
Device Profile = trust definition
Resource Policy = access requirement
```

## Verification

- [ ] Admin can create a Linux Device Profile.
- [ ] Admin can configure its supported Linux posture requirements.
- [ ] Admin can see posture satisfaction/visibility without binding the profile.
- [ ] Admin can create a Resource Policy.
- [ ] Admin can attach the Linux profile.
- [ ] Admin can remove the profile.
- [ ] Admin can leave the profile list empty.
- [ ] Admin cannot attach a second Resource Policy to the same Resource.
