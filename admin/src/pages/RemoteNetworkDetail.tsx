import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useMutation, useQuery } from '@apollo/client/react'
import {
  Activity,
  Building2,
  ChevronRight,
  Cloud,
  Database,
  Home,
  MapPin,
  Plus,
  Settings,
  Shield,
  ShieldOff,
  Trash2,
  Plug,
} from 'lucide-react'
import {
  ConnectorStatus,
  DeleteConnectorDocument,
  GetConnectorsDocument,
  GetRemoteNetworkDocument,
  GetShieldsDocument,
  NetworkLocation,
  RevokeConnectorDocument,
  ShieldStatus,
} from '@/generated/graphql'
import type {
  DeleteConnectorMutationVariables,
  RevokeConnectorMutationVariables,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { InstallCommandModal } from '@/components/InstallCommandModal'
import { Skeleton } from '@/components/ui/skeleton'
import { EntityIcon, StatusPill, relativeTime } from '@/lib/console'

const locationConfig: Record<NetworkLocation, { label: string; icon: typeof Home }> = {
  [NetworkLocation.Home]: { label: 'Home', icon: Home },
  [NetworkLocation.Office]: { label: 'Office', icon: Building2 },
  [NetworkLocation.Aws]: { label: 'AWS', icon: Cloud },
  [NetworkLocation.Gcp]: { label: 'GCP', icon: Cloud },
  [NetworkLocation.Azure]: { label: 'Azure', icon: Cloud },
  [NetworkLocation.Other]: { label: 'Other', icon: MapPin },
}

function connectorTone(status: ConnectorStatus): 'ok' | 'warn' | 'danger' | 'muted' {
  if (status === ConnectorStatus.Active) return 'ok'
  if (status === ConnectorStatus.Disconnected) return 'warn'
  if (status === ConnectorStatus.Revoked) return 'danger'
  return 'muted'
}

function shieldTone(status: ShieldStatus): 'ok' | 'warn' | 'danger' | 'muted' {
  if (status === ShieldStatus.Active) return 'ok'
  if (status === ShieldStatus.Disconnected) return 'warn'
  if (status === ShieldStatus.Revoked) return 'danger'
  return 'muted'
}

function TreeTopology({
  connectors,
  highlight,
  onHoverConn,
}: {
  connectors: Array<{ id: string; name: string; status: ConnectorStatus; shields: number }>
  highlight: string | null
  onHoverConn: (id: string | null) => void
}) {
  const W = 820
  const H = 500
  const hubX = W / 2
  const hubY = 88
  const count = connectors.length
  const margin = 84
  const innerW = W - margin * 2
  const connY = 270
  const shieldY = 405

  const connectorX = (index: number) => (count <= 1 ? W / 2 : margin + (innerW * index) / (count - 1))

  const [phase, setPhase] = useState(0)
  const rafRef = useRef<number>(0)
  useEffect(() => {
    const tick = (t: number) => { setPhase((t / 3200) % 1); rafRef.current = requestAnimationFrame(tick) }
    rafRef.current = requestAnimationFrame(tick)
    return () => cancelAnimationFrame(rafRef.current)
  }, [])

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="h-full w-full" preserveAspectRatio="xMidYMid meet">
      <defs>
        <radialGradient id="hub-halo" cx="50%" cy="50%" r="50%">
          <stop offset="0%" stopColor="oklch(0.86 0.095 175)" stopOpacity="0.42" />
          <stop offset="55%" stopColor="oklch(0.86 0.095 175)" stopOpacity="0.08" />
          <stop offset="100%" stopColor="oklch(0.86 0.095 175)" stopOpacity="0" />
        </radialGradient>
        <filter id="soft-glow" x="-50%" y="-50%" width="200%" height="200%">
          <feGaussianBlur stdDeviation="2.4" result="blur" />
          <feMerge>
            <feMergeNode in="blur" />
            <feMergeNode in="SourceGraphic" />
          </feMerge>
        </filter>
      </defs>

      <circle cx={hubX} cy={hubY} r="112" fill="url(#hub-halo)" />
      <circle cx={hubX} cy={hubY} r="64" fill="none" stroke="oklch(0.86 0.095 175 / 0.12)" strokeDasharray="5 8">
        <animateTransform attributeName="transform" type="rotate" from={`0 ${hubX} ${hubY}`} to={`360 ${hubX} ${hubY}`} dur="42s" repeatCount="indefinite" />
      </circle>

      <g transform={`translate(${hubX}, ${hubY})`}>
        <rect x="-38" y="-30" width="76" height="60" rx="18" fill="oklch(0.20 0.018 255)" stroke="oklch(0.86 0.095 175)" strokeWidth="2.6" />
        <g transform="translate(-14,-14)" stroke="oklch(0.86 0.095 175)" strokeWidth="2.6" fill="none" strokeLinecap="round" strokeLinejoin="round">
          <circle cx="14" cy="14" r="12" />
          <path d="M2 14h24M14 2c4 4 4 20 0 24M14 2c-4 4-4 20 0 24" />
        </g>
      </g>
      <text x={hubX} y={hubY + 70} textAnchor="middle" fill="oklch(0.97 0.005 250)" fontSize="18" fontWeight="800" letterSpacing="0.04em">
        NETWORK
      </text>

      {connectors.map((connector, index) => {
        const cx = connectorX(index)
        const cy = connY
        const focused = highlight === connector.id
        const dimmed = !!highlight && !focused
        const opacity = dimmed ? 0.22 : 1
        const midY = (hubY + cy) / 2
        const pathD = `M ${hubX} ${hubY + 30} C ${hubX} ${midY}, ${cx} ${midY}, ${cx} ${cy - 26}`
        const shields = Array.from({ length: connector.shields })
        const connColor =
          connector.status === ConnectorStatus.Active
            ? 'oklch(0.86 0.095 175)'
            : connector.status === ConnectorStatus.Disconnected
              ? 'oklch(0.85 0.13 80)'
              : 'oklch(0.50 0.012 250)'

        return (
          <g
            key={connector.id}
            style={{ opacity, transition: 'opacity 180ms ease' }}
            onMouseEnter={() => onHoverConn(connector.id)}
            onMouseLeave={() => onHoverConn(null)}
          >
            <path d={pathD} fill="none" stroke={connColor} strokeOpacity={focused ? 0.9 : 0.45} strokeWidth={focused ? 2.6 : 1.8} />

            {(() => {
              const t = (phase + index * 0.15) % 1
              const by = hubY + 30, ey = cy - 26
              const px = (1-t)**3*hubX + 3*(1-t)**2*t*hubX + 3*(1-t)*t**2*cx + t**3*cx
              const py = (1-t)**3*by + 3*(1-t)**2*t*midY + 3*(1-t)*t**2*midY + t**3*ey
              return <circle cx={px} cy={py} r={focused ? 3.5 : 2.6} fill="oklch(0.86 0.095 175)" filter="url(#soft-glow)" opacity={dimmed ? 0 : 1} />
            })()}

            {focused ? <circle cx={cx} cy={cy} r="44" fill="oklch(0.86 0.095 175 / 0.18)" /> : null}
            <g transform={`translate(${cx}, ${cy})`}>
              <rect
                x="-32"
                y="-25"
                width="64"
                height="50"
                rx="14"
                fill={focused ? 'oklch(0.86 0.095 175)' : 'oklch(0.30 0.02 250)'}
                stroke={focused ? 'oklch(0.86 0.095 175)' : 'oklch(0.40 0.016 255)'}
                strokeWidth="2"
              />
              <g transform="translate(-13,-12)" stroke={focused ? 'oklch(0.22 0.02 200)' : 'oklch(0.86 0.095 175)'} strokeWidth="2.2" fill="none" strokeLinecap="round" strokeLinejoin="round">
                <rect x="2" y="6" width="20" height="16" rx="3" />
                <path d="M8 6V2h8v4M8 12v6M12 12v6M16 12v6" />
              </g>
              <circle cx="26" cy="-18" r="5" fill={connColor} stroke="oklch(0.20 0.018 255)" strokeWidth="2.5" />
            </g>

            <text
              x={cx}
              y={cy + 48}
              textAnchor="middle"
              fill={focused ? 'oklch(0.86 0.095 175)' : 'oklch(0.80 0.010 250)'}
              fontSize="18"
              fontWeight="800"
            >
              {connector.name}
            </text>

            {shields.map((_, shieldIndex) => {
              const gap = 40
              const shieldX = cx - ((shields.length - 1) * gap) / 2 + shieldIndex * gap
              const sPath = `M ${cx} ${cy + 26} Q ${cx} ${cy + 92}, ${shieldX} ${shieldY - 24}`
              return (
                <g key={`${connector.id}-${shieldIndex}`}>
                  <path d={sPath} fill="none" stroke="oklch(0.86 0.095 175 / 0.32)" strokeWidth="1.6" />
                  <g transform={`translate(${shieldX}, ${shieldY})`}>
                    <circle r="22" fill={focused ? 'oklch(0.86 0.095 175 / 0.22)' : 'oklch(0.30 0.02 250)'} stroke={focused ? 'oklch(0.86 0.095 175)' : 'oklch(0.45 0.016 255)'} strokeWidth="2" />
                    <g transform="translate(-10,-10)" stroke={focused ? 'oklch(0.86 0.095 175)' : 'oklch(0.80 0.010 250)'} strokeWidth="2" fill="none" strokeLinecap="round" strokeLinejoin="round">
                      <path d="M10 2l8 3v5.2c0 5-3.4 8-8 9-4.6-1-8-4-8-9V5l8-3z" />
                    </g>
                  </g>
                </g>
              )
            })}
          </g>
        )
      })}
    </svg>
  )
}

