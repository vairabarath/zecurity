import { screen, waitFor, within } from '@testing-library/react'
import { LEGACY_PLAN_ID, PLANS, TENANT_FREE_PLAN } from '@/test/plans'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { TenantDetail, TenantSummary } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { TENANT_DEFAULT_POOL } from '@/test/pools'
import { renderApp, signedInAs } from '@/test/render'

const ID_A = 'aaaaaaaa-1111-2222-3333-444444444444'
const DAY = 24 * 60 * 60_000

const zeroAgents = { pending: 0, active: 0, disconnected: 0, revoked: 0 }
function tenant(over: Partial<TenantSummary>): TenantSummary {
  return {
    id: ID_A,
    slug: 'acme',
    name: 'Acme',
    status: 'active',
    trust_domain: 'acme.zecurity.test',
    created_at: '2026-10-01T00:00:00Z',
    ca_not_after: new Date(Date.now() + 200 * DAY).toISOString(),
    counts: {
      connectors: { pending: 1, active: 3, disconnected: 1, revoked: 0 },
      shields: { ...zeroAgents, active: 5 },
      remote_networks: 2,
      users: { active: 40, suspended: 1, locked: 0, deleted: 2 },
      client_devices: { active: 30, re_enroll_required: 1, renew_pending: 0, revoked: 4 },
      policies: { resources: 12, groups: 4, access_rules: 9, resource_policies: 2 },
    },
    ...over,
  }
}
const A = tenant({})
const B = tenant({ id: 'bbbbbbbb-1111-2222-3333-444444444444', slug: 'beta', name: 'Beta', status: 'suspended', ca_not_after: null, counts: { ...A.counts, connectors: zeroAgents } })

const listCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path === '/provider/tenants')
const detailCalls = (calls: RecordedCall[]) => calls.filter((c) => c.path.startsWith('/provider/tenants/'))
const qs = (c: RecordedCall) => Object.fromEntries(new URLSearchParams(c.search))

