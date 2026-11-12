import { request } from './client'
import type { CreateRelayPoolResponse, PoolMigration } from './types'

// Relay-pool mutations (D-37 … D-41; D-30 as amended by D-41). The
// controller decides who may call them (decide()):
//   create, rename, delete an empty pool, move a relay: super-admin and relay-ops
//   a pool's region or residency, a tenant's pool: super-admin only
// Reasons go in the body, never the URL (D-17 style).

function poolPath(id: string): string {
  return `/provider/relay-pools/${encodeURIComponent(id)}`
}

/**
 * Region and residency are sent only when set: relay-ops may create a pool
 * without them, and the controller refuses either one from relay-ops.
 */
export function createRelayPool(name: string, region?: string | null, residency?: boolean): Promise<CreateRelayPoolResponse> {
  return request<CreateRelayPoolResponse>('POST', '/provider/relay-pools', {
    body: { name, ...(region ? { region } : {}), ...(residency === true ? { residency: true } : {}) },
  })
}

/** Sends only the name, so the controller treats it as a rename (both roles). */
export function renameRelayPool(id: string, name: string): Promise<void> {
  return request<void>('PATCH', poolPath(id), { body: { name } })
}

/** Region (null clears it) and residency: super-admin only, and only while no tenant uses the pool. */
export function setRelayPoolPlacement(id: string, region: string | null, residency: boolean): Promise<void> {
  return request<void>('PATCH', poolPath(id), { body: { region, residency } })
}

/** Only an empty pool (no relays, no tenants); never the default pool. */
export function deleteRelayPool(id: string): Promise<void> {
  return request<void>('DELETE', poolPath(id))
}

/** Moves a relay to another pool; the reason is required and audited. */
export function moveRelayToPool(relayId: string, poolId: string, reason: string): Promise<void> {
  return request<void>('POST', `/provider/relays/${encodeURIComponent(relayId)}/pool`, { body: { pool_id: poolId, reason } })
}

/**
 * A tenant's relay pools (super-admin only). The body is the whole
 * assignment (D-39): `failoverPoolId` null removes the failover pool, so it
 * is always sent. `graceful` keeps the old primary authorized for the
 * migration window so connectors move make-before-break; `immediate` drops it
 * at once (it matters only when the primary changes).
 */
export function assignTenantRelayPool(
  tenantId: string,
  poolId: string,
  failoverPoolId: string | null,
  reason: string,
  migration: PoolMigration,
): Promise<void> {
  return request<void>('PUT', `/provider/tenants/${encodeURIComponent(tenantId)}/relay-pool`, {
    body: { pool_id: poolId, failover_pool_id: failoverPoolId ?? null, reason, migration },
  })
}
