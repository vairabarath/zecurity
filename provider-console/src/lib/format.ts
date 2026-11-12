import type { AgentCounts } from '@/api/types'

/** A timestamp in the viewer's locale and time zone; "—" when absent. */
export function formatDateTime(value: string | number | null | undefined): string {
  if (value === null || value === undefined || value === '') return '—'
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleString()
}

/** A calendar date in the viewer's locale; "—" when absent. */
export function formatDate(value: string | null | undefined): string {
  if (!value) return '—'
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? '—' : d.toLocaleDateString()
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

const SCOPES: Record<string, string> = {
  public: 'Public',
  private: 'Private',
  loopback: 'Loopback',
  link_local: 'Link-local',
  unknown: 'Unknown',
}

/** A relay address_scope for display; "—" when absent, raw when unknown. */
export function formatScope(scope: string | null | undefined): string {
  if (!scope) return '—'
  return SCOPES[scope] ?? scope
}

/** "10 minutes", "24 hours": a CRL validity in words (CA hierarchy, F15). */
export function validityText(seconds: number): string {
  if (seconds % 3600 === 0) {
    const h = seconds / 3600
    return `${h} ${h === 1 ? 'hour' : 'hours'}`
  }
  if (seconds % 60 === 0) {
    const m = seconds / 60
    return `${m} ${m === 1 ? 'minute' : 'minutes'}`
  }
  return `${seconds} seconds`
}

const RATE_UNITS = ['B/s', 'kB/s', 'MB/s', 'GB/s', 'TB/s']

/** "1.2 MB/s": bytes per second in decimal (SI) units, as reported (F5). */
export function formatRate(bytesPerSec: number): string {
  let v = Math.max(0, bytesPerSec)
  let i = 0
  while (v >= 1000 && i < RATE_UNITS.length - 1) {
    v /= 1000
    i++
  }
  const digits = i === 0 || v >= 100 ? 0 : 1
  return `${v.toLocaleString(undefined, { maximumFractionDigits: digits, minimumFractionDigits: digits })} ${RATE_UNITS[i]}`
}

/** "5 minutes", "30 seconds": an averaging window in words, rounded (F5). */
export function windowText(seconds: number): string {
  const s = Math.round(seconds)
  if (s >= 90) {
    const m = Math.round(s / 60)
    return `${m} ${m === 1 ? 'minute' : 'minutes'}`
  }
  return `${s} ${s === 1 ? 'second' : 'seconds'}`
}
