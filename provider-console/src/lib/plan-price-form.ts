import { BILLING_METERS, type BillingMeter, type MeterPrice, type PlanPrice } from '@/api/types'
import { moneyInput, parseMoney, parseUnits, unitOptions } from '@/lib/billing-money'

// The price part of the plan card form (Sprint 26, D-63). The form holds
// text; parsing happens here with string arithmetic (billing-money). The
// payload rule for an edit:
//   - unchanged price → the field is left out (undefined);
//   - billing turned off → null (the card becomes not billable);
//   - anything else → the whole new price.

export interface MeterInput {
  billed: boolean
  included: string // whole display units
  unitSize: number // the chosen unit, in the meter's base unit
  unitPrice: string // major units of the currency
}

export interface PriceInputs {
  billable: boolean
  baseFee: string
  meters: Record<BillingMeter, MeterInput>
}

export function priceInputs(price: PlanPrice | null): PriceInputs {
  const meters = {} as Record<BillingMeter, MeterInput>
  for (const m of BILLING_METERS) {
    const p = price?.meters[m]
    const unitSize = p && unitOptions(m).some((u) => u.size === p.unit) ? p.unit : unitOptions(m)[0].size
    meters[m] = p
      ? {
          billed: true,
          included: p.included % unitSize === 0 ? String(p.included / unitSize) : '',
          unitSize,
          unitPrice: moneyInput(p.unit_price_minor, price!.currency),
        }
      : { billed: false, included: '0', unitSize, unitPrice: '' }
  }
  return {
    billable: price !== null,
    baseFee: price ? moneyInput(price.base_fee_minor, price.currency) : '',
    meters,
  }
}

export type ParsedPrice = { ok: true; price: PlanPrice | null } | { ok: false; errors: PriceErrors }

/** Field-level problems, keyed "baseFee" or "<meter>.included" / "<meter>.unitPrice". */
export type PriceErrors = Record<string, string>

/** Parses the form; `price: null` when billing is off. */
export function parsePriceInputs(inputs: PriceInputs, currency: string): ParsedPrice {
  if (!inputs.billable) return { ok: true, price: null }
  const errors: PriceErrors = {}
  const base = parseMoney(inputs.baseFee, currency)
  if (base === null) errors.baseFee = `Enter an amount in ${currency}, like 20 or 19.99.`
  const meters: Partial<Record<BillingMeter, MeterPrice>> = {}
  for (const m of BILLING_METERS) {
    const i = inputs.meters[m]
    if (!i.billed) continue
    const included = parseUnits(i.included, i.unitSize)
    const unitPrice = parseMoney(i.unitPrice, currency)
    if (included === null) errors[`${m}.included`] = 'Enter a whole number (0 or more).'
    if (unitPrice === null) errors[`${m}.unitPrice`] = `Enter an amount in ${currency}.`
    if (included !== null && unitPrice !== null) meters[m] = { included, unit: i.unitSize, unit_price_minor: unitPrice }
  }
  if (Object.keys(errors).length > 0) return { ok: false, errors }
  return { ok: true, price: { currency, base_fee_minor: base!, meters } }
}

export function samePrice(a: PlanPrice | null, b: PlanPrice | null): boolean {
  if (a === null || b === null) return a === b
  if (a.currency !== b.currency || a.base_fee_minor !== b.base_fee_minor) return false
  return BILLING_METERS.every((m) => {
    const x = a.meters[m]
    const y = b.meters[m]
    if (!x || !y) return !x && !y
    return x.included === y.included && x.unit === y.unit && x.unit_price_minor === y.unit_price_minor
  })
}

/** The `price` field of an update: undefined = leave out, null = remove, else the new price. */
export function pricePayload(before: PlanPrice | null, after: PlanPrice | null): PlanPrice | null | undefined {
  return samePrice(before, after) ? undefined : after
}
