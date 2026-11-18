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
  forbidden: 'Only super-admins can create tenants.',
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
