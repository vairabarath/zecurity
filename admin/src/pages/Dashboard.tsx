import { useQuery } from '@apollo/client/react'
import { Link } from 'react-router-dom'
import {
  Clock3,
  Globe,
  Network,
  Plug,
  Shield,
  ArrowRight,
} from 'lucide-react'
import {
  ConnectorStatus,
  GetRemoteNetworksDocument,
  GetWorkspaceDocument,
  MeDocument,
  RemoteNetworkStatus,
  type GetRemoteNetworksQuery,
} from '@/generated/graphql'
import { Skeleton } from '@/components/ui/skeleton'
import { EntityIcon, StatusPill, relativeTime } from '@/lib/console'

function TopologyCard({
  networks,
  connectors,
  shields,
}: {
  networks: number
  connectors: number
  shields: number
}) {
  return (
    <div className="surface-card relative overflow-hidden p-6">
      <div className="mb-4 flex items-center justify-between">
        <div>
          <div className="text-sm font-semibold">Topology Overview</div>
          <div className="mt-1 text-xs text-muted-foreground">Networks, connectors, and shields in one plane.</div>
        </div>
        <StatusPill label="Live" tone="ok" />
      </div>

      <div className="relative h-[280px] rounded-[18px] border border-border bg-[linear-gradient(180deg,oklch(0.20_0.018_255/0.9),oklch(0.17_0.018_255/0.96))]">
        <div className="absolute inset-0 opacity-50" style={{ backgroundImage: 'radial-gradient(circle at center, oklch(0.86 0.095 175 / 0.12) 0, transparent 55%)' }} />
        <svg viewBox="0 0 760 300" className="h-full w-full">
          <defs>
            <radialGradient id="mintGlow" cx="50%" cy="50%" r="50%">
              <stop offset="0%" stopColor="oklch(0.86 0.095 175)" stopOpacity="0.36" />
              <stop offset="100%" stopColor="oklch(0.86 0.095 175)" stopOpacity="0" />
            </radialGradient>
          </defs>
          <circle cx="220" cy="120" r="22" fill="url(#mintGlow)" />
          <circle cx="220" cy="120" r="18" fill="oklch(0.86 0.095 175 / 0.14)" stroke="oklch(0.86 0.095 175)" />
          <circle cx="390" cy="150" r="28" fill="oklch(0.86 0.095 175 / 0.14)" stroke="oklch(0.86 0.095 175)" />
          <circle cx="540" cy="105" r="14" fill="oklch(0.78 0.10 235 / 0.18)" stroke="oklch(0.78 0.10 235)" />
          <circle cx="560" cy="185" r="14" fill="oklch(0.78 0.10 235 / 0.18)" stroke="oklch(0.78 0.10 235)" />
          <circle cx="660" cy="120" r="12" fill="oklch(0.83 0.11 55 / 0.18)" stroke="oklch(0.83 0.11 55)" />
          <circle cx="670" cy="205" r="12" fill="oklch(0.83 0.11 55 / 0.18)" stroke="oklch(0.83 0.11 55)" />
          <line x1="238" y1="120" x2="362" y2="150" stroke="oklch(0.86 0.095 175 / 0.32)" strokeWidth="1.5" />
          <line x1="418" y1="144" x2="526" y2="109" stroke="oklch(0.86 0.095 175 / 0.32)" strokeWidth="1.5" />
          <line x1="418" y1="158" x2="546" y2="181" stroke="oklch(0.86 0.095 175 / 0.32)" strokeWidth="1.5" />
          <line x1="554" y1="113" x2="648" y2="122" stroke="oklch(0.86 0.095 175 / 0.32)" strokeWidth="1.5" />
          <line x1="572" y1="191" x2="658" y2="202" stroke="oklch(0.86 0.095 175 / 0.32)" strokeWidth="1.5" />
          <text x="198" y="153" fill="oklch(0.80 0.010 250)" fontSize="12" fontWeight="700">edge</text>
          <text x="360" y="196" fill="oklch(0.97 0.005 250)" fontSize="12" fontWeight="700">workspace</text>
          <text x="512" y="94" fill="oklch(0.80 0.010 250)" fontSize="11" fontWeight="700">connector</text>
          <text x="642" y="106" fill="oklch(0.80 0.010 250)" fontSize="11" fontWeight="700">shield</text>
        </svg>

        <div className="absolute bottom-4 left-4 flex flex-wrap gap-2">
          <span className="metric-chip"><strong>{networks}</strong> networks</span>
          <span className="metric-chip"><strong>{connectors}</strong> connectors</span>
          <span className="metric-chip"><strong>{shields}</strong> shields</span>
        </div>
      </div>
    </div>
  )
}

