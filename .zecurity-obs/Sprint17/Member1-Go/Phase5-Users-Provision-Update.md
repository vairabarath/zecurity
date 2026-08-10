---
type: phase
member: M1
sprint: 17
phase: 5
title: SCIM Users — Provision + Update
status: planned
depends_on: [4]
tags: [go, identity, scim, users, provisioning, pending-05]
---

# Phase 5 — SCIM Users: Provision + Update

> Depends on Phase 4. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §4, §5, §9, §10 · [[PENDING-05-SCIM-Implementation-Plan]] P5.

## Goal
Stand up the SCIM Users engine over the existing identity pipeline: create canonical users from the
directory and keep their directory-owned attributes in sync.

## Files
| File | Change |
| --- | --- |
| `controller/internal/scim/directory_service.go` | **new** — `identity.DirectoryService`, scope-binding |
| `controller/internal/scim/users.go` | **new** — POST/GET/PUT/PATCH handlers |
| `controller/internal/identity/*` | reuse `Resolver`/`Linker` (do not rebuild) |

## Steps
- [ ] `DirectoryService` binds `(workspace_id, connection_id)` from the validated token into **every** query/mutation (§10) — never from the request payload.
- [ ] Provision → `Resolver` on `(connection, CanonicalIdentityKey)`; miss → `Linker` JIT-create with `provisioned_by=scim`, `provisioning_owner=scim`, `sync_instance_id`. Hit on JIT/manual → hand to Phase 8 (`409`).
- [ ] Update → **directory-owned attrs only**; reject writes to Zecurity-owned fields at the mutation layer.
- [ ] RFC 7644 envelopes/errors; emit `meta.version`; filter `eq` on `userName`/`externalId` only; tombstones hidden from collections.

## Rules
- Never key on email. Ownership is enforced server-side, not just in the UI.

## Build gate
`go build ./...` + DB-integration: provision + update + scope isolation (A cannot touch B).
