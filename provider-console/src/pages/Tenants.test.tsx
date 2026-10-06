import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { TenantDetail, TenantSummary } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
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
    await user.click(screen.getByRole('combobox', { name: 'Status filter' }))
    await user.click(await screen.findByRole('option', { name: 'deleted' }))
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
    const detail: TenantDetail = { ...A, remote_networks: [], remote_networks_truncated: false, connectors: [], connectors_truncated: false, shields: [], shields_truncated: false }
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
    expect(within(main).getAllByRole('button').map((b) => b.textContent?.trim())).toEqual(['Refresh'])
    expect(within(main).queryByText(/\b(revoke|delete|edit|create|suspend)\b/i)).not.toBeInTheDocument()
    await userEvent.setup().click(screen.getByRole('button', { name: 'Refresh tenants' }))
    await waitFor(() => expect(listCalls(calls)).toHaveLength(2))
    view.unmount()

    mockFetch({ 'GET /provider/tenants': { status: 403, body: { error: 'forbidden' } } })
    renderApp('/tenants')
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
  })
})
