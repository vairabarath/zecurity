---
title: Sprint 20 Fix 19 — deleting a resource names the groups that lose access (F25)
date: 2026-10-27
type: fix
status: FIXED and verified live (2026-10-27) — commit dfcaed60 (local), on the #146 branch.
owner: Sathiya (M1)
findings: F25 (Sprint20/path.md → Post-Sprint Fixes); new finding F54 (resource lists never returned their groups)
---

# Fix 19 — resource delete dialog (F25)

## F25

**Issue:** deleting a resource silently deleted its access rules (`access_rules.resource_id … ON DELETE CASCADE`). The delete was a plain `window.confirm`.
- In the live run, the resource was deleted and re-created because its host couldn't be edited (F24).
- The new resource had no group, so users lost access without any warning.

**Decision (user, 2026-10-27):** the delete dialog warns, and since a resource can now be edited (Fix 18), it offers Edit instead.

**Fix** (`admin/src/pages/ResourceDetail.tsx`):
- Delete opens a dialog that lists the groups that lose access, and warns that creating the resource again doesn't bring them back.
- It also says that to change the host, port or protocol you should edit instead, which keeps group access.
- Buttons: **Cancel**, **Edit instead** (opens the Edit Resource dialog) and **Delete**.
- The cascade itself is kept, because a rule for a deleted resource means nothing.

## F54 (new, found while building the dialog) — resource lists never returned their groups

**Issue:**
- `Resource.groups` was set only in the `assignResourceToGroup` / `unassignResourceFromGroup` responses (`loadResourceWithGroups`).
- `allResources` and `resources(remoteNetworkId)` always returned `groups: []`.
- So the Resources list's Groups column was always "none", and the dialog would have said "No group has access" for a resource that groups can reach.

**Fix:**
- `policy.Store.ListGroupsByResource` reads every enabled access rule of the workspace, with its group, in one query.
- `attachResourceGroups` fills `Groups` for both list queries.

## Tests

- `TestResourceQueries_ReturnGroups`: both queries return the groups of enabled rules only, ordered by name, and none for a resource without rules.
- 2 mutants killed: groups not attached; disabled rules counted.
- Controller 1435 pass, 0 fail. Admin 114 pass; ESLint clean across the whole project.

## Live (2026-10-27, admin UI on :5173, controller with this commit)

1. `f24-res` (in `cl-group`) → Delete. The dialog showed "1 group loses access: cl-group", the warning, and the edit hint.
2. **Edit instead** opened Edit Resource for `f24-res`. Nothing was deleted; the resource still had its rule.
3. Throwaway `f25-tmp` (in `cl-group`) → Delete → **Delete**. The page went to `/resources`; the resource and its rule were gone (0 rows).
4. The Resources list's Groups column shows `cl-group` for the assigned resources.