describe('Tenants list', () => {
  it('renders the API counts; missing CA shows —', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/tenants': { status: 200, body: { items: [A, B], next_cursor: null } } })
    renderApp('/tenants')
    const a = within(await screen.findByRole('row', { name: 'Acme' }))
    expect(a.getByRole('link', { name: 'Acme' })).toHaveAttribute('href', `/tenants/${ID_A}`)
    expect(a.getByText('acme')).toBeInTheDocument()
    expect(a.getByText('acme.zecurity.test')).toBeInTheDocument()
    expect(a.getByText('OK')).toHaveAttribute('data-bucket', 'ok')
    expect(a.getByText('3 active · 1 pending · 1 disconnected')).toBeInTheDocument()
    expect(a.getByText('5 active')).toBeInTheDocument()
    expect(a.getByText('2')).toBeInTheDocument()
    expect(a.getByText('40')).toBeInTheDocument()
    expect(a.getByText('30')).toBeInTheDocument()
    // F8: resources, with the rest of the policy in the cell's title
    expect(a.getByText('12')).toHaveAttribute('title', '4 groups · 9 access rules · 2 resource policies')
    const b = within(screen.getByRole('row', { name: 'Beta' }))
    expect(b.getByText('suspended')).toBeInTheDocument()
    expect(b.getByText('—')).toBeInTheDocument()
    expect(b.getByText('0')).toBeInTheDocument() // no connectors
  })

  it('never renders identity data, even if a response carried some', async () => {
    signedInAs(superAdminMe)
    const leaky = { ...A, admin_email: 'admin.canary@acme.example', users: [{ email: 'u.canary@acme.example' }] }
    mockFetch({ 'GET /provider/tenants': { status: 200, body: { items: [leaky], next_cursor: null } } })
    renderApp('/tenants')
    await screen.findByRole('row', { name: 'Acme' })
    expect(document.body.textContent).not.toMatch(/canary|@acme\.example/)
  })

  it('the status filter restarts paging at page 1 with no carried cursor', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/tenants': (c) => {
        const p = qs(c)
        const tag = p.status ?? 'all'
        if (p.cursor && !p.cursor.startsWith(tag)) return { status: 400, body: { error: 'invalid_cursor' } }
        const page = p.cursor ? 2 : 1
        return { status: 200, body: { items: [tenant({ name: `${tag}-p${page}` })], next_cursor: page === 1 ? `${tag}#2` : null } }
      },
    })
    renderApp('/tenants')
    const user = userEvent.setup()
    await screen.findByText('all-p1')
    await user.click(screen.getByRole('button', { name: /Next/ }))
    await screen.findByText('all-p2')
    await user.click(within(screen.getByRole('group', { name: 'Status filter' })).getByRole('button', { name: 'Deleted' }))
    expect(await screen.findByText('deleted-p1')).toBeInTheDocument()
    expect(screen.getByText('Page 1')).toBeInTheDocument()
    expect(qs(listCalls(calls).at(-1)!)).toEqual({ status: 'deleted' })
  })

  it('deleted tenants are excluded by default: no status is sent', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/tenants': { status: 200, body: { items: [], next_cursor: null } } })
    renderApp('/tenants')
    expect(await screen.findByText('No tenants yet.')).toBeInTheDocument()
    expect(qs(listCalls(calls)[0])).toEqual({})
  })

  it('hovering a row link makes no tenant detail request; a click makes exactly one', async () => {
    signedInAs(superAdminMe)
    const detail: TenantDetail = { ...A, remote_networks: [], remote_networks_truncated: false, connectors: [], connectors_truncated: false, shields: [], shields_truncated: false, relay_pool: TENANT_DEFAULT_POOL, plan: TENANT_FREE_PLAN, first_admin_invitation: null }
    const { calls } = mockFetch({
      'GET /provider/tenants': { status: 200, body: { items: [A], next_cursor: null } },
      [`GET /provider/tenants/${ID_A}`]: { status: 200, body: detail },
    })
    renderApp('/tenants')
    const user = userEvent.setup()
    const link = await screen.findByRole('link', { name: 'Acme' })
    await user.hover(link)
    link.focus()
    await new Promise((r) => setTimeout(r, 50))
    expect(detailCalls(calls)).toHaveLength(0)
    await user.click(link)
    await screen.findByRole('heading', { name: 'Acme', level: 1 })
    expect(detailCalls(calls)).toHaveLength(1)
  })

  it('relay-ops gets Forbidden without any request, and has no nav entry', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({})
    renderApp('/tenants')
    expect(screen.getByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    expect(within(screen.getByRole('navigation', { name: 'Sections' })).queryByRole('link', { name: 'Tenants' })).not.toBeInTheDocument()
    expect(calls).toHaveLength(0)
  })

  it('a server role 403 shows Forbidden; Refresh is one request; read-only', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/tenants': { status: 200, body: { items: [A], next_cursor: null } } })
    const view = renderApp('/tenants')
    await screen.findByRole('row', { name: 'Acme' })
    const main = screen.getByRole('main')
    const chips = within(screen.getByRole('group', { name: 'Status filter' })).getAllByRole('button')
    expect(chips.map((b) => b.textContent)).toEqual(['All except deleted', 'Active', 'Provisioning', 'Suspended', 'Deleted'])
    // The only write here is Create tenant (Sprint 23 S2); rows stay read-only.
    expect(within(main).getAllByRole('button').filter((b) => !chips.includes(b)).map((b) => b.textContent?.trim())).toEqual(['Refresh', 'Create tenant'])
    expect(within(main).queryByText(/\b(revoke|delete|edit|suspend)\b/i)).not.toBeInTheDocument()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh tenants' }))
    await waitFor(() => expect(listCalls(calls)).toHaveLength(2))
    view.unmount()

    mockFetch({ 'GET /provider/tenants': { status: 403, body: { error: 'forbidden' } } })
    renderApp('/tenants')
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
  })
})

