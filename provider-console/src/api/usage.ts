import { request } from './client'
import { buildQuery } from './reads'

// Tenant usage (Sprint 25 U2/U3, F33; D-57, D-59, D-60). GET only. Usage is
// tenant data: super-admin only (tenant.usage.read); every read is audited
// by the controller (tenant.usage.read / usage.fleet.read), so the console
// reads once per view and on an explicit change, never by polling.
//
// A bucket is complete when the collector counted every one of its minutes
// (D-59). An incomplete bucket's numbers are "at least this", never an
// estimate; a complete bucket without usage is 0.

export const HOURLY_METERS = [
  'relay_bytes_to_clients',
  'relay_bytes_to_connectors',
  'session_minutes',
  'connector_minutes',
  'shield_minutes',
] as const
export type HourlyMeter = (typeof HOURLY_METERS)[number]
export type UsageMeter = HourlyMeter | 'active_users'
export const USAGE_METERS: readonly UsageMeter[] = [...HOURLY_METERS, 'active_users']

export type UsageGranularity = 'hour' | 'day' | 'month'

export interface UsageTenant {
  id: string
  name: string
  slug: string
  plan_name: string | null
  /** Deleted and purged: named by the usage snapshot kept after purge (D-58). */
  purged: boolean
}

export interface UsageBucket {
  start: string
  complete: boolean
  collected_minutes: number
  minutes: number
  meters: Record<HourlyMeter, number>
  /** Month buckets only. */
  active_users?: number
}

export interface TenantUsage {
  tenant: UsageTenant
  granularity: UsageGranularity
  from: string
  to: string
  buckets: UsageBucket[]
}

export interface TenantMonthUsage {
  tenant: UsageTenant
  meters: Record<HourlyMeter, number>
  active_users: number
}

export interface FleetUsagePage {
  month: string
  complete: boolean
  collected_minutes: number
  minutes: number
  sort: UsageMeter
  items: TenantMonthUsage[]
  next_cursor: string | null
}

export interface TenantUsageParams {
  tenant_id: string
  from?: string
  to?: string
  granularity?: UsageGranularity
}

export interface FleetUsageParams {
  /** YYYY-MM */
  month?: string
  sort?: UsageMeter
  cursor?: string
  limit?: number
}

const TENANT_USAGE_KEYS = ['tenant_id', 'from', 'to', 'granularity'] as const
const FLEET_USAGE_KEYS = ['month', 'sort', 'cursor', 'limit'] as const

export function getTenantUsage(params: TenantUsageParams): Promise<TenantUsage> {
  return request<TenantUsage>('GET', '/provider/usage' + buildQuery(params, TENANT_USAGE_KEYS))
}

export function listFleetUsage(params?: FleetUsageParams): Promise<FleetUsagePage> {
  return request<FleetUsagePage>('GET', '/provider/usage/tenants' + buildQuery(params, FLEET_USAGE_KEYS))
}
