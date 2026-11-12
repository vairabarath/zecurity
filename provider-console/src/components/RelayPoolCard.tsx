import { useRef, useState, type FormEvent } from 'react'
import { ApiError } from '@/api/client'
import { moveRelayToPool } from '@/api/pools'
import type { RelayDetail } from '@/api/types'
import { hasRole, PROVIDER_ROLES } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { PoolLink, RegionCode, ResidencyBadge } from '@/components/PoolBadges'
import { PoolSelect } from '@/components/PoolSelect'
import { ReasonField } from '@/components/ReasonField'
import type { RelayActionNotice } from '@/components/RelayDangerZone'
import { Alert, AlertDescription } from '@/components/ui/alert'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { Label } from '@/components/ui/label'
import { Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle } from '@/components/ui/dialog'
import { moveRelayFailure, type PoolFailure } from '@/lib/pool-errors'
import { reasonValid } from '@/lib/pool-format'
import { usePools } from '@/lib/usePools'

const plural = (n: number, one: string) => `${n.toLocaleString()} ${one}${n === 1 ? '' : 's'}`

// Relay detail's "Relay pool" card (D-37, D-41). Shows the relay's pool and,
// for both provider roles, "Move to pool" on a relay that isn't revoked or
// deleted. Hiding it is UX only; the controller authorizes every request and
// refuses a move that would leave a pool tenants use without an eligible
// relay (409 last_eligible_relay, per pool).
export function RelayPoolCard({ relay, onDone }: { relay: RelayDetail; onDone: (notice: RelayActionNotice) => void }) {
  const role = useSessionStore((s) => s.me?.role)
  const [moving, setMoving] = useState(false)
  const canMove = hasRole(role, PROVIDER_ROLES) && relay.status !== 'revoked' && relay.status !== 'deleted'
  const p = relay.pool

  return (
    <Card role="region" aria-labelledby="relay-pool-title" className="px-5 py-5">
      <h2 id="relay-pool-title" className="font-display text-base font-semibold">
        Relay pool
      </h2>
      <div className="mt-4 flex flex-wrap items-center justify-between gap-3">
        <dl className="flex flex-wrap gap-x-8 gap-y-3 text-sm">
          <div className="flex flex-col gap-1">
            <dt className="text-xs font-medium tracking-wide text-muted-foreground">Pool</dt>
            <dd>
              <PoolLink pool={p} />
            </dd>
          </div>
          <div className="flex flex-col gap-1">
            <dt className="text-xs font-medium tracking-wide text-muted-foreground">Region</dt>
            <dd>
              <RegionCode region={p.region} />
            </dd>
          </div>
          <div className="flex flex-col gap-1">
            <dt className="text-xs font-medium tracking-wide text-muted-foreground">Residency</dt>
            <dd>
              <ResidencyBadge residency={p.residency} />
            </dd>
          </div>
          <div className="flex flex-col gap-1">
            <dt className="text-xs font-medium tracking-wide text-muted-foreground">Eligible relays in this pool</dt>
            <dd className="font-mono">{relay.eligible_relay_count}</dd>
          </div>
        </dl>
        {canMove && (
          <Button variant="outline" onClick={() => setMoving(true)}>
            Move to pool
          </Button>
        )}
      </div>
      {canMove && moving && (
        <MoveRelayDialog
          relay={relay}
          onCancel={() => setMoving(false)}
          onDone={(notice) => {
            setMoving(false)
            onDone(notice)
          }}
        />
      )}
    </Card>
  )
}

function MoveRelayDialog({
  relay,
  onCancel,
  onDone,
}: {
  relay: RelayDetail
  onCancel: () => void
  onDone: (notice: RelayActionNotice) => void
}) {
  const pools = usePools()
  const [target, setTarget] = useState('')
  const [reason, setReason] = useState('')
  const [pending, setPending] = useState(false)
  const [error, setError] = useState<PoolFailure>()
  const inFlight = useRef(false)

  const others = pools.status === 'ready' ? pools.items.filter((p) => p.id !== relay.pool.id) : []
  const chosen = others.find((p) => p.id === target)
  const valid = chosen !== undefined && reasonValid(reason)
  // Advisory, as of the page load: the controller decides (409 last_eligible_relay).
  const lastInPool = relay.in_relay_list && relay.eligible_relay_count === 1

  async function submit(e: FormEvent) {
    e.preventDefault()
    if (!valid || !chosen || inFlight.current) return
    inFlight.current = true
    setPending(true)
    setError(undefined)
    try {
      await moveRelayToPool(relay.id, chosen.id, reason.trim())
      onDone({ tone: 'success', text: `${relay.name} moved to ${chosen.name}. Tenants using ${relay.pool.name} are no longer offered it.` })
    } catch (err) {
      // An ended session is handled by the session store (sign-in page).
      if (err instanceof ApiError && err.sessionEnded) return
      const failure = moveRelayFailure(err, relay.name)
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
            <DialogTitle>Move to pool</DialogTitle>
            <DialogDescription>
              <strong className="text-foreground">{relay.name}</strong> leaves {relay.pool.name}. Only tenants using the
              new pool are offered it; connectors of {relay.pool.name}’s tenants move to that pool’s other relays.
            </DialogDescription>
          </DialogHeader>
          {lastInPool && (
            <Alert role="note">
              <AlertDescription>
                This is the only eligible relay in {relay.pool.name}. If any tenant uses that pool, the controller refuses
                the move: add or resume another relay in it first, or move those tenants to another pool.
              </AlertDescription>
            </Alert>
          )}
          {error && !error.field && (
            <Alert variant="destructive" role="alert">
              <AlertDescription>{error.message}</AlertDescription>
            </Alert>
          )}
          <div className="grid gap-2">
            <Label htmlFor="move-pool">New pool</Label>
            {pools.status === 'ready' ? (
              others.length > 0 ? (
                <PoolSelect id="move-pool" label="New pool" pools={others} value={target} onChange={setTarget} />
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
            {chosen && (
              <p className="text-xs text-muted-foreground">
                {chosen.name} has {plural(chosen.eligible_relays, 'eligible relay')} and {plural(chosen.tenants, 'tenant')}.
              </p>
            )}
          </div>
          <ReasonField id="move-reason" value={reason} onChange={setReason} error={error?.field === 'reason' ? error.message : undefined} />
          <DialogFooter>
            <Button type="button" variant="ghost" onClick={onCancel} disabled={pending}>
              Cancel
            </Button>
            <Button type="submit" disabled={!valid || pending}>
              {pending ? 'Moving…' : 'Move relay'}
            </Button>
          </DialogFooter>
        </form>
      </DialogContent>
    </Dialog>
  )
}
