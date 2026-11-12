import type { RelayPoolRef, RelayPoolSummary, TenantRelayPool } from '@/api/types'

// Relay-pool fixtures shared by the page tests (D-37).

export const DEFAULT_POOL_ID = '00000000-0000-0000-0000-00000000d001'
export const EU_POOL_ID = 'eeeeeeee-0000-0000-0000-0000000000e1'
export const EMPTY_POOL_ID = 'eeeeeeee-0000-0000-0000-0000000000e2'

export const DEFAULT_POOL: RelayPoolRef = { id: DEFAULT_POOL_ID, name: 'default', region: null, residency: false }
export const EU_POOL: RelayPoolRef = { id: EU_POOL_ID, name: 'eu-1', region: 'eu-central-1', residency: true }

export function poolSummary(over: Partial<RelayPoolSummary> = {}): RelayPoolSummary {
  return {
    ...DEFAULT_POOL,
    is_default: true,
    relays: 3,
    eligible_relays: 2,
    tenants: 4,
    failover_tenants: 0,
    state: 'healthy',
    exhausted_at: null,
    eligible_since: null,
    created_at: '2026-10-01T00:00:00Z',
    ...over,
  }
}

export const POOLS: RelayPoolSummary[] = [
  poolSummary(),
  poolSummary({ ...EU_POOL, is_default: false, relays: 2, eligible_relays: 1, tenants: 1 }),
  poolSummary({ id: EMPTY_POOL_ID, name: 'spare', region: null, residency: false, is_default: false, relays: 0, eligible_relays: 0, tenants: 0 }),
]

export const TENANT_DEFAULT_POOL: TenantRelayPool = {
  ...DEFAULT_POOL,
  eligible_relays: 2,
  previous: null,
  failover: null,
  using_failover: false,
  failed_over_since: null,
}

// D-39 failover candidates: eu-2 is a residency pool in eu-1's region; eu-plain
// shares the region without residency; backup has no region (matches default).
export const EU2_POOL_ID = 'eeeeeeee-0000-0000-0000-0000000000e3'
export const EU_PLAIN_POOL_ID = 'eeeeeeee-0000-0000-0000-0000000000e4'
export const BACKUP_POOL_ID = 'eeeeeeee-0000-0000-0000-0000000000e5'
export const FAILOVER_POOLS: RelayPoolSummary[] = [
  ...POOLS,
  poolSummary({ id: EU2_POOL_ID, name: 'eu-2', region: 'eu-central-1', residency: true, is_default: false, relays: 1, eligible_relays: 1, tenants: 0 }),
  poolSummary({ id: EU_PLAIN_POOL_ID, name: 'eu-plain', region: 'eu-central-1', residency: false, is_default: false, relays: 1, eligible_relays: 1, tenants: 0 }),
  poolSummary({ id: BACKUP_POOL_ID, name: 'backup', region: null, residency: false, is_default: false, relays: 2, eligible_relays: 2, tenants: 0 }),
]
