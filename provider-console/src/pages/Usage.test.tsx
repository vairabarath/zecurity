import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { TenantMonthUsage } from '@/api/usage'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { renderApp, signedInAs } from '@/test/render'

// Sprint 25 U3 (F33; D-58, D-59, D-60): the fleet Usage page — super-admin
// only, one read per view / month / sort / page (each audited), deleted
// tenants named without a link, incomplete months said, export of every page.

const m = (over: Record<string, number> = {}) => ({ relay_bytes_to_clients: 0, relay_bytes_to_connectors: 0, session_minutes: 0, connector_minutes: 0, shield_minutes: 0, ...over })
const ROWS: TenantMonthUsage[] = [
  { tenant: { id: '11111111-1111-1111-1111-111111111111', name: 'Beta', slug: 'beta', plan_name: 'Free', purged: false }, meters: m({ relay_bytes_to_clients: 5e9 }), active_users: 3 },
  { tenant: { id: '22222222-2222-2222-2222-222222222222', name: 'Gone Ltd', slug: 'gone', plan_name: null, purged: true }, meters: m({ relay_bytes_to_clients: 4e9 }), active_users: 1 },
  { tenant: { id: '33333333-3333-3333-3333-333333333333', name: 'Acme', slug: 'acme', plan_name: 'Free', purged: false }, meters: m({ relay_bytes_to_clients: 3e9 }), active_users: 9 },
]
const page = (items: TenantMonthUsage[], next: string | null, complete = false) => ({
  status: 200,
  body: { month: '2026-11-01T00:00:00Z', complete, collected_minutes: 21600, minutes: 43200, sort: 'relay_bytes_to_clients', items, next_cursor: next },
})
const qs = (c: RecordedCall) => Object.fromEntries(new URLSearchParams(c.search))
const thisMonth = () => {
  const d = new Date()
  return `${d.getUTCFullYear()}-${String(d.getUTCMonth() + 1).padStart(2, '0')}`
}

afterEach(() => vi.restoreAllMocks())

describe('Usage page', () => {
  it('super-admin: this month, largest first; deleted tenants named without a link; incomplete month said', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/usage/tenants': page(ROWS, null) })
    renderApp('/usage')
    const table = await screen.findByRole('table', { name: 'Usage by tenant' })
    const rows = within(table).getAllByRole('row').slice(1)
    expect(rows.map((r) => r.getAttribute('aria-label'))).toEqual(['Beta', 'Gone Ltd', 'Acme'])
    expect(within(rows[0]).getByRole('link', { name: 'Beta' })).toHaveAttribute('href', '/tenants/11111111-1111-1111-1111-111111111111')
    expect(within(rows[1]).queryByRole('link')).not.toBeInTheDocument()
    expect(within(rows[1]).getByText('Deleted')).toBeInTheDocument()
    expect(within(rows[0]).getByText('5.0 GB')).toBeInTheDocument()
    expect(screen.getByText('Incomplete')).toBeInTheDocument()
    expect(screen.getByText(/50% of the month’s minutes counted so far/)).toBeInTheDocument()
    expect(calls.map(qs)).toEqual([{ month: thisMonth(), sort: 'relay_bytes_to_clients', limit: '50' }])
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })

  it('sort, previous month and paging each read once', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/usage/tenants': (c) => (qs(c).cursor === 'p2' ? page(ROWS.slice(2), null) : page(ROWS.slice(0, 2), 'p2')),
    })
    renderApp('/usage?month=2026-10')
    await screen.findByRole('table', { name: 'Usage by tenant' })
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Next' }))
    await waitFor(() => expect(calls.at(-1) && qs(calls.at(-1)!)).toEqual({ month: '2026-10', sort: 'relay_bytes_to_clients', cursor: 'p2', limit: '50' }))
    await user.click(within(screen.getByRole('group', { name: 'Sort by' })).getByRole('button', { name: 'Active users' }))
    await waitFor(() => expect(qs(calls.at(-1)!)).toEqual({ month: '2026-10', sort: 'active_users', limit: '50' }))
    await user.click(screen.getByRole('button', { name: 'Previous month' }))
    await waitFor(() => expect(qs(calls.at(-1)!)).toEqual({ month: '2026-09', sort: 'active_users', limit: '50' }))
    expect(calls).toHaveLength(4)
  })

  it('a future or malformed month falls back to this month', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/usage/tenants': page([], null) })
    renderApp('/usage?month=2999-01&sort=bogus')
    expect(await screen.findByText('No tenants with usage in this month.')).toBeInTheDocument()
    expect(qs(calls[0])).toEqual({ month: thisMonth(), sort: 'relay_bytes_to_clients', limit: '50' })
    expect(screen.getByRole('button', { name: 'Next month' })).toBeDisabled()
  })

  it('export collects every page of the month and marks completeness', async () => {
    signedInAs(superAdminMe)
    const files: { name: string; text: Promise<string> }[] = []
    vi.spyOn(URL, 'createObjectURL').mockImplementation((b) => {
      files.push({ name: '', text: (b as Blob).text() })
      return 'blob:test'
    })
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => {})
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (this: HTMLAnchorElement) {
      files[files.length - 1].name = this.download
    })
    const { calls } = mockFetch({
      'GET /provider/usage/tenants': (c) => (qs(c).cursor === 'p2' ? page(ROWS.slice(2), null) : page(ROWS.slice(0, 2), 'p2')),
    })
    renderApp('/usage?month=2026-10')
    await screen.findByRole('table', { name: 'Usage by tenant' })
    const before = calls.length
    await userEvent.setup().click(screen.getByRole('button', { name: 'CSV' }))
    await waitFor(() => expect(files).toHaveLength(1))
    expect(calls.slice(before).map(qs)).toEqual([
      { month: '2026-10', sort: 'relay_bytes_to_clients', limit: '200' },
      { month: '2026-10', sort: 'relay_bytes_to_clients', cursor: 'p2', limit: '200' },
    ])
    expect(files[0].name).toBe('usage-2026-10.csv')
    const lines = (await files[0].text).trimEnd().split('\r\n')
    expect(lines).toHaveLength(4)
    expect(lines[2]).toMatch(/^2026-10,false,22222222-2222-2222-2222-222222222222,Gone Ltd,gone,true,,4000000000,/)
  })

  it('relay-ops: no Usage in the nav, and the page is forbidden', async () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({ 'GET /provider/usage/tenants': page(ROWS, null) })
    renderApp('/usage')
    expect(await screen.findByRole('heading', { name: /forbidden|not allowed|access/i })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: /Usage/ })).not.toBeInTheDocument()
    expect(calls.filter((c) => c.path.startsWith('/provider/usage'))).toHaveLength(0)
  })
})
