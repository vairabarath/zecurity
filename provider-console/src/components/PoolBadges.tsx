import { Link } from 'react-router-dom'
import type { RelayPoolRef } from '@/api/types'
import { Badge } from '@/components/ui/badge'

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
