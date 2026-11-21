import type { RelaySummary } from '@/api/types'
import { Card } from '@/components/ui/card'
import { MetricChart, RangePicker } from '@/components/MetricChart'
import { formatLinkSpeed } from '@/lib/capacity'
import { sumSeries, type MetricRange } from '@/lib/metrics'
import { useMetricSeries, type MetricState } from '@/lib/useMetricSeries'

// Relay detail "History" (Sprint 24 M2, F23): stored connections and
// bandwidth over a range. Relay series: both roles (relay.metrics.read).
// With a link capacity (B2), a "Both directions" line is drawn against it:
// both flows share the link (D-42), so the capacity bounds their sum, never
// one direction alone.

function both(a: MetricState, b: MetricState): MetricState {
  if (a.status !== 'ready') return a
  if (b.status !== 'ready') return b
  return { status: 'ready', data: sumSeries(a.data, b.data, 'relay.bytes_both_directions_per_sec') }
}

export function RelayHistory({
  relay,
  range,
  onRange,
}: {
  relay: Pick<RelaySummary, 'id' | 'link_capacity_bps'>
  range: MetricRange
  onRange: (r: MetricRange) => void
}) {
  const conns = useMetricSeries('relay.connections', relay.id, range)
  const toClients = useMetricSeries('relay.bytes_to_clients_per_sec', relay.id, range)
  const toConnectors = useMetricSeries('relay.bytes_to_connectors_per_sec', relay.id, range)
  const cap = relay.link_capacity_bps
  return (
    <Card role="region" aria-labelledby="relay-history" className="flex flex-col gap-5 px-5 py-5">
      <div className="flex flex-wrap items-center justify-between gap-3">
        <div className="flex flex-col gap-0.5">
          <h2 id="relay-history" className="font-semibold">
            History
          </h2>
          <p className="text-[13px] text-muted-foreground">Stored every minute. A break in a line means the relay wasn’t reporting.</p>
        </div>
        <RangePicker value={range} onChange={onRange} />
      </div>
      <MetricChart title="Connections" unit="count" range={range} lines={[{ label: 'Connections', state: conns }]} />
      <MetricChart
        title="Bandwidth"
        unit="bytes_per_sec"
        range={range}
        lines={[
          { label: 'To clients', state: toClients },
          { label: 'To connectors', state: toConnectors, tone: 'info' },
          ...(cap !== null ? [{ label: 'Both directions', state: both(toClients, toConnectors), tone: 'warning' as const }] : []),
        ]}
        ceiling={cap !== null ? { value: cap, label: `Link capacity ${formatLinkSpeed(cap)}` } : null}
      />
    </Card>
  )
}
