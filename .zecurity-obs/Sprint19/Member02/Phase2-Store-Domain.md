---
type: phase
member: M02
sprint: 19
phase: 2
title: Controller Store / Domain Model
status: planned
depends_on: [1]
tags: [resource-policy, go, store, controller, pending-16]
---

# Phase 2 — Controller Store / Domain Model

## Goal

Make Resource Policies first-class Controller objects and provide safe store operations.

## Required work

- [ ] Add Resource Policy domain structs/types.
- [ ] Add store methods for create/read/update/delete Resource Policies.
- [ ] Add store methods for assigning exactly one Resource Policy to a Resource.
- [ ] Add store methods for adding/removing Device Profiles from a Resource Policy.
- [ ] Validate Resource, Resource Policy, Device Profile, and Workspace all belong to the same workspace.
- [ ] Reject assigning a second Resource Policy to a Resource.
- [ ] Allow a Resource Policy to contain zero profiles.
- [ ] Reject duplicate Resource Policy/Profile bindings.
- [ ] Ensure deletion behavior is explicit and safe.
- [ ] Preserve existing `resource_profile_bindings` operations during the temporary coexistence period.
- [ ] Ensure every successful authorization-relevant mutation invokes the existing policy-change notification path.

## Error cases

- [ ] Resource not found.
- [ ] Resource Policy not found.
- [ ] Device Profile not found.
- [ ] Cross-workspace assignment.
- [ ] Resource already has a different Resource Policy.
- [ ] Duplicate profile binding.
- [ ] Attempt to delete a policy still required by a Resource, unless an explicit safe detach operation exists.
- [ ] Invalid/empty identifiers.

## Build gate

```bash
cd controller
go build ./...
go test ./internal/...
```
