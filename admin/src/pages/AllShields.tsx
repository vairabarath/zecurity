import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { motion } from 'framer-motion'
import {
  GetRemoteNetworksDocument,
  ShieldStatus,
  type GetRemoteNetworksQuery,
} from '@/generated/graphql'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { Button } from '@/components/ui/button'
import { InstallCommandModal } from '@/components/InstallCommandModal'
import { cn } from '@/lib/utils'
import {
  Shield,
  Zap,
  Clock,
  ArrowRight,
  Inbox,
  Plus,
  Network,
  CircleDot,
  CircleDotDashed,
  Ban,
} from 'lucide-react'

type NetworkShield = GetRemoteNetworksQuery['remoteNetworks'][number]['shields'][number] & {
  networkId: string
  networkName: string
}

const statusConfig: Record<ShieldStatus, { label: string; className: string; icon: React.ReactNode }> = {
  [ShieldStatus.Active]: {
    label: 'Active',
    className: 'text-emerald-600 bg-emerald-500/10 border-emerald-500/20',
    icon: <CircleDot className="h-3 w-3 fill-emerald-500 text-emerald-500" />,
  },
  [ShieldStatus.Disconnected]: {
    label: 'Disconnected',
    className: 'text-amber-600 bg-amber-500/10 border-amber-500/20',
    icon: <CircleDotDashed className="h-3 w-3 fill-amber-500 text-amber-500" />,
  },
  [ShieldStatus.Pending]: {
    label: 'Pending',
    className: 'text-gray-600 bg-gray-500/10 border-gray-500/20',
    icon: <CircleDotDashed className="h-3 w-3 fill-gray-400 text-gray-400" />,
  },
  [ShieldStatus.Revoked]: {
    label: 'Revoked',
    className: 'text-red-600 bg-red-500/10 border-red-500/20',
    icon: <Ban className="h-3 w-3 text-red-500" />,
  },
}

