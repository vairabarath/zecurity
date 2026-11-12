import { screen, within } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { Page, RelayBandwidth, RelayDetail, RelaySummary } from '@/api/types'
import { mockFetch, superAdminMe } from '@/test/fetch'
import { DEFAULT_POOL, RELAY_POLICY } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'
import { formatRate, windowText } from '@/lib/format'

// F5: relay bandwidth on the relay list and the relay detail page.

const ID = '11111111-1111-1111-1111-111111111111'
const AT = '2026-11-11T10:05:00Z'

function summary(over: Partial<RelaySummary>): RelaySummary {
  return {
    id: ID, name: 'relay-a', status: 'active', version: '0.22.0', hostname: 'relay-a.local',
    public_addr: '203.0.113.5:9093', address_scope: 'public', capacity_label: 'high',
    connection_count: 12, max_connections: 500, cert_serial: 'abc', cert_not_after: null,
    last_heartbeat_at: null, attached_connectors: 1, created_at: '2026-10-01T00:00:00Z',
    draining_since: null, drain_state: null, bandwidth: null, pool: DEFAULT_POOL, policy: RELAY_POLICY, ...over,
  }
}

const REPORTED: RelayBandwidth = { measured_at: AT, window_seconds: 300, to_client_bytes_per_sec: 1_250_000, to_connector_bytes_per_sec: 42_000 }
const MEASURING: RelayBandwidth = { measured_at: AT, window_seconds: null, to_client_bytes_per_sec: null, to_connector_bytes_per_sec: null }

describe('Relay bandwidth: list', () => {
  it('shows each direction, or says Measuring / Not reported; never zero for a missing value', async () => {
    signedInAs(superAdminMe)
    const items = [
      summary({ bandwidth: REPORTED }),
      summary({ id: '22222222-2222-2222-2222-222222222222', name: 'relay-b', bandwidth: MEASURING }),
      summary({ id: '33333333-3333-3333-3333-333333333333', name: 'relay-c', bandwidth: null }),
    ]
    mockFetch({ 'GET /provider/relays': { status: 200, body: { items, next_cursor: null } satisfies Page<RelaySummary> } })
    renderApp('/relays')
    const a = within(await screen.findByRole('row', { name: 'relay-a' }))
    const cell = a.getByLabelText('Bandwidth')
    expect(cell).toHaveTextContent('to clients 1.3 MB/s')
    expect(cell).toHaveTextContent('to connectors 42.0 kB/s')
    expect(cell.getAttribute('title')).toMatch(/^Average over 5 minutes, as of /)
    expect(within(screen.getByRole('row', { name: 'relay-b' })).getByText('Measuring…')).toBeInTheDocument()
    expect(within(screen.getByRole('row', { name: 'relay-c' })).getByText('Not reported')).toBeInTheDocument()
    expect(screen.queryByText(/0 B\/s/)).not.toBeInTheDocument()
  })
})

describe('Relay bandwidth: detail', () => {
  const detail = (bandwidth: RelayBandwidth | null): RelayDetail => ({
    ...summary({ bandwidth }), dns_allowlist: [], ip_allowlist: [], certificates: [], certificates_truncated: false,
    attachments: [], attachments_truncated: false, in_relay_list: true, eligible_relay_count: 2,
  })

  it('shows both directions with the window', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail(REPORTED) } })
    renderApp(`/relays/${ID}`)
    const card = await screen.findByRole('region', { name: 'Bandwidth' })
    expect(within(card).getByText('To clients').nextSibling).toHaveTextContent('1.3 MB/s')
    expect(within(card).getByText('To connectors').nextSibling).toHaveTextContent('42.0 kB/s')
    expect(card).toHaveTextContent(/Average over 5 minutes, as of .+\. Bridged client traffic only\./)
  })

  it('Measuring and Not reported are worded, not shown as rates', async () => {
    for (const [bw, text] of [[MEASURING, /^Measuring\./], [null, /^Not reported\./]] as const) {
      signedInAs(superAdminMe)
      mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail(bw) } })
      const { unmount } = renderApp(`/relays/${ID}`)
      const card = await screen.findByRole('region', { name: 'Bandwidth' })
      expect(within(card).getByText(text)).toBeInTheDocument()
      expect(within(card).queryByText(/B\/s/)).not.toBeInTheDocument()
      unmount()
    }
  })
})

describe('formatRate / windowText', () => {
  it('uses decimal units and sensible precision', () => {
    expect(formatRate(0)).toBe('0 B/s')
    expect(formatRate(999)).toBe('999 B/s')
    expect(formatRate(1000)).toBe('1.0 kB/s')
    expect(formatRate(1_250_000)).toBe('1.3 MB/s')
    expect(formatRate(250_000_000)).toBe('250 MB/s')
    expect(formatRate(3.2e9)).toBe('3.2 GB/s')
    expect(formatRate(-5)).toBe('0 B/s')
  })
  it('words the averaging window', () => {
    expect(windowText(300)).toBe('5 minutes')
    expect(windowText(299.6)).toBe('5 minutes')
    expect(windowText(30)).toBe('30 seconds')
    expect(windowText(60)).toBe('60 seconds')
    expect(windowText(1)).toBe('1 second')
  })
})
