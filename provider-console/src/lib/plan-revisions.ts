import { BILLING_METERS, PLAN_LIMIT_KEYS, type PlanRevision } from '@/api/types'
import { formatIncluded, formatMoney, METER_LABELS, unitName } from '@/lib/billing-money'
import { formatLimit, PLAN_LIMIT_LABELS } from '@/lib/plan-format'

type Card = PlanRevision['card']

function meterTerms(card: Card, m: (typeof BILLING_METERS)[number]): string | null {
  const p = card.price?.meters[m]
  if (!card.price || !p) return null
  return `${formatIncluded(m, p.included, p.unit)} included, then ${formatMoney(p.unit_price_minor, card.price.currency)} per ${unitName(m, p.unit)}`
}

/**
 * What one revision changed against the one before it, as short sentences
 * for the History dialog (D-63: the history is the whole card after each
 * change; the console shows the difference). The first revision is "Created".
 */
export function describeRevision(card: Card, prev: Card | undefined): string[] {
  if (!prev) return ['Created']
  const out: string[] = []
  if (card.name !== prev.name) out.push(`Name: ${prev.name} → ${card.name}`)
  if (card.description !== prev.description) out.push('Description changed')
  for (const k of PLAN_LIMIT_KEYS) {
    if (card.limits[k] !== prev.limits[k]) out.push(`${PLAN_LIMIT_LABELS[k]}: ${formatLimit(prev.limits[k])} → ${formatLimit(card.limits[k])}`)
  }
  if (!prev.price && card.price) out.push(`Price added: ${formatMoney(card.price.base_fee_minor, card.price.currency)} per month`)
  else if (prev.price && !card.price) out.push('Price removed: not billable')
  else if (prev.price && card.price) {
    if (prev.price.currency !== card.price.currency) out.push(`Currency: ${prev.price.currency} → ${card.price.currency} (amounts not converted)`)
    else if (prev.price.base_fee_minor !== card.price.base_fee_minor) {
      out.push(`Monthly fee: ${formatMoney(prev.price.base_fee_minor, prev.price.currency)} → ${formatMoney(card.price.base_fee_minor, card.price.currency)}`)
    }
  }
  if (prev.price && card.price) {
    for (const m of BILLING_METERS) {
      const a = meterTerms(prev, m)
      const b = meterTerms(card, m)
      if (a === b || prev.price.currency !== card.price.currency) continue
      if (!a) out.push(`${METER_LABELS[m]} billed: ${b}`)
      else if (!b) out.push(`${METER_LABELS[m]} no longer billed`)
      else out.push(`${METER_LABELS[m]}: ${a} → ${b}`)
    }
  }
  return out.length ? out : ['No visible change']
}
