import { Badge, type BadgeProps } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

type Variant = NonNullable<BadgeProps['variant']>
type Tone = 'ok' | 'warn' | 'crit' | 'info' | 'neutral'

// Status values the read APIs return (relays, tenants, connectors, shields,
// remote networks). The badge only colours the API's own value; it never
// derives a status.
const TONES: Record<string, Tone> = {
  active: 'ok',
  pending: 'info',
  provisioning: 'info',
  inactive: 'warn',
  disconnected: 'warn',
  suspended: 'warn',
  revoked: 'crit',
  deleted: 'neutral',
}

const VARIANT: Record<Tone, Variant> = { ok: 'success', warn: 'warning', crit: 'destructive', info: 'info', neutral: 'secondary' }
const DOT: Record<Tone, string> = { ok: 'bg-secure', warn: 'bg-warning', crit: 'bg-critical', info: 'bg-info', neutral: 'bg-neutral' }

/** A lifecycle status as a badge; an unknown value is shown as-is, neutral. */
export function StatusBadge({ status, className }: { status: string; className?: string }) {
  const tone = TONES[status] ?? 'neutral'
  return (
    <Badge variant={VARIANT[tone]} className={cn('whitespace-nowrap capitalize', className)} data-status={status} data-tone={tone}>
      <span aria-hidden className={cn('size-1.5 shrink-0 rounded-full', DOT[tone])} />
      {status}
    </Badge>
  )
}
