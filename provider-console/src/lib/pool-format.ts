import { MAX_DANGER_REASON } from '@/components/ConfirmDangerDialog'
import type { RelayPoolSummary } from '@/api/types'

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

/** Tenants use the pool but none of its relays is eligible: their relay path is down (D-38 fails closed). */
export function poolStranded(p: Pick<RelayPoolSummary, 'tenants' | 'eligible_relays'>): boolean {
  return p.tenants > 0 && p.eligible_relays === 0
}
