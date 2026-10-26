import { Badge } from '@/components/ui/badge'
import { cn } from '@/lib/utils'

// Relay capacity, shown exactly as the API reports it: connection_count of
// max_connections, and the capacity_label the controller publishes to
// connectors for relay selection (high = the most spare capacity).

const HIGH_USE_PCT = 85

/** "12 / 500" with a usage bar; amber from 85% used. No max → "Not reported". */
export function ConnectionsBar({ count, max }: { count: number; max: number }) {
  if (max <= 0) return <span className="text-muted-foreground">Not reported</span>
  const pct = Math.min(100, Math.round((count / max) * 100))
  return (
    <div className="flex flex-col gap-1.5">
      <div className="flex justify-between gap-3 text-xs">
        <span className="font-mono tabular-nums">
          {count.toLocaleString()} / {max.toLocaleString()}
        </span>
        <span className="text-muted-foreground">{pct}%</span>
      </div>
      <div className="h-[5px] rounded-full bg-border" aria-hidden>
        <div className={cn('h-[5px] rounded-full', pct >= HIGH_USE_PCT ? 'bg-warning' : pct > 0 ? 'bg-secure' : 'bg-neutral')} style={{ width: `${pct}%` }} />
      </div>
    </div>
  )
}

const TIER_VARIANT: Record<string, 'success' | 'warning' | 'destructive'> = { high: 'success', medium: 'warning', low: 'destructive' }

/** The published capacity tier; an unknown value is shown as-is, neutral. */
export function CapacityTier({ label }: { label: string }) {
  return (
    <Badge variant={TIER_VARIANT[label] ?? 'secondary'} className="whitespace-nowrap capitalize" data-tier={label}>
      {label}
    </Badge>
  )
}
