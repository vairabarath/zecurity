import type { HealthResult, HealthStatus } from '@/api/types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card } from '@/components/ui/card'
import { formatDateTime } from '@/lib/format'
import type { DetailState } from '@/lib/useDetailQuery'

// Control-plane health on Overview (quick-win F25, health.read: super-admin).
//
// Renders GET /provider/health as returned: per component its status,
// whether it is critical, its latency and the server's message. The server
// already reduced every message to a fixed check message or "check failed";
// nothing here adds detail, and raw errors are never shown.

const LABEL: Record<HealthStatus, string> = { healthy: 'Healthy', degraded: 'Degraded', unhealthy: 'Unhealthy' }
const VARIANT: Record<HealthStatus, 'success' | 'warning' | 'destructive'> = {
  healthy: 'success',
  degraded: 'warning',
  unhealthy: 'destructive',
}

export function HealthBadge({ status }: { status: HealthStatus }) {
  return (
    <Badge variant={VARIANT[status]} data-health={status}>
      {LABEL[status]}
    </Badge>
  )
}

export function ControlPlaneHealth({ state, onRetry }: { state: DetailState<HealthResult | null>; onRetry: () => void }) {
  return (
    <Card role="region" aria-labelledby="health-heading">
      <div className="flex items-center justify-between gap-3 border-b border-border px-5 py-4">
        <div>
          <h2 id="health-heading" className="font-semibold">
            Control plane
          </h2>
          {state.status === 'ready' && state.data && (
            <p className="text-[13px] text-muted-foreground">Checked {formatDateTime(state.data.checked_at)}</p>
          )}
        </div>
        {state.status === 'ready' && state.data && <HealthBadge status={state.data.status} />}
      </div>
      <div className="px-5 py-3">
        {state.status === 'loading' && <p className="text-sm text-muted-foreground">Checking…</p>}
        {(state.status === 'error' || state.status === 'not_found' || state.status === 'forbidden') && (
          <p className="text-sm text-destructive">
            Couldn’t load control-plane health.{' '}
            <Button variant="link" className="h-auto p-0" onClick={onRetry}>
              Retry
            </Button>
          </p>
        )}
        {state.status === 'ready' && state.data && (
          <ul aria-label="Control-plane components" className="flex flex-col divide-y divide-border">
            {state.data.components.map((c) => (
              <li key={c.name} aria-label={c.name} className="flex flex-col gap-0.5 py-2">
                <div className="flex items-center justify-between gap-2">
                  <span className="font-medium capitalize">{c.name}</span>
                  <HealthBadge status={c.status} />
                </div>
                <p className="text-xs text-muted-foreground">
                  {c.critical ? 'Critical' : 'Not critical'} · {c.latency_ms.toLocaleString(undefined, { maximumFractionDigits: 1 })} ms
                  {c.message && <> · {c.message}</>}
                </p>
              </li>
            ))}
          </ul>
        )}
      </div>
    </Card>
  )
}
