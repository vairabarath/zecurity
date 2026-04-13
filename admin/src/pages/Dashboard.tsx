import { Link } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { motion } from 'framer-motion'
import {
  MeDocument,
  GetWorkspaceDocument,
  GetRemoteNetworksDocument,
  WorkspaceStatus,
  ConnectorStatus,
  RemoteNetworkStatus,
  type MeQuery,
  type GetWorkspaceQuery,
  type GetRemoteNetworksQuery,
} from '@/generated/graphql'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import {
  Globe,
  Network,
  Clock,
  Server,
  Plug,
  ArrowRight,
  Inbox,
} from 'lucide-react'
import { cn } from '@/lib/utils'

const statusVariant: Record<WorkspaceStatus, 'default' | 'secondary' | 'destructive' | 'outline'> = {
  [WorkspaceStatus.Active]: 'default',
  [WorkspaceStatus.Provisioning]: 'secondary',
  [WorkspaceStatus.Suspended]: 'destructive',
  [WorkspaceStatus.Deleted]: 'outline',
}

interface StatCardProps {
  title: string
  value: string | number
  loading?: boolean
  delay?: number
}

function StatCard({ title, value, loading, delay = 0 }: StatCardProps) {
  return (
    <motion.div
      initial={{ opacity: 0, y: 16 }}
      animate={{ opacity: 1, y: 0 }}
      transition={{ delay, duration: 0.4, ease: [0.22, 1, 0.36, 1] }}
      className="group relative rounded-xl border border-border bg-white p-5 hover:border-primary/30 hover:shadow-md transition-all duration-300"
    >
      <div className="mb-3 text-xs font-medium text-muted-foreground">{title}</div>
      {loading ? (
        <Skeleton className="h-9 w-16" />
      ) : (
        <div className="text-3xl font-semibold text-foreground">{value}</div>
      )}
    </motion.div>
  )
}

