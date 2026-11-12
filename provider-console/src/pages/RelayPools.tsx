import { useState } from 'react'
import { AlertTriangle, Plus, RefreshCw } from 'lucide-react'
import { Navigate } from 'react-router-dom'
import { listRelayPools } from '@/api/reads'
import type { RelayPoolSummary } from '@/api/types'
import { hasRole, PROVIDER_ROLES } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { PageHeader } from '@/components/PageHeader'
import { DefaultPoolBadge, PoolLink, PoolStateBadge, RegionCode, ResidencyBadge } from '@/components/PoolBadges'
import { CreatePoolDialog } from '@/components/PoolDialogs'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'
import { Table, TableBody, TableCell, TableHead, TableHeader, TableRow } from '@/components/ui/table'
import { poolStranded } from '@/lib/pool-format'
import { useDetailQuery } from '@/lib/useDetailQuery'

// Relay pools (D-37 … D-41; relay.read: super-admin and relay-ops). One
// request when the page opens; Refresh and Retry are the only other reads.
// The list is counts only (never tenant names). The failover state (D-39) is
// the controller's; the console only shows it. The one write here is
// Create pool (both roles; region and residency fields for super-admin
// only). Rename, region/residency and delete live on the pool's page.

const fetchPools = () => listRelayPools()

export function RelayPools() {
  const { state, reload } = useDetailQuery('pools', fetchPools)
  const role = useSessionStore((s) => s.me?.role)
  const canCreate = hasRole(role, PROVIDER_ROLES)
  const isSuperAdmin = hasRole(role, ['super-admin'])
  const [creating, setCreating] = useState(false)
  const [notice, setNotice] = useState<RelayActionNotice | null>(null)

  let body
  switch (state.status) {
    case 'loading':
      body = (
        <div aria-busy="true" aria-label="Loading relay pools" className="flex flex-col gap-2">
          <Skeleton className="h-8 w-full" />
          <Skeleton className="h-8 w-full" />
        </div>
      )
      break
    case 'session_ended':
      return null
    case 'forbidden':
      return <Navigate to="/forbidden" replace />
    case 'not_found':
    case 'error':
      body = (
        <Alert variant="destructive">
          <AlertDescription className="flex flex-wrap items-center justify-between gap-3">
            <span>Couldn’t load relay pools.</span>
            <Button variant="outline" size="sm" onClick={reload}>
              Retry
            </Button>
          </AlertDescription>
        </Alert>
      )
      break
    case 'ready':
      body = <PoolTable pools={state.data.items} />
      break
  }

  return (
    <div className="flex flex-col gap-6">
      <PageHeader
        title="Relay pools"
        description="Every relay belongs to one pool, and each tenant’s connectors are offered only the eligible relays of its pool."
        actions={
          <>
            <Button
              variant="outline"
              onClick={() => {
                setNotice(null)
                reload()
              }}
              aria-label="Refresh relay pools"
            >
              <RefreshCw aria-hidden />
              Refresh
            </Button>
            {canCreate && (
              <Button onClick={() => setCreating(true)}>
                <Plus aria-hidden />
                Create pool
              </Button>
            )}
          </>
        }
      />
      {notice && (
        <Alert role="status" data-tone={notice.tone}>
          <AlertDescription>{notice.text}</AlertDescription>
        </Alert>
      )}
      {body}
      {canCreate && creating && (
        <CreatePoolDialog
          canSetPlacement={isSuperAdmin}
          onCancel={() => setCreating(false)}
          onCreated={(p) => {
            setCreating(false)
            setNotice({ tone: 'success', text: `Pool ${p.name} created. Move relays into it from each relay’s page.` })
            reload()
          }}
        />
      )}
    </div>
  )
}

function PoolTable({ pools }: { pools: RelayPoolSummary[] }) {
  const stranded = pools.filter(poolStranded)
  if (pools.length === 0) return <p className="py-6 text-sm text-muted-foreground">No relay pools.</p>
  return (
    <>
      {stranded.length > 0 && (
        <Alert variant="destructive" role="alert">
          <AlertDescription>
            {stranded.map((p) => p.name).join(', ')} {stranded.length === 1 ? 'is' : 'are'} used by tenants but{' '}
            {stranded.length === 1 ? 'has' : 'have'} no eligible relay: tenants without a working failover pool have no
            relay path (direct connections still work), and {stranded.length === 1 ? 'it' : 'they'} can’t take over as a
            failover pool. Add, move or resume a relay in {stranded.length === 1 ? 'that pool' : 'those pools'}, or
            reassign the tenants.
          </AlertDescription>
        </Alert>
      )}
      <Card>
        <Table aria-label="Relay pools">
          <TableHeader>
            <TableRow className="hover:bg-transparent">
              <TableHead>Pool</TableHead>
              <TableHead>Region</TableHead>
              <TableHead>Residency</TableHead>
              <TableHead>State</TableHead>
              <TableHead className="text-right">Relays</TableHead>
              <TableHead className="text-right">Eligible relays</TableHead>
              <TableHead className="text-right">Tenants</TableHead>
              <TableHead className="text-right">Failover for</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pools.map((p) => (
              <TableRow key={p.id} aria-label={p.name}>
                <TableCell>
                  <span className="inline-flex flex-wrap items-center gap-2">
                    <PoolLink pool={p} />
                    {p.is_default && <DefaultPoolBadge />}
                  </span>
                </TableCell>
                <TableCell>
                  <RegionCode region={p.region} />
                </TableCell>
                <TableCell>
                  <ResidencyBadge residency={p.residency} />
                </TableCell>
                <TableCell>
                  <PoolStateBadge pool={p} withTime />
                </TableCell>
                <TableCell className="text-right font-mono text-[13px] tabular-nums">{p.relays}</TableCell>
                <TableCell className="text-right font-mono text-[13px] tabular-nums">
                  <span className="inline-flex items-center justify-end gap-2">
                    {poolStranded(p) && (
                      <Badge variant="destructive" title="Tenants use this pool (as primary or failover) but it has no eligible relay">
                        <AlertTriangle className="size-3" aria-hidden />
                        No eligible relay
                      </Badge>
                    )}
                    {p.eligible_relays}
                  </span>
                </TableCell>
                <TableCell className="text-right font-mono text-[13px] tabular-nums">{p.tenants}</TableCell>
                <TableCell className="text-right font-mono text-[13px] tabular-nums" aria-label={`Failover for ${p.failover_tenants} tenants`}>
                  {p.failover_tenants}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </Card>
    </>
  )
}
