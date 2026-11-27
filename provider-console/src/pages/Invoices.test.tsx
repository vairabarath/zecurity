import { screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { afterEach, describe, expect, it, vi } from 'vitest'
import type { InvoiceListItem, InvoicePage } from '@/api/types'
import { mockFetch, relayOpsMe, superAdminMe, type RecordedCall } from '@/test/fetch'
import { invoice } from '@/test/invoices'
import { renderApp, signedInAs } from '@/test/render'

// Invoices page (Sprint 26 P4; D-66, D-68, D-70): one audited read per
// month, filter, page or Refresh; never polled. Super-admin only.

const qs = (c: RecordedCall) => Object.fromEntries(new URLSearchParams(c.search))
const item = (over: Partial<InvoiceListItem> = {}): InvoiceListItem => ({ ...invoice(), lines: undefined, last_email_status: 'sent', ...over })
const ITEMS: InvoiceListItem[] = [
  item(),
  item({ id: 'inv-2', number: null, status: 'draft', tenant_id: 't-beta', tenant_name: 'Beta', last_email_status: null, incomplete_hours: 6 }),
  item({ id: 'inv-3', number: 'INV-2026-000002', tenant_id: 't-gone', tenant_name: 'Gone Ltd', purged: true, status: 'paid', last_email_status: 'failed' }),
]
const page = (items: InvoiceListItem[], next: string | null = null): { status: number; body: InvoicePage } => ({
  status: 200,
  body: {
    items,
    currency: 'INR',
    next_cursor: next,
    totals: {
      draft: { count: 1, subtotal_minor: 168800, tax_minor: 30384, total_minor: 199184 },
      issued: { count: 1, subtotal_minor: 168800, tax_minor: 30384, total_minor: 199184 },
    },
  },
})
const lastMonth = () => {
  const d = new Date()
  const m = new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth() - 1, 1))
  return `${m.getUTCFullYear()}-${String(m.getUTCMonth() + 1).padStart(2, '0')}`
}

afterEach(() => vi.restoreAllMocks())

