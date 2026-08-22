---
type: phase
member: M02
sprint: 19
phase: 1
title: Database / Resource Policy Model
status: planned
depends_on: []
tags: [resource-policy, database, migration, pending-16]
---

# Phase 1 — Database / Resource Policy Model

> Source: PENDING-16 and Sprint 19 decisions.
> This phase establishes the persistent Resource Policy model without breaking existing access.

## Goal

Introduce the persistent model required for:

```text
Resource
   ↓
Resource Policy
   ↓
Device Profile(s)
```

with the invariant that one Resource has exactly one Resource Policy.

## Required work

- [ ] Inspect the final current migrations and choose the next available migration number.
- [ ] Add the Resource Policy table with workspace ownership.
- [ ] Add the Resource → Resource Policy relationship.
- [ ] Enforce one policy per Resource at the database level where practical.
- [ ] Add the Resource Policy → Device Profile relationship.
- [ ] Preserve workspace/tenant isolation on every relationship.
- [ ] Add appropriate primary keys, foreign keys, uniqueness constraints, and indexes.
- [ ] Preserve the ability to represent zero Device Profiles.
- [ ] Do not create an Audit/Enforce field for the Resource Policy.
- [ ] Do not add a new Device Profile mode field.
- [ ] Do not destructively remove `resource_profile_bindings` in this phase.
- [ ] Document the exact migration path from existing bindings.

## Required invariants

```text
Resource → exactly 1 Resource Policy
Resource Policy → 0..N Device Profiles
```

Multiple profiles must be possible, but duplicate Resource Policy/Profile bindings must not be possible.

## Verification

- [ ] Fresh database migration succeeds.
- [ ] Existing database migration succeeds.
- [ ] Workspace isolation is enforced.
- [ ] Duplicate policy assignment to a Resource is rejected.
- [ ] Zero-profile Resource Policy is valid.
- [ ] Duplicate profile binding is rejected.
- [ ] Existing `resource_profile_bindings` data remains intact after migration.