function relativeTime(dateStr: string | null | undefined): string {
  if (!dateStr) return 'Never'
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

export default function AllShields() {
  const [showAdd, setShowAdd] = useState(false)

  const { data, loading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const networks = data?.remoteNetworks ?? []
  const allShields: NetworkShield[] = networks.flatMap((n) =>
    n.shields.map((s) => ({ ...s, networkId: n.id, networkName: n.name })),
  )
  const activeCount = allShields.filter((s) => s.status === ShieldStatus.Active).length

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
            <Shield className="h-5 w-5 text-primary" />
          </div>
          <div>
            <h1 className="font-display text-xl font-bold tracking-wide">Shields</h1>
            <p className="text-xs text-muted-foreground mt-0.5">
              Resource host agents enforcing zero-trust access
            </p>
          </div>
        </div>

        <div className="flex items-center gap-3">
          {!loading && (
            <>
              <div className="flex items-center gap-1.5 rounded-lg bg-muted/60 px-3 py-1.5 ring-1 ring-border/30">
                <Zap className="h-3 w-3 text-emerald-500" />
                <span className="text-[11px] font-mono text-muted-foreground">
                  {activeCount} active
                </span>
              </div>
              <div className="flex items-center gap-1.5 rounded-lg bg-muted/60 px-3 py-1.5 ring-1 ring-border/30">
                <span className="text-[11px] font-mono text-muted-foreground">
                  {allShields.length} total
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
            Add Shield
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
          <div className="grid grid-cols-[1fr_110px_140px_130px_110px_110px_80px] gap-4 px-5 py-3 border-b border-border/50 bg-muted/20 min-w-[860px]">
            {['Name', 'Status', 'Network', 'Interface', 'Hostname', 'Last Seen', ''].map((col, i) => (
              <span
                key={i}
                className={cn(
                  'text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60',
                  i === 6 && 'text-right',
                )}
              >
                {col}
              </span>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-[860px]">
              {Array.from({ length: 5 }).map((_, i) => (
                <div
                  key={i}
                  className="grid grid-cols-[1fr_110px_140px_130px_110px_110px_80px] gap-4 items-center px-5 py-3.5 border-b border-border/20 last:border-0"
                >
                  <Skeleton className="h-4 w-32" />
                  <Skeleton className="h-5 w-20" />
                  <Skeleton className="h-4 w-28" />
                  <Skeleton className="h-4 w-24" />
                  <Skeleton className="h-4 w-24" />
                  <Skeleton className="h-4 w-16" />
                  <Skeleton className="h-6 w-16 ml-auto" />
                </div>
              ))}
            </div>
          ) : allShields.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-16 text-center">
              <div className="rounded-full p-3 bg-primary/5 border border-primary/10 mb-3">
                <Inbox className="w-6 h-6 text-primary/40" />
              </div>
              <p className="text-sm text-foreground/70">No shields enrolled</p>
              <p className="text-xs text-muted-foreground mt-0.5 max-w-xs">
                {networks.length === 0
                  ? 'Create a remote network first, then deploy a shield to it.'
                  : 'Click "Add Shield" to deploy one.'}
              </p>
              {networks.length === 0 && (
                <Link
                  to="/remote-networks"
                  className="inline-flex items-center gap-1 text-xs text-primary hover:underline mt-3"
                >
                  Create a remote network <ArrowRight className="w-3 h-3" />
                </Link>
              )}
            </div>
          ) : (
            <div className="min-w-[860px]">
              {allShields.map((s, i) => {
                const st = statusConfig[s.status]
                return (
                  <motion.div
                    key={s.id}
                    initial={{ opacity: 0 }}
                    animate={{ opacity: 1 }}
                    transition={{ delay: i * 0.03 }}
                    className="grid grid-cols-[1fr_110px_140px_130px_110px_110px_80px] gap-4 items-center px-5 py-3.5 border-b border-border/20 last:border-0 hover:bg-muted/20 transition-colors"
                  >
                    <div className="flex items-center gap-2.5 min-w-0">
                      <div className="flex items-center justify-center h-8 w-8 rounded-lg bg-primary/10 border border-primary/20 shrink-0">
                        <Shield className="h-4 w-4 text-primary" />
                      </div>
                      <Link to={`/shields/${s.id}`} className="text-sm font-medium truncate hover:text-primary transition-colors">
                        {s.name}
                      </Link>
                    </div>

                    <div>
                      <Badge
                        variant="outline"
                        className={cn('text-[10px] font-mono border gap-1', st.className)}
                      >
                        {st.icon}
                        {st.label}
                      </Badge>
                    </div>

                    <Link
                      to={`/remote-networks/${s.networkId}/shields`}
                      className="flex items-center gap-1 text-xs text-primary hover:underline truncate font-mono"
                    >
                      <Network className="h-3 w-3 shrink-0" />
                      <span className="truncate">{s.networkName}</span>
                    </Link>

                    <span className="text-xs text-muted-foreground font-mono">
                      {s.interfaceAddr ?? '—'}
                    </span>

                    <span className="text-xs text-muted-foreground font-mono truncate">
                      {s.hostname ?? '—'}
                    </span>

                    <div className="flex items-center gap-1 text-xs text-muted-foreground">
                      <Clock className="w-3 h-3 shrink-0" />
                      {relativeTime(s.lastSeenAt)}
                    </div>

                    <div className="flex justify-end">
                      <Link
                        to={`/shields/${s.id}`}
                        className="flex items-center gap-1 text-xs text-primary hover:text-primary/80 font-medium transition-colors"
                      >
                        Manage
                        <ArrowRight className="w-3 h-3" />
                      </Link>
                    </div>
                  </motion.div>
                )
              })}
            </div>
          )}
        </div>
      </motion.div>

      <InstallCommandModal
        open={showAdd}
        onClose={() => setShowAdd(false)}
        variant="shield"
      />
    </div>
  )
}
