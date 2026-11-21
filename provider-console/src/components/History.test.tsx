import { render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useState } from 'react'
import { describe, expect, it } from 'vitest'
import { RelayHistory } from '@/components/RelayHistory'
import { TenantHistory } from '@/components/TenantHistory'
import { DEFAULT_RANGE, type MetricRange } from '@/lib/metrics'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { signedInAs } from '@/test/render'

// Sprint 24 M2 (F23): the History sections read their series once, again on
// a range change, with the subject and the range's step; tenant history
// never re-reads the (audited) tenant record.

const RELAY = '22222222-2222-2222-2222-222222222222'
const TENANT = '33333333-3333-3333-3333-333333333333'

function body(series: string, step = '1m', points: { ts: string; value: number }[] = []) {
  return { status: 200, body: { series, subject: 'x', unit: series.includes('bytes') ? 'bytes_per_sec' : 'count', step, from: '2026-11-21T10:00:00Z', to: '2026-11-21T11:00:00Z', points } }
}
const metricRoutes = (names: string[]) =>
  Object.fromEntries(names.map((n) => [`GET /provider/metrics/${n}`, (c: RecordedCall) => body(n, new URLSearchParams(c.search).get('step') ?? '1m', [{ ts: '2026-11-21T10:05:00Z', value: 3 }])]))

const RELAY_SERIES = ['relay.connections', 'relay.bytes_to_clients_per_sec', 'relay.bytes_to_connectors_per_sec']
const TENANT_SERIES = ['tenant.active_sessions', 'tenant.connectors_online', 'tenant.shields_online', 'tenant.relay_bytes_to_clients_per_sec', 'tenant.relay_bytes_to_connectors_per_sec']

function Relay({ cap }: { cap: number | null }) {
  const [range, setRange] = useState<MetricRange>(DEFAULT_RANGE)
  return <RelayHistory relay={{ id: RELAY, link_capacity_bps: cap }} range={range} onRange={setRange} />
}
function Tenant() {
  const [range, setRange] = useState<MetricRange>(DEFAULT_RANGE)
  return <TenantHistory tenantId={TENANT} range={range} onRange={setRange} />
}

const metricCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path.startsWith('/provider/metrics/'))

describe('Relay history', () => {
  it('reads the three relay series for this relay over 24 h, then hourly for 7 d (relay-ops may)', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch(metricRoutes(RELAY_SERIES))
    render(<Relay cap={null} />)
    await waitFor(() => expect(metricCalls(calls)).toHaveLength(3))
    expect(metricCalls(calls).map((c) => c.path.replace('/provider/metrics/', '')).sort()).toEqual([...RELAY_SERIES].sort())
    for (const c of metricCalls(calls)) {
      const q = new URLSearchParams(c.search)
      expect(q.get('subject')).toBe(RELAY)
      expect(q.get('step')).toBe('1m')
    }
    await userEvent.setup().click(await screen.findByRole('button', { name: '7 d' }))
    await waitFor(() => expect(metricCalls(calls)).toHaveLength(6))
    expect(metricCalls(calls).slice(3).every((c) => new URLSearchParams(c.search).get('step') === '1h')).toBe(true)
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
    expect(screen.getByRole('button', { name: '7 d' })).toHaveAttribute('aria-pressed', 'true')
  })

  it('with a link capacity, adds "Both directions" against the ceiling (no extra request)', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(metricRoutes(RELAY_SERIES))
    const { container } = render(<Relay cap={125_000_000} />)
    expect(await screen.findByText('Both directions')).toBeInTheDocument()
    // The sample traffic (3 B/s) is far below 1 Gbps: named, off scale.
    expect(container.querySelector('[data-ceiling-label]')).toHaveTextContent('Link capacity 1 Gbps')
    expect(metricCalls(calls)).toHaveLength(3)
  })

  it('every chart on the page reads the same window', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(metricRoutes(RELAY_SERIES))
    render(<Relay cap={null} />)
    await waitFor(() => expect(metricCalls(calls)).toHaveLength(3))
    const windows = new Set(metricCalls(calls).map((c) => { const q = new URLSearchParams(c.search); return `${q.get('from')}|${q.get('to')}` }))
    expect(windows.size).toBe(1)
  })

  it('on hourly ranges, no "Both directions" line or ceiling: hourly means would hide a full link', async () => {
    signedInAs(superAdminMe)
    mockFetch(metricRoutes(RELAY_SERIES))
    const { container } = render(<Relay cap={125_000_000} />)
    await screen.findByText('Both directions')
    await userEvent.setup().click(screen.getByRole('button', { name: '7 d' }))
    expect(await screen.findByText(/compared on the 1 h and 24 h ranges/)).toBeInTheDocument()
    expect(screen.queryByText('Both directions')).not.toBeInTheDocument()
    expect(container.querySelector('[data-ceiling]')).toBeNull()
    expect(container.querySelector('[data-ceiling-label]')).toBeNull()
  })

  it('without a capacity, no ceiling and no sum line', async () => {
    signedInAs(superAdminMe)
    mockFetch(metricRoutes(RELAY_SERIES))
    const { container } = render(<Relay cap={null} />)
    await screen.findByText('To connectors')
    expect(screen.queryByText('Both directions')).not.toBeInTheDocument()
    expect(container.querySelector('[data-ceiling]')).toBeNull()
  })
})

describe('Tenant history', () => {
  it('reads the five tenant series; changing the range never reads the tenant record (no audit entry)', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(metricRoutes(TENANT_SERIES))
    render(<Tenant />)
    await waitFor(() => expect(metricCalls(calls)).toHaveLength(5))
    await userEvent.setup().click(await screen.findByRole('button', { name: '30 d' }))
    await waitFor(() => expect(metricCalls(calls)).toHaveLength(10))
    expect(calls.some((c) => c.path.startsWith('/provider/tenants'))).toBe(false)
    expect(metricCalls(calls).every((c) => new URLSearchParams(c.search).get('subject') === TENANT)).toBe(true)
  })

  it('no relay traffic in the range reads "Not reported", not zero', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      ...metricRoutes(TENANT_SERIES.slice(0, 3)),
      'GET /provider/metrics/tenant.relay_bytes_to_clients_per_sec': body('tenant.relay_bytes_to_clients_per_sec'),
      'GET /provider/metrics/tenant.relay_bytes_to_connectors_per_sec': body('tenant.relay_bytes_to_connectors_per_sec'),
    })
    render(<Tenant />)
    expect(await screen.findByText(/Not reported: no relay reported this tenant’s traffic/)).toBeInTheDocument()
  })
})
