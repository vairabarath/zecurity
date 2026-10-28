import { act, fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { CreateRelayResponse, Page, RelaySummary } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { renderApp, sessionStorageDump, signedInAs } from '@/test/render'

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
    connection_count: 12,
    max_connections: 500,
    cert_serial: 'abc',
    cert_not_after: iso(3 * DAY),
    last_heartbeat_at: iso(-2 * MIN),
    attached_connectors: 4,
    created_at: '2026-10-01T00:00:00Z',
    ...over,
  }
}

const A = relay({})
const B = relay({ id: '22222222-2222-2222-2222-222222222222', name: 'relay-b', status: 'pending', version: null, hostname: null, public_addr: null, cert_not_after: null, last_heartbeat_at: null, attached_connectors: 0 })

const relayCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === '/provider/relays')
const qs = (c: RecordedCall) => Object.fromEntries(new URLSearchParams(c.search))

describe('Relays list', () => {
  it('renders every column; missing values show —', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A, B], next_cursor: null } satisfies Page<RelaySummary> } })
    renderApp('/relays')
    const rowA = await screen.findByRole('row', { name: 'relay-a' })
    const a = within(rowA)
    expect(a.getByText('active')).toHaveAttribute('data-status', 'active')
    expect(a.getByRole('link', { name: 'relay-a' })).toHaveAttribute('href', `/relays/${A.id}`)
    expect(a.getByText('0.21.0')).toBeInTheDocument()
    expect(a.getByText('relay-a.local')).toBeInTheDocument()
    expect(a.getByText('203.0.113.5:9093')).toBeInTheDocument()
    expect(a.getByText('12 / 500')).toBeInTheDocument()
    expect(a.getByText('high')).toHaveAttribute('data-tier', 'high')
    expect(a.getByText('2 min ago')).toBeInTheDocument()
    expect(a.getByText('< 7 days')).toHaveAttribute('data-bucket', 'lt_7d') // shared bucketFor
    expect(a.getByText('4')).toBeInTheDocument()

    const b = within(screen.getByRole('row', { name: 'relay-b' }))
    expect(b.getAllByText('—').length).toBeGreaterThanOrEqual(5) // version, hostname, address, heartbeat, expiry
    expect(b.getByText('pending')).toBeInTheDocument()
  })

  it('the status filter is sent and kept in the URL', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } } })
    renderApp('/relays')
    await screen.findByRole('row', { name: 'relay-a' })
    const user = userEvent.setup()
    await user.click(within(screen.getByRole('group', { name: 'Status filter' })).getByRole('button', { name: 'Inactive' }))
    await waitFor(() => expect(relayCalls(calls)).toHaveLength(2))
    expect(qs(relayCalls(calls)[1])).toEqual({ status: 'inactive' })
  })

  it('changing the status filter clears the cursor stack and starts at page 1', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/relays': (c) => {
        const p = qs(c)
        const tag = p.status ?? 'all'
        if (p.cursor && !p.cursor.startsWith(tag)) return { status: 400, body: { error: 'invalid_cursor' } } // foreign cursor
        const page = p.cursor ? 2 : 1
        return {
          status: 200,
          body: { items: [relay({ name: `${tag}-p${page}` })], next_cursor: page === 1 ? `${tag}#2` : null },
        }
      },
    })
    renderApp('/relays')
    const user = userEvent.setup()
    await screen.findByText('all-p1')
    await user.click(screen.getByRole('button', { name: /Next/ }))
    await screen.findByText('all-p2')
    expect(screen.getByText('Page 2')).toBeInTheDocument()

    await user.click(within(screen.getByRole('group', { name: 'Status filter' })).getByRole('button', { name: 'Active' }))
    expect(await screen.findByText('active-p1')).toBeInTheDocument()
    expect(screen.getByText('Page 1')).toBeInTheDocument()
    const last = relayCalls(calls).at(-1)!
    expect(qs(last)).toEqual({ status: 'active' }) // no cursor carried over
    expect(relayCalls(calls).every((c) => !qs(c).cursor || qs(c).cursor.startsWith(qs(c).status ?? 'all'))).toBe(true)
  })

  it('empty, error with Retry, and invalid filter with Reset', async () => {
    signedInAs(superAdminMe)
    let mode: 'empty' | 'error' = 'error'
    const { calls } = mockFetch({
      'GET /provider/relays': () => (mode === 'error' ? { status: 500, body: { error: 'server_error' } } : { status: 200, body: { items: [], next_cursor: null } }),
    })
    const view = renderApp('/relays')
    expect(await screen.findByText('Couldn’t load relays.')).toBeInTheDocument()
    expect(relayCalls(calls)).toHaveLength(1) // no automatic retry
    mode = 'empty'
    await userEvent.setup().click(screen.getByRole('button', { name: 'Retry' }))
    expect(await screen.findByText('No relays yet.')).toBeInTheDocument()
    expect(relayCalls(calls)).toHaveLength(2)
    view.unmount()

    renderApp('/relays?status=bogus')
    expect(await screen.findByText(/Invalid filter/)).toBeInTheDocument()
    expect(relayCalls(calls)).toHaveLength(2) // the invalid URL made no request
    await userEvent.setup().click(screen.getByRole('button', { name: 'Reset filters' }))
    expect(await screen.findByText('No relays yet.')).toBeInTheDocument()
    expect(qs(relayCalls(calls).at(-1)!)).toEqual({})
  })

  it('a server 400 (e.g. a stale cursor) shows Invalid filter', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/relays': { status: 400, body: { error: 'invalid_cursor' } } })
    renderApp('/relays')
    expect(await screen.findByText(/Invalid filter/)).toBeInTheDocument()
  })

  it('exactly one request on open under StrictMode; no refetch on focus or visibility', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } } })
    renderApp('/relays', undefined, { strict: true })
    await screen.findByRole('row', { name: 'relay-a' })
    act(() => {
      window.dispatchEvent(new Event('focus'))
      document.dispatchEvent(new Event('visibilitychange'))
    })
    await new Promise((r) => setTimeout(r, 50))
    expect(relayCalls(calls)).toHaveLength(1)
  })

  it('Refresh sends exactly one new request', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } } })
    renderApp('/relays')
    await screen.findByRole('row', { name: 'relay-a' })
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh relays' }))
    await waitFor(() => expect(relayCalls(calls)).toHaveLength(2))
    await new Promise((r) => setTimeout(r, 30))
    expect(relayCalls(calls)).toHaveLength(2)
  })

  it('relay-ops can open Relays, and both roles have the nav entry', async () => {
    for (const me of [relayOpsMe, superAdminMe]) {
      signedInAs(me)
      mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } } })
      const view = renderApp('/relays')
      await screen.findByRole('row', { name: 'relay-a' })
      expect(within(screen.getByRole('navigation', { name: 'Sections' })).getByRole('link', { name: 'Relays' })).toHaveAttribute('href', '/relays')
      view.unmount()
    }
  })

  it('a role 403 from the server shows Forbidden', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/relays': { status: 403, body: { error: 'forbidden' } } })
    renderApp('/relays')
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
  })

  it('the only mutating control is Add relay; no revoke, delete or edit', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: 'c' } } })
    renderApp('/relays')
    await screen.findByRole('row', { name: 'relay-a' })
    const main = screen.getByRole('main')
    const chips = within(screen.getByRole('group', { name: 'Status filter' })).getAllByRole('button')
    expect(chips.map((b) => b.textContent)).toEqual(['All except deleted', 'Active', 'Inactive', 'Pending', 'Revoked', 'Deleted'])
    const buttons = within(main)
      .getAllByRole('button')
      .filter((b) => !chips.includes(b))
      .map((b) => b.textContent?.trim())
    expect(buttons.sort()).toEqual(['Add relay', 'Next', 'Previous', 'Refresh'].sort())
    expect(within(main).queryByText(/\b(revoke|delete|edit|suspend)\b/i)).not.toBeInTheDocument()
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })
})

