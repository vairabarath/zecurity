import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { BillingSettings, PlanRevision } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { FREE_LIMITS, FREE_PLAN_ID, LEGACY_PLAN_ID, PLANS, planCard } from '@/test/plans'
import { renderApp, signedInAs } from '@/test/render'

// Plans as cards (Sprint 23 B1 + Sprint 26 P4; D-43, D-63): super-admin only.
// A card is the plan's six limits plus an optional price in the settings
// currency; every write that changes it carries a reason.

const SETTINGS: BillingSettings = {
  country: 'IN',
  currency: 'INR',
  tax_rate_bp: 1800,
  tax_label: 'GST',
  invoice_retention_years: 8,
  email_tenants: true,
}
const TEAM_ID = '00000000-0000-0000-0001-0000000000b2'
const TEAM = planCard({ id: TEAM_ID })
const CARDS = [...PLANS.map((p) => ({ ...p, price: null, revision: 1 })), TEAM]
const listOk: FakeResponse = { status: 200, body: { items: CARDS } }
const settingsOk: FakeResponse = { status: 200, body: SETTINGS }
const routes = { 'GET /provider/plans': listOk, 'GET /provider/billing/settings': settingsOk }
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')
const card = async (name: string) => within(await screen.findByRole('article', { name }))
const REASON = 'pricing for launch'

