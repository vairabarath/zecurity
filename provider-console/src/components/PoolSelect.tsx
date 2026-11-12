import type { RelayPoolSummary } from '@/api/types'
import { poolOptionLabel } from '@/lib/pool-format'
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from '@/components/ui/select'

// A pool picker over an already-loaded list. `disabledReason` greys out a
// pool and says why (e.g. no eligible relay); the controller checks again.
export function PoolSelect({
  id,
  label,
  pools,
  value,
  onChange,
  disabledReason,
}: {
  id: string
  label: string
  pools: RelayPoolSummary[]
  value: string
  onChange: (id: string) => void
  disabledReason?: (p: RelayPoolSummary) => string | null
}) {
  return (
    <Select value={value || undefined} onValueChange={onChange}>
      <SelectTrigger id={id} aria-label={label}>
        <SelectValue placeholder="Choose a pool" />
      </SelectTrigger>
      <SelectContent>
        {pools.map((p) => {
          const why = disabledReason?.(p) ?? null
          return (
            <SelectItem key={p.id} value={p.id} disabled={why !== null}>
              {poolOptionLabel(p)}
              {why && ` (${why})`}
            </SelectItem>
          )
        })}
      </SelectContent>
    </Select>
  )
}
