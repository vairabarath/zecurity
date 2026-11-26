import { fireEvent, screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import type { InvoiceDetail } from '@/api/types'
import { mockFetch, superAdminMe, type RecordedCall } from '@/test/fetch'
import { invoice } from '@/test/invoices'
import { renderApp, signedInAs } from '@/test/render'

// Invoice detail (Sprint 26 P4; D-66, D-68): the actions follow the
// lifecycle; void needs a reason and the typed number; email deliveries show
// counts, never addresses.

const detail = (over: Partial<InvoiceDetail> = {}): InvoiceDetail => ({
  ...invoice(),
  lines: invoice().lines!,
  emails: [{ kind: 'issued', seq: 1, status: 'sent', recipients_count: 2, attempts: 1, sent_at: '2026-12-01T09:01:00Z', last_error: null }],
  history: [
    { status: 'draft', at: '2026-12-01T06:00:00Z', actor: 'system', reason: null },
    { status: 'issued', at: '2026-12-01T09:00:00Z', actor: 'admin@provider.test', reason: 'reviewed' },
  ],
  email_tenants: true,
  provider_name: 'Example Networks',
  ...over,
})
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')
const buttons = () => screen.getAllByRole('button').map((b) => b.textContent?.trim())

describe('Invoice detail', () => {
  it('a draft offers Issue only; the confirmation says the admins are emailed; one POST', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/invoices/inv-1': { status: 200, body: detail({ status: 'draft', number: null, emails: [], issued_at: null }) },
      'POST /provider/invoices/inv-1/issue': { status: 204 },
    })
    renderApp('/invoices/inv-1')
    await screen.findByRole('heading', { name: /Draft/ })
    expect(buttons()).toContain('Issue')
    expect(buttons()).not.toContain('Mark paid')
    expect(buttons()).not.toContain('Void')
    expect(buttons()).not.toContain('Resend email')
    expect(screen.getByText('Emailed when issued.')).toBeInTheDocument()
    expect(screen.getByText('DRAFT')).toBeInTheDocument()
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Issue' }))
    const dialog = within(await screen.findByRole('dialog'))
    expect(dialog.getByText(/The tenant’s admins are emailed the invoice\./)).toBeInTheDocument()
    await user.click(dialog.getByRole('button', { name: 'Issue invoice' }))
    expect(await screen.findByRole('status')).toHaveTextContent('Invoice issued.')
    expect(writes(calls)).toEqual([expect.objectContaining({ path: '/provider/invoices/inv-1/issue', body: {} })])
  })

  it('an issued invoice offers Mark paid, Void and Resend; history and email deliveries show', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/invoices/inv-1': { status: 200, body: detail() } })
    renderApp('/invoices/inv-1')
    await screen.findByRole('heading', { name: /INV-2026-000001/ })
    expect(buttons()).toEqual(expect.arrayContaining(['Mark paid', 'Void', 'Resend email']))
    expect(buttons()).not.toContain('Issue')
    const history = within(screen.getByRole('list', { name: 'Status history' }))
    expect(history.getByText('Reason: reviewed')).toBeInTheDocument()
    const emails = within(screen.getByRole('table', { name: 'Email deliveries' }))
    const cells = within(emails.getAllByRole('row')[1]).getAllByRole('cell').map((c) => c.textContent)
    expect(cells.slice(0, 3)).toEqual(['Invoice', 'Sent', '2'])
    expect(screen.getByText(/plan Team \(revision 3\)/)).toBeInTheDocument()
  })

  it('a paid entry shows its calendar date (no time, no zone shift) and its payment reference', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      'GET /provider/invoices/inv-1': {
        status: 200,
        body: detail({
          status: 'paid',
          paid_at: '2026-12-05',
          paid_reference: 'NEFT 8842',
          history: [
            { status: 'issued', at: '2026-12-01T09:00:00Z', actor: 'admin@provider.test', reason: null },
            { status: 'paid', at: '2026-12-05T00:00:00Z', actor: 'admin@provider.test', reason: 'NEFT 8842' },
          ],
        }),
      },
    })
    renderApp('/invoices/inv-1')
    const history = within(await screen.findByRole('list', { name: 'Status history' }))
    const paid = new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeZone: 'UTC' }).format(new Date('2026-12-05T00:00:00Z'))
    expect(history.getByText(new RegExp(`^${paid} ·`))).toBeInTheDocument()
    expect(history.getByText('Reference: NEFT 8842')).toBeInTheDocument()
    expect(history.queryByText('Reason: NEFT 8842')).toBeNull()
  })

  it('void needs a reason and the typed invoice number', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/invoices/inv-1': { status: 200, body: detail() },
      'POST /provider/invoices/inv-1/void': { status: 204 },
    })
    renderApp('/invoices/inv-1')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Void' }))
    const dialog = within(await screen.findByRole('dialog'))
    const confirm = dialog.getByRole('button', { name: 'Void invoice' })
    await user.type(dialog.getByLabelText('Reason'), 'wrong plan')
    expect(confirm).toBeDisabled()
    const typed = dialog.getByRole('textbox', { name: /Type/ })
    await user.type(typed, 'INV-2026-00000')
    expect(confirm).toBeDisabled()
    await user.type(typed, '1')
    await user.click(confirm)
    expect(await screen.findByRole('status')).toHaveTextContent('INV-2026-000001 voided.')
    expect(writes(calls)).toEqual([expect.objectContaining({ path: '/provider/invoices/inv-1/void', body: { reason: 'wrong plan' } })])
  })

  it('mark paid sends the date, reference and reason; a future date or long reference blocks saving', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({
      'GET /provider/invoices/inv-1': { status: 200, body: detail() },
      'POST /provider/invoices/inv-1/mark-paid': { status: 204 },
    })
    renderApp('/invoices/inv-1')
    const user = userEvent.setup()
    await user.click(await screen.findByRole('button', { name: 'Mark paid' }))
    const dialog = within(await screen.findByRole('dialog', { name: 'Mark INV-2026-000001 paid' }))
    const save = dialog.getByRole('button', { name: 'Mark paid' })
    const date = dialog.getByLabelText('Paid on')
    fireEvent.change(date, { target: { value: '2999-01-01' } })
    await user.type(dialog.getByLabelText('Reason'), 'payment received')
    expect(save).toBeDisabled()
    fireEvent.change(date, { target: { value: '2026-11-20' } })
    await user.type(dialog.getByLabelText('Reference'), 'x'.repeat(201))
    expect(save).toBeDisabled()
    await user.clear(dialog.getByLabelText('Reference'))
    await user.type(dialog.getByLabelText('Reference'), ' UTR 991 ')
    await user.click(save)
    expect(await screen.findByRole('status')).toHaveTextContent('INV-2026-000001 marked paid.')
    expect(writes(calls)).toEqual([
      expect.objectContaining({ path: '/provider/invoices/inv-1/mark-paid', body: { paid_at: '2026-11-20', reference: 'UTR 991', reason: 'payment received' } }),
    ])
  })

  it('a paid invoice offers Resend only; a refusal is shown in the dialog', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      'GET /provider/invoices/inv-1': { status: 200, body: detail({ status: 'paid', paid_at: '2026-12-05', paid_reference: 'UTR 991' }) },
      'POST /provider/invoices/inv-1/resend': { status: 409, body: { error: 'invoice_not_issued' } },
    })
    renderApp('/invoices/inv-1')
    await screen.findByRole('heading', { name: /INV-2026-000001/ })
    expect(buttons()).toContain('Resend email')
    expect(buttons()).not.toContain('Mark paid')
    expect(buttons()).not.toContain('Void')
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Resend email' }))
    const dialog = within(await screen.findByRole('dialog'))
    await user.type(dialog.getByLabelText('Reason'), 'admin lost it')
    await user.click(dialog.getByRole('button', { name: 'Resend email' }))
    expect(await dialog.findByText(/Only an issued invoice can be marked paid or voided/)).toBeInTheDocument()
  })

  it('warns about incomplete hours and marks a deleted tenant', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/invoices/inv-1': { status: 200, body: detail({ incomplete_hours: 6, purged: true }) } })
    renderApp('/invoices/inv-1')
    expect(await screen.findByText(/6 hours were not fully counted this month/)).toBeInTheDocument()
    expect(screen.getByText(/This tenant was deleted/)).toBeInTheDocument()
  })

  it('an unknown invoice says so', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'GET /provider/invoices/nope': { status: 404, body: { error: 'not_found' } } })
    renderApp('/invoices/nope')
    expect(await screen.findByText(/That invoice doesn’t exist/)).toBeInTheDocument()
  })
})
