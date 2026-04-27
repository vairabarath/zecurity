import { useState } from 'react'
import { Link, useParams } from 'react-router-dom'
import { useMutation, useQuery } from '@apollo/client/react'
import { ChevronDown, ChevronRight, Clock3, Plus, ShieldOff, Trash2 } from 'lucide-react'
import {
  DeleteShieldDocument,
  GetRemoteNetworkDocument,
  GetShieldsDocument,
  RevokeShieldDocument,
  ShieldStatus,
  type DeleteShieldMutationVariables,
  type RevokeShieldMutationVariables,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { InstallCommandModal } from '@/components/InstallCommandModal'
import { DiscoveredServicesPanel } from '@/components/DiscoveredServicesPanel'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, EntityIcon, StatusPill, relativeTime } from '@/lib/console'

function statusTone(status: ShieldStatus): 'ok' | 'warn' | 'danger' | 'muted' {
  if (status === ShieldStatus.Active) return 'ok'
  if (status === ShieldStatus.Disconnected) return 'warn'
  if (status === ShieldStatus.Revoked) return 'danger'
  return 'muted'
}

function truncateId(id: string | null | undefined) {
  if (!id) return 'pending'
  return id.length > 12 ? `${id.slice(0, 8)}...` : id
}

export default function Shields() {
  const { id } = useParams<{ id: string }>()
  const [showInstall, setShowInstall] = useState(false)
  const [expanded, setExpanded] = useState<Set<string>>(new Set())

  function toggleExpanded(shieldId: string) {
    setExpanded((prev) => {
      const next = new Set(prev)
      if (next.has(shieldId)) next.delete(shieldId)
      else next.add(shieldId)
      return next
    })
  }

  const { data: networkData } = useQuery(GetRemoteNetworkDocument, {
    variables: { id: id! },
    skip: !id,
  })

  const { data, loading } = useQuery(GetShieldsDocument, {
    variables: { remoteNetworkId: id! },
    skip: !id,
    pollInterval: 30000,
  })

  const [revokeShield] = useMutation(RevokeShieldDocument, {
    refetchQueries: id ? [{ query: GetShieldsDocument, variables: { remoteNetworkId: id } }] : [],
  })
  const [deleteShield] = useMutation(DeleteShieldDocument, {
    refetchQueries: id ? [{ query: GetShieldsDocument, variables: { remoteNetworkId: id } }] : [],
  })

  async function handleRevoke(shieldId: string, shieldName: string) {
    if (!window.confirm(`Revoke shield "${shieldName}"?`)) return
    await revokeShield({ variables: { id: shieldId } as RevokeShieldMutationVariables })
  }

  async function handleDelete(shieldId: string, shieldName: string) {
    if (!window.confirm(`Delete shield "${shieldName}"? This cannot be undone.`)) return
    await deleteShield({ variables: { id: shieldId } as DeleteShieldMutationVariables })
  }

  const networkName = networkData?.remoteNetwork?.name ?? 'Network'
  const shields = data?.shields ?? []

  return (
    <div className="space-y-6">
      <div className="flex items-center gap-2 text-sm text-muted-foreground">
        <Link to="/remote-networks" className="transition hover:text-foreground">Remote Networks</Link>
        <ChevronRight className="h-4 w-4" />
        <span>{networkName}</span>
        <ChevronRight className="h-4 w-4" />
        <span className="text-foreground">Shields</span>
      </div>

      <div className="page-header">
        <div>
          <h2 className="page-title">Shields</h2>
          <p className="page-subtitle">Shield agents currently assigned to {networkName}.</p>
        </div>
        <Button onClick={() => setShowInstall(true)} className="gap-2">
          <Plus className="h-4 w-4" />
          Add Shield
        </Button>
      </div>

      {id ? (
        <InstallCommandModal remoteNetworkId={id} variant="shield" open={showInstall} onClose={() => setShowInstall(false)} />
      ) : null}

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-[980px] grid-cols-[36px_1.15fr_130px_160px_160px_130px_110px_170px] gap-4 px-5 py-3">
            {['', 'Name', 'Status', 'Interface', 'Via Connector', 'Last Seen', 'Version', 'Actions'].map((label, index) => (
              <div key={label + index} className={`table-head-label ${index === 7 ? 'text-right' : ''}`}>{label}</div>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-[980px] p-5 space-y-3">
              {Array.from({ length: 4 }).map((_, index) => (
                <Skeleton key={index} className="h-14 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : shields.length === 0 ? (
            <EmptyState
              title="No shields for this network"
              description="Generate the first shield install command to bring a host under protection."
              action={<Button onClick={() => setShowInstall(true)}>Add Shield</Button>}
            />
          ) : (
            <div className="min-w-[980px]">
              {shields.map((shield) => {
                const canRevoke = shield.status === ShieldStatus.Active || shield.status === ShieldStatus.Disconnected
                const canDelete = shield.status === ShieldStatus.Pending || shield.status === ShieldStatus.Revoked

                const isExpanded = expanded.has(shield.id)

                return (
                  <div key={shield.id}>
                  <div className="admin-table-row grid grid-cols-[36px_1.15fr_130px_160px_160px_130px_110px_170px] gap-4 px-5 py-4">
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
                        <Link to={`/shields/${shield.id}`} className="block truncate text-sm font-semibold transition hover:text-primary">
                          {shield.name}
                        </Link>
                        <div className="truncate text-xs text-muted-foreground">{shield.hostname ?? 'hostname unavailable'}</div>
                      </div>
                    </div>
                    <div><StatusPill label={shield.status.toLowerCase()} tone={statusTone(shield.status)} /></div>
                    <div className="text-sm text-muted-foreground">{shield.interfaceAddr ?? '—'}</div>
                    <div className="text-sm text-muted-foreground">{truncateId(shield.connectorId)}</div>
                    <div className="text-sm text-muted-foreground">
                      <span className="inline-flex items-center gap-1">
                        <Clock3 className="h-3.5 w-3.5" />
                        {relativeTime(shield.lastSeenAt)}
                      </span>
                    </div>
                    <div className="text-sm text-muted-foreground">{shield.version ?? '—'}</div>
                    <div className="flex items-center justify-end gap-2">
                      <Link to={`/shields/${shield.id}`} className="text-sm font-semibold text-primary">Manage</Link>
                      {canRevoke ? (
                        <button
                          onClick={() => handleRevoke(shield.id, shield.name)}
                          className="rounded-xl border border-[oklch(0.85_0.13_80/0.28)] bg-[oklch(0.85_0.13_80/0.12)] p-2 text-[oklch(0.85_0.13_80)]"
                        >
                          <ShieldOff className="h-4 w-4" />
                        </button>
                      ) : null}
                      {canDelete ? (
                        <button
                          onClick={() => handleDelete(shield.id, shield.name)}
                          className="rounded-xl border border-[oklch(0.75_0.16_25/0.28)] bg-[oklch(0.75_0.16_25/0.12)] p-2 text-[oklch(0.75_0.16_25)]"
                        >
                          <Trash2 className="h-4 w-4" />
                        </button>
                      ) : null}
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
    </div>
  )
}
