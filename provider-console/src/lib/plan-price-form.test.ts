import { describe, expect, it } from 'vitest'
import type { PlanPrice } from '@/api/types'
import { parsePriceInputs, priceInputs, pricePayload, samePrice } from '@/lib/plan-price-form'

const TEAM: PlanPrice = {
  currency: 'INR',
  base_fee_minor: 100000,
  meters: {
    relay_bytes_to_clients: { included: 50_000_000_000, unit: 1_000_000_000, unit_price_minor: 800 },
    connector_minutes: { included: 6000, unit: 60, unit_price_minor: 250 },
  },
}

describe('plan price form', () => {
  it('round-trips a price through the form unchanged', () => {
    const inputs = priceInputs(TEAM)
    expect(inputs.baseFee).toBe('1000.00')
    expect(inputs.meters.relay_bytes_to_clients).toEqual({ billed: true, included: '50', unitSize: 1e9, unitPrice: '8.00' })
    expect(inputs.meters.connector_minutes).toEqual({ billed: true, included: '100', unitSize: 60, unitPrice: '2.50' })
    expect(inputs.meters.active_users.billed).toBe(false)
    const parsed = parsePriceInputs(inputs, 'INR')
    expect(parsed).toEqual({ ok: true, price: TEAM })
    expect(pricePayload(TEAM, parsed.ok ? parsed.price : null)).toBeUndefined() // unchanged → left out
  })

  it('a new card starts not billable; turning billing on needs a base fee', () => {
    const inputs = priceInputs(null)
    expect(inputs.billable).toBe(false)
    expect(parsePriceInputs(inputs, 'USD')).toEqual({ ok: true, price: null })
    const on = { ...inputs, billable: true }
    const r = parsePriceInputs(on, 'USD')
    expect(r.ok).toBe(false)
    expect(!r.ok && r.errors.baseFee).toMatch(/Enter an amount in USD/)
    expect(parsePriceInputs({ ...on, baseFee: '19.99' }, 'USD')).toEqual({
      ok: true,
      price: { currency: 'USD', base_fee_minor: 1999, meters: {} },
    })
  })

  it('an included quantity and a unit price are converted exactly', () => {
    const inputs = priceInputs(null)
    inputs.billable = true
    inputs.baseFee = '0'
    inputs.meters.relay_bytes_to_connectors = { billed: true, included: '1,000', unitSize: 1e12, unitPrice: '0.29' }
    inputs.meters.active_users = { billed: true, included: '5', unitSize: 1, unitPrice: '3' }
    const r = parsePriceInputs(inputs, 'USD')
    expect(r).toEqual({
      ok: true,
      price: {
        currency: 'USD',
        base_fee_minor: 0,
        meters: {
          relay_bytes_to_connectors: { included: 1000e12, unit: 1e12, unit_price_minor: 29 },
          active_users: { included: 5, unit: 1, unit_price_minor: 300 },
        },
      },
    })
  })

  it('reports each bad field', () => {
    const inputs = priceInputs(TEAM)
    inputs.meters.relay_bytes_to_clients.included = '1.5'
    inputs.meters.connector_minutes.unitPrice = '2.555'
    const r = parsePriceInputs(inputs, 'INR')
    expect(r.ok).toBe(false)
    expect(!r.ok && Object.keys(r.errors).sort()).toEqual(['connector_minutes.unitPrice', 'relay_bytes_to_clients.included'])
  })

  it('payload: an edit sends the new price, and turning billing off sends null', () => {
    const cheaper = { ...TEAM, base_fee_minor: 90000 }
    expect(pricePayload(TEAM, cheaper)).toEqual(cheaper)
    expect(pricePayload(TEAM, null)).toBeNull()
    expect(pricePayload(null, null)).toBeUndefined()
    expect(pricePayload(null, cheaper)).toEqual(cheaper)
  })

  it('samePrice compares every term', () => {
    const moreIncluded: PlanPrice = { ...TEAM, meters: { ...TEAM.meters, connector_minutes: { included: 6060, unit: 60, unit_price_minor: 250 } } }
    expect(samePrice(TEAM, moreIncluded)).toBe(false)
    expect(samePrice(TEAM, { ...TEAM, currency: 'USD' })).toBe(false)
    expect(samePrice(TEAM, { ...TEAM, meters: { relay_bytes_to_clients: TEAM.meters.relay_bytes_to_clients } })).toBe(false)
    expect(samePrice(TEAM, structuredClone(TEAM))).toBe(true)
  })
})
