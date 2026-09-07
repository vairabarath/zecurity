---
type: phase
member: M02
sprint: 19
phase: 4
title: Legacy Binding Compatibility / Migration
status: done
depends_on:
  - 1
  - 2
  - 3
tags:
  - resource-profile-bindings
  - migration
  - compatibility
  - pending-16
---

# Phase 4 — Legacy Binding Compatibility / Migration

> The selected architecture is Option B: keep the existing binding model temporarily and migrate
> safely. This phase must not silently change customer access.

## Goal

Move existing direct Resource → Device Profile relationships into the new Resource Policy model
without losing access semantics.

## Required work

- [x] Inventory existing `resource_profile_bindings`.
- [x] Identify resources with zero, one, and multiple bindings.
- [x] Identify orphaned/cross-workspace/invalid rows before migration.
- [x] Define a deterministic migration for valid existing bindings:

```text
Resource
   ↓
create one Resource Policy
   ↓
move existing profile bindings to that policy
```

- [x] Preserve workspace IDs and relevant IDs where the schema allows.
- [x] Ensure a resource receives only one migrated policy.
- [x] Ensure multiple existing profiles remain attached to the same migrated policy.
- [x] Preserve existing OR behavior.
- [x] Preserve existing Any Device behavior for resources with no posture profiles.
- [x] Decide and document how legacy rows coexist during the transition.
- [x] Do not drop `resource_profile_bindings` until the new path is proven and a separate cleanup decision exists.
- [x] Add migration verification for access-equivalence before/after.

## Safety requirement

For a migrated Resource, the effective authorization result before and after migration must be
equivalent unless an explicitly documented PENDING-16 behavior change applies.

## Verification

- [x] Existing single-profile resource migrates correctly.
- [x] Existing multi-profile resource migrates correctly.
- [x] Existing zero-profile resource remains Any Device.
- [x] Cross-workspace rows are detected/rejected safely.
- [x] Migration is idempotent or safely guarded against repeated execution.
- [x] No existing customer binding disappears silently.

---

## Status — verified, then deliberately withdrawn (2026-09-05)

**Read this before looking for the code: there is none in this branch, on purpose.**

Phase 4 was implemented and verified end to end, and the implementation was then
removed from the branch by decision. What follows records exactly what was
proven, what was decided, and what a future implementation must reproduce.

### Why nothing is retained

The project is pre-production. There is no database with legacy
`resource_profile_bindings` rows anywhere — verified: no `ztna_pgdata` volume, no
`ztna_postgres` container, no `ztna_platform` database on any reachable
PostgreSQL instance, and no `037`/`038` migration has ever been applied to a
persistent database. A backfill today would move **zero rows**.

So the deliverable that has lasting value is the **decision record and the
verification method**, not a migration that would run against an empty database
and then need re-verifying whenever real data first appears.

### What was actually built and proven

Implemented as `controller/internal/posture/resource_policy_migration.go` plus
`resource_policy_migration_test.go` (both since removed), providing three
operations:

| Operation | Purpose |
|---|---|
| `InventoryLegacyBindings` | read-only classification of every resource, plus flags for conditions that must not be silent |
| `MigrateLegacyBindings` | the backfill, one transaction per workspace, full reconcile |
| `VerifyMigrationEquivalence` | the proof: `LegacySet == NewSet` for every resource |

Verified against nine synthetic legacy shapes in a temporary PostgreSQL database
on 2026-09-05. **All seven tests passed:**

```text
TestInventoryClassifiesEveryLegacyShape          PASS
TestMigrateLegacyBindingsPreservesAuthorization  PASS
TestMigrateLegacyBindingsIsIdempotent            PASS
TestMigrateLegacyBindingsReconcilesDrift         PASS
TestMigrateLegacyBindingsSkipsRenamedPolicies    PASS
TestMigrateLegacyBindingsIsWorkspaceScoped       PASS
TestVerifyMigrationEquivalenceDetectsDrift       PASS
```

Equivalence held for every fixture:

| Fixture | LegacySet | NewSet | |
|---|---|---|---|
| zero bindings | `{}` | `{}` | ✅ |
| one enforce | `{A}` | `{A}` | ✅ |
| three enforce | `{A,B,C}` | `{A,B,C}` | ✅ |
| audit-only | `{}` | `{}` | ✅ |
| mixed audit + enforce | `{B,C}` | `{B,C}` | ✅ |
| zero-requirement enforce | `{empty}` | `{empty}` | ✅ |
| cross-workspace | `{}` | `{}` | ✅ |

Idempotency: a second run was a total no-op (all four counters zero, policy count
unchanged at one per eligible resource). Legacy row count identical before and
after — the migration reads `resource_profile_bindings` and never writes to it.

