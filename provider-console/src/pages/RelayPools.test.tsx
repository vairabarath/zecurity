import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { RelayPoolDetail } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { DEFAULT_POOL_ID, EMPTY_POOL_ID, EU_POOL, EU_POOL_ID, POOLS, poolSummary, RELAY_POLICY } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

// Relay pools (D-37 … D-41): both roles read; relay-ops never see tenant
// names nor region/residency controls; residency reads "enforced" only when
// the pool read says residency_enforced (D-40).

const listOk: FakeResponse = { status: 200, body: { items: POOLS } }
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

function poolDetail(over: Partial<RelayPoolDetail> = {}): RelayPoolDetail {
  return {
    ...POOLS[1],
    relay_list: [
      { id: '11111111-1111-1111-1111-111111111111', name: 'relay-eu-a', status: 'active', eligible: true, policy: RELAY_POLICY },
      { id: '22222222-2222-2222-2222-222222222222', name: 'relay-eu-b', status: 'inactive', eligible: false, policy: RELAY_POLICY },
    ],
    relay_list_truncated: false,
    tenant_list: [{ id: 'aaaaaaaa-1111-2222-3333-444444444444', slug: 'acme', name: 'Acme' }],
    tenant_list_truncated: false,
    ...over,
  }
}

describe('Relay pools: list', () => {
  it('shows each pool with region, residency (as enforced by the API), counts and the default marked', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/relay-pools': listOk })
    renderApp('/relay-pools')
    const def = within(await screen.findByRole('row', { name: 'default' }))
    expect(def.getByText('Default')).toBeInTheDocument()
    expect(def.getByRole('link', { name: 'default' })).toHaveAttribute('href', `/relay-pools/${DEFAULT_POOL_ID}`)
    expect(def.getAllByText('—')).toHaveLength(2) // no region, no residency

    const eu = within(screen.getByRole('row', { name: 'eu-1' }))
    expect(eu.getByText('eu-central-1')).toBeInTheDocument()
    const badge = eu.getByText(/Residency \(enforced\)/)
    expect(badge).toHaveAttribute('data-residency', 'enforced')
    expect(badge).toHaveAttribute('title', expect.stringMatching(/accept only this pool’s tenants/))
    expect(badge).toHaveTextContent(/lease \(10 minutes by default\)/) // screen-reader text
    expect(eu.queryByText('Default')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toMatch(/residency enforced|enforces residency/i)
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })

  it('warns when a pool has tenants but no eligible relay', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/relay-pools': { status: 200, body: { items: [POOLS[0], { ...POOLS[1], eligible_relays: 0 }, POOLS[2]] } } })
    renderApp('/relay-pools')
    const alert = await screen.findByRole('alert')
    expect(alert).toHaveTextContent(/eu-1 is used by tenants but has no eligible relay: tenants without a working failover pool have no relay path/)
    expect(within(screen.getByRole('row', { name: 'eu-1' })).getByText('No eligible relay')).toBeInTheDocument()
    // An empty pool without tenants is not a problem.
    expect(within(screen.getByRole('row', { name: 'spare' })).queryByText('No eligible relay')).not.toBeInTheDocument()
  })

  it('both roles have the nav entry next to Relays', async () => {
    for (const me of [superAdminMe, relayOpsMe]) {
      signedInAs(me)
      mockFetch({ 'GET /provider/relay-pools': listOk })
      const { unmount } = renderApp('/relay-pools')
      await screen.findByRole('row', { name: 'default' })
      const nav = within(screen.getByRole('navigation', { name: 'Sections' }))
      const labels = nav.getAllByRole('link').map((a) => a.textContent?.trim())
      expect(labels.indexOf('Relay pools')).toBe(labels.indexOf('Relays') + 1)
      unmount()
    }
  })

  it('relay-ops create a pool with a name only: no region or residency fields', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({
      'GET /provider/relay-pools': listOk,
      'POST /provider/relay-pools': { status: 201, body: { id: EMPTY_POOL_ID, name: 'spare-2', region: null, residency: false } },
    })
    renderApp('/relay-pools')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Create pool' }))
    const dialog = await screen.findByRole('dialog', { name: 'Create pool' })
    expect(within(dialog).queryByLabelText('Region')).not.toBeInTheDocument()
    expect(within(dialog).queryByRole('checkbox')).not.toBeInTheDocument()
    await user.type(within(dialog).getByLabelText('Name'), ' spare-2 ')
    await user.click(within(dialog).getByRole('button', { name: 'Create pool' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Pool spare-2 created.')
    expect(writes(calls)).toEqual([expect.objectContaining({ method: 'POST', path: '/provider/relay-pools', body: { name: 'spare-2' } })])
    await waitFor(() => expect(calls.filter((c) => c.path === '/provider/relay-pools' && c.method === 'GET')).toHaveLength(2))
  })

  it('a super-admin can set region and residency; residency needs a valid region first', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/relay-pools': listOk,
      'POST /provider/relay-pools': { status: 201, body: { ...EU_POOL, name: 'eu-2' } },
    })
    renderApp('/relay-pools')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Create pool' }))
    const dialog = await screen.findByRole('dialog', { name: 'Create pool' })
    const create = within(dialog).getByRole('button', { name: 'Create pool' })
    await user.type(within(dialog).getByLabelText('Name'), 'eu-2')
    await user.click(within(dialog).getByRole('checkbox', { name: /^Residency pool$/ }))
    expect(within(dialog).getByText('Residency needs a region.')).toBeInTheDocument()
    expect(create).toBeDisabled()
    await user.type(within(dialog).getByLabelText('Region'), 'EU_Central')
    expect(within(dialog).getByText(/Use a lowercase code/)).toBeInTheDocument()
    expect(create).toBeDisabled()
    await user.clear(within(dialog).getByLabelText('Region'))
    await user.type(within(dialog).getByLabelText('Region'), 'eu-central-1')
    expect(within(dialog).getByText(/once every active relay supports the workspace policy/)).toBeInTheDocument()
    await user.click(create)
    await screen.findByRole('status')
    expect(writes(calls)[0].body).toEqual({ name: 'eu-2', region: 'eu-central-1', residency: true })
  })

  it.each([
    ['name_taken', 409, 'Another pool already has this name.'],
    ['invalid_name', 400, 'Enter a name of 1 to 64 characters, without control characters.'],
  ])('%s is shown on the name field', async (code, status, message) => {
    signedInAs(relayOpsMe)
    mockFetch({ 'GET /provider/relay-pools': listOk, 'POST /provider/relay-pools': { status, body: { error: code } } })
    renderApp('/relay-pools')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Create pool' }))
    const dialog = await screen.findByRole('dialog', { name: 'Create pool' })
    await user.type(within(dialog).getByLabelText('Name'), 'default')
    await user.click(within(dialog).getByRole('button', { name: 'Create pool' }))
    expect(await within(dialog).findByText(message)).toBeInTheDocument()
    expect(within(dialog).getByLabelText('Name')).toHaveAttribute('aria-invalid', 'true')
  })
})

