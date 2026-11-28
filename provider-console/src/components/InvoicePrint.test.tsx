import { render, screen } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import { InvoicePrint } from '@/components/InvoicePrint'
import { invoice } from '@/test/invoices'

describe('InvoicePrint', () => {
  it('lays out an issued invoice: lines, tax and total, no watermark', () => {
    render(<InvoicePrint invoice={invoice()} providerName="Example Networks" />)
    const doc = screen.getByRole('article', { name: 'Invoice INV-2026-000001' })
    expect(doc).toHaveClass('invoice-print')
    expect(doc).toHaveTextContent('Example Networks')
    expect(doc).toHaveTextContent('Invoice for November 2026')
    expect(doc).toHaveTextContent('Monthly feeINR 1,000.00')
    expect(doc).toHaveTextContent('60.50 GB used, 50.00 GB included; 11 × INR 8.00 per GB')
    expect(doc).toHaveTextContent('7 users used, 5 users included; 2 × INR 300.00 per user')
    expect(doc).toHaveTextContent('SubtotalINR 1,688.00')
    expect(doc).toHaveTextContent('GST 18%INR 303.84')
    expect(doc).toHaveTextContent('TotalINR 1,991.84')
    expect(screen.queryByText('DRAFT')).toBeNull()
    expect(screen.queryByText('Tax not included.')).toBeNull()
  })

  it('a draft has the watermark and no number; no tax says so; incomplete hours are noted', () => {
    render(
      <InvoicePrint
        invoice={invoice({ number: null, status: 'draft', issued_at: null, tax_rate_bp: 0, tax_label: '', tax_minor: 0, total_minor: 168800, incomplete_hours: 1 })}
        providerName="Example Networks"
      />,
    )
    expect(screen.getByText('DRAFT')).toBeInTheDocument()
    expect(screen.getByRole('article', { name: 'Invoice draft' })).toHaveTextContent('Draft (no number yet)')
    expect(screen.getByText('Tax not included.')).toBeInTheDocument()
    expect(screen.getByText(/Usage for 1 hour of this month couldn’t be collected/)).toBeInTheDocument()
  })

  it('a void invoice says it doesn’t need to be paid; a deleted tenant keeps its name', () => {
    render(<InvoicePrint invoice={invoice({ status: 'void', void_reason: 'Wrong plan', purged: true })} providerName="Example Networks" />)
    expect(screen.getByRole('note')).toHaveTextContent('This invoice is void: Wrong plan It doesn’t need to be paid.')
    expect(screen.getByText('(workspace deleted)')).toBeInTheDocument()
  })
})
