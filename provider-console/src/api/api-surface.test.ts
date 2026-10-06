import { describe, expect, it } from 'vitest'
import { useSessionStore } from '@/auth/session'
import { mockFetch, superAdminMe } from '@/test/fetch'
import * as authApi from './auth'
import * as operatorsApi from './operators'
import * as readsApi from './reads'

// Invariant 2 (Phase C, AT-CORE-3): the console's only non-GET calls are the
// three auth calls and the five operator mutations (C-b). Phase P adds GETs
// only. Every exported API function is called here; a new write must be
// added to ALLOWED_WRITES, and a new read to ALLOWED_READS, on purpose.

const ALLOWED_WRITES = [
  'PATCH /provider/users/{id}',
  'POST /provider/auth/login',
  'POST /provider/auth/logout',
  'POST /provider/auth/password',
  'POST /provider/users',
  'POST /provider/users/{id}/disable',
  'POST /provider/users/{id}/enable',
  'POST /provider/users/{id}/reset-password',
]

const ALLOWED_READS = [
  'GET /provider/audit',
  'GET /provider/certificates',
  'GET /provider/me',
  'GET /provider/relays',
  'GET /provider/relays/{id}',
  'GET /provider/tenants',
  'GET /provider/tenants/{id}',
  'GET /provider/users',
]

const API_MODULES: Record<string, Record<string, unknown>> = { auth: authApi, operators: operatorsApi, reads: readsApi }

// Every function is called with ('a', 'b'): an id is always "a" (list
// wrappers ignore the non-object argument).
const ID = 'a'
const normalize = (path: string) => path.replace(new RegExp(`^(/provider/(?:users|relays|tenants))/${ID}(?=/|$)`), '$1/{id}')

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
      'GET /provider/relays': page,
      [`GET /provider/relays/${ID}`]: { status: 200, body: {} },
      'GET /provider/tenants': page,
      [`GET /provider/tenants/${ID}`]: { status: 200, body: {} },
      'GET /provider/audit': page,
      'GET /provider/certificates': { status: 200, body: { summary: {}, items: [], next_cursor: null } },
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
      'GET /provider/tenants': page,
      [`GET /provider/tenants/${ID}`]: { status: 200, body: {} },
      'GET /provider/audit': page,
      'GET /provider/certificates': { status: 200, body: { summary: {}, items: [], next_cursor: null } },
    })
    const wrappers = Object.entries(readsApi).filter(([name, fn]) => typeof fn === 'function' && name !== 'buildQuery')
    expect(wrappers.map(([n]) => n).sort()).toEqual(
      ['getCertificates', 'getRelay', 'getTenant', 'listRelays', 'listTenants', 'queryAudit'],
    )
    for (const [, fn] of wrappers) await (fn as (id: string) => Promise<unknown>)(ID)
    expect(calls.every((c) => c.method === 'GET' && c.body === undefined)).toBe(true)
  })
})
