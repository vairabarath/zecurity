import { describe, expect, it } from 'vitest'
import { toInvoiceLinesCSV, toInvoicesCSV, toInvoicesJSON } from '@/lib/invoiceExport'
import { invoice } from '@/test/invoices'

describe('invoice export', () => {
  it('one CSV row per invoice, amounts as decimal text with the currency', () => {
    const csv = toInvoicesCSV([invoice(), invoice({ id: 'inv-2', number: null, status: 'draft', tax_rate_bp: 0, tax_label: '', tax_minor: 0, total_minor: 168800, incomplete_hours: 2, issued_at: null })])
    const [head, first, second] = csv.trimEnd().split('\r\n')
    expect(head).toBe(
      'number,status,tenant_name,tenant_slug,tenant_deleted,month,currency,plan_name,subtotal,tax_label,tax_rate,tax,total,incomplete_hours,issued_at,paid_at,paid_reference,void_reason,id',
    )
    expect(first).toBe('INV-2026-000001,issued,Acme,acme,no,2026-11,INR,Team,1688.00,GST,18%,303.84,1991.84,0,2026-12-01T09:00:00Z,,,,inv-1')
    expect(second).toBe('Draft,draft,Acme,acme,no,2026-11,INR,Team,1688.00,,not included,0.00,1688.00,2,,,,,inv-2')
    expect(csv.endsWith('\r\n')).toBe(true)
  })

  it('neutralises formula cells and marks deleted tenants', () => {
    const csv = toInvoicesCSV([invoice({ tenant_name: '=HYPERLINK("http://x")', purged: true, void_reason: '+1 wrong, "quoted"' })])
    expect(csv).toContain(`"'=HYPERLINK(""http://x"")"`)
    expect(csv).toContain(',yes,')
    expect(csv).toContain(`"'+1 wrong, ""quoted"""`)
  })

  it('one CSV row per line, with readable items and units', () => {
    const rows = toInvoiceLinesCSV([invoice(), invoice({ id: 'no-lines', lines: undefined })]).trimEnd().split('\r\n')
    expect(rows).toEqual([
      'invoice_number,invoice_id,position,item,meter,quantity,included,unit,billable_units,unit_price,amount,currency',
      'INV-2026-000001,inv-1,1,Monthly fee,,,,,,,1000.00,INR',
      'INV-2026-000001,inv-1,2,Relay traffic to clients,relay_bytes_to_clients,60500000000,50000000000,GB,11,8.00,88.00,INR',
      'INV-2026-000001,inv-1,3,Active users,active_users,7,5,user,2,300.00,600.00,INR',
    ])
  })

  it('JSON keeps the API shape (minor units)', () => {
    const parsed = JSON.parse(toInvoicesJSON([invoice()]))
    expect(parsed[0].total_minor).toBe(199184)
    expect(parsed[0].lines).toHaveLength(3)
  })
})
