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

## Post-Phase Fixes

### Fix: soft-delete left orphaned SCIM groups, tokens and sync instances (2026-09-07)
**Issue:** After deleting an Okta connection and creating a fresh one for the same Okta org, group
operations from Okta failed with `404 group not found`. The SCIM-managed `hermes` group was still
present in Zecurity, `origin='scim'`, `connection_id` pointing at the **deleted** connection.

**Root Cause:** Phase 9's `SoftDeleteConnection` (`controller/internal/idp/store.go`) only flipped
`identity_connections.status` to `'deleted'`. Everything keyed on `connection_id` survived:

| Orphaned state | Consequence |
|---|---|
| `groups` (`origin='scim'`) | unreachable by the replacement connection — it carries a different `connection_id`, so Okta's group updates 404 |
| `group_members` | frozen membership still resolvable by the ACL compiler |
| `scim_tokens` | still active against a deleted connection |
| `scim_sync_instances` | orphaned sync metadata |

`groups.connection_id` is `ON DELETE CASCADE`, so these vanish on a **hard** delete but survive the
**soft** delete that ADR-025 §12 mandates whenever linked users exist.

**Fix applied** (`ba68e24`) — `SoftDeleteConnection` now runs the status flip plus cleanup in a single
transaction:

```go
// BEFORE: single UPDATE, no cleanup.
tag, err := s.pool.Exec(ctx,
    `UPDATE identity_connections SET status = 'deleted', updated_at = NOW()
      WHERE id = $1 AND tenant_id = $2`, connectionID, tenantID)
return nil

// AFTER: tx { status flip → delete scim groups → revoke tokens → purge sync instances }
tx, err := s.pool.Begin(ctx)
defer tx.Rollback(ctx)
// … UPDATE identity_connections … (ErrConnectionNotFound when 0 rows)
`DELETE FROM groups WHERE workspace_id = $1 AND origin = 'scim' AND connection_id = $2`
`UPDATE scim_tokens SET revoked_at = COALESCE(revoked_at, NOW())
   WHERE workspace_id = $1 AND connection_id = $2 AND revoked_at IS NULL`
`DELETE FROM scim_sync_instances WHERE workspace_id = $1 AND connection_id = $2`
return tx.Commit(ctx)
```

`group_members` rows are removed by the database, not by this code: `group_members.group_id
REFERENCES groups(id) ON DELETE CASCADE` (`controller/migrations/012_groups_acl.sql:18`) — verified.

**Related files also changed:**
- `controller/graph/resolvers/idp.resolvers.go` — delete path simplified to call the unified
  `SoftDeleteConnection`.
- `controller/internal/scim/group_cleanup_integration_test.go` — **new**, ~500 lines, covers the full
  delete → re-provision lifecycle.

**Scope guard (ADR-025 §12).** Only SCIM-owned metadata is removed. Manually-created users and
groups, `external_identities` links, roles, policies, devices and audit history are all untouched.

> ⚠️ **Unratified ADR reading — to verify.** `path.md` and
> [[Sprint17/Member1-Frontend/Phase7-SCIM-Config-Missing-Fields]] both recorded orphaned SCIM groups
> as an **open product decision**, and Phase 7 states plainly that deleting them is *"forbidden by
> ADR-025 §12"*. This fix deletes them. It does so on the reading that §12's preservation list
> (user, external identity link, local roles, resources/policies/access rules, device assignments,
> audit history) **does not include groups**, so groups are SCIM-owned metadata rather than protected
> identity data. That reading is asserted in commit `ba68e24` and in the working note it came from —
> **it is not recorded in ADR-025 itself.** Treat the product decision as *made in code, not
> ratified*: confirm against ADR-025 §12 (and, if it holds, amend the ADR) before relying on it.
> The alternative Phase 7 proposed — reconcile groups onto the new connection by `external_id` under
> the same admin-approval rule as users — was not implemented and was not explicitly rejected.

**Verified:**
```bash
cd controller && go test ./internal/scim/... \
  -run "TestGroupCleanupOnConnectionDelete_Integration|TestReprovisionAfterConnectionDelete_Integration"
```
Requires a live Postgres via `PKI_TEST_DATABASE_URL` (see `agent.md` for the dev DSN — do not inline
credentials here).