type RecentConnector = GetRemoteNetworksQuery['remoteNetworks'][number]['connectors'][number] & {
  networkId: string
  networkName: string
}

export default function Dashboard() {
  const { data: meData } = useQuery(MeDocument)
  const { data: workspaceData, loading: workspaceLoading } = useQuery(GetWorkspaceDocument)
  const { data: networksData, loading: networkLoading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const networks = networksData?.remoteNetworks ?? []
  const activeNetworks = networks.filter((network) => network.status === RemoteNetworkStatus.Active)
  const connectors: RecentConnector[] = networks.flatMap((network) =>
    network.connectors.map((connector) => ({
      ...connector,
      networkId: network.id,
      networkName: network.name,
    })),
  )
  const activeConnectors = connectors.filter((connector) => connector.status === ConnectorStatus.Active)
  const shields = networks.flatMap((network) => network.shields)

  const recentConnectors = [...connectors]
    .sort((a, b) => {
      const aTime = a.lastSeenAt ? new Date(a.lastSeenAt).getTime() : 0
      const bTime = b.lastSeenAt ? new Date(b.lastSeenAt).getTime() : 0
      return bTime - aTime
    })
    .slice(0, 5)

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Workspace Pulse</h2>
          <p className="page-subtitle">
            {workspaceData?.workspace?.name ?? 'Your workspace'} is under active observation.
          </p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="metric-chip">
            <Globe className="h-3.5 w-3.5 text-primary" />
            <strong>{meData?.me?.email ?? 'operator'}</strong>
          </span>
          <span className="metric-chip">
            <Clock3 className="h-3.5 w-3.5 text-primary" />
            <strong>30s</strong> polling
          </span>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
        <div className="kpi-card">
          <div className="kpi-label">Active Networks</div>
          <div className="kpi-value">{networkLoading && !networksData ? '...' : activeNetworks.length}</div>
        </div>
        <div className="kpi-card">
          <div className="kpi-label">Connectors</div>
          <div className="kpi-value">{networkLoading && !networksData ? '...' : connectors.length}</div>
        </div>
        <div className="kpi-card">
          <div className="kpi-label">Active Connectors</div>
          <div className="kpi-value">{networkLoading && !networksData ? '...' : activeConnectors.length}</div>
        </div>
        <div className="kpi-card">
          <div className="kpi-label">Shields</div>
          <div className="kpi-value">{networkLoading && !networksData ? '...' : shields.length}</div>
        </div>
      </div>

      <div className="grid gap-6 xl:grid-cols-[minmax(0,1.45fr)_minmax(320px,0.85fr)]">
        <TopologyCard networks={networks.length} connectors={connectors.length} shields={shields.length} />

        <div className="surface-card p-6">
          <div className="mb-4 flex items-center gap-2">
            <Network className="h-4 w-4 text-primary" />
            <div className="text-sm font-semibold">Workspace</div>
          </div>

          {workspaceLoading && !workspaceData ? (
            <div className="space-y-3">
              <Skeleton className="h-5 w-32 bg-secondary" />
              <Skeleton className="h-4 w-24 bg-secondary" />
              <Skeleton className="h-20 w-full rounded-2xl bg-secondary" />
            </div>
          ) : (
            <div className="space-y-4">
              <div className="section-card p-4">
                <div className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">Workspace</div>
                <div className="mt-2 text-lg font-semibold">{workspaceData?.workspace.name}</div>
                <div className="mt-1 font-mono text-sm text-primary">{workspaceData?.workspace.slug}.zecurity.in</div>
              </div>

              <div className="grid gap-3 sm:grid-cols-2">
                <div className="section-card p-4">
                  <div className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">Networks</div>
                  <div className="mt-2 flex items-center gap-2">
                    <EntityIcon type="network" />
                    <div className="text-sm font-semibold">{networks.length} registered</div>
                  </div>
                </div>
                <div className="section-card p-4">
                  <div className="text-[11px] font-semibold uppercase tracking-[0.08em] text-muted-foreground">Protected Services</div>
                  <div className="mt-2 flex items-center gap-2">
                    <EntityIcon type="resource" />
                    <div className="text-sm font-semibold">Route via Resources</div>
                  </div>
                </div>
              </div>

              <Link
                to="/remote-networks"
                className="inline-flex items-center gap-2 text-sm font-semibold text-primary transition hover:opacity-80"
              >
                View remote networks
                <ArrowRight className="h-4 w-4" />
              </Link>
            </div>
          )}
        </div>
      </div>

      <div className="surface-card overflow-hidden">
        <div className="flex items-center justify-between border-b border-border px-5 py-4">
          <div className="flex items-center gap-2">
            <Plug className="h-4 w-4 text-primary" />
            <div className="text-sm font-semibold">Recent Connectors</div>
          </div>
          <Link to="/connectors" className="text-sm font-semibold text-primary">
            Open inventory
          </Link>
        </div>

        {networkLoading && !networksData ? (
          <div className="space-y-3 p-5">
            {Array.from({ length: 4 }).map((_, index) => (
              <Skeleton key={index} className="h-16 rounded-2xl bg-secondary" />
            ))}
          </div>
        ) : recentConnectors.length === 0 ? (
          <div className="px-5 py-16 text-center">
            <div className="mx-auto mb-4 grid h-14 w-14 place-items-center rounded-full border border-primary/20 bg-primary/10">
              <Shield className="h-6 w-6 text-primary" />
            </div>
            <h3 className="text-lg font-semibold">No connectors yet</h3>
            <p className="mt-2 text-sm text-muted-foreground">
              Create a remote network and deploy your first connector to begin heartbeat flow.
            </p>
          </div>
        ) : (
          <div className="space-y-3 p-5">
            {recentConnectors.map((connector) => (
              <Link
                key={connector.id}
                to={`/connectors/${connector.id}`}
                className="section-card flex items-center gap-4 p-4 transition hover:border-primary/40 hover:bg-accent/50"
              >
                <EntityIcon type="connector" />
                <div className="min-w-0 flex-1">
                  <div className="flex flex-wrap items-center gap-2">
                    <div className="truncate text-sm font-semibold">{connector.name}</div>
                    <StatusPill
                      label={connector.status.toLowerCase()}
                      tone={connector.status === ConnectorStatus.Active ? 'ok' : connector.status === ConnectorStatus.Disconnected ? 'warn' : 'muted'}
                    />
                  </div>
                  <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                    <span>{connector.networkName}</span>
                    <span>{connector.hostname ?? 'hostname unavailable'}</span>
                    <span>{connector.version ?? 'version unavailable'}</span>
                  </div>
                </div>
                <div className="text-right text-xs text-muted-foreground">
                  <div className="inline-flex items-center gap-1">
                    <Clock3 className="h-3.5 w-3.5" />
                    {relativeTime(connector.lastSeenAt)}
                  </div>
                </div>
              </Link>
            ))}
          </div>
        )}
      </div>

      <div className="grid gap-4 lg:grid-cols-3">
        <Link to="/remote-networks" className="section-card p-4 transition hover:border-primary/40">
          <div className="flex items-center gap-3">
            <EntityIcon type="network" />
            <div>
              <div className="text-sm font-semibold">Remote Networks</div>
              <div className="text-xs text-muted-foreground">Open topology and lifecycle status.</div>
            </div>
          </div>
        </Link>
        <Link to="/shields" className="section-card p-4 transition hover:border-primary/40">
          <div className="flex items-center gap-3">
            <EntityIcon type="shield" />
            <div>
              <div className="text-sm font-semibold">Shields</div>
              <div className="text-xs text-muted-foreground">Review host agents and interface addresses.</div>
            </div>
          </div>
        </Link>
        <Link to="/resources" className="section-card p-4 transition hover:border-primary/40">
          <div className="flex items-center gap-3">
            <EntityIcon type="resource" />
            <div>
              <div className="text-sm font-semibold">Resources</div>
              <div className="text-xs text-muted-foreground">See what is currently protected.</div>
            </div>
          </div>
        </Link>
      </div>
    </div>
  )
}
