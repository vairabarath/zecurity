import { useState, type ReactNode } from 'react'
import { ArrowLeft, RefreshCw } from 'lucide-react'
import { Link, Navigate, useNavigate, useParams } from 'react-router-dom'
import { getRelayPool } from '@/api/reads'
import type { RelayPoolDetail as RelayPoolDetailDTO } from '@/api/types'
import { hasRole, PROVIDER_ROLES } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { DefaultPoolBadge, HOLD_DOWN_NOTE, PoolStateBadge, RegionCode, ResidencyBadge } from '@/components/PoolBadges'
import { DeletePoolDialog, PlacementDialog, RenamePoolDialog } from '@/components/PoolDialogs'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { StatusBadge } from '@/components/StatusBadge'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { formatDateTime } from '@/lib/format'
import { poolStranded } from '@/lib/pool-format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Relay pool detail (D-37 … D-41; relay.read: both roles). One request when
// the page opens; Refresh and Retry are the only other reads. The tenant list
// is in the response only for super-admins (tenant_list is null for
// relay-ops, who see the count only, D-41); this page shows exactly what the
// API sent. Writes: rename and delete (both roles), region and residency
// (super-admin only); none for the default pool. Hiding them is UX only; the
// controller authorizes every request.

type Edit = 'rename' | 'placement' | 'delete'

const back = (
  <Link to="/relay-pools" className="inline-flex items-center gap-1 self-start text-[13px] text-muted-foreground hover:text-primary">
    <ArrowLeft className="size-4" aria-hidden />
    Back to relay pools
  </Link>
)

export function RelayPoolDetail() {
  const { id = '' } = useParams()
  const { state, reload } = useDetailQuery(id, getRelayPool)
  const [notice, setNotice] = useState<(RelayActionNotice & { id: string }) | null>(null)

  switch (state.status) {
    case 'loading':
      return (
        <div aria-busy="true" aria-label="Loading relay pool" className="flex flex-col gap-3">
          <Skeleton className="h-10 w-64" />
          <Skeleton className="h-40 w-full" />
        </div>
      )
    case 'session_ended':
      return null
    case 'forbidden':
      return <Navigate to="/forbidden" replace />
    case 'not_found':
      return (
        <div className="flex flex-col items-start gap-3 py-10">
          <h1 className="font-display text-xl font-semibold">Relay pool not found</h1>
          <p className="text-sm text-muted-foreground">It may have been deleted, or the link is wrong.</p>
          {back}
        </div>
      )
    case 'error':
      return (
        <div className="flex flex-col gap-4">
          {back}
          <Alert variant="destructive">
            <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
              <span>Couldn’t load this relay pool.</span>
              <Button variant="outline" size="sm" onClick={reload}>
                Retry
              </Button>
            </AlertDescription>
          </Alert>
        </div>
      )
    case 'ready':
      return (
        <PoolView
          pool={state.data}
          notice={notice?.id === id ? notice : null}
          onRefresh={() => {
            setNotice(null)
            reload()
          }}
          onActionDone={(n) => {
            setNotice({ ...n, id })
            reload()
          }}
        />
      )
  }
}

function Fact({ label, children }: { label: string; children: ReactNode }) {
  return (
    <div className="flex min-w-32 flex-[1_1_130px] flex-col gap-1">
      <dt className="text-xs font-medium tracking-wide text-muted-foreground">{label}</dt>
      <dd className="text-sm">{children}</dd>
    </div>
  )
}

function TruncatedNote({ show }: { show: boolean }) {
  if (!show) return null
  return <p className="px-5 py-3 text-xs text-muted-foreground">Showing the first 200 entries.</p>
}

