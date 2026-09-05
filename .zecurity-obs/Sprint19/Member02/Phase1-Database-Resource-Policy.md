---
type: phase
member: M02
sprint: 19
phase: 1
title: Database / Resource Policy Model
status: done
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

- [x] Inspect the final current migrations and choose the next available migration number.
- [x] Add the Resource Policy table with workspace ownership.
- [x] Add the Resource → Resource Policy relationship.
- [x] Enforce one policy per Resource at the database level where practical.
- [x] Add the Resource Policy → Device Profile relationship.
- [x] Preserve workspace/tenant isolation on every relationship.
- [x] Add appropriate primary keys, foreign keys, uniqueness constraints, and indexes.
- [x] Preserve the ability to represent zero Device Profiles.
- [x] Do not create an Audit/Enforce field for the Resource Policy.
- [x] Do not add a new Device Profile mode field.
- [x] Do not destructively remove `resource_profile_bindings` in this phase.
- [ ] Document the exact migration path from existing bindings.
      **Deliberately left open — this is Phase 4 scope.** Phase 1–3 only stand the
      new model up beside the legacy one; nothing has been migrated yet.

## Required invariants

```text
Resource → exactly 1 Resource Policy
Resource Policy → 0..N Device Profiles
```

Multiple profiles must be possible, but duplicate Resource Policy/Profile bindings must not be possible.

## Verification

- [x] Fresh database migration succeeds.
- [x] Existing database migration succeeds.
- [x] Workspace isolation is enforced.
- [x] Duplicate policy assignment to a Resource is rejected.
- [x] Zero-profile Resource Policy is valid.
- [x] Duplicate profile binding is rejected.
- [x] Existing `resource_profile_bindings` data remains intact after migration.

---

## Implementation (completed 2026-09-05)

**Migration:** `controller/migrations/037_device_resource_policies.sql`

```text
device_resource_policies              (id, workspace_id, name, created_at, updated_at)
resources.device_resource_policy_id   nullable FK  -> one policy per resource
resource_policy_profile_bindings      (policy, profile, workspace) -> 0..N profiles
```

`resource_profile_bindings` is not referenced anywhere in this migration.

### Post-Phase Fixes

#### Fix: tenant relationships were not workspace-safe at the database level

**Issue:** The original migration's foreign keys referenced `id` alone, so nothing
in the database stopped a Workspace-A resource from pointing at a Workspace-B
policy, or a policy from binding another workspace's profile. Required invariants
2 and 4 were only met by application code.

**Root cause:** Single-column foreign keys carry no tenant information. The
existing `resource_profile_bindings` (migration 030) has the same shape and
documents in its own comment that the application must do the checking.

**Fix applied:** tenant-paired composite foreign keys.

```sql
-- BEFORE
ALTER TABLE resources
    ADD COLUMN device_resource_policy_id UUID NULL
        REFERENCES device_resource_policies(id) ON DELETE NO ACTION;

-- AFTER
ALTER TABLE resources
    ADD COLUMN IF NOT EXISTS device_resource_policy_id UUID NULL;
ALTER TABLE resources
    ADD CONSTRAINT resources_device_resource_policy_fkey
        FOREIGN KEY (device_resource_policy_id, tenant_id)
        REFERENCES device_resource_policies (id, workspace_id)
        ON DELETE NO ACTION;
```

Requires `UNIQUE (id, workspace_id)` on `device_resource_policies` and an additive
`UNIQUE (id, workspace_id)` on `device_profiles` as FK targets. Migration 030 is
not edited. `MATCH SIMPLE` means the check is skipped while the column is `NULL`,
so "no policy assigned" stays valid.

**Related files:** `resource_policy_profile_bindings` gets the same treatment on
both its policy and profile sides.

#### Fix: migration renumbered 034 -> 037

**Issue:** Authored as `034` on a branch that predated `034_device_status`,
`034_scim_directory_sync`, `035` and `036`. Merging would have produced a third
`034`.

**Root cause:** Migrations apply in filename order with no tracking table, so a
duplicated number makes ordering ambiguous.

**Fix applied:** renamed to `037_device_resource_policies.sql`. Verified safe
first: no persistent database has ever had the `034` version applied (no
`ztna_pgdata` volume, no `ztna_postgres` container, no `ztna_platform` database
anywhere reachable, and the file never existed on `fixed-pendings`, which is what
deploys).

#### Fix: migration made idempotent

**Issue:** Re-applying the file errored with `relation ... already exists`.

**Root cause:** Staging/prod is applied by hand with psql and there is no
migration-tracking table, so a double-run must be a harmless no-op. Same problem
migration 018 was fixed for (`chore(migrations): make 018 idempotent`).

**Fix applied:** `CREATE TABLE/INDEX IF NOT EXISTS`, `ADD COLUMN IF NOT EXISTS`,
and `DO $$ ... EXCEPTION WHEN duplicate_object THEN NULL; END $$` around the two
`ADD CONSTRAINT` statements. Re-application now emits only `NOTICE ... skipping`.

## Verification evidence

- Fresh chain (001..037) applies to a new database — every integration test does this.
- **Existing-database path proven explicitly:** applied 001..036, seeded a legacy
  `resource_profile_bindings` row, then applied 037 — it succeeded and the legacy
  row survived (1 before, 1 after), with resources preserved and the composite FK
  present.
- Double-apply of 037 against an already-migrated database: NOTICEs only, no errors.
- DB-level tenant safety proven by `DatabaseRejectsCrossWorkspaceRows`, which
  bypasses the store with raw SQL and asserts both cross-workspace shapes are refused.
