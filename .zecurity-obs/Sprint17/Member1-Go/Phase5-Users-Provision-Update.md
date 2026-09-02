---
type: phase
member: M1
sprint: 17
phase: 5
title: SCIM Users — Provision + Update
status: done
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
| `controller/internal/scim/provisioner.go` | **new** — `identity.Provisioner` adapter (SCIM-owned, no workspace) |
| `controller/internal/scim/response.go` | **new** — RFC 7644 envelopes/errors |
| `controller/internal/scim/helpers.go` | **new** — decode/err helpers |
| `controller/internal/scim/users_integration_test.go` | **new** — DB integration tests |
| `controller/cmd/server/main.go` | wire `/scim/v2/` under SCIM auth middleware |
| `controller/internal/scim/validation_test.go` | test-harness isolation fix (own child DB) |

## Steps
- [x] `DirectoryService` binds `(workspace_id, connection_id)` from the validated token into **every** query/mutation (§10) — never from the request payload.
- [x] Provision → `Resolver` on `(connection, CanonicalIdentityKey)`; miss → `Linker` JIT-create with `provisioned_by=scim`, `provisioning_owner=scim`, `sync_instance_id`. Hit on JIT/manual → `409 identity_conflict` (Phase 8 writes the conflict row — Phase 5 only returns the error, does not persist the conflict record).
- [x] Update → **directory-owned attrs only** (`email`, `status`, `sync_instance_id`); reject writes to Zecurity-owned fields at the mutation layer. `active=false` → `status='suspended'` (ADR-025 §5).
- [x] RFC 7644 envelopes/errors; emit `meta.version`; filter `eq` on `userName`/`externalId` only; tombstones hidden from collections.

## Known gap (deferred — documented, not silently dropped)
The `users` table has **no** `name` / `displayName` / `title` / `department` columns. The ADR lists
these as "directory-owned" for v1, but the v1 schema cannot store them. Phase 5 therefore scopes
directory-owned attribute writes to the columns that exist (`email`, `status`, `sync_instance_id`)
and returns a `400` for patches targeting unsupported attributes (`name`, etc.). This is a schema gap,
not a behavior bug — surfaced here and tracked for the schema-extension phase. See ADR-025 §5.

## Rules
- Never key on email. Ownership is enforced server-side, not just in the UI.
- Scope is always derived from the SCIM token (`TokenFromContext`); the request payload never
  influences which workspace/connection a mutation targets.

## Build gate
`go build ./...` + DB-integration: provision + update + scope isolation (A cannot touch B).
All 8 integration subtests pass against a live Postgres (`PKI_TEST_DATABASE_URL`).