describe('Invoices page', () => {
  it('defaults to last month, reads once, and shows totals, status, email status, incomplete hours and deleted tenants', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/invoices': page(ITEMS) })
    renderApp('/invoices')
    const table = await screen.findByRole('table', { name: 'Invoices' })
    expect(calls.map(qs)).toEqual([{ month: lastMonth(), limit: '50' }])
    const rows = within(table).getAllByRole('row').slice(1)
    expect(within(rows[0]).getByRole('link', { name: 'INV-2026-000001' })).toHaveAttribute('href', '/invoices/inv-1')
    expect(within(rows[0]).getByRole('link', { name: 'Acme' })).toHaveAttribute('href', '/tenants/t-acme')
    expect(within(rows[0]).getByText('INR 1,991.84')).toBeInTheDocument()
    expect(within(rows[0]).getByText('Sent')).toBeInTheDocument()
    expect(within(rows[1]).getByRole('link', { name: 'Draft' })).toBeInTheDocument()
    expect(within(rows[1]).getByText('6')).toBeInTheDocument()
    // A purged tenant keeps its name with a Deleted badge and no tenant link.
    expect(within(rows[2]).getByText('Gone Ltd')).toBeInTheDocument()
    expect(within(rows[2]).getByText('Deleted')).toBeInTheDocument()
    expect(within(rows[2]).queryByRole('link', { name: 'Gone Ltd' })).not.toBeInTheDocument()
    expect(within(rows[2]).getByText('Failed')).toBeInTheDocument()
    const totals = within(screen.getByLabelText('Totals per status'))
    expect(totals.getAllByText('INR 1,991.84')).toHaveLength(2)
    expect(calls.every((c) => c.method === 'GET')).toBe(true)
  })

  it('a status filter or another month is one new read each; no polling', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({ 'GET /provider/invoices': page(ITEMS) })
    renderApp('/invoices?month=2026-10')
    await screen.findByRole('table', { name: 'Invoices' })
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Draft' }))
    await waitFor(() => expect(calls).toHaveLength(2))
    await user.click(screen.getByRole('button', { name: 'Previous month' }))
    await waitFor(() => expect(calls).toHaveLength(3))
    expect(calls.map(qs)).toEqual([
      { month: '2026-10', limit: '50' },
      { month: '2026-10', status: 'draft', limit: '50' },
      { month: '2026-09', status: 'draft', limit: '50' },
    ])
  })

  it('regenerate lists the drafts that will change and sends the month with the reason', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/invoices': page(ITEMS),
      'POST /provider/invoices/regenerate': { status: 200, body: { regenerated: 1 } },
    })
    renderApp('/invoices?month=2026-10')
    await screen.findByRole('table', { name: 'Invoices' })
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Regenerate drafts' }))
    const dialog = within(await screen.findByRole('dialog'))
    expect(dialog.getByText('Beta')).toBeInTheDocument()
    expect(dialog.getByText(/Issued, paid and void invoices never change/)).toBeInTheDocument()
    await user.type(dialog.getByLabelText('Reason'), 'fixed the GST rate')
    await user.click(dialog.getByRole('button', { name: 'Regenerate drafts' }))
    expect(await screen.findByRole('status')).toHaveTextContent('1 draft regenerated.')
    expect(calls.filter((c) => c.method !== 'GET')).toEqual([
      expect.objectContaining({ path: '/provider/invoices/regenerate', body: { month: '2026-10', reason: 'fixed the GST rate' } }),
    ])
  })

  it('export reads every page and each invoice (for its lines), and neutralises formula cells', async () => {
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
    const evil = item({ id: 'inv-9', tenant_name: '=HYPERLINK("x")' })
    const { calls } = mockFetch({
      'GET /provider/invoices': (c) => (qs(c).cursor === 'p2' ? page([evil]) : page(ITEMS.slice(0, 1), 'p2')),
      'GET /provider/invoices/inv-1': { status: 200, body: { ...invoice(), emails: [], history: [], email_tenants: true, provider_name: 'P' } },
      'GET /provider/invoices/inv-9': { status: 200, body: { ...invoice({ id: 'inv-9', tenant_name: '=HYPERLINK("x")' }), emails: [], history: [], email_tenants: true, provider_name: 'P' } },
    })
    renderApp('/invoices?month=2026-10')
    await screen.findByRole('table', { name: 'Invoices' })
    const before = calls.length
    await userEvent.setup().click(screen.getByRole('button', { name: 'Export invoices as CSV' }))
    await waitFor(() => expect(files).toHaveLength(1))
    expect(calls.slice(before).map((c) => `${c.path}?${c.search}`)).toEqual([
      '/provider/invoices?month=2026-10&limit=200',
      '/provider/invoices?month=2026-10&cursor=p2&limit=200',
      '/provider/invoices/inv-1?',
      '/provider/invoices/inv-9?',
    ])
    expect(files[0].name).toBe('invoices-2026-10.csv')
    const text = await files[0].text
    expect(text).toContain(`"'=HYPERLINK(""x"")"`)
    expect(text.trimEnd().split('\r\n')).toHaveLength(1 + 2)
  })

  it('relay-ops see no Invoices or Billing settings entry, and the pages are forbidden', async () => {
    signedInAs(relayOpsMe)
    mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [], next_cursor: null } }, 'GET /provider/relay-pools': { status: 200, body: { items: [] } } })
    renderApp('/invoices')
    expect(await screen.findByRole('heading', { name: /forbidden|not allowed|access/i })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Invoices' })).not.toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Billing settings' })).not.toBeInTheDocument()
  })

  it('super-admins see Invoices and Billing settings in the navigation', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/invoices': page([]) })
    renderApp('/invoices')
    await screen.findByText('No invoices for this month yet.')
    expect(screen.getByRole('link', { name: 'Invoices' })).toHaveAttribute('href', '/invoices')
    expect(screen.getByRole('link', { name: 'Billing settings' })).toHaveAttribute('href', '/billing/settings')
  })
})
