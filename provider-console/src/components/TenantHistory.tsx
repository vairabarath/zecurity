import { Card } from '@/components/ui/card'
import { MetricChart, RangePicker } from '@/components/MetricChart'
import type { MetricRange } from '@/lib/metrics'
import { useMetricSeries, useMetricWindow } from '@/lib/useMetricSeries'

// Tenant detail "History" (Sprint 24 M2, F23): stored sessions, agents
// online and the tenant's relay traffic (R1). Tenant series are super-admin
// only (tenant.metrics.read, D-40 extended), like the page. Metric reads
// are not audited: changing the range writes no tenant.read entry.

export function TenantHistory({ tenantId, range, onRange }: { tenantId: string; range: MetricRange; onRange: (r: MetricRange) => void }) {
  const win = useMetricWindow(range)
  const sessions = useMetricSeries('tenant.active_sessions', tenantId, win)
  const connectors = useMetricSeries('tenant.connectors_online', tenantId, win)
  const shields = useMetricSeries('tenant.shields_online', tenantId, win)
  const toClients = useMetricSeries('tenant.relay_bytes_to_clients_per_sec', tenantId, win)
  const toConnectors = useMetricSeries('tenant.relay_bytes_to_connectors_per_sec', tenantId, win)
  return (
    <Card role="region" aria-labelledby="tenant-history" className="flex flex-col gap-5 px-5 py-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-col gap-0.5">
          <h2 id="tenant-history" className="font-semibold">
            History
          </h2>
          <p className="text-[13px] text-muted-foreground">Stored every minute. Changing the range doesn’t record an audit entry.</p>
        </div>
        <RangePicker value={range} onChange={onRange} />
      </div>
      <MetricChart title="Active sessions" unit="count" range={range} lines={[{ label: 'Active sessions', state: sessions }]} />
      <MetricChart
        title="Agents online"
        unit="count"
        range={range}
        lines={[
          { label: 'Connectors', state: connectors },
          { label: 'Shields', state: shields, tone: 'info' },
        ]}
      />
      <MetricChart
        title="Relay traffic"
        unit="bytes_per_sec"
        range={range}
        lines={[
          { label: 'To clients', state: toClients },
          { label: 'To connectors', state: toConnectors, tone: 'info' },
        ]}
        empty="Not reported: no relay reported this tenant’s traffic in this range."
      />
    </Card>
  )
}