// Sprint 23 S2 (D-44): Create tenant. Super-admin only (the controller
// decides); the reason is in the body; the response never has a token.
describe('Create tenant', () => {
  const NEW_ID = 'cccccccc-1111-2222-3333-444444444444'
  const created = {
    id: NEW_ID,
    slug: 'acme-corp',
    name: 'Acme Corp',
    trust_domain: 'ws-acme-corp.zecurity.in',
    plan_id: LEGACY_PLAN_ID,
    plan_name: 'Legacy',
    invitation: { id: 'inv-1', email: 'first@acme.test', expires_at: '2026-11-25T00:00:00Z' },
  }
  const createCalls = (calls: RecordedCall[]) => calls.filter((c) => c.method === 'POST' && c.path === '/provider/tenants')

  async function openDialog() {
    const user = userEvent.setup()
    await screen.findByRole('row', { name: 'Acme' })
    await user.click(screen.getByRole('button', { name: 'Create tenant' }))
    return { user, dialog: within(await screen.findByRole('dialog', { name: 'Create tenant' })) }
  }

  it('creates a tenant: slug from the name, chosen plan, reason in the body; shows the invitation and reloads the list', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/tenants': { status: 200, body: { items: [A], next_cursor: null } },
      'GET /provider/plans': { status: 200, body: { items: PLANS } },
      'POST /provider/tenants': { status: 201, body: created },
    })
    renderApp('/tenants')
    const { user, dialog } = await openDialog()
    const submit = dialog.getByRole('button', { name: 'Create tenant' })
    expect(submit).toBeDisabled()

    await user.type(dialog.getByLabelText('Name'), 'Acme Corp')
    expect(dialog.getByLabelText('Slug')).toHaveValue('acme-corp')
    expect(dialog.getByText('ws-acme-corp.zecurity.in')).toBeInTheDocument()
    await user.click(await dialog.findByRole('combobox', { name: 'Plan' }))
    await user.click(await screen.findByRole('option', { name: 'Legacy' }))
    await user.type(dialog.getByLabelText('First admin’s email'), 'first@acme.test')
    expect(submit).toBeDisabled()
    await user.type(dialog.getByLabelText('Reason'), 'New customer onboarding')
    await user.click(submit)

    await waitFor(() => expect(createCalls(calls)).toHaveLength(1))
    expect(createCalls(calls)[0].body).toEqual({
      slug: 'acme-corp',
      name: 'Acme Corp',
      plan_id: LEGACY_PLAN_ID,
      admin_email: 'first@acme.test',
      reason: 'New customer onboarding',
    })
    expect(createCalls(calls)[0].search).toBe('')
    const notice = await screen.findByRole('status')
    expect(notice).toHaveTextContent('Created Acme Corp on the Legacy plan. An invitation was sent to first@acme.test')
    expect(within(notice).getByRole('link', { name: 'Acme Corp' })).toHaveAttribute('href', `/tenants/${NEW_ID}`)
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    await waitFor(() => expect(listCalls(calls).filter((c) => c.method === 'GET')).toHaveLength(2))
  })

  it('the default plan sends no plan_id; an invalid slug blocks submit; a server error keeps the dialog open', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/tenants': { status: 200, body: { items: [A], next_cursor: null } },
      'GET /provider/plans': { status: 200, body: { items: PLANS } },
      'POST /provider/tenants': { status: 409, body: { error: 'slug_taken' } },
    })
    renderApp('/tenants')
    const { user, dialog } = await openDialog()
    await user.type(dialog.getByLabelText('Name'), 'Acme')
    await user.type(dialog.getByLabelText('First admin’s email'), 'first@acme.test')
    await user.type(dialog.getByLabelText('Reason'), 'r')
    const slug = dialog.getByLabelText('Slug')
    await user.clear(slug)
    await user.type(slug, 'Bad_Slug')
    expect(slug).toHaveAttribute('aria-invalid', 'true')
    expect(dialog.getByRole('button', { name: 'Create tenant' })).toBeDisabled()
    await user.clear(slug)
    await user.type(slug, 'acme')
    await user.click(dialog.getByRole('button', { name: 'Create tenant' }))

    expect(await dialog.findByRole('alert')).toHaveTextContent('A tenant with this slug already exists.')
    expect(createCalls(calls)[0].body).toEqual({ slug: 'acme', name: 'Acme', admin_email: 'first@acme.test', reason: 'r' })
    expect(screen.getByRole('dialog', { name: 'Create tenant' })).toBeInTheDocument()
  })
})
