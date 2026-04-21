import { useState } from 'react'
import { useParams, Link } from 'react-router-dom'
import { useQuery, useMutation } from '@apollo/client/react'
import { motion } from 'framer-motion'
import {
  GetRemoteNetworkDocument,
  GetConnectorsDocument,
  GetShieldsDocument,
  RevokeConnectorDocument,
  DeleteConnectorDocument,
  RevokeShieldDocument,
  DeleteShieldDocument,
  ConnectorStatus,
  ShieldStatus,
  NetworkLocation,
} from '@/generated/graphql'
import type {
  RevokeConnectorMutationVariables,
  DeleteConnectorMutationVariables,
  RevokeShieldMutationVariables,
  DeleteShieldMutationVariables,
} from '@/generated/graphql'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'
import { InstallCommandModal } from '@/components/InstallCommandModal'
import {
  Plus,
  ChevronRight,
  ChevronDown,
  ShieldOff,
  Trash2,
  Shield,
  Plug,
  Home,
  Building2,
  Cloud,
  MapPin,
} from 'lucide-react'
import { cn } from '@/lib/utils'

function relativeTime(dateStr: string | null | undefined): string {
  if (!dateStr) return 'Never'
  const diff = Date.now() - new Date(dateStr).getTime()
  if (diff < 0) return 'Just now'
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

const locationConfig: Record<NetworkLocation, { label: string; icon: typeof Home; color: string }> = {
  [NetworkLocation.Home]:   { label: 'Home',   icon: Home,      color: 'text-blue-400 bg-blue-400/10 border-blue-400/20' },
  [NetworkLocation.Office]: { label: 'Office', icon: Building2, color: 'text-violet-400 bg-violet-400/10 border-violet-400/20' },
  [NetworkLocation.Aws]:    { label: 'AWS',    icon: Cloud,     color: 'text-amber-400 bg-amber-400/10 border-amber-400/20' },
  [NetworkLocation.Gcp]:    { label: 'GCP',    icon: Cloud,     color: 'text-sky-400 bg-sky-400/10 border-sky-400/20' },
  [NetworkLocation.Azure]:  { label: 'Azure',  icon: Cloud,     color: 'text-cyan-400 bg-cyan-400/10 border-cyan-400/20' },
  [NetworkLocation.Other]:  { label: 'Other',  icon: MapPin,    color: 'text-gray-400 bg-gray-400/10 border-gray-400/20' },
}


const connectorStatusConfig: Record<ConnectorStatus, { label: string; className: string }> = {
  [ConnectorStatus.Pending]:      { label: 'Pending',      className: 'text-gray-600 bg-gray-500/10 border-gray-500/20' },
  [ConnectorStatus.Active]:       { label: 'Active',       className: 'text-emerald-600 bg-emerald-500/10 border-emerald-500/20' },
  [ConnectorStatus.Disconnected]: { label: 'Disconnected', className: 'text-amber-600 bg-amber-500/10 border-amber-500/20' },
  [ConnectorStatus.Revoked]:      { label: 'Revoked',      className: 'text-red-600 bg-red-500/10 border-red-500/20' },
}

const shieldStatusConfig: Record<ShieldStatus, { label: string; className: string }> = {
  [ShieldStatus.Pending]:      { label: 'Pending',      className: 'text-gray-600 bg-gray-500/10 border-gray-500/20' },
  [ShieldStatus.Active]:       { label: 'Active',       className: 'text-emerald-600 bg-emerald-500/10 border-emerald-500/20' },
  [ShieldStatus.Disconnected]: { label: 'Disconnected', className: 'text-amber-600 bg-amber-500/10 border-amber-500/20' },
  [ShieldStatus.Revoked]:      { label: 'Revoked',      className: 'text-red-600 bg-red-500/10 border-red-500/20' },
}

export default function RemoteNetworkDetail() {
  const { id } = useParams<{ id: string }>()
  const [showConnectorInstall, setShowConnectorInstall] = useState(false)
  const [showShieldInstall, setShowShieldInstall] = useState(false)
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [initialised, setInitialised] = useState(false)

  const { data: networkData } = useQuery(GetRemoteNetworkDocument, {
    variables: { id: id! },
    skip: !id,
  })

  const { data: connectorsData, loading: connectorsLoading } = useQuery(GetConnectorsDocument, {
    variables: { remoteNetworkId: id! },
    skip: !id,
    pollInterval: 15000,
  })

  // Expand all connectors once on first load
  if (!initialised && connectorsData) {
    setExpanded(new Set(connectorsData.connectors.map((c) => c.id)))
    setInitialised(true)
  }

  const { data: shieldsData } = useQuery(GetShieldsDocument, {
    variables: { remoteNetworkId: id! },
    skip: !id,
    pollInterval: 15000,
  })

  const refetchConnectors = [{ query: GetConnectorsDocument, variables: { remoteNetworkId: id! } }]
  const refetchShields    = [{ query: GetShieldsDocument,    variables: { remoteNetworkId: id! } }]

  const [revokeConnector] = useMutation(RevokeConnectorDocument, { refetchQueries: refetchConnectors })
  const [deleteConnector] = useMutation(DeleteConnectorDocument, { refetchQueries: refetchConnectors })
  const [revokeShield]    = useMutation(RevokeShieldDocument,    { refetchQueries: refetchShields })
  const [deleteShield]    = useMutation(DeleteShieldDocument,    { refetchQueries: refetchShields })

  function toggleExpand(connectorId: string) {
    setExpanded((prev) => {
      const next = new Set(prev)
      next.has(connectorId) ? next.delete(connectorId) : next.add(connectorId)
      return next
    })
  }

  async function handleRevokeConnector(connectorId: string) {
    await revokeConnector({ variables: { id: connectorId } as RevokeConnectorMutationVariables })
  }

  async function handleDeleteConnector(connectorId: string) {
    await deleteConnector({ variables: { id: connectorId } as DeleteConnectorMutationVariables })
  }

  async function handleRevokeShield(shieldId: string, shieldName: string) {
    if (!window.confirm(`Revoke shield "${shieldName}"?`)) return
    await revokeShield({ variables: { id: shieldId } as RevokeShieldMutationVariables })
  }

  async function handleDeleteShield(shieldId: string, shieldName: string) {
    if (!window.confirm(`Delete shield "${shieldName}"? This cannot be undone.`)) return
    await deleteShield({ variables: { id: shieldId } as DeleteShieldMutationVariables })
  }

  const network    = networkData?.remoteNetwork
  const networkName = network?.name ?? 'Network'
  const connectors = connectorsData?.connectors ?? []
  const shields    = shieldsData?.shields ?? []
  const isLoading  = connectorsLoading && !connectorsData

  const loc = network ? locationConfig[network.location] : null
  const networkId = id!

  return (
    <div className="space-y-6">
      {/* Breadcrumb */}
      <motion.div
        className="flex items-center gap-2 text-sm"
        initial={{ opacity: 0, y: -6 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ duration: 0.3 }}
      >
        <Link to="/remote-networks" className="text-muted-foreground hover:text-foreground transition-colors">
          Remote Networks
        </Link>
        <ChevronRight className="w-3.5 h-3.5 text-muted-foreground/50" />
        <span className="text-foreground font-medium">{networkName}</span>
      </motion.div>

      {/* Header */}
      <motion.div
        className="flex items-center justify-between"
        initial={{ opacity: 0, y: -8 }}
        animate={{ opacity: 1, y: 0 }}
        transition={{ delay: 0.05, duration: 0.4 }}
      >
        <div className="space-y-1.5">
          <h1 className="text-2xl font-display font-bold tracking-tight">{networkName}</h1>
          <div className="flex items-center gap-2 flex-wrap">
            {loc && (
              <Badge variant="outline" className={cn('text-[10px] font-mono border', loc.color)}>
                <loc.icon className="w-3 h-3 mr-1" />
                {loc.label}
              </Badge>
            )}
            <span className="text-xs text-muted-foreground font-mono">
              {connectors.length} connector{connectors.length !== 1 ? 's' : ''}
              {' · '}
              {shields.length} shield{shields.length !== 1 ? 's' : ''}
            </span>
          </div>
        </div>

        <div className="flex items-center gap-2">
          <Button variant="outline" size="sm" className="gap-2" onClick={() => setShowShieldInstall(true)}>
            <Plus className="w-4 h-4" />
            Add Shield
          </Button>
          <Button size="sm" className="gap-2" onClick={() => setShowConnectorInstall(true)}>
            <Plus className="w-4 h-4" />
            Add Connector
          </Button>
        </div>
      </motion.div>

      {/* Install Modals */}
      {id && (
        <>
          <InstallCommandModal remoteNetworkId={networkId} open={showConnectorInstall} onClose={() => setShowConnectorInstall(false)} />
          <InstallCommandModal remoteNetworkId={networkId} variant="shield" open={showShieldInstall} onClose={() => setShowShieldInstall(false)} />
        </>
      )}

      {/* Loading skeletons */}
      {isLoading && (
        <Card className="bg-card/60">
          <CardContent className="p-0">
            {Array.from({ length: 3 }).map((_, i) => (
              <div key={i} className="px-5 py-4 border-b border-border/30 last:border-0 space-y-3">
                <div className="flex items-center gap-3">
                  <Skeleton className="h-4 w-4 rounded" />
                  <Skeleton className="h-4 w-40" />
                  <Skeleton className="h-5 w-20" />
                  <Skeleton className="h-4 w-24 ml-auto" />
                </div>
                <div className="ml-8 space-y-2">
                  <Skeleton className="h-4 w-56" />
                  <Skeleton className="h-4 w-48" />
                </div>
              </div>
            ))}
          </CardContent>
        </Card>
      )}

      {/* Empty state */}
      {!isLoading && connectors.length === 0 && (
        <div className="flex flex-col items-center justify-center py-20 text-center">
          <div className="rounded-full p-4 bg-primary/5 border border-primary/10 mb-4">
            <Plug className="w-8 h-8 text-primary/40" />
          </div>
          <h3 className="text-lg font-display font-semibold text-foreground/80">No connectors yet</h3>
          <p className="text-sm text-muted-foreground mt-1 max-w-sm">
            Deploy a connector to establish a secure tunnel to this network.
          </p>
          <Button onClick={() => setShowConnectorInstall(true)} className="gap-2 mt-4" variant="outline">
            <Plus className="w-4 h-4" />
            Add your first connector
          </Button>
        </div>
      )}

      {/* Tree */}
      {!isLoading && connectors.length > 0 && (
        <motion.div
          initial={{ opacity: 0, y: 16 }}
          animate={{ opacity: 1, y: 0 }}
          transition={{ delay: 0.1, duration: 0.4 }}
        >
          <Card className="bg-card/60 backdrop-blur-sm border-border/50 overflow-hidden">
            <CardContent className="p-0 overflow-x-auto">
              {/* Column headers */}
              <div className="grid grid-cols-[20px_1fr_110px_80px_160px_60px_120px] gap-3 px-4 py-2.5 border-b border-border/50 bg-muted/20 min-w-[700px]">
                <span />
                <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Name</span>
                <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Status</span>
                <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Last Seen</span>
                <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Hostname</span>
                <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60">Version</span>
                <span className="text-[10px] font-mono uppercase tracking-wider text-muted-foreground/60 text-right">Actions</span>
              </div>
              {connectors.map((connector, i) => {
                const cst = connectorStatusConfig[connector.status]
                const canRevokeC = connector.status === ConnectorStatus.Active || connector.status === ConnectorStatus.Disconnected
                const canDeleteC = connector.status === ConnectorStatus.Revoked || connector.status === ConnectorStatus.Pending
                const myShields = shields.filter((s) => s.connectorId === connector.id)
                const isExpanded = expanded.has(connector.id)
                const isLast = i === connectors.length - 1

                return (
                  <div key={connector.id} className={cn('border-b border-border/20', isLast && 'border-0')}>
                    {/* Connector row */}
                    <div
                      className="grid grid-cols-[20px_1fr_110px_80px_160px_60px_120px] gap-3 items-center px-4 py-3.5 hover:bg-muted/10 transition-colors min-w-[700px] cursor-pointer"
                      onClick={() => toggleExpand(connector.id)}
                    >
                      {/* Expand toggle */}
                      <span className="text-muted-foreground/50">
                        {isExpanded
                          ? <ChevronDown className="w-3.5 h-3.5" />
                          : <ChevronRight className="w-3.5 h-3.5" />
                        }
                      </span>

                      {/* Icon + name */}
                      <div className="flex items-center gap-2 min-w-0">
                        <Plug className="w-4 h-4 text-muted-foreground shrink-0" />
                        <Link
                          to={`/connectors/${connector.id}`}
                          className="text-sm font-medium truncate hover:text-primary transition-colors"
                          onClick={(e) => e.stopPropagation()}
                        >
                          {connector.name}
                        </Link>
                      </div>

                      {/* Status */}
                      <div>
                        <Badge variant="outline" className={cn('text-[10px] font-mono border', cst.className)}>
                          {cst.label}
                        </Badge>
                      </div>

                      {/* Last Seen */}
                      <span className="text-xs text-muted-foreground font-mono">
                        {relativeTime(connector.lastSeenAt)}
                      </span>

                      {/* Hostname */}
                      <span className="text-xs text-muted-foreground font-mono truncate">
                        {connector.hostname ?? '—'}
                      </span>

                      {/* Version */}
                      <span className="text-xs text-muted-foreground font-mono">
                        {connector.version ?? '—'}
                      </span>

                      {/* Actions */}
                      <div className="flex items-center justify-end gap-1.5" onClick={(e) => e.stopPropagation()}>
                        {canRevokeC && (
                          <Button
                            variant="outline"
                            size="sm"
                            className="h-7 px-2 text-[10px] text-amber-600 hover:text-amber-700 hover:bg-amber-500/10 border-amber-500/20"
                            onClick={() => handleRevokeConnector(connector.id)}
                          >
                            <ShieldOff className="w-3 h-3 mr-1" />
                            Revoke
                          </Button>
                        )}
                        {canDeleteC && (
                          <Button
                            variant="outline"
                            size="sm"
                            className="h-7 px-2 text-[10px] text-destructive hover:text-destructive hover:bg-destructive/10 border-destructive/20"
                            onClick={() => handleDeleteConnector(connector.id)}
                          >
                            <Trash2 className="w-3 h-3" />
                          </Button>
                        )}
                      </div>
                    </div>

                    {/* Shield rows (tree children) */}
                    {isExpanded && (
                      <div className="ml-8 border-l-2 border-border/30">
                        {myShields.length === 0 ? (
                          <div className="px-4 py-2.5 text-xs text-muted-foreground/50 font-mono italic">
                            no shields
                          </div>
                        ) : (
                          myShields.map((shield, si) => {
                            const sst = shieldStatusConfig[shield.status]
                            const canRevokeS = shield.status === ShieldStatus.Active || shield.status === ShieldStatus.Disconnected
                            const canDeleteS = shield.status === ShieldStatus.Pending || shield.status === ShieldStatus.Revoked
                            const isLastShield = si === myShields.length - 1

                            return (
                              <div
                                key={shield.id}
                                className={cn(
                                  'grid grid-cols-[20px_1fr_110px_80px_160px_60px_120px] gap-3 items-center pl-8 pr-4 py-2.5 hover:bg-muted/5 transition-colors min-w-[700px]',
                                  !isLastShield && 'border-b border-border/10',
                                )}
                              >
                                {/* Tree glyph */}
                                <span className="text-muted-foreground/30 text-xs font-mono select-none">
                                  {isLastShield ? '└' : '├'}
                                </span>

                                {/* Icon + name */}
                                <div className="flex items-center gap-2 min-w-0">
                                  <Shield className="w-3.5 h-3.5 text-muted-foreground/70 shrink-0" />
                                  <Link
                                    to={`/shields/${shield.id}`}
                                    className="text-sm font-medium truncate hover:text-primary transition-colors"
                                  >
                                    {shield.name}
                                  </Link>
                                </div>

                                {/* Status */}
                                <div>
                                  <Badge variant="outline" className={cn('text-[10px] font-mono border', sst.className)}>
                                    {sst.label}
                                  </Badge>
                                </div>

                                {/* Last Seen */}
                                <span className="text-xs text-muted-foreground font-mono">
                                  {relativeTime(shield.lastSeenAt)}
                                </span>

                                {/* Hostname */}
                                <span className="text-xs text-muted-foreground font-mono truncate">
                                  {shield.hostname ?? '—'}
                                </span>

                                {/* Version */}
                                <span className="text-xs text-muted-foreground font-mono">
                                  {shield.version ?? '—'}
                                </span>

                                {/* Actions */}
                                <div className="flex items-center justify-end gap-1.5">
                                  {canRevokeS && (
                                    <Button
                                      variant="outline"
                                      size="sm"
                                      className="h-7 px-2 text-[10px] text-amber-600 hover:text-amber-700 hover:bg-amber-500/10 border-amber-500/20"
                                      onClick={() => handleRevokeShield(shield.id, shield.name)}
                                    >
                                      <ShieldOff className="w-3 h-3 mr-1" />
                                      Revoke
                                    </Button>
                                  )}
                                  {canDeleteS && (
                                    <Button
                                      variant="outline"
                                      size="sm"
                                      className="h-7 px-2 text-[10px] text-destructive hover:text-destructive hover:bg-destructive/10 border-destructive/20"
                                      onClick={() => handleDeleteShield(shield.id, shield.name)}
                                    >
                                      <Trash2 className="w-3 h-3" />
                                    </Button>
                                  )}
                                </div>
                              </div>
                            )
                          })
                        )}
                      </div>
                    )}
                  </div>
                )
              })}
            </CardContent>
          </Card>
        </motion.div>
      )}
    </div>
  )
}