### The rule any implementation must preserve

`compiler.go:160` discards non-enforce profiles before building the OR-set:

```go
if profile.Mode != posture.ModeEnforce { continue }
```

Therefore

```text
LegacySet(resource) = { profile : binding(resource, profile)
                                  AND profile.mode = 'enforce' }
```

**Audit bindings grant nothing and deny nothing.** Copying one into a policy
turns an inert relationship into a real gate — the new model has no `mode`, so
membership *is* enforcement — and would cut access for every device. Audit
bindings must be skipped and reported, never migrated and never silently dropped.

### Decisions taken (previously open questions)

| Question | Decision |
|---|---|
| Which resources get a policy | **Every non-`deleting` resource**, including those with zero bindings. A policy with no profiles is the Any Device state, never deny-all. |
| `deleting` resources | **Skipped**, and flagged. ADR-004 tombstones awaiting reap, already excluded from ACL compilation by `ListEnabledRulesWithResources` (`AND r.status != 'deleting'`), so they gate nothing. |
| Policy naming | `Migrated policy for <resource-uuid>`. Must be deterministic so a re-run collides with itself instead of creating a second policy. Resource names are unusable: `resources` is `UNIQUE (shield_id, name)` — per shield, **not** per workspace — so two shields in one workspace can both host `db`, breaking `device_resource_policies UNIQUE (workspace_id, name)`. |
| Reporting invalid rows | Structured report from the inventory operation. No new table, no schema change. |
| Coexistence drift | **Full reconcile on every run** — see below. |

### Required per-case behaviour

| Legacy shape | Required result |
|---|---|
| zero bindings | policy created, **0 profiles** (Any Device) |
| one enforce binding | policy with that profile |
| several enforce bindings | **one** policy holding all of them (the OR set) |
| audit-only | policy created, **0 profiles**; audit profiles reported |
| mixed audit + enforce | policy holds **only** the enforce subset |
| enforce profile with zero requirements | migrated (it is in `LegacySet`) **and flagged** — vacuously satisfied by every device |
| cross-workspace binding | **not migrated**; reported. Constructible only by raw SQL, since the store validates all three workspace columns |
| duplicate legacy binding | impossible — `UNIQUE (resource_id, profile_id)` |
| orphaned row | impossible — both foreign keys `ON DELETE CASCADE`; report as `0 by construction` rather than leaving it unexamined |
| `deleting` resource | skipped, flagged, counted |
| already migrated | policy reused, never duplicated; profile set reconciled |
| policy renamed by an admin | **left completely alone** — reconcile scoped to the generated name |

### Coexistence: how legacy rows behave during the transition

The legacy `bindResourceToProfile` / `unbindResourceFromProfile` mutations stay
live, and the ACL compiler still reads **only** the legacy tables. So
`LegacySet == NewSet` is true only at the instant of migration; the next legacy
mutation breaks it.

The migration must therefore be a **reconcile, not an insert**: it makes the
policy's profile set exactly equal `LegacySet` on every execution, removing stale
rows as well as adding missing ones. It is then re-run immediately before the
Phase 5 cutover, which is the moment equivalence actually matters. Verified
against three drift kinds: a legacy unbind, a new legacy bind, and an enforce
profile demoted to audit.

`resource_profile_bindings` is not dropped and no row is deleted from it. A later,
explicit cleanup decision owns that.

## Carried into Phase 5 — two hazards

1. **A `NULL` policy silently ungates a resource.** `applyPosture`
   (`compiler.go:320-326`) treats zero enforced profiles as "allow every
   group-authorized device". So when Phase 5 switches the compiler to read
   `resource → policy → profiles`, any resource whose policy is `NULL` but whose
   legacy set is non-empty goes from **gated** to **ungated** — a silent
   authorization widening. Phase 5 must either run the reconcile first, treat
   `NULL` as a deliberate fall-back to the legacy bindings, or refuse to compile
   until every resource carries a policy.

2. **New resources are created with no policy.** `internal/resource` never writes
   `device_resource_policy_id`, so every resource created after any backfill has
   `NULL`. "Each resource has exactly one policy" is therefore not self-
   maintaining; either `createResource` must assign one, or Phase 5 must handle
   `NULL` explicitly.

## Architecture boundary held

No migration file was introduced. `internal/policy/`, `internal/connector/`,
`graph/`, `connector/`, `shield/`, `client/`, `relay/`, `proto/`, `admin/`,
`migrations/`, `go.mod` and `go.sum` are all unmodified by Phase 4. No
Audit/Enforce field was added to Resource Policy, the ACL compiler and
`applyPosture()` are untouched, and the legacy table was neither dropped nor
written to. Phase 5 still owns the compiler cutover.
