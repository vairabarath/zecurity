import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { FREE_PLAN_ID, LEGACY_PLAN_ID, PLANS, PRO_PLAN_ID } from '@/test/plans'
import { renderApp, signedInAs } from '@/test/render'

// Plans (Sprint 23 B1, D-43): super-admin only. A plan defines all six
// limits (a number or unlimited = null); writes that change what tenants may
// create need a reason.

const listOk: FakeResponse = { status: 200, body: { items: PLANS } }
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

describe('Plans: list', () => {
  it('shows every plan with its limits, the default marked, and the right actions', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/plans': listOk })
    renderApp('/plans')
    const free = within(await screen.findByRole('row', { name: 'Free' }))
    expect(free.getByText('Default')).toBeInTheDocument()
    expect(free.getByText('Default plan for new workspaces.')).toBeInTheDocument()
    const cell = (row: typeof free, key: string) => row.getAllByRole('cell').find((c) => c.getAttribute('data-limit') === key)!
    expect(cell(free, 'users')).toHaveTextContent('5')
    expect(cell(free, 'resources')).toHaveTextContent('50')
    expect(cell(free, 'remote_networks')).toHaveTextContent('3')
    // The default plan can't be made default again or deleted.
    expect(free.queryByRole('button', { name: 'Make Free the default' })).not.toBeInTheDocument()
    expect(free.queryByRole('button', { name: 'Delete Free' })).not.toBeInTheDocument()

    const legacy = within(screen.getByRole('row', { name: 'Legacy' }))
    expect(cell(legacy, 'users')).toHaveTextContent('Unlimited')
    expect(legacy.getByRole('button', { name: 'Delete Legacy' })).toBeEnabled()
    // A plan tenants are on can't be deleted.
    const pro = within(screen.getByRole('row', { name: 'Pro' }))
    expect(pro.getByRole('button', { name: 'Delete Pro' })).toBeDisabled()
  })

  it('relay-ops have no Plans entry and the page is forbidden', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [], next_cursor: null } }, 'GET /provider/relay-pools': { status: 200, body: { items: [] } } })
    renderApp('/plans')
    expect(await screen.findByRole('heading', { name: /forbidden|not allowed|access/i })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Plans' })).not.toBeInTheDocument()
  })
})

describe('Plans: writes', () => {
  it('create sends all six limits, unlimited as null', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/plans': listOk,
      'POST /provider/plans': { status: 201, body: { id: 'new', name: 'Team', description: '', limits: {}, is_default: false } },
    })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Create plan' }))
    const dialog = await screen.findByRole('dialog', { name: 'Create plan' })
    const submit = within(dialog).getByRole('button', { name: 'Create plan' })
    await user.type(within(dialog).getByLabelText('Name'), ' Team ')
    // Every limit starts unlimited; set two of them.
    await user.click(within(dialog).getByRole('checkbox', { name: 'Users unlimited' }))
    await user.type(within(dialog).getByLabelText('Users'), '20')
    await user.click(within(dialog).getByRole('checkbox', { name: 'Resources unlimited' }))
    await user.type(within(dialog).getByLabelText('Resources'), 'x')
    expect(submit).toBeDisabled() // not a whole number
    await user.clear(within(dialog).getByLabelText('Resources'))
    await user.type(within(dialog).getByLabelText('Resources'), '200')
    await user.click(submit)
    expect(await screen.findByRole('status')).toHaveTextContent('Plan Team created.')
    expect(writes(calls)).toEqual([
      expect.objectContaining({
        method: 'POST',
        path: '/provider/plans',
        body: {
          name: 'Team',
          description: '',
          limits: { users: 20, resources: 200, remote_networks: null, connectors: null, shields: null, client_devices: null },
        },
      }),
    ])
  })

  it('edit needs a reason and sends only what changed', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/plans': listOk, [`PATCH /provider/plans/${FREE_PLAN_ID}`]: { status: 204 } })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Edit Free' }))
    const dialog = await screen.findByRole('dialog', { name: 'Edit plan' })
    expect(within(dialog).getByText(/apply at once to its 3 tenants/)).toBeInTheDocument()
    const save = within(dialog).getByRole('button', { name: 'Save plan' })
    const users = within(dialog).getByLabelText('Users')
    await user.clear(users)
    await user.type(users, '7')
    expect(save).toBeDisabled() // reason missing
    await user.type(within(dialog).getByLabelText('Reason'), ' launch tweak ')
    await user.click(save)
    expect(await screen.findByRole('status')).toHaveTextContent('Plan Free saved.')
    expect(writes(calls)).toEqual([
      expect.objectContaining({
        method: 'PATCH',
        path: `/provider/plans/${FREE_PLAN_ID}`,
        body: {
          limits: { users: 7, resources: 50, remote_networks: 3, connectors: 10, shields: 10, client_devices: 25 },
          reason: 'launch tweak',
        },
      }),
    ])
  })

  it('make default sends the reason', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/plans': listOk, [`POST /provider/plans/${PRO_PLAN_ID}/default`]: { status: 204 } })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Make Pro the default' }))
    const dialog = await screen.findByRole('dialog', { name: 'Make default plan' })
    expect(within(dialog).getByText(/Existing tenants keep their plan/)).toBeInTheDocument()
    await user.type(within(dialog).getByLabelText('Reason'), 'promo')
    await user.click(within(dialog).getByRole('button', { name: 'Make default' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Pro is now the default plan.')
    expect(writes(calls)[0]).toEqual(expect.objectContaining({ path: `/provider/plans/${PRO_PLAN_ID}/default`, body: { reason: 'promo' } }))
  })

  it('delete explains a refusal in the dialog', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      'GET /provider/plans': listOk,
      [`DELETE /provider/plans/${LEGACY_PLAN_ID}`]: { status: 409, body: { error: 'plan_in_use' } },
    })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Delete Legacy' }))
    const dialog = await screen.findByRole('dialog', { name: 'Delete plan' })
    await user.type(within(dialog).getByLabelText('Reason'), 'retire')
    await user.click(within(dialog).getByRole('button', { name: 'Delete plan' }))
    await waitFor(() => expect(within(dialog).getByRole('alert')).toHaveTextContent('Tenants are on this plan. Move them to another plan first.'))
  })
})
