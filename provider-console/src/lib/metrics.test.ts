import { describe, expect, it } from 'vitest'
import type { MetricSeriesData } from '@/api/metrics'
import { formatMetric, nearestIndex, niceMax, rangeWindow, segments, sumSeries, summarize } from './metrics'

// Sprint 24 M2 (F23, D-49): a missing point is a gap, never a line across it
// and never zero.

const p = (ts: string, value: number, extra: object = {}) => ({ ts, value, ...extra })

describe('segments', () => {
  it('breaks the line where a minute is missing', () => {
    const runs = segments([p('2026-11-21T10:00:00Z', 1), p('2026-11-21T10:01:00Z', 2), p('2026-11-21T10:05:00Z', 3), p('2026-11-21T10:06:00Z', 4)], '1m')
    expect(runs.map((r) => r.map((x) => x.v))).toEqual([[1, 2], [3, 4]])
  })

  it('uses the hourly step for hourly points and keeps min and max', () => {
    const runs = segments([p('2026-11-21T10:00:00Z', 5, { min: 1, max: 9 }), p('2026-11-21T11:00:00Z', 6, { min: 2, max: 8 }), p('2026-11-21T14:00:00Z', 7)], '1h')
    expect(runs).toHaveLength(2)
    expect(runs[0][0]).toMatchObject({ v: 5, min: 1, max: 9 })
  })

  it('skips unparseable points instead of drawing them', () => {
    expect(segments([p('nonsense', 1), p('2026-11-21T10:00:00Z', Number.NaN)], '1m')).toEqual([])
  })
})

describe('range windows', () => {
  it('uses 1-minute points up to 24 hours and hourly points beyond', () => {
    const now = new Date('2026-11-21T12:00:42.500Z')
    expect(rangeWindow('1h', now)).toEqual({ from: '2026-11-21T11:00:42Z', to: '2026-11-21T12:00:42Z', step: '1m' })
    expect(rangeWindow('24h', now).step).toBe('1m')
    expect(rangeWindow('7d', now)).toMatchObject({ from: '2026-11-14T12:00:42Z', step: '1h' })
    expect(rangeWindow('30d', now).step).toBe('1h')
  })
})

describe('helpers', () => {
  it('nearestIndex finds the closest point in time', () => {
    const pts = [{ t: 0, v: 1 }, { t: 60_000, v: 2 }, { t: 120_000, v: 3 }]
    expect(nearestIndex(pts, 70_000)).toBe(1)
    expect(nearestIndex([], 5)).toBe(-1)
  })

  it('summarize reports latest, low and high (hourly min and max count)', () => {
    expect(summarize([p('a', 4), p('b', 2), p('c', 3)])).toEqual({ latest: 3, min: 2, max: 4 })
    expect(summarize([p('a', 5, { min: 1, max: 12 })])).toEqual({ latest: 5, min: 1, max: 12 })
    expect(summarize([])).toBeNull()
  })

  it('niceMax rounds the axis up', () => {
    expect(niceMax(0)).toBe(1)
    expect(niceMax(7)).toBe(10)
    expect(niceMax(120)).toBe(200)
    expect(niceMax(125_000_000)).toBe(200_000_000)
  })

  it('formatMetric uses the B2 rate formatting for bytes and whole counts otherwise', () => {
    expect(formatMetric('bytes_per_sec', 15_000_000)).toBe('15.0 MB/s')
    expect(formatMetric('count', 3.4)).toBe('3')
  })
})

describe('sumSeries (both directions against the link capacity, D-42)', () => {
  const series = (points: MetricSeriesData['points']): MetricSeriesData => ({ series: 's', subject: 'r', unit: 'bytes_per_sec', step: '1m', from: 'f', to: 't', points })
  it('adds only where both have a point; a gap in either stays a gap', () => {
    const a = series([p('2026-11-21T10:00:00Z', 60), p('2026-11-21T10:01:00Z', 61), p('2026-11-21T10:02:00Z', 62)])
    const b = series([p('2026-11-21T10:00:00Z', 50), p('2026-11-21T10:02:00Z', 52)])
    expect(sumSeries(a, b, 'both').points).toEqual([p('2026-11-21T10:00:00Z', 110), p('2026-11-21T10:02:00Z', 114)])
  })
  it('drops hourly min and max (the sum of minimums is not the minimum of the sum)', () => {
    const a = series([p('2026-11-21T10:00:00Z', 6, { min: 1, max: 9 })])
    const out = sumSeries(a, a, 'both').points[0]
    expect(out).toEqual({ ts: '2026-11-21T10:00:00Z', value: 12 })
  })
})
