---
type: phase
member: M02
sprint: 19
phase: 2
title: Controller Store / Domain Model
status: done
depends_on: [1]
tags: [resource-policy, go, store, controller, pending-16]
---

# Phase 2 — Controller Store / Domain Model

## Goal

Make Resource Policies first-class Controller objects and provide safe store operations.

## Required work

- [x] Add Resource Policy domain structs/types.
- [x] Add store methods for create/read/update/delete Resource Policies.
- [x] Add store methods for assigning exactly one Resource Policy to a Resource.
- [x] Add store methods for adding/removing Device Profiles from a Resource Policy.
- [x] Validate Resource, Resource Policy, Device Profile, and Workspace all belong to the same workspace.
- [x] Reject assigning a second Resource Policy to a Resource.
- [x] Allow a Resource Policy to contain zero profiles.
- [x] Reject duplicate Resource Policy/Profile bindings.
- [x] Ensure deletion behavior is explicit and safe.
- [x] Preserve existing `resource_profile_bindings` operations during the temporary coexistence period.
- [x] Ensure every successful authorization-relevant mutation invokes the existing policy-change notification path.

## Error cases

- [x] Resource not found.
- [x] Resource Policy not found.
- [x] Device Profile not found.
- [x] Cross-workspace assignment.
- [x] Resource already has a different Resource Policy.
- [x] Duplicate profile binding.
- [x] Attempt to delete a policy still required by a Resource, unless an explicit safe detach operation exists.
- [x] Invalid/empty identifiers.

## Build gate

```bash
cd controller
go build ./...
go test ./internal/...
```

---

## Implementation (completed 2026-09-05)

**File:** `controller/internal/posture/resource_policy_store.go` (extended from the
Phase 2 stub; no second, competing store was created).

### Operations

| Method | Behaviour worth knowing |
|---|---|
| `CreateResourcePolicy` | Trims the name; blank -> `ErrInvalidPolicyName`. Starts with zero profiles (Any Device). |
| `GetResourcePolicy` / `ListResourcePolicies` | Workspace-scoped; list ordered by name. |
| `UpdateResourcePolicy` | Renames, sets `updated_at = NOW()`. |
| `DeleteResourcePolicy` | Locks the policy `FOR UPDATE`, counts assigned resources, refuses with `ErrPolicyAssigned`. Profile bindings cascade. |
| `AssignResourcePolicy` | Locks the resource row `FOR UPDATE`. Same policy again = no-op success; a *different* second policy = `ErrResourceAlreadyAssigned`, never a silent replace. |
| `UnassignResourcePolicy` | Idempotent; unknown/cross-workspace resource -> `ErrNotFound`. |
| `GetResourcePolicyForResource` | `(nil, nil)` when unassigned, `ErrNotFound` when the resource does not exist — so "no policy" and "no such resource" stay distinguishable. |
| `ListResourceIDsForPolicy` / `ListProfilesForPolicy` | Targeted reverse/forward lookups for the GraphQL relationship fields. |
| `AddProfileToPolicy` | Validates both sides' workspace; duplicate -> `ErrDuplicateProfileBinding`. |
| `RemoveProfileFromPolicy` | Removing the last profile is allowed — the policy becomes Any Device. |

### Errors

New, all distinguishable: `ErrInvalidPolicyName`, `ErrDuplicatePolicyName`,
`ErrDuplicateProfileBinding`, `ErrResourceAlreadyAssigned`, `ErrPolicyAssigned`.
Existing `ErrNotFound` / `ErrWorkspaceMismatch` reused. No new error framework.

### Deliberate omissions

- **No audit/enforce guard.** `CreateResourceBinding` refuses to bind an
  enforce-mode profile with zero requirements (`ErrEmptyEnforceProfile`). That
  guard exists *only* because of `device_profiles.mode`, so it is intentionally
  **not** carried into `AddProfileToPolicy`. Porting it would smuggle the retired
  concept into the new model.
- **Notification lives in the resolvers**, matching every existing posture
  mutation. The store does not notify.

### Concurrency

`AssignResourcePolicy` and `DeleteResourcePolicy` both take row locks. Delete also
treats the `ON DELETE NO ACTION` foreign-key violation as `ErrPolicyAssigned`, so
an assignment that races the count still produces the right domain error rather
than a raw database error.

## Tests

`controller/internal/posture/resource_policy_store_integration_test.go` — real
PostgreSQL, run with `-race`:

| Subtest | Covers |
|---|---|
| `CRUD` | create/get/list/update/delete, name trimming, per-tenant name uniqueness |
| `ProfileAttachments` | zero / one / many profiles, duplicate rejected, cross-workspace both directions |
| `ResourceAssignment` | assign, idempotent re-assign, second policy refused, delete-while-assigned refused, unassign, cascade |
| `ConcurrentAssignment` | two goroutines, two policies — exactly one wins |
| `DatabaseRejectsCrossWorkspaceRows` | raw SQL bypassing the store; the schema itself refuses |
| `LegacyBindingsPreserved` | legacy `resource_profile_bindings` still works alongside the new model |

```bash
cd controller
PKI_TEST_DATABASE_URL=<admin dsn> go test ./internal/posture/... -race
```
