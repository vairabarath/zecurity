---
type: phase
member: M1
sprint: 17
phase: 7
title: SCIM Groups
status: planned
depends_on: [5]
tags: [go, identity, scim, groups, policy, pending-05]
---

# Phase 7 — SCIM Groups

> Depends on Phase 5. Full spec: [[ADR-025-SCIM-Directory-Synchronization]] §6, §8 · [[PENDING-05-SCIM-Implementation-Plan]] P7.

## Goal
Sync directory groups + membership so the policy engine reads directory-accurate membership — making
"groups are hints, not authorization" operationally true.

## Files
| File | Change |
| --- | --- |
| `controller/internal/scim/groups.go` | **new** — POST/PUT/PATCH group handlers + membership sync |
| `controller/internal/idp` / groups store | origin-aware group lookup/create |

## Steps
- [ ] `scim`-origin groups keyed on connection-scoped `external_id` (never display name); coexist with `manual`/`system` groups of the same name.
- [ ] Sync `group_members` from SCIM membership ops; call `policy.Notifier.NotifyPolicyChange` on change (same as a manual edit).
- [ ] Out-of-order membership (member references a not-yet-provisioned user) → **`404`**, no dangling membership; provider retries.
- [ ] All access-rule / membership references use an **origin-aware identifier**.

## Rules
- SCIM-group membership is directory-authoritative; manual-group membership stays admin-authoritative. No cross-origin auto-merge.

## Build gate
`go build ./...` + tests: group create/sync, out-of-order 404, ACL snapshot invalidation.

## Audit notes (Phase 7 final review)
- Origin-aware group uniqueness is correctly established by `controller/migrations/034_scim_directory_sync.sql`:
  it runs `DROP CONSTRAINT IF EXISTS groups_workspace_id_name_key` (the legacy
  `UNIQUE (workspace_id, name)` from `012_groups_acl.sql`), then adds two origin-scoped
  partial unique indexes:
  - `idx_groups_manual_name` ON `(workspace_id, name)` WHERE `origin IN ('manual','system')`
  - `idx_groups_scim_external_id` ON `(workspace_id, connection_id, external_id)` WHERE `origin = 'scim'`
  So a `scim` group and a `manual` group may share a display name, and scim identity is
  keyed on `(connection, external_id)`, not name. Intended; no schema change needed.
- The read-only review's "Bug #1" (supposed cross-origin/cross-connection name
  uniqueness with a misleading 409) was a FALSE POSITIVE — raised from a truncated read
  of migration 034 that missed the `DROP CONSTRAINT` at the top. No action required.
- The single confirmed defect, "Bug #2" (RFC 7644 `replace` op with multiple member
  values silently kept only the last member), is FIXED in `controller/internal/scim/groups.go`
  (`groupPatch.Ops []patchOp` preserves the operation boundary; `PatchGroup` resets the
  working set exactly once per replace op and applies all its members). Regression tests added
  to `controller/internal/scim/groups_integration_test.go`:
  - `TestGroups_Integration / patch replace with MULTIPLE values sets the exact set (Bug #2)`
  - `TestGroups_Integration / mixed replace/add/remove in request order (Bug #2)`
  - `TestGroups_HTTP / multi-value replace via real RFC 7644 array form` (GET asserts exact member IDs)
  Fix verified: `gofmt` clean, `go build ./...`, `go vet ./...`, `go test ./internal/scim/... -count=1` all pass against a live Postgres. Not yet committed.
