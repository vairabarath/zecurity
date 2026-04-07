import { useQuery } from '@apollo/client/react'
import { GetWorkspaceDocument, type GetWorkspaceQuery } from '@/generated/graphql'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'

export default function Settings() {
  const { data, loading } = useQuery<GetWorkspaceQuery>(GetWorkspaceDocument)

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
