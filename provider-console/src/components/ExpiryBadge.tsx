import type { ExpiryBucket } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { BUCKET_LABELS, BUCKET_VARIANTS, bucketFor } from '@/lib/expiry'
import { formatDateTime } from '@/lib/format'
import { cn } from '@/lib/utils'

const BUCKET_DOTS: Record<ExpiryBucket, string> = {
  expired: 'bg-critical',
  lt_24h: 'bg-critical',
  lt_7d: 'bg-warning',
  lt_30d: 'bg-info',
  ok: 'bg-secure',
}

/**
 * A certificate expiry as a coloured badge. Pass the server's `bucket` when
 * the API provides one (Certificates page); otherwise it is computed from
 * the date with the server's exact boundaries. No date → "—".
 */
export function ExpiryBadge({
  notAfter,
  bucket,
  now,
}: {
  notAfter: string | null
  bucket?: ExpiryBucket
  now?: Date
}) {
  if (!notAfter) return <span className="text-muted-foreground">—</span>
  const b = bucket ?? bucketFor(notAfter, now)
  if (!b) return <span className="text-muted-foreground">—</span>
  return (
    <Badge variant={BUCKET_VARIANTS[b]} className="whitespace-nowrap" title={formatDateTime(notAfter)} data-bucket={b}>
      <span aria-hidden className={cn('size-1.5 shrink-0 rounded-full', BUCKET_DOTS[b])} />
      {BUCKET_LABELS[b]}
    </Badge>
  )
}
