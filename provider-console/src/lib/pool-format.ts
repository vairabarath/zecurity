import { MAX_DANGER_REASON } from '@/components/ConfirmDangerDialog'
import type { RelayPoolRef, RelayPoolSummary } from '@/api/types'

/** "eu-1 · eu-central-1 · 2 eligible relays": what a picker needs to choose a pool. */
export function poolOptionLabel(p: RelayPoolSummary): string {
  const parts = [p.name]
  if (p.region) parts.push(p.region)
  if (p.residency) parts.push('residency (routing only)')
  parts.push(`${p.eligible_relays} eligible relay${p.eligible_relays === 1 ? '' : 's'}`)
  return parts.join(' · ')
}

/** Characters as the server counts them (Go utf8.RuneCountInString), after trimming. */
export function reasonLength(s: string): number {
  return Array.from(s.trim()).length
}

/** A required reason is present and within the server's 500-character limit. */
export function reasonValid(s: string): boolean {
  const n = reasonLength(s)
  return n > 0 && n <= MAX_DANGER_REASON
}

/**
 * Tenants use the pool (as primary or failover) but none of its relays is
 * eligible: primary tenants without a working failover have no relay path
 * (D-38 fails closed), and failover tenants have nothing to fail over to.
 */
export function poolStranded(p: Pick<RelayPoolSummary, 'tenants' | 'failover_tenants' | 'eligible_relays'>): boolean {
  return p.tenants + p.failover_tenants > 0 && p.eligible_relays === 0
}

/**
 * Why `failover` can't be the failover pool of `primary` (D-39), or null when
 * it can: a different pool, exactly the same region (two pools without a
 * region match), a residency primary needs a residency failover, and at
 * least one eligible relay. Mirrors the controller, which checks again.
 */
export function failoverProblem(
  primary: Pick<RelayPoolRef, 'id' | 'region' | 'residency'>,
  failover: Pick<RelayPoolSummary, 'id' | 'region' | 'residency' | 'eligible_relays'>,
): string | null {
  if (primary.id === failover.id) return 'the primary pool'
  if ((primary.region ?? null) !== (failover.region ?? null)) return 'another region'
  if (primary.residency && !failover.residency) return 'not a residency pool'
  if (failover.eligible_relays === 0) return 'no eligible relay'
  return null
}
