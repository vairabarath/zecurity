import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { Clock3, Radar, Search, ShieldCheck } from 'lucide-react'
import {
  ConnectorStatus,
  GetAllResourcesDocument,
  GetRemoteNetworksDocument,
  ShieldStatus,
  type GetRemoteNetworksQuery,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { DiscoveredServicesPanel } from '@/components/DiscoveredServicesPanel'
import { ScanModal } from '@/components/ScanModal'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, EntityIcon, StatusPill, relativeTime } from '@/lib/console'

type DiscoveryView = 'connectors' | 'shields'

type DiscoveryConnector = GetRemoteNetworksQuery['remoteNetworks'][number]['connectors'][number] & {
  networkId: string
  networkName: string
}

type DiscoveryShield = GetRemoteNetworksQuery['remoteNetworks'][number]['shields'][number] & {
  networkId: string
  networkName: string
}

function connectorTone(status: ConnectorStatus): 'ok' | 'warn' | 'danger' | 'muted' {
  if (status === ConnectorStatus.Active) return 'ok'
  if (status === ConnectorStatus.Disconnected) return 'warn'
  if (status === ConnectorStatus.Revoked) return 'danger'
  return 'warn'
}

function shieldTone(status: ShieldStatus): 'ok' | 'warn' | 'danger' | 'muted' {
  if (status === ShieldStatus.Active) return 'ok'
  if (status === ShieldStatus.Disconnected) return 'warn'
  if (status === ShieldStatus.Revoked) return 'danger'
  return 'muted'
}

function statusDot(status: ShieldStatus): string {
  if (status === ShieldStatus.Active) return 'bg-emerald-500'
  if (status === ShieldStatus.Disconnected) return 'bg-amber-500'
  return 'bg-muted-foreground/50'
}

function ShieldListItem({
  shield,
  isActive,
  managedCount,
  onClick,
}: {
  shield: DiscoveryShield
  isActive: boolean
  managedCount: number
  onClick: () => void
}) {
  return (
    <button
      onClick={onClick}
      className={`flex w-full items-center gap-3 rounded-xl px-2 py-2.5 text-left transition ${
        isActive ? 'bg-primary/10' : 'hover:bg-secondary'
      }`}
    >
      <div
        className={`relative flex h-9 w-9 shrink-0 items-center justify-center rounded-xl border ${
          isActive
            ? 'border-primary/30 bg-primary/10 text-primary'
            : 'border-border bg-secondary text-muted-foreground'
        }`}
      >
        <ShieldCheck className="h-4 w-4" />
        <span
          className={`absolute -right-0.5 -top-0.5 h-2.5 w-2.5 rounded-full border-2 border-background ${statusDot(shield.status)}`}
        />
      </div>
      <div className="min-w-0 flex-1">
        <div
          className={`truncate text-[13px] font-bold leading-tight ${
            isActive ? 'text-primary' : 'text-foreground'
          }`}
        >
          {shield.name}
        </div>
        <div className="truncate font-mono text-[11px] text-muted-foreground">
          {shield.hostname ?? 'pending'} · {shield.networkName}
        </div>
      </div>
      {managedCount > 0 && (
        <span className="shrink-0 rounded-full bg-emerald-500/10 px-2 py-0.5 text-[10px] font-bold text-emerald-500">
          {managedCount}
        </span>
      )}
    </button>
  )
}

