import { ApiError } from '@/api/client'
import type { ApiKeyRole } from '@/api/apiKeys'

// Display helpers for provider API keys (Sprint 27 K2, D-76).

const DAY = 24 * 60 * 60 * 1000

/** Under this many days a key shows the "expires soon" warning (the expiry alert fires at 14, D-76). */
export const EXPIRES_SOON_DAYS = 14

/** What each key role can read (keys never write, D-76). */
export const API_KEY_ROLE_HELP: Record<ApiKeyRole, string> = {
  'relay-ops': 'Relays, relay pools, relay metrics and relay alerts. No tenant data.',
  auditor: 'Everything the console can read, including the provider audit log.',
  billing: 'Plan cards, billing settings, invoices, usage and the tenant list.',
  support: 'Tenants and their agents, the tenants’ alerts and timeline, and health.',
}

/** Expiry presets in days. 365 is the longest a key may live. */
export const EXPIRY_PRESETS = [30, 90, 365] as const

/**
 * The expiry for a preset. The server refuses anything at or beyond
 * created_at + 365 days (less a minute of slack), so the longest preset ends
 * a few minutes short of a full year.
 */
export function presetExpiry(days: number, now: Date = new Date()): Date {
  const t = now.getTime() + days * DAY
  return new Date(days >= 365 ? t - 10 * 60 * 1000 : t)
}

/** The expiry for a picked calendar date (YYYY-MM-DD): the end of that UTC day. */
export function dateExpiry(date: string): Date | null {
  if (!/^\d{4}-\d{2}-\d{2}$/.test(date)) return null
  const d = new Date(`${date}T23:59:00Z`)
  return Number.isNaN(d.getTime()) ? null : d
}

/** Whether an expiry is in the future and within the 365-day limit. */
export function expiryValid(exp: Date | null, now: Date = new Date()): boolean {
  if (!exp) return false
  const ms = exp.getTime() - now.getTime()
  return ms > 0 && ms < 365 * DAY - 2 * 60 * 1000
}

/** Whole days until an expiry (negative once past). */
export function daysUntil(value: string, now: Date = new Date()): number {
  return Math.floor((Date.parse(value) - now.getTime()) / DAY)
}

/** "expires in N days" / "expires today" / "expired". */
export function expiryText(value: string, now: Date = new Date()): string {
  const ms = Date.parse(value) - now.getTime()
  if (Number.isNaN(ms)) return '—'
  if (ms <= 0) return 'expired'
  const d = Math.floor(ms / DAY)
  if (d === 0) return 'expires today'
  return d === 1 ? 'expires in 1 day' : `expires in ${d} days`
}

/** Active and under EXPIRES_SOON_DAYS days left. */
export function expiresSoon(status: string, value: string, now: Date = new Date()): boolean {
  const ms = Date.parse(value) - now.getTime()
  return status === 'active' && ms > 0 && ms < EXPIRES_SOON_DAYS * DAY
}

/** Splits the CIDR field (commas, spaces or new lines) into entries. */
export function parseCidrs(text: string): string[] {
  return text
    .split(/[\s,]+/)
    .map((s) => s.trim())
    .filter((s) => s !== '')
}

/** A light shape check; the server validates for real. */
export function cidrLooksValid(s: string): boolean {
  return /^[0-9a-fA-F:.]+\/\d{1,3}$/.test(s)
}

const ERRORS: Record<string, string> = {
  invalid_name: 'Enter a name of 1 to 64 characters.',
  invalid_role: 'Choose one of the four roles. A key can never be a super-admin.',
  invalid_expires_at: 'Pick an expiry in the future and at most 365 days away.',
  invalid_cidr: 'One of the CIDRs isn’t valid (for example 203.0.113.0/24).',
  reason_required: 'Enter a reason.',
  reason_too_long: 'Keep the reason to 500 characters or fewer.',
  name_taken: 'A key with this name already exists. Pick another name or revoke the old key.',
  confirmation_mismatch: 'Type the key’s name exactly to confirm.',
  already_revoked: 'This key is already revoked.',
  not_found: 'This key no longer exists.',
  forbidden: 'Only super-admins can manage API keys.',
}

export function apiKeyErrorMessage(err: unknown): string {
  if (err instanceof ApiError) return ERRORS[err.code] ?? 'Something went wrong. Try again.'
  return 'Couldn’t reach the controller. Try again.'
}
