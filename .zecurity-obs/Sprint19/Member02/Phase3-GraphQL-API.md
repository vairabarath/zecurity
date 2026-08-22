---
type: phase
member: M02
sprint: 19
phase: 3
title: GraphQL Resource Policy API
status: planned
depends_on: [2]
tags: [resource-policy, graphql, controller, pending-16]
---

# Phase 3 — GraphQL Resource Policy API

## Goal

Expose the Resource Policy model through the Controller GraphQL API for the administrative surface.

## Required operations

Implement the API required by the existing project conventions for:

- [ ] List Resource Policies for the current workspace.
- [ ] Get a Resource Policy by ID.
- [ ] Create a Resource Policy.
- [ ] Update Resource Policy metadata if supported by the agreed model.
- [ ] Delete a Resource Policy safely.
- [ ] Assign a Resource Policy to a Resource.
- [ ] Unassign a Resource Policy from a Resource where the target model permits it.
- [ ] List Device Profiles attached to a Resource Policy.
- [ ] Add a Device Profile to a Resource Policy.
- [ ] Remove a Device Profile from a Resource Policy.
- [ ] Query the Resource associated with a Resource Policy.
- [ ] Expose enough data for the UI to show whether a Resource Policy has zero, one, or multiple profiles.

## Rules

- All operations must be workspace-scoped.
- Do not expose a fake Audit/Enforce switch.
- Do not make the Connector consume Resource Policy GraphQL data.
- Use existing authorization/admin guards.
- Use existing error conventions.
- Mutations must trigger the existing policy invalidation/propagation path.

## Verification

- [ ] Schema generation succeeds.
- [ ] Resolver tests cover successful and rejected mutations.
- [ ] Cross-workspace operations fail.
- [ ] Duplicate/second policy assignment fails.
- [ ] Empty Device Profile selection remains valid.