function relativeTime(dateStr: string | null | undefined): string {
  if (!dateStr) return 'Never'
  const now = Date.now()
  const then = new Date(dateStr).getTime()
  const diff = now - then
  if (diff < 0) return 'just now'
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

const connectorStatusClass: Record<ConnectorStatus, string> = {
  [ConnectorStatus.Pending]: 'text-gray-500 bg-gray-500/10 border-gray-500/20',
  [ConnectorStatus.Active]: 'text-emerald-600 bg-emerald-500/10 border-emerald-500/20',
  [ConnectorStatus.Disconnected]: 'text-amber-600 bg-amber-500/10 border-amber-500/20',
  [ConnectorStatus.Revoked]: 'text-red-600 bg-red-500/10 border-red-500/20',
}

export default function Dashboard() {
  const { data: meData, loading: meLoading } = useQuery<MeQuery>(MeDocument)
  const { data: wsData, loading: wsLoading } = useQuery<GetWorkspaceQuery>(GetWorkspaceDocument)
  const { data: rnData, loading: rnLoading } = useQuery<GetRemoteNetworksQuery>(
    GetRemoteNetworksDocument,
    { fetchPolicy: 'cache-and-network', pollInterval: 30000 },
  )

  const networks = rnData?.remoteNetworks ?? []
  const activeNetworks = networks.filter((n) => n.status === RemoteNetworkStatus.Active)

  const allConnectors = networks.flatMap((n) =>
    n.connectors.map((c) => ({ ...c, networkId: n.id, networkName: n.name })),
  )
  const activeConnectors = allConnectors.filter((c) => c.status === ConnectorStatus.Active)

  // Most recently seen connectors first; fall back to created_at-style ordering by name
  const recentConnectors = [...allConnectors]
    .sort((a, b) => {
      const aTime = a.lastSeenAt ? new Date(a.lastSeenAt).getTime() : 0
      const bTime = b.lastSeenAt ? new Date(b.lastSeenAt).getTime() : 0
      return bTime - aTime
    })
    .slice(0, 6)

  return (
    <div className="space-y-6">
      {/* Header */}
      <motion.div
        className="flex items-center justify-between"
        initial={{ opacity: 0, y: -8 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.4 }}
      >
        <div>
          <h1 className="text-2xl font-semibold text-foreground">Dashboard</h1>
          <p className="text-sm text-muted-foreground mt-1">Monitor your zero trust network</p>
        </div>
        <div className="flex items-center gap-2">
          <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
            <span className="relative flex h-2 w-2">
              <span className="absolute inline-flex h-full w-full rounded-full bg-secure opacity-50 animate-ping" />
              <span className="relative inline-flex rounded-full h-2 w-2 bg-secure" />
            </span>
            Live
          </span>
        </div>
      </motion.div>

      {/* Stats — all derived from real GraphQL data */}
      <div className="grid grid-cols-1 md:grid-cols-3 gap-4">
        <StatCard
          title="Active Networks"
          value={activeNetworks.length}
          loading={rnLoading && !rnData}
          delay={0.1}
        />
        <StatCard
          title="Total Connectors"
          value={allConnectors.length}
          loading={rnLoading && !rnData}
          delay={0.15}
        />
        <StatCard
          title="Active Connectors"
          value={activeConnectors.length}
          loading={rnLoading && !rnData}
          delay={0.2}
        />
      </div>

      {/* Main Grid */}
      <div className="grid grid-cols-1 lg:grid-cols-3 gap-6">
        {/* Recent Connectors */}
        <motion.div
          className="lg:col-span-2 rounded-xl border border-border bg-white overflow-hidden"
          initial={{ opacity: 0, y: 16 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ delay: 0.3, duration: 0.4 }}
        >
          <div className="flex items-center justify-between px-5 py-4 border-b border-border">
            <div className="flex items-center gap-2">
              <Plug className="w-4 h-4 text-primary" />
              <h2 className="font-semibold text-foreground">Recent Connectors</h2>
            </div>
            <Link to="/remote-networks" className="text-xs text-primary hover:underline">
              View networks
            </Link>
          </div>
          <div className="p-4">
            {rnLoading && !rnData ? (
              <div className="space-y-2">
                {Array.from({ length: 4 }).map((_, i) => (
                  <Skeleton key={i} className="h-14 w-full rounded-xl" />
                ))}
              </div>
            ) : recentConnectors.length === 0 ? (
              <div className="flex flex-col items-center justify-center py-10 text-center">
                <div className="rounded-full p-3 bg-primary/5 border border-primary/10 mb-3">
                  <Inbox className="w-6 h-6 text-primary/40" />
                </div>
                <p className="text-sm text-foreground/70">No connectors yet</p>
                <p className="text-xs text-muted-foreground mt-0.5">
                  Add a remote network and deploy a connector to get started.
                </p>
              </div>
            ) : (
              <div className="space-y-2">
                {recentConnectors.map((c) => (
                  <div
                    key={c.id}
                    className="flex items-center gap-3 p-3 rounded-xl bg-muted/40 hover:bg-muted transition-colors"
                  >
                    <div className="flex items-center justify-center w-8 h-8 rounded-lg bg-primary/10 text-primary shrink-0">
                      <Server className="w-4 h-4" />
                    </div>
                    <div className="flex-1 min-w-0">
                      <div className="flex items-center gap-2">
                        <p className="text-sm font-medium text-foreground truncate">{c.name}</p>
                        <Badge
                          variant="outline"
                          className={cn('text-[10px] font-mono border', connectorStatusClass[c.status])}
                        >
                          {c.status.toLowerCase()}
                        </Badge>
                      </div>
                      <p className="text-xs text-muted-foreground truncate">
                        {c.networkName} · {c.hostname ?? 'no hostname'}
                      </p>
                    </div>
                    <div className="flex items-center gap-1 text-xs text-muted-foreground shrink-0">
                      <Clock className="w-3 h-3" />
                      {relativeTime(c.lastSeenAt)}
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </motion.div>

        {/* Workspace */}
        <motion.div
          initial={{ opacity: 0, y: 16 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ delay: 0.35, duration: 0.4 }}
        >
          <div className="rounded-xl border border-border bg-white overflow-hidden h-full">
            <div className="flex items-center gap-2 px-5 py-4 border-b border-border">
              <Server className="w-4 h-4 text-primary" />
              <h2 className="font-semibold text-foreground">Workspace</h2>
            </div>
            <div className="p-5 space-y-4">
              {wsLoading && !wsData ? (
                <div className="space-y-3">
                  <Skeleton className="h-5 w-32" />
                  <Skeleton className="h-4 w-24" />
                </div>
              ) : (
                <>
                  <div>
                    <p className="text-xs text-muted-foreground mb-1">Name</p>
                    <p className="font-semibold text-foreground">{wsData?.workspace.name}</p>
                  </div>
                  <div>
                    <p className="text-xs text-muted-foreground mb-1">Endpoint</p>
                    <p className="text-sm font-mono text-primary">
                      {wsData?.workspace.slug}.zecurity.in
                    </p>
                  </div>
                  <div>
                    {wsData?.workspace.status && (
                      <Badge variant={statusVariant[wsData.workspace.status] ?? 'outline'}>
                        {wsData.workspace.status}
                      </Badge>
                    )}
                  </div>
                  <div className="pt-3 border-t border-border">
                    <p className="text-xs text-muted-foreground mb-1">Account</p>
                    {meLoading && !meData ? (
                      <Skeleton className="h-4 w-40" />
                    ) : (
                      <p className="text-sm text-foreground truncate">{meData?.me.email}</p>
                    )}
                  </div>
                </>
              )}
            </div>
          </div>
        </motion.div>
      </div>

      {/* Networks */}
      <motion.div
        initial={{ opacity: 0, y: 16 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ delay: 0.4, duration: 0.4 }}
      >
        <div className="flex items-center justify-between mb-4">
          <div className="flex items-center gap-2">
            <Network className="w-4 h-4 text-primary" />
            <h2 className="font-semibold text-foreground">Networks</h2>
          </div>
          <Link to="/remote-networks" className="text-xs text-primary hover:underline">
            Manage networks
          </Link>
        </div>
        {rnLoading && !rnData ? (
          <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
            {Array.from({ length: 3 }).map((_, i) => (
              <Skeleton key={i} className="h-24 w-full rounded-xl" />
            ))}
          </div>
        ) : networks.length === 0 ? (
          <div className="rounded-xl border border-dashed border-border bg-muted/20 p-8 text-center">
            <div className="inline-flex rounded-full p-3 bg-primary/5 border border-primary/10 mb-3">
              <Globe className="w-6 h-6 text-primary/40" />
            </div>
            <p className="text-sm text-foreground/70">No remote networks yet</p>
            <Link
              to="/remote-networks"
              className="inline-flex items-center gap-1 text-xs text-primary hover:underline mt-2"
            >
              Create your first network <ArrowRight className="w-3 h-3" />
            </Link>
          </div>
        ) : (
          <div className="grid grid-cols-1 md:grid-cols-2 lg:grid-cols-3 gap-4">
            {networks.map((n, i) => (
              <motion.div
                key={n.id}
                className="group relative rounded-xl border border-border bg-white p-4 hover:border-primary/30 hover:shadow-md transition-all duration-300"
                initial={{ opacity: 0, scale: 0.98 }}
                animate={{ opacity: 1, scale: 1 }}
                transition={{ delay: i * 0.05, duration: 0.3 }}
                whileHover={{ scale: 1.01 }}
              >
                <Link to={`/remote-networks/${n.id}/connectors`} className="block">
                  <div className="flex items-start justify-between mb-3">
                    <div className="flex items-center gap-2 min-w-0">
                      <Globe className="w-4 h-4 text-primary shrink-0" />
                      <span className="font-medium text-foreground truncate">{n.name}</span>
                    </div>
                    <Badge
                      variant={n.status === RemoteNetworkStatus.Active ? 'default' : 'secondary'}
                      className="text-[10px]"
                    >
                      {n.status.toLowerCase()}
                    </Badge>
                  </div>
                  <div className="flex items-center gap-4 text-xs text-muted-foreground">
                    <span className="flex items-center gap-1">
                      <Plug className="w-3 h-3" /> {n.connectors.length} connector
                      {n.connectors.length !== 1 ? 's' : ''}
                    </span>
                    <span className="font-mono text-[10px] uppercase tracking-wider">
                      {n.location.toLowerCase()}
                    </span>
                  </div>
                </Link>
              </motion.div>
            ))}
          </div>
        )}
      </motion.div>
    </div>
  )
}
