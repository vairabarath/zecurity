import { useState } from 'react'
import { useParams, Link } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { GetRemoteNetworkDocument } from '@/generated/graphql'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Plus,
  ChevronRight,
  ShieldOff,
  Trash2,
  Shield,
} from 'lucide-react'
import { cn } from '@/lib/utils'

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

// Shield status config — will use generated ShieldStatus enum after codegen
type ShieldStatusKey = 'PENDING' | 'ACTIVE' | 'DISCONNECTED' | 'REVOKED'

const statusConfig: Record<ShieldStatusKey, { label: string; className: string }> = {
  PENDING: {
    label: 'Pending',
    className: 'text-gray-400 bg-gray-400/10 border-gray-400/20',
  },
  ACTIVE: {
    label: 'Active',
    className: 'text-emerald-400 bg-emerald-400/10 border-emerald-400/20',
  },
  DISCONNECTED: {
    label: 'Disconnected',
    className: 'text-amber-400 bg-amber-400/10 border-amber-400/20',
  },
  REVOKED: {
    label: 'Revoked',
    className: 'text-red-400 bg-red-400/10 border-red-400/20',
  },
}

// Placeholder type until codegen generates the real one
interface ShieldRow {
  id: string
  name: string
  status: ShieldStatusKey
  interfaceAddr: string | null
  connectorId: string
  lastSeenAt: string | null
  version: string | null
  hostname: string | null
}

export default function Shields() {
  const { id } = useParams<{ id: string }>()
  const [, setShowInstall] = useState(false)

  const { data: networkData } = useQuery(GetRemoteNetworkDocument, {
    variables: { id: id! },
    skip: !id,
  })

  // TODO: Replace with useGetShieldsQuery after codegen
  const loading = false
  const shields: ShieldRow[] = []

  const networkName = networkData?.remoteNetwork?.name ?? 'Network'

  return (
    <div className="space-y-6">
      {/* Breadcrumb */}
      <div className="flex items-center gap-2 text-sm">
        <Link to="/remote-networks" className="text-muted-foreground hover:text-foreground transition-colors">
          Remote Networks
        </Link>
        <ChevronRight className="w-3.5 h-3.5 text-muted-foreground/50" />
        <Link to={`/remote-networks/${id}/connectors`} className="text-muted-foreground hover:text-foreground transition-colors">
          {networkName}
        </Link>
        <ChevronRight className="w-3.5 h-3.5 text-muted-foreground/50" />
        <span className="text-foreground font-medium">Shields</span>
      </div>

      {/* Page Header */}
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-display font-bold tracking-tight">Shields</h1>
          <p className="text-sm text-muted-foreground mt-1">
            Manage shield agents for <span className="text-foreground/80">{networkName}</span>.
          </p>
        </div>
        <Button onClick={() => setShowInstall(true)} className="gap-2">
          <Plus className="w-4 h-4" />
          Add Shield
        </Button>
      </div>

      {/* Loading Skeletons */}
      {loading && !shields.length && (
        <Card className="bg-card/60">
          <CardContent className="p-0">
            {Array.from({ length: 4 }).map((_, i) => (
              <div key={i} className="flex items-center gap-4 px-5 py-4 border-b border-border/30 last:border-0">
                <Skeleton className="h-4 w-40" />
                <Skeleton className="h-5 w-20" />
                <Skeleton className="h-4 w-28" />
                <Skeleton className="h-4 w-24" />
                <Skeleton className="h-4 w-20" />
                <Skeleton className="h-4 w-16 ml-auto" />
              </div>
            ))}
          </CardContent>
        </Card>
      )}

      {/* Empty State */}
      {!loading && shields.length === 0 && (
        <div className="flex flex-col items-center justify-center py-20 text-center">
          <div className="rounded-full p-4 bg-primary/5 border border-primary/10 mb-4">
            <Shield className="w-8 h-8 text-primary/40" />
          </div>
          <h3 className="text-lg font-display font-semibold text-foreground/80">No shields yet</h3>
          <p className="text-sm text-muted-foreground mt-1 max-w-sm">
            Deploy a shield agent on a resource host to protect it with zero-trust network access.
          </p>
          <Button onClick={() => setShowInstall(true)} className="gap-2 mt-4" variant="outline">
            <Plus className="w-4 h-4" />
            Add your first shield
          </Button>
        </div>
      )}

      {/* Shield Rows */}
      {shields.length > 0 && (
        <Card className="bg-card/60 backdrop-blur-sm border-border/50 overflow-hidden">
          <CardContent className="p-0">
            {/* Table Header */}
            <div className="grid grid-cols-[1fr_100px_120px_1fr_100px_100px_120px] gap-4 px-5 py-3 border-b border-border/50 bg-muted/20">
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Name</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Status</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Interface</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Hostname</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Last Seen</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Version</span>
              <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60 text-right">Actions</span>
            </div>

            {/* Rows */}
            {shields.map((shield, i) => {
              const status = statusConfig[shield.status]
              const canRevoke = shield.status === 'ACTIVE' || shield.status === 'DISCONNECTED'
              const canDelete = shield.status === 'REVOKED' || shield.status === 'PENDING'

              return (
                <div
                  key={shield.id}
                  className={cn(
                    'grid grid-cols-[1fr_100px_120px_1fr_100px_100px_120px] gap-4 items-center px-5 py-3.5 border-b border-border/20 last:border-0 transition-colors hover:bg-muted/10',
                  )}
                  style={{ animationDelay: `${i * 50}ms` }}
                >
                  {/* Name */}
                  <div className="flex items-center gap-2.5 min-w-0">
                    <Shield className="w-4 h-4 text-muted-foreground shrink-0" />
                    <span className="text-sm font-medium truncate">{shield.name}</span>
                  </div>

                  {/* Status */}
                  <div>
                    <Badge variant="outline" className={cn('text-[10px] font-mono border', status.className)}>
                      {status.label}
                    </Badge>
                  </div>

                  {/* Interface */}
                  <span className="text-xs text-muted-foreground font-mono">
                    {shield.interfaceAddr ?? '-'}
                  </span>

                  {/* Hostname */}
                  <span className="text-xs text-muted-foreground font-mono truncate">
                    {shield.hostname ?? '-'}
                  </span>

                  {/* Last Seen */}
                  <span className="text-xs text-muted-foreground font-mono">
                    {relativeTime(shield.lastSeenAt)}
                  </span>

                  {/* Version */}
                  <span className="text-xs text-muted-foreground font-mono">
                    {shield.version ?? '-'}
                  </span>

                  {/* Actions */}
                  <div className="flex items-center justify-end gap-1.5">
                    {canRevoke && (
                      <Button
                        variant="outline"
                        size="sm"
                        className="h-7 px-2 text-[10px] text-amber-400 hover:text-amber-300 hover:bg-amber-400/10 border-amber-400/20"
                        onClick={() => {/* TODO: revokeShield mutation */}}
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
                        onClick={() => {/* TODO: deleteShield mutation */}}
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
