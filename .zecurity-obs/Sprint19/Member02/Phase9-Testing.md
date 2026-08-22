---
type: phase
member: M02
sprint: 19
phase: 9
title: Full PENDING-16 Testing
status: planned
depends_on: [1, 2, 3, 4, 5, 6, 7, 8]
tags: [tests, postgres, graphql, acl, posture, frontend, pending-16]
---

# Phase 9 — Full PENDING-16 Testing

## Goal

Convert every PENDING-16 requirement and Sprint 19 invariant into tests.

## Database integration tests

- [ ] One Resource cannot have two Resource Policies.
- [ ] A Resource Policy can have zero profiles.
- [ ] A Resource Policy can have multiple profiles.
- [ ] Duplicate Policy/Profile binding is rejected.
- [ ] Cross-workspace relationships are rejected.
- [ ] Existing legacy bindings survive the migration.
- [ ] Migration preserves effective access relationships.

## Controller/store tests

- [ ] Create/read/update/delete policy.
- [ ] Assign/unassign policy.
- [ ] Add/remove profile.
- [ ] Workspace isolation.
- [ ] Duplicate/invalid operations.
- [ ] Policy change triggers notification.

## GraphQL tests

- [ ] Queries return correct workspace-scoped data.
- [ ] Mutations enforce admin authorization.
- [ ] Invalid second policy assignment fails.
- [ ] Empty profile list is accepted.
- [ ] Profile binding changes propagate.

## ACL compiler tests

- [ ] Zero profiles = Any Device.
- [ ] One passing profile = allow.
- [ ] One failing profile = deny.
- [ ] Multiple profiles = OR.
- [ ] All selected profiles fail = deny.
- [ ] Profile revision mismatch/stale posture follows existing semantics.
- [ ] Final ACL still contains the expected allowed SPIFFE IDs.
- [ ] Device Profile IDs do not need to be added to the Connector ACL protocol.

## Propagation tests

- [ ] Cache invalidation occurs after policy mutation.
- [ ] Epoch blocks stale in-flight compilation.
- [ ] New ACL is pushed to live Connectors.
- [ ] Connector catches up after missed push.
- [ ] Revoked/removed authorization is reflected after convergence.

## Linux end-to-end tests

- [ ] Real Linux posture report.
- [ ] Real Linux posture evaluation.
- [ ] Passing Linux device authorized.
- [ ] Failing Linux device denied.
- [ ] Resource Policy binding changes alter live authorization.

## Frontend tests

- [ ] Resource Policy CRUD.
- [ ] Resource assignment constraint.
- [ ] Profile selection.
- [ ] Empty selection = Any Device.
- [ ] Multiple selection = OR.
- [ ] No Audit/Enforce control.
- [ ] Linux Device Profile creation/editing.

## Required gates

```bash
cd controller && go build ./...
cd controller && go vet ./...
cd controller && go test ./...
```

Run the repository's frontend test/build commands as defined by its existing package scripts.

All mandatory tests must pass before Sprint 19 is marked complete.
