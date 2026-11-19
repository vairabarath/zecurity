import { ApiError } from '@/api/client'

// Tenant creation helpers (Sprint 23 S2, D-44).

/** One DNS label that fits "ws-<slug>": the controller checks the same rule. */
export const SLUG_PATTERN = /^[a-z0-9](?:[a-z0-9-]{0,58}[a-z0-9])?$/
export const EMAIL_PATTERN = /^[^\s@<>]+@[^\s@<>]+\.[^\s@<>]+$/

const TENANT_ERRORS: Record<string, string> = {
  slug_taken: 'A tenant with this slug already exists. Choose another slug.',
  invalid_slug: 'Use 1 to 60 lowercase letters, digits or dashes, starting and ending with a letter or digit.',
  invalid_name: 'Enter a name of 1 to 100 characters.',
  invalid_email: 'Enter a valid email address for the first admin.',
  plan_not_found: 'That plan no longer exists. Choose another plan.',
  reason_required: 'Enter a reason.',
  reason_too_long: 'Keep the reason to 500 characters or fewer.',
  forbidden: 'Only super-admins can create tenants or send invitations.',
  first_admin_exists: 'This tenant already has an admin. Its admins invite people themselves.',
  email_has_account: 'That email already has an account in this tenant. Use another address.',
  tenant_not_active: 'This tenant isn’t active.',
  not_found: 'This tenant no longer exists.',
}

export function tenantCreateErrorMessage(err: unknown): string {
  if (err instanceof ApiError) return TENANT_ERRORS[err.code] ?? 'Something went wrong. Try again.'
  return 'Couldn’t reach the controller. Try again.'
}

/** A slug suggestion from the name: lowercase, dashes for runs of other characters. */
export function slugFromName(name: string): string {
  return name
    .toLowerCase()
    .replace(/[^a-z0-9]+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 60)
    .replace(/-+$/, '')
}

/** "in 29 days", "in 5 hours", "in 3 minutes", or null once due. */
export function timeUntil(iso: string, now = Date.now()): string | null {
  const ms = new Date(iso).getTime() - now
  if (ms <= 0) return null
  const plural = (n: number, unit: string) => `in ${n} ${unit}${n === 1 ? '' : 's'}`
  if (ms < 3_600_000) return plural(Math.max(1, Math.ceil(ms / 60_000)), 'minute')
  if (ms < 86_400_000) return plural(Math.ceil(ms / 3_600_000), 'hour')
  return plural(Math.ceil(ms / 86_400_000), 'day')
}