describe('Relay pools: detail', () => {
  const path = `/relay-pools/${EU_POOL_ID}`

  it('a super-admin sees relays, tenant names and every management action', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ tenants: 0, tenant_list: [] }) } })
    renderApp(path)
    expect(await screen.findByRole('heading', { name: 'eu-1', level: 1 })).toBeInTheDocument()
    const relays = within(screen.getByRole('table', { name: 'Pool relays' }))
    expect(relays.getByRole('link', { name: 'relay-eu-a' })).toHaveAttribute('href', '/relays/11111111-1111-1111-1111-111111111111')
    expect(within(relays.getByRole('row', { name: 'relay-eu-a' })).getByText('Eligible')).toBeInTheDocument()
    expect(within(relays.getByRole('row', { name: 'relay-eu-b' })).getByText('Not eligible')).toBeInTheDocument()
    expect(screen.getByText('No tenants use this pool as their primary pool.')).toBeInTheDocument()
    const manage = within(screen.getByRole('region', { name: 'Manage pool' }))
    expect(manage.getAllByRole('button').map((b) => b.textContent)).toEqual(['Rename pool', 'Edit region and residency', 'Delete pool'])
    expect(manage.getByRole('button', { name: 'Edit region and residency' })).toBeEnabled()
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })

  it('a super-admin sees the tenant list; region and residency are locked while tenants use the pool', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail() } })
    renderApp(path)
    const tenants = within(await screen.findByRole('table', { name: 'Pool tenants' }))
    expect(tenants.getByText('Acme')).toBeInTheDocument()
    expect(tenants.getByText('acme')).toBeInTheDocument()
    expect(tenants.queryByRole('link')).not.toBeInTheDocument() // tenant detail is an audited read
    expect(screen.getByRole('button', { name: 'Edit region and residency' })).toBeDisabled()
    expect(screen.getByText(/create a new pool with the region and residency you want and move the tenants to it instead/i)).toBeInTheDocument()
  })

  it('relay-ops see the tenant count only, and no region or residency control', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ tenants: 3, tenant_list: null }) } })
    renderApp(path)
    await screen.findByRole('heading', { name: 'eu-1', level: 1 })
    expect(screen.getByText(/3 tenants use this pool\. Tenant names are shown to super-admins only\./)).toBeInTheDocument()
    expect(screen.queryByRole('table', { name: 'Pool tenants' })).not.toBeInTheDocument()
    expect(screen.queryByText('Acme')).not.toBeInTheDocument()
    const manage = within(screen.getByRole('region', { name: 'Manage pool' }))
    expect(manage.getAllByRole('button').map((b) => b.textContent)).toEqual(['Rename pool', 'Delete pool'])
    expect(screen.queryByText(/Region and residency\./)).not.toBeInTheDocument()
  })

  it('the default pool has no management actions', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relay-pools/${DEFAULT_POOL_ID}`]: { status: 200, body: { ...poolDetail(), ...poolSummary() } } })
    renderApp(`/relay-pools/${DEFAULT_POOL_ID}`)
    await screen.findByRole('heading', { name: 'default', level: 1 })
    expect(screen.queryByRole('region', { name: 'Manage pool' })).not.toBeInTheDocument()
    expect(within(screen.getByRole('main')).getAllByRole('button').map((b) => b.textContent?.trim())).toEqual(['Refresh'])
    expect(screen.getByText(/can’t be renamed, changed or deleted/)).toBeInTheDocument()
  })

  it('warns when tenants use the pool and it has no eligible relay', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ eligible_relays: 0, tenant_list: null }) } })
    renderApp(path)
    expect(await screen.findByRole('alert')).toHaveTextContent(/no eligible relay: tenants without a working failover pool have no relay path/)
  })

  it('rename sends only the name and reloads', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({
      [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ tenant_list: null }) },
      [`PATCH /provider/relay-pools/${EU_POOL_ID}`]: { status: 204 },
    })
    renderApp(path)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Rename pool' }))
    const dialog = await screen.findByRole('dialog', { name: 'Rename pool' })
    const submit = within(dialog).getByRole('button', { name: 'Rename pool' })
    expect(submit).toBeDisabled() // unchanged name
    await user.clear(within(dialog).getByLabelText('Name'))
    await user.type(within(dialog).getByLabelText('Name'), 'eu-west')
    await user.click(submit)
    expect(await screen.findByRole('status')).toHaveTextContent('Pool renamed to eu-west.')
    expect(writes(calls)).toEqual([expect.objectContaining({ method: 'PATCH', body: { name: 'eu-west' } })])
    expect(calls.filter((c) => c.method === 'GET' && c.path === `/provider/relay-pools/${EU_POOL_ID}`)).toHaveLength(2)
  })

  it('region/residency: pool_in_use explains to create a new pool and move tenants', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ tenants: 0, tenant_list: [] }) },
      [`PATCH /provider/relay-pools/${EU_POOL_ID}`]: { status: 409, body: { error: 'pool_in_use' } },
    })
    renderApp(path)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Edit region and residency' }))
    const dialog = await screen.findByRole('dialog', { name: 'Region and residency' })
    expect(within(dialog).getByLabelText('Region')).toHaveValue('eu-central-1')
    await user.click(within(dialog).getByRole('checkbox', { name: /Residency pool/ }))
    await user.click(within(dialog).getByRole('button', { name: 'Save' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/used by tenants.*Create a new pool .* move the tenants to it instead/)
    expect(writes(calls)[0].body).toEqual({ region: 'eu-central-1', residency: false })
  })

  it('delete: pool_not_empty is explained in the dialog; success returns to the list', async () => {
    signedInAs(relayOpsMe)
    let status = 409
    const { calls } = mockFetch({
      [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ tenant_list: null }) },
      [`DELETE /provider/relay-pools/${EU_POOL_ID}`]: () => (status === 409 ? { status, body: { error: 'pool_not_empty' } } : { status: 204 }),
      'GET /provider/relay-pools': listOk,
    })
    renderApp(path)
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Delete pool' }))
    const dialog = await screen.findByRole('dialog', { name: 'Delete pool' })
    expect(within(dialog).getByRole('note')).toHaveTextContent(/2 relays and 1 tenant/)
    await user.click(within(dialog).getByRole('button', { name: 'Delete pool' }))
    expect(await within(dialog).findByRole('alert')).toHaveTextContent(/still has relays or tenants \(including tenants using it as their failover pool\)\. Move its relays/)
    status = 204
    await user.click(within(dialog).getByRole('button', { name: 'Delete pool' }))
    expect(await screen.findByRole('heading', { name: 'Relay pools', level: 1 })).toBeInTheDocument()
    expect(writes(calls).map((c) => `${c.method} ${c.path}`)).toEqual([
      `DELETE /provider/relay-pools/${EU_POOL_ID}`,
      `DELETE /provider/relay-pools/${EU_POOL_ID}`,
    ])
  })

  it('an unknown pool shows Not found', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 404, body: { error: 'not_found' } } })
    renderApp(path)
    expect(await screen.findByRole('heading', { name: 'Relay pool not found' })).toBeInTheDocument()
  })
})

// ── D-39: failover state and failover tenants ───────────────────────────────

describe('Relay pools: failover state', () => {
  const failed = { ...POOLS[1], state: 'failed_over' as const, eligible_relays: 0, exhausted_at: new Date(Date.now() - 4 * 60_000).toISOString() }
  const recovering = { ...POOLS[2], state: 'recovering' as const, eligible_relays: 1, eligible_since: new Date(Date.now() - 2 * 60_000).toISOString(), failover_tenants: 3 }

  it('the list shows each pool’s state with its time and the failover count', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ 'GET /provider/relay-pools': { status: 200, body: { items: [POOLS[0], failed, recovering] } } })
    renderApp('/relay-pools')
    const def = within(await screen.findByRole('row', { name: 'default' }))
    expect(def.getByText('Healthy')).toHaveAttribute('data-pool-state', 'healthy')
    const eu = within(screen.getByRole('row', { name: 'eu-1' }))
    expect(eu.getByText('Failed over')).toBeInTheDocument()
    expect(eu.getByText('since 4 min ago')).toBeInTheDocument()
    const spare = within(screen.getByRole('row', { name: 'spare' }))
    expect(spare.getByText('Recovering')).toHaveAttribute('title', expect.stringMatching(/hold-down \(5 minutes by default\)/))
    expect(spare.getByText('eligible again since 2 min ago')).toBeInTheDocument()
    expect(spare.getByLabelText('Failover for 3 tenants')).toHaveTextContent('3')
    const headers = within(screen.getByRole('table', { name: 'Relay pools' })).getAllByRole('columnheader').map((h) => h.textContent)
    expect(headers).toContain('State')
    expect(headers).toContain('Failover for')
  })

  it('a pool used only as a failover pool, with no eligible relay, is flagged', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ 'GET /provider/relay-pools': { status: 200, body: { items: [POOLS[0], { ...POOLS[2], failover_tenants: 2 }] } } })
    renderApp('/relay-pools')
    expect(await screen.findByRole('alert')).toHaveTextContent(/spare is used by tenants but has no eligible relay/)
    expect(within(screen.getByRole('row', { name: 'spare' })).getByText('No eligible relay')).toBeInTheDocument()
  })

  it('detail explains failed_over with its time; relay-ops see counts, never names', async () => {
    signedInAs(relayOpsMe)
    mockFetch({
      [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ ...failed, failover_tenants: 2, tenant_list: null }) },
    })
    renderApp(`/relay-pools/${EU_POOL_ID}`)
    await screen.findByRole('heading', { name: 'eu-1', level: 1 })
    const status = screen.getAllByRole('status').find((el) => el.getAttribute('data-pool-state') === 'failed_over')!
    expect(status).toHaveTextContent(`Failed over since ${new Date(failed.exhausted_at).toLocaleString()}: tenants of this pool that have a failover pool are using it`)
    expect(screen.getByText('Failover for').nextElementSibling).toHaveTextContent('2 tenants')
    expect(screen.queryByText('Acme')).not.toBeInTheDocument()
  })

  it('detail explains recovering and the hold-down', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ ...recovering, id: EU_POOL_ID, name: 'eu-1' }) } })
    renderApp(`/relay-pools/${EU_POOL_ID}`)
    await screen.findByRole('heading', { name: 'eu-1', level: 1 })
    const status = screen.getAllByRole('status').find((el) => el.getAttribute('data-pool-state') === 'recovering')!
    expect(status).toHaveTextContent(/switch back once it has stayed eligible for the hold-down \(5 minutes by default\)/)
  })

  it('a pool used only as a failover pool locks region and residency', async () => {
    signedInAs(superAdminMe)
    mockFetch({ [`GET /provider/relay-pools/${EU_POOL_ID}`]: { status: 200, body: poolDetail({ tenants: 0, tenant_list: [], failover_tenants: 1 }) } })
    renderApp(`/relay-pools/${EU_POOL_ID}`)
    expect(await screen.findByRole('button', { name: 'Edit region and residency' })).toBeDisabled()
    expect(screen.getByText(/as their primary or failover pool\), so these can’t change/)).toBeInTheDocument()
  })
})
