import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { motion } from 'framer-motion'
import {
  GetRemoteNetworksDocument,
  ConnectorStatus,
  type GetRemoteNetworksQuery,
} from '@/generated/graphql'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { Button } from '@/components/ui/button'
import { InstallCommandModal } from '@/components/InstallCommandModal'
import { cn } from '@/lib/utils'
import {
  Globe,
  Zap,
  Server,
  Clock,
  ArrowRight,
  Inbox,
  Plus,
  Network,
  CircleDot,
  CircleDotDashed,
  Ban,
} from 'lucide-react'

type NetworkConnector = GetRemoteNetworksQuery['remoteNetworks'][number]['connectors'][number] & {
  networkId: string
  networkName: string
}

const statusConfig: Record<ConnectorStatus, { label: string; className: string; icon: React.ReactNode }> = {
  [ConnectorStatus.Active]: {
    label: 'Active',
    className: 'text-emerald-600 bg-emerald-500/10 border-emerald-500/20',
    icon: <CircleDot className="h-3 w-3 fill-emerald-500 text-emerald-500" />,
  },
  [ConnectorStatus.Disconnected]: {
    label: 'Disconnected',
    className: 'text-amber-600 bg-amber-500/10 border-amber-500/20',
    icon: <CircleDotDashed className="h-3 w-3 fill-amber-500 text-amber-500" />,
  },
  [ConnectorStatus.Pending]: {
    label: 'Pending',
    className: 'text-gray-600 bg-gray-500/10 border-gray-500/20',
    icon: <CircleDotDashed className="h-3 w-3 fill-gray-400 text-gray-400" />,
  },
  [ConnectorStatus.Revoked]: {
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

// Network picker shown when "Add Connector" is clicked and multiple networks exist
interface NetworkPickerProps {
  networks: GetRemoteNetworksQuery['remoteNetworks']
  onSelect: (id: string) => void
  onClose: () => void
}

function NetworkPicker({ networks, onSelect, onClose }: NetworkPickerProps) {
  return (
    <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/40 backdrop-blur-sm">
      <motion.div
        initial={{ opacity: 0, scale: 0.96 }}
        animate={{ opacity: 1, scale: 1 }}
        className="bg-white rounded-2xl border border-border shadow-xl p-6 w-full max-w-sm mx-4"
      >
        <h2 className="font-semibold text-foreground mb-1">Select a Network</h2>
        <p className="text-xs text-muted-foreground mb-4">
          Choose which remote network to add the connector to.
        </p>
        <div className="space-y-2">
          {networks.map((n) => (
            <button
              key={n.id}
              onClick={() => onSelect(n.id)}
              className="w-full flex items-center gap-3 rounded-xl border border-border bg-muted/30 px-4 py-3 text-left hover:border-primary/30 hover:bg-primary/5 transition-all"
            >
              <div className="flex items-center justify-center h-8 w-8 rounded-lg bg-primary/10 border border-primary/20 shrink-0">
                <Network className="h-4 w-4 text-primary" />
              </div>
              <div className="flex-1 min-w-0">
                <div className="text-sm font-medium truncate">{n.name}</div>
                <div className="text-xs text-muted-foreground font-mono">
                  {n.connectors.length} connector{n.connectors.length !== 1 ? 's' : ''}
                </div>
              </div>
              <ArrowRight className="h-4 w-4 text-muted-foreground shrink-0" />
            </button>
          ))}
        </div>
        <button
          onClick={onClose}
          className="mt-4 w-full text-xs text-muted-foreground hover:text-foreground transition-colors"
        >
          Cancel
        </button>
      </motion.div>
    </div>
  )
}

export default function AllConnectors() {
  const [installNetworkId, setInstallNetworkId] = useState<string | null>(null)
  const [showNetworkPicker, setShowNetworkPicker] = useState(false)

  const { data, loading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const networks = data?.remoteNetworks ?? []
  const allConnectors: NetworkConnector[] = networks.flatMap((n) =>
    n.connectors.map((c) => ({ ...c, networkId: n.id, networkName: n.name })),
  )
  const activeCount = allConnectors.filter((c) => c.status === ConnectorStatus.Active).length

  function handleAddConnector() {
    if (networks.length === 0) return
    if (networks.length === 1) {
      setInstallNetworkId(networks[0].id)
    } else {
      setShowNetworkPicker(true)
    }
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
            <Globe className="h-5 w-5 text-primary" />
          </div>
          <div>
            <h1 className="font-display text-xl font-bold tracking-wide">Connectors</h1>
            <p className="text-xs text-muted-foreground mt-0.5">
              Network gateways providing access to remote networks
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
                  {allConnectors.length} total
                </span>
              </div>
            </>
          )}
          <Button
            onClick={handleAddConnector}
            disabled={networks.length === 0}
            className="gap-2 text-[12px]"
            size="sm"
          >
            <Plus className="h-4 w-4" />
            Add Connector
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
        {/* Table Header */}
        <div className="grid grid-cols-[1fr_110px_140px_140px_90px_110px_80px] gap-4 px-5 py-3 border-b border-border/50 bg-muted/20 min-w-[800px]">
          {['Name', 'Status', 'Network', 'Hostname', 'Version', 'Last Seen', ''].map((col) => (
            <span
              key={col}
              className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60 last:text-right"
            >
              {col}
            </span>
          ))}
        </div>

        <div className="overflow-x-auto">
          {loading && !data ? (
            <div className="min-w-[800px]">
              {Array.from({ length: 5 }).map((_, i) => (
                <div
                  key={i}
                  className="grid grid-cols-[1fr_110px_140px_140px_90px_110px_80px] gap-4 items-center px-5 py-3.5 border-b border-border/20 last:border-0"
                >
                  <Skeleton className="h-4 w-32" />
                  <Skeleton className="h-5 w-20" />
                  <Skeleton className="h-4 w-28" />
                  <Skeleton className="h-4 w-24" />
                  <Skeleton className="h-4 w-12" />
                  <Skeleton className="h-4 w-16" />
                  <Skeleton className="h-6 w-16 ml-auto" />
                </div>
              ))}
            </div>
          ) : allConnectors.length === 0 ? (
            <div className="flex flex-col items-center justify-center py-16 text-center">
              <div className="rounded-full p-3 bg-primary/5 border border-primary/10 mb-3">
                <Inbox className="w-6 h-6 text-primary/40" />
              </div>
              <p className="text-sm text-foreground/70">No connectors yet</p>
              <p className="text-xs text-muted-foreground mt-0.5 max-w-xs">
                {networks.length === 0
                  ? 'Create a remote network first, then deploy a connector to it.'
                  : 'Add a connector to one of your remote networks to get started.'}
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
            <div className="min-w-[800px]">
              {allConnectors.map((c, i) => {
                const st = statusConfig[c.status]
                return (
                  <motion.div
                    key={c.id}
                    initial={{ opacity: 0 }}
                    animate={{ opacity: 1 }}
                    transition={{ delay: i * 0.03 }}
                    className="grid grid-cols-[1fr_110px_140px_140px_90px_110px_80px] gap-4 items-center px-5 py-3.5 border-b border-border/20 last:border-0 hover:bg-muted/20 transition-colors"
                  >
                    {/* Name */}
                    <div className="flex items-center gap-2.5 min-w-0">
                      <div className="flex items-center justify-center h-8 w-8 rounded-lg bg-primary/10 border border-primary/20 shrink-0">
                        <Server className="h-4 w-4 text-primary" />
                      </div>
                      <span className="text-sm font-medium truncate">{c.name}</span>
                    </div>

                    {/* Status */}
                    <div>
                      <Badge
                        variant="outline"
                        className={cn('text-[10px] font-mono border gap-1', st.className)}
                      >
                        {st.icon}
                        {st.label}
                      </Badge>
                    </div>

                    {/* Network */}
                    <Link
                      to={`/remote-networks/${c.networkId}/connectors`}
                      className="flex items-center gap-1 text-xs text-primary hover:underline truncate font-mono"
                    >
                      <Network className="h-3 w-3 shrink-0" />
                      <span className="truncate">{c.networkName}</span>
                    </Link>

                    {/* Hostname */}
                    <span className="text-xs text-muted-foreground font-mono truncate">
                      {c.hostname ?? '—'}
                    </span>

                    {/* Version */}
                    <span className="text-xs text-muted-foreground font-mono">
                      {c.version ?? '—'}
                    </span>

                    {/* Last Seen */}
                    <div className="flex items-center gap-1 text-xs text-muted-foreground">
                      <Clock className="w-3 h-3 shrink-0" />
                      {relativeTime(c.lastSeenAt)}
                    </div>

                    {/* Manage */}
                    <div className="flex justify-end">
                      <Link
                        to={`/connectors/${c.id}`}
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

      {/* Network picker overlay */}
      {showNetworkPicker && (
        <NetworkPicker
          networks={networks}
          onSelect={(id) => {
            setShowNetworkPicker(false)
            setInstallNetworkId(id)
          }}
          onClose={() => setShowNetworkPicker(false)}
        />
      )}

      {/* Install modal */}
      {installNetworkId && (
        <InstallCommandModal
          remoteNetworkId={installNetworkId}
          open={true}
          onClose={() => setInstallNetworkId(null)}
        />
      )}
    </div>
  )
}
