import { useState, useEffect } from 'react'
import { useParams, Link, useNavigate } from 'react-router-dom'
import { useQuery, useMutation } from '@apollo/client/react'
import {
  GetAllResourcesDocument,
  GetShieldsDocument,
  ProtectResourceDocument,
  UnprotectResourceDocument,
  DeleteResourceDocument,
  ShieldStatus,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { EditResourceModal } from '@/components/EditResourceModal'
import { StatusPill, relativeTime } from '@/lib/console'
import { cn } from '@/lib/utils'
import {
  ArrowLeft,
  Box,
  Shield,
  ShieldOff,
  Pencil,
  Trash2,
  Loader2,
  Globe,
  Server,
  Clock,
  Hash,
  Wifi,
  Activity,
  FileText,
  Plus,
  CheckCircle2,
  AlertTriangle,
} from 'lucide-react'
import { toast } from 'sonner'

function MetaCell({
  label,
  value,
  icon,
  mono,
  span2,
}: {
  label: string
  value: React.ReactNode
  icon?: React.ReactNode
  mono?: boolean
  span2?: boolean
}) {
  return (
    <div className={cn('border-b border-border px-5 py-4', span2 ? 'col-span-3 border-r-0' : 'border-r last:border-r-0')}>
      <div className="flex items-center gap-1.5 text-[10px] font-bold uppercase tracking-[0.08em] text-muted-foreground/70">
        {icon}
        {label}
      </div>
      <div className={cn('mt-1.5 text-[14px] font-semibold text-foreground', mono && 'font-mono text-[13px] text-muted-foreground')}>
        {value ?? '—'}
      </div>
    </div>
  )
}

function resourceTone(status: string): 'ok' | 'warn' | 'danger' | 'muted' | 'info' {
  if (status === 'protected') return 'ok'
  if (status === 'failed') return 'danger'
  if (status === 'protecting' || status === 'managing' || status === 'removing') return 'warn'
  return 'muted'
}

function formatPort(from: number, to: number) {
  return from === to ? `${from}` : `${from}–${to}`
}

export default function ResourceDetail() {
  const { resourceId } = useParams<{ resourceId: string }>()
  const navigate = useNavigate()
  const [editOpen, setEditOpen] = useState(false)
  const [selectedShieldId, setSelectedShieldId] = useState<string>('')

  const { data, loading, refetch, startPolling } = useQuery(GetAllResourcesDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 10000,
  })

  const resource = data?.allResources.find((r) => r.id === resourceId)

  const { data: shieldsData } = useQuery(GetShieldsDocument, {
    variables: { remoteNetworkId: resource?.remoteNetwork.id ?? '' },
    skip: !resource?.remoteNetwork.id,
    fetchPolicy: 'cache-and-network',
  })

  const shields = shieldsData?.shields ?? []
  const candidateShields = shields.filter((s) => s.status !== ShieldStatus.Revoked)

  useEffect(() => {
    if (candidateShields.length > 0 && !selectedShieldId) {
      setSelectedShieldId(candidateShields[0].id)
    }
  }, [candidateShields, selectedShieldId])

  useEffect(() => {
    if (!resource) return
    const transitional = ['managing', 'protecting', 'removing'].includes(resource.status)
    startPolling(transitional ? 3000 : 10000)
  }, [resource, startPolling])

  const [protectResource, { loading: protecting }] = useMutation(ProtectResourceDocument, {
    onCompleted: () => {
      toast.success('Protection started')
      void refetch()
    },
    onError: (e) => toast.error(e.message),
  })

  const [unprotectResource, { loading: unprotecting }] = useMutation(UnprotectResourceDocument, {
    onCompleted: () => {
      toast.success('Resource unprotected')
      void refetch()
    },
    onError: (e) => toast.error(e.message),
  })

  const [deleteResource, { loading: deleting }] = useMutation(DeleteResourceDocument, {
    onCompleted: () => {
      toast.success('Resource deleted')
      navigate('/resources')
    },
    onError: (e) => toast.error(e.message),
  })

  function handleProtect() {
    if (!resourceId) return
    void protectResource({ variables: { id: resourceId } })
  }

  function handleUnprotect() {
    if (!resourceId) return
    if (!window.confirm('Remove protection from this resource?')) return
    void unprotectResource({ variables: { id: resourceId } })
  }

  function handleDelete() {
    if (!resourceId) return
    if (!window.confirm('Delete this resource? This cannot be undone.')) return
    void deleteResource({ variables: { id: resourceId } })
  }

  if (loading && !resource) {
    return (
      <div className="flex items-center justify-center p-16">
        <div className="flex flex-col items-center gap-3">
          <Loader2 className="h-6 w-6 animate-spin text-primary" />
          <p className="font-mono text-xs tracking-[0.14em] text-muted-foreground">Loading resource...</p>
        </div>
      </div>
    )
  }

  if (!resource) {
    return (
      <div className="space-y-6">
        <Link to="/resources" className="inline-flex items-center gap-2 text-sm text-muted-foreground transition hover:text-foreground">
          <ArrowLeft className="h-4 w-4" />
          Back to Resources
        </Link>
        <div className="surface-card px-6 py-20 text-center">
          <Box className="mx-auto h-14 w-14 text-destructive/40" />
          <h2 className="mt-4 text-2xl font-bold">Resource not found</h2>
          <p className="mt-2 text-muted-foreground">This resource no longer exists or was deleted.</p>
        </div>
      </div>
    )
  }

  const isProtected = resource.status === 'protected'
  const shield = resource.shield
  const transitional = ['managing', 'protecting', 'removing'].includes(resource.status)

  return (
    <div className="space-y-6">
      <Link to="/resources" className="inline-flex items-center gap-2 text-sm text-muted-foreground transition hover:text-foreground">
        <ArrowLeft className="h-4 w-4" />
        Back to Resources
      </Link>

      {/* Header */}
      <div className="flex items-start justify-between gap-4">
        <div className="flex min-w-0 items-center gap-4">
          <div className="grid h-12 w-12 place-items-center rounded-2xl bg-[oklch(0.78_0.09_310/0.16)] text-[oklch(0.78_0.09_310)]">
            <Box className="h-6 w-6" />
          </div>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-3">
              <h1 className="truncate text-[2.2rem] font-bold tracking-[-0.03em]">{resource.name}</h1>
              <StatusPill label={resource.status} tone={resourceTone(resource.status)} />
            </div>
            <div className="mt-2 flex flex-wrap items-center gap-2 text-sm text-muted-foreground">
              <span className="font-mono text-[12px] opacity-60">{resource.id}</span>
              <span className="opacity-40">·</span>
              <div className="flex items-center gap-1.5 rounded-lg bg-[oklch(0.86_0.095_175/0.12)] px-2 py-0.5 text-xs font-bold text-[oklch(0.86_0.095_175)]">
                <Globe className="h-3 w-3" />
                {resource.remoteNetwork.name}
              </div>
              {shield && (
                <>
                  <span className="opacity-40">→</span>
                  <div className="flex items-center gap-1.5 rounded-lg bg-secondary px-2 py-0.5 text-xs font-bold">
                    <Shield className="h-3 w-3" />
                    {shield.name}
                  </div>
                </>
              )}
            </div>
          </div>
        </div>

        <div className="flex shrink-0 items-center gap-2">
          {!isProtected && !transitional && (
            <Button
              onClick={handleProtect}
              disabled={protecting || candidateShields.length === 0}
              className="gap-2"
              variant="outline"
            >
              {protecting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Shield className="h-4 w-4" />}
              Protect this resource
            </Button>
          )}
          {isProtected && (
            <Button
              onClick={handleUnprotect}
              disabled={unprotecting}
              variant="outline"
              className="gap-2 text-destructive/80 hover:bg-destructive/5 hover:text-destructive"
            >
              {unprotecting ? <Loader2 className="h-4 w-4 animate-spin" /> : <ShieldOff className="h-4 w-4" />}
              Unprotect
            </Button>
          )}
          <Button variant="ghost" size="sm" onClick={() => setEditOpen(true)} className="gap-2">
            <Pencil className="h-4 w-4" />
            Edit
          </Button>
          <Button
            variant="ghost"
            size="sm"
            onClick={handleDelete}
            disabled={deleting}
            className="gap-2 text-destructive/80 hover:bg-destructive/5 hover:text-destructive"
          >
            {deleting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Trash2 className="h-4 w-4" />}
            Delete
          </Button>
        </div>
      </div>

      {/* Protection hero */}
      {isProtected ? (
        <div className="relative overflow-hidden rounded-2xl border border-[oklch(0.82_0.12_160/0.25)] bg-[oklch(0.82_0.12_160/0.07)] p-5">
          <div className="flex items-center gap-4">
            <div className="grid h-12 w-12 shrink-0 place-items-center rounded-2xl bg-[oklch(0.82_0.12_160/0.20)] text-[oklch(0.82_0.12_160)] shadow-[0_0_20px_oklch(0.82_0.12_160/0.20)]">
              <Shield className="h-6 w-6" />
            </div>
            <div className="flex-1 min-w-0">
              <div className="text-[10px] font-bold uppercase tracking-[0.1em] text-[oklch(0.82_0.12_160)]">Protected</div>
              <div className="text-lg font-bold">
                Enforced by{' '}
                <Link to={`/shields/${shield?.id}`} className="text-[oklch(0.82_0.12_160)] hover:opacity-80 transition">
                  {shield?.name}
                </Link>
              </div>
              <div className="text-sm text-muted-foreground">
                Access is gated by zero-trust policies · last verified {relativeTime(resource.lastVerifiedAt)}.
              </div>
            </div>
            <Button variant="ghost" size="sm" className="shrink-0 gap-1.5 text-muted-foreground hover:text-foreground">
              <FileText className="h-4 w-4" />
              View policies
            </Button>
          </div>
        </div>
      ) : (
        <div className="relative overflow-hidden rounded-2xl border border-[oklch(0.85_0.13_80/0.25)] bg-[oklch(0.85_0.13_80/0.07)] p-5">
          <div className="flex items-center gap-4">
            <div className="grid h-12 w-12 shrink-0 place-items-center rounded-2xl bg-[oklch(0.85_0.13_80/0.18)] text-[oklch(0.85_0.13_80)]">
              <Shield className="h-6 w-6" />
            </div>
            <div className="flex-1 min-w-0">
              <div className="text-[10px] font-bold uppercase tracking-[0.1em] text-[oklch(0.85_0.13_80)]">Unprotected</div>
              <div className="text-lg font-bold">No shield is enforcing this resource</div>
              <div className="text-sm text-muted-foreground">
                {candidateShields.length > 0 ? (
                  <>
                    A shield is available on{' '}
                    <code className="rounded bg-secondary px-1 font-mono text-xs">{resource.host}</code>
                    . Click <strong>Protect this resource</strong> above to enable enforcement.
                  </>
                ) : (
                  <>
                    Install a shield on{' '}
                    <code className="rounded bg-secondary px-1 font-mono text-xs">{resource.host}</code>{' '}
                    in {resource.remoteNetwork.name} to start enforcing access.
                  </>
                )}
              </div>
            </div>
            {candidateShields.length > 0 && (
              <Button onClick={handleProtect} disabled={protecting} className="shrink-0 gap-2">
                {protecting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Shield className="h-4 w-4" />}
                Protect
              </Button>
            )}
          </div>
        </div>
      )}

      {/* KPI row (protected only) */}
      {isProtected && (
        <div className="grid grid-cols-2 gap-4 sm:grid-cols-4">
          {[
            { label: 'Open sessions', value: '—', icon: <Wifi className="h-4 w-4" />, color: 'bg-blue-500/12 text-blue-500' },
            { label: 'Sessions / 24h', value: '—', icon: <Activity className="h-4 w-4" />, color: 'bg-emerald-500/12 text-emerald-500' },
            { label: 'Policies', value: '0', icon: <FileText className="h-4 w-4" />, color: 'bg-orange-500/12 text-orange-500' },
            {
              label: 'Endpoint',
              value: `${resource.protocol.toUpperCase()}:${formatPort(resource.portFrom, resource.portTo)}`,
              icon: <Shield className="h-4 w-4" />,
              color: 'bg-[oklch(0.78_0.09_310/0.12)] text-[oklch(0.78_0.09_310)]',
            },
          ].map((kpi) => (
            <div key={kpi.label} className="surface-card flex items-center gap-4 p-5">
              <div className={cn('grid h-10 w-10 shrink-0 place-items-center rounded-xl', kpi.color)}>
                {kpi.icon}
              </div>
              <div className="min-w-0">
                <div className="truncate text-2xl font-bold tracking-tight">{kpi.value}</div>
                <div className="text-xs font-medium text-muted-foreground">{kpi.label}</div>
              </div>
            </div>
          ))}
        </div>
      )}

      {/* Transitional state warning */}
      {transitional && (
        <div className="flex items-center gap-3 rounded-2xl border border-[oklch(0.85_0.13_80/0.25)] bg-[oklch(0.85_0.13_80/0.07)] px-5 py-4">
          <Loader2 className="h-4 w-4 animate-spin text-[oklch(0.85_0.13_80)]" />
          <span className="text-sm font-medium capitalize text-[oklch(0.85_0.13_80)]">{resource.status}…</span>
          {resource.errorMessage && (
            <span className="ml-2 text-sm text-destructive">{resource.errorMessage}</span>
          )}
        </div>
      )}

      {/* Main grid */}
      <div className="grid gap-4 xl:grid-cols-[minmax(0,1.35fr)_minmax(320px,0.95fr)]">

        {/* Left: Config + Tags */}
        <div className="space-y-4">
          <div className="surface-card overflow-hidden">
            <div className="border-b border-border px-5 py-4">
              <div className="text-[1.1rem] font-bold">Configuration</div>
              <div className="mt-0.5 text-sm text-muted-foreground">Resource identity & endpoint</div>
            </div>
            <div className="grid grid-cols-3">
              <MetaCell
                label="Name"
                value={resource.name}
                icon={<Hash className="h-3 w-3" />}
              />
              <MetaCell
                label="Status"
                value={<StatusPill label={resource.status} tone={resourceTone(resource.status)} />}
                icon={<Wifi className="h-3 w-3" />}
              />
              <MetaCell
                label="Remote Network"
                value={
                  <Link to={`/remote-networks/${resource.remoteNetwork.id}`} className="text-[oklch(0.86_0.095_175)] hover:opacity-80 transition">
                    {resource.remoteNetwork.name}
                  </Link>
                }
                icon={<Globe className="h-3 w-3" />}
              />
              <MetaCell
                label="Shield"
                value={
                  shield ? (
                    <Link to={`/shields/${shield.id}`} className="text-[oklch(0.78_0.10_235)] hover:opacity-80 transition">
                      {shield.name}
                    </Link>
                  ) : (
                    <span className="text-muted-foreground">—</span>
                  )
                }
                icon={<Shield className="h-3 w-3" />}
              />
              <MetaCell
                label="Host IP"
                value={resource.host}
                icon={<Server className="h-3 w-3" />}
                mono
              />
              <MetaCell
                label="Protocol"
                value={
                  <span className="inline-flex items-center rounded-md border border-border bg-secondary px-2 py-0.5 text-[12px] font-bold uppercase tracking-wider">
                    {resource.protocol}
                  </span>
                }
                icon={<Globe className="h-3 w-3" />}
              />
              <MetaCell
                label="Port"
                value={formatPort(resource.portFrom, resource.portTo)}
                icon={<Globe className="h-3 w-3" />}
                mono
              />
              <MetaCell
                label="Last Verified"
                value={relativeTime(resource.lastVerifiedAt)}
                icon={<Clock className="h-3 w-3" />}
              />
              <MetaCell label="" value="" />
              <MetaCell
                label="Created"
                value={new Date(resource.createdAt).toLocaleDateString('en-US', {
                  month: 'short',
                  day: 'numeric',
                  year: 'numeric',
                }) + ' · ' + new Date(resource.createdAt).toLocaleTimeString('en-US', { hour: '2-digit', minute: '2-digit' })}
                icon={<Clock className="h-3 w-3" />}
                span2
              />
              {resource.description && (
                <MetaCell
                  label="Description"
                  value={resource.description}
                  icon={<Hash className="h-3 w-3" />}
                  span2
                />
              )}
            </div>
          </div>

          <div className="surface-card overflow-hidden">
            <div className="border-b border-border px-5 py-4">
              <div className="text-[1.1rem] font-bold">Tags</div>
              <div className="mt-0.5 text-sm text-muted-foreground">Used for policy targeting</div>
            </div>
            <div className="px-5 py-4">
              <p className="text-sm text-muted-foreground">No tags assigned.</p>
            </div>
          </div>
        </div>

        {/* Right: Protection panel + Access policies */}
        <div className="space-y-4">
          <div className="surface-card overflow-hidden">
            <div className="border-b border-border px-5 py-4">
              <div className="text-[1.1rem] font-bold">Protection</div>
              <div className="mt-0.5 text-sm text-muted-foreground">How this resource is secured</div>
            </div>
            <div className="p-4">
              {isProtected && shield ? (
                <div className="flex items-center gap-3 rounded-xl border border-[oklch(0.82_0.12_160/0.28)] bg-[oklch(0.82_0.12_160/0.08)] px-4 py-3">
                  <div className="grid h-9 w-9 shrink-0 place-items-center rounded-xl bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
                    <Shield className="h-4 w-4" />
                  </div>
                  <div className="flex-1 min-w-0">
                    <div className="text-sm font-bold">{shield.name}</div>
                    <div className="text-[11px] font-mono text-muted-foreground">{shield.lanIp}</div>
                  </div>
                  <span className="flex items-center gap-1.5 rounded-full border border-[oklch(0.82_0.12_160/0.28)] bg-[oklch(0.82_0.12_160/0.12)] px-2.5 py-0.5 text-[11px] font-bold text-[oklch(0.82_0.12_160)]">
                    <span className="h-1.5 w-1.5 rounded-full bg-[oklch(0.82_0.12_160)]" />
                    Enforcing
                  </span>
                </div>
              ) : candidateShields.length > 0 ? (
                <>
                  <p className="mb-3 text-xs text-muted-foreground">
                    Shields available on this host. Select one and click <strong>Protect this resource</strong> to enable enforcement:
                  </p>
                  <div className="space-y-2">
                    {candidateShields.map((s) => (
                      <button
                        key={s.id}
                        onClick={() => setSelectedShieldId(s.id)}
                        className={cn(
                          'flex w-full items-center gap-3 rounded-xl border px-4 py-3 text-left transition',
                          selectedShieldId === s.id
                            ? 'border-primary/40 bg-primary/5'
                            : 'border-border bg-secondary/40 hover:border-border/80 hover:bg-secondary/60'
                        )}
                      >
                        <div className="grid h-9 w-9 shrink-0 place-items-center rounded-xl bg-[oklch(0.78_0.10_235/0.16)] text-[oklch(0.78_0.10_235)]">
                          <Shield className="h-4 w-4" />
                        </div>
                        <div className="flex-1 min-w-0">
                          <div className="text-sm font-bold">{s.name}</div>
                          <div className="text-[11px] font-mono text-muted-foreground">
                            {s.hostname ?? 'unknown'} · {s.lastSeenAt ? relativeTime(s.lastSeenAt) : 'never seen'}
                          </div>
                        </div>
                        {selectedShieldId === s.id && (
                          <CheckCircle2 className="h-4 w-4 shrink-0 text-primary" />
                        )}
                      </button>
                    ))}
                  </div>
                  <Button onClick={handleProtect} disabled={protecting} className="mt-3 w-full gap-2">
                    {protecting ? <Loader2 className="h-4 w-4 animate-spin" /> : <Shield className="h-4 w-4" />}
                    Protect with {candidateShields.find((s) => s.id === selectedShieldId)?.name ?? candidateShields[0]?.name}
                  </Button>
                </>
              ) : (
                <div className="flex flex-col items-center justify-center py-8 text-center">
                  <AlertTriangle className="mb-3 h-8 w-8 text-muted-foreground/30" />
                  <p className="text-sm font-medium text-muted-foreground">No shield installed on this host</p>
                  <p className="mt-1 text-xs text-muted-foreground">
                    Install a shield on <code className="rounded bg-secondary px-1 font-mono">{resource.host}</code> in {resource.remoteNetwork.name}.
                  </p>
                </div>
              )}
            </div>
          </div>

          <div className="surface-card overflow-hidden">
            <div className="flex items-center justify-between border-b border-border px-5 py-4">
              <div>
                <div className="text-[1.1rem] font-bold">Access policies</div>
                <div className="mt-0.5 text-sm text-muted-foreground">No policies assigned yet</div>
              </div>
              <Button size="sm" className="h-8 gap-1.5 rounded-lg text-xs font-bold">
                <Plus className="h-3 w-3" />
                Policy
              </Button>
            </div>
            <div className="flex flex-col items-center justify-center px-5 py-10 text-center">
              <p className="text-sm text-muted-foreground">Define a policy to control who can reach this resource.</p>
            </div>
          </div>
        </div>
      </div>

      <EditResourceModal
        resource={resource}
        open={editOpen}
        onOpenChange={setEditOpen}
        onSuccess={() => void refetch()}
      />
    </div>
  )
}