export default function ResourceDiscovery() {
  const [view, setView] = useState<DiscoveryView>('connectors')
  const [query, setQuery] = useState('')
  const [selectedShieldId, setSelectedShieldId] = useState<string | null>(null)
  const [scanTarget, setScanTarget] = useState<{ networkId: string; connectorId: string } | null>(null)

  const { data, loading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })
  const { data: resourcesData } = useQuery(GetAllResourcesDocument, {
    fetchPolicy: 'cache-and-network',
  })

  const networks = data?.remoteNetworks ?? []
  const connectors: DiscoveryConnector[] = networks.flatMap((network) =>
    network.connectors.map((connector) => ({ ...connector, networkId: network.id, networkName: network.name })),
  )
  const shields: DiscoveryShield[] = networks.flatMap((network) =>
    network.shields.map((shield) => ({ ...shield, networkId: network.id, networkName: network.name })),
  )

  const connectorMap = useMemo(
    () => new Map(connectors.map((c) => [c.id, c])),
    [connectors],
  )

  const needle = query.trim().toLowerCase()

  const filteredConnectors = useMemo(() =>
    connectors.filter((c) => {
      if (!needle) return true
      return (
        c.name.toLowerCase().includes(needle) ||
        c.networkName.toLowerCase().includes(needle) ||
        (c.hostname ?? '').toLowerCase().includes(needle)
      )
    }),
  [connectors, needle])

  const filteredShields = useMemo(() =>
    shields.filter((s) => {
      if (!needle) return true
      return (
        s.name.toLowerCase().includes(needle) ||
        s.networkName.toLowerCase().includes(needle) ||
        (s.hostname ?? '').toLowerCase().includes(needle)
      )
    }),
  [shields, needle])

  const activeShieldsList = filteredShields.filter((s) => s.status === ShieldStatus.Active)
  const otherShieldsList  = filteredShields.filter((s) => s.status !== ShieldStatus.Active)

  const managedByShield = useMemo(() => {
    const m = new Map<string, number>()
    for (const r of resourcesData?.allResources ?? []) {
      if (!r.shield) continue
      m.set(r.shield.id, (m.get(r.shield.id) ?? 0) + 1)
    }
    return m
  }, [resourcesData])

  const effectiveShieldId = useMemo(() => {
    if (selectedShieldId && filteredShields.some((s) => s.id === selectedShieldId)) return selectedShieldId
    return filteredShields[0]?.id ?? null
  }, [selectedShieldId, filteredShields])

  const selectedShield = filteredShields.find((s) => s.id === effectiveShieldId) ?? null

  const activeConnectorsCount = connectors.filter((c) => c.status === ConnectorStatus.Active).length
  const activeShieldsCount    = shields.filter((s) => s.status === ShieldStatus.Active).length

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Resource Discovery</h2>
          <p className="page-subtitle">Run connector-side scans and inspect services discovered by shields.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="metric-chip">
            <strong>{activeConnectorsCount}</strong>/{connectors.length} connectors active
          </span>
          <span className="metric-chip">
            <strong>{activeShieldsCount}</strong>/{shields.length} shields active
          </span>
        </div>
      </div>

      {/* View toggle + search */}
      <div className="surface-card p-4">
        <div className="flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
          <div className="inline-flex rounded-2xl border border-border bg-secondary p-1">
            <button
              onClick={() => setView('connectors')}
              className={`rounded-xl px-4 py-2 text-sm font-semibold transition ${
                view === 'connectors'
                  ? 'bg-primary text-primary-foreground'
                  : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              Connectors
            </button>
            <button
              onClick={() => setView('shields')}
              className={`rounded-xl px-4 py-2 text-sm font-semibold transition ${
                view === 'shields'
                  ? 'bg-primary text-primary-foreground'
                  : 'text-muted-foreground hover:text-foreground'
              }`}
            >
              Shields
            </button>
          </div>
          <label className="toolbar-input max-w-[340px]">
            <Search className="h-4 w-4 shrink-0" />
            <input
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              placeholder={
                view === 'connectors'
                  ? 'Search connectors, networks, hostnames...'
                  : 'Search shields, networks, hostnames...'
              }
            />
          </label>
        </div>
        <div className="mt-4 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          {view === 'connectors' ? (
            <>
              <span>Connector-side TCP discovery workspace.</span>
              <span>•</span>
              <span><strong className="text-foreground">{activeConnectorsCount}</strong> active</span>
            </>
          ) : (
            <>
              <span>Shield-discovered services across enrolled hosts.</span>
              <span>•</span>
              <span><strong className="text-foreground">{activeShieldsCount}</strong> active</span>
            </>
          )}
        </div>
      </div>

      {/* ── Connectors view ────────────────────────────────────────────────── */}
      {view === 'connectors' ? (
        <div className="table-shell">
          <div className="table-scroll">
            <div className="table-head grid min-w-[1240px] items-center grid-cols-[1.35fr_150px_1fr_1fr_110px_140px_170px_100px] gap-4 px-5 py-4">
              {['Name', 'Status', 'Network', 'Hostname', 'Version', 'Last Seen', 'Discovery', 'Actions'].map((label, i) => (
                <div key={label + i} className={`table-head-label ${i >= 6 ? 'text-right' : ''}`}>{label}</div>
              ))}
            </div>
            {loading && !data ? (
              <div className="min-w-[1240px] space-y-3 p-5">
                {Array.from({ length: 5 }).map((_, i) => (
                  <Skeleton key={i} className="h-16 rounded-2xl bg-secondary" />
                ))}
              </div>
            ) : filteredConnectors.length === 0 ? (
              <EmptyState
                title="No connectors available for discovery"
                description={
                  networks.length === 0
                    ? 'Create a remote network and enroll a connector first.'
                    : 'Try another search or enroll a connector into a remote network.'
                }
                action={
                  networks.length === 0 ? (
                    <Link to="/remote-networks" className="text-sm font-semibold text-primary">Open remote networks</Link>
                  ) : undefined
                }
              />
            ) : (
              <div className="min-w-[1240px]">
                {filteredConnectors.map((connector) => (
                  <div key={connector.id} className="admin-table-row group grid items-center grid-cols-[1.35fr_150px_1fr_1fr_110px_140px_170px_100px] gap-4 px-5 py-4">
                    <div className="flex min-w-0 items-center gap-3">
                      <EntityIcon type="connector" />
                      <div className="min-w-0">
                        <div className="truncate text-[15px] font-bold leading-tight">{connector.name}</div>
                        {connector.status === ConnectorStatus.Pending && (
                          <div className="truncate font-mono text-[10.5px] font-medium tracking-tight text-muted-foreground/70">not installed</div>
                        )}
                      </div>
                    </div>
                    <div>
                      <StatusPill
                        label={connector.status === ConnectorStatus.Disconnected ? 'degraded' : connector.status.toLowerCase()}
                        tone={connectorTone(connector.status)}
                      />
                    </div>
                    <div className="truncate text-sm font-semibold text-primary">
                      <Link to={`/remote-networks/${connector.networkId}`} className="inline-flex items-center gap-1.5 transition hover:opacity-80">
                        <span className="h-1 w-1 rounded-full bg-primary/40" />
                        {connector.networkName}
                      </Link>
                    </div>
                    <div className="truncate font-mono text-[12.5px] text-muted-foreground/80">{connector.hostname ?? '—'}</div>
                    <div className="font-mono text-[12.5px] text-muted-foreground/80">{connector.version ?? '—'}</div>
                    <div className="font-mono text-[12.5px] text-muted-foreground/80">
                      <span className="inline-flex items-center gap-1.5">
                        <Clock3 className="h-3.5 w-3.5 opacity-60" />
                        {relativeTime(connector.lastSeenAt)}
                      </span>
                    </div>
                    <div className="text-right">
                      <Button
                        type="button"
                        size="sm"
                        disabled={connector.status !== ConnectorStatus.Active}
                        onClick={() => setScanTarget({ networkId: connector.networkId, connectorId: connector.id })}
                        className="gap-2"
                      >
                        <Radar className="h-4 w-4" />
                        Scan Network
                      </Button>
                    </div>
                    <div className="text-right">
                      <Link to={`/connectors/${connector.id}`} className="inline-flex items-center gap-1.5 text-[12.5px] font-bold text-primary transition hover:opacity-80">
                        Manage <span className="text-[10px] transition-transform group-hover:translate-x-0.5">→</span>
                      </Link>
                    </div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </div>

      ) : (
        /* ── Shields master-detail view ──────────────────────────────────── */
        <div className="surface-card overflow-hidden">
          {loading && !data ? (
            <div className="space-y-3 p-5">
              {Array.from({ length: 4 }).map((_, i) => (
                <Skeleton key={i} className="h-14 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : filteredShields.length === 0 ? (
            <EmptyState
              title="No shields available for discovery"
              description={
                networks.length === 0
                  ? 'Create a remote network and enroll a shield first.'
                  : 'Try another search or enroll a shield into a remote network.'
              }
              action={
                networks.length === 0 ? (
                  <Link to="/remote-networks" className="text-sm font-semibold text-primary">Open remote networks</Link>
                ) : undefined
              }
            />
          ) : (
            <div className="flex" style={{ minHeight: '560px' }}>

              {/* Shield list sidebar */}
              <div className="flex w-[260px] shrink-0 flex-col border-r border-border">
                <div className="flex-1 overflow-y-auto px-2 py-2">
                  {activeShieldsList.length > 0 && (
                    <>
                      <div className="px-2 pb-1 pt-2 text-[10px] font-bold uppercase tracking-[0.08em] text-muted-foreground">
                        Active · {activeShieldsList.length}
                      </div>
                      {activeShieldsList.map((s) => (
                        <ShieldListItem
                          key={s.id}
                          shield={s}
                          isActive={s.id === effectiveShieldId}
                          managedCount={managedByShield.get(s.id) ?? 0}
                          onClick={() => setSelectedShieldId(s.id)}
                        />
                      ))}
                    </>
                  )}
                  {otherShieldsList.length > 0 && (
                    <>
                      <div className="px-2 pb-1 pt-3 text-[10px] font-bold uppercase tracking-[0.08em] text-muted-foreground">
                        Other · {otherShieldsList.length}
                      </div>
                      {otherShieldsList.map((s) => (
                        <ShieldListItem
                          key={s.id}
                          shield={s}
                          isActive={s.id === effectiveShieldId}
                          managedCount={managedByShield.get(s.id) ?? 0}
                          onClick={() => setSelectedShieldId(s.id)}
                        />
                      ))}
                    </>
                  )}
                </div>
              </div>

              {/* Detail pane */}
              <div className="flex min-w-0 flex-1 flex-col">
                {selectedShield ? (
                  <>
                    {/* Shield detail header */}
                    <div className="flex flex-wrap items-start gap-4 border-b border-border bg-secondary/20 px-5 py-4">
                      <div className="relative flex h-12 w-12 shrink-0 items-center justify-center rounded-2xl border border-primary/20 bg-primary/10 text-primary">
                        <ShieldCheck className="h-5 w-5" />
                      </div>
                      <div className="min-w-0 flex-1">
                        <div className="flex flex-wrap items-center gap-2">
                          <h3 className="text-[17px] font-bold leading-tight">{selectedShield.name}</h3>
                          <StatusPill
                            label={selectedShield.status.toLowerCase()}
                            tone={shieldTone(selectedShield.status)}
                          />
                        </div>
                        <div className="mt-2.5 flex flex-wrap gap-x-6 gap-y-2">
                          <div className="flex flex-col gap-0.5">
                            <span className="text-[10px] font-bold uppercase tracking-[0.07em] text-muted-foreground/60">Host</span>
                            <span className="text-xs font-semibold text-foreground/80">{selectedShield.hostname ?? '—'}</span>
                          </div>
                          <div className="flex flex-col gap-0.5">
                            <span className="text-[10px] font-bold uppercase tracking-[0.07em] text-muted-foreground/60">Network</span>
                            <Link
                              to={`/remote-networks/${selectedShield.networkId}`}
                              className="text-xs font-semibold text-primary transition hover:opacity-80"
                            >
                              {selectedShield.networkName}
                            </Link>
                          </div>
                          <div className="flex flex-col gap-0.5">
                            <span className="text-[10px] font-bold uppercase tracking-[0.07em] text-muted-foreground/60">Interface</span>
                            <span className="font-mono text-xs font-semibold text-primary/80">{selectedShield.interfaceAddr ?? '—'}</span>
                          </div>
                          <div className="flex flex-col gap-0.5">
                            <span className="text-[10px] font-bold uppercase tracking-[0.07em] text-muted-foreground/60">Last Seen</span>
                            <span className="flex items-center gap-1 text-xs font-semibold text-foreground/80">
                              <Clock3 className="h-3 w-3 opacity-50" />
                              {relativeTime(selectedShield.lastSeenAt)}
                            </span>
                          </div>
                        </div>
                      </div>
                      <Link
                        to={`/shields/${selectedShield.id}`}
                        className="inline-flex items-center gap-1.5 rounded-xl border border-border bg-background px-4 py-2 text-sm font-bold text-primary transition hover:bg-secondary"
                      >
                        Manage <span>→</span>
                      </Link>
                    </div>

                    <DiscoveredServicesPanel shieldId={selectedShield.id} />
                  </>
                ) : null}
              </div>

            </div>
          )}
        </div>
      )}

      {scanTarget ? (
        <ScanModal
          connectorId={scanTarget.connectorId}
          remoteNetworkId={scanTarget.networkId}
          connectorName={connectorMap.get(scanTarget.connectorId)?.name ?? 'connector'}
          connectorLanAddr={connectorMap.get(scanTarget.connectorId)?.lanAddr ?? undefined}
          onClose={() => setScanTarget(null)}
        />
      ) : null}
    </div>
  )
}
