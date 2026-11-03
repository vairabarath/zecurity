import { act, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { TenantDetail } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { renderApp, signedInAs } from '@/test/render'

// The audited-fetch invariant (OQ-1): every successful GET
// /provider/tenants/{id} writes one provider audit row on the server (proven
// by the controller's R6 tests). These tests pin the number of such GETs the
// console sends, so the audit trail matches what the operator actually did.

const ID = 'aaaaaaaa-1111-2222-3333-444444444444'
const ID_B = 'bbbbbbbb-1111-2222-3333-444444444444'
const NET = 'cccccccc-1111-2222-3333-444444444444'
const CONN = 'dddddddd-1111-2222-3333-444444444444'
const DAY = 24 * 60 * 60_000
const iso = (ms: number) => new Date(Date.now() + ms).toISOString()

function detail(over: Partial<TenantDetail> = {}): TenantDetail {
  return {
    id: ID,
    slug: 'acme',
    name: 'Acme',
    status: 'active',
    trust_domain: 'acme.zecurity.test',
    created_at: '2026-10-01T00:00:00Z',
    ca_not_after: iso(200 * DAY),
    counts: {
      connectors: { pending: 0, active: 1, disconnected: 0, revoked: 1 },
      shields: { pending: 0, active: 1, disconnected: 0, revoked: 0 },
      remote_networks: 1,
      users: { active: 40, suspended: 1, locked: 0, deleted: 2 },
      client_devices: { active: 30, re_enroll_required: 1, renew_pending: 0, revoked: 4 },
    },
    remote_networks: [
      { id: NET, name: 'hq', status: 'active' },
      { id: 'eeeeeeee-1111-2222-3333-444444444444', name: 'old', status: 'deleted' },
    ],
    remote_networks_truncated: false,
    connectors: [
      { id: CONN, name: 'hq-1', status: 'active', remote_network_id: NET, version: '0.21.0', cert_not_after: iso(3 * DAY), last_heartbeat_at: iso(-60_000), revoked_at: null },
      { id: 'ffffffff-1111-2222-3333-444444444444', name: 'hq-2', status: 'revoked', remote_network_id: NET, version: null, cert_not_after: null, last_heartbeat_at: null, revoked_at: '2026-10-03T10:00:00Z' },
    ],
    connectors_truncated: false,
    shields: [{ id: '12121212-1111-2222-3333-444444444444', name: 'db-1', status: 'active', connector_id: CONN, remote_network_id: NET, cert_not_after: iso(40 * DAY), last_heartbeat_at: iso(-120_000) }],
    shields_truncated: false,
    ...over,
  }
}

const detailCalls = (calls: RecordedCall[], id = ID) => calls.filter((c) => c.path === `/provider/tenants/${id}`)
const quiet = () => new Promise((r) => setTimeout(r, 50))
const ok = (body: TenantDetail = detail()): FakeResponse => ({ status: 200, body })
const listOk: FakeResponse = { status: 200, body: { items: [detail(), { ...detail(), id: ID_B, name: 'Beta', slug: 'beta' }], next_cursor: null } }

describe('Tenant detail: the audited fetch', () => {
  it('navigation under StrictMode sends exactly one GET; focus/visibility send none', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ [`GET /provider/tenants/${ID}`]: ok() })
    renderApp(`/tenants/${ID}`, undefined, { strict: true })
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    act(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
      window.dispatchEvent(new Event('online'))
    })
    await quiet()
    expect(detailCalls(calls)).toHaveLength(1)
    expect(calls.filter((c) => c.method !== 'GET')).toHaveLength(0)
  })

  it('navigating away and back sends exactly one new GET', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ [`GET /provider/tenants/${ID}`]: ok(), 'GET /provider/tenants': listOk })
    renderApp(`/tenants/${ID}`, undefined, { strict: true })
    const user = userEvent.setup()
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    await user.click(screen.getByRole('link', { name: /Back to tenants/ }))
    await screen.findByRole('row', { name: 'Acme' })
    expect(detailCalls(calls)).toHaveLength(1) // the list never fetches detail
    await user.click(screen.getByRole('link', { name: 'Acme' }))
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    await quiet()
    expect(detailCalls(calls)).toHaveLength(2)
  })

  it('Refresh (records an audit entry) sends exactly one new GET', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ [`GET /provider/tenants/${ID}`]: ok() })
    renderApp(`/tenants/${ID}`)
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    const refresh = screen.getByRole('button', { name: 'Refresh (records an audit entry)' })
    await userEvent.setup().click(refresh)
    await waitFor(() => expect(detailCalls(calls)).toHaveLength(2))
    await quiet()
    expect(detailCalls(calls)).toHaveLength(2)
  })

  it('an error never retries by itself; Retry sends exactly one new GET', async () => {
    signedInAs(superAdminMe)
    let fail = true
    const { calls } = mockFetch({
      [`GET /provider/tenants/${ID}`]: () => (fail ? { status: 500, body: { error: 'server_error' } } : ok()),
    })
    renderApp(`/tenants/${ID}`)
    expect(await screen.findByText('Couldn’t load this tenant.')).toBeInTheDocument()
    await quiet()
    expect(detailCalls(calls)).toHaveLength(1)
    fail = false
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    expect(detailCalls(calls)).toHaveLength(2)
  })

  it.each(['acme', 'not-a-uuid', '1%27%20OR%201%3D1', 'aaaaaaaa11112222333344444444444'])('malformed id %s sends no request', async (bad) => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({})
    renderApp(`/tenants/${bad}`)
    expect(screen.getByRole('heading', { name: 'Tenant not found' })).toBeInTheDocument()
    await quiet()
    expect(calls).toHaveLength(0)
  })

  it.each([
    ['404', { status: 404, body: { error: 'not_found' } }],
    ['400', { status: 400, body: { error: 'invalid_id' } }],
  ])('%s shows Tenant not found', async (_n, resp) => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ [`GET /provider/tenants/${ID}`]: resp })
    renderApp(`/tenants/${ID}`)
    expect(await screen.findByRole('heading', { name: 'Tenant not found' })).toBeInTheDocument()
    expect(detailCalls(calls)).toHaveLength(1)
  })

  it('a slow response for a tenant the operator navigated away from is ignored', async () => {
    signedInAs(superAdminMe)
    let releaseA!: () => void
    const gateA = new Promise<void>((r) => (releaseA = r))
    const { fn } = mockFetch({
      [`GET /provider/tenants/${ID}`]: ok(),
      [`GET /provider/tenants/${ID_B}`]: ok({ ...detail(), id: ID_B, name: 'Beta', slug: 'beta' }),
      'GET /provider/tenants': listOk,
    })
    const real = fn.getMockImplementation()!
    fn.mockImplementation(async (input, init) => {
      if (String(input).endsWith(ID)) await gateA
      return real(input, init)
    })
    renderApp(`/tenants/${ID}`)
    const user = userEvent.setup()
    expect(screen.getByLabelText('Loading tenant')).toBeInTheDocument()
    await user.click(within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Tenants' }))
    await user.click(await screen.findByRole('link', { name: 'Beta' }))
    await screen.findByRole('heading', { name: 'Beta', level: 1 })
    await act(async () => releaseA())
    await quiet()
    expect(screen.getByRole('heading', { name: 'Beta', level: 1 })).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Acme', level: 1 })).not.toBeInTheDocument()
  })

  it('a role 403 shows Forbidden; relay-ops is refused before any request', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/tenants/${ID}`]: { status: 403, body: { error: 'forbidden' } } })
    const view = renderApp(`/tenants/${ID}`)
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    view.unmount()

    signedInAs(relayOpsMe)
    const { calls } = mockFetch({})
    renderApp(`/tenants/${ID}`)
    expect(screen.getByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    await quiet()
    expect(calls).toHaveLength(0)
  })
})

describe('Tenant detail: content', () => {
  it('shows overview, remote networks, connectors and shields from the response', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/tenants/${ID}`]: ok() })
    renderApp(`/tenants/${ID}`)
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    expect(screen.getByText('acme.zecurity.test')).toBeInTheDocument()
    expect(screen.getByLabelText('User counts')).toHaveTextContent('40 active · 1 suspended · 0 locked · 2 deleted')

    const nets = within(screen.getByRole('table', { name: 'Remote networks' }))
    expect(nets.getByRole('row', { name: 'old' })).toHaveTextContent('deleted') // lifecycle kept, per R3 review

    const conns = screen.getByRole('table', { name: 'Connectors' })
    const hq1 = within(within(conns).getByRole('row', { name: 'hq-1' }))
    expect(hq1.getByText('hq')).toBeInTheDocument() // network name from the same response
    expect(hq1.getByText('0.21.0')).toBeInTheDocument()
    expect(hq1.getByText('< 7 days')).toHaveAttribute('data-bucket', 'lt_7d')
    const hq2 = within(within(conns).getByRole('row', { name: 'hq-2' }))
    expect(hq2.getByText('revoked')).toBeInTheDocument()
    expect(hq2.getAllByText('—').length).toBeGreaterThanOrEqual(3) // version, heartbeat, expiry nulls

    const db1 = within(within(screen.getByRole('table', { name: 'Shields' })).getByRole('row', { name: 'db-1' }))
    expect(db1.getByText('hq-1')).toBeInTheDocument()
    expect(screen.queryByText(/first 200/)).not.toBeInTheDocument()
  })

  it('no ids become links: nothing on the page can open another tenant detail', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/tenants/${ID}`]: ok() })
    renderApp(`/tenants/${ID}`)
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    const links = within(screen.getByRole('main')).getAllByRole('link').map((a) => a.getAttribute('href'))
    expect(links).toEqual(['/tenants']) // only "Back to tenants"
  })

  it('renders no identity data even if the response carried some', async () => {
    signedInAs(superAdminMe)
    const leaky = { ...detail(), admin_email: 'admin.canary@acme.example', users: [{ email: 'u.canary@acme.example', provider_sub: 'SUBCANARY' }] }
    mockFetch({ [`GET /provider/tenants/${ID}`]: { status: 200, body: leaky } })
    renderApp(`/tenants/${ID}`)
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    expect(document.body.textContent).not.toMatch(/canary|@acme\.example|SUBCANARY/i)
  })

  it('truncation notes appear only when the API says so; [] and null render as empty/—', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      [`GET /provider/tenants/${ID}`]: ok(detail({ remote_networks: [], connectors: [], shields: [], ca_not_after: null, remote_networks_truncated: true, connectors_truncated: true, shields_truncated: true })),
    })
    renderApp(`/tenants/${ID}`)
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    expect(screen.getByText('No remote networks.')).toBeInTheDocument()
    expect(screen.getByText('No connectors.')).toBeInTheDocument()
    expect(screen.getByText('No shields.')).toBeInTheDocument()
    expect(screen.getAllByText('Showing the first 200 entries.')).toHaveLength(3)
  })

  it('is read-only: the only button is Refresh', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/tenants/${ID}`]: ok() })
    renderApp(`/tenants/${ID}`)
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    const main = screen.getByRole('main')
    expect(within(main).getAllByRole('button').map((b) => b.textContent?.trim())).toEqual(['Refresh (records an audit entry)'])
    expect(within(main).queryByText(/\b(revoke|delete|edit|create|suspend)\b/i)).not.toBeInTheDocument()
  })
})
