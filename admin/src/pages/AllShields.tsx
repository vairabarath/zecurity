import { useState } from 'react'
import { Link } from 'react-router-dom'
import { useQuery } from '@apollo/client/react'
import { Clock3, Plus } from 'lucide-react'
import {
  GetRemoteNetworksDocument,
  ShieldStatus,
  type GetRemoteNetworksQuery,
} from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { InstallCommandModal } from '@/components/InstallCommandModal'
import { Skeleton } from '@/components/ui/skeleton'
import { EmptyState, EntityIcon, StatusPill, relativeTime } from '@/lib/console'

type NetworkShield = GetRemoteNetworksQuery['remoteNetworks'][number]['shields'][number] & {
  networkId: string
  networkName: string
}

function statusTone(status: ShieldStatus): 'ok' | 'warn' | 'danger' | 'muted' {
  if (status === ShieldStatus.Active) return 'ok'
  if (status === ShieldStatus.Disconnected) return 'warn'
  if (status === ShieldStatus.Revoked) return 'danger'
  return 'muted'
}

export default function AllShields() {
  const [showAdd, setShowAdd] = useState(false)

  const { data, loading } = useQuery(GetRemoteNetworksDocument, {
    fetchPolicy: 'cache-and-network',
    pollInterval: 30000,
  })

  const networks = data?.remoteNetworks ?? []
  const allShields: NetworkShield[] = networks.flatMap((network) =>
    (network.shields ?? []).map((shield) => ({
      ...shield,
      networkId: network.id,
      networkName: network.name,
    })),
  )
  const activeCount = allShields.filter((shield) => shield.status === ShieldStatus.Active).length

  return (
    <div className="space-y-6">
      <div className="page-header">
        <div>
          <h2 className="page-title">Shields</h2>
          <p className="page-subtitle">Host agents enforcing access at the resource edge.</p>
        </div>
        <div className="flex flex-wrap items-center gap-2">
          <span className="status-pill border-[oklch(0.82_0.12_160/0.28)] bg-[oklch(0.82_0.12_160/0.12)] text-[oklch(0.82_0.12_160)]">
            <span className="status-pill-dot bg-[oklch(0.82_0.12_160)]" />
            <span className="font-bold">{activeCount}</span> active
          </span>
          <span className="status-pill border-border bg-secondary text-muted-foreground">
            <span className="font-bold text-foreground">{allShields.length}</span> total
          </span>
          <Button onClick={() => setShowAdd(true)} disabled={networks.length === 0} className="gap-2">
            <Plus className="h-4 w-4" />
            Add Shield
          </Button>
        </div>
      </div>

      <div className="table-shell">
        <div className="table-scroll">
          <div className="table-head grid min-w-[920px] items-center grid-cols-[1.15fr_130px_170px_160px_150px_130px_100px] gap-4 px-5 py-4">
            {['Name', 'Status', 'Network', 'Interface', 'Hostname', 'Last Seen', 'Actions'].map((label, index) => (
              <div key={label + index} className={`table-head-label ${index === 6 ? 'text-right' : ''}`}>{label}</div>
            ))}
          </div>

          {loading && !data ? (
            <div className="min-w-[920px] p-5 space-y-3">
              {Array.from({ length: 5 }).map((_, index) => (
                <Skeleton key={index} className="h-14 rounded-2xl bg-secondary" />
              ))}
            </div>
          ) : allShields.length === 0 ? (
            <EmptyState
              title="No shields enrolled"
              description={
                networks.length === 0
                  ? 'Create a remote network first, then issue a shield install command.'
                  : 'Issue the first shield install command to bring a host under protection.'
              }
              action={
                networks.length === 0 ? (
                  <Link to="/remote-networks" className="text-sm font-semibold text-primary">Create a remote network</Link>
                ) : (
                  <Button onClick={() => setShowAdd(true)}>Add Shield</Button>
                )
              }
            />
          ) : (
            <div className="min-w-[920px]">
              {allShields.map((shield) => (
                <div key={shield.id} className="admin-table-row group grid items-center grid-cols-[1.15fr_130px_170px_160px_150px_130px_100px] gap-4 px-5 py-4">
                  <div className="flex min-w-0 items-center gap-3">
                    <EntityIcon type="shield" />
                    <div className="min-w-0">
                      <div className="truncate text-[15px] font-bold leading-tight">{shield.name}</div>
                      <div className="truncate font-mono text-[11px] font-medium text-muted-foreground">{shield.hostname ?? 'pending'}</div>
                    </div>
                  </div>
                  <div>
                    <StatusPill label={shield.status.toLowerCase()} tone={statusTone(shield.status)} />
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
              ))}
            </div>
          )}
        </div>
      </div>

      <InstallCommandModal open={showAdd} onClose={() => setShowAdd(false)} variant="shield" />
    </div>
  )
}
