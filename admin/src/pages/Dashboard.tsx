import { useQuery } from '@apollo/client/react'
import {
  MeDocument,
  GetWorkspaceDocument,
  WorkspaceStatus,
  type MeQuery,
  type GetWorkspaceQuery,
} from '@/generated/graphql'
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
  const { data: meData, loading: meLoading } = useQuery<MeQuery>(MeDocument)
  const { data: wsData, loading: wsLoading } = useQuery<GetWorkspaceQuery>(GetWorkspaceDocument)

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
                <Badge variant={statusVariant[wsData?.workspace.status!] ?? 'outline'}>
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
