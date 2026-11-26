import { BILLING_METERS, type Invoice } from '@/api/types'
import { csvCell } from '@/lib/auditExport'
import { formatRate, METER_LABELS, moneyInput, unitName } from '@/lib/billing-money'

// Invoice export (Sprint 26 P4): CSV and JSON of the invoices shown, built
// in the browser from what the API returned (nothing is added). Amounts are
// written as decimal text in the invoice's currency ("1991.84"), converted
// from minor units without floating point, next to a currency column.
// Spreadsheet formula cells are neutralised (csvCell, as in the audit export).

const INVOICE_COLUMNS = [
  'number',
  'status',
  'tenant_name',
  'tenant_slug',
  'tenant_deleted',
  'month',
  'currency',
  'plan_name',
  'subtotal',
  'tax_label',
  'tax_rate',
  'tax',
  'total',
  'incomplete_hours',
  'issued_at',
  'paid_at',
  'paid_reference',
  'void_reason',
  'id',
] as const

function invoiceRow(i: Invoice): Record<(typeof INVOICE_COLUMNS)[number], unknown> {
  return {
    number: i.number ?? 'Draft',
    status: i.status,
    tenant_name: i.tenant_name,
    tenant_slug: i.tenant_slug,
    tenant_deleted: i.purged ? 'yes' : 'no',
    month: i.month.slice(0, 7),
    currency: i.currency,
    plan_name: i.plan_name,
    subtotal: moneyInput(i.subtotal_minor, i.currency),
    tax_label: i.tax_rate_bp > 0 ? i.tax_label : '',
    tax_rate: i.tax_rate_bp > 0 ? formatRate(i.tax_rate_bp) : 'not included',
    tax: moneyInput(i.tax_minor, i.currency),
    total: moneyInput(i.total_minor, i.currency),
    incomplete_hours: i.incomplete_hours,
    issued_at: i.issued_at,
    paid_at: i.paid_at,
    paid_reference: i.paid_reference,
    void_reason: i.void_reason,
    id: i.id,
  }
}

/** One row per invoice. */
export function toInvoicesCSV(invoices: Invoice[]): string {
  const lines = [INVOICE_COLUMNS.join(',')]
  for (const i of invoices) {
    const r = invoiceRow(i)
    lines.push(INVOICE_COLUMNS.map((c) => csvCell(r[c])).join(','))
  }
  return lines.join('\r\n') + '\r\n'
}

const LINE_COLUMNS = ['invoice_number', 'invoice_id', 'position', 'item', 'meter', 'quantity', 'included', 'unit', 'billable_units', 'unit_price', 'amount', 'currency'] as const

/** One row per invoice line (invoices without loaded lines are skipped). */
export function toInvoiceLinesCSV(invoices: Invoice[]): string {
  const out = [LINE_COLUMNS.join(',')]
  for (const i of invoices) {
    for (const l of i.lines ?? []) {
      const meter = l.meter && (BILLING_METERS as readonly string[]).includes(l.meter) ? l.meter : null
      const row: Record<(typeof LINE_COLUMNS)[number], unknown> = {
        invoice_number: i.number ?? 'Draft',
        invoice_id: i.id,
        position: l.position,
        item: l.kind === 'base' ? 'Monthly fee' : meter ? METER_LABELS[meter] : l.meter,
        meter: l.meter ?? '',
        quantity: l.quantity,
        included: l.included,
        unit: meter && l.unit ? unitName(meter, l.unit) : '',
        billable_units: l.billable_units,
        unit_price: l.unit_price_minor === null ? '' : moneyInput(l.unit_price_minor, i.currency),
        amount: moneyInput(l.amount_minor, i.currency),
        currency: i.currency,
      }
      out.push(LINE_COLUMNS.map((c) => csvCell(row[c])).join(','))
    }
  }
  return out.join('\r\n') + '\r\n'
}

/** The invoices as the API returned them (amounts stay in minor units). */
export function toInvoicesJSON(invoices: Invoice[]): string {
  return JSON.stringify(invoices, null, 2) + '\n'
}
