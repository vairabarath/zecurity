import { request } from './client'
import type { CreateTenantResponse, ReinviteResponse } from './types'

// Tenant creation (Sprint 23 S2, D-44; D-30 as amended). Super-admin only; the
// controller decides (decide()). The reason goes in the body, never the URL,
// and is audited. The response carries the first-admin invitation's id and
// expiry, never its token: the invitation is emailed to the admin.

/** Create a workspace (same bootstrap as self-signup) and invite its first admin. `planId` null = the default plan. */
export function createTenant(slug: string, name: string, planId: string | null, adminEmail: string, reason: string): Promise<CreateTenantResponse> {
  return request<CreateTenantResponse>('POST', '/provider/tenants', {
    body: { slug, name, ...(planId ? { plan_id: planId } : {}), admin_email: adminEmail, reason },
  })
}

/**
 * A new single-use, 7-day first-admin invitation for a tenant whose first admin
 * never signed in; earlier links stop working. `adminEmail` "" re-sends to the
 * previous invitee.
 */
export function reinviteFirstAdmin(tenantId: string, adminEmail: string, reason: string): Promise<ReinviteResponse> {
  return request<ReinviteResponse>('POST', `/provider/tenants/${encodeURIComponent(tenantId)}/first-admin-invitation`, {
    body: { ...(adminEmail ? { admin_email: adminEmail } : {}), reason },
  })
}
