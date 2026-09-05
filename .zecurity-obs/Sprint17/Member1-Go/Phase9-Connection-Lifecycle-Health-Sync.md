---
type: phase
member: M1
sprint: 17
phase: 9
title: Connection Lifecycle + Identity Health + Sync Instances
status: done
depends_on: [5]
tags: [go, identity, scim, lifecycle, health, sync-instance, pending-05]
---

# Phase 9 — Connection Lifecycle + Identity Health + Sync Instances

> Depends on Phase 5. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §12, Addendum · [[PENDING-05-SCIM-Implementation-Plan]] P9 · [[Identity-Lifecycle-and-Ownership-Design-Review]] §8.

## Goal
Give a SCIM connection a safe lifecycle, surface sync health, and make disable→re-enable reconnects clean.

## Files
| File | Change |
| --- | --- |
| `controller/internal/idp/store.go` | `TouchConnectionSync` (stamp `last_sync_at`); `SuspendSCIMUsersForConnection`; `SetSCIMUsersUnmanaged`; `LinkedUserCount`; `SoftDeleteConnection`; `LastSyncAt`/`TenantIDOrEmpty` on `Connection`. |
| `controller/internal/scim/sync_instance.go` | **new** — `OpenSyncInstance` / `EnsureSyncInstance` / `CurrentSyncInstance` / `ReconcileStaleUsers` / `ReconcileStaleGroups`. |
| `controller/internal/scim/directory_service.go` | `touchSyncInstance` now stamps connection `last_sync_at` (not only the sync instance); `IdentityHealth` (Healthy/Delayed/Disconnected/Disabled); `resolveScope` refuses non-active connections (DISABLE stops SCIM). |
| `controller/internal/scim/users.go` + `groups.go` | writes route through `EnsureSyncInstance`; group create stamps `sync_instance_id`; membership writes stamp connection `last_sync_at`. |
| `controller/graph/idp.graphqls` + regenerated `generated.go`/`models_gen.go` | `WorkspaceIdpConnection.identityHealth` + `lastSyncAt`; `deleteIdpConnection(id, force!)`. |
| `controller/graph/resolvers/idp.resolvers.go` | DISABLE suspends + unmanages SCIM users + revokes sessions; DELETE guarded soft-delete (preserves users) when linked users exist. |
| `controller/graph/resolvers/idp_helpers.go` | `idpConnToGQL` populates `lastSyncAt` + derives `identityHealth`. |
| `controller/migrations/035_groups_sync_instance.sql` | **new** — adds `groups.sync_instance_id` (omitted from 034). |
| `controller/internal/scim/lifecycle_integration_test.go` | **new** — 10 subtests (sync instance, health thresholds, disable stops SCIM, delete guard/soft-delete, ownership-not-auto-restored). |

## Steps
- [x] Connection `status: active → disabled → deleted`. **DISABLE** (reversible): SCIM writes refused (resolveScope 403), sessions revoked via `Revoker.BumpGeneration`, linked SCIM users **suspended** (`status='suspended'`), `provisioning_owner scim→unmanaged` (immutable `provisioned_by` preserved). **DELETE** guarded: refused without `force` when `linked_users > 0`; with users present → soft-delete (`status='deleted'` + ownership flip, users/external_identities preserved); 0 linked users → hard delete.
- [x] `last_sync_at` → **Identity Health**: Healthy (≤24h) / Delayed (≤72h) / Disconnected (>72h or null) / Disabled (status≠active). Surfaced on `WorkspaceIdpConnection`.
- [x] `scim_sync_instances`: `EnsureSyncInstance` opens one UUID per connection (reused until reconnect); provisioned users / external_identities / groups stamp `sync_instance_id`; `ReconcileStaleUsers` / `ReconcileStaleGroups` identify prior-instance objects on reconnect.

## Rules
- DISABLE is the reversible off-switch; DELETE never silently mass-suspends. Users are never orphaned active-with-no-login-path. Re-enable does NOT auto-restore SCIM ownership (explicit re-enroll is a separate future action).

## Deferred (out of scope, per ADR §12 re-enable flow)
- The explicit authorized admin action that re-enrolls `unmanaged` users back to `scim` ownership after a re-enable. Phase 9 guarantees ownership is NOT auto-restored; the re-enroll verb is a separate future action.
- Frontend health badge is Phase 12 (backend GraphQL surface only delivered here).

## Build gate
`go build ./...` + `go vet ./...` + `lifecycle_integration_test.go` (10 subtests) + full `go test ./internal/scim/... ./internal/idp/... ./graph/...` green on live Postgres.
