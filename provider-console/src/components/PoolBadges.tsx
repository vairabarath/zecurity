import { Link } from 'react-router-dom'
import type { RelayPoolRef, RelayPoolSummary, RelaySummary } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { formatRelative } from '@/lib/expiry'
import { formatDateTime } from '@/lib/format'

// Pool labels shared by the relay, pool and tenant pages (D-37, D-40).
//
// Residency (D-40). A pool's relays enforce it only when the pool read says
// `residency_enforced`; nothing else may be shown as "enforced". Where only a
// pool ref is at hand (relay and tenant pages carry no enforcement flag) the
// badge says "Residency" and points to the pool page, claiming nothing.

export const RESIDENCY_ENFORCED_NOTE =
  'Relays in this pool accept only this pool’s tenants. A removed tenant’s sessions end at the relay’s next heartbeat (30 seconds at most by default). If a relay loses the controller for longer than its lease (10 minutes by default), it stops serving this pool’s tenants until the controller is back.'

export const RESIDENCY_NOT_ENFORCED_NOTE =
  'At least one active relay in this pool doesn’t support the workspace policy or hasn’t applied the current version yet (for example an older relay build, or a change made since its last heartbeat). Traffic is routed to the pool, but its relays don’t enforce residency.'

export const RESIDENCY_UNKNOWN_NOTE = 'A residency pool. Whether its relays enforce residency is shown on the pool’s page.'

/** What a residency pool's checkbox means (create and edit dialogs). */
export const RESIDENCY_NOTE =
  'Tenants of a residency pool are offered only its relays. Its relays enforce it (accept only its tenants) once every active relay supports the workspace policy and has applied it; the pool’s page shows whether that is the case.'

/**
 * `enforced` is the pool read's residency_enforced; leave it undefined where
 * only a pool ref is known, and the badge makes no enforcement claim.
 */
export function ResidencyBadge({ residency, enforced }: { residency: boolean; enforced?: boolean }) {
  if (!residency) return <span className="text-muted-foreground">—</span>
  const [label, note, variant, data] =
    enforced === true
      ? (['Residency (enforced)', RESIDENCY_ENFORCED_NOTE, 'success', 'enforced'] as const)
      : enforced === false
        ? (['Residency (not enforced)', RESIDENCY_NOT_ENFORCED_NOTE, 'warning', 'not-enforced'] as const)
        : (['Residency', RESIDENCY_UNKNOWN_NOTE, 'secondary', 'unknown'] as const)
  return (
    <Badge variant={variant} className="whitespace-nowrap" title={note} data-residency={data}>
      {label}
      <span className="sr-only">. {note}</span>
    </Badge>
  )
}

/**
 * A relay's workspace-policy state (D-40), for relays in residency pools;
 * "—" elsewhere. Only the API's own flags; nothing is derived.
 */
export function RelayPolicyBadge({ relay }: { relay: Pick<RelaySummary, 'pool' | 'policy'> }) {
  if (!relay.pool.residency) return <span className="text-muted-foreground">—</span>
  const p = relay.policy
  if (!p.supported) {
    return (
      <Badge variant="destructive" className="whitespace-nowrap" data-policy="unsupported" title="An older relay build: it can’t enforce the workspace policy, so residency isn’t enforced on this relay.">
        Not supported
      </Badge>
    )
  }
  if (!p.current) {
    return (
      <Badge variant="warning" className="whitespace-nowrap" data-policy="applying" title="It applies the pool’s current policy at its next heartbeat.">
        Applying…
      </Badge>
    )
  }
  return (
    <Badge variant="success" className="whitespace-nowrap" data-policy="enforcing" title="It has applied the pool’s current policy and accepts only the pool’s tenants.">
      Enforcing
    </Badge>
  )
}

/** A region code, or "—" when the pool has none. */
export function RegionCode({ region }: { region: string | null }) {
  return region ? <span className="font-mono text-[13px]">{region}</span> : <span className="text-muted-foreground">—</span>
}

export function DefaultPoolBadge() {
  return (
    <Badge variant="secondary" title="Built in: new relays and tenants start here. It can’t be renamed, changed or deleted.">
      Default
    </Badge>
  )
}

/** The pool's name as a link to its detail page. */
export function PoolLink({ pool, className }: { pool: Pick<RelayPoolRef, 'id' | 'name'>; className?: string }) {
  return (
    <Link to={`/relay-pools/${encodeURIComponent(pool.id)}`} className={className ?? 'font-medium text-foreground hover:text-primary'}>
      {pool.name}
    </Link>
  )
}

/** The hold-down before tenants switch back (D-39 default; the console can't read the configured value). */
export const HOLD_DOWN_NOTE = '5 minutes by default'

/**
 * A pool's failover state (D-39) as the controller reports it, with its time.
 * Never derived here: "healthy" is also what an untracked pool reports.
 */
export function PoolStateBadge({ pool, withTime = false }: { pool: Pick<RelayPoolSummary, 'state' | 'exhausted_at' | 'eligible_since'>; withTime?: boolean }) {
  switch (pool.state) {
    case 'failed_over':
      return (
        <span className="inline-flex flex-wrap items-center gap-2" data-pool-state="failed_over">
          <Badge variant="destructive" title={`No eligible relay since ${formatDateTime(pool.exhausted_at)}`}>
            Failed over
          </Badge>
          {withTime && <span className="text-[13px] text-subtle-foreground">since {formatRelative(pool.exhausted_at)}</span>}
        </span>
      )
    case 'recovering':
      return (
        <span className="inline-flex flex-wrap items-center gap-2" data-pool-state="recovering">
          <Badge
            variant="warning"
            title={`Eligible again since ${formatDateTime(pool.eligible_since)}; tenants switch back after it stays healthy for the hold-down (${HOLD_DOWN_NOTE})`}
          >
            Recovering
          </Badge>
          {withTime && <span className="text-[13px] text-subtle-foreground">eligible again since {formatRelative(pool.eligible_since)}</span>}
        </span>
      )
    case 'healthy':
      return (
        <Badge variant="success" data-pool-state="healthy">
          Healthy
        </Badge>
      )
    default:
      return (
        <Badge variant="secondary" data-pool-state={String(pool.state)}>
          {String(pool.state)}
        </Badge>
      )
  }
}
