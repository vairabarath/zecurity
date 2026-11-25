import { ApiError } from '@/api/client'
import type { InvoiceEmailDelivery, InvoiceStatus } from '@/api/types'

// Invoice display (Sprint 26 P4, D-66, D-68). Money is formatted by
// billing-money.ts from integer minor units.

export const INVOICE_STATUSES: readonly InvoiceStatus[] = ['draft', 'issued', 'paid', 'void']

export const INVOICE_STATUS_LABELS: Record<InvoiceStatus, string> = {
  draft: 'Draft',
  issued: 'Issued',
  paid: 'Paid',
  void: 'Void',
}

export const INVOICE_STATUS_VARIANTS: Record<InvoiceStatus, 'secondary' | 'info' | 'success' | 'destructive'> = {
  draft: 'secondary',
  issued: 'info',
  paid: 'success',
  void: 'destructive',
}

export const EMAIL_STATUS_LABELS: Record<InvoiceEmailDelivery['status'], string> = {
  pending: 'Queued',
  sending: 'Sending',
  sent: 'Sent',
  failed: 'Failed',
  not_delivered: 'Not delivered',
}

export const EMAIL_KIND_LABELS: Record<InvoiceEmailDelivery['kind'], string> = {
  issued: 'Invoice',
  voided: 'Void notice',
  resend: 'Resent',
}

/** "November 2026" from the invoice month (YYYY-MM-DD, first of the month, UTC). */
export function invoiceMonthLabel(month: string): string {
  const d = new Date(`${month.slice(0, 7)}-01T00:00:00Z`)
  return Number.isNaN(d.getTime()) ? month : d.toLocaleDateString(undefined, { month: 'long', year: 'numeric', timeZone: 'UTC' })
}

/** "Draft" until issued, then the number (INV-2026-000123). */
export function invoiceTitle(i: { number: string | null }): string {
  return i.number ?? 'Draft'
}

/** Today's date as YYYY-MM-DD in local time (the paid date default). */
export function todayISODate(now = new Date()): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${now.getFullYear()}-${p(now.getMonth() + 1)}-${p(now.getDate())}`
}

const INVOICE_ERRORS: Record<string, string> = {
  invoice_not_draft: 'This invoice is no longer a draft. Reload to see its status.',
  invoice_not_issued: 'Only an issued invoice can be marked paid or voided. Reload to see its status.',
  month_open: 'The month hasn’t closed yet (plus the 6-hour settling delay). Try again after that.',
  reason_required: 'Enter a reason.',
  reason_too_long: 'Keep the reason to 500 characters or fewer.',
  reference_too_long: 'Keep the reference to 200 characters or fewer.',
  invalid_paid_at: 'Enter the date the payment arrived (not in the future).',
  invoice_not_found: 'That invoice no longer exists. Reload and try again.',
  not_found: 'That invoice no longer exists. Reload and try again.',
  forbidden: 'Only super-admins can manage invoices.',
}

/** A readable message for a failed invoice write. */
export function invoiceErrorMessage(err: unknown): string {
  if (err instanceof ApiError) return INVOICE_ERRORS[err.code] ?? 'Something went wrong. Try again.'
  return 'Couldn’t reach the controller. Try again.'
}
