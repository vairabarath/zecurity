import { request } from './client'

// Tenant lifecycle writes (Sprint 23 S1, D-45; D-30 amended). Super-admin
// only; the controller decides (decide()). The reason (and, for suspend, the
// tenant's slug typed back, D-17 extended) go in the body, never the URL; the
// server checks both again and audits the change with before/after.

function tenantPath(id: string): string {
  return `/provider/tenants/${encodeURIComponent(id)}`
}

/**
 * Suspends a tenant: its users can't sign in, its sessions end, its agents
 * stay connected with a deny-all policy (D-08).
 */
export function suspendTenant(id: string, reason: string, confirmSlug: string): Promise<void> {
  return request<void>('POST', `${tenantPath(id)}/suspend`, { body: { reason, confirm_slug: confirmSlug } })
}

/** Lifts a suspension; agents get the normal policy back with no re-enrollment. */
export function resumeTenant(id: string, reason: string): Promise<void> {
  return request<void>('POST', `${tenantPath(id)}/resume`, { body: { reason } })
}
