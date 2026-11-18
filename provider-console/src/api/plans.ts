import { request } from './client'
import type { CreatePlanResponse, PlanLimits } from './types'

// Plan mutations (Sprint 23 B1, D-43; D-30 as amended). All are super-admin
// only; the controller decides (decide()). Writes that change what tenants
// may create (update, delete, set default, assign) carry a reason in the
// body, never the URL (D-17 style); it is audited.

function planPath(id: string): string {
  return `/provider/plans/${encodeURIComponent(id)}`
}

/** A new plan is never the default. `limits` must define all six keys (null = unlimited). */
export function createPlan(name: string, description: string, limits: PlanLimits): Promise<CreatePlanResponse> {
  return request<CreatePlanResponse>('POST', '/provider/plans', { body: { name, description, limits } })
}

/** Changed limits apply at once to every tenant on the plan; nothing is ever deleted. */
export function updatePlan(id: string, change: { name?: string; description?: string; limits?: PlanLimits }, reason: string): Promise<void> {
  return request<void>('PATCH', planPath(id), { body: { ...change, reason } })
}

/** Refused while the plan is the default or assigned to any tenant. */
export function deletePlan(id: string, reason: string): Promise<void> {
  return request<void>('DELETE', planPath(id), { body: { reason } })
}

/** New workspaces get the default from then on; existing tenants keep their plan. */
export function setDefaultPlan(id: string, reason: string): Promise<void> {
  return request<void>('POST', `${planPath(id)}/default`, { body: { reason } })
}

/** A tenant's plan; the new limits apply at once. */
export function assignTenantPlan(tenantId: string, planId: string, reason: string): Promise<void> {
  return request<void>('PUT', `/provider/tenants/${encodeURIComponent(tenantId)}/plan`, { body: { plan_id: planId, reason } })
}
