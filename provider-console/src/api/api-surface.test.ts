import { describe, expect, it } from 'vitest'
import { useSessionStore } from '@/auth/session'
import { mockFetch, superAdminMe } from '@/test/fetch'
import * as authApi from './auth'
import * as operatorsApi from './operators'
import * as plansApi from './plans'
import * as poolsApi from './pools'
import * as quotasApi from './quotas'
import * as readsApi from './reads'
import * as relaysApi from './relays'
import * as tenantsApi from './tenants'

// Invariant 2 (Phase C, AT-CORE-3): the console's only non-GET calls are the
// auth calls (incl. the Sprint 22 TOTP steps), the operator mutations
// (C-b, plus reset-totp) and the relay mutations (Sprint 22 D-30: Phase E
// adds create, Phase X revoke and delete, Phase D drain and resume). Phase P adds GETs
// only, and so do the provider quick wins (GET /provider/pki, GET /provider/health;
// the audit export reuses GET /provider/audit). Relay pools (D-41 amends D-30) add pool
// create/rename/region-residency/delete, moving a relay between pools and a
// tenant's pool assignment, plus the two pool reads. Plans (Sprint 23 B1, D-43 amends D-30) add plan create /
// update / delete / set default and a tenant's plan assignment, plus the two plan reads. Sprint 23 S1 (D-45
// amends D-30) adds tenant suspend and resume; S2 (D-44) adds POST /provider/tenants and the first-admin
// re-invite; S4 (D-46) adds delete, cancel delete and legal hold. Relay link capacity (Sprint 23 B2, D-42) adds
// PUT /provider/relays/{id}/link-capacity. Every exported API function is called here; a new write must be
// added to ALLOWED_WRITES, and a new read to ALLOWED_READS, on purpose.

const ALLOWED_WRITES = [
  'DELETE /provider/plans/{id}',
  'DELETE /provider/relay-pools/{id}',
  'DELETE /provider/relays/{id}',
  'PATCH /provider/plans/{id}',
  'PATCH /provider/relay-pools/{id}',
  'PATCH /provider/users/{id}',
  'POST /provider/auth/login',
  'POST /provider/auth/logout',
  'POST /provider/auth/password',
  'POST /provider/auth/totp/enroll',
  'POST /provider/auth/totp/enroll/confirm',
  'POST /provider/auth/totp/verify',
  'POST /provider/plans',
  'POST /provider/plans/{id}/default',
  'POST /provider/relay-pools',
  'POST /provider/relays',
  'POST /provider/relays/{id}/drain',
  'POST /provider/relays/{id}/pool',
  'POST /provider/relays/{id}/resume',
  'POST /provider/relays/{id}/revoke',
  'POST /provider/tenants',
  'POST /provider/tenants/{id}/delete',
  'POST /provider/tenants/{id}/delete/cancel',
  'POST /provider/tenants/{id}/first-admin-invitation',
  'POST /provider/tenants/{id}/resume',
  'POST /provider/tenants/{id}/suspend',
  'POST /provider/users',
  'POST /provider/users/{id}/disable',
  'POST /provider/users/{id}/enable',
  'POST /provider/users/{id}/reset-password',
  'POST /provider/users/{id}/reset-totp',
  'PUT /provider/relays/{id}/link-capacity',
  'PUT /provider/tenants/{id}/legal-hold',
  'PUT /provider/tenants/{id}/plan',
  'PUT /provider/tenants/{id}/quotas/{key}',
  'PUT /provider/tenants/{id}/relay-pool',
]

const ALLOWED_READS = [
  'GET /provider/audit',
  'GET /provider/certificates',
  'GET /provider/health',
  'GET /provider/me',
  'GET /provider/pki',
  'GET /provider/plans',
  'GET /provider/plans/{id}',
  'GET /provider/relay-pools',
  'GET /provider/relay-pools/{id}',
  'GET /provider/relays',
  'GET /provider/relays/{id}',
  'GET /provider/tenants',
  'GET /provider/tenants/{id}',
  'GET /provider/users',
]

