import { describe, expect, it } from 'vitest'
import { useSessionStore } from '@/auth/session'
import { mockFetch, superAdminMe } from '@/test/fetch'
import * as authApi from './auth'
import * as operatorsApi from './operators'

// Invariant 2 (Phase C): the console's only non-GET calls are the three auth
// calls and the five operator mutations (C-b). Every exported API function is
// called here; a new write must be added to ALLOWED_WRITES on purpose.

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

const API_MODULES: Record<string, Record<string, unknown>> = { auth: authApi, operators: operatorsApi }

// Every function is called with ('a', 'b'), so an operator id is always "a".
const ID = 'a'
const normalize = (path: string) => path.replace(`/provider/users/${ID}`, '/provider/users/{id}')

describe('API surface', () => {
  it('the only non-GET calls are the allowed writes, and every call stays under /provider/', async () => {
    useSessionStore.getState().startSession('tok', 900, false)
    const ok = {
      status: 200,
      body: { token: 't', expires_in: 900, password_change_required: false, temporary_password: 'x', ...superAdminMe },
    }
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
    })

    let exported = 0
    for (const mod of Object.values(API_MODULES)) {
      for (const fn of Object.values(mod)) {
        if (typeof fn !== 'function') continue
        exported++
        await (fn as (...args: string[]) => Promise<unknown>)(ID, 'b')
      }
    }

    expect(calls).toHaveLength(exported)
    expect(calls.every((c) => c.path.startsWith('/provider/'))).toBe(true)
    const writes = [
      ...new Set(calls.filter((c) => c.method !== 'GET').map((c) => `${c.method} ${normalize(c.path)}`)),
    ].sort()
    expect(writes).toEqual(ALLOWED_WRITES)
  })
})
