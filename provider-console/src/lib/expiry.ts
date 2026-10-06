import type { ExpiryBucket } from '@/api/types'

// Expiry buckets, mirroring the controller exactly
// (internal/providerquery/certificates.go bucketFor):
//
//   expired  not_after <= now
//   lt_24h   now        < not_after <= now + 24h
//   lt_7d    now + 24h  < not_after <= now + 7d
//   lt_30d   now + 7d   < not_after <= now + 30d
//   ok       not_after  > now + 30d
//
// Used only where the API gives a date without a bucket (relay and tenant
// certificate dates). The Certificates page shows the server's own bucket.
// This is the console's only derived "health" value (D-23).

const HOUR = 60 * 60 * 1000
const DAY = 24 * HOUR

/** The bucket for an expiry timestamp, or null if it isn't a valid date. */
export function bucketFor(notAfter: string | Date, now: Date = new Date()): ExpiryBucket | null {
  const t = typeof notAfter === 'string' ? Date.parse(notAfter) : notAfter.getTime()
  if (Number.isNaN(t)) return null
  const n = now.getTime()
  if (t <= n) return 'expired'
  if (t <= n + DAY) return 'lt_24h'
  if (t <= n + 7 * DAY) return 'lt_7d'
  if (t <= n + 30 * DAY) return 'lt_30d'
  return 'ok'
}

export const BUCKET_LABELS: Record<ExpiryBucket, string> = {
  expired: 'Expired',
  lt_24h: '< 24h',
  lt_7d: '< 7 days',
  lt_30d: '< 30 days',
  ok: 'OK',
}

/** Badge colour per bucket (one fixed mapping for every page). */
export const BUCKET_VARIANTS: Record<ExpiryBucket, 'destructive' | 'warning' | 'outline' | 'success'> = {
  expired: 'destructive',
  lt_24h: 'destructive',
  lt_7d: 'warning',
  lt_30d: 'outline',
  ok: 'success',
}

/** "just now", "5 min ago", "3 h ago", "2 days ago", "in 4 days"; "—" when absent. */
export function formatRelative(value: string | null | undefined, now: Date = new Date()): string {
  if (!value) return '—'
  const t = Date.parse(value)
  if (Number.isNaN(t)) return '—'
  const diff = t - now.getTime()
  const abs = Math.abs(diff)
  const past = diff <= 0
  let unit: string
  if (abs < 60_000) return past ? 'just now' : 'in under a minute'
  if (abs < HOUR) unit = `${Math.floor(abs / 60_000)} min`
  else if (abs < DAY) unit = `${Math.floor(abs / HOUR)} h`
  else {
    const d = Math.floor(abs / DAY)
    unit = `${d} day${d === 1 ? '' : 's'}`
  }
  return past ? `${unit} ago` : `in ${unit}`
}
