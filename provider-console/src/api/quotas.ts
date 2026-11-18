import { request } from './client'
import type { PlanLimitKey } from './types'

// Per-tenant quota overrides (Sprint 23 S3, D-47; D-30 as amended).
// Super-admin only; the controller decides (decide()). The reason goes in
// the body, never the URL, and is audited with before/after. Lowering a
// limit never deletes anything.

function quotaPath(tenantId: string, key: PlanLimitKey | string): string {
  return `/provider/tenants/${encodeURIComponent(tenantId)}/quotas/${encodeURIComponent(key)}`
}

/** Sets the tenant's own limit for `key`; null is explicitly unlimited. */
export function setTenantQuota(tenantId: string, key: PlanLimitKey | string, limit: number | null, reason: string): Promise<void> {
  return request<void>('PUT', quotaPath(tenantId, key), { body: { limit, reason } })
}

/** Removes the tenant's override for `key`: the plan's value applies again. */
export function inheritTenantQuota(tenantId: string, key: PlanLimitKey | string, reason: string): Promise<void> {
  return request<void>('PUT', quotaPath(tenantId, key), { body: { inherit: true, reason } })
}
