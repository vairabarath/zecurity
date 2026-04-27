import { useEffect, useMemo, useRef, useState } from 'react'
import { Link, useNavigate, useParams } from 'react-router-dom'
import { useMutation, useQuery } from '@apollo/client/react'
import {
  ArrowLeft,
  CheckCircle,
  Copy,
  Loader2,
  Plus,
  RefreshCw,
  Server,
  Shield,
  ShieldOff,
  Terminal,
  Trash2,
} from 'lucide-react'
import {
  ConnectorStatus,
  DeleteConnectorDocument,
  DeleteShieldDocument,
  GetRemoteNetworksDocument,
  GetShieldsDocument,
  RevokeConnectorDocument,
  RevokeShieldDocument,
  ShieldStatus,
} from '@/generated/graphql'
import type {
  DeleteConnectorMutationVariables,
  DeleteShieldMutationVariables,
  RevokeConnectorMutationVariables,
  RevokeShieldMutationVariables,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { useAuthStore } from '@/store/auth'
import { cn } from '@/lib/utils'
import { StatusPill, relativeTime } from '@/lib/console'

function Sparkline({ points, color }: { points: number[]; color: string }) {
  const W = 170
  const H = 36
  const max = Math.max(...points, 1)
  const step = W / (points.length - 1)
  const path = points
    .map((value, index) => `${index === 0 ? 'M' : 'L'} ${index * step} ${H - (value / max) * (H - 4) - 2}`)
    .join(' ')

  return (
    <svg viewBox={`0 0 ${W} ${H}`} className="h-9 w-full" preserveAspectRatio="none">
      <path d={path} fill="none" stroke={color} strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" />
    </svg>
  )
}

function MetricCell({
  label,
  value,
  unit,
  points,
  color,
}: {
  label: string
  value: string
  unit?: string
  points: number[]
  color: string
}) {
  return (
    <div className="border-t border-border p-5 first:border-t-0 md:border-l md:first:border-l-0 md:first:border-t-0">
      <div className="text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">{label}</div>
      <div className="mt-2 text-[2rem] font-bold leading-none">
        {value}
        {unit ? <span className="ml-1 text-lg text-muted-foreground">{unit}</span> : null}
      </div>
      <div className="mt-3">
        <Sparkline points={points} color={color} />
      </div>
    </div>
  )
}

function MetadataCell({
  label,
  value,
  accent = false,
}: {
  label: string
  value: React.ReactNode
  accent?: boolean
}) {
  return (
    <div className="border-t border-border p-5 md:border-l md:first:border-l-0 md:[&:nth-child(3n+1)]:border-l-0">
      <div className="text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">{label}</div>
      <div className={cn('mt-3 text-xl font-semibold', accent ? 'text-primary' : 'text-foreground')}>
        {value}
      </div>
    </div>
  )
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
  return 'warn'
}

export default function ConnectorDetail() {
  const { connectorId } = useParams<{ connectorId: string }>()
  const navigate = useNavigate()
  const accessToken = useAuthStore((state) => state.accessToken)

  const { data, loading } = useQuery(GetRemoteNetworksDocument, {
    pollInterval: 10000,
    fetchPolicy: 'cache-and-network',
  })

  const found = data?.remoteNetworks
    .flatMap((network) => network.connectors.map((connector) => ({
      ...connector,
      networkId: network.id,
      networkName: network.name,
    })))
    .find((connector) => connector.id === connectorId)

  const networkId = found?.networkId
  const networkName = found?.networkName ?? 'Network'

  const { data: shieldsData } = useQuery(GetShieldsDocument, {
    variables: { remoteNetworkId: networkId ?? '' },
    skip: !networkId,
    pollInterval: 10000,
  })

  const shields = useMemo(
    () => (shieldsData?.shields ?? []).filter((shield) => shield.connectorId === connectorId),
    [connectorId, shieldsData?.shields],
  )

  const [tokenLoading, setTokenLoading] = useState(false)
  const [tokenError, setTokenError] = useState<string | null>(null)
  const [installCommand, setInstallCommand] = useState<string | null>(null)
  const [copied, setCopied] = useState(false)
  const didFetch = useRef(false)

  const fetchInstallCommand = async () => {
    if (!connectorId || !accessToken) return
    setTokenLoading(true)
    setTokenError(null)
    try {
      const response = await fetch(`/api/connectors/${connectorId}/token`, {
        method: 'POST',
        credentials: 'include',
        headers: { Authorization: `Bearer ${accessToken}` },
      })
      if (!response.ok) {
        const text = await response.text()
        throw new Error(text || 'Failed to generate token')
      }
      const result = await response.json()
      setInstallCommand(result.install_command)
    } catch (error: unknown) {
      setTokenError(error instanceof Error ? error.message : 'Failed to generate token')
    } finally {
      setTokenLoading(false)
    }
  }

  const pending = found?.status === ConnectorStatus.Pending

  useEffect(() => {
    if (found && pending && !didFetch.current) {
      didFetch.current = true
      void fetchInstallCommand()
    }
  }, [found, pending])

  function handleCopy() {
    if (!installCommand) return
    navigator.clipboard.writeText(installCommand)
    setCopied(true)
    setTimeout(() => setCopied(false), 1800)
  }

  const [revokeConnector, { loading: revoking }] = useMutation(RevokeConnectorDocument, {
    refetchQueries: [{ query: GetRemoteNetworksDocument }],
  })
  const [deleteConnector, { loading: deleting }] = useMutation(DeleteConnectorDocument, {
    refetchQueries: [{ query: GetRemoteNetworksDocument }],
    onCompleted: () => navigate('/connectors'),
  })
  const [revokeShield] = useMutation(RevokeShieldDocument, {
    refetchQueries: networkId ? [{ query: GetShieldsDocument, variables: { remoteNetworkId: networkId } }] : [],
  })
  const [deleteShield] = useMutation(DeleteShieldDocument, {
    refetchQueries: networkId ? [{ query: GetShieldsDocument, variables: { remoteNetworkId: networkId } }] : [],
  })

  async function handleRevokeConnector() {
    if (!connectorId) return
    await revokeConnector({ variables: { id: connectorId } as RevokeConnectorMutationVariables })
  }

  async function handleDeleteConnector() {
    if (!connectorId) return
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

  if (loading && !found) {
    return (
      <div className="flex items-center justify-center p-16">
        <div className="flex flex-col items-center gap-3">
          <Loader2 className="h-6 w-6 animate-spin text-primary" />
          <p className="text-xs font-mono tracking-[0.14em] text-muted-foreground">Loading connector...</p>
        </div>
      </div>
    )
  }

  if (!found) {
    return (
      <div className="space-y-6">
        <Link to="/connectors" className="inline-flex items-center gap-2 text-sm text-muted-foreground transition hover:text-foreground">
          <ArrowLeft className="h-4 w-4" />
          Back to Connectors
        </Link>
        <div className="surface-card px-6 py-20 text-center">
          <Server className="mx-auto h-14 w-14 text-destructive/40" />
          <h2 className="mt-4 text-2xl font-bold">Connector not found</h2>
          <p className="mt-2 text-muted-foreground">This connector no longer exists or was deleted.</p>
        </div>
      </div>
    )
  }

  const c = found

  const canRevoke = c.status === ConnectorStatus.Active || c.status === ConnectorStatus.Disconnected
  const canDelete = pending || c.status === ConnectorStatus.Revoked

  const cpuPoints = pending ? [1, 1, 1, 1, 1, 1, 1, 1] : [2.3, 3.1, 2.8, 4.4, 3.2, 3.7, 2.6, 5.1, 4.0, 3.5, 4.3, 3.1]
  const memPoints = pending ? [110, 110, 110, 110, 110, 110, 110, 110] : [130, 138, 136, 144, 142, 145, 148, 149, 146, 147, 147, 142]
  const ingressPoints = pending ? [0, 0, 0, 0, 0, 0, 0, 0] : [3.2, 4.1, 5.0, 4.2, 6.0, 6.8, 5.1, 6.0, 7.7, 6.9, 6.0, 6.8]
  const egressPoints = pending ? [0, 0, 0, 0, 0, 0, 0, 0] : [1.1, 2.0, 2.1, 3.2, 2.0, 3.3, 2.1, 4.4, 3.2, 3.1, 2.0, 2.4]

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Link to="/connectors" className="transition hover:text-foreground">Connectors</Link>
        <span>/</span>
        <span className="text-foreground">{c.name}</span>
      </div>

      <Link to="/connectors" className="inline-flex items-center gap-2 text-sm text-muted-foreground transition hover:text-foreground">
        <ArrowLeft className="h-4 w-4" />
        Back to Connectors
      </Link>

      <div className="flex items-start justify-between gap-4">
        <div className="flex min-w-0 items-start gap-4">
          <div className="grid h-14 w-14 place-items-center rounded-[16px] bg-primary/12 text-primary">
            <Server className="h-7 w-7" />
          </div>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="truncate text-[2.2rem] font-bold tracking-[-0.03em]">{c.name}</h1>
              <StatusPill
                label={c.status === ConnectorStatus.Disconnected ? 'degraded' : c.status === ConnectorStatus.Pending ? 'pending' : c.status.toLowerCase()}
                tone={connectorTone(c.status)}
              />
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-3 text-sm text-muted-foreground">
              <span className="font-mono text-[13px] opacity-70">{c.id}</span>
              <span className="opacity-40">•</span>
              <span className="font-semibold text-primary/80">{networkName}</span>
              <span className="opacity-40">•</span>
              <span>Linux</span>
            </div>
          </div>
        </div>

        <div className="flex items-center gap-3">
          {canRevoke ? (
            <Button variant="outline" onClick={handleRevokeConnector} disabled={revoking} className="gap-2 text-destructive">
              {revoking ? <Loader2 className="h-4 w-4 animate-spin" /> : <ShieldOff className="h-4 w-4" />}
              Revoke
            </Button>
          ) : null}
          {canDelete ? (
            <Button variant="outline" onClick={handleDeleteConnector} disabled={deleting} className="gap-2 text-destructive">
              {deleting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Trash2 className="h-4 w-4" />}
              Delete Connector
            </Button>
          ) : null}
        </div>
      </div>

      {pending ? (
        <div className="rounded-[18px] border border-[oklch(0.85_0.13_80/0.35)] bg-[oklch(0.85_0.13_80/0.08)] px-5 py-4">
          <div className="flex items-start gap-3">
            <div className="grid h-12 w-12 place-items-center rounded-2xl bg-[oklch(0.85_0.13_80/0.15)] text-[oklch(0.85_0.13_80)]">
              <Terminal className="h-5 w-5" />
            </div>
            <div>
              <div className="text-lg font-semibold">Connector registered, not installed</div>
              <p className="mt-1 text-sm text-muted-foreground">
                Run the command below on your server to bring this connector online. It will appear as Active once it checks in.
              </p>
            </div>
          </div>
        </div>
      ) : null}

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
            <div className="grid grid-cols-1 md:grid-cols-3">
              <MetadataCell label="Name" value={c.name} />
              <MetadataCell label="Status" value={<StatusPill label={c.status === ConnectorStatus.Disconnected ? 'degraded' : c.status.toLowerCase()} tone={connectorTone(c.status)} />} />
              <MetadataCell label="Version" value={c.version ?? '—'} />
              <MetadataCell label="Last Seen" value={relativeTime(c.lastSeenAt)} />
              <MetadataCell label="Remote Network" value={<Link to={`/remote-networks/${networkId}`} className="text-primary">{networkName}</Link>} accent />
              <MetadataCell label="Hostname" value={c.hostname ?? '—'} />
              <MetadataCell label="Public IP" value={c.publicIp ?? '—'} />
              <MetadataCell label="LAN IP" value={c.lanAddr ? c.lanAddr.split(':')[0] : '—'} />
              <MetadataCell label="Cert Expires" value={c.certNotAfter ? new Date(c.certNotAfter).toLocaleString() : '—'} />
              <MetadataCell label="Created" value={new Date(c.createdAt).toLocaleString()} />
            </div>
          </div>
        </div>

        <div className="space-y-4">
          <div className="surface-card overflow-hidden">
            <div className="flex items-center gap-4 border-b border-border px-5 py-6">
              <div className="grid h-14 w-14 place-items-center rounded-2xl bg-primary/14 text-primary">
                <Shield className="h-7 w-7" />
              </div>
              <div>
                <div className="text-xs font-semibold uppercase tracking-[0.08em] text-muted-foreground">Health</div>
                <div className="mt-1 text-[2rem] font-bold leading-none">
                  {pending ? 'Awaiting install' : c.status === ConnectorStatus.Disconnected ? 'Degraded' : 'Operational'}
                </div>
                <div className="mt-2 text-sm text-muted-foreground">
                  {pending ? 'Never reported in' : `Up ${relativeTime(c.lastSeenAt).replace('ago', '').trim()} · last check-in ${relativeTime(c.lastSeenAt)}`}
                </div>
              </div>
            </div>

            <div className="grid grid-cols-1 md:grid-cols-2">
              <MetricCell label="CPU" value={pending ? '—' : '3.1'} unit="%" points={cpuPoints} color="oklch(0.86 0.095 175)" />
              <MetricCell label="Memory" value={pending ? '—' : '142'} unit="MB" points={memPoints} color="oklch(0.86 0.095 175)" />
              <MetricCell label="Ingress" value={pending ? '—' : '6.8'} unit="MB/s" points={ingressPoints} color="oklch(0.78 0.10 235)" />
              <MetricCell label="Egress" value={pending ? '—' : '2.4'} unit="MB/s" points={egressPoints} color="oklch(0.78 0.09 310)" />
            </div>
          </div>

          <div className="surface-card overflow-hidden">
            <div className="flex items-start justify-between border-b border-border px-5 py-4">
              <div>
                <div className="text-[1.15rem] font-bold">Shields</div>
                <div className="mt-1 text-sm text-muted-foreground">{shields.length} resource{shields.length === 1 ? '' : 's'} protected</div>
              </div>
              <Button size="sm" className="gap-2">
                <Plus className="h-4 w-4" />
                Shield
              </Button>
            </div>

            {shields.length === 0 ? (
              <div className="px-5 py-10 text-center text-sm text-muted-foreground">
                No shields yet. Attach one to start protecting resources behind this connector.
              </div>
            ) : (
              <div className="space-y-3 p-5">
                {shields.map((shield) => {
                  const canRevokeShield = shield.status === ShieldStatus.Active || shield.status === ShieldStatus.Disconnected
                  const canDeleteShield = shield.status === ShieldStatus.Pending || shield.status === ShieldStatus.Revoked
                  return (
                    <div key={shield.id} className="rounded-2xl border border-border bg-secondary px-4 py-3">
                      <div className="flex items-start justify-between gap-3">
                        <div className="min-w-0">
                          <div className="flex flex-wrap items-center gap-2">
                            <Link to={`/shields/${shield.id}`} className="text-base font-semibold text-foreground transition hover:text-primary">
                              {shield.name}
                            </Link>
                            <StatusPill label={shield.status.toLowerCase()} tone={shieldTone(shield.status)} />
                          </div>
                          <div className="mt-2 flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
                            <span>{shield.hostname ?? shield.interfaceAddr ?? 'shield'}</span>
                            {shield.interfaceAddr ? (
                              <>
                                <span>•</span>
                                <span>{shield.interfaceAddr}</span>
                              </>
                            ) : null}
                          </div>
                        </div>
                        <div className="flex items-center gap-2">
                          {canRevokeShield ? (
                            <Button variant="outline" size="sm" onClick={() => handleRevokeShield(shield.id, shield.name)} className="gap-1.5">
                              <ShieldOff className="h-3.5 w-3.5" />
                              Revoke
                            </Button>
                          ) : null}
                          {canDeleteShield ? (
                            <Button variant="outline" size="sm" onClick={() => handleDeleteShield(shield.id, shield.name)}>
                              <Trash2 className="h-3.5 w-3.5" />
                            </Button>
                          ) : null}
                        </div>
                      </div>
                    </div>
                  )
                })}
              </div>
            )}
          </div>

          {pending && installCommand ? (
            <div className="surface-card overflow-hidden">
              <div className="border-b border-border px-5 py-4">
                <div className="text-[1.15rem] font-bold">Enrollment token</div>
                <div className="mt-1 text-sm text-muted-foreground">Single-use · expires in 24h</div>
              </div>
              <div className="px-5 py-4 font-mono text-sm text-primary">{installCommand.match(/ENROLLMENT_TOKEN=([^\s\\]+)/)?.[1] ?? 'embedded in install command'}</div>
            </div>
          ) : null}
        </div>
      </div>
    </div>
  )
}
