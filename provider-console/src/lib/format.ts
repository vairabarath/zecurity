import type { AgentCounts } from '@/api/types'

/** A timestamp in the viewer's locale and time zone; "—" when absent. */
export function formatDateTime(value: string | number | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—'
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

/** Remaining time as m:ss (never negative). */
export function formatRemaining(ms: number): string {
  const total = Math.max(0, Math.ceil(ms / 1000))
  const m = Math.floor(total / 60)
  const s = total % 60
  return `${m}:${String(s).padStart(2, '0')}`
}

/** "Try again in N minutes." from a Retry-After value (rounded up, at least 1). */
export function tryAgainIn(retryAfterSeconds: number | undefined): string {
  if (retryAfterSeconds === undefined) return 'Try again later.'
  const m = Math.max(1, Math.ceil(retryAfterSeconds / 60))
  return `Try again in ${m} minute${m === 1 ? '' : 's'}.`
}

/** "3 active · 1 disconnected": the API's own counts, zero entries left out. */
export function agentSummary(c: AgentCounts): string {
  const parts = (['active', 'pending', 'disconnected', 'revoked'] as const).filter((k) => c[k] > 0).map((k) => `${c[k]} ${k}`)
  return parts.length ? parts.join(' · ') : '0'
}
