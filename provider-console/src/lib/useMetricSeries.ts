import { getMetricSeries, type MetricSeriesData } from '@/api/metrics'
import { rangeWindow, type MetricRange } from '@/lib/metrics'
import { useDetailQuery, type DetailState } from '@/lib/useDetailQuery'

// One stored metric series over a range (Sprint 24 M2, F23). Read once when
// the chart appears and again when the range or `round` (page Refresh)
// changes: the console's no-polling discipline (useDetailQuery). Metric
// reads are not audited, so changing the range on tenant detail writes no
// tenant.read entry.

export type MetricState = DetailState<MetricSeriesData>

export function useMetricSeries(series: string, subject: string | undefined, range: MetricRange, round = 0): MetricState {
  const key = `${series}|${subject ?? ''}|${range}|${round}`
  return useDetailQuery(key, () => getMetricSeries(series, { subject, ...rangeWindow(range, new Date()) })).state
}
