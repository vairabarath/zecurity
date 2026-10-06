import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { RelayDetail } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { renderApp, signedInAs } from '@/test/render'

const ID = '11111111-1111-1111-1111-111111111111'
const TENANT = '99999999-9999-9999-9999-999999999999'
const DAY = 24 * 60 * 60_000
const iso = (ms: number) => new Date(Date.now() + ms).toISOString()

function detail(over: Partial<RelayDetail> = {}): RelayDetail {
  return {
    id: ID,
    name: 'relay-a',
    status: 'active',
    version: '0.21.0',
    hostname: 'relay-a.local',
    public_addr: '203.0.113.5:9093',
    address_scope: 'public',
    capacity_label: 'medium',
    connection_count: 7,
    max_connections: 100,
    cert_serial: 'cur1',
    cert_not_after: iso(20 * DAY),
    last_heartbeat_at: iso(-60_000),
    attached_connectors: 1,
    created_at: '2026-10-01T00:00:00Z',
    dns_allowlist: ['relay.example.com'],
    ip_allowlist: ['203.0.113.5'],
    certificates: [
      // Newest by issue date, but NOT current: "Current" must come from is_current only.
      { serial: 'newer-not-current', issued_at: iso(-DAY), not_after: iso(200 * DAY), revoked_at: null, revocation_reason: null, is_current: false },
      { serial: 'cur1', issued_at: iso(-10 * DAY), not_after: iso(20 * DAY), revoked_at: null, revocation_reason: null, is_current: true },
      // Expired by date but not revoked: must not be shown as revoked.
      { serial: 'old-expired', issued_at: iso(-400 * DAY), not_after: iso(-30 * DAY), revoked_at: null, revocation_reason: null, is_current: false },
      // Revoked although its date is still valid: revoked comes from revoked_at only.
      { serial: 'rev1', issued_at: iso(-20 * DAY), not_after: iso(100 * DAY), revoked_at: '2026-10-02T08:00:00Z', revocation_reason: 'superseded', is_current: false },
    ],
    certificates_truncated: false,
    attachments: [
      { connector_id: 'c-1', tenant_id: TENANT, attached_at: '2026-10-03T00:00:00Z', last_confirmed: iso(-5 * 60_000), source: 'heartbeat' },
    ],
    attachments_truncated: false,
    ...over,
  }
}

const relayCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === `/provider/relays/${ID}`)

describe('Relay detail', () => {
  it('shows overview, allowlists, certificate history and attachments', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail() } })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByRole('heading', { name: 'relay-a', level: 1 })).toBeInTheDocument()
    expect(screen.getByText('medium · 7 / 100')).toBeInTheDocument()
    expect(within(screen.getByRole('list', { name: 'DNS allowlist' })).getByText('relay.example.com')).toBeInTheDocument()
    expect(within(screen.getByRole('list', { name: 'IP allowlist' })).getByText('203.0.113.5')).toBeInTheDocument()

    const att = screen.getByRole('table', { name: 'Attached connectors' })
    expect(within(att).getByText('c-1')).toBeInTheDocument()
    expect(within(att).getByText(TENANT)).toBeInTheDocument()
    expect(within(att).queryByRole('link')).not.toBeInTheDocument() // tenant id is plain text
    expect(screen.queryByText(/first 200/)).not.toBeInTheDocument()
  })

  it('"Current" comes only from is_current and revocation only from revoked_at', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail() } })
    renderApp(`/relays/${ID}`)
    await screen.findByRole('table', { name: 'Certificate history' })
    const row = (serial: string) => within(screen.getByRole('row', { name: serial }))

    expect(screen.getAllByText('Current')).toHaveLength(1)
    expect(row('cur1').getByText('Current')).toBeInTheDocument()
    expect(screen.getByRole('row', { name: 'cur1' })).toHaveAttribute('data-current', 'true')
    expect(row('newer-not-current').queryByText('Current')).not.toBeInTheDocument()

    expect(screen.getAllByText('Revoked')).toHaveLength(1)
    expect(row('rev1').getByText('Revoked')).toBeInTheDocument()
    expect(row('rev1').getByText(/superseded/)).toBeInTheDocument()
    expect(row('old-expired').queryByText('Revoked')).not.toBeInTheDocument()

    // Expiry badges use the shared bucketFor semantics.
    expect(row('old-expired').getByText('Expired')).toHaveAttribute('data-bucket', 'expired')
    expect(row('cur1').getByText('< 30 days')).toHaveAttribute('data-bucket', 'lt_30d')
    expect(row('rev1').getByText('OK')).toHaveAttribute('data-bucket', 'ok')
  })

  it('shows the 200-entry notes only when the API says a list was cut', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail({ certificates_truncated: true, attachments_truncated: true }) } })
    renderApp(`/relays/${ID}`)
    await screen.findByRole('table', { name: 'Certificate history' })
    expect(screen.getAllByText('Showing the first 200 entries.')).toHaveLength(2)
  })

  it('empty collections show their empty text', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      [`GET /provider/relays/${ID}`]: { status: 200, body: detail({ dns_allowlist: [], ip_allowlist: [], certificates: [], attachments: [], cert_serial: null, cert_not_after: null }) },
    })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByText('No certificates issued yet.')).toBeInTheDocument()
    expect(screen.getByText('No connectors attached.')).toBeInTheDocument()
    expect(screen.getAllByText('None')).toHaveLength(2)
  })

  it.each([
    ['unknown id (404)', 404, 'not_found'],
    ['malformed id (400)', 400, 'invalid_id'],
  ])('%s shows Relay not found with a way back', async (_n, status, error) => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status, body: { error } } })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByRole('heading', { name: 'Relay not found' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /Back to relays/ })).toHaveAttribute('href', '/relays')
  })

  it('an error offers Retry, which sends exactly one new request', async () => {
    signedInAs(superAdminMe)
    let fail = true
    const { calls } = mockFetch({
      [`GET /provider/relays/${ID}`]: () => (fail ? { status: 500, body: { error: 'server_error' } } : { status: 200, body: detail() }),
    })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByText('Couldn’t load this relay.')).toBeInTheDocument()
    await new Promise((r) => setTimeout(r, 30))
    expect(relayCalls(calls)).toHaveLength(1)
    fail = false
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    expect(relayCalls(calls)).toHaveLength(2)
  })

  it('one request on open (StrictMode), none on focus; Refresh adds exactly one', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail() } })
    renderApp(`/relays/${ID}`, undefined, { strict: true })
    await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    act(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await new Promise((r) => setTimeout(r, 50))
    expect(relayCalls(calls)).toHaveLength(1)
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh relay' }))
    await waitFor(() => expect(relayCalls(calls)).toHaveLength(2))
    await new Promise((r) => setTimeout(r, 30))
    expect(relayCalls(calls)).toHaveLength(2)
  })

  it('a role 403 shows Forbidden', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relays/${ID}`]: { status: 403, body: { error: 'forbidden' } } })
    renderApp(`/relays/${ID}`)
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
  })

  it('is read-only: only Refresh, no mutating controls, GETs only', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ [`GET /provider/relays/${ID}`]: { status: 200, body: detail() } })
    renderApp(`/relays/${ID}`)
    await screen.findByRole('heading', { name: 'relay-a', level: 1 })
    const main = screen.getByRole('main')
    expect(within(main).getAllByRole('button').map((b) => b.textContent?.trim())).toEqual(['Refresh'])
    expect(within(main).queryByText(/\b(revoke|delete|edit|create|suspend)\b/i)).not.toBeInTheDocument()
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })
})
