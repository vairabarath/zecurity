import { useNavigate, useParams } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { ArrowLeft, KeyRound } from 'lucide-react'
import { GetIdpConnectionsDocument } from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState, StatusPill } from '@/lib/console'
import { ScimConfigCard, type ScimConfigConnection } from '@/components/scim/ScimConfigCard'
import { ScimBaseUrlBox } from '@/components/scim/ScimBaseUrlBox'
import { ScimTokenPanel } from '@/components/scim/ScimTokenPanel'

type IdpConnectionDetailData = ScimConfigConnection & {
  protocol: string
  issuer: string
  status: string
}

export default function IdpConnectionDetail() {
  const { id } = useParams<{ id: string }>()
  const navigate = useNavigate()

  // There is no single-connection query in the schema — idpConnections returns
  // the workspace's full (small) list, so we select from it rather than adding a
  // backend field this phase does not own.
  const { data, loading, error, refetch } = useQuery(GetIdpConnectionsDocument, {
    fetchPolicy: 'cache-and-network',
  })

  const connection = (data?.idpConnections ?? []).find(
    (c) => c.id === id,
  ) as IdpConnectionDetailData | undefined

  if (loading && !connection) {
    return (
      <div className="space-y-4">
        <Skeleton className="h-10 w-64" />
        <Skeleton className="h-48 w-full" />
        <Skeleton className="h-32 w-full" />
      </div>
    )
  }

  if (error && !connection) {
    return (
      <ErrorState
        title="Could not load connection"
        description={error.message}
        action={
          <Button variant="outline" onClick={() => void refetch()}>
            Retry
          </Button>
        }
      />
    )
  }

  if (!connection) {
    return (
      <EmptyState
        icon={<KeyRound className="h-6 w-6" />}
        title="Connection not found"
        description="This identity-provider connection does not exist, or you no longer have access to it."
        action={
          <Button variant="outline" onClick={() => navigate('/idp-connections')}>
            Back to Identity Providers
          </Button>
        }
      />
    )
  }

  return (
    <div className="space-y-6">
      <div>
        <button
          onClick={() => navigate('/idp-connections')}
          className="mb-3 inline-flex items-center gap-1.5 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
        >
          <ArrowLeft className="h-3.5 w-3.5" />
          Identity Providers
        </button>

        <div className="page-header">
          <div className="min-w-0">
            <h2 className="page-title truncate">{connection.displayName}</h2>
            <p className="page-subtitle truncate">
              {connection.provider} · {connection.protocol} · {connection.issuer}
            </p>
          </div>
          <div className="flex flex-wrap items-center gap-2">
            <StatusPill
              label={connection.status}
              tone={connection.status === 'active' ? 'ok' : 'muted'}
            />
            {connection.managed ? (
              <span className="status-pill border-border bg-secondary text-muted-foreground">
                platform-managed
              </span>
            ) : null}
          </div>
        </div>
      </div>

      <ScimConfigCard connection={connection} onChanged={() => void refetch()} />
      <ScimBaseUrlBox />
      <ScimTokenPanel connectionId={connection.id} />
    </div>
  )
}
