import { useState } from 'react'
import { useParams, Link } from 'react-router-dom'
import { useQuery, useMutation } from '@apollo/client/react'
import {
  GetRemoteNetworkDocument,
  GetConnectorsDocument,
  RevokeConnectorDocument,
  DeleteConnectorDocument,
  ConnectorStatus,
} from '@/generated/graphql'
import type {
  RevokeConnectorMutationVariables,
  DeleteConnectorMutationVariables,
} from '@/generated/graphql'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Plus,
  ChevronRight,
  ShieldOff,
  Trash2,
  Server,
  Plug,
} from 'lucide-react'
import { cn } from '@/lib/utils'
import { InstallCommandModal } from '@/components/InstallCommandModal'

function relativeTime(dateStr: string | null | undefined): string {
  if (!dateStr) return 'Never'
  const now = Date.now()
  const then = new Date(dateStr).getTime()
  const diff = now - then
  if (diff < 0) return 'Just now'

  const seconds = Math.floor(diff / 1000)
  if (seconds < 60) return `${seconds}s ago`
  const minutes = Math.floor(seconds / 60)
  if (minutes < 60) return `${minutes}m ago`
  const hours = Math.floor(minutes / 60)
  if (hours < 24) return `${hours}h ago`
  const days = Math.floor(hours / 24)
  if (days < 30) return `${days}d ago`
  const months = Math.floor(days / 30)
  return `${months}mo ago`
}

const statusConfig: Record<ConnectorStatus, { label: string; className: string }> = {
  [ConnectorStatus.Pending]: {
    label: 'Pending',
    className: 'text-gray-400 bg-gray-400/10 border-gray-400/20',
  },
  [ConnectorStatus.Active]: {
    label: 'Active',
    className: 'text-emerald-400 bg-emerald-400/10 border-emerald-400/20',
  },
  [ConnectorStatus.Disconnected]: {
    label: 'Disconnected',
    className: 'text-amber-400 bg-amber-400/10 border-amber-400/20',
  },
  [ConnectorStatus.Revoked]: {
    label: 'Revoked',
    className: 'text-red-400 bg-red-400/10 border-red-400/20',
  },
}

