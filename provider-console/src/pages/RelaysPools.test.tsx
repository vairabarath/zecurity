import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { RelayDetail, RelaySummary } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { DEFAULT_POOL, DEFAULT_POOL_ID, EMPTY_POOL_ID, EU_POOL, EU_POOL_ID, POOLS, RELAY_POLICY } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

// Relay pools on the Relays list, Add relay and relay detail (D-37, D-41).

const A_ID = '11111111-1111-1111-1111-111111111111'

function relay(over: Partial<RelaySummary> = {}): RelaySummary {
  return {
    id: A_ID,
    name: 'relay-a',
    status: 'active',
    version: '0.22.0',
    hostname: 'relay-a.local',
    public_addr: '203.0.113.5:9093',
    address_scope: 'public',
    capacity_label: 'high',
    connection_count: 1,
    max_connections: 100,
    cert_serial: 'abc',
    cert_not_after: null,
    last_heartbeat_at: null,
    attached_connectors: 0,
    created_at: '2026-10-01T00:00:00Z',
    draining_since: null,
    drain_state: null,
    bandwidth: null,
    pool: DEFAULT_POOL, policy: RELAY_POLICY,
    ...over,
  }
}

const A = relay()
const B = relay({ id: '22222222-2222-2222-2222-222222222222', name: 'relay-eu', pool: EU_POOL })
const poolsOk: FakeResponse = { status: 200, body: { items: POOLS } }
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

describe('Relays list: pools', () => {
  it('shows each relay’s pool (linked) and region', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A, B], next_cursor: null } } })
    renderApp('/relays')
    const a = within(await screen.findByRole('row', { name: 'relay-a' }))
    expect(a.getByRole('link', { name: 'default' })).toHaveAttribute('href', `/relay-pools/${DEFAULT_POOL_ID}`)
    const b = within(screen.getByRole('row', { name: 'relay-eu' }))
    expect(b.getByRole('link', { name: 'eu-1' })).toHaveAttribute('href', `/relay-pools/${EU_POOL_ID}`)
    expect(b.getByText('eu-central-1')).toBeInTheDocument()
    const headers = within(screen.getByRole('table', { name: 'Relays' })).getAllByRole('columnheader').map((h) => h.textContent)
    expect(headers.slice(0, 4)).toEqual(['Relay', 'Status', 'Pool', 'Region'])
  })

  it('the pool filter narrows the loaded page without a request', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A, B], next_cursor: null } } })
    renderApp('/relays')
    await screen.findByRole('row', { name: 'relay-a' })
    const user = userEvent.setup()
    const chips = within(screen.getByRole('group', { name: 'Pool filter (this page)' }))
    expect(chips.getAllByRole('button').map((b) => b.textContent)).toEqual(['All pools on this page', 'default', 'eu-1'])
    await user.click(chips.getByRole('button', { name: 'eu-1' }))
    expect(screen.queryByRole('row', { name: 'relay-a' })).not.toBeInTheDocument()
    expect(screen.getByRole('row', { name: 'relay-eu' })).toBeInTheDocument()
    await user.click(chips.getByRole('button', { name: 'All pools on this page' }))
    expect(screen.getByRole('row', { name: 'relay-a' })).toBeInTheDocument()
    expect(calls).toHaveLength(1)
    expect(calls[0].search).toBe('')
  })

  it('no pool filter when the page holds a single pool', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } } })
    renderApp('/relays')
    await screen.findByRole('row', { name: 'relay-a' })
    expect(screen.queryByRole('group', { name: 'Pool filter (this page)' })).not.toBeInTheDocument()
  })
})

