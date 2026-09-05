---
type: phase
member: M02
sprint: 19
phase: 3
title: GraphQL Resource Policy API
status: done
depends_on: [2]
tags: [resource-policy, graphql, controller, pending-16]
---

# Phase 3 — GraphQL Resource Policy API

## Goal

Expose the Resource Policy model through the Controller GraphQL API for the administrative surface.

## Required operations

Implement the API required by the existing project conventions for:

- [x] List Resource Policies for the current workspace.
- [x] Get a Resource Policy by ID.
- [x] Create a Resource Policy.
- [x] Update Resource Policy metadata if supported by the agreed model.
- [x] Delete a Resource Policy safely.
- [x] Assign a Resource Policy to a Resource.
- [x] Unassign a Resource Policy from a Resource where the target model permits it.
- [x] List Device Profiles attached to a Resource Policy.
- [x] Add a Device Profile to a Resource Policy.
- [x] Remove a Device Profile from a Resource Policy.
- [x] Query the Resource associated with a Resource Policy.
- [x] Expose enough data for the UI to show whether a Resource Policy has zero, one, or multiple profiles.

## Rules

- All operations must be workspace-scoped.
- Do not expose a fake Audit/Enforce switch.
- Do not make the Connector consume Resource Policy GraphQL data.
- Use existing authorization/admin guards.
- Use existing error conventions.
- Mutations must trigger the existing policy invalidation/propagation path.

## Verification

- [x] Schema generation succeeds.
- [x] Resolver tests cover successful and rejected mutations.
- [x] Cross-workspace operations fail.
- [x] Duplicate/second policy assignment fails.
- [x] Empty Device Profile selection remains valid.

---

## Implementation (completed 2026-09-05)

**Files:** `controller/graph/resourcepolicy.graphqls` (new, registered in
`graph/gqlgen.yml`), `controller/graph/resolvers/resourcepolicy.resolvers.go`,
`controller/graph/resolvers/resourcepolicy_helpers.go`.

### Surface

```graphql
type ResourcePolicy { id, name, deviceProfiles, resources, createdAt, updatedAt }
extend type Resource { resourcePolicy: ResourcePolicy }

Query:    resourcePolicies, resourcePolicy(id)
Mutation: createResourcePolicy, updateResourcePolicy, deleteResourcePolicy,
          assignResourcePolicy, unassignResourcePolicy,
          addProfileToResourcePolicy, removeProfileFromResourcePolicy
```

`Resource` is extended from this file, so `resource.graphqls` needed no edit.
An empty `deviceProfiles` list is the "Any Device" state the UI reads.

### Conventions honoured

- **9 x `@hasRole(roles: [ADMIN])`** — every query and mutation. Field resolvers
  inherit the guard from their already-admin-only parents, matching
  `DeviceProfile.requirements` / `.boundResources`.
- **12 x `tenant.MustGet(ctx)`** — all 12 resolvers are workspace-scoped.
- **7 x `NotifyPolicyChange`** — exactly one per mutation, on the success path
  only. Rejected mutations must not notify, and tests assert the count stays put.
- **27 x `apperr.UserErrorf`**, mutation-name-prefixed; raw store errors are
  wrapped with `fmt.Errorf` and never surfaced to clients.
- Relationship fields use `@goField(forceResolver: true)` and the targeted store
  lookups (`ListProfilesForPolicy`, `ListResourceIDsForPolicy`,
  `GetResourcePolicyForResource`) — no workspace-wide scan per field.

### Judgement calls (spec was silent)

1. `assignResourcePolicy` / `unassignResourcePolicy` return `Resource!`, so a
   client can read back `resource.resourcePolicy` in the same round trip.
2. `resourcePolicy(id)` is **nullable** (matching `group` / `shield` /
   `connector`) but returns a user error on not-found (matching the posture
   resolvers). Two conventions collide; the posture one was chosen since this is
   posture-package work.
3. Re-assigning the policy a resource already has succeeds silently; only a
   *different* second policy is refused.

## Tests

- `resourcepolicy_guard_test.go` — no database required. 9 admin-guard cases,
  9 invalid-UUID cases, blank-name case. Uses a nil store pool, so any resolver
  that reached the store would panic — proving validation happens first.
- `resourcepolicy_resolvers_test.go` — real PostgreSQL, self-contained fixture
  (creates and drops its own database, no pre-seeded data needed): queries+CRUD,
  assignment, profile relationships, cross-workspace refusal, legacy coexistence.

```bash
cd controller
PKI_TEST_DATABASE_URL=<admin dsn> go test ./graph/resolvers/ -race
```

## Codegen note

`go run github.com/99designs/gqlgen@v0.17.90 generate --config graph/gqlgen.yml`
succeeds and is idempotent. Two environment prerequisites, both pre-existing and
unrelated to this phase:

1. `controller/gen` protobuf stubs must exist first (`make generate-proto`);
   gqlgen type-checks the whole package.
2. gqlgen v0.17.90 pins `x/tools` v0.42.0, which cannot parse Go 1.27 export
   data (`internal error: package "time" without types`). Run codegen under the
   toolchain `go.mod` declares: `GOTOOLCHAIN=go1.25.0`.

**Not done here (Phase 7 scope):** `admin/src/generated/graphql.ts` has no
`ResourcePolicy` types yet. `make codegen` runs gqlgen *and* the admin codegen;
only the Go half was run.