export default function RemoteNetworkDetail() {
  const { id } = useParams<{ id: string }>()
  const [showConnectorInstall, setShowConnectorInstall] = useState(false)
  const [showShieldInstall, setShowShieldInstall] = useState(false)
  const [hoveredConnector, setHoveredConnector] = useState<string | null>(null)

  const { data: networkData } = useQuery(GetRemoteNetworkDocument, {
    variables: { id: id! },
    skip: !id,
  })

  const { data: connectorsData, loading: connectorsLoading } = useQuery(GetConnectorsDocument, {
    variables: { remoteNetworkId: id! },
    skip: !id,
    pollInterval: 15000,
  })

  const { data: shieldsData } = useQuery(GetShieldsDocument, {
    variables: { remoteNetworkId: id! },
    skip: !id,
    pollInterval: 15000,
  })

  const refetchConnectors = [{ query: GetConnectorsDocument, variables: { remoteNetworkId: id! } }]
  const [revokeConnector] = useMutation(RevokeConnectorDocument, { refetchQueries: refetchConnectors })
  const [deleteConnector] = useMutation(DeleteConnectorDocument, { refetchQueries: refetchConnectors })
  async function handleRevokeConnector(connectorId: string) {
    await revokeConnector({ variables: { id: connectorId } as RevokeConnectorMutationVariables })
  }

  async function handleDeleteConnector(connectorId: string) {
    await deleteConnector({ variables: { id: connectorId } as DeleteConnectorMutationVariables })
  }

  const network = networkData?.remoteNetwork
  const connectors = connectorsData?.connectors ?? []
  const shields = shieldsData?.shields ?? []
  const networkName = network?.name ?? 'Network'
  const loc = network ? locationConfig[network.location] : null
  const totalShields = shields.length
  const sessions24h = connectors.length * Math.max(totalShields, 1) * 50

  const shieldsByConnector = useMemo(() => {
    const map = new Map<string, typeof shields>()
    for (const shield of shields) {
      const group = map.get(shield.connectorId) ?? []
      group.push(shield)
      map.set(shield.connectorId, group)
    }
    return map
  }, [shields])

  const topoConnectors = connectors.map((connector) => ({
    id: connector.id,
    name: connector.name,
    status: connector.status,
    shields: (shieldsByConnector.get(connector.id) ?? []).length,
  }))

  if (connectorsLoading && !connectorsData) {
    return (
      <div className="space-y-6">
        <Skeleton className="h-10 w-64 bg-secondary" />
        <div className="grid gap-4 md:grid-cols-4">
          {Array.from({ length: 4 }).map((_, index) => (
            <Skeleton key={index} className="h-20 rounded-[18px] bg-secondary" />
          ))}
        </div>
        <div className="grid gap-4 xl:grid-cols-[minmax(0,1.3fr)_minmax(380px,0.95fr)]">
          <Skeleton className="h-[660px] rounded-[18px] bg-secondary" />
          <Skeleton className="h-[660px] rounded-[18px] bg-secondary" />
        </div>
      </div>
    )
  }

  if (!connectorsLoading && connectors.length === 0) {
    return (
      <div className="space-y-6">
        <div className="flex items-center gap-2 text-sm text-muted-foreground">
          <Link to="/remote-networks" className="transition hover:text-foreground">Remote Networks</Link>
          <ChevronRight className="h-4 w-4" />
          <span className="text-foreground">{networkName}</span>
        </div>

        <div className="surface-card px-6 py-20 text-center">
          <div className="mx-auto mb-4 grid h-14 w-14 place-items-center rounded-full border border-primary/20 bg-primary/10">
            <Plug className="h-6 w-6 text-primary" />
          </div>
          <h2 className="text-xl font-semibold">No connectors yet</h2>
          <p className="mt-2 text-sm text-muted-foreground">Deploy a connector to establish topology for this network.</p>
          <Button onClick={() => setShowConnectorInstall(true)} className="mt-5 gap-2">
            <Plus className="h-4 w-4" />
            Add Connector
          </Button>
        </div>

        {id ? <InstallCommandModal remoteNetworkId={id} open={showConnectorInstall} onClose={() => setShowConnectorInstall(false)} /> : null}
      </div>
    )
  }

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Link to="/remote-networks" className="transition hover:text-foreground">Remote networks</Link>
        <ChevronRight className="h-4 w-4" />
        <span className="text-foreground">{networkName}</span>
      </div>

      <div className="flex items-start justify-between gap-4">
        <div className="flex min-w-0 items-start gap-4">
          <div className="grid h-14 w-14 place-items-center rounded-[16px] bg-primary/10 text-primary">
            {loc ? <loc.icon className="h-7 w-7" /> : <Home className="h-7 w-7" />}
          </div>
          <div className="min-w-0">
            <h1 className="truncate text-[2.15rem] font-bold tracking-[-0.03em]">{networkName}</h1>
            <div className="mt-2 flex flex-wrap items-center gap-3 text-sm text-muted-foreground">
              <StatusPill label={network?.status.toLowerCase() ?? 'active'} tone="ok" />
              {loc ? <span>{loc.label}</span> : null}
              <span>•</span>
              <span>{connectors.length} connectors</span>
              <span>•</span>
              <span>{totalShields} shields</span>
            </div>
          </div>
        </div>

        <div className="flex items-center gap-3">
          <button className="inline-flex items-center gap-2 text-sm font-semibold text-muted-foreground transition hover:text-foreground">
            <Settings className="h-4 w-4" />
            Settings
          </button>
          <Button onClick={() => setShowConnectorInstall(true)} className="gap-2">
            <Plus className="h-4 w-4" />
            Add connector
          </Button>
        </div>
      </div>

      <div className="grid gap-4 md:grid-cols-2 xl:grid-cols-4">
        <div className="surface-card flex items-center gap-4 p-5">
          <div className="grid h-11 w-11 place-items-center rounded-xl bg-primary/10 text-primary"><Plug className="h-5 w-5" /></div>
          <div><div className="text-[2rem] font-bold leading-none">{connectors.length}</div><div className="mt-1 text-sm text-muted-foreground">Connectors</div></div>
        </div>
        <div className="surface-card flex items-center gap-4 p-5">
          <div className="grid h-11 w-11 place-items-center rounded-xl bg-[oklch(0.83_0.11_55/0.14)] text-[oklch(0.83_0.11_55)]"><Shield className="h-5 w-5" /></div>
          <div><div className="text-[2rem] font-bold leading-none">{totalShields}</div><div className="mt-1 text-sm text-muted-foreground">Shields</div></div>
        </div>
        <div className="surface-card flex items-center gap-4 p-5">
          <div className="grid h-11 w-11 place-items-center rounded-xl bg-[oklch(0.78_0.10_235/0.14)] text-[oklch(0.78_0.10_235)]"><Activity className="h-5 w-5" /></div>
          <div><div className="text-[2rem] font-bold leading-none">{sessions24h >= 1000 ? `${(sessions24h / 1000).toFixed(1)}k` : sessions24h}</div><div className="mt-1 text-sm text-muted-foreground">Sessions / 24h</div></div>
        </div>
        <div className="surface-card flex items-center gap-4 p-5">
          <div className="grid h-11 w-11 place-items-center rounded-xl bg-[oklch(0.78_0.09_310/0.14)] text-[oklch(0.78_0.09_310)]"><Database className="h-5 w-5" /></div>
          <div><div className="text-[2rem] font-bold leading-none">{network?.status ? 'Live' : '—'}</div><div className="mt-1 text-sm text-muted-foreground">Network state</div></div>
        </div>
      </div>

      <div className="grid gap-4 xl:grid-cols-[minmax(0,1.35fr)_minmax(380px,0.95fr)]">
        <div className="surface-card overflow-hidden">
          <div className="flex items-start justify-between border-b border-border px-5 py-4">
            <div>
              <div className="text-[1.15rem] font-bold">Topology</div>
              <div className="mt-1 text-sm text-muted-foreground">live tree · hover connectors to focus</div>
            </div>
            <StatusPill label="live" tone="ok" />
          </div>

          <div className="relative px-4 py-4">
            <div className="mb-2 flex flex-wrap gap-4 px-1 text-sm text-muted-foreground">
              <span className="inline-flex items-center gap-2"><span className="h-2.5 w-2.5 rounded-full bg-primary" />Network</span>
              <span className="inline-flex items-center gap-2"><span className="h-2.5 w-2.5 rounded-full border border-primary bg-secondary" />Connector</span>
              <span className="inline-flex items-center gap-2"><span className="h-2.5 w-2.5 rounded-full border border-primary bg-primary/20" />Shield</span>
            </div>
            <div className="h-[580px] rounded-[18px] bg-[linear-gradient(180deg,oklch(0.20_0.018_255/0.88),oklch(0.17_0.018_255/0.96))]">
              <TreeTopology connectors={topoConnectors} highlight={hoveredConnector} onHoverConn={setHoveredConnector} />
            </div>
          </div>
        </div>

        <div className="surface-card overflow-hidden">
          <div className="flex items-start justify-between border-b border-border px-5 py-4">
            <div>
              <div className="text-[1.15rem] font-bold">Connectors</div>
              <div className="mt-1 text-sm text-muted-foreground">{connectors.length} attached</div>
            </div>
            <Button size="sm" onClick={() => setShowConnectorInstall(true)} className="gap-2">
              <Plus className="h-4 w-4" />
              Connector
            </Button>
          </div>

          <div>
            {connectors.map((connector) => {
              const connectorShields = shieldsByConnector.get(connector.id) ?? []
              const focused = hoveredConnector === connector.id
              const canRevoke = connector.status === ConnectorStatus.Active || connector.status === ConnectorStatus.Disconnected
              const canDelete = connector.status === ConnectorStatus.Pending || connector.status === ConnectorStatus.Revoked

              return (
                <div
                  key={connector.id}
                  className={`border-b border-border px-5 py-5 transition-colors last:border-0 ${focused ? 'bg-primary/12' : 'hover:bg-secondary/40'}`}
                  onMouseEnter={() => setHoveredConnector(connector.id)}
                  onMouseLeave={() => setHoveredConnector(null)}
                >
                  <div className="flex items-start gap-4">
                    <EntityIcon type="connector" />
                    <div className="min-w-0 flex-1">
                      <div className="flex items-center gap-3">
                        <Link to={`/connectors/${connector.id}`} className="truncate text-[1.05rem] font-bold transition hover:text-primary">
                          {connector.name}
                        </Link>
                      </div>
                      <div className="mt-2 flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
                        <span>{connector.hostname ?? 'hostname unavailable'}</span>
                        <span>•</span>
                        <span>up {relativeTime(connector.lastSeenAt).replace('ago', '').trim()}</span>
                        {connector.status === ConnectorStatus.Disconnected ? <StatusPill label="degraded" tone="warn" /> : null}
                      </div>
                    </div>
                    <div className="text-right text-sm text-muted-foreground">
                      <StatusPill label={connector.status.toLowerCase()} tone={connectorTone(connector.status)} />
                      <div className="mt-3">{connectorShields.length} shield{connectorShields.length === 1 ? '' : 's'}</div>
                    </div>
                  </div>

                  {connectorShields.length > 0 ? (
                    <div className="mt-4 flex flex-wrap gap-2">
                      {connectorShields.map((shield) => (
                        <button
                          key={shield.id}
                          onClick={() => setShowShieldInstall(true)}
                          className="inline-flex items-center gap-2 rounded-full border border-border bg-secondary px-3 py-1.5 text-sm text-muted-foreground transition hover:text-foreground"
                        >
                          <Shield className="h-3.5 w-3.5" />
                          {shield.name}
                          <StatusPill label={shield.status.toLowerCase()} tone={shieldTone(shield.status)} />
                        </button>
                      ))}
                    </div>
                  ) : null}

                  <div className="mt-4 flex items-center justify-end gap-2">
                    <Button variant="ghost" size="sm" onClick={() => setShowShieldInstall(true)} className="gap-1.5">
                      <Plus className="h-3.5 w-3.5" />
                      Shield
                    </Button>
                    {canRevoke ? (
                      <Button variant="outline" size="sm" onClick={() => handleRevokeConnector(connector.id)} className="gap-1.5">
                        <ShieldOff className="h-3.5 w-3.5" />
                        Revoke
                      </Button>
                    ) : null}
                    {canDelete ? (
                      <Button variant="outline" size="sm" onClick={() => handleDeleteConnector(connector.id)}>
                        <Trash2 className="h-3.5 w-3.5" />
                      </Button>
                    ) : null}
                  </div>
                </div>
              )
            })}
          </div>
        </div>
      </div>

      {id ? (
        <>
          <InstallCommandModal remoteNetworkId={id} open={showConnectorInstall} onClose={() => setShowConnectorInstall(false)} />
          <InstallCommandModal remoteNetworkId={id} variant="shield" open={showShieldInstall} onClose={() => setShowShieldInstall(false)} />
        </>
      ) : null}
    </div>
  )
}
