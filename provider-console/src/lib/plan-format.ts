import { ApiError } from '@/api/client'
import { PLAN_LIMIT_KEYS, type PlanLimitKey, type PlanLimits } from '@/api/types'

// Plan display helpers (Sprint 23 B1, D-43). Limits are shown as the API
// sends them: a number, or "Unlimited" for an explicit null.

export const PLAN_LIMIT_LABELS: Record<PlanLimitKey, string> = {
  users: 'Users',
  resources: 'Resources',
  remote_networks: 'Remote networks',
  connectors: 'Connectors',
  shields: 'Shields',
  client_devices: 'Client devices',
}

export function formatLimit(v: number | null | undefined): string {
  return v === null || v === undefined ? 'Unlimited' : v.toLocaleString()
}

/** The form state of one limit: a number string, or unlimited. */
export interface LimitInput {
  unlimited: boolean
  value: string
}

export type LimitInputs = Record<PlanLimitKey, LimitInput>

export function limitInputs(limits?: PlanLimits): LimitInputs {
  const out = {} as LimitInputs
  for (const k of PLAN_LIMIT_KEYS) {
    const v = limits ? limits[k] : null
    out[k] = v === null ? { unlimited: true, value: '' } : { unlimited: false, value: String(v) }
  }
  return out
}

/** Parses the form into limits; null when any finite value isn't a whole number ≥ 0. */
export function parseLimitInputs(inputs: LimitInputs): PlanLimits | null {
  const out = {} as PlanLimits
  for (const k of PLAN_LIMIT_KEYS) {
    const i = inputs[k]
    if (i.unlimited) {
      out[k] = null
      continue
    }
    const t = i.value.trim()
    if (!/^\d{1,9}$/.test(t)) return null
    out[k] = Number(t)
  }
  return out
}

export function sameLimits(a: PlanLimits, b: PlanLimits): boolean {
  return PLAN_LIMIT_KEYS.every((k) => a[k] === b[k])
}

const PLAN_ERRORS: Record<string, string> = {
  plan_name_taken: 'A plan with this name already exists.',
  plan_in_use: 'Tenants are on this plan. Move them to another plan first.',
  plan_is_default: 'This is the default plan. Make another plan the default first.',
  invalid_name: 'Enter a name of 1 to 64 characters.',
  invalid_description: 'Keep the description to 500 characters.',
  invalid_limits: 'Every limit must be a whole number of 0 or more, or unlimited.',
  plan_not_found: 'That plan no longer exists. Reload and try again.',
  reason_required: 'Enter a reason.',
  reason_too_long: 'Keep the reason to 500 characters or fewer.',
  invalid_price: 'Check the price: whole amounts of 0 or more, a unit for every billed meter.',
  currency_mismatch: 'The billing currency changed. Reload the page and enter the price again.',
  price_overflow: 'An amount is too large.',
  invalid_settings: 'Check the settings and try again.',
  invalid_currency: 'Choose a currency from the list.',
  invalid_country: 'Choose a country from the list, or a currency for it.',
  forbidden: 'Only super-admins can change plans.',
  not_found: 'This plan or tenant no longer exists.',
}

/** A readable message for a failed plan write. */
export function planErrorMessage(err: unknown): string {
  if (err instanceof ApiError) return PLAN_ERRORS[err.code] ?? 'Something went wrong. Try again.'
  return 'Couldn’t reach the controller. Try again.'
}
