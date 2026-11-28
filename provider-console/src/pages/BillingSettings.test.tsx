import { render, screen, waitFor, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import type { BillingSettings } from '@/api/types'
import { TenantInvoices } from '@/components/TenantInvoices'
import { mockFetch, superAdminMe, type RecordedCall } from '@/test/fetch'
import { invoice } from '@/test/invoices'
import { PLANS, planCard } from '@/test/plans'
import { renderApp, signedInAs } from '@/test/render'

// Billing settings page and the tenant Invoices section (Sprint 26 P4;
// D-64, D-66, D-67, D-68, D-69).

const SETTINGS: BillingSettings = { country: 'US', currency: 'USD', tax_rate_bp: 0, tax_label: '', invoice_retention_years: 8, email_tenants: true }
const CARDS = [...PLANS.map((p) => ({ ...p, price: null, revision: 1 })), planCard({ id: 'c1' }), planCard({ id: 'c2', name: 'Team 2' })]
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

describe('Billing settings page', () => {
  it('counts the priced cards for the currency switch and saves only the changes with the reason', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/billing/settings': { status: 200, body: { ...SETTINGS, countries: [{ code: 'US', name: 'United States', currency: 'USD' }, { code: 'IN', name: 'India', currency: 'INR' }] } },
      'GET /provider/plans': { status: 200, body: { items: CARDS } },
      'PATCH /provider/billing/settings': { status: 204 },
    })
    renderApp('/billing/settings')
    const user = userEvent.setup()
    await user.selectOptions(await screen.findByLabelText('Country'), 'IN')
    expect(screen.getByLabelText('Currency')).toHaveValue('INR')
    // Two cards have a price; the four plans without one don't switch.
    expect(screen.getByText(/All 2 plan cards switch to INR\. Amounts are not converted/)).toBeInTheDocument()
    await user.click(screen.getByLabelText('I’ll review the card prices'))
    await user.type(screen.getByLabelText('Reason'), 'moving to India')
    await user.click(screen.getByRole('button', { name: /save/i }))
    expect(await screen.findByRole('status')).toHaveTextContent('Billing settings saved.')
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'PATCH', path: '/provider/billing/settings', body: { country: 'IN', currency: 'INR', reason: 'moving to India' } }),
    ])
    // After a save the page reads the settings again.
    await waitFor(() => expect(calls.filter((c) => c.path === '/provider/billing/settings' && c.method === 'GET')).toHaveLength(2))
  })

  it('falls back to the built-in countries when the controller serves none', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/billing/settings': { status: 200, body: SETTINGS }, 'GET /provider/plans': { status: 200, body: { items: CARDS } } })
    renderApp('/billing/settings')
    const country = await screen.findByLabelText('Country')
    expect(within(country).getByRole('option', { name: 'India' })).toBeInTheDocument()
  })

  it('a failed read offers a retry', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/billing/settings': { status: 500, body: { error: 'internal' } }, 'GET /provider/plans': { status: 200, body: { items: CARDS } } })
    renderApp('/billing/settings')
    expect(await screen.findByText('Couldn’t load the billing settings.')).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Retry' })).toBeInTheDocument()
  })
})

describe('Tenant invoices section', () => {
  it('lists the tenant’s invoices with one read, linking to each', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/invoices': {
        status: 200,
        body: { items: [{ ...invoice(), last_email_status: 'sent' }, { ...invoice({ id: 'd1', number: null, status: 'draft' }), last_email_status: null }], totals: {}, currency: 'INR', next_cursor: null },
      },
    })
    render(
      <MemoryRouter>
        <TenantInvoices tenantId="t-acme" />
      </MemoryRouter>,
    )
    const table = within(await screen.findByRole('table', { name: 'Tenant invoices' }))
    expect(table.getByRole('link', { name: 'INV-2026-000001' })).toHaveAttribute('href', '/invoices/inv-1')
    expect(table.getByRole('link', { name: 'Draft' })).toHaveAttribute('href', '/invoices/d1')
    expect(table.getAllByText('INR 1,991.84')).toHaveLength(2)
    expect(calls).toHaveLength(1)
    expect(Object.fromEntries(new URLSearchParams(calls[0].search))).toEqual({ tenant_id: 't-acme', limit: '12' })
  })

  it('says when there are none yet', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/invoices': { status: 200, body: { items: [], totals: {}, currency: 'USD', next_cursor: null } } })
    render(
      <MemoryRouter>
        <TenantInvoices tenantId="t-acme" />
      </MemoryRouter>,
    )
    expect(await screen.findByText(/No invoices yet\. A month is invoiced after it closes\./)).toBeInTheDocument()
  })
})
