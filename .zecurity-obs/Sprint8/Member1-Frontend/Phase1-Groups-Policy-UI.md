---
type: phase
status: done
sprint: 8
member: M1
phase: Phase1-Groups-Policy-UI
depends_on:
  - M2-Phase1-Policy-Schema
  - M3-Phase1-Policy-Compiler
tags:
  - frontend
  - react
  - policy-engine
  - groups
---

# M1 Phase 1 — Groups + Policy UI

---

## What You're Building

Build the admin UI for group-based resource access.

---

## Required Screens

### Groups Page

- List groups in the current workspace.
- Create group.
- Edit group name/description.
- Delete group with confirmation.

### Members Tab

- Show users in a group.
- Add users to group.
- Remove users from group.

### Resources Tab

- Show resources assigned to a group.
- Assign resources to group.
- Remove resource assignment.
- Clearly show disabled/empty policy states.

### Resources Page Integration

- Show which groups have access to each resource.
- Link from resource to group details where useful.

---

## UX Rules

- This is an admin/workflow UI, not a marketing page.
- Keep tables dense and scannable.
- Show default-deny clearly: no groups assigned means no client access.
- Use existing design system and GraphQL patterns.

---

## Build Check

```bash
cd admin && npm run build
```

---

## Files Touched / Changed

### Created
| File | What |
|------|------|
| `admin/src/pages/Groups.tsx` | Groups list page — create/edit/delete modals, table with name/description/member count/resource count/created/actions columns |
| `admin/src/pages/GroupDetail.tsx` | Group detail page — breadcrumb + back link, Members tab (add/remove with user dropdown), Resources tab (assign/unassign with resource dropdown, default-deny warning) |

### Modified
| File | What |
|------|------|
| `admin/src/graphql/queries.graphql` | Added `GetUsers`, `GetGroups`, `GetGroup` queries; added `groups { id name }` to `GetAllResources` |
| `admin/src/graphql/mutations.graphql` | Added `CreateGroup`, `UpdateGroup`, `DeleteGroup`, `AddGroupMember`, `RemoveGroupMember`, `AssignResourceToGroup`, `UnassignResourceFromGroup` |
| `admin/src/App.tsx` | Added `/groups` and `/groups/:id` routes |
| `admin/src/components/layout/Sidebar.tsx` | Added Groups nav item between Resources and Settings |
| `admin/src/pages/Resources.tsx` | Added Groups column with badge pills (clickable, up to 2 + overflow), updated grid from 8 to 9 columns |
| `admin/codegen.yml` | Added `../controller/graph/policy.graphqls` to schema sources so group types are generated |

---

## Post-Phase Fixes

### Fix: `Makefile` GQLGEN_VERSION mismatch (fixed by M1 on 2026-04-30)

**Issue:** `make gqlgen` failed because `Makefile` had `GQLGEN_VERSION := v0.17.89` but `controller/go.mod` pinned `v0.17.90`.

**Fix:** Updated `Makefile` line 3: `GQLGEN_VERSION := v0.17.90`.

---

### Fix: Apollo Client v4 HTTP 401 not caught by error link (fixed by M1 on 2026-04-30)

**Issue:** Active users were being logged out after 15 minutes even while using the app. Apollo's error link checked for GraphQL-level `UNAUTHORIZED` errors but Apollo v4 treats HTTP 401 responses as network errors (`CombinedGraphQLErrors.is()` returns false for them), so the refresh/logout logic never triggered.

**Root Cause:** Apollo Client v4 changed how HTTP-level errors are surfaced. A 401 response comes through as a network error object with `statusCode: 401`, not as a GraphQL error.

**Fix Applied (`admin/src/apollo/links/error.ts`):**

```ts
// BEFORE: only checked GraphQL-level errors
function isUnauthorizedError(error: unknown): boolean {
  return CombinedGraphQLErrors.is(error) &&
    error.errors.some((e) => (e.extensions?.code as string) === 'UNAUTHORIZED')
}

// AFTER: also catches HTTP 401 network errors
function isUnauthorizedError(error: unknown): boolean {
  if (CombinedGraphQLErrors.is(error)) {
    return error.errors.some((e) => (e.extensions?.code as string) === 'UNAUTHORIZED')
  }
  if (error instanceof Error && 'statusCode' in error) {
    return (error as unknown as { statusCode: number }).statusCode === 401
  }
  if (error instanceof Response) {
    return error.status === 401
  }
  return false
}
```

---

### Fix: Refresh token TTL not sliding — active users logged out (fixed by M1 on 2026-04-30)

**Issue:** Redis TTL for refresh tokens was set only at login and never extended. After 7 days the refresh token expired regardless of activity, logging out active users.

**Root Cause:** `refresh.go` issued a new access token but never reset the Redis TTL on the refresh token cookie.

**Fix Applied (`controller/internal/auth/refresh.go`):** After issuing the new access token, parse `JWTRefreshTTL` from config and call `SetRefreshToken` to slide the Redis expiry:

```go
ttl, perr := time.ParseDuration(s.cfg.JWTRefreshTTL)
if perr != nil {
    ttl = 7 * 24 * time.Hour
}
s.redisClient.SetRefreshToken(ctx, userID, cookieToken, ttl)
```
