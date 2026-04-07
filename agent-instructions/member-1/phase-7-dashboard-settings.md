# Phase 7 — Dashboard + Settings Pages

Needs `schema.graphqls` from Member 4 Phase 1 for codegen hooks.
Full data display needs Member 4's resolvers running (me + workspace queries).

---

## File 1: `admin/src/pages/Dashboard.tsx`

**Path:** `admin/src/pages/Dashboard.tsx`

```tsx
import { useMeQuery, useGetWorkspaceQuery, WorkspaceStatus } from '@/generated/graphql'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'

// Status badge color mapping
const statusVariant: Record<WorkspaceStatus, 'default' | 'secondary' | 'destructive' | 'outline'> = {
  [WorkspaceStatus.Active]:       'default',
  [WorkspaceStatus.Provisioning]: 'secondary',
  [WorkspaceStatus.Suspended]:    'destructive',
  [WorkspaceStatus.Deleted]:      'outline',
}

export default function Dashboard() {
  const { data: meData,        loading: meLoading }  = useMeQuery()
  const { data: wsData,        loading: wsLoading }  = useGetWorkspaceQuery()

  return (
    <div className="space-y-6">
      <h1 className="text-xl font-semibold">Dashboard</h1>

      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">

        {/* User card */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Your Account</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            {meLoading ? (
              <>
                <Skeleton className="h-4 w-48" />
                <Skeleton className="h-4 w-24" />
              </>
            ) : (
              <>
                <p className="text-sm">{meData?.me.email}</p>
                <Badge variant="outline">{meData?.me.role}</Badge>
              </>
            )}
          </CardContent>
        </Card>

        {/* Workspace card */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Workspace</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            {wsLoading ? (
              <>
                <Skeleton className="h-4 w-48" />
                <Skeleton className="h-4 w-24" />
              </>
            ) : (
              <>
                <p className="text-sm font-medium">{wsData?.workspace.name}</p>
                <p className="text-xs text-muted-foreground">
                  {wsData?.workspace.slug}
                </p>
                <Badge variant={statusVariant[wsData?.workspace.status!]}>
                  {wsData?.workspace.status}
                </Badge>
              </>
            )}
          </CardContent>
        </Card>

      </div>
    </div>
  )
}
```

### Dashboard Data Sources

| Card | Query | Backend Owner |
|------|-------|---------------|
| Your Account | `me` | Member 4 resolver → `users` table scoped by tenant_id + user_id |
| Workspace | `workspace` | Member 4 resolver → `workspaces` table scoped by tenant_id |

Both queries are protected. The JWT's `tenant_id` and `sub` claims determine what data is returned.
The frontend never sends tenant_id — it comes from the JWT, enforced by middleware.

---

## File 2: `admin/src/pages/Settings.tsx`

**Path:** `admin/src/pages/Settings.tsx`

```tsx
import { useGetWorkspaceQuery } from '@/generated/graphql'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'

export default function Settings() {
  const { data, loading } = useGetWorkspaceQuery()

  return (
    <div className="space-y-6">
      <h1 className="text-xl font-semibold">Settings</h1>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">Workspace Info</CardTitle>
        </CardHeader>
        <CardContent>
          {loading ? (
            <div className="space-y-2">
              <Skeleton className="h-4 w-48" />
              <Skeleton className="h-4 w-64" />
            </div>
          ) : (
            <dl className="space-y-3 text-sm">
              <div className="flex gap-4">
                <dt className="text-muted-foreground w-24">Name</dt>
                <dd>{data?.workspace.name}</dd>
              </div>
              <div className="flex gap-4">
                <dt className="text-muted-foreground w-24">Slug</dt>
                <dd className="font-mono text-xs">{data?.workspace.slug}</dd>
              </div>
              <div className="flex gap-4">
                <dt className="text-muted-foreground w-24">ID</dt>
                <dd className="font-mono text-xs">{data?.workspace.id}</dd>
              </div>
              <div className="flex gap-4">
                <dt className="text-muted-foreground w-24">Created</dt>
                <dd>{new Date(data?.workspace.createdAt!).toLocaleDateString()}</dd>
              </div>
            </dl>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
```

Settings is read-only in this sprint.
Workspace name editing, user invitations, policy management — all future sprints.

---

## How to Test Before Backend Resolvers Are Ready

Apollo's mock link can return hardcoded data for UI development:

```typescript
const mockUser = {
  id: '1',
  email: 'test@test.com',
  role: 'ADMIN' as Role,
  provider: 'google',
  createdAt: new Date().toISOString(),
}

const mockWorkspace = {
  id: 'ws-123',
  slug: 'acme-corp',
  name: 'Acme Corporation',
  status: 'ACTIVE' as WorkspaceStatus,
  createdAt: new Date().toISOString(),
}
```

Or test by manually setting the Zustand store in the browser console
and verifying the Dashboard/Settings pages render correctly.

---

## Verification Checklist

```
[x] Loading skeletons shown while queries are in flight
[x] me query renders user email and role on Dashboard
[x] workspace query renders workspace name, slug, status on Dashboard
[x] WorkspaceStatus badge color matches status value:
    - ACTIVE → default (green/primary)
    - PROVISIONING → secondary
    - SUSPENDED → destructive (red)
    - DELETED → outline
[x] Settings page shows workspace name, slug, ID, created date
[x] Workspace ID displayed in monospace font
[x] Created date formatted with toLocaleDateString()
[x] Sign out clears Apollo cache AND Zustand store
[x] After sign out, navigating to /dashboard redirects to /login
```

> **Note: Apollo Client v4 API change from the v3 plan**
> - `useMeQuery()` → `useQuery<MeQuery>(MeDocument)` (codegen no longer generates React hooks)
> - `useGetWorkspaceQuery()` → `useQuery<GetWorkspaceQuery>(GetWorkspaceDocument)` (same reason)
> - All behavioral requirements from the plan are preserved.
