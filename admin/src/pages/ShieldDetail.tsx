import { useEffect, useMemo, useRef, useState } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { useQuery, useMutation } from '@apollo/client/react'
import {
  GetShieldDocument,
  GetRemoteNetworkDocument,
  GetConnectorDocument,
  RevokeShieldDocument,
  DeleteShieldDocument,
  ShieldStatus,
  GetRemoteNetworksDocument,
  GetAllResourcesDocument,
} from '@/generated/graphql'
import type {
  RevokeShieldMutationVariables,
  DeleteShieldMutationVariables,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { useAuthStore } from '@/store/auth'
import { cn } from '@/lib/utils'
import {
  ArrowLeft,
  Shield,
  ShieldOff,
  Trash2,
  CheckCircle,
  ChevronRight,
  Copy,
  RefreshCw,
  Loader2,
  Globe,
  Database,
  Clock,
  Wifi,
  Network,
  Server,
  Activity,
  Zap,
  Lock,
  Plus,
} from 'lucide-react'
import { StatusPill, relativeTime } from '@/lib/console'

function Sparkline({ points, color = 'oklch(0.86 0.095 175)' }: { points: number[], color?: string }) {
  const W = 160, H = 32
  const max = Math.max(...points, 1)
  const step = W / (points.length - 1)
  const path = points.map((v, i) => `${i === 0 ? 'M' : 'L'} ${(i * step).toFixed(1)} ${(H - (v / max) * (H - 4)).toFixed(1)}`).join(' ')
  return (
    <svg viewBox={`0 0 ${W} ${H}`} width="100%" height={H} preserveAspectRatio="none" className="overflow-visible">
      <path d={path} fill="none" stroke={color} strokeWidth="1.8" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

function MetadataCell({ label, value, icon, mono }: { label: string, value: React.ReactNode, icon?: React.ReactNode, mono?: boolean }) {
  return (
    <div className="border-b border-border px-5 py-4 last:border-0 sm:border-r sm:last:border-r-0">
      <div className="flex items-center gap-2 text-[10px] font-bold uppercase tracking-[0.08em] text-muted-foreground/80">
        {icon}
        {label}
      </div>
      <div className={cn('mt-1.5 truncate text-[14px] font-semibold text-foreground', mono && 'font-mono text-[13px] text-muted-foreground')}>
        {value}
      </div>
    </div>
  )
}

export default function ShieldDetail() {
  const { shieldId } = useParams<{ shieldId: string }>()
  const navigate = useNavigate()
  const accessToken = useAuthStore((s) => s.accessToken)

  const { data, loading } = useQuery(GetShieldDocument, {
    variables: { id: shieldId! },
    skip: !shieldId,
    pollInterval: 10000,
    fetchPolicy: 'cache-and-network',
  })

  const shield = data?.shield

  const { data: networkData } = useQuery(GetRemoteNetworkDocument, {
    variables: { id: shield?.remoteNetworkId ?? '' },
    skip: !shield?.remoteNetworkId,
  })

  const networkName = networkData?.remoteNetwork?.name ?? 'Network'

  const { data: connectorData } = useQuery(GetConnectorDocument, {
    variables: { id: shield?.connectorId ?? '' },
    skip: !shield?.connectorId,
  })

  const connectorName = connectorData?.connector?.name ?? shield?.connectorId ?? 'Connector'

  // Fetch all resources to find ones belonging to this shield
  const { data: resourcesData } = useQuery(GetAllResourcesDocument, {
    pollInterval: 20000,
  })

  const shieldResources = useMemo(() => {
    if (!resourcesData || !shieldId) return []
    return resourcesData.allResources.filter(r => r.shield?.id === shieldId)
  }, [resourcesData, shieldId])

  // Install command state
  const [tokenLoading, setTokenLoading] = useState(false)
  const [tokenError, setTokenError] = useState<string | null>(null)
  const [installCommand, setInstallCommand] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)
  const didFetch = useRef(false)

  const fetchInstallCommand = async () => {
    if (!shieldId || !accessToken) return
    setTokenLoading(true)
    setTokenError(null)
    try {
      const resp = await fetch(`/api/shields/${shieldId}/token`, {
        method: 'POST',
        credentials: 'include',
        headers: { Authorization: `Bearer ${accessToken}` },
      })
      if (!resp.ok) {
        const text = await resp.text()
        throw new Error(text || 'Failed to generate token')
      }
      const result = await resp.json()
      setInstallCommand(result.install_command)
    } catch (e: unknown) {
      setTokenError(e instanceof Error ? e.message : 'Failed to generate token')
    } finally {
      setTokenLoading(false)
    }
  }

  const pending = shield?.status === ShieldStatus.Pending

  useEffect(() => {
    if (shield && pending && !didFetch.current) {
      didFetch.current = true
      void fetchInstallCommand()
    }
  }, [shield, pending])

  function handleCopy() {
    if (!installCommand) return
    navigator.clipboard.writeText(installCommand)
    setCopied(true)
    setTimeout(() => setCopied(false), 2000)
  }

  const [revokeShield, { loading: revoking }] = useMutation(RevokeShieldDocument, {
    refetchQueries: [{ query: GetShieldDocument, variables: { id: shieldId } }],
  })

  const [deleteShield, { loading: deleting }] = useMutation(DeleteShieldDocument, {
    refetchQueries: [{ query: GetRemoteNetworksDocument }],
    onCompleted: () => navigate('/shields'),
  })

  async function handleRevoke() {
    if (!shieldId) return
    if (!window.confirm(`Revoke shield "${shield?.name}"?`)) return
    await revokeShield({ variables: { id: shieldId } as RevokeShieldMutationVariables })
  }

  async function handleDelete() {
    if (!shieldId) return
    if (!window.confirm(`Delete shield "${shield?.name}"? This cannot be undone.`)) return
    await deleteShield({ variables: { id: shieldId } as DeleteShieldMutationVariables })
  }

  if (loading && !shield) {
    return (
      <div className="flex items-center justify-center p-16">
        <div className="flex flex-col items-center gap-3">
          <Loader2 className="h-6 w-6 animate-spin text-primary" />
          <p className="text-xs font-mono tracking-[0.14em] text-muted-foreground">Loading shield...</p>
        </div>
      </div>
    )
  }

  if (!shield) {
    return (
      <div className="space-y-6">
        <Link to="/shields" className="inline-flex items-center gap-2 text-sm text-muted-foreground transition hover:text-foreground">
          <ArrowLeft className="h-4 w-4" />
          Back to Shields
        </Link>
        <div className="surface-card px-6 py-20 text-center">
          <Shield className="mx-auto h-14 w-14 text-destructive/40" />
          <h2 className="mt-4 text-2xl font-bold">Shield not found</h2>
          <p className="mt-2 text-muted-foreground">This shield no longer exists or was deleted.</p>
        </div>
      </div>
    )
  }

  const isRevoked = shield.status === ShieldStatus.Revoked
  const canRevoke = shield.status === ShieldStatus.Active || shield.status === ShieldStatus.Disconnected
  const canDelete = pending || isRevoked

  // Mock telemetry data
  const throughputInPoints = pending ? [0, 0, 0, 0, 0, 0, 0, 0] : [2.1, 3.4, 4.2, 3.8, 5.1, 4.2, 4.5, 3.9, 4.2, 4.0, 4.2]
  const throughputOutPoints = pending ? [0, 0, 0, 0, 0, 0, 0, 0] : [0.8, 1.2, 1.1, 1.5, 2.0, 1.3, 1.1, 1.4, 1.2, 1.0, 1.1]
  const allowedPoints = pending ? [0, 0, 0, 0, 0, 0, 0, 0] : [800, 850, 920, 1050, 1100, 1080, 1024]
  const deniedPoints = pending ? [0, 0, 0, 0, 0, 0, 0, 0] : [2, 5, 8, 12, 15, 22, 37]

  return (
    <div className="space-y-6">
      <Link to="/shields" className="inline-flex items-center gap-2 text-sm text-muted-foreground transition hover:text-foreground">
        <ArrowLeft className="h-4 w-4" />
        Back to Shields
      </Link>

      <div className="flex items-start justify-between gap-4">
        <div className="flex min-w-0 items-center gap-4">
          <div className="grid h-12 w-12 place-items-center rounded-2xl bg-[oklch(0.78_0.10_235/0.14)] text-[oklch(0.78_0.10_235)]">
            <Shield className="h-6 w-6" />
          </div>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="truncate text-[2.2rem] font-bold tracking-[-0.03em]">{shield.name}</h1>
              <StatusPill
                label={shield.status === ShieldStatus.Disconnected ? 'degraded' : shield.status.toLowerCase()}
                tone={shield.status === ShieldStatus.Active ? 'ok' : shield.status === ShieldStatus.Disconnected ? 'warn' : shield.status === ShieldStatus.Revoked ? 'danger' : 'muted'}
              />
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-3 text-sm text-muted-foreground">
              <span className="font-mono text-[13px] opacity-70">{shield.id}</span>
              <span className="opacity-40">.</span>
              <div className="flex items-center gap-1.5 rounded-lg bg-[oklch(0.86_0.095_175/0.12)] px-2 py-0.5 text-xs font-bold text-[oklch(0.86_0.095_175)]">
                <Globe className="h-3 w-3" />
                {networkName}
              </div>
              <ChevronRight className="h-3.5 w-3.5 opacity-40" />
              <div className="flex items-center gap-1.5 rounded-lg bg-secondary px-2 py-0.5 text-xs font-bold">
                <Server className="h-3 w-3" />
                {connectorName}
              </div>
            </div>
          </div>
        </div>

        <div className="flex items-center gap-3">
          {canRevoke && (
            <Button variant="outline" size="sm" onClick={handleRevoke} disabled={revoking} className="gap-2 text-destructive/80 hover:bg-destructive/5 hover:text-destructive">
              {revoking ? <Loader2 className="h-4 w-4 animate-spin" /> : <ShieldOff className="h-4 w-4" />}
              Revoke
            </Button>
          )}
          {canDelete && (
            <Button variant="destructive" size="sm" onClick={handleDelete} disabled={deleting} className="gap-2">
              {deleting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Trash2 className="h-4 w-4" />}
              Delete Shield
            </Button>
          )}
        </div>
      </div>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-4">
        {[
          { label: 'Open sessions', value: pending ? '0' : '18', icon: <Wifi className="h-4 w-4" />, tone: 'info' },
          { label: 'Sessions / 24h', value: pending ? '0' : '412', icon: <Activity className="h-4 w-4" />, tone: 'ok' },
          { label: 'Denied / 24h', value: pending ? '0' : '37', icon: <ShieldOff className="h-4 w-4" />, tone: 'warn' },
          { label: 'Policies', value: '3', icon: <Lock className="h-4 w-4" />, tone: 'muted' },
        ].map((kpi) => (
          <div key={kpi.label} className="surface-card flex items-center gap-4 p-5">
            <div className={cn('grid h-10 w-10 place-items-center rounded-xl', {
              'bg-blue-500/12 text-blue-500': kpi.tone === 'info',
              'bg-emerald-500/12 text-emerald-500': kpi.tone === 'ok',
              'bg-orange-500/12 text-orange-500': kpi.tone === 'warn',
              'bg-secondary text-muted-foreground': kpi.tone === 'muted',
            })}>
              {kpi.icon}
            </div>
            <div>
              <div className="text-2xl font-bold tracking-tight">{kpi.value}</div>
              <div className="text-xs font-medium text-muted-foreground">{kpi.label}</div>
            </div>
          </div>
        ))}
      </div>

      <div className="grid gap-4 xl:grid-cols-[minmax(0,1.35fr)_minmax(360px,0.95fr)]">
        <div className="space-y-4">
          {pending ? (
            <div className="surface-card overflow-hidden">
              <div className="flex items-center justify-between border-b border-border px-5 py-4">
                <div className="text-sm font-semibold uppercase tracking-[0.08em] text-muted-foreground">Install Command</div>
                <div className="flex items-center gap-2">
                  <div className="rounded-xl bg-secondary p-1">
                    <button className="rounded-lg bg-card px-4 py-2 text-sm font-semibold">Linux</button>
                    <button className="rounded-lg px-4 py-2 text-sm font-semibold text-muted-foreground">Docker</button>
                  </div>
                  <Button variant="outline" size="sm" onClick={handleCopy} disabled={!installCommand} className="gap-2">
                    {copied ? <CheckCircle className="h-4 w-4 text-primary" /> : <Copy className="h-4 w-4" />}
                    {copied ? 'Copied' : 'Copy'}
                  </Button>
                </div>
              </div>

              <div className="bg-[linear-gradient(180deg,oklch(0.20_0.018_255/0.88),oklch(0.17_0.018_255/0.96))] p-5">
                {tokenLoading ? (
                  <div className="flex items-center gap-2 text-sm text-muted-foreground">
                    <Loader2 className="h-4 w-4 animate-spin" />
                    Generating enrollment token...
                  </div>
                ) : tokenError ? (
                  <div className="space-y-3">
                    <p className="text-sm text-destructive">{tokenError}</p>
                    <Button variant="outline" size="sm" onClick={fetchInstallCommand} className="gap-2">
                      <RefreshCw className="h-4 w-4" />
                      Retry
                    </Button>
                  </div>
                ) : (
                  <pre className="overflow-x-auto whitespace-pre-wrap break-words font-mono text-[15px] leading-8 text-foreground/90">
                    {installCommand ?? '# Install command will appear here'}
                  </pre>
                )}
              </div>
            </div>
          ) : null}

          <div className="surface-card overflow-hidden">
            <div className="border-b border-border px-5 py-4">
              <div className="text-[1.15rem] font-bold">Metadata</div>
              <div className="mt-1 text-sm text-muted-foreground">Provisioning and identity</div>
            </div>
            <div className="grid grid-cols-1 sm:grid-cols-3">
              <MetadataCell label="Name" value={shield.name} icon={<span className="text-muted-foreground">#</span>} />
              <MetadataCell label="Status" value={<StatusPill label={shield.status === ShieldStatus.Disconnected ? 'degraded' : shield.status.toLowerCase()} tone={shield.status === ShieldStatus.Active ? 'ok' : shield.status === ShieldStatus.Disconnected ? 'warn' : shield.status === ShieldStatus.Revoked ? 'danger' : 'muted'} />} icon={<Wifi className="h-3 w-3" />} />
              <MetadataCell label="Version" value={shield.version ?? '—'} icon={<Zap className="h-3 w-3" />} mono />

              <MetadataCell label="Last Seen" value={relativeTime(shield.lastSeenAt)} icon={<Clock className="h-3 w-3" />} />
              <MetadataCell label="Remote Network" value={<Link to={`/remote-networks/${shield.remoteNetworkId}`} className="text-primary hover:opacity-80 transition">{networkName}</Link>} icon={<Globe className="h-3 w-3" />} />
              <MetadataCell label="Connected Via" value={<Link to={`/connectors/${shield.connectorId}`} className="text-primary hover:opacity-80 transition">{connectorName}</Link>} icon={<Server className="h-3 w-3" />} />

              <MetadataCell label="Hostname" value={shield.hostname ?? '—'} icon={<Server className="h-3 w-3" />} mono />
              <MetadataCell label="LAN IP" value={shield.lanIp ?? '—'} icon={<Network className="h-3 w-3" />} mono />
              <MetadataCell label="Interface Addr" value={shield.interfaceAddr ?? '—'} icon={<Wifi className="h-3 w-3" />} mono />

              <MetadataCell label="Cert Expires" value={shield.certNotAfter ? relativeTime(shield.certNotAfter) : '—'} icon={<Shield className="h-3 w-3" />} />
              <MetadataCell label="Created" value={new Date(shield.createdAt).toLocaleDateString('en-US', { month: 'short', day: 'numeric', year: 'numeric' })} icon={<Clock className="h-3 w-3" />} />
            </div>
          </div>
        </div>

        <div className="space-y-4">
          <div className="surface-card p-5">
            <div className="flex items-center gap-4 border-b border-border pb-5">
              <div className={cn('grid h-12 w-12 place-items-center rounded-2xl transition-all duration-700', {
                'bg-[oklch(0.82_0.12_160/0.15)] text-[oklch(0.82_0.12_160)] shadow-[0_0_20px_oklch(0.82_0.12_160/0.15)]': shield.status === ShieldStatus.Active,
                'bg-orange-500/12 text-orange-500': shield.status === ShieldStatus.Disconnected,
                'bg-secondary text-muted-foreground': pending || isRevoked,
              })}>
                <Shield className="h-6 w-6" />
              </div>
              <div className="flex-1">
                <div className="text-[10px] font-bold uppercase tracking-[0.1em] text-muted-foreground/80">Health</div>
                <div className="text-xl font-bold tracking-tight text-foreground">
                  {shield.status === ShieldStatus.Active ? 'Enforcing' : shield.status === ShieldStatus.Disconnected ? 'Degraded' : 'Awaiting install'}
                </div>
                {!pending && (
                  <div className="mt-1 text-xs text-muted-foreground">
                    via {connectorName} • {pending ? '0' : '1,024'} rules allowed today
                  </div>
                )}
              </div>
            </div>

            {!pending && (
              <div className="mt-6 space-y-8">
                <div className="grid grid-cols-2 gap-x-8 gap-y-6">
                  <div className="space-y-3">
                    <div className="flex items-center justify-between">
                      <div className="text-[10px] font-bold uppercase tracking-[0.1em] text-muted-foreground/80">Throughput In</div>
                      <div className="text-sm font-bold tracking-tight">{pending ? '0' : '4.2'}<span className="ml-1 text-[10px] text-muted-foreground font-medium">MB/s</span></div>
                    </div>
                    <Sparkline points={throughputInPoints} color="oklch(0.78 0.10 235)" />
                  </div>
                  <div className="space-y-3">
                    <div className="flex items-center justify-between">
                      <div className="text-[10px] font-bold uppercase tracking-[0.1em] text-muted-foreground/80">Throughput Out</div>
                      <div className="text-sm font-bold tracking-tight">{pending ? '0' : '1.1'}<span className="ml-1 text-[10px] text-muted-foreground font-medium">MB/s</span></div>
                    </div>
                    <Sparkline points={throughputOutPoints} color="oklch(0.78 0.09 310)" />
                  </div>
                  <div className="space-y-3 border-t border-border/60 pt-6">
                    <div className="text-[10px] font-bold uppercase tracking-[0.1em] text-muted-foreground/80">Allowed / 24h</div>
                    <div className="text-2xl font-bold tracking-tight">{pending ? '0' : '1,024'}</div>
                    <Sparkline points={allowedPoints} color="oklch(0.82 0.12 160)" />
                  </div>
                  <div className="space-y-3 border-t border-border/60 pt-6">
                    <div className="text-[10px] font-bold uppercase tracking-[0.1em] text-muted-foreground/80">Denied / 24h</div>
                    <div className="text-2xl font-bold tracking-tight">{pending ? '0' : '37'}</div>
                    <Sparkline points={deniedPoints} color="oklch(0.83 0.11 55)" />
                  </div>
                </div>
              </div>
            )}
          </div>

          <div className="surface-card overflow-hidden">
            <div className="flex items-center justify-between border-b border-border px-5 py-4">
              <div>
                <div className="text-base font-bold">Protected resources</div>
                <div className="mt-1 text-xs text-muted-foreground">{shieldResources.length} resource{shieldResources.length === 1 ? '' : 's'} fronted by this shield</div>
              </div>
              <Button variant="outline" size="sm" className="h-8 gap-2 rounded-lg text-xs font-bold" asChild>
                <Link to="/resources">
                  <Plus className="h-3 w-3" />
                  Resource
                </Link>
              </Button>
            </div>
            <div className="max-h-[320px] overflow-y-auto">
              {shieldResources.length === 0 ? (
                <div className="flex flex-col items-center justify-center py-12 px-5 text-center">
                  <div className="mb-3 grid h-10 w-10 place-items-center rounded-full border border-border bg-secondary/50 text-muted-foreground/40">
                    <Lock className="h-5 w-5" />
                  </div>
                  <p className="text-xs text-muted-foreground">No resources yet. Add one to route traffic through this shield.</p>
                </div>
              ) : (
                <div className="divide-y divide-border/60">
                  {shieldResources.map(res => (
                    <div key={res.id} className="group flex items-center gap-3 px-5 py-3 transition hover:bg-secondary/40">
                      <div className="grid h-8 w-8 place-items-center rounded-lg bg-blue-500/10 text-blue-500 border border-blue-500/20">
                        <Database className="h-4 w-4" />
                      </div>
                      <div className="flex-1 min-w-0">
                        <div className="text-sm font-bold truncate">{res.name}</div>
                        <div className="text-[11px] font-mono text-muted-foreground truncate">{res.protocol.toLowerCase()}://{res.host}:{res.portFrom}</div>
                      </div>
                      <Link to="/resources" className="opacity-0 group-hover:opacity-100 transition p-1 hover:text-primary">
                        <ChevronRight className="h-4 w-4" />
                      </Link>
                    </div>
                  ))}
                </div>
              )}
            </div>
          </div>
        </div>
      </div>
    </div>
  )
}

