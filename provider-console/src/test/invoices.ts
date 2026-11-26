import type { Invoice } from '@/api/types'

// Invoice fixtures (Sprint 26 P4). The same invoice as the controller's
// email test and the 2026-11-23 live check: INR 1,688.00 + GST 18 % = 1,991.84.

export function invoice(over: Partial<Invoice> = {}): Invoice {
  return {
    id: 'inv-1',
    number: 'INV-2026-000001',
    tenant_id: 't-acme',
    tenant_name: 'Acme',
    tenant_slug: 'acme',
    purged: false,
    month: '2026-11-01',
    status: 'issued',
    currency: 'INR',
    plan_name: 'Team',
    plan_revision: 3,
    subtotal_minor: 168800,
    tax_rate_bp: 1800,
    tax_label: 'GST',
    tax_minor: 30384,
    total_minor: 199184,
    incomplete_hours: 0,
    generated_at: '2026-12-01T06:00:00Z',
    issued_at: '2026-12-01T09:00:00Z',
    paid_at: null,
    paid_reference: null,
    voided_at: null,
    void_reason: null,
    lines: [
      { position: 1, kind: 'base', meter: null, quantity: null, included: null, unit: null, billable_units: null, unit_price_minor: null, amount_minor: 100000 },
      { position: 2, kind: 'meter', meter: 'relay_bytes_to_clients', quantity: 60_500_000_000, included: 50_000_000_000, unit: 1_000_000_000, billable_units: 11, unit_price_minor: 800, amount_minor: 8800 },
      { position: 3, kind: 'meter', meter: 'active_users', quantity: 7, included: 5, unit: 1, billable_units: 2, unit_price_minor: 30000, amount_minor: 60000 },
    ],
    ...over,
  }
}
