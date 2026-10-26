import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { AuditEntry, CertificateExpiryResult, Page, RelaySummary } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { renderApp, signedInAs } from '@/test/render'

const iso = (ms: number) => new Date(Date.now() + ms).toISOString()
const MIN = 60_000
const DAY = 24 * 60 * MIN

function relay(over: Partial<RelaySummary>): RelaySummary {
  return {
    id: '11111111-1111-1111-1111-111111111111',
    name: 'relay-a',
    status: 'active',
    version: '0.21.0',
    hostname: 'relay-a.local',
    public_addr: '203.0.113.5:9093',
    address_scope: 'public',
    capacity_label: 'high',
    connection_count: 100,
    max_connections: 500,
    cert_serial: 'abc',
    cert_not_after: iso(20 * DAY),
    last_heartbeat_at: iso(-1 * MIN),
    attached_connectors: 4,
    created_at: '2026-10-01T00:00:00Z',
    ...over,
  }
}

const RELAYS = [
  relay({}),
  relay({ id: '22222222-2222-2222-2222-222222222222', name: 'relay-low', capacity_label: 'low', connection_count: 450, attached_connectors: 6 }),
  relay({ id: '33333333-3333-3333-3333-333333333333', name: 'relay-idle', status: 'inactive', connection_count: 0, attached_connectors: 0, last_heartbeat_at: iso(-6 * MIN) }),
  relay({ id: '44444444-4444-4444-4444-444444444444', name: 'relay-cert', cert_not_after: iso(3 * DAY), connection_count: 0, attached_connectors: 0 }),
]

const CERTS: CertificateExpiryResult = {
  summary: { expired: 1, lt_24h: 2, lt_7d: 3, lt_30d: 4, ok: 50 },
  items: [
    { kind: 'connector', tenant_id: '1f3c5a7e-0000-0000-0000-000000000001', entity_id: 'c-1', entity_name: 'edge-gw-03', serial: 's1', not_after: iso(-1 * DAY), bucket: 'expired' },
    { kind: 'relay', tenant_id: null, entity_id: RELAYS[3].id, entity_name: 'relay-cert', serial: 's2', not_after: iso(3 * DAY), bucket: 'lt_7d' },
  ],
  next_cursor: null,
}

const AUDIT: Page<AuditEntry> = {
  items: [
    {
      id: 'a1',
      created_at: iso(-2 * MIN),
      provider_user_id: superAdminMe.user_id,
      provider_email: 'boss@provider.test',
      action: 'provider_user.role_change',
      target_type: 'provider_user',
      target_id: 'x',
      details: {},
      ip_address: null,
    },
  ],
  next_cursor: null,
}

const routes = {
  'GET /provider/relays': { status: 200, body: { items: RELAYS, next_cursor: null } },
  'GET /provider/certificates': { status: 200, body: CERTS },
  'GET /provider/audit': { status: 200, body: AUDIT },
}

const paths = (calls: RecordedCall[]) => calls.map((c) => `${c.method} ${c.path}`)
const qs = (c: RecordedCall) => Object.fromEntries(new URLSearchParams(c.search))

describe('Overview', () => {
  it('super-admin: one request per source under StrictMode, with the documented parameters', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(routes)
    renderApp('/', undefined, { strict: true })
    await screen.findByText('relay-low is in the low capacity tier')
    await waitFor(() => expect(calls).toHaveLength(3))
    expect(paths(calls).sort()).toEqual(['GET /provider/audit', 'GET /provider/certificates', 'GET /provider/relays'])
    expect(qs(calls.find((c) => c.path === '/provider/certificates')!)).toEqual({ within: '168h', limit: '6' })
    expect(qs(calls.find((c) => c.path === '/provider/audit')!)).toEqual({ limit: '6' })
    expect(qs(calls.find((c) => c.path === '/provider/relays')!)).toEqual({ limit: '200' })
    await act(async () => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
    })
    expect(calls).toHaveLength(3)
    expect(calls.every((c) => c.method === 'GET' && !c.path.startsWith('/provider/tenants'))).toBe(true)
  })

  it('relay-ops reads relays only: no certificate or audit request', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch(routes)
    renderApp('/')
    await screen.findByText('relay-low is in the low capacity tier')
    expect(paths(calls)).toEqual(['GET /provider/relays'])
    expect(screen.getByRole('heading', { name: 'Your access' })).toBeInTheDocument()
    expect(screen.queryByText('Certificates needing action')).not.toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Recent operator activity' })).not.toBeInTheDocument()
  })

  it('reads every relay page by cursor', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({
      'GET /provider/relays': (c) =>
        qs(c).cursor
          ? { status: 200, body: { items: [RELAYS[2]], next_cursor: null } }
          : { status: 200, body: { items: [RELAYS[0]], next_cursor: 'p2' } },
    })
    renderApp('/')
    await screen.findByText('relay-idle is inactive')
    expect(calls.map(qs)).toEqual([{ limit: '200' }, { limit: '200', cursor: 'p2' }])
    expect(screen.getByText(/of 2 active/).parentElement).toHaveTextContent('1 of 2 active')
  })

  it('tiles and attention items come from the API fields', async () => {
    signedInAs(superAdminMe)
    mockFetch(routes)
    renderApp('/')
    await screen.findByText('relay-low is in the low capacity tier')
    // 3 active relays: 100 + 450 + 0 of 3 × 500; attached 4 + 6 + 0.
    expect(screen.getByText(/of 4 active/).parentElement).toHaveTextContent('3 of 4 active')
    expect(screen.getByText(/\/ 1,500/).parentElement).toHaveTextContent('550 / 1,500')
    expect(screen.getByText('10')).toBeInTheDocument()
    // Certificates tile is the API summary: expired + lt_24h + lt_7d.
    const tile = screen.getByRole('link', { name: /Certificates needing action/ })
    expect(within(tile).getByText('6')).toBeInTheDocument()
    expect(tile).toHaveAttribute('href', '/certificates')
    // Attention: inactive, low tier, relay cert < 7 days (once), and the expired connector cert.
    expect(screen.getByRole('link', { name: /relay-idle is inactive/ })).toHaveAttribute('href', `/relays/${RELAYS[2].id}`)
    expect(screen.getByRole('link', { name: /relay-cert certificate/ })).toHaveAttribute('href', `/relays/${RELAYS[3].id}`)
    expect(screen.getAllByText(/relay-cert/)).toHaveLength(1)
    expect(screen.getByRole('link', { name: /Connector edge-gw-03/ })).toHaveAttribute('href', '/certificates')
    // Recent activity is the audit page as returned.
    expect(screen.getByText('provider_user.role_change')).toBeInTheDocument()
  })

  it('Refresh sends exactly one new request per source', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch(routes)
    renderApp('/')
    await screen.findByText('relay-low is in the low capacity tier')
    await waitFor(() => expect(calls).toHaveLength(3))
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh overview' }))
    await waitFor(() => expect(calls).toHaveLength(6))
    expect(paths(calls.slice(3)).sort()).toEqual(['GET /provider/audit', 'GET /provider/certificates', 'GET /provider/relays'])
  })

  it('a failed source shows Retry without hiding the rest', async () => {
    signedInAs(superAdminMe)
    mockFetch({ ...routes, 'GET /provider/audit': { status: 500, body: { error: 'server_error' } } })
    renderApp('/')
    await screen.findByText('relay-low is in the low capacity tier')
    expect(await screen.findByText(/Couldn’t load activity/)).toBeInTheDocument()
    expect(screen.getByRole('region', { name: 'Your session' })).toBeInTheDocument()
  })
})
