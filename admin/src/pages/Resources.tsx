import { useState } from 'react'
import { useQuery, useMutation } from '@apollo/client/react'
import { motion } from 'framer-motion'
import {
  GetAllResourcesDocument,
  GetRemoteNetworksDocument,
  type GetAllResourcesQuery,
  ShieldStatus,
} from '@/generated/graphql'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { Button } from '@/components/ui/button'
import {
  ProtectResourceDocument,
  UnprotectResourceDocument,
  DeleteResourceDocument,
} from '@/generated/graphql'
import { CreateResourceModal } from '@/components/CreateResourceModal'
import { cn } from '@/lib/utils'
import {
  Box,
  Lock,
  Unlock,
  Trash2,
  Plus,
  Inbox,
  AlertCircle,
  CircleDot,
  CircleDotDashed,
  Loader2,
  Wifi,
  WifiOff,
} from 'lucide-react'
import { toast } from 'sonner'

type Resource = GetAllResourcesQuery['allResources'][number]

const statusConfig: Record<string, { label: string; className: string; icon: React.ReactNode }> = {
  pending: {
    label: 'Pending',
    className: 'text-gray-600 bg-gray-500/10 border-gray-500/20',
    icon: <CircleDotDashed className="h-3 w-3 fill-gray-400 text-gray-400" />,
  },
  managing: {
    label: 'Managing',
    className: 'text-amber-600 bg-amber-500/10 border-amber-500/20',
    icon: <Loader2 className="h-3 w-3 animate-spin text-amber-500" />,
  },
  protecting: {
    label: 'Protecting',
    className: 'text-amber-600 bg-amber-500/10 border-amber-500/20',
    icon: <Loader2 className="h-3 w-3 animate-spin text-amber-500" />,
  },
  protected: {
    label: 'Protected',
    className: 'text-emerald-600 bg-emerald-500/10 border-emerald-500/20',
    icon: <CircleDot className="h-3 w-3 fill-emerald-500 text-emerald-500" />,
  },
  failed: {
    label: 'Failed',
    className: 'text-red-600 bg-red-500/10 border-red-500/20',
    icon: <AlertCircle className="h-3 w-3 text-red-500" />,
  },
  removing: {
    label: 'Removing',
    className: 'text-orange-600 bg-orange-500/10 border-orange-500/20',
    icon: <Loader2 className="h-3 w-3 animate-spin text-orange-500" />,
  },
  deleted: {
    label: 'Deleted',
    className: 'text-gray-400 bg-gray-200/30 border-gray-300/30 line-through',
    icon: <CircleDotDashed className="h-3 w-3 text-gray-400" />,
  },
}

function relativeTime(dateStr: string | null | undefined): string {
  if (!dateStr) return '—'
  const diff = Date.now() - new Date(dateStr).getTime()
  if (diff < 0) return 'just now'
  const s = Math.floor(diff / 1000)
  if (s < 60) return `${s}s ago`
  const m = Math.floor(s / 60)
  if (m < 60) return `${m}m ago`
  const h = Math.floor(m / 60)
  if (h < 24) return `${h}h ago`
  const d = Math.floor(h / 24)
  if (d < 30) return `${d}d ago`
  return `${Math.floor(d / 30)}mo ago`
}

