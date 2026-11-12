import { Link } from 'react-router-dom'
import type { RelayPoolRef, RelayPoolSummary } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { formatRelative } from '@/lib/expiry'
import { formatDateTime } from '@/lib/format'

// Pool labels shared by the relay, pool and tenant pages (D-37, D-40).
//
// Residency is routing only until relay-side enforcement ships and is
// live-checked (D-40, PR 3): the pool's relay list is guidance, relays still
// accept every workspace. The badge says so in its own text, never just
// "Residency", so it can't be read as a guarantee.

export const RESIDENCY_NOTE =
  'Routing only: this pool’s tenants are offered only its relays, but relays don’t enforce residency yet (relay-side enforcement isn’t shipped), so residency is not enforced and must not be promised to tenants.'

export function ResidencyBadge({ residency }: { residency: boolean }) {
  if (!residency) return <span className="text-muted-foreground">—</span>
  return (
    <Badge variant="info" className="whitespace-nowrap" title={RESIDENCY_NOTE} data-residency="routing-only">
      Residency (routing only)
      <span className="sr-only">. {RESIDENCY_NOTE}</span>
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
