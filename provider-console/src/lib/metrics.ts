import type { MetricPoint, MetricSeriesData, MetricStep, MetricUnit } from '@/api/metrics'
import { formatRate } from '@/lib/format'

// Chart helpers for stored metric series (Sprint 24 M2, F23). Pure, so the
// gap and range rules are tested without rendering.

export type MetricRange = '1h' | '24h' | '7d' | '30d'

export interface RangeDef {
  key: MetricRange
  label: string
  ms: number
  step: MetricStep
}

const MIN = 60_000
const HOUR = 60 * MIN

// 1-minute points up to 24 h (≤ 1,440 points), hourly beyond (D-49:
// 1-minute samples are kept 7 days).
export const RANGES: readonly RangeDef[] = [
  { key: '1h', label: '1 h', ms: HOUR, step: '1m' },
  { key: '24h', label: '24 h', ms: 24 * HOUR, step: '1m' },
  { key: '7d', label: '7 d', ms: 7 * 24 * HOUR, step: '1h' },
  { key: '30d', label: '30 d', ms: 30 * 24 * HOUR, step: '1h' },
]

export const DEFAULT_RANGE: MetricRange = '24h'

export function rangeDef(key: MetricRange): RangeDef {
  return RANGES.find((r) => r.key === key) ?? RANGES[1]
}

export function stepMs(step: MetricStep): number {
  return step === '1h' ? HOUR : MIN
}

export interface MetricWindow {
  from: string
  to: string
  step: MetricStep
}

/** The window to request for `range`, ending now (seconds dropped). */
export function rangeWindow(range: MetricRange, now: Date): MetricWindow {
  const def = rangeDef(range)
  const to = new Date(Math.floor(now.getTime() / 1000) * 1000)
  return { from: new Date(to.getTime() - def.ms).toISOString().replace('.000Z', 'Z'), to: to.toISOString().replace('.000Z', 'Z'), step: def.step }
}

export interface XY {
  t: number
  v: number
  min?: number
  max?: number
}

/**
 * Splits points into runs with no gap: a missing step starts a new run, so a
 * chart draws a break where the value was unknown, never a line across it
 * (and never a drop to zero).
 */
export function segments(points: readonly MetricPoint[], step: MetricStep): XY[][] {
  const gap = stepMs(step) * 1.5
  const out: XY[][] = []
  let run: XY[] = []
  let last = -Infinity
  for (const p of points) {
    const t = Date.parse(p.ts)
    if (Number.isNaN(t) || !Number.isFinite(p.value)) continue
    if (t - last > gap && run.length > 0) {
      out.push(run)
      run = []
    }
    run.push({ t, v: p.value, min: p.min, max: p.max })
    last = t
  }
  if (run.length > 0) out.push(run)
  return out
}

/** Index of the point closest in time to t, or -1 for no points. */
export function nearestIndex(points: readonly XY[], t: number): number {
  let best = -1
  let bestD = Infinity
  points.forEach((p, i) => {
    const d = Math.abs(p.t - t)
    if (d < bestD) {
      bestD = d
      best = i
    }
  })
  return best
}

export interface SeriesSummary {
  latest: number
  min: number
  max: number
}

/** Latest, min and max of the stored values (hourly points use their min and max). */
export function summarize(points: readonly MetricPoint[]): SeriesSummary | null {
  const finite = points.filter((p) => Number.isFinite(p.value))
  if (finite.length === 0) return null
  let min = Infinity
  let max = -Infinity
  for (const p of finite) {
    min = Math.min(min, p.min ?? p.value)
    max = Math.max(max, p.max ?? p.value)
  }
  return { latest: finite[finite.length - 1].value, min, max }
}

/** A "nice" upper bound for the y axis: at least `floor`, rounded up. */
export function niceMax(value: number, floor = 1): number {
  const v = Math.max(value, floor)
  const exp = Math.pow(10, Math.floor(Math.log10(v)))
  for (const m of [1, 2, 2.5, 5, 10]) {
    if (m * exp >= v) return m * exp
  }
  return 10 * exp
}

/** A value in its unit: a rate (B2 formatting) or a whole count. */
export function formatMetric(unit: MetricUnit, v: number): string {
  return unit === 'bytes_per_sec' ? formatRate(v) : Math.round(v).toLocaleString()
}

/**
 * Whether a relay's "Both directions" line and link-capacity ceiling are
 * drawn for `range`: 1-minute ranges only. An hourly point is the mean of
 * the minutes that had a sample, so a sum of hourly means hides the minutes
 * the link was full and would show headroom that wasn't there.
 */
export function showsLinkCeiling(range: MetricRange): boolean {
  return rangeDef(range).step === '1m'
}

/**
 * Two series of the same step added point by point, only where both have a
 * point at the same time (a gap in either stays a gap). Hourly sums carry no
 * min / max: the sum of two minimums isn't the minimum of the sum. Used for
 * a relay's traffic in both directions against its link capacity (B2, D-42:
 * both flows share the link).
 */
export function sumSeries(a: MetricSeriesData, b: MetricSeriesData, name: string): MetricSeriesData {
  const other = new Map(b.points.map((p) => [Date.parse(p.ts), p.value]))
  const points = a.points.flatMap((p) => {
    const v = other.get(Date.parse(p.ts))
    return v === undefined ? [] : [{ ts: p.ts, value: p.value + v }]
  })
  return { ...a, series: name, points }
}
