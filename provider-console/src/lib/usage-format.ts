import type { HourlyMeter, TenantMonthUsage, TenantUsage, UsageMeter } from '@/api/usage'
import { csvCell } from '@/lib/auditExport'

// Usage display and export (Sprint 25 U3, F33). Units: bytes in decimal
// units (1 GB = 10^9 bytes, as the relay rates); minutes shown as hours;
// active users as a count. All months and days are UTC (D-57).

export const METER_LABELS: Record<UsageMeter, string> = {
  relay_bytes_to_clients: 'Relayed to clients',
  relay_bytes_to_connectors: 'Relayed to connectors',
  session_minutes: 'Session hours',
  connector_minutes: 'Connector hours',
  shield_minutes: 'Shield hours',
  active_users: 'Active users',
}

const BYTE_UNITS = ['B', 'kB', 'MB', 'GB', 'TB', 'PB']

export function formatBytes(bytes: number): string {
  let v = Math.max(0, bytes)
  let i = 0
  while (v >= 1000 && i < BYTE_UNITS.length - 1) {
    v /= 1000
    i++
  }
  const digits = i === 0 || v >= 100 ? 0 : 1
  return `${v.toLocaleString(undefined, { maximumFractionDigits: digits, minimumFractionDigits: digits })} ${BYTE_UNITS[i]}`
}

/** Minutes as hours, one decimal ("12.5 h"). */
export function formatMinutesAsHours(minutes: number): string {
  return `${(Math.max(0, minutes) / 60).toLocaleString(undefined, { maximumFractionDigits: 1, minimumFractionDigits: 1 })} h`
}

export function formatMeter(meter: UsageMeter, value: number): string {
  if (meter === 'relay_bytes_to_clients' || meter === 'relay_bytes_to_connectors') return formatBytes(value)
  if (meter === 'active_users') return Math.round(value).toLocaleString()
  return formatMinutesAsHours(value)
}

/** "YYYY-MM" of a date, in UTC. */
export function monthKey(d: Date): string {
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}`
}

/** The first instant of a "YYYY-MM" month (UTC), or null if malformed. */
export function monthStart(key: string): Date | null {
  const m = /^(\d{4})-(0[1-9]|1[0-2])$/.exec(key)
  return m ? new Date(Date.UTC(Number(m[1]), Number(m[2]) - 1, 1)) : null
}

export function addMonths(d: Date, n: number): Date {
  return new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth() + n, 1))
}

/** "November 2026" (UTC). */
export function monthLabel(d: Date): string {
  return d.toLocaleDateString(undefined, { month: 'long', year: 'numeric', timeZone: 'UTC' })
}

const INCOMPLETE_NOTE =
  'complete=false means the collector did not count every minute of the period (e.g. the controller was down): its usage is at least the figures shown, never an estimate.'

/** A tenant's buckets as CSV: one row per bucket, raw quantities; the
 * complete column marks incomplete buckets (D-59). */
export function tenantUsageCSV(u: TenantUsage, meters: readonly HourlyMeter[]): string {
  const withUsers = u.granularity === 'month'
  const head = ['start', 'complete', 'collected_minutes', 'minutes', ...meters, ...(withUsers ? ['active_users'] : [])]
  const lines = [head.join(',')]
  for (const b of u.buckets) {
    lines.push(
      [b.start, b.complete, b.collected_minutes, b.minutes, ...meters.map((m) => b.meters[m] ?? 0), ...(withUsers ? [b.active_users ?? 0] : [])]
        .map(csvCell)
        .join(','),
    )
  }
  return lines.join('\r\n') + '\r\n'
}

export function tenantUsageJSON(u: TenantUsage): string {
  return JSON.stringify({ note: INCOMPLETE_NOTE, ...u }, null, 2)
}

/** The fleet for a month as CSV: one row per tenant, raw quantities, and
 * the month's completeness on every row (D-59). */
export function fleetUsageCSV(month: string, complete: boolean, rows: readonly TenantMonthUsage[], meters: readonly HourlyMeter[]): string {
  const head = ['month', 'month_complete', 'tenant_id', 'tenant_name', 'tenant_slug', 'deleted', 'plan', ...meters, 'active_users']
  const lines = [head.join(',')]
  for (const r of rows) {
    lines.push(
      [month, complete, r.tenant.id, r.tenant.name, r.tenant.slug, r.tenant.purged, r.tenant.plan_name ?? '', ...meters.map((m) => r.meters[m] ?? 0), r.active_users]
        .map(csvCell)
        .join(','),
    )
  }
  return lines.join('\r\n') + '\r\n'
}

export function fleetUsageJSON(month: string, complete: boolean, rows: readonly TenantMonthUsage[]): string {
  return JSON.stringify({ month, complete, note: INCOMPLETE_NOTE, tenants: rows }, null, 2)
}

// Usage alert thresholds (Sprint 25 U4, D-61): entered in display units
// (GB for bytes, hours for minutes, users as a count), stored in the
// meter's raw unit (bytes, minutes, users).
const MAX_THRESHOLD = 1e18

export function thresholdUnit(meter: UsageMeter): string {
  if (meter === 'relay_bytes_to_clients' || meter === 'relay_bytes_to_connectors') return 'GB'
  if (meter === 'active_users') return 'users'
  return 'hours'
}

function unitFactor(meter: UsageMeter): number {
  const u = thresholdUnit(meter)
  return u === 'GB' ? 1e9 : u === 'hours' ? 60 : 1
}

/** A raw threshold in the display unit, e.g. 500000000000 bytes → "500". */
export function thresholdToDisplay(meter: UsageMeter, raw: number): string {
  return String(Math.round((raw / unitFactor(meter)) * 1000) / 1000)
}

/** The raw threshold for a display value, or null when invalid (not a
 * positive number, users not whole, or out of range). */
export function parseThreshold(meter: UsageMeter, input: string): number | null {
  const t = input.trim()
  if (!/^\d{1,15}(\.\d{1,3})?$/.test(t)) return null
  if (meter === 'active_users' && t.includes('.')) return null
  const raw = Math.round(Number(t) * unitFactor(meter))
  return raw >= 1 && raw <= MAX_THRESHOLD ? raw : null
}
