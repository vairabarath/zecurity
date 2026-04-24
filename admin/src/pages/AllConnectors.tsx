import { useMemo, useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { Clock3, Plus, Search } from 'lucide-react'
import {
  ConnectorStatus,
  GetRemoteNetworksDocument,
  type GetRemoteNetworksQuery,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { InstallCommandModal } from '@/components/InstallCommandModal'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, EntityIcon, StatusPill, relativeTime } from '@/lib/console'

type NetworkConnector = GetRemoteNetworksQuery['remoteNetworks'][number]['connectors'][number] & {
  networkId: string
  networkName: string
}

type Filter = 'all' | 'active' | 'pending' | 'degraded'

function connectorTone(status: ConnectorStatus): 'ok' | 'warn' | 'danger' | 'muted' {
  if (status === ConnectorStatus.Active) return 'ok'
  if (status === ConnectorStatus.Disconnected) return 'warn'
  if (status === ConnectorStatus.Revoked) return 'danger'
  return 'warn'
}

export default function AllConnectors() {
  const [showAdd, setShowAdd] = useState(false)
  const [query, setQuery] = useState('')
  const [filter, setFilter] = useState<Filter>('all')

  const { data, loading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const networks = data?.remoteNetworks ?? []
  const allConnectors: NetworkConnector[] = networks.flatMap((network) =>
    (network.connectors ?? []).map((connector) => ({
      ...connector,
      networkId: network.id,
      networkName: network.name,
    })),
  )

  const filteredConnectors = useMemo(() => {
    return allConnectors.filter((connector) => {
      const matchesFilter =
        filter === 'all'
          ? true
          : filter === 'degraded'
            ? connector.status === ConnectorStatus.Disconnected || connector.status === ConnectorStatus.Revoked
            : filter === 'active'
              ? connector.status === ConnectorStatus.Active
              : connector.status === ConnectorStatus.Pending

      const needle = query.trim().toLowerCase()
      const matchesQuery =
        !needle ||
        connector.name.toLowerCase().includes(needle) ||
        connector.networkName.toLowerCase().includes(needle) ||
        (connector.hostname ?? '').toLowerCase().includes(needle)

      return matchesFilter && matchesQuery
    })
  }, [allConnectors, filter, query])

  const activeCount = allConnectors.filter((connector) => connector.status === ConnectorStatus.Active).length

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Connectors</h2>
          <p className="page-subtitle">Network gateways providing access to remote networks.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-[oklch(0.82_0.12_160/0.28)] bg-[oklch(0.82_0.12_160/0.12)] text-[oklch(0.82_0.12_160)]">
            <span className="status-pill-dot bg-[oklch(0.82_0.12_160)]" />
            <span className="font-bold">{activeCount}</span> active
          </span>
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">{allConnectors.length}</span> total
          </span>
          <Button onClick={() => setShowAdd(true)} disabled={networks.length === 0} className="gap-2">
            <Plus className="h-4 w-4" />
            Add Connector
          </Button>
        </div>
      </div>

      <div className="flex flex-wrap items-center gap-3">
        <label className="toolbar-input max-w-[330px]">
          <Search className="h-4 w-4 shrink-0" />
          <input
            value={query}
            onChange={(event) => setQuery(event.target.value)}
            placeholder="Search connectors, networks, hostnames..."
          />
        </label>

        {(['all', 'active', 'pending', 'degraded'] as const).map((option) => (
          <button
            key={option}
            onClick={() => setFilter(option)}
            className={`rounded-full px-3.5 py-1.5 text-xs font-bold transition ${
              filter === option
                ? 'border border-primary/30 bg-primary/12 text-primary'
                : 'bg-secondary text-muted-foreground hover:text-foreground'
            }`}
          >
            <span className="capitalize">{option}</span>
          </button>
        ))}
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-[1120px] items-center grid-cols-[1.5fr_160px_1fr_1fr_110px_140px_110px] gap-4 px-5 py-4">
            {['Name', 'Status', 'Network', 'Hostname', 'Version', 'Last Seen', 'Actions'].map((label, index) => (
              <div key={label + index} className={`table-head-label ${index === 6 ? 'text-right' : ''}`}>{label}</div>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-[1120px] p-5 space-y-3">
              {Array.from({ length: 5 }).map((_, index) => (
                <Skeleton key={index} className="h-16 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : filteredConnectors.length === 0 ? (
            <EmptyState
              title="No connectors match the current filters"
              description={networks.length === 0 ? 'Create a remote network first.' : 'Try another filter or add a new connector.'}
              action={networks.length > 0 ? <Button onClick={() => setShowAdd(true)}>Add Connector</Button> : <Link to="/remote-networks" className="text-sm font-semibold text-primary">Create a remote network</Link>}
            />
          ) : (
            <div className="min-w-[1120px]">
              {filteredConnectors.map((connector) => (
                <div key={connector.id} className="admin-table-row group grid items-center grid-cols-[1.5fr_160px_1fr_1fr_110px_140px_110px] gap-4 px-5 py-4">
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
                      label={
                        connector.status === ConnectorStatus.Disconnected
                          ? 'degraded'
                          : connector.status.toLowerCase()
                      }
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

      <InstallCommandModal open={showAdd} onClose={() => setShowAdd(false)} />
    </div>
  )
}
