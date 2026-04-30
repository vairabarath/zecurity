import { useEffect, useState } from 'react'
import { Link, useLocation, useNavigate } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { AlertCircle, Plus, Users, Wifi, WifiOff } from 'lucide-react'
import {
  GetAllResourcesDocument,
  GetRemoteNetworksDocument,
  ShieldStatus,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { CreateResourceModal } from '@/components/CreateResourceModal'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, EntityIcon, StatusPill, relativeTime } from '@/lib/console'

const transitionalStates = new Set(['managing', 'protecting', 'removing'])

interface ResourcePrefillState {
  createResourceDefaults?: {
    remoteNetworkId: string
    name?: string
    host: string
    protocol: string
    portFrom: number
    portTo: number
  }
}

function resourceTone(status: string): 'ok' | 'warn' | 'danger' | 'muted' | 'info' {
  if (status === 'protected') return 'ok'
  if (status === 'failed') return 'danger'
  if (status === 'protecting' || status === 'managing' || status === 'removing') return 'warn'
  if (status === 'unprotected' || status === 'deleted') return 'muted'
  return 'info'
}

function formatPort(from: number, to: number) {
  return from === to ? `${from}` : `${from}-${to}`
}

export default function Resources() {
  const navigate = useNavigate()
  const location = useLocation()
  const [showAdd, setShowAdd] = useState(false)
  const [prefill, setPrefill] = useState<ResourcePrefillState['createResourceDefaults'] | null>(null)

  const { data: networkData } = useQuery(GetRemoteNetworksDocument)
  const networks = networkData?.remoteNetworks ?? []

  const { data, loading, refetch, startPolling } = useQuery(GetAllResourcesDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const resources = data?.allResources ?? []
  const protectedCount = resources.filter((resource) => resource.status === 'protected').length

  useEffect(() => {
    const hasTransition = resources.some((resource) => transitionalStates.has(resource.status))
    startPolling(hasTransition ? 3000 : 30000)
  }, [resources, startPolling])

  useEffect(() => {
    const state = location.state as ResourcePrefillState | null
    if (!state?.createResourceDefaults) return
    setPrefill(state.createResourceDefaults)
    setShowAdd(true)
    navigate(location.pathname, { replace: true, state: null })
  }, [location.pathname, location.state, navigate])

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Resources</h2>
          <p className="page-subtitle">Managed services protected and relayed through shields.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-[oklch(0.82_0.12_160/0.28)] bg-[oklch(0.82_0.12_160/0.12)] text-[oklch(0.82_0.12_160)]">
            <span className="status-pill-dot bg-[oklch(0.82_0.12_160)]" />
            <span className="font-bold">{protectedCount}</span> protected
          </span>
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">{resources.length}</span> total
          </span>
          <Button onClick={() => setShowAdd(true)} disabled={networks.length === 0} className="gap-2">
            <Plus className="h-4 w-4" />
            Add Resource
          </Button>
        </div>
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-[1200px] items-center grid-cols-[1.3fr_130px_90px_100px_180px_150px_120px_130px_100px] gap-4 px-5 py-4">
            {['Name', 'Host', 'Proto', 'Port', 'Shield', 'Groups', 'Status', 'Last Verified', 'Actions'].map((label, index) => (
              <div key={label + index} className={`table-head-label ${index === 8 ? 'text-right' : ''}`}>{label}</div>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-[1200px] p-5 space-y-3">
              {Array.from({ length: 5 }).map((_, index) => (
                <Skeleton key={index} className="h-14 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : resources.length === 0 ? (
            <EmptyState
              title="No resources defined"
              description={
                networks.length === 0
                  ? 'Create a remote network first, then map a resource onto a shield.'
                  : 'Add the first resource and start protection from the console.'
              }
              action={networks.length > 0 ? <Button onClick={() => setShowAdd(true)}>Add Resource</Button> : undefined}
            />
          ) : (
            <div className="min-w-[1200px]">
              {resources.map((resource) => {
                const shield = resource.shield
                const shieldOffline = resource.shield?.status === ShieldStatus.Disconnected
                const noShield = !shield
                const groups = resource.groups ?? []

                return (
                  <div key={resource.id} className="admin-table-row group grid items-center grid-cols-[1.3fr_130px_90px_100px_180px_150px_120px_130px_100px] gap-4 px-5 py-4">
                    <div className="flex min-w-0 items-center gap-3">
                      <EntityIcon type="resource" />
                      <div className="min-w-0">
                        <div className="truncate text-[15px] font-bold leading-tight">{resource.name}</div>
                        <div className="truncate font-mono text-[11px] font-medium text-muted-foreground">
                          {resource.description || resource.remoteNetwork.name}
                        </div>
                      </div>
                    </div>
                    <div className="font-mono text-[13px] text-muted-foreground">{resource.host}</div>
                    <div className="text-[13px] font-bold uppercase text-muted-foreground">{resource.protocol}</div>
                    <div className="font-mono text-[13px] text-muted-foreground">{formatPort(resource.portFrom, resource.portTo)}</div>
                    <div className="text-sm font-semibold text-primary">
                      {noShield && (
                        <span className="inline-flex items-center gap-1.5 opacity-60">
                          <AlertCircle className="h-3.5 w-3.5" />
                          Unassigned
                        </span>
                      )}
                      {!noShield && shieldOffline && shield && (
                        <span className="inline-flex items-center gap-1.5 text-[oklch(0.85_0.13_80)]">
                          <WifiOff className="h-3.5 w-3.5" />
                          {shield.name}
                        </span>
                      )}
                      {!noShield && !shieldOffline && shield && (
                        <span className="inline-flex items-center gap-1.5 text-[oklch(0.82_0.12_160)]">
                          <Wifi className="h-3.5 w-3.5" />
                          {shield.name}
                        </span>
                      )}
                    </div>
                    <div className="flex flex-wrap gap-1">
                      {groups.length === 0 ? (
                        <span className="inline-flex items-center gap-1 text-[11.5px] text-muted-foreground italic opacity-60">
                          <Users className="h-3 w-3" />
                          none
                        </span>
                      ) : (
                        groups.slice(0, 2).map((g) => (
                          <Link
                            key={g.id}
                            to={`/groups/${g.id}`}
                            className="inline-flex items-center gap-1 rounded-full border border-[oklch(0.85_0.13_80/0.28)] bg-[oklch(0.85_0.13_80/0.10)] px-2 py-0.5 text-[11px] font-semibold text-[oklch(0.85_0.13_80)] transition hover:opacity-80"
                          >
                            {g.name}
                          </Link>
                        ))
                      )}
                      {groups.length > 2 && (
                        <span className="text-[11px] text-muted-foreground">+{groups.length - 2}</span>
                      )}
                    </div>

                    <div>
                      <StatusPill label={resource.status} tone={resourceTone(resource.status)} />
                    </div>
                    <div className="font-mono text-[13px] text-muted-foreground">{relativeTime(resource.lastVerifiedAt)}</div>
                    <div className="text-right">
                      <button
                        onClick={() => navigate(`/resources/${resource.id}`)}
                        className="inline-flex items-center gap-1 text-[13px] font-bold text-primary transition hover:opacity-80"
                      >
                        Manage <span className="transition-transform group-hover:translate-x-0.5">→</span>
                      </button>
                    </div>
                  </div>
                )
              })}
            </div>
          )}
        </div>
      </div>

      <CreateResourceModal
        open={showAdd}
        onOpenChange={(open) => {
          setShowAdd(open)
          if (!open) setPrefill(null)
        }}
        onSuccess={() => refetch()}
        defaults={prefill}
      />
    </div>
  )
}
