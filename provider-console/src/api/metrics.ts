import { request } from './client'
import { buildQuery } from './reads'

// Stored metric series (Sprint 24 M1/M2, F23, D-49). GET only. Relay and
// fleet relay series: both roles; tenant series and fleet totals of tenant
// data: super-admin only (the controller decides, D-40 extended). A missing
// point is a gap (the value was unknown), never zero.

export type MetricStep = '1m' | '1h'
export type MetricUnit = 'count' | 'bytes_per_sec'

export interface MetricPoint {
  ts: string
  /** The sample, or the hour's mean for hourly points. */
  value: number
  /** Hourly points only. */
  min?: number
  max?: number
  /** Minutes in the hour that had a sample. */
  count?: number
}

export interface MetricSeriesData {
  series: string
  subject: string | null
  unit: MetricUnit
  step: MetricStep
  from: string
  to: string
  points: MetricPoint[]
}

export interface MetricParams {
  subject?: string
  from?: string
  to?: string
  step?: MetricStep
}

const METRIC_KEYS = ['subject', 'from', 'to', 'step'] as const

export function getMetricSeries(series: string, params?: MetricParams): Promise<MetricSeriesData> {
  return request<MetricSeriesData>('GET', `/provider/metrics/${encodeURIComponent(series)}` + buildQuery(params, METRIC_KEYS))
}
