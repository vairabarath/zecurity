import { useRef, useState, type FormEvent } from 'react'
import { ApiError } from '@/api/client'
import { assignTenantRelayPool } from '@/api/pools'
import type { PoolMigration, TenantDetail } from '@/api/types'
import { hasRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { PoolLink, RegionCode, ResidencyBadge } from '@/components/PoolBadges'
import { PoolSelect } from '@/components/PoolSelect'
import { ReasonField } from '@/components/ReasonField'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { formatDateTime } from '@/lib/format'
import { assignPoolFailure, type PoolFailure } from '@/lib/pool-errors'
import { reasonValid } from '@/lib/pool-format'
import { usePools } from '@/lib/usePools'

const SUPER_ADMIN = ['super-admin'] as const

// Tenant detail's "Relay pool" section (D-38, D-40, D-41). It renders only
// what the audited tenant read already returned; the Change pool dialog
// loads the pool list (not audited) when it opens. Assigning a tenant's pool
// is super-admin only (decide()); hiding the button is UX only.
export function TenantRelayPoolSection({ tenant, onDone }: { tenant: TenantDetail; onDone: (notice: RelayActionNotice) => void }) {
  const role = useSessionStore((s) => s.me?.role)
  const [changing, setChanging] = useState(false)
  const rp = tenant.relay_pool
  const canChange = hasRole(role, SUPER_ADMIN)

  return (
    <Card role="region" aria-labelledby="tenant-pool-title" className="px-5 py-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <h2 id="tenant-pool-title" className="font-semibold">
          Relay pool
        </h2>
        {canChange && (
          <Button variant="outline" onClick={() => setChanging(true)}>
            Change pool
          </Button>
        )}
      </div>
      <dl className="mt-4 flex flex-wrap gap-x-8 gap-y-3 text-sm">
        <div className="flex flex-col gap-1">
          <dt className="text-xs font-medium tracking-wide text-muted-foreground">Pool</dt>
          <dd>
            <PoolLink pool={rp} />
          </dd>
        </div>
        <div className="flex flex-col gap-1">
          <dt className="text-xs font-medium tracking-wide text-muted-foreground">Region</dt>
          <dd>
            <RegionCode region={rp.region} />
          </dd>
        </div>
        <div className="flex flex-col gap-1">
          <dt className="text-xs font-medium tracking-wide text-muted-foreground">Residency</dt>
          <dd>
            <ResidencyBadge residency={rp.residency} />
          </dd>
        </div>
        <div className="flex flex-col gap-1">
          <dt className="text-xs font-medium tracking-wide text-muted-foreground">Eligible relays</dt>
          <dd className="inline-flex items-center gap-2">
            <span className="font-mono">{rp.eligible_relays}</span>
            {rp.eligible_relays === 0 && <Badge variant="destructive">No eligible relay</Badge>}
          </dd>
        </div>
      </dl>
      {rp.eligible_relays === 0 && (
        <Alert variant="destructive" role="alert" className="mt-4">
          <AlertDescription>
            This tenant’s pool has no eligible relay, so its relay path is down (direct connections still work) and there
            is no fallback to another pool. Add, move or resume a relay in {rp.name}, or move the tenant to another pool.
          </AlertDescription>
        </Alert>
      )}
      {rp.previous && (
        <p role="status" className="mt-4 text-sm text-subtle-foreground" data-migration="graceful">
          Migrating from <PoolLink pool={rp.previous} /> until {formatDateTime(rp.previous.until)}. Until then the old
          pool stays authorized so connectors can move without dropping traffic.
        </p>
      )}
      {canChange && changing && (
        <ChangePoolDialog
          tenant={tenant}
          onCancel={() => setChanging(false)}
          onDone={(n) => {
            setChanging(false)
            onDone(n)
          }}
        />
      )}
    </Card>
  )
}

function ChangePoolDialog({
  tenant,
  onCancel,
  onDone,
}: {
  tenant: TenantDetail
  onCancel: () => void
  onDone: (notice: RelayActionNotice) => void
}) {
  const current = tenant.relay_pool
  const pools = usePools()
  const [target, setTarget] = useState('')
  const [migration, setMigration] = useState<PoolMigration>('graceful')
  const [reason, setReason] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<PoolFailure>()
  const inFlight = useRef(false)

  const others = pools.status === 'ready' ? pools.items.filter((p) => p.id !== current.id) : []
  // A pool with no eligible relay is refused (409 pool_has_no_eligible_relay); as of the list load.
  const chosen = others.find((p) => p.id === target && p.eligible_relays > 0)
  const valid = chosen !== undefined && reasonValid(reason)
  const crossRegion = chosen !== undefined && current.residency && chosen.region !== current.region

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || !chosen || inFlight.current) return
    inFlight.current = true
    setPending(true)
    setError(undefined)
    try {
      await assignTenantRelayPool(tenant.id, chosen.id, reason.trim(), migration)
      onDone({
        tone: 'success',
        text:
          migration === 'graceful'
            ? `${tenant.name} now uses ${chosen.name}. ${current.name} stays authorized for the migration window while connectors move.`
            : `${tenant.name} now uses ${chosen.name}. It no longer has access to ${current.name}.`,
      })
    } catch (err) {
      // An ended session is handled by the session store (sign-in page).
      if (err instanceof ApiError && err.sessionEnded) return
      const failure = assignPoolFailure(err, tenant.name)
      if (failure.refresh) onDone({ tone: 'info', text: failure.message })
      else setError(failure)
    } finally {
      inFlight.current = false
      setPending(false)
    }
  }

  return (
    <Dialog open onOpenChange={(o) => !o && !pending && onCancel()}>
      <DialogContent>
        <form onSubmit={(e) => void submit(e)} className="grid gap-4" noValidate>
          <DialogHeader>
            <DialogTitle>Change relay pool</DialogTitle>
            <DialogDescription>
              <strong className="text-foreground">{tenant.name}</strong>’s connectors will be offered only the new pool’s
              eligible relays. It now uses {current.name}.
            </DialogDescription>
          </DialogHeader>
          <Alert role="note">
            <AlertDescription>
              The tenant’s relay availability becomes the new pool’s: if it has fewer eligible relays, fewer relays carry
              this tenant’s traffic, and if they all go, its relay path stops (no fallback to another pool).
            </AlertDescription>
          </Alert>
          {crossRegion && (
            <Alert role="note">
              <AlertDescription>
                {current.name} is a residency pool (routing only) in {current.region ?? 'no region'}; {chosen.name} is in{' '}
                {chosen.region ?? 'no region'}. This moves the tenant’s relay traffic to another region.
              </AlertDescription>
            </Alert>
          )}
          {error && !error.field && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error.message}</AlertDescription>
            </Alert>
          )}
          <div className="grid gap-2">
            <Label htmlFor="tenant-pool">New pool</Label>
            {pools.status === 'ready' ? (
              others.length > 0 ? (
                <PoolSelect
                  id="tenant-pool"
                  label="New pool"
                  pools={others}
                  value={target}
                  onChange={setTarget}
                  disabledReason={(p) => (p.eligible_relays > 0 ? null : 'no eligible relay')}
                />
              ) : (
                <p className="text-sm text-muted-foreground">There is no other pool. Create one on the Relay pools page first.</p>
              )
            ) : pools.status === 'loading' ? (
              <p className="text-sm text-muted-foreground" aria-busy="true">
                Loading pools…
              </p>
            ) : (
              <p className="text-sm text-destructive">Couldn’t load the relay pools. Close this dialog and try again.</p>
            )}
            <p className="text-xs text-muted-foreground">Pools with no eligible relay can’t be chosen.</p>
          </div>
          <fieldset className="grid gap-2">
            <legend className="mb-1 text-sm font-medium">Migration</legend>
            <label className="flex items-start gap-2.5 text-sm">
              <input
                type="radio"
                name="pool-migration"
                value="graceful"
                className="mt-1 accent-[var(--color-primary)]"
                checked={migration === 'graceful'}
                onChange={() => setMigration('graceful')}
              />
              <span>
                <span className="font-medium">Graceful</span> (recommended).{' '}
                <span className="text-muted-foreground">
                  {current.name} stays authorized for a short migration window (5 minutes by default), so connectors
                  connect to the new pool before leaving the old one.
                </span>
              </span>
            </label>
            <label className="flex items-start gap-2.5 text-sm">
              <input
                type="radio"
                name="pool-migration"
                value="immediate"
                className="mt-1 accent-[var(--color-primary)]"
                checked={migration === 'immediate'}
                onChange={() => setMigration('immediate')}
              />
              <span>
                <span className="font-medium">Immediate</span>.{' '}
                <span className="text-muted-foreground">
                  The tenant loses {current.name} at once, with no migration window.
                </span>
              </span>
            </label>
          </fieldset>
          <ReasonField id="tenant-pool-reason" value={reason} onChange={setReason} error={error?.field === 'reason' ? error.message : undefined} />
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Changing…' : 'Change pool'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
