import { useMemo, useState } from 'react'
import { useNavigate, useSearchParams } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { GetIdpConnectionsDocument, GetScimConflictsDocument } from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState } from '@/lib/console'
import { ConflictRow } from '@/components/scim/ConflictRow'

export default function ScimConflicts() {
  const [params, setParams] = useSearchParams()
  const navigate = useNavigate()

  // Deep-link support: ?connectionId=… (from IdpConnectionDetail). When absent,
  // the admin picks a connection — this page is a work list, not a connection tab,
  // and scimConflicts(connectionId:) requires a selection regardless.
  const connectionId = params.get('connectionId') ?? ''
  const [picker, setPicker] = useState(connectionId)

  const connections = useQuery(GetIdpConnectionsDocument, {
    fetchPolicy: 'cache-and-network',
  })

  const activeId = connectionId || picker

  const conflicts = useQuery(GetScimConflictsDocument, {
    variables: { connectionId: activeId },
    skip: !activeId,
    fetchPolicy: 'cache-and-network',
  })

  const connectionOptions = useMemo(
    () => connections.data?.idpConnections ?? [],
    [connections.data],
  )

  function selectConnection(id: string) {
    setPicker(id)
    if (id) {
      params.set('connectionId', id)
    } else {
      params.delete('connectionId')
    }
    setParams(params, { replace: true })
  }

  if (!activeId) {
    return (
      <div className="space-y-6">
        <PageHeader />
        <div className="space-y-3">
          <label className="text-sm font-medium" htmlFor="conflict-connection">
            Identity provider connection
          </label>
          <select
            id="conflict-connection"
            value={picker}
            onChange={(e) => selectConnection(e.target.value)}
            className="w-full max-w-md rounded-md border border-input bg-background px-3 py-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30"
          >
            <option value="">Select a connection…</option>
            {connectionOptions.map((c) => (
              <option key={c.id} value={c.id}>
                {c.displayName} · {c.provider}
              </option>
            ))}
          </select>
          {connections.loading ? (
            <Skeleton className="h-10 w-64" />
          ) : connectionOptions.length === 0 ? (
            <EmptyState
              title="No connections"
              description="Create an identity-provider connection before resolving provisioning conflicts."
              action={
                <Button variant="outline" onClick={() => navigate('/idp-connections')}>
                  Identity Providers
                </Button>
              }
            />
          ) : null}
        </div>
      </div>
    )
  }

  const rows = conflicts.data?.scimConflicts ?? []

  return (
    <div className="space-y-6">
      <PageHeader />

      <div className="space-y-2">
        <label className="text-sm font-medium" htmlFor="conflict-connection">
          Identity provider connection
        </label>
        <select
          id="conflict-connection"
          value={activeId}
          onChange={(e) => selectConnection(e.target.value)}
          className="w-full max-w-md rounded-md border border-input bg-background px-3 py-2 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/30"
        >
          {connectionOptions.map((c) => (
            <option key={c.id} value={c.id}>
              {c.displayName} · {c.provider}
            </option>
          ))}
        </select>
      </div>

      {conflicts.loading && !conflicts.data ? (
        <div className="space-y-3">
          <Skeleton className="h-24 w-full" />
          <Skeleton className="h-24 w-full" />
        </div>
      ) : conflicts.error ? (
        <ErrorState
          title="Could not load conflicts"
          description={conflicts.error.message}
          action={
            <Button variant="outline" onClick={() => void conflicts.refetch()}>
              Retry
            </Button>
          }
        />
      ) : rows.length === 0 ? (
        <EmptyState
          title="No provisioning conflicts"
          description="This connection has no pending or resolved directory-link conflicts."
        />
      ) : (
        <div className="space-y-3">
          {rows.map((conflict) => (
            <ConflictRow
              key={conflict.id}
              conflict={conflict}
              connectionId={activeId}
              onResolved={() => void conflicts.refetch()}
            />
          ))}
        </div>
      )}
    </div>
  )
}

function PageHeader() {
  const navigate = useNavigate()
  return (
    <div>
      <button
        onClick={() => navigate('/idp-connections')}
        className="mb-3 inline-flex items-center gap-1.5 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
      >
        Identity Providers
      </button>
      <div className="page-header">
        <div className="min-w-0">
          <h2 className="page-title truncate">Provisioning conflicts</h2>
          <p className="page-subtitle truncate">
            Resolve directory-link collisions for a SCIM connection
          </p>
        </div>
      </div>
    </div>
  )
}
