import { useRef, useState, type FormEvent } from 'react'
import { ApiError } from '@/api/client'
import { assignTenantRelayPool } from '@/api/pools'
import type { PoolMigration, RelayPoolSummary, TenantDetail } from '@/api/types'
import { hasRole } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { HOLD_DOWN_NOTE, PoolLink, RegionCode, ResidencyBadge } from '@/components/PoolBadges'
import { PoolSelect } from '@/components/PoolSelect'
import { ReasonField } from '@/components/ReasonField'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { Label } from '@/components/ui/label'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'
import { formatDateTime } from '@/lib/format'
import { assignPoolFailure, type PoolFailure } from '@/lib/pool-errors'
import { failoverProblem, poolOptionLabel, reasonValid } from '@/lib/pool-format'
import { usePools } from '@/lib/usePools'

const SUPER_ADMIN = ['super-admin'] as const
/** Radix Select can't use "" as an item value; this stands for "no failover pool". */
const NONE = 'none'

// Tenant detail's "Relay pool" section (D-38 … D-41). It renders only what
// the audited tenant read already returned; the Change pool dialog loads the
// pool list (not audited) when it opens. Assigning a tenant's pools is
// super-admin only (decide()); hiding the button is UX only. Failover (D-39)
// is controller-driven: the console shows the controller's state and never
// derives one (whether the primary is recovering can't be told from here).
export function TenantRelayPoolSection({ tenant, onDone }: { tenant: TenantDetail; onDone: (notice: RelayActionNotice) => void }) {
  const role = useSessionStore((s) => s.me?.role)
  const [changing, setChanging] = useState(false)
  const rp = tenant.relay_pool
  const fo = rp.failover
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
      {rp.using_failover && fo && (
        <Alert role="status" data-using-failover className="mt-4">
          <AlertDescription>
            Using failover pool <PoolLink pool={fo} /> since{' '}
            {formatDateTime(rp.failed_over_since)}: the primary
            pool {rp.name} has no eligible relay. The controller switches back once {rp.name} has had an eligible relay for
            the hold-down ({HOLD_DOWN_NOTE}).
          </AlertDescription>
        </Alert>
      )}
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
      <div className="mt-4 border-t border-border pt-4" role="group" aria-label="Failover pool">
        <h3 className="text-sm font-medium">Failover pool</h3>
        {fo ? (
          <dl className="mt-3 flex flex-wrap gap-x-8 gap-y-3 text-sm">
            <div className="flex flex-col gap-1">
              <dt className="text-xs font-medium tracking-wide text-muted-foreground">Pool</dt>
              <dd>
                <PoolLink pool={fo} />
              </dd>
            </div>
            <div className="flex flex-col gap-1">
              <dt className="text-xs font-medium tracking-wide text-muted-foreground">Region</dt>
              <dd>
                <RegionCode region={fo.region} />
              </dd>
            </div>
            <div className="flex flex-col gap-1">
              <dt className="text-xs font-medium tracking-wide text-muted-foreground">Failover eligible relays</dt>
              <dd className="inline-flex items-center gap-2">
                <span className="font-mono">{fo.eligible_relays}</span>
                {fo.eligible_relays === 0 && <Badge variant="destructive">No eligible relay</Badge>}
              </dd>
            </div>
          </dl>
        ) : (
          <p className="mt-2 text-sm text-muted-foreground">No failover pool. If {rp.name} has no eligible relay, this tenant’s relay path stops.</p>
        )}
        {fo && fo.eligible_relays === 0 && (
          <p className="mt-3 text-sm text-destructive">{fo.name} has no eligible relay, so it can’t take over if {rp.name} fails.</p>
        )}
      </div>
      {rp.eligible_relays === 0 && !rp.using_failover && (
        <Alert variant="destructive" role="alert" className="mt-4">
          <AlertDescription>
            This tenant’s pool has no eligible relay, so its relay path is down (direct connections still work)
            {fo ? ' until the controller switches it to its failover pool' : ' and it has no failover pool'}. Add, move or
            resume a relay in {rp.name}, or move the tenant to another pool.
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

function FailoverSelect({
  pools,
  primary,
  value,
  onChange,
}: {
  pools: RelayPoolSummary[]
  primary: RelayPoolSummary | undefined
  value: string
  onChange: (v: string) => void
}) {
  return (
    <Select value={value} onValueChange={onChange}>
      <SelectTrigger id="tenant-failover" aria-label="Failover pool">
        <SelectValue />
      </SelectTrigger>
      <SelectContent>
        <SelectItem value={NONE}>None</SelectItem>
        {pools.map((p) => {
          const why = primary ? failoverProblem(primary, p) : null
          return (
            <SelectItem key={p.id} value={p.id} disabled={why !== null}>
              {poolOptionLabel(p)}
              {why && ` (${why})`}
            </SelectItem>
          )
        })}
      </SelectContent>
    </Select>
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
  const currentFailover = current.failover?.id ?? NONE
  const pools = usePools()
  const [primaryId, setPrimaryId] = useState(current.id)
  const [failoverId, setFailoverId] = useState(currentFailover)
  const [migration, setMigration] = useState<PoolMigration>('graceful')
  const [reason, setReason] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<PoolFailure>()
  const inFlight = useRef(false)

  const items = pools.status === 'ready' ? pools.items : []
  const primary = items.find((p) => p.id === primaryId)
  const failover = failoverId === NONE ? undefined : items.find((p) => p.id === failoverId)
  const primaryChanged = primaryId !== current.id
  const changed = primaryChanged || failoverId !== currentFailover
  // As of the list load; the controller checks every rule again.
  const primaryProblem = primary && primaryChanged && primary.eligible_relays === 0 ? 'That pool has no eligible relay.' : null
  const failoverIssue = primary && failover ? failoverProblem(primary, failover) : null
  const failoverMissing = failoverId !== NONE && pools.status === 'ready' && !failover
  const valid = primary !== undefined && changed && !primaryProblem && !failoverIssue && !failoverMissing && reasonValid(reason)
  const crossRegion = primary !== undefined && primaryChanged && current.residency && primary.region !== current.region
  const failoverError = error?.field === 'failover' ? error.message : failoverIssue ? `This failover pool doesn’t fit the primary: ${failoverIssue}.` : undefined

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || !primary || inFlight.current) return
    inFlight.current = true
    setPending(true)
    setError(undefined)
    const fo = failover ? failover.id : null
    try {
      await assignTenantRelayPool(tenant.id, primary.id, fo, reason.trim(), migration)
      const foText = failover ? `failover pool ${failover.name}` : 'no failover pool'
      onDone({
        tone: 'success',
        text: !primaryChanged
          ? `${tenant.name} keeps ${primary.name} with ${foText}.`
          : migration === 'graceful'
            ? `${tenant.name} now uses ${primary.name} with ${foText}. ${current.name} stays authorized for the migration window while connectors move.`
            : `${tenant.name} now uses ${primary.name} with ${foText}. It no longer has access to ${current.name}.`,
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
              <strong className="text-foreground">{tenant.name}</strong>’s connectors are offered only the eligible relays
              of its pool. It now uses {current.name}
              {current.failover ? `, with failover pool ${current.failover.name}` : ', with no failover pool'}.
            </DialogDescription>
          </DialogHeader>
          <Alert role="note">
            <AlertDescription>
              The tenant’s relay availability becomes the chosen pools’: if they have fewer eligible relays, fewer relays
              carry this tenant’s traffic, and if they all go, its relay path stops.
            </AlertDescription>
          </Alert>
          {crossRegion && primary && (
            <Alert role="note">
              <AlertDescription>
                {current.name} is a residency pool (routing only) in {current.region ?? 'no region'}; {primary.name} is in{' '}
                {primary.region ?? 'no region'}. This moves the tenant’s relay traffic to another region.
              </AlertDescription>
            </Alert>
          )}
          {error && !error.field && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error.message}</AlertDescription>
            </Alert>
          )}
          {pools.status === 'loading' && (
            <p className="text-sm text-muted-foreground" aria-busy="true">
              Loading pools…
            </p>
          )}
          {(pools.status === 'error' || pools.status === 'session_ended') && (
            <p className="text-sm text-destructive">Couldn’t load the relay pools. Close this dialog and try again.</p>
          )}
          {pools.status === 'ready' && (
            <>
              <div className="grid gap-2">
                <Label htmlFor="tenant-pool">Primary pool</Label>
                <PoolSelect
                  id="tenant-pool"
                  label="Primary pool"
                  pools={items}
                  value={primaryId}
                  onChange={setPrimaryId}
                  disabledReason={(p) => (p.id === current.id ? null : p.eligible_relays > 0 ? null : 'no eligible relay')}
                />
                <p className={primaryProblem ? 'text-xs text-destructive' : 'text-xs text-muted-foreground'}>
                  {primaryProblem ?? 'Keep the current pool to change only the failover pool. Pools with no eligible relay can’t be chosen.'}
                </p>
              </div>
              <div className="grid gap-2">
                <Label htmlFor="tenant-failover">Failover pool</Label>
                <FailoverSelect pools={items} primary={primary} value={failoverId} onChange={setFailoverId} />
                {failoverError ? (
                  <p className="text-xs text-destructive" role="alert">
                    {failoverError}
                  </p>
                ) : (
                  <p className="text-xs text-muted-foreground">
                    Optional. Controller-driven: when the primary has no eligible relay (typically 1.5 to 2.5 minutes after
                    its last relay stops), the tenant switches to the failover pool, and switches back once the primary
                    has been healthy for {HOLD_DOWN_NOTE}. Same region only, never cross-region; a residency primary needs
                    a residency failover. None removes the failover pool.
                  </p>
                )}
              </div>
            </>
          )}
          {primaryChanged && (
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
                  <span className="text-muted-foreground">The tenant loses {current.name} at once, with no migration window.</span>
                </span>
              </label>
            </fieldset>
          )}
          <ReasonField id="tenant-pool-reason" value={reason} onChange={setReason} error={error?.field === 'reason' ? error.message : undefined} />
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Saving…' : 'Save pools'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}