describe('Plans: cards', () => {
  it('shows each card with its price or "Not billable", its limits, and the right actions', async () => {
    signedInAs(superAdminMe)
    mockFetch(routes)
    renderApp('/plans')
    const free = await card('Free')
    expect(free.getByText('Default')).toBeInTheDocument()
    expect(free.getByTestId('plan-price')).toHaveTextContent('Not billable')
    const limit = (c: typeof free, key: string) => c.getAllByRole('definition').find((d) => d.getAttribute('data-limit') === key)!
    expect(limit(free, 'users')).toHaveTextContent('5')
    // The default card can't be made default again, deleted, or lose a price it doesn't have.
    expect(free.queryByRole('button', { name: 'Make Free the default' })).not.toBeInTheDocument()
    expect(free.queryByRole('button', { name: 'Delete Free' })).not.toBeInTheDocument()
    expect(free.queryByRole('button', { name: 'Remove the price of Free' })).not.toBeInTheDocument()

    const team = await card('Team')
    expect(team.getByTestId('plan-price')).toHaveTextContent('INR 1,000.00')
    expect(team.getByText(/50 GB included, then INR 8\.00 per GB/)).toBeInTheDocument()
    expect(team.getByRole('button', { name: 'Remove the price of Team' })).toBeInTheDocument()
    // A card tenants are on can't be deleted.
    expect(team.getByRole('button', { name: 'Delete Team' })).toBeDisabled()
    expect(screen.getByText(/Prices are in INR\./)).toBeInTheDocument()
    expect(screen.getByRole('link', { name: 'Change the billing currency' })).toHaveAttribute('href', '/billing/settings')
  })

  it('without the billing settings, cards still show but can’t be created or edited', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/plans': listOk, 'GET /provider/billing/settings': { status: 500, body: { error: 'internal' } } })
    renderApp('/plans')
    const team = await card('Team')
    expect(await screen.findByText(/Couldn’t load the billing settings/)).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'New card' })).toBeDisabled()
    expect(team.queryByRole('button', { name: 'Edit Team' })).not.toBeInTheDocument()
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
  it('a new card is one POST with its limits, price and reason, in the settings currency', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ ...routes, 'POST /provider/plans': { status: 201, body: { id: 'new' } } })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'New card' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'New plan card' }))
    await user.type(dialog.getByLabelText('Name'), 'Starter')
    await user.click(dialog.getByLabelText('Bill tenants on this card'))
    await user.type(dialog.getByLabelText('Monthly fee (INR)'), '499')
    await user.click(dialog.getByLabelText('Bill relay traffic to clients'))
    await user.type(dialog.getByLabelText(/^Included$/), '50')
    await user.type(dialog.getByLabelText('Price per GB (INR)'), '2')
    await user.type(dialog.getByLabelText('Reason'), REASON)
    await user.click(dialog.getByRole('button', { name: 'Create card' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Plan card Starter created.')
    expect(writes(calls)).toEqual([
      expect.objectContaining({
        method: 'POST',
        path: '/provider/plans',
        body: {
          name: 'Starter',
          description: '',
          limits: expect.objectContaining({ users: null }),
          price: {
            currency: 'INR',
            base_fee_minor: 49900,
            meters: { relay_bytes_to_clients: { included: 50_000_000_000, unit: 1_000_000_000, unit_price_minor: 200 } },
          },
          reason: REASON,
        },
      }),
    ])
  })

  it('a new card starts with no meter billed (session minutes are never on by default)', async () => {
    signedInAs(superAdminMe)
    mockFetch(routes)
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'New card' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'New plan card' }))
    await user.click(dialog.getByLabelText('Bill tenants on this card'))
    for (const label of ['Bill client session time', 'Bill connector time', 'Bill shield time', 'Bill relay traffic to clients', 'Bill active users']) {
      expect(dialog.getByLabelText(label)).not.toBeChecked()
    }
  })

  it('edit sends only what changed with the reason, and says edits bill from next month', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ ...routes, [`PATCH /provider/plans/${FREE_PLAN_ID}`]: { status: 204 } })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click((await card('Free')).getByRole('button', { name: 'Edit Free' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Edit plan card' }))
    expect(dialog.getByText(/Price changes apply from next month: this month is billed at the card as it was on the 1st/)).toBeInTheDocument()
    const users = dialog.getByLabelText('Users')
    await user.clear(users)
    await user.type(users, '7')
    const save = dialog.getByRole('button', { name: 'Save card' })
    expect(save).toBeDisabled() // reason missing
    await user.type(dialog.getByLabelText('Reason'), ' launch tweak ')
    await user.click(save)
    expect(await screen.findByRole('status')).toHaveTextContent('Plan card Free saved.')
    expect(writes(calls)).toEqual([
      expect.objectContaining({
        method: 'PATCH',
        path: `/provider/plans/${FREE_PLAN_ID}`,
        body: { limits: { ...FREE_LIMITS, users: 7 }, reason: 'launch tweak' },
      }),
    ])
  })

  it('duplicate creates a new card from every field of the source', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ ...routes, 'POST /provider/plans': { status: 201, body: { id: 'dup' } } })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click((await card('Team')).getByRole('button', { name: 'Duplicate Team' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Duplicate plan card' }))
    await user.type(dialog.getByLabelText('Reason'), 'custom deal for Acme')
    await user.click(dialog.getByRole('button', { name: 'Create card' }))
    await screen.findByRole('status')
    const body = writes(calls)[0].body as { name: string; price: unknown }
    expect(body.name).toBe('Team (copy)')
    expect(body.price).toEqual(planCard().price)
  })

  it('remove price sends price null with the reason', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ ...routes, [`PATCH /provider/plans/${TEAM_ID}`]: { status: 204 } })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click((await card('Team')).getByRole('button', { name: 'Remove the price of Team' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Remove the price of Team?' }))
    expect(dialog.getByText(/4 tenants use this card\. From next month they get no invoice/)).toBeInTheDocument()
    await user.type(dialog.getByLabelText('Reason'), 'free during beta')
    await user.click(dialog.getByRole('button', { name: 'Remove price' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Team is no longer billable from next month.')
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'PATCH', path: `/provider/plans/${TEAM_ID}`, body: { price: null, reason: 'free during beta' } }),
    ])
  })

  it('history lists each revision with who, when, the reason and what changed', async () => {
    signedInAs(superAdminMe)
    const base = { name: 'Team', description: 'For growing teams.', limits: FREE_LIMITS }
    const price = planCard().price!
    const revs: PlanRevision[] = [
      { revision: 1, changed_at: '2026-11-01T09:00:00Z', changed_by: 'admin@provider.test', reason: 'launch', card: { ...base, price: null } },
      { revision: 2, changed_at: '2026-11-05T09:00:00Z', changed_by: 'admin@provider.test', reason: 'price it', card: { ...base, price } },
      {
        revision: 3,
        changed_at: '2026-11-15T09:00:00Z',
        changed_by: 'ops@provider.test',
        reason: 'cheaper overage',
        card: { ...base, price: { ...price, meters: { ...price.meters, relay_bytes_to_clients: { included: 100_000_000_000, unit: 1_000_000_000, unit_price_minor: 100 } } } },
      },
    ]
    const { calls } = mockFetch({ ...routes, [`GET /provider/plans/${TEAM_ID}/revisions`]: { status: 200, body: { items: revs } } })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click((await card('Team')).getByRole('button', { name: 'History of Team' }))
    const list = within(await screen.findByRole('list', { name: 'Revisions' }))
    const items = list.getAllByRole('listitem').filter((li) => li.getAttribute('aria-label')?.startsWith('Revision'))
    expect(items.map((li) => li.getAttribute('aria-label'))).toEqual(['Revision 3', 'Revision 2', 'Revision 1'])
    expect(within(items[0]).getByText(/Relay traffic to clients: 50 GB included, then INR 8\.00 per GB → 100 GB included, then INR 1\.00 per GB/)).toBeInTheDocument()
    expect(within(items[0]).getByText('Reason: cheaper overage')).toBeInTheDocument()
    expect(within(items[1]).getByText(/Price added: INR 1,000\.00 per month/)).toBeInTheDocument()
    expect(within(items[2]).getByText('Created')).toBeInTheDocument()
    expect(calls.filter((c) => c.path.endsWith('/revisions'))).toHaveLength(1)
  })

  it('make default sends the reason', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ ...routes, [`POST /provider/plans/${LEGACY_PLAN_ID}/default`]: { status: 204 } })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click((await card('Legacy')).getByRole('button', { name: 'Make Legacy the default' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Make default plan' }))
    await user.type(dialog.getByLabelText('Reason'), 'promo')
    await user.click(dialog.getByRole('button', { name: 'Make default' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Legacy is now the default plan.')
    expect(writes(calls)[0]).toEqual(expect.objectContaining({ path: `/provider/plans/${LEGACY_PLAN_ID}/default`, body: { reason: 'promo' } }))
  })

  it('delete explains a refusal in the dialog', async () => {
    signedInAs(superAdminMe)
    mockFetch({ ...routes, [`DELETE /provider/plans/${LEGACY_PLAN_ID}`]: { status: 409, body: { error: 'plan_in_use' } } })
    renderApp('/plans')
    const user = userEvent.setup()
    await user.click((await card('Legacy')).getByRole('button', { name: 'Delete Legacy' }))
    const dialog = await screen.findByRole('dialog', { name: 'Delete plan' })
    await user.type(within(dialog).getByLabelText('Reason'), 'retire')
    await user.click(within(dialog).getByRole('button', { name: 'Delete plan' }))
    await waitFor(() => expect(within(dialog).getByRole('alert')).toHaveTextContent('Tenants are on this plan. Move them to another plan first.'))
  })
})
