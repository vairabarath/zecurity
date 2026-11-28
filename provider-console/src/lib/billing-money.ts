import type { BillingMeter } from '@/api/types'

// Billing money and quantities (Sprint 26, D-64). Money is always whole minor
// units; nothing here uses floating-point arithmetic on amounts. Typed
// amounts are parsed as strings, and amounts are formatted by splitting
// digits. The rules mirror the controller's internal/billing/money.go.

/** ISO 4217 minor-unit digits that differ from 2. */
const EXPONENT: Record<string, number> = {
  BIF: 0, CLP: 0, DJF: 0, GNF: 0, ISK: 0, JPY: 0, KMF: 0, KRW: 0, PYG: 0, RWF: 0,
  UGX: 0, UYI: 0, VND: 0, VUV: 0, XAF: 0, XOF: 0, XPF: 0,
  BHD: 3, IQD: 3, JOD: 3, KWD: 3, LYD: 3, OMR: 3, TND: 3,
}

export function currencyExponent(currency: string): number {
  return EXPONENT[currency] ?? 2
}

/** Thousands separators in a run of digits. */
function group(digits: string): string {
  return digits.replace(/\B(?=(\d{3})+(?!\d))/g, ',')
}

/** Minor units as "USD 1,234.56" (the same text the invoice email shows). */
export function formatMoney(minor: number, currency: string): string {
  if (!Number.isSafeInteger(minor)) return `${currency} —`
  const neg = minor < 0
  const exp = currencyExponent(currency)
  const digits = String(Math.abs(minor)).padStart(exp + 1, '0')
  const whole = digits.slice(0, digits.length - exp)
  const frac = exp > 0 ? '.' + digits.slice(digits.length - exp) : ''
  return `${currency} ${neg ? '-' : ''}${group(whole)}${frac}`
}

/**
 * Parses an amount typed in the currency's major unit ("12.34", "1,000",
 * "0.5") into minor units, using string arithmetic. Returns null for anything
 * that isn't a non-negative amount with at most the currency's decimals, or
 * that exceeds the safe integer range.
 */
export function parseMoney(input: string, currency: string): number | null {
  const exp = currencyExponent(currency)
  const t = input.trim().replace(/,(?=\d{3}(\D|$))/g, '')
  const m = /^(\d+)(?:\.(\d*))?$/.exec(t)
  if (!m) return null
  const frac = m[2] ?? ''
  if (frac.length > exp) return null
  const digits = (m[1] + frac.padEnd(exp, '0')).replace(/^0+(?=\d)/, '')
  if (digits.length > 15) return null
  const n = Number(digits)
  return Number.isSafeInteger(n) ? n : null
}

/** Minor units back to the form's major-unit text ("12.34"; "1000" for JPY). */
export function moneyInput(minor: number, currency: string): string {
  const exp = currencyExponent(currency)
  if (exp === 0) return String(minor)
  const digits = String(Math.abs(minor)).padStart(exp + 1, '0')
  return `${digits.slice(0, -exp)}.${digits.slice(-exp)}`
}

/** Parses a tax percentage ("18", "7.25") into basis points (0–10000); null if invalid. */
export function parseTaxPercent(input: string): number | null {
  const m = /^(\d{1,3})(?:\.(\d{1,2}))?$/.exec(input.trim())
  if (!m) return null
  const bp = Number(m[1]) * 100 + Number((m[2] ?? '').padEnd(2, '0'))
  return bp <= 10000 ? bp : null
}

/** Basis points as a percentage: 1800 → "18%", 725 → "7.25%", 750 → "7.5%". */
export function formatRate(bp: number): string {
  const whole = Math.trunc(bp / 100)
  const rest = bp % 100
  return rest === 0 ? `${whole}%` : `${whole}.${String(rest).padStart(2, '0').replace(/0$/, '')}%`
}

/** Basis points back to the form's percentage text. */
export function taxPercentInput(bp: number): string {
  return formatRate(bp).replace('%', '')
}

// ---- meters ----

export const METER_LABELS: Record<BillingMeter, string> = {
  relay_bytes_to_clients: 'Relay traffic to clients',
  relay_bytes_to_connectors: 'Relay traffic to connectors',
  session_minutes: 'Client session time',
  connector_minutes: 'Connector time',
  shield_minutes: 'Shield time',
  active_users: 'Active users',
}

/** A unit a price can be quoted in: its name and its size in the meter's base unit. */
export interface UnitOption {
  name: string
  size: number
}

export function isByteMeter(m: BillingMeter): boolean {
  return m.startsWith('relay_bytes_')
}

function isMinuteMeter(m: BillingMeter): boolean {
  return m.endsWith('_minutes')
}

/** The units offered for a meter, the first being the default. */
export function unitOptions(m: BillingMeter): UnitOption[] {
  if (isByteMeter(m))
    return [
      { name: 'GB', size: 1_000_000_000 },
      { name: 'TB', size: 1_000_000_000_000 },
    ]
  if (isMinuteMeter(m))
    return [
      { name: 'hour', size: 60 },
      { name: 'minute', size: 1 },
    ]
  return [{ name: 'user', size: 1 }]
}

/** The name of a unit size for a meter ("GB", "hour", "user"; else "N <base>"). */
export function unitName(m: BillingMeter, size: number): string {
  const known = unitOptions(m).find((u) => u.size === size)
  if (known) return known.name
  if (isByteMeter(m)) return size === 1_000_000 ? 'MB' : `${group(String(size))} bytes`
  if (isMinuteMeter(m)) return `${group(String(size))} min`
  return group(String(size))
}

/** A meter quantity for people: bytes as GB (two decimals, rounded down), minutes, users. */
export function formatQuantity(m: BillingMeter, q: number): string {
  if (isByteMeter(m)) {
    const gb = 1_000_000_000
    const whole = Math.floor(q / gb)
    const hundredths = Math.floor((q % gb) / 10_000_000)
    return `${group(String(whole))}.${String(hundredths).padStart(2, '0')} GB`
  }
  if (isMinuteMeter(m)) return `${group(String(q))} min`
  return q === 1 ? '1 user' : `${group(String(q))} users`
}

/** An included quantity in the meter's unit ("50 GB", "100 hours", "5 users"). */
export function formatIncluded(m: BillingMeter, included: number, unit: number): string {
  if (included % unit === 0) {
    const n = included / unit
    const name = unitName(m, unit)
    const plural = n === 1 || name === 'GB' || name === 'TB' || name === 'MB' ? name : `${name}s`
    return `${group(String(n))} ${plural}`
  }
  return formatQuantity(m, included)
}

/** Parses a whole number of display units into base units ("50" GB → 50e9); null if invalid. */
export function parseUnits(input: string, size: number): number | null {
  const t = input.trim().replace(/,(?=\d{3}(\D|$))/g, '')
  if (!/^\d{1,12}$/.test(t)) return null
  const n = Number(t) * size
  return Number.isSafeInteger(n) ? n : null
}
