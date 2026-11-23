import type { PlanCardSummary, PlanSummary, TenantPlan, TenantQuota } from '@/api/types'

// Plan fixtures shared by the page tests (Sprint 23 B1, D-43).

export const FREE_PLAN_ID = '00000000-0000-0000-0001-000000000001'
export const LEGACY_PLAN_ID = '00000000-0000-0000-0001-000000000002'
export const PRO_PLAN_ID = '00000000-0000-0000-0001-0000000000a1'

export const FREE_LIMITS = { users: 5, resources: 50, remote_networks: 3, connectors: 10, shields: 10, client_devices: 25 }
export const UNLIMITED = { users: null, resources: null, remote_networks: null, connectors: null, shields: null, client_devices: null }

export const TENANT_FREE_PLAN: TenantPlan = { id: FREE_PLAN_ID, name: 'Free', limits: FREE_LIMITS, is_default: true }

export function planSummary(over: Partial<PlanSummary> = {}): PlanSummary {
  return {
    id: FREE_PLAN_ID,
    name: 'Free',
    description: 'Default plan for new workspaces.',
    limits: FREE_LIMITS,
    is_default: true,
    tenants: 3,
    created_at: '2026-11-17T00:00:00Z',
    updated_at: '2026-11-17T00:00:00Z',
    ...over,
  }
}

export const PLANS: PlanSummary[] = [
  planSummary(),
  planSummary({ id: LEGACY_PLAN_ID, name: 'Legacy', description: 'Unlimited compatibility plan.', limits: UNLIMITED, is_default: false, tenants: 0 }),
  planSummary({ id: PRO_PLAN_ID, name: 'Pro', description: 'Paid', limits: { ...FREE_LIMITS, users: 50 }, is_default: false, tenants: 1 }),
]

/** Tenant detail quotas on the Free plan, nothing in use (Sprint 23 S3, D-47). */
export const FREE_QUOTAS: TenantQuota[] = (Object.keys(FREE_LIMITS) as (keyof typeof FREE_LIMITS)[]).map((key) => ({
  key,
  limit: FREE_LIMITS[key],
  source: 'plan' as const,
  used: 0,
  over: false,
}))

// ---- Sprint 26 plan cards ----

export function planCard(over: Partial<PlanCardSummary> = {}): PlanCardSummary {
  return {
    ...planSummary({ id: PRO_PLAN_ID, name: 'Team', description: 'For growing teams.', is_default: false, tenants: 4 }),
    price: {
      currency: 'INR',
      base_fee_minor: 100000,
      meters: {
        relay_bytes_to_clients: { included: 50_000_000_000, unit: 1_000_000_000, unit_price_minor: 800 },
        active_users: { included: 5, unit: 1, unit_price_minor: 30000 },
      },
    },
    revision: 3,
    ...over,
  }
}