describe('Add relay: pool picker', () => {
  function setup(me = superAdminMe, pools: FakeResponse = poolsOk) {
    signedInAs(me)
    const { calls } = mockFetch({
      'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } },
      'GET /provider/relay-pools': pools,
      'POST /provider/relays': { status: 201, body: { relay_id: 'r', provisioning_token: 't', expires_at: '2026-11-13T00:00:00Z', ca_fingerprint: 'ab', controller_grpc_addr: '', controller_http_addr: '' } },
    })
    renderApp('/relays')
    return { user: userEvent.setup(), calls }
  }

  it('preselects the default pool and sends its id', async () => {
    const { user, calls } = setup(relayOpsMe)
    await user.click(await screen.findByRole('button', { name: 'Add relay' }))
    const dialog = await screen.findByRole('dialog', { name: 'Add relay' })
    expect(await within(dialog).findByRole('combobox', { name: 'Relay pool' })).toHaveTextContent(/^default/)
    await user.type(within(dialog).getByLabelText('Name'), 'relay-new')
    await user.click(within(dialog).getByRole('button', { name: 'Create relay' }))
    await screen.findByRole('dialog', { name: 'Install relay-new' })
    expect(writes(calls)[0].body).toEqual({ name: 'relay-new', dns_allowlist: [], ip_allowlist: [], pool_id: DEFAULT_POOL_ID })
  })

  it('sends the chosen pool', async () => {
    const { user, calls } = setup()
    await user.click(await screen.findByRole('button', { name: 'Add relay' }))
    const dialog = await screen.findByRole('dialog', { name: 'Add relay' })
    await user.click(await within(dialog).findByRole('combobox', { name: 'Relay pool' }))
    await user.click(await screen.findByRole('option', { name: /^eu-1/ }))
    await user.type(within(dialog).getByLabelText('Name'), 'relay-new')
    await user.click(within(dialog).getByRole('button', { name: 'Create relay' }))
    await screen.findByRole('dialog', { name: 'Install relay-new' })
    expect(writes(calls)[0].body).toMatchObject({ pool_id: EU_POOL_ID })
  })

  it('if the pools can’t load, says the relay goes to the default pool and omits pool_id', async () => {
    const { user, calls } = setup(superAdminMe, { status: 500, body: { error: 'server_error' } })
    await user.click(await screen.findByRole('button', { name: 'Add relay' }))
    const dialog = await screen.findByRole('dialog', { name: 'Add relay' })
    expect(await within(dialog).findByRole('note')).toHaveTextContent(/goes to the default pool/)
    await user.type(within(dialog).getByLabelText('Name'), 'relay-new')
    await user.click(within(dialog).getByRole('button', { name: 'Create relay' }))
    await screen.findByRole('dialog', { name: 'Install relay-new' })
    expect(writes(calls)[0].body).toEqual({ name: 'relay-new', dns_allowlist: [], ip_allowlist: [] })
  })

  it('pool_not_found is explained', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      'GET /provider/relays': { status: 200, body: { items: [A], next_cursor: null } },
      'GET /provider/relay-pools': poolsOk,
      'POST /provider/relays': { status: 400, body: { error: 'pool_not_found' } },
    })
    renderApp('/relays')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Add relay' }))
    const dialog = await screen.findByRole('dialog', { name: 'Add relay' })
    await within(dialog).findByRole('combobox', { name: 'Relay pool' })
    await user.type(within(dialog).getByLabelText('Name'), 'relay-new')
    await user.click(within(dialog).getByRole('button', { name: 'Create relay' }))
    expect(await within(dialog).findByText(/That relay pool no longer exists/)).toBeInTheDocument()
  })
})

// ── Relay detail: Move to pool ──────────────────────────────────────────────

function detail(over: Partial<RelayDetail> = {}): RelayDetail {
  return {
    ...relay(),
    dns_allowlist: [],
    ip_allowlist: [],
    certificates: [],
    certificates_truncated: false,
    attachments: [],
    attachments_truncated: false,
    in_relay_list: true,
    eligible_relay_count: 2,
    ...over,
  }
}

function moveSetup(me = superAdminMe, move: FakeResponse = { status: 204 }, body: RelayDetail = detail()) {
  signedInAs(me)
  const { calls } = mockFetch({
    [`GET /provider/relays/${A_ID}`]: { status: 200, body },
    'GET /provider/relay-pools': poolsOk,
    [`POST /provider/relays/${A_ID}/pool`]: move,
  })
  renderApp(`/relays/${A_ID}`)
  return { user: userEvent.setup(), calls }
}

async function openMove(user: ReturnType<typeof userEvent.setup>) {
  await user.click(await screen.findByRole('button', { name: 'Move to pool' }))
  return screen.findByRole('dialog', { name: 'Move to pool' })
}