function PoolView({
  pool: p,
  notice,
  onRefresh,
  onActionDone,
}: {
  pool: RelayPoolDetailDTO
  notice: RelayActionNotice | null
  onRefresh: () => void
  onActionDone: (notice: RelayActionNotice) => void
}) {
  const role = useSessionStore((s) => s.me?.role)
  const navigate = useNavigate()
  const [edit, setEdit] = useState<Edit | null>(null)
  const canManage = !p.is_default && hasRole(role, PROVIDER_ROLES)
  const canPlace = !p.is_default && hasRole(role, ['super-admin'])
  // As primary or failover pool (D-39): either way, the controller refuses region/residency changes.
  const inUse = p.tenants + p.failover_tenants > 0
  const done = (n: RelayActionNotice) => {
    setEdit(null)
    onActionDone(n)
  }

  return (
    <div className="flex flex-col gap-6">
      {back}
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div className="flex flex-col gap-2">
          <div className="flex flex-wrap items-center gap-3">
            <h1 className="font-display text-[26px] font-semibold tracking-[-0.02em]">{p.name}</h1>
            {p.is_default && <DefaultPoolBadge />}
            <ResidencyBadge residency={p.residency} />
          </div>
          <p className="text-sm text-subtle-foreground">created {formatDateTime(p.created_at)}</p>
        </div>
        <Button variant="outline" onClick={onRefresh} aria-label="Refresh relay pool">
          <RefreshCw aria-hidden />
          Refresh
        </Button>
      </header>

      {notice && (
        <Alert role="status" data-tone={notice.tone}>
          <AlertDescription>{notice.text}</AlertDescription>
        </Alert>
      )}

      {poolStranded(p) && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>
            Tenants use this pool but it has no eligible relay: tenants without a working failover pool have no relay
            path (direct connections still work), and it can’t take over for tenants using it as their failover pool.
            Add, move or resume a relay in this pool, or reassign the tenants.
          </AlertDescription>
        </Alert>
      )}

      {p.state === 'failed_over' && (
        <Alert role="status" data-pool-state="failed_over">
          <AlertDescription>
            Failed over since {formatDateTime(p.exhausted_at)}: tenants of this pool that have a failover pool are using
            it. They switch back once this pool has had an eligible relay for the hold-down ({HOLD_DOWN_NOTE}).
          </AlertDescription>
        </Alert>
      )}
      {p.state === 'recovering' && (
        <Alert role="status" data-pool-state="recovering">
          <AlertDescription>
            Recovering: eligible again since {formatDateTime(p.eligible_since)}. Tenants still on their failover pool
            switch back once it has stayed eligible for the hold-down ({HOLD_DOWN_NOTE}).
          </AlertDescription>
        </Alert>
      )}

      <Card className="px-5 py-5">
        <h2 className="sr-only">Overview</h2>
        <dl className="flex flex-wrap gap-x-6 gap-y-5">
          <Fact label="Region">
            <RegionCode region={p.region} />
          </Fact>
          <Fact label="Residency">
            <ResidencyBadge residency={p.residency} />
          </Fact>
          <Fact label="Relays">
            <span className="font-mono">{p.relays}</span>
          </Fact>
          <Fact label="Eligible relays">
            <span className="font-mono">{p.eligible_relays}</span>
          </Fact>
          <Fact label="State">
            <PoolStateBadge pool={p} />
          </Fact>
          <Fact label="Tenants">
            <span className="font-mono">{p.tenants}</span>
          </Fact>
          <Fact label="Failover for">
            <span className="font-mono">
              {p.failover_tenants} tenant{p.failover_tenants === 1 ? '' : 's'}
            </span>
          </Fact>
          <Fact label="Pool ID">
            <span className="break-all font-mono text-subtle-foreground">{p.id}</span>
          </Fact>
        </dl>
      </Card>

      <Card>
        <div className="flex flex-col gap-0.5 px-5 py-4">
          <h2 className="font-semibold">Relays</h2>
          <p className="text-[13px] text-muted-foreground">Eligible relays are the ones this pool’s tenants are offered now.</p>
        </div>
        {p.relay_list.length === 0 ? (
          <p className="px-5 pb-5 text-sm text-muted-foreground">No relays in this pool.</p>
        ) : (
          <Table aria-label="Pool relays">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Relay</TableHead>
                <TableHead>Status</TableHead>
                <TableHead>Eligible</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {p.relay_list.map((r) => (
                <TableRow key={r.id} aria-label={r.name}>
                  <TableCell>
                    <Link to={`/relays/${encodeURIComponent(r.id)}`} className="font-mono text-[13px] text-foreground hover:text-primary">
                      {r.name}
                    </Link>
                  </TableCell>
                  <TableCell>
                    <StatusBadge status={r.status} />
                  </TableCell>
                  <TableCell>{r.eligible ? <Badge variant="success">Eligible</Badge> : <Badge variant="secondary">Not eligible</Badge>}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        <TruncatedNote show={p.relay_list_truncated} />
      </Card>

      <Card>
        {/* The API lists the tenants whose primary pool this is; failover tenants are a count only. */}
        <h2 className="px-5 py-4 font-semibold">Tenants</h2>
        {p.tenant_list === null ? (
          <p className="px-5 pb-5 text-sm text-muted-foreground">
            {p.tenants} tenant{p.tenants === 1 ? '' : 's'} use{p.tenants === 1 ? 's' : ''} this pool. Tenant names are
            shown to super-admins only.
          </p>
        ) : p.tenant_list.length === 0 ? (
          <p className="px-5 pb-5 text-sm text-muted-foreground">No tenants use this pool as their primary pool.</p>
        ) : (
          <Table aria-label="Pool tenants">
            <TableHeader>
              <TableRow className="hover:bg-transparent">
                <TableHead>Tenant</TableHead>
                <TableHead>Slug</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {p.tenant_list.map((t) => (
                <TableRow key={t.id} aria-label={t.name}>
                  {/* Plain text: tenant detail is an audited read, opened from the Tenants page on purpose. */}
                  <TableCell className="font-medium">{t.name}</TableCell>
                  <TableCell className="font-mono text-[13px] text-subtle-foreground">{t.slug}</TableCell>
                </TableRow>
              ))}
            </TableBody>
          </Table>
        )}
        {p.tenant_list !== null && <TruncatedNote show={p.tenant_list_truncated} />}
      </Card>

      {p.is_default ? (
        <p className="text-[13px] text-muted-foreground">
          The default pool is built in: new relays and tenants start here, and it can’t be renamed, changed or deleted.
        </p>
      ) : (
        canManage && (
          <Card role="region" aria-labelledby="pool-manage-title" className="px-5 py-5">
            <h2 id="pool-manage-title" className="font-display text-base font-semibold">
              Manage pool
            </h2>
            <div className="mt-4 flex flex-col gap-4">
              <div className="flex flex-wrap items-center justify-between gap-3">
                <p className="max-w-xl text-sm text-subtle-foreground">
                  <span className="font-medium text-foreground">Rename.</span> Its relays and tenants don’t change.
                </p>
                <Button variant="outline" onClick={() => setEdit('rename')}>
                  Rename pool
                </Button>
              </div>
              {canPlace && (
                <div className="flex flex-wrap items-center justify-between gap-3">
                  <p className="max-w-xl text-sm text-subtle-foreground">
                    <span className="font-medium text-foreground">Region and residency.</span>{' '}
                    {inUse
                      ? 'Tenants use this pool (as their primary or failover pool), so these can’t change. Create a new pool with the region and residency you want and move the tenants to it instead.'
                      : 'Can change only while no tenant uses the pool.'}
                  </p>
                  <Button variant="outline" disabled={inUse} onClick={() => setEdit('placement')}>
                    Edit region and residency
                  </Button>
                </div>
              )}
              <div className="flex flex-wrap items-center justify-between gap-3">
                <p className="max-w-xl text-sm text-subtle-foreground">
                  <span className="font-medium text-foreground">Delete.</span> Only an empty pool: move its relays and
                  tenants to other pools first.
                </p>
                <Button variant="destructive" onClick={() => setEdit('delete')}>
                  Delete pool
                </Button>
              </div>
            </div>
          </Card>
        )
      )}

      {canManage && edit === 'rename' && <RenamePoolDialog pool={p} onCancel={() => setEdit(null)} onDone={done} />}
      {canPlace && edit === 'placement' && <PlacementDialog pool={p} onCancel={() => setEdit(null)} onDone={done} />}
      {canManage && edit === 'delete' && (
        <DeletePoolDialog
          pool={p}
          onCancel={() => setEdit(null)}
          onRefresh={done}
          onDeleted={() => navigate('/relay-pools', { replace: true })}
        />
      )}
    </div>
  )
}
