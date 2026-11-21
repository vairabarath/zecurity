import { useState } from 'react'
import { getMetricSeries, type MetricSeriesData } from '@/api/metrics'
import { rangeWindow, type MetricRange, type MetricWindow } from '@/lib/metrics'
import { useDetailQuery, type DetailState } from '@/lib/useDetailQuery'

// One stored metric series over a window (Sprint 24 M2, F23). Read once when
// the chart appears and again when the window changes (range change or page
// Refresh): the console's no-polling discipline (useDetailQuery). Metric
// reads are not audited, so changing the range on tenant detail writes no
// tenant.read entry.

export type MetricState = DetailState<MetricSeriesData>

/**
 * The window for `range`, taken once per range and `reset` (the page's
 * Refresh counter, or anything else that should start a new window) so
 * every chart on a page reads the same window: one set of x-axis bounds,
 * and the hover compares like with like. Held in state, not a memo: React
 * may drop a memo, and a new window would send every chart's read again.
 */
export function useMetricWindow(range: MetricRange, reset: string | number = 0): MetricWindow {
  const key = `${range}|${reset}`
  const [held, setHeld] = useState(() => ({ key, win: rangeWindow(range, new Date()) }))
  if (held.key === key) return held.win
  // A new range or reset: adjust state during render (React's pattern for
  // state derived from props).
  const next = { key, win: rangeWindow(range, new Date()) }
  setHeld(next)
  return next.win
}

const skip = () => Promise.resolve(null)

/**
 * `series` null: not read yet (e.g. the role isn't known); stays loading.
 * `round` is the page's Refresh counter: a Refresh within the same second
 * yields the same window but must still read once more.
 */
export function useMetricSeries(series: string | null, subject: string | undefined, window: MetricWindow, round = 0): MetricState {
  const key = series ? `${series}|${subject ?? ''}|${window.from}|${window.to}|${window.step}|${round}` : 'none'
  const { state } = useDetailQuery<MetricSeriesData | null>(key, series ? () => getMetricSeries(series, { subject, ...window }) : skip)
  if (!series || (state.status === 'ready' && state.data === null)) return { status: 'loading' }
  return state as MetricState
}
