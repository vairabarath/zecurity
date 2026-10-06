import { describe, expect, it } from 'vitest'
import { SESSION_STORAGE_KEY, useSessionStore } from '@/auth/session'
import { mockFetch, superAdminMe } from '@/test/fetch'
import { ApiError } from './client'
import {
  changeOperatorRole,
  createOperator,
  disableOperator,
  enableOperator,
  listOperators,
  resetOperatorPassword,
} from './operators'
import type { OperatorView } from './types'

const ID = '33333333-3333-3333-3333-333333333333'
const TEMP = 'TempPass-Abc23456'

const operator: OperatorView = {
  id: ID,
  email: 'ops@provider.test',
  role: 'relay-ops',
  disabled_at: null,
  created_at: '2026-10-05T08:00:00Z',
  last_login_at: null,
  must_change_password: true,
  has_password: true,
}

function signedIn() {
  useSessionStore.getState().startSession('tok-full', 900, false)
  useSessionStore.getState().setMe(superAdminMe)
}

describe('operator API', () => {
  it('lists operators with the bearer token', async () => {
    signedIn()
    const { calls } = mockFetch({ 'GET /provider/users': { status: 200, body: [operator] } })
    await expect(listOperators()).resolves.toEqual([operator])
    expect(calls[0]).toMatchObject({ method: 'GET', path: '/provider/users', body: undefined })
    expect(calls[0].headers.Authorization).toBe('Bearer tok-full')
  })

  it('creates an operator and returns the one-time temporary password', async () => {
    signedIn()
    const { calls } = mockFetch({
      'POST /provider/users': { status: 201, body: { user: operator, temporary_password: TEMP } },
    })
    await expect(createOperator('ops@provider.test', 'relay-ops')).resolves.toEqual({
      user: operator,
      temporary_password: TEMP,
    })
    expect(calls[0]).toMatchObject({ method: 'POST', body: { email: 'ops@provider.test', role: 'relay-ops' } })
  })

  it('changes a role with PATCH', async () => {
    signedIn()
    const updated = { ...operator, role: 'super-admin' }
    const { calls } = mockFetch({ [`PATCH /provider/users/${ID}`]: { status: 200, body: { user: updated } } })
    await expect(changeOperatorRole(ID, 'super-admin')).resolves.toEqual({ user: updated })
    expect(calls[0].body).toEqual({ role: 'super-admin' })
  })

  it('disable and enable are bodyless POSTs that return nothing', async () => {
    signedIn()
    const { calls } = mockFetch({
      [`POST /provider/users/${ID}/disable`]: { status: 204 },
      [`POST /provider/users/${ID}/enable`]: { status: 204 },
    })
    await expect(disableOperator(ID)).resolves.toBeUndefined()
    await expect(enableOperator(ID)).resolves.toBeUndefined()
    expect(calls.map((c) => [c.method, c.path, c.body])).toEqual([
      ['POST', `/provider/users/${ID}/disable`, undefined],
      ['POST', `/provider/users/${ID}/enable`, undefined],
    ])
  })

  it('enable does not also reset the password', async () => {
    signedIn()
    const { calls } = mockFetch({ [`POST /provider/users/${ID}/enable`]: { status: 204 } })
    await enableOperator(ID)
    expect(calls).toHaveLength(1)
  })

  it('reset returns the one-time temporary password', async () => {
    signedIn()
    mockFetch({ [`POST /provider/users/${ID}/reset-password`]: { status: 200, body: { temporary_password: TEMP } } })
    await expect(resetOperatorPassword(ID)).resolves.toEqual({ temporary_password: TEMP })
  })

  it('encodes the id into the path', async () => {
    signedIn()
    const { calls } = mockFetch({ 'POST /provider/users/a%2Fb/disable': { status: 204 } })
    await disableOperator('a/b')
    expect(calls[0].path).toBe('/provider/users/a%2Fb/disable')
  })

  it('temporary passwords are never written to storage', async () => {
    signedIn()
    mockFetch({
      'POST /provider/users': { status: 201, body: { user: operator, temporary_password: TEMP } },
      [`POST /provider/users/${ID}/reset-password`]: { status: 200, body: { temporary_password: TEMP } },
    })
    await createOperator('ops@provider.test', 'relay-ops')
    await resetOperatorPassword(ID)
    expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).not.toContain(TEMP)
    expect(sessionStorage.length).toBe(1) // the session entry only
    expect(localStorage.length).toBe(0)
    expect(JSON.stringify(useSessionStore.getState())).not.toContain(TEMP)
  })

  it.each([
    ['cannot_modify_self', 409],
    ['last_super_admin', 409],
    ['already_exists', 409],
    ['exists_disabled', 409],
    ['account_disabled', 409],
    ['invalid_email', 400],
    ['invalid_role', 400],
    ['not_found', 404],
    ['service_unavailable', 503],
  ])('%s (%i) surfaces as an ApiError and keeps the session', async (code, status) => {
    signedIn()
    mockFetch({ [`PATCH /provider/users/${ID}`]: { status, body: { error: code } } })
    const err = await changeOperatorRole(ID, 'relay-ops').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status, code, sessionEnded: false })
    expect(useSessionStore.getState()).toMatchObject({ status: 'authenticated', token: 'tok-full' })
  })

  it.each([
    ['GET /provider/users', 'provider action forbidden', () => listOperators()],
    ['POST /provider/users', 'forbidden', () => createOperator('x@provider.test', 'relay-ops')],
  ])('a 403 from %s keeps the session', async (route, error, call) => {
    signedIn()
    mockFetch({ [route]: { status: 403, body: { error } } })
    await expect(call()).rejects.toMatchObject({ status: 403, sessionEnded: false })
    expect(useSessionStore.getState().status).toBe('authenticated')
  })

  it('a revoked session during an operator call ends the session (C-a handling unchanged)', async () => {
    signedIn()
    mockFetch({ [`POST /provider/users/${ID}/disable`]: { status: 401, body: { error: 'provider session revoked' } } })
    await expect(disableOperator(ID)).rejects.toMatchObject({ sessionEnded: true })
    expect(useSessionStore.getState()).toMatchObject({ status: 'anonymous', endReason: 'session_ended' })
  })
})