export default function Connectors() {
  const { id } = useParams<{ id: string }>()
  const [showInstall, setShowInstall] = useState(false)

  const { data: networkData } = useQuery(GetRemoteNetworkDocument, {
    variables: { id: id! },
    skip: !id,
  })

  const { data, loading } = useQuery(GetConnectorsDocument, {
    variables: { remoteNetworkId: id! },
    skip: !id,
    pollInterval: 30000,
  })

  const [revokeConnector] = useMutation(RevokeConnectorDocument, {
    refetchQueries: [{ query: GetConnectorsDocument, variables: { remoteNetworkId: id! } }],
  })

  const [deleteConnector] = useMutation(DeleteConnectorDocument, {
    refetchQueries: [{ query: GetConnectorsDocument, variables: { remoteNetworkId: id! } }],
  })

  async function handleRevoke(connectorId: string) {
    await revokeConnector({ variables: { id: connectorId } as RevokeConnectorMutationVariables })
  }

  async function handleDelete(connectorId: string) {
    await deleteConnector({ variables: { id: connectorId } as DeleteConnectorMutationVariables })
  }

  const networkName = networkData?.remoteNetwork?.name ?? 'Network'
  const connectors = data?.connectors ?? []

  return (
    <div className="space-y-6">
      {/* Breadcrumb */}
      <div className="flex items-center gap-2 text-sm">
        <Link to="/remote-networks" className="text-muted-foreground hover:text-foreground transition-colors">
          Remote Networks
        </Link>
        <ChevronRight className="w-3.5 h-3.5 text-muted-foreground/50" />
        <span className="text-foreground font-medium">{networkName}</span>
      </div>

      {/* Page Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-display font-bold tracking-tight">Connectors</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Manage connectors for <span className="text-foreground/80">{networkName}</span>.
          </p>
        </div>
        <Button onClick={() => setShowInstall(true)} className="gap-2">
          <Plus className="w-4 h-4" />
          Add Connector
        </Button>
      </div>

      {/* Install Modal */}
      {id && (
        <InstallCommandModal
          remoteNetworkId={id}
          open={showInstall}
          onClose={() => setShowInstall(false)}
        />
      )}

      {/* Loading Skeletons */}
      {loading && !data && (
        <Card className="bg-card/60">
          <CardContent className="p-0">
            {Array.from({ length: 4 }).map((_, i) => (
              <div key={i} className="flex items-center gap-4 px-5 py-4 border-b border-border/30 last:border-0">
                <Skeleton className="h-4 w-40" />
                <Skeleton className="h-5 w-20" />
                <Skeleton className="h-4 w-24" />
                <Skeleton className="h-4 w-32 ml-auto" />
              </div>
            ))}
          </CardContent>
        </Card>
      )}

      {/* Empty State */}
      {!loading && connectors.length === 0 && (
        <div className="flex flex-col items-center justify-center py-20 text-center">
          <div className="rounded-full p-4 bg-primary/5 border border-primary/10 mb-4">
            <Plug className="w-8 h-8 text-primary/40" />
          </div>
          <h3 className="text-lg font-display font-semibold text-foreground/80">No connectors yet</h3>
          <p className="text-sm text-muted-foreground mt-1 max-w-sm">
            Deploy a connector to establish a secure tunnel to this network.
          </p>
          <Button onClick={() => setShowInstall(true)} className="gap-2 mt-4" variant="outline">
            <Plus className="w-4 h-4" />
            Add your first connector
          </Button>
        </div>
      )}

      {/* Connector Rows */}
      {connectors.length > 0 && (
        <Card className="bg-card/60 backdrop-blur-sm border-border/50 overflow-hidden">
          <CardContent className="p-0">
            {/* Table Header */}
            <div className="grid grid-cols-[1fr_100px_100px_1fr_100px_120px] gap-4 px-5 py-3 border-b border-border/50 bg-muted/20">
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Name</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Status</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Last Seen</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Hostname</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Version</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60 text-right">Actions</span>
            </div>

            {/* Rows */}
            {connectors.map((connector, i) => {
              const status = statusConfig[connector.status]
              const canRevoke = connector.status === ConnectorStatus.Active || connector.status === ConnectorStatus.Disconnected
              const canDelete = connector.status === ConnectorStatus.Revoked || connector.status === ConnectorStatus.Pending

              return (
                <div
                  key={connector.id}
                  className={cn(
                    'grid grid-cols-[1fr_100px_100px_1fr_100px_120px] gap-4 items-center px-5 py-3.5 border-b border-border/20 last:border-0 transition-colors hover:bg-muted/10',
                  )}
                  style={{ animationDelay: `${i * 50}ms` }}
                >
                  {/* Name */}
                  <div className="flex items-center gap-2.5 min-w-0">
                    <Server className="w-4 h-4 text-muted-foreground shrink-0" />
                    <span className="text-sm font-medium truncate">{connector.name}</span>
                  </div>

                  {/* Status */}
                  <div>
                    <Badge variant="outline" className={cn('text-[10px] font-mono border', status.className)}>
                      {status.label}
                    </Badge>
                  </div>

                  {/* Last Seen */}
                  <span className="text-xs text-muted-foreground font-mono">
                    {relativeTime(connector.lastSeenAt)}
                  </span>

                  {/* Hostname */}
                  <span className="text-xs text-muted-foreground font-mono truncate">
                    {connector.hostname ?? '-'}
                  </span>

                  {/* Version */}
                  <span className="text-xs text-muted-foreground font-mono">
                    {connector.version ?? '-'}
                  </span>

                  {/* Actions */}
                  <div className="flex items-center justify-end gap-1.5">
                    {canRevoke && (
                      <Button
                        variant="outline"
                        size="sm"
                        className="h-7 px-2 text-[10px] text-amber-400 hover:text-amber-300 hover:bg-amber-400/10 border-amber-400/20"
                        onClick={() => handleRevoke(connector.id)}
                      >
                        <ShieldOff className="w-3 h-3 mr-1" />
                        Revoke
                      </Button>
                    )}
                    {canDelete && (
                      <Button
                        variant="outline"
                        size="sm"
                        className="h-7 px-2 text-[10px] text-destructive hover:text-destructive hover:bg-destructive/10 border-destructive/20"
                        onClick={() => handleDelete(connector.id)}
                      >
                        <Trash2 className="w-3 h-3" />
                      </Button>
                    )}
                  </div>
                </div>
              )
            })}
          </CardContent>
        </Card>
      )}
    </div>
  )
}
