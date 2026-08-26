import { useNavigate } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { ChevronRight, KeyRound } from 'lucide-react'
import { GetIdpConnectionsDocument } from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, ErrorState, StatusPill, relativeTime } from '@/lib/console'
import { cn } from '@/lib/utils'

type IdpConnection = {
  id: string
  protocol: string
  provider: string
  displayName: string
  issuer: string
  status: string
  managed: boolean
  lastSyncAt?: string | null
  identityHealth: string
  scimEnabled: boolean
}

function IdpIcon() {
  return (
    <span
      className={cn(
        'grid h-9 w-9 place-items-center rounded-xl',
        'bg-[oklch(0.78_0.10_235/0.14)] text-[oklch(0.78_0.10_235)] border border-[oklch(0.78_0.10_235/0.25)]',
      )}
    >
      <KeyRound className="h-4 w-4" />
    </span>
  )
}

export default function IdpConnections() {
  const navigate = useNavigate()

  const { data, loading, error, refetch } = useQuery(GetIdpConnectionsDocument, {
    fetchPolicy: 'cache-and-network',
  })
  const connections: IdpConnection[] = (data?.idpConnections ?? []) as IdpConnection[]

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Identity Providers</h2>
          <p className="page-subtitle">
            Configure SCIM directory synchronization for your identity-provider connections.
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">{connections.length}</span> connections
          </span>
        </div>
      </div>

      <div className="app-panel overflow-hidden">
        {loading && connections.length === 0 ? (
          <div className="space-y-2 p-5">
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
            <Skeleton className="h-16 w-full" />
          </div>
        ) : error ? (
          <ErrorState
            title="Could not load connections"
            description={error.message}
            action={
              <Button variant="outline" onClick={() => void refetch()}>
                Retry
              </Button>
            }
          />
        ) : connections.length === 0 ? (
          <EmptyState
            icon={<KeyRound className="h-6 w-6" />}
            title="No identity providers"
            description="This workspace has no identity-provider connections yet. Add one in Settings before configuring SCIM."
          />
        ) : (
          <div className="divide-y divide-border/40">
            {connections.map((conn) => (
              <button
                key={conn.id}
                onClick={() => navigate(`/idp-connections/${conn.id}`)}
                className="flex w-full flex-wrap items-center gap-3 px-5 py-4 text-left transition-colors hover:bg-secondary/60"
              >
                <IdpIcon />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <span className="truncate text-sm font-semibold">{conn.displayName}</span>
                    {conn.managed ? (
                      <span className="status-pill border-border bg-secondary text-muted-foreground">
                        platform-managed
                      </span>
                    ) : null}
                  </div>
                  <div className="mt-0.5 truncate text-xs text-muted-foreground">
                    {conn.provider} · {conn.protocol} · {conn.issuer}
                  </div>
                </div>
                <StatusPill
                  label={conn.status}
                  tone={conn.status === 'active' ? 'ok' : 'muted'}
                />
                <StatusPill
                  label={conn.scimEnabled ? 'SCIM on' : 'SCIM off'}
                  tone={conn.scimEnabled ? 'info' : 'muted'}
                />
                <span className="w-28 shrink-0 text-right text-xs text-muted-foreground">
                  {conn.scimEnabled ? `synced ${relativeTime(conn.lastSyncAt)}` : ''}
                </span>
                <ChevronRight className="h-4 w-4 shrink-0 text-muted-foreground" />
              </button>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}
