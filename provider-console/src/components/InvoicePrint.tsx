import { BILLING_METERS, type Invoice } from '@/api/types'
import { formatMoney, formatQuantity, formatRate, METER_LABELS, unitName } from '@/lib/billing-money'

// Printable invoice (Sprint 26 P4): the invoice detail laid out for the
// browser's "Print / Save as PDF". The same content as the tenant's invoice
// email (D-68). Drafts carry a DRAFT watermark; a void invoice says so.
// The print stylesheet (index.css, .invoice-print) shows only this element,
// black on white.

const formatIssued = (iso: string) => new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeZone: 'UTC' }).format(new Date(iso))

const monthName = (ymd: string) => new Date(`${ymd.slice(0, 7)}-01T00:00:00Z`).toLocaleDateString('en', { month: 'long', year: 'numeric', timeZone: 'UTC' })

export function InvoicePrint({ invoice, providerName }: { invoice: Invoice; providerName: string }) {
  const i = invoice
  const draft = i.status === 'draft'
  return (
    <article className="invoice-print relative mx-auto max-w-3xl rounded-xl border border-border bg-card p-8 text-sm" aria-label={`Invoice ${i.number ?? 'draft'}`}>
      {draft && (
        <div aria-hidden className="invoice-watermark pointer-events-none absolute inset-0 flex items-center justify-center text-7xl font-bold tracking-widest opacity-10">
          DRAFT
        </div>
      )}
      <header className="flex flex-wrap items-start justify-between gap-4">
        <div>
          <p className="font-display text-lg font-semibold">{providerName}</p>
          <h2 className="text-muted-foreground">Invoice for {monthName(i.month)}</h2>
        </div>
        <dl className="grid grid-cols-[auto_auto] gap-x-4 gap-y-1 text-right">
          <dt className="text-muted-foreground">Invoice</dt>
          <dd className="font-mono" translate="no">{i.number ?? 'Draft (no number yet)'}</dd>
          <dt className="text-muted-foreground">Status</dt>
          <dd>{draft ? 'Draft' : i.status === 'void' ? 'Void' : i.status === 'paid' ? 'Paid' : 'Issued'}</dd>
          {i.issued_at && (
            <>
              <dt className="text-muted-foreground">Issued</dt>
              <dd>{formatIssued(i.issued_at)}</dd>
            </>
          )}
        </dl>
      </header>

      <section className="mt-6">
        <h3 className="text-muted-foreground">Bill to</h3>
        <p className="break-words font-medium">
          {i.tenant_name}
          {i.purged && <span className="ml-2 text-muted-foreground">(workspace deleted)</span>}
        </p>
        <p className="text-muted-foreground">Plan {i.plan_name}</p>
      </section>

      {i.status === 'void' && (
        <p className="mt-4 font-medium" role="note">
          This invoice is void{i.void_reason ? `: ${i.void_reason}` : '.'} It doesn’t need to be paid.
        </p>
      )}

      <table className="mt-6 w-full border-collapse">
        <thead>
          <tr className="border-b border-border text-left text-muted-foreground">
            <th className="py-2 font-normal">Item</th>
            <th className="py-2 text-right font-normal">Amount</th>
          </tr>
        </thead>
        <tbody>
          {(i.lines ?? []).map((l) => {
            const meter = l.meter && (BILLING_METERS as readonly string[]).includes(l.meter) ? l.meter : null
            return (
              <tr key={l.position} className="border-b border-border align-top">
                <td className="py-2">
                  {l.kind === 'base' ? 'Monthly fee' : meter ? METER_LABELS[meter] : l.meter}
                  {meter && l.quantity !== null && l.included !== null && l.unit && l.unit_price_minor !== null && (
                    <span className="block text-xs text-muted-foreground tabular-nums">
                      {formatQuantity(meter, l.quantity)} used, {formatQuantity(meter, l.included)} included; {l.billable_units} ×{' '}
                      {formatMoney(l.unit_price_minor, i.currency)} per {unitName(meter, l.unit)}
                    </span>
                  )}
                </td>
                <td className="py-2 text-right tabular-nums whitespace-nowrap">{formatMoney(l.amount_minor, i.currency)}</td>
              </tr>
            )
          })}
        </tbody>
        <tfoot className="tabular-nums">
          <tr>
            <td className="pt-3">Subtotal</td>
            <td className="pt-3 text-right whitespace-nowrap">{formatMoney(i.subtotal_minor, i.currency)}</td>
          </tr>
          {i.tax_rate_bp > 0 ? (
            <tr>
              <td className="pt-1">
                {i.tax_label || 'Tax'} {formatRate(i.tax_rate_bp)}
              </td>
              <td className="pt-1 text-right whitespace-nowrap">{formatMoney(i.tax_minor, i.currency)}</td>
            </tr>
          ) : (
            <tr>
              <td colSpan={2} className="pt-1 text-xs text-muted-foreground">
                Tax not included.
              </td>
            </tr>
          )}
          <tr className="font-semibold">
            <td className="border-t border-foreground pt-2">Total</td>
            <td className="border-t border-foreground pt-2 text-right whitespace-nowrap">{formatMoney(i.total_minor, i.currency)}</td>
          </tr>
        </tfoot>
      </table>

      {i.incomplete_hours > 0 && (
        <p className="mt-4 text-xs text-muted-foreground">
          Usage for {i.incomplete_hours} hour{i.incomplete_hours === 1 ? '' : 's'} of this month couldn’t be collected; usage is billed as
          counted.
        </p>
      )}
    </article>
  )
}
