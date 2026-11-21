import { describe, expect, it } from 'vitest'
import type { TenantMonthUsage, TenantUsage } from '@/api/usage'
import { HOURLY_METERS } from '@/api/usage'
import { addMonths, fleetUsageCSV, formatBytes, formatMeter, formatMinutesAsHours, monthKey, monthStart, tenantUsageCSV } from './usage-format'

// Sprint 25 U3 (F33): units, UTC months, and exports that keep raw
// quantities, mark incomplete periods and neutralise formula cells.

const meters = (over: Partial<Record<(typeof HOURLY_METERS)[number], number>> = {}) => ({
  relay_bytes_to_clients: 0, relay_bytes_to_connectors: 0, session_minutes: 0, connector_minutes: 0, shield_minutes: 0, ...over,
})

describe('usage format', () => {
  it('bytes in decimal units, minutes as hours, users as a count', () => {
    expect(formatBytes(999)).toBe('999 B')
    expect(formatBytes(1_500_000)).toBe('1.5 MB')
    expect(formatBytes(512_000_000_000)).toBe('512 GB')
    expect(formatMinutesAsHours(90)).toBe('1.5 h')
    expect(formatMeter('session_minutes', 30)).toBe('0.5 h')
    expect(formatMeter('active_users', 3)).toBe('3')
    expect(formatMeter('relay_bytes_to_connectors', 2_000)).toBe('2.0 kB')
  })

  it('months are UTC and malformed keys are refused', () => {
    // 23:30 UTC on Oct 31 is already Nov 1 in Asia: still October.
    expect(monthKey(new Date('2026-10-31T23:30:00Z'))).toBe('2026-10')
    expect(monthStart('2026-11')?.toISOString()).toBe('2026-11-01T00:00:00.000Z')
    expect(monthStart('2026-13')).toBeNull()
    expect(monthStart('26-11')).toBeNull()
    expect(addMonths(new Date('2026-01-01T00:00:00Z'), -1).toISOString()).toBe('2025-12-01T00:00:00.000Z')
  })

  it('tenant CSV: raw quantities, completeness per row, active users on months only', () => {
    const u: TenantUsage = {
      tenant: { id: 't1', name: 'Acme', slug: 'acme', plan_name: 'Free', purged: false },
      granularity: 'month', from: '2026-10-01T00:00:00Z', to: '2026-12-01T00:00:00Z',
      buckets: [
        { start: '2026-10-01T00:00:00Z', complete: true, collected_minutes: 44640, minutes: 44640, meters: meters({ relay_bytes_to_clients: 123456789 }), active_users: 4 },
        { start: '2026-11-01T00:00:00Z', complete: false, collected_minutes: 100, minutes: 43200, meters: meters({ session_minutes: 7 }), active_users: 2 },
      ],
    }
    const lines = tenantUsageCSV(u, HOURLY_METERS).trimEnd().split('\r\n')
    expect(lines[0]).toBe('start,complete,collected_minutes,minutes,relay_bytes_to_clients,relay_bytes_to_connectors,session_minutes,connector_minutes,shield_minutes,active_users')
    expect(lines[1]).toBe('2026-10-01T00:00:00Z,true,44640,44640,123456789,0,0,0,0,4')
    expect(lines[2]).toBe('2026-11-01T00:00:00Z,false,100,43200,0,0,7,0,0,2')
    const day = tenantUsageCSV({ ...u, granularity: 'day' }, HOURLY_METERS).split('\r\n')[0]
    expect(day.endsWith('shield_minutes')).toBe(true)
  })

  it('fleet CSV: month and completeness on every row; tenant names that look like formulas stay text', () => {
    const rows: TenantMonthUsage[] = [
      { tenant: { id: 't1', name: '=HYPERLINK("x")', slug: 'evil', plan_name: null, purged: true }, meters: meters({ shield_minutes: 60 }), active_users: 1 },
    ]
    const lines = fleetUsageCSV('2026-11', false, rows, HOURLY_METERS).trimEnd().split('\r\n')
    expect(lines[0]).toBe('month,month_complete,tenant_id,tenant_name,tenant_slug,deleted,plan,relay_bytes_to_clients,relay_bytes_to_connectors,session_minutes,connector_minutes,shield_minutes,active_users')
    expect(lines[1]).toBe(`2026-11,false,t1,"'=HYPERLINK(""x"")",evil,true,,0,0,0,0,60,1`)
  })
})
