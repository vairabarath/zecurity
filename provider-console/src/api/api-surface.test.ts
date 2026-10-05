import { describe, expect, it } from 'vitest'
import { useSessionStore } from '@/auth/session'
import { mockFetch, superAdminMe } from '@/test/fetch'
import * as authApi from './auth'

// Invariant 2 (Phase C): the console's only non-GET calls are the three auth
// calls and, from C-b on, the five operator mutations. Every exported API
// function is called here; a new write must be added to ALLOWED_WRITES on
// purpose.

const ALLOWED_WRITES = [
  'POST /provider/auth/login',
  'POST /provider/auth/logout',
  'POST /provider/auth/password',
]

const API_MODULES: Record<string, Record<string, unknown>> = { auth: authApi }

describe('API surface', () => {
  it('the only non-GET calls are the allowed writes, and every call stays under /provider/', async () => {
    useSessionStore.getState().startSession('tok', 900, false)
    const ok = { status: 200, body: { token: 't', expires_in: 900, password_change_required: false, ...superAdminMe } }
    const { calls } = mockFetch({
      'POST /provider/auth/login': ok,
      'POST /provider/auth/password': ok,
      'POST /provider/auth/logout': { status: 204 },
      'GET /provider/me': ok,
    })

    let exported = 0
    for (const mod of Object.values(API_MODULES)) {
      for (const fn of Object.values(mod)) {
        if (typeof fn !== 'function') continue
        exported++
        await (fn as (...args: string[]) => Promise<unknown>)('a', 'b')
      }
    }

    expect(calls).toHaveLength(exported)
    expect(calls.every((c) => c.path.startsWith('/provider/'))).toBe(true)
    const writes = [...new Set(calls.filter((c) => c.method !== 'GET').map((c) => `${c.method} ${c.path}`))].sort()
    expect(writes).toEqual(ALLOWED_WRITES)
  })
})