describe('Relay detail: pool and Move to pool', () => {
  it('shows the pool, linked, with per-pool eligible count', async () => {
    moveSetup()
    const card = within(await screen.findByRole('region', { name: 'Relay pool' }))
    expect(card.getByRole('link', { name: 'default' })).toHaveAttribute('href', `/relay-pools/${DEFAULT_POOL_ID}`)
    expect(card.getByText('Eligible relays in this pool').nextElementSibling).toHaveTextContent('2')
  })

  it.each([
    ['super-admin', superAdminMe],
    ['relay-ops', relayOpsMe],
  ])('%s moves a relay: the current pool isn’t offered, a reason is required, then one POST and a reload', async (_, me) => {
    const { user, calls } = moveSetup(me)
    const dialog = await openMove(user)
    const submit = within(dialog).getByRole('button', { name: 'Move relay' })
    await user.click(await within(dialog).findByRole('combobox', { name: 'New pool' }))
    const options = (await screen.findAllByRole('option')).map((o) => o.textContent)
    expect(options.some((o) => o?.startsWith('default'))).toBe(false)
    expect(options).toHaveLength(2)
    await user.click(screen.getByRole('option', { name: /^spare/ }))
    expect(submit).toBeDisabled() // no reason yet
    await user.type(within(dialog).getByLabelText('Reason'), '  rebalance  ')
    await user.click(submit)
    expect(await screen.findByRole('status')).toHaveTextContent('relay-a moved to spare.')
    expect(writes(calls)).toEqual([expect.objectContaining({ path: `/provider/relays/${A_ID}/pool`, body: { pool_id: EMPTY_POOL_ID, reason: 'rebalance' }, search: '' })])
    expect(calls.filter((c) => c.path === `/provider/relays/${A_ID}`)).toHaveLength(2)
  })

  it.each([
    ['last_eligible_relay', 409, /last eligible relay of its pool, and tenants use that pool/],
    ['pool_not_found', 400, /That pool no longer exists/],
    ['forbidden', 403, /Your role can’t move relays between pools/],
  ])('%s stays in the dialog with an explanation', async (code, status, message) => {
    const { user } = moveSetup(relayOpsMe, { status, body: { error: code } })
    const dialog = await openMove(user)
    await user.click(await within(dialog).findByRole('combobox', { name: 'New pool' }))
    await user.click(await screen.findByRole('option', { name: /^eu-1/ }))
    await user.type(within(dialog).getByLabelText('Reason'), 'r')
    await user.click(within(dialog).getByRole('button', { name: 'Move relay' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(message)
  })

  it('reason_required is shown on the reason field', async () => {
    const { user } = moveSetup(superAdminMe, { status: 400, body: { error: 'reason_required' } })
    const dialog = await openMove(user)
    await user.click(await within(dialog).findByRole('combobox', { name: 'New pool' }))
    await user.click(await screen.findByRole('option', { name: /^eu-1/ }))
    await user.type(within(dialog).getByLabelText('Reason'), 'r')
    await user.click(within(dialog).getByRole('button', { name: 'Move relay' }))
    await waitFor(() => expect(within(dialog).getByLabelText('Reason')).toHaveAttribute('aria-invalid', 'true'))
    expect(within(dialog).getByText(/Enter a reason/)).toBeInTheDocument()
  })

  it('already_revoked closes the dialog and reloads with a notice', async () => {
    const { user, calls } = moveSetup(superAdminMe, { status: 409, body: { error: 'already_revoked' } })
    const dialog = await openMove(user)
    await user.click(await within(dialog).findByRole('combobox', { name: 'New pool' }))
    await user.click(await screen.findByRole('option', { name: /^eu-1/ }))
    await user.type(within(dialog).getByLabelText('Reason'), 'r')
    await user.click(within(dialog).getByRole('button', { name: 'Move relay' }))
    expect(await screen.findByRole('status')).toHaveTextContent(/has been revoked/)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(calls.filter((c) => c.path === `/provider/relays/${A_ID}`)).toHaveLength(2)
  })

  it('warns in pool terms when this is the pool’s only eligible relay', async () => {
    const { user } = moveSetup(superAdminMe, { status: 204 }, detail({ eligible_relay_count: 1 }))
    const dialog = await openMove(user)
    expect(within(dialog).getByRole('note')).toHaveTextContent(/only eligible relay in default/)
  })

  it.each(['revoked', 'deleted'] as const)('no Move to pool on a %s relay', async (status) => {
    moveSetup(superAdminMe, { status: 204 }, detail({ status, in_relay_list: false }))
    await screen.findByRole('region', { name: 'Relay pool' })
    expect(screen.queryByRole('button', { name: 'Move to pool' })).not.toBeInTheDocument()
  })
})
