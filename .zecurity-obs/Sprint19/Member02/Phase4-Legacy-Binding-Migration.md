---
type: phase
member: M02
sprint: 19
phase: 4
title: Legacy Binding Compatibility / Migration
status: planned
depends_on: [1, 2, 3]
tags: [resource-profile-bindings, migration, compatibility, pending-16]
---

# Phase 4 — Legacy Binding Compatibility / Migration

> The selected architecture is Option B: keep the existing binding model temporarily and migrate
> safely. This phase must not silently change customer access.

## Goal

Move existing direct Resource → Device Profile relationships into the new Resource Policy model
without losing access semantics.

## Required work

- [ ] Inventory existing `resource_profile_bindings`.
- [ ] Identify resources with zero, one, and multiple bindings.
- [ ] Identify orphaned/cross-workspace/invalid rows before migration.
- [ ] Define a deterministic migration for valid existing bindings:

```text
Resource
   ↓
create one Resource Policy
   ↓
move existing profile bindings to that policy
```

- [ ] Preserve workspace IDs and relevant IDs where the schema allows.
- [ ] Ensure a resource receives only one migrated policy.
- [ ] Ensure multiple existing profiles remain attached to the same migrated policy.
- [ ] Preserve existing OR behavior.
- [ ] Preserve existing Any Device behavior for resources with no posture profiles.
- [ ] Decide and document how legacy rows coexist during the transition.
- [ ] Do not drop `resource_profile_bindings` until the new path is proven and a separate cleanup decision exists.
- [ ] Add migration verification for access-equivalence before/after.

## Safety requirement

For a migrated Resource, the effective authorization result before and after migration must be
equivalent unless an explicitly documented PENDING-16 behavior change applies.

## Verification

- [ ] Existing single-profile resource migrates correctly.
- [ ] Existing multi-profile resource migrates correctly.
- [ ] Existing zero-profile resource remains Any Device.
- [ ] Cross-workspace rows are detected/rejected safely.
- [ ] Migration is idempotent or safely guarded against repeated execution.
- [ ] No existing customer binding disappears silently.
