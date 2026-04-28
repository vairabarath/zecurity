import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { ChevronDown, ChevronRight, Clock3, Radar, Search } from 'lucide-react'
import {
  ConnectorStatus,
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

export default function ResourceDiscovery() {
  const [view, setView] = useState<DiscoveryView>('connectors')
  const [query, setQuery] = useState('')
  const [expanded, setExpanded] = useState<Set<string>>(new Set())
  const [scanTarget, setScanTarget] = useState<{ networkId: string; connectorId: string } | null>(null)

  const { data, loading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const networks = data?.remoteNetworks ?? []
  const connectors: DiscoveryConnector[] = networks.flatMap((network) =>
    network.connectors.map((connector) => ({
      ...connector,
      networkId: network.id,
      networkName: network.name,
    })),
  )
  const shields: DiscoveryShield[] = networks.flatMap((network) =>
    network.shields.map((shield) => ({
      ...shield,
      networkId: network.id,
      networkName: network.name,
    })),
  )

  const connectorMap = useMemo(
    () => new Map(connectors.map((connector) => [connector.id, connector])),
    [connectors],
  )

  const needle = query.trim().toLowerCase()
  const filteredConnectors = useMemo(() => {
    return connectors.filter((connector) => {
      if (!needle) return true
      return (
        connector.name.toLowerCase().includes(needle) ||
        connector.networkName.toLowerCase().includes(needle) ||
        (connector.hostname ?? '').toLowerCase().includes(needle)
      )
    })
  }, [connectors, needle])

  const filteredShields = useMemo(() => {
    return shields.filter((shield) => {
      if (!needle) return true
      return (
        shield.name.toLowerCase().includes(needle) ||
        shield.networkName.toLowerCase().includes(needle) ||
        (shield.hostname ?? '').toLowerCase().includes(needle)
      )
    })
  }, [needle, shields])

  function toggleExpanded(shieldId: string) {
    setExpanded((current) => {
      const next = new Set(current)
      if (next.has(shieldId)) next.delete(shieldId)
      else next.add(shieldId)
      return next
    })
  }

  const activeConnectors = connectors.filter((connector) => connector.status === ConnectorStatus.Active).length
  const activeShields = shields.filter((shield) => shield.status === ShieldStatus.Active).length

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Resource Discovery</h2>
          <p className="page-subtitle">Run connector-side scans and inspect services discovered by shields.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="metric-chip">
            <strong>{connectors.length}</strong> connectors
          </span>
          <span className="metric-chip">
            <strong>{shields.length}</strong> shields
          </span>
        </div>
      </div>

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
              onChange={(event) => setQuery(event.target.value)}
              placeholder={view === 'connectors' ? 'Search connectors, networks, hostnames...' : 'Search shields, networks, hostnames...'}
            />
          </label>
        </div>

        <div className="mt-4 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
          {view === 'connectors' ? (
            <>
              <span>Connector-side TCP discovery workspace.</span>
              <span>•</span>
              <span>
                <strong className="text-foreground">{activeConnectors}</strong> active
              </span>
            </>
          ) : (
            <>
              <span>Shield-discovered services across enrolled hosts.</span>
              <span>•</span>
              <span>
                <strong className="text-foreground">{activeShields}</strong> active
              </span>
            </>
          )}
        </div>
      </div>

      {view === 'connectors' ? (
        <div className="table-shell">
          <div className="table-scroll">
            <div className="table-head grid min-w-[1240px] items-center grid-cols-[1.35fr_150px_1fr_1fr_110px_140px_170px_100px] gap-4 px-5 py-4">
              {['Name', 'Status', 'Network', 'Hostname', 'Version', 'Last Seen', 'Discovery', 'Actions'].map((label, index) => (
                <div key={label + index} className={`table-head-label ${index >= 6 ? 'text-right' : ''}`}>{label}</div>
              ))}
            </div>

            {loading && !data ? (
              <div className="min-w-[1240px] p-5 space-y-3">
                {Array.from({ length: 5 }).map((_, index) => (
                  <Skeleton key={index} className="h-16 rounded-2xl bg-secondary" />
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
                        {connector.status === ConnectorStatus.Pending ? (
                          <div className="truncate font-mono text-[10.5px] font-medium tracking-tight text-muted-foreground/70">not installed</div>
                        ) : null}
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
        <div className="table-shell">
          <div className="table-scroll">
            <div className="table-head grid min-w-[1080px] grid-cols-[36px_1.15fr_130px_170px_160px_150px_130px_100px] gap-4 px-5 py-4">
              {['', 'Name', 'Status', 'Network', 'Interface', 'Hostname', 'Last Seen', 'Actions'].map((label, index) => (
                <div key={label + index} className={`table-head-label ${index === 7 ? 'text-right' : ''}`}>{label}</div>
              ))}
            </div>

            {loading && !data ? (
              <div className="min-w-[1080px] p-5 space-y-3">
                {Array.from({ length: 5 }).map((_, index) => (
                  <Skeleton key={index} className="h-14 rounded-2xl bg-secondary" />
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
              <div className="min-w-[1080px]">
                {filteredShields.map((shield) => {
                  const isExpanded = expanded.has(shield.id)
                  return (
                    <div key={shield.id}>
                      <div className="admin-table-row grid grid-cols-[36px_1.15fr_130px_170px_160px_150px_130px_100px] gap-4 px-5 py-4">
                        <button
                          onClick={() => toggleExpanded(shield.id)}
                          className="flex h-7 w-7 items-center justify-center rounded-lg border border-border text-muted-foreground transition hover:bg-secondary hover:text-foreground"
                          aria-label={isExpanded ? 'Hide discovered services' : 'Show discovered services'}
                        >
                          {isExpanded ? <ChevronDown className="h-4 w-4" /> : <ChevronRight className="h-4 w-4" />}
                        </button>
                        <div className="flex min-w-0 items-center gap-3">
                          <EntityIcon type="shield" />
                          <div className="min-w-0">
                            <div className="truncate text-[15px] font-bold leading-tight">{shield.name}</div>
                            <div className="truncate font-mono text-[11px] font-medium text-muted-foreground">{shield.hostname ?? 'pending'}</div>
                          </div>
                        </div>
                        <div>
                          <StatusPill label={shield.status.toLowerCase()} tone={shieldTone(shield.status)} />
                        </div>
                        <div className="truncate text-sm font-semibold text-primary">
                          <Link to={`/remote-networks/${shield.networkId}`} className="transition hover:opacity-80">
                            {shield.networkName}
                          </Link>
                        </div>
                        <div className="font-mono text-[13px] text-muted-foreground">{shield.interfaceAddr ?? '—'}</div>
                        <div className="truncate font-mono text-[13px] text-muted-foreground">{shield.hostname ?? '—'}</div>
                        <div className="font-mono text-[13px] text-muted-foreground">
                          <span className="inline-flex items-center gap-1.5">
                            <Clock3 className="h-3.5 w-3.5" />
                            {relativeTime(shield.lastSeenAt)}
                          </span>
                        </div>
                        <div className="text-right">
                          <Link to={`/shields/${shield.id}`} className="inline-flex items-center gap-1 text-[13px] font-bold text-primary transition hover:opacity-80">
                            Manage <span className="transition-transform group-hover:translate-x-0.5">→</span>
                          </Link>
                        </div>
                      </div>
                      {isExpanded ? (
                        <div className="border-b border-border bg-background/40 px-5 py-4">
                          <DiscoveredServicesPanel shieldId={shield.id} />
                        </div>
                      ) : null}
                    </div>
                  )
                })}
              </div>
            )}
          </div>
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