export default function Resources() {
  const [showAdd, setShowAdd] = useState(false)
  const [deletingId, setDeletingId] = useState<string | null>(null)

  const { data: networkData } = useQuery(GetRemoteNetworksDocument)
  const networks = networkData?.remoteNetworks ?? []

  const { data, loading, refetch } = useQuery(GetAllResourcesDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const [protectResource, { loading: protecting }] = useMutation(ProtectResourceDocument, {
    onCompleted: () => {
      toast.success('Resource protection started')
      refetch()
    },
    onError: (err) => toast.error(err.message),
  })

  const [unprotectResource, { loading: unprotecting }] = useMutation(UnprotectResourceDocument, {
    onCompleted: () => {
      toast.success('Resource unprotected')
      refetch()
    },
    onError: (err) => toast.error(err.message),
  })

  const [deleteResourceMut] = useMutation(DeleteResourceDocument, {
    onCompleted: () => {
      toast.success('Resource deleted')
      refetch()
      setDeletingId(null)
    },
    onError: (err) => {
      toast.error(err.message)
      setDeletingId(null)
    },
  })

  const resources: Resource[] = data?.allResources ?? []
  const protectedCount = resources.filter((r) => r.status === 'protected').length

  const handleDelete = (id: string) => {
    if (window.confirm('Are you sure you want to delete this resource?')) {
      setDeletingId(id)
      deleteResourceMut({ variables: { id } })
    }
  }

  const formatPort = (from: number, to: number) => {
    if (from === to) return from.toString()
    return `${from}–${to}`
  }

  return (
    <div className="space-y-6">
      {/* Header */}
      <motion.div
        className="flex items-center justify-between"
        initial={{ opacity: 0, y: -8 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.4 }}
      >
        <div className="flex items-center gap-4">
          <div className="flex h-11 w-11 items-center justify-center rounded-xl bg-primary/10 ring-1 ring-primary/20">
            <Box className="h-5 w-5 text-primary" />
          </div>
          <div>
            <h1 className="font-display text-xl font-bold tracking-wide">Resources</h1>
            <p className="text-xs text-muted-foreground mt-0.5">
              Managed resources protected by shields
            </p>
          </div>
        </div>

        <div className="flex items-center gap-3">
          {!loading && (
            <>
              <div className="flex items-center gap-1.5 rounded-lg bg-muted/60 px-3 py-1.5 ring-1 ring-border/30">
                <Lock className="h-3 w-3 text-emerald-500" />
                <span className="text-[11px] font-mono text-muted-foreground">
                  {protectedCount} protected
                </span>
              </div>
              <div className="flex items-center gap-1.5 rounded-lg bg-muted/60 px-3 py-1.5 ring-1 ring-border/30">
                <span className="text-[11px] font-mono text-muted-foreground">
                  {resources.length} total
                </span>
              </div>
            </>
          )}
          <Button
            onClick={() => setShowAdd(true)}
            disabled={networks.length === 0}
            className="gap-2 text-[12px]"
            size="sm"
          >
            <Plus className="h-4 w-4" />
            Add Resource
          </Button>
        </div>
      </motion.div>

      {/* Table */}
      <motion.div
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ delay: 0.1, duration: 0.4 }}
        className="rounded-xl border border-border bg-white overflow-hidden"
      >
        <div className="overflow-x-auto">
          <div className="grid grid-cols-[1.2fr_100px_90px_100px_130px_110px_110px_80px] gap-4 px-5 py-3 border-b border-border/50 bg-muted/20 min-w-[960px]">
            {['Name', 'Host IP', 'Protocol', 'Port', 'Shield', 'Status', 'Last Verified', ''].map((col, i) => (
              <span
                key={i}
                className={cn(
                  'text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60',
                  i === 7 && 'text-right',
                )}
              >
                {col}
              </span>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-[960px]">
              {Array.from({ length: 5 }).map((_, i) => (
                <div
                  key={i}
                  className="grid grid-cols-[1.2fr_100px_90px_100px_130px_110px_110px_80px] gap-4 items-center px-5 py-3.5 border-b border-border/20 last:border-0"
                >
                  <Skeleton className="h-4 w-32" />
                  <Skeleton className="h-4 w-20" />
                  <Skeleton className="h-5 w-16" />
                  <Skeleton className="h-4 w-16" />
                  <Skeleton className="h-5 w-20" />
                  <Skeleton className="h-5 w-20" />
                  <Skeleton className="h-4 w-16" />
                  <Skeleton className="h-6 w-16 ml-auto" />
                </div>
              ))}
            </div>
          ) : resources.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-16 text-center">
              <div className="rounded-full p-3 bg-primary/5 border border-primary/10 mb-3">
                <Inbox className="w-6 h-6 text-primary/40" />
              </div>
              <p className="text-sm text-foreground/70">No resources defined</p>
              <p className="text-xs text-muted-foreground mt-0.5 max-w-xs">
                {networks.length === 0
                  ? 'Create a remote network first, then add a resource to protect.'
                  : 'Click "Add Resource" to define a protected resource.'}
              </p>
            </div>
          ) : (
            <div className="min-w-[960px]">
              {resources.map((r) => {
                const st = statusConfig[r.status] || statusConfig.pending
                const shieldOffline = r.shield?.status === ShieldStatus.Disconnected
                const noShield = !r.shield

                return (
                  <motion.div
                    key={r.id}
                    initial={{ opacity: 0 }}
                    animate={{ opacity: 1 }}
                    className="grid grid-cols-[1.2fr_100px_90px_100px_130px_110px_110px_80px] gap-4 items-center px-5 py-3.5 border-b border-border/20 last:border-0 hover:bg-muted/10 transition-colors"
                  >
                    <div className="flex flex-col">
                      <span className="font-medium text-sm">{r.name}</span>
                      {r.description && (
                        <span className="text-xs text-muted-foreground truncate max-w-[200px]">
                          {r.description}
                        </span>
                      )}
                    </div>

                    <span className="font-mono text-sm">{r.host}</span>

                    <Badge variant="outline" className="w-fit text-[10px] uppercase">
                      {r.protocol}
                    </Badge>

                    <span className="font-mono text-sm">{formatPort(r.portFrom, r.portTo)}</span>

                    {noShield ? (
                      <div className="flex items-center gap-1.5 text-muted-foreground">
                        <AlertCircle className="h-3 w-3" />
                        <span className="text-xs">No shield</span>
                      </div>
                    ) : shieldOffline ? (
                      <div className="flex items-center gap-1.5 text-amber-600">
                        <WifiOff className="h-3 w-3" />
                        <span className="text-xs">{r.shield?.name}</span>
                      </div>
                    ) : (
                      <div className="flex items-center gap-1.5 text-emerald-600">
                        <Wifi className="h-3 w-3" />
                        <span className="text-xs">{r.shield?.name}</span>
                      </div>
                    )}

                    <div>
                      <Badge
                        className={cn('gap-1.5 text-[10px]', st.className)}
                      >
                        {st.icon}
                        {st.label}
                      </Badge>
                      {r.errorMessage && (
                        <p className="text-[10px] text-red-500 mt-0.5 truncate max-w-[100px]" title={r.errorMessage}>
                          {r.errorMessage}
                        </p>
                      )}
                    </div>

                    <span className="text-xs text-muted-foreground">
                      {relativeTime(r.lastVerifiedAt)}
                    </span>

                    <div className="flex items-center gap-1 ml-auto">
                      {!noShield && !shieldOffline && (
                        <>
                          {(r.status === 'pending' || r.status === 'failed') && (
                            <Button
                              size="sm"
                              variant="ghost"
                              className="h-7 gap-1 text-[11px]"
                              disabled={protecting}
                              onClick={() => protectResource({ variables: { id: r.id } })}
                            >
                              <Lock className="h-3 w-3" />
                              Protect
                            </Button>
                          )}
                          {r.status === 'protected' && (
                            <Button
                              size="sm"
                              variant="ghost"
                              className="h-7 gap-1 text-[11px]"
                              disabled={unprotecting}
                              onClick={() => unprotectResource({ variables: { id: r.id } })}
                            >
                              <Unlock className="h-3 w-3" />
                              Unprotect
                            </Button>
                          )}
                        </>
                      )}
                      {(r.status === 'managing' ||
                        r.status === 'protecting' ||
                        r.status === 'removing') && (
                          <Button size="sm" variant="ghost" className="h-7" disabled>
                            <Loader2 className="h-3 w-3 animate-spin" />
                          </Button>
                        )}
                      {noShield && (
                        <span className="text-xs text-muted-foreground/50">—</span>
                      )}
                      {!noShield && r.status !== 'deleted' && (
                        <Button
                          size="sm"
                          variant="ghost"
                          className="h-7 text-red-500 hover:text-red-600"
                          disabled={deletingId === r.id}
                          onClick={() => handleDelete(r.id)}
                        >
                          <Trash2 className="h-3 w-3" />
                        </Button>
                      )}
                    </div>
                  </motion.div>
                )
              })}
            </div>
          )}
        </div>
      </motion.div>

      <CreateResourceModal
        open={showAdd}
        onOpenChange={setShowAdd}
        onSuccess={() => {
          refetch()
          setShowAdd(false)
        }}
      />
    </div>
  )
}