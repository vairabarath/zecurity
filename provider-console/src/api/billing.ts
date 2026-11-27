import { request } from './client'
import { buildQuery } from './reads'
import type {
  BillingSettings,
  BillingSettingsResponse,
  InvoiceDetail,
  InvoicePage,
  InvoiceStatus,
  NewPlanCard,
  PlanCardChange,
  PlanCardSummary,
  PlanRevision,
} from './types'

// Billing (Sprint 26 F34; D-62 … D-70). Super-admin only: the controller
// decides (billing.settings.write, invoice.read, invoice.write; cards use the
// plan actions) and relay-ops get 403 (D-70). Writes carry a reason in the
// body, never the URL; the controller audits them with before/after. Invoice
// reads are audited (invoice.read, invoice.fleet.read), so the console reads
// once per view and on an explicit change, never by polling.

const enc = encodeURIComponent

/** GET /provider/billing/settings (P1b). */
export function getBillingSettings(): Promise<BillingSettingsResponse> {
  return request<BillingSettingsResponse>('GET', '/provider/billing/settings')
}

/** PATCH /provider/billing/settings: only the changed fields. A currency change switches every card (D-64, no conversion). */
export function updateBillingSettings(change: Partial<BillingSettings>, reason: string): Promise<void> {
  return request<void>('PATCH', '/provider/billing/settings', { body: { ...change, reason } })
}

/** GET /provider/plans with each card's price and revision (P1b extends B1's list). */
export function listPlanCards(): Promise<{ items: PlanCardSummary[] }> {
  return request<{ items: PlanCardSummary[] }>('GET', '/provider/plans')
}

/** POST /provider/plans with an optional price (null = not billable). */
export function createPlanCard(card: NewPlanCard, reason: string): Promise<{ id: string }> {
  return request<{ id: string }>('POST', '/provider/plans', { body: { ...card, reason } })
}

/** PATCH /provider/plans/{id}: any fields in one call; `price: null` removes the price, an absent `price` keeps it. */
export function updatePlanCard(id: string, change: PlanCardChange, reason: string): Promise<void> {
  return request<void>('PATCH', `/provider/plans/${enc(id)}`, { body: { ...change, reason } })
}

/** GET /provider/plans/{id}/revisions: the card's append-only history, newest first. */
export function listPlanRevisions(id: string): Promise<{ items: PlanRevision[] }> {
  return request<{ items: PlanRevision[] }>('GET', `/provider/plans/${enc(id)}/revisions`)
}

export interface InvoiceListParams {
  /** YYYY-MM */
  month?: string
  status?: InvoiceStatus
  tenant_id?: string
  cursor?: string
  limit?: number
}

const INVOICE_LIST_KEYS = ['month', 'status', 'tenant_id', 'cursor', 'limit'] as const

/** GET /provider/invoices (P3): a keyset page plus the month's totals per status. */
export function listInvoices(params?: InvoiceListParams): Promise<InvoicePage> {
  return request<InvoicePage>('GET', '/provider/invoices' + buildQuery(params, INVOICE_LIST_KEYS))
}

/** GET /provider/invoices/{id}: lines, status history and email deliveries (counts, never addresses). */
export function getInvoice(id: string): Promise<InvoiceDetail> {
  return request<InvoiceDetail>('GET', `/provider/invoices/${enc(id)}`)
}

/** Draft → issued: gets its number; the tenant's admins are emailed when email is on (D-68). */
export function issueInvoice(id: string, reason: string): Promise<void> {
  return request<void>('POST', `/provider/invoices/${enc(id)}/issue`, { body: reason ? { reason } : {} })
}

/** Issued → paid. `paid_at` is YYYY-MM-DD; the reference is free text (≤ 200 characters). */
export function markInvoicePaid(id: string, paid: { paid_at: string; reference: string }, reason: string): Promise<void> {
  return request<void>('POST', `/provider/invoices/${enc(id)}/mark-paid`, { body: { ...paid, reason } })
}

/** Issued → void (reason required); the tenant's admins get a void notice when email is on. */
export function voidInvoice(id: string, reason: string): Promise<void> {
  return request<void>('POST', `/provider/invoices/${enc(id)}/void`, { body: { reason } })
}

/** Rebuild the month's drafts from the cards, settings and usage (issued invoices never change). */
export function regenerateInvoices(month: string, reason: string, tenantId?: string): Promise<{ regenerated: number }> {
  return request<{ regenerated: number }>('POST', '/provider/invoices/regenerate', {
    body: { month, reason, ...(tenantId ? { tenant_id: tenantId } : {}) },
  })
}

/** Send the issued, paid or void invoice's email again (a new delivery row). */
export function resendInvoice(id: string, reason: string): Promise<void> {
  return request<void>('POST', `/provider/invoices/${enc(id)}/resend`, { body: { reason } })
}
