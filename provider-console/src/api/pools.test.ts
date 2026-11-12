import { describe, expect, it } from 'vitest'
import { useSessionStore } from '@/auth/session'
import { mockFetch } from '@/test/fetch'
import { assignTenantRelayPool, createRelayPool, deleteRelayPool, moveRelayToPool, renameRelayPool, setRelayPoolPlacement } from './pools'
import { createRelay } from './relays'

// Relay-pool writes (D-37 … D-41): exact methods, paths and bodies. Reasons
// go in the body, never the URL.

const POOL = 'eeeeeeee-0000-0000-0000-0000000000e1'
const RELAY = '11111111-1111-1111-1111-111111111111'
const TENANT = 'aaaaaaaa-1111-2222-3333-444444444444'
const FAILOVER = 'eeeeeeee-0000-0000-0000-0000000000e3'

describe('relay-pool writes', () => {
  it('create without placement sends the name only (what relay-ops may send)', async () => {
    useSessionStore.getState().startSession('tok', 900, false)
    const { calls } = mockFetch({ 'POST /provider/relay-pools': { status: 201, body: { id: POOL, name: 'eu-1', region: null, residency: false } } })
    await expect(createRelayPool('eu-1')).resolves.toEqual({ id: POOL, name: 'eu-1', region: null, residency: false })
    await createRelayPool('eu-2', null, false)
    expect(calls.map((c) => c.body)).toEqual([{ name: 'eu-1' }, { name: 'eu-2' }])
  })

  it('create with region and residency sends both', async () => {
    useSessionStore.getState().startSession('tok', 900, false)
    const { calls } = mockFetch({ 'POST /provider/relay-pools': { status: 201, body: { id: POOL, name: 'eu-1', region: 'eu-central-1', residency: true } } })
    await createRelayPool('eu-1', 'eu-central-1', true)
    expect(calls[0]).toMatchObject({ method: 'POST', path: '/provider/relay-pools', body: { name: 'eu-1', region: 'eu-central-1', residency: true } })
  })

  it('rename sends only the name; placement sends region (null clears) and residency', async () => {
    useSessionStore.getState().startSession('tok', 900, false)
    const { calls } = mockFetch({ [`PATCH /provider/relay-pools/${POOL}`]: { status: 204 } })
    await renameRelayPool(POOL, 'eu-west')
    await setRelayPoolPlacement(POOL, null, false)
    expect(calls.map((c) => c.body)).toEqual([{ name: 'eu-west' }, { region: null, residency: false }])
  })

  it('delete sends no body', async () => {
    useSessionStore.getState().startSession('tok', 900, false)
    const { calls } = mockFetch({ [`DELETE /provider/relay-pools/${POOL}`]: { status: 204 } })
    await deleteRelayPool(POOL)
    expect(calls[0]).toMatchObject({ method: 'DELETE', path: `/provider/relay-pools/${POOL}`, body: undefined, search: '' })
  })

  it('move and tenant assignment put the reason in the body', async () => {
    useSessionStore.getState().startSession('tok', 900, false)
    const { calls } = mockFetch({
      [`POST /provider/relays/${RELAY}/pool`]: { status: 204 },
      [`PUT /provider/tenants/${TENANT}/relay-pool`]: { status: 204 },
    })
    await moveRelayToPool(RELAY, POOL, 'rebalance')
    await assignTenantRelayPool(TENANT, POOL, null, 'EU customer', 'immediate')
    await assignTenantRelayPool(TENANT, POOL, FAILOVER, 'add failover', 'graceful')
    expect(calls[0]).toMatchObject({ method: 'POST', body: { pool_id: POOL, reason: 'rebalance' }, search: '' })
    // D-39: the body is the whole assignment, so failover_pool_id is always present (null removes it).
    expect(calls[1]).toMatchObject({ method: 'PUT', search: '' })
    expect(calls[1].body).toEqual({ pool_id: POOL, failover_pool_id: null, reason: 'EU customer', migration: 'immediate' })
    expect(calls[2].body).toEqual({ pool_id: POOL, failover_pool_id: FAILOVER, reason: 'add failover', migration: 'graceful' })
  })

  it('Add relay sends pool_id only when one is chosen', async () => {
    useSessionStore.getState().startSession('tok', 900, false)
    const { calls } = mockFetch({ 'POST /provider/relays': { status: 201, body: {} } })
    await createRelay('r1', [], [], POOL)
    await createRelay('r2', [], [])
    expect(calls.map((c) => c.body)).toEqual([
      { name: 'r1', dns_allowlist: [], ip_allowlist: [], pool_id: POOL },
      { name: 'r2', dns_allowlist: [], ip_allowlist: [] },
    ])
  })
})