// ── Sprint 22 Phase E: relay enrolment (D-30, D-20) ─────────────────────────

const TOKEN = 'fake-provisioning-token-for-tests-0001'
const FP = 'ab'.repeat(32)
const CREATED: CreateRelayResponse = {
  relay_id: '33333333-3333-3333-3333-333333333333',
  provisioning_token: TOKEN,
  expires_at: '2026-10-28T12:00:00Z',
  ca_fingerprint: FP,
  controller_grpc_addr: 'controller.example.com:9090',
  controller_http_addr: 'controller.example.com:8080',
}

function enrolSetup(me = superAdminMe, create: FakeResponse = { status: 201, body: CREATED }) {
  signedInAs(me)
  const { calls } = mockFetch({
    'GET /provider/relays': { status: 200, body: { items: [A, B], next_cursor: null } },
    'POST /provider/relays': create,
  })
  renderApp('/relays')
  return { user: userEvent.setup(), calls }
}

async function openAdd(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'Add relay' }))
  return screen.findByRole('dialog', { name: 'Add relay' })
}

const posts = (calls: RecordedCall[]) => calls.filter((c) => c.method === 'POST')

describe('Relays: add relay', () => {
  it.each([
    ['super-admin', superAdminMe],
    ['relay-ops', relayOpsMe],
  ])('%s can add a relay and gets the token and install commands once', async (_, me) => {
    const { user, calls } = enrolSetup(me)
    const dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText('Name'), ' relay-new ')
    await user.type(within(dialog).getByLabelText('DNS names'), 'relay-new.example.com, relay-new.internal')
    await user.type(within(dialog).getByLabelText('IP addresses'), '203.0.113.9')
    await user.click(within(dialog).getByRole('button', { name: 'Create relay' }))

    const tok = await screen.findByRole('dialog', { name: 'Install relay-new' })
    expect(posts(calls)).toEqual([
      expect.objectContaining({
        path: '/provider/relays',
        body: { name: 'relay-new', dns_allowlist: ['relay-new.example.com', 'relay-new.internal'], ip_allowlist: ['203.0.113.9'] },
      }),
    ])
    expect(within(tok).getByLabelText('Provisioning token')).toHaveTextContent(TOKEN)
    const installer = within(tok).getByLabelText('Install command')
    expect(installer).toHaveTextContent(`RELAY_ID='${CREATED.relay_id}'`)
    expect(installer).toHaveTextContent(`RELAY_CA_FINGERPRINT='${FP}'`)
    expect(installer).toHaveTextContent(`RELAY_PROVISIONING_TOKEN='${TOKEN}'`)
    expect(installer).toHaveTextContent(`CONTROLLER_ADDR='controller.example.com:9090'`)
    expect(installer).toHaveTextContent(`RELAY_DNS_SANS='relay-new.example.com,relay-new.internal'`)
    expect(installer).toHaveTextContent(`RELAY_IP_SANS='203.0.113.9'`)
    expect(within(tok).getByLabelText('Direct command')).toHaveTextContent(`RELAY_PROVISIONING_TOKEN='${TOKEN}'`)
    expect(within(tok).getByText(/single use/i)).toBeInTheDocument()
    expect(within(tok).getByText(/24 hours/)).toBeInTheDocument()
    expect(within(tok).queryByText(/placeholder/i)).not.toBeInTheDocument()

    await user.click(within(tok).getByRole('button', { name: 'I’ve saved it, close' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain(TOKEN)
    expect(sessionStorageDump()).not.toContain(TOKEN)
    expect(localStorage.length).toBe(0)
    expect(window.location.href).not.toContain(TOKEN)
    await waitFor(() => expect(relayCalls(calls).filter((c) => c.method === 'GET')).toHaveLength(2)) // reload on close
  })

  it('unconfigured controller addresses show placeholders and say so', async () => {
    const { user } = enrolSetup(superAdminMe, {
      status: 201,
      body: { ...CREATED, controller_grpc_addr: '', controller_http_addr: '' },
    })
    const dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText('Name'), 'relay-new')
    await user.click(within(dialog).getByRole('button', { name: 'Create relay' }))
    const tok = await screen.findByRole('dialog', { name: 'Install relay-new' })
    expect(within(tok).getByLabelText('Install command')).toHaveTextContent(`CONTROLLER_ADDR='<controller-host>:9090'`)
    expect(within(tok).getByText(/placeholder/i)).toBeInTheDocument()
  })

  it.each([
    ['DNS names', 'Relay.Example.com', /lowercase/i],
    ['DNS names', '*.example.com', /wildcard/i],
    ['IP addresses', '10.0.0.01', /canonical/i],
  ])('an invalid %s entry (%s) is caught before any request', async (field, value, message) => {
    const { user, calls } = enrolSetup()
    const dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText('Name'), 'relay-new')
    await user.type(within(dialog).getByLabelText(field), value)
    expect(within(dialog).getByText(message)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Create relay' })).toBeDisabled()
    expect(posts(calls)).toHaveLength(0)
  })

  it('duplicate entries are caught before any request', async () => {
    const { user, calls } = enrolSetup()
    const dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText('Name'), 'relay-new')
    await user.type(within(dialog).getByLabelText('DNS names'), 'a.example.com a.example.com')
    expect(within(dialog).getByText(/listed twice/i)).toBeInTheDocument()
    expect(within(dialog).getByRole('button', { name: 'Create relay' })).toBeDisabled()
    expect(posts(calls)).toHaveLength(0)
  })

  it('a blank name keeps Create disabled', async () => {
    const { user } = enrolSetup()
    const dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText('Name'), '   ')
    expect(within(dialog).getByRole('button', { name: 'Create relay' })).toBeDisabled()
  })

  it.each([
    ['invalid_dns_san', 'invalid DNS SAN "x"', /DNS name was rejected: invalid DNS SAN "x"/],
    ['invalid_name', undefined, /1 to 128 characters/],
    ['forbidden', undefined, /role can’t do this/],
  ])('server %s is shown and the dialog stays open, without reloading', async (code, detail, message) => {
    const { user, calls } = enrolSetup(superAdminMe, { status: code === 'forbidden' ? 403 : 400, body: { error: code, detail } })
    const dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText('Name'), 'relay-new')
    await user.click(within(dialog).getByRole('button', { name: 'Create relay' }))
    expect(await within(dialog).findByText(message)).toBeInTheDocument()
    expect(relayCalls(calls).filter((c) => c.method === 'GET')).toHaveLength(1)
  })

  it('copies only on click; clicking outside keeps the token, Escape discards it', async () => {
    const { user } = enrolSetup()
    const writeText = vi.spyOn(navigator.clipboard, 'writeText')
    const dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText('Name'), 'relay-new')
    await user.click(within(dialog).getByRole('button', { name: 'Create relay' }))
    const tok = await screen.findByRole('dialog', { name: 'Install relay-new' })
    expect(writeText).not.toHaveBeenCalled()
    await user.click(within(tok).getByRole('button', { name: 'Copy install command' }))
    expect(writeText).toHaveBeenCalledWith(expect.stringContaining(`RELAY_PROVISIONING_TOKEN='${TOKEN}'`))
    await user.click(within(tok).getByRole('button', { name: 'Copy token' }))
    expect(writeText).toHaveBeenLastCalledWith(TOKEN)

    fireEvent.pointerDown(document.body)
    expect(screen.getByRole('dialog', { name: 'Install relay-new' })).toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain(TOKEN)
  })

  it('after a successful create, Add relay opens with an empty form', async () => {
    const { user } = enrolSetup()
    let dialog = await openAdd(user)
    await user.type(within(dialog).getByLabelText('Name'), 'relay-new')
    await user.type(within(dialog).getByLabelText('DNS names'), 'relay-new.example.com')
    await user.click(within(dialog).getByRole('button', { name: 'Create relay' }))
    const tok = await screen.findByRole('dialog', { name: 'Install relay-new' })
    await user.click(within(tok).getByRole('button', { name: 'I’ve saved it, close' }))
    dialog = await openAdd(user)
    expect(within(dialog).getByLabelText('Name')).toHaveValue('')
    expect(within(dialog).getByLabelText('DNS names')).toHaveValue('')
    expect(within(dialog).getByLabelText('IP addresses')).toHaveValue('')
  })

  it('a pending relay says it is waiting for enrolment', async () => {
    enrolSetup()
    const row = await screen.findByRole('row', { name: 'relay-b' })
    expect(within(row).getByText('Waiting for enrolment')).toBeInTheDocument()
    expect(within(screen.getByRole('row', { name: 'relay-a' })).queryByText('Waiting for enrolment')).not.toBeInTheDocument()
  })
})
