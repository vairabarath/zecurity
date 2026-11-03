---
type: phase
member: M1
sprint: 17
phase: 7
title: SCIM Groups
status: done
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
- [x] `scim`-origin groups keyed on connection-scoped `external_id` (never display name); coexist with `manual`/`system` groups of the same name. (verified: CreateGroup inserts origin='scim' w/ connection_id+external_id; 034 drops name-uniqueness, adds `idx_groups_scim_external_id` + `idx_groups_manual_name`)
- [x] Sync `group_members` from SCIM membership ops; call `policy.Notifier.NotifyPolicyChange` on change (same as a manual edit). (verified: ReplaceGroup/PatchGroup/DeleteGroup call `s.notifier.NotifyPolicyChange`)
- [x] Out-of-order membership (member references a not-yet-provisioned user) → **`404`**, no dangling membership; provider retries. (verified: ReplaceGroup/PatchGroup resolve ALL members before mutating; unknown → 404, zero-partial)
- [x] All access-rule / membership references use an **origin-aware identifier**. (verified: every group/member query is scoped by `(workspace_id, connection_id, origin='scim')`; `OriginAwareID` helper for audit/logging)

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

## Verification (re-checked against the codebase, 2026-08-28)
- Implementation present and wired: `controller/internal/scim/groups.go` (Create/Get/List/
  Update/Replace/Patch/DeleteGroup + membership helpers, RFC 7644 op-boundary fix confirmed);
  HTTP handlers registered in `users.go:29-34` (POST/GET/GET{id}/PUT/PATCH/DELETE
  `/scim/v2/Groups`). Migration `034_scim_directory_sync.sql` matches the audit notes exactly
  (drops legacy `UNIQUE(workspace_id,name)`, adds the two origin-scoped partial unique
  indexes). Bug #2 regression tests are present in `groups_integration_test.go`.
- STATUS NOTE on prior audit's "Not yet committed": the fix and tests WERE committed —
  `git log` shows commit `5aa9f8f feat(scim): Phase 7 SCIM Groups + RFC 7644 replace-op fix (Bug #2)`.
  The "not committed" line in the audit above is stale.
- STATUS NOTE on prior audit's "gofmt clean": `groupRow` struct-field alignment in
  `groups.go` was NOT gofmt-clean (gofmt -l flagged it). Re-ran `gofmt -w` on 2026-08-28;
  16 lines reformatted. After fix: `gofmt -l` clean, `go build ./...` OK, `go vet ./...` OK.
  The gofmt reformat is an UNCOMMITTED working-tree change (implementation stays uncommitted
  per Sprint17 discipline — only commit when explicitly told).
- `go test ./internal/scim/... -count=1` could NOT be re-run here: no live Postgres is
  available in this environment (`PKI_TEST_DATABASE_URL` unset, `pg_isready` down). The Bug #2
  tests exist and build/vet pass, but live-DB test execution was not independently verified in
  this pass — flagged, not assumed green.

---

## Post-Phase Fixes

### Fix: Okta group member removals silently never applied (2026-09-03)

**Issue:** Removing a user from a group in the Okta dashboard did not remove them
from the group in Zecurity — the user stayed a member indefinitely. Reported for
"any change to group members".

**Root cause:** `internal/scim/groups.go`, `groupMemberValues()`. The function
lowercased the whole PATCH `path` and then ran the member-filter regex against
that lowercased string:

```go
normed := strings.ToLower(strings.TrimSpace(path))
...
if m := memberFilterRe.FindStringSubmatch(normed); m != nil {
    return []string{m[1]}, nil   // ← id has been case-folded
}
```

Okta sends a member removal as a targeted filter path —
`{"op":"remove","path":"members[value eq \"00u16w7qu5saDn60H698\"]"}` — and Okta
user ids are **mixed case**. The captured id came back lowercased
(`00u16w7qu5sadn60h698`) and `userIDsByExternalOrUUID` compares it byte-for-byte
against `external_identities.subject` (`AND ei.subject = $3`). It matched zero
rows, the UUID fallback rejected it as malformed, so `PatchGroup` collected it
into `unknowns` and returned **404 `unknown members: …`**. No membership delta was
ever computed, and Okta's push task failed.

Lowercasing was intended only to make the *attribute name* case-insensitive; it
also destroyed the *value*, which is an opaque provider identifier.

**Why the existing tests missed it:** the only coverage
(`groups_integration_test.go:441`) removes the member id `"h-1"`, which is already
lowercase, so case folding was invisible.

**Fix applied (`internal/scim/groups.go`):**
```go
// BEFORE:
var memberFilterRe = regexp.MustCompile(`^members\[value eq "([^"]*)"\]$`)
normed := strings.ToLower(strings.TrimSpace(path))
... FindStringSubmatch(normed)

// AFTER: attribute name case-insensitive + whitespace-tolerant via the regex,
// matched against the ORIGINAL-CASE path so the captured value survives intact.
var memberFilterRe = regexp.MustCompile(`(?i)^members\[\s*value\s+eq\s+"([^"]*)"\s*\]$`)
trimmed := strings.TrimSpace(path)   // used for the filter match
normed  := strings.ToLower(trimmed)  // used ONLY for path keyword comparison
... FindStringSubmatch(trimmed)
```
`normed` is still used for the `""` / `"members"` keyword comparisons, where
case-insensitivity is correct and no value is extracted.

**Scope:** only the targeted-removal filter path was affected. Adds and
replaces carry member ids in the `value` array (`{"value":[{"value":"00u…"}]}`),
never in the path, so their case was always preserved.

**Not changed:** `ei.subject = $3` stays a case-SENSITIVE comparison. The subject
is an opaque per-issuer identifier (ADR-024); case-folding it in SQL would be the
same class of mistake one layer down, and would risk collapsing two distinct
provider identities.

**Tests:** new `internal/scim/group_patch_path_test.go` —
- `TestGroupMemberValues_FilterPathPreservesValueCase` (real Okta id shape)
- `TestGroupMemberValues_FilterPathAttributeCaseAndSpacing` (`Members[Value eq …]`,
  extra whitespace)
- `TestGroupMemberValues_FilterPathRejectedForNonRemove` (add/replace still refused)
- `TestPatchGroupFromOps_OktaRemoveShapePreservesCase` (through the Operations parser)

Verified these FAIL on the unfixed tree (`got [00u16w7qu5sadn60h698] want
[00u16w7qu5saDn60H698]`) and pass after. `go build ./...` clean;
`go test ./internal/... ./graph/...` all green.