const API_MODULES: Record<string, Record<string, unknown>> = {
  auth: authApi,
  operators: operatorsApi,
  plans: plansApi,
  pools: poolsApi,
  quotas: quotasApi,
  reads: readsApi,
  relays: relaysApi,
  tenants: tenantsApi,
}

// Every function is called with ('a', 'b'): an id is always "a" (list
// wrappers ignore the non-object argument).
const ID = 'a'
const normalize = (path: string) =>
  path
    .replace(new RegExp(`^(/provider/(?:users|relay-pools|relays|tenants|plans))/${ID}(?=/|$)`), '$1/{id}')
    .replace(/^(\/provider\/tenants\/\{id\}\/quotas)\/[^/]+$/, '$1/{key}')

describe('API surface', () => {
  it('writes are exactly the allowed writes, reads exactly the allowed reads, all under /provider/', async () => {
    useSessionStore.getState().startSession('tok', 900, false)
    const ok = {
      status: 200,
      body: { token: 't', expires_in: 900, password_change_required: false, temporary_password: 'x', ...superAdminMe },
    }
    const page = { status: 200, body: { items: [], next_cursor: null } }
    const { calls } = mockFetch({
      'POST /provider/auth/login': ok,
      'POST /provider/auth/password': ok,
      'POST /provider/auth/logout': { status: 204 },
      'GET /provider/me': ok,
      'GET /provider/users': { status: 200, body: [] },
      'POST /provider/users': { status: 201, body: { user: {}, temporary_password: 'x' } },
      [`PATCH /provider/users/${ID}`]: { status: 200, body: { user: {} } },
      [`POST /provider/users/${ID}/disable`]: { status: 204 },
      [`POST /provider/users/${ID}/enable`]: { status: 204 },
      [`POST /provider/users/${ID}/reset-password`]: { status: 200, body: { temporary_password: 'x' } },
      'POST /provider/auth/totp/verify': ok,
      'POST /provider/auth/totp/enroll': { status: 200, body: { secret: 'S', otpauth_uri: 'otpauth://totp/x' } },
      'POST /provider/auth/totp/enroll/confirm': ok,
      [`POST /provider/users/${ID}/reset-totp`]: { status: 204 },
      'GET /provider/relays': page,
      'POST /provider/relays': { status: 201, body: { relay_id: ID, provisioning_token: 't' } },
      [`POST /provider/relays/${ID}/revoke`]: { status: 204 },
      [`POST /provider/relays/${ID}/drain`]: { status: 204 },
      [`POST /provider/relays/${ID}/resume`]: { status: 204 },
      [`DELETE /provider/relays/${ID}`]: { status: 204 },
      [`GET /provider/relays/${ID}`]: { status: 200, body: {} },
      [`POST /provider/relays/${ID}/pool`]: { status: 204 },
      [`PUT /provider/relays/${ID}/link-capacity`]: { status: 204 },
      'GET /provider/relay-pools': { status: 200, body: { items: [] } },
      [`GET /provider/relay-pools/${ID}`]: { status: 200, body: {} },
      'POST /provider/relay-pools': { status: 201, body: { id: ID, name: 'a', region: null, residency: false } },
      [`PATCH /provider/relay-pools/${ID}`]: { status: 204 },
      [`DELETE /provider/relay-pools/${ID}`]: { status: 204 },
      [`PUT /provider/tenants/${ID}/relay-pool`]: { status: 204 },
      [`POST /provider/tenants/${ID}/suspend`]: { status: 204 },
      [`POST /provider/tenants/${ID}/resume`]: { status: 204 },
      'GET /provider/plans': { status: 200, body: { items: [] } },
      [`GET /provider/plans/${ID}`]: { status: 200, body: {} },
      'POST /provider/plans': { status: 201, body: { id: ID, name: 'a', description: '', limits: {}, is_default: false } },
      [`PATCH /provider/plans/${ID}`]: { status: 204 },
      [`DELETE /provider/plans/${ID}`]: { status: 204 },
      [`POST /provider/plans/${ID}/default`]: { status: 204 },
      [`PUT /provider/tenants/${ID}/plan`]: { status: 204 },
      [`PUT /provider/tenants/${ID}/quotas/b`]: { status: 204 },
      'GET /provider/tenants': page,
      [`POST /provider/tenants/${ID}/delete`]: { status: 200, body: { delete_after: '2026-12-18T00:00:00Z' } },
      [`POST /provider/tenants/${ID}/delete/cancel`]: { status: 204 },
      [`PUT /provider/tenants/${ID}/legal-hold`]: { status: 204 },
      [`POST /provider/tenants/${ID}/first-admin-invitation`]: { status: 201, body: { tenant_id: ID, invitation: { id: 'i', email: 'b', expires_at: '' } } },
      'POST /provider/tenants': { status: 201, body: { id: ID, slug: 'a', name: 'a', invitation: { id: 'i', email: 'b', expires_at: '' } } },
      [`GET /provider/tenants/${ID}`]: { status: 200, body: {} },
      'GET /provider/audit': page,
      'GET /provider/certificates': { status: 200, body: { summary: {}, items: [], next_cursor: null } },
      'GET /provider/pki': { status: 200, body: {} },
      'GET /provider/health': { status: 200, body: {} },
    })

    let exported = 0
    for (const mod of Object.values(API_MODULES)) {
      for (const [name, fn] of Object.entries(mod)) {
        if (typeof fn !== 'function' || name === 'buildQuery') continue
        exported++
        await (fn as (...args: string[]) => Promise<unknown>)(ID, 'b')
      }
    }

    expect(calls).toHaveLength(exported)
    expect(calls.every((c) => c.path.startsWith('/provider/'))).toBe(true)
    const surface = (pred: (m: string) => boolean) =>
      [...new Set(calls.filter((c) => pred(c.method)).map((c) => `${c.method} ${normalize(c.path)}`))].sort()
    expect(surface((m) => m !== 'GET')).toEqual(ALLOWED_WRITES)
    expect(surface((m) => m === 'GET')).toEqual(ALLOWED_READS)
  })

  it('the read wrappers issue GETs only', async () => {
    useSessionStore.getState().startSession('tok', 900, false)
    const page = { status: 200, body: { items: [], next_cursor: null } }
    const { calls } = mockFetch({
      'GET /provider/relays': page,
      [`GET /provider/relays/${ID}`]: { status: 200, body: {} },
      'GET /provider/relay-pools': { status: 200, body: { items: [] } },
      [`GET /provider/relay-pools/${ID}`]: { status: 200, body: {} },
      'GET /provider/tenants': page,
      [`GET /provider/tenants/${ID}`]: { status: 200, body: {} },
      'GET /provider/audit': page,
      'GET /provider/certificates': { status: 200, body: { summary: {}, items: [], next_cursor: null } },
      'GET /provider/pki': { status: 200, body: {} },
      'GET /provider/health': { status: 200, body: {} },
      'GET /provider/plans': { status: 200, body: { items: [] } },
      [`GET /provider/plans/${ID}`]: { status: 200, body: {} },
    })
    const wrappers = Object.entries(readsApi).filter(([name, fn]) => typeof fn === 'function' && name !== 'buildQuery')
    expect(wrappers.map(([n]) => n).sort()).toEqual(
      ['getCertificates', 'getHealth', 'getPki', 'getPlan', 'getRelay', 'getRelayPool', 'getTenant', 'listPlans', 'listRelayPools', 'listRelays', 'listTenants', 'queryAudit'],
    )
    for (const [, fn] of wrappers) await (fn as (id: string) => Promise<unknown>)(ID)
    expect(calls.every((c) => c.method === 'GET' && c.body === undefined)).toBe(true)
  })
})
