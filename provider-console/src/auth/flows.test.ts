import { describe, expect, it } from 'vitest'
import { ApiError } from '@/api/client'
import { mockFetch, superAdminMe, type RecordedCall } from '@/test/fetch'
import { changePassword, rehydrate, signIn, signOut } from './flows'
import { SESSION_STORAGE_KEY, useSessionStore } from './session'

const PASSWORD = 'correct horse battery staple'
const NEW_PASSWORD = 'a brand new long passphrase'

function storeSession(token: string, pwcOnly = false) {
  sessionStorage.setItem(
    SESSION_STORAGE_KEY,
    JSON.stringify({ token, expiresAt: Date.now() + 600_000, pwcOnly }),
  )
}

function storedToken(): string | undefined {
  const raw = sessionStorage.getItem(SESSION_STORAGE_KEY)
  return raw ? JSON.parse(raw).token : undefined
}

describe('rehydrate', () => {
  it('does nothing without a stored session', async () => {
    const { calls } = mockFetch({})
    await rehydrate()
    expect(calls).toHaveLength(0)
    expect(useSessionStore.getState().status).toBe('anonymous')
  })

  it('trusts a stored token only after /provider/me accepts it', async () => {
    storeSession('tok-stored')
    const { calls } = mockFetch({ 'GET /provider/me': { status: 200, body: superAdminMe } })
    const pending = rehydrate()
    expect(useSessionStore.getState().status).toBe('loading')
    await pending
    expect(calls[0].headers.Authorization).toBe('Bearer tok-stored')
    expect(useSessionStore.getState()).toMatchObject({ status: 'authenticated', me: superAdminMe })
  })

  it('a password-change-only token lands on the password-change state', async () => {
    storeSession('tok-pwc', true)
    mockFetch({ 'GET /provider/me': { status: 403, body: { error: 'password_change_required' } } })
    await rehydrate()
    expect(useSessionStore.getState()).toMatchObject({ status: 'password_change', token: 'tok-pwc' })
  })

  it('a revoked token is cleared', async () => {
    storeSession('tok-revoked')
    mockFetch({ 'GET /provider/me': { status: 401, body: { error: 'provider session revoked' } } })
    await rehydrate()
    expect(useSessionStore.getState()).toMatchObject({ status: 'anonymous', endReason: 'session_ended' })
    expect(storedToken()).toBeUndefined()
  })

  it('a disabled account is cleared', async () => {
    storeSession('tok')
    mockFetch({ 'GET /provider/me': { status: 403, body: { error: 'not a provider user' } } })
    await rehydrate()
    expect(useSessionStore.getState()).toMatchObject({ status: 'anonymous', endReason: 'disabled' })
  })

  it('fails closed when the server cannot confirm the session', async () => {
    storeSession('tok')
    mockFetch({ 'GET /provider/me': { status: 500, body: { error: 'provider lookup failed' } } })
    await rehydrate()
    expect(useSessionStore.getState()).toMatchObject({ status: 'anonymous', endReason: 'unverified' })
    expect(storedToken()).toBeUndefined()
  })
})

describe('signIn', () => {
  it('stores the token, loads /provider/me and is authenticated', async () => {
    const { calls } = mockFetch({
      'POST /provider/auth/login': {
        status: 200,
        body: { token: 'tok-full', expires_in: 900, password_change_required: false },
      },
      'GET /provider/me': { status: 200, body: superAdminMe },
    })
    await expect(signIn('admin@provider.test', PASSWORD)).resolves.toBe('authenticated')
    expect(calls[0].headers.Authorization).toBeUndefined()
    expect(calls[0].body).toEqual({ email: 'admin@provider.test', password: PASSWORD })
    expect(calls[1].headers.Authorization).toBe('Bearer tok-full')
    expect(useSessionStore.getState()).toMatchObject({ status: 'authenticated', me: superAdminMe })
    expect(storedToken()).toBe('tok-full')
  })

  it('a must-change login stops at the password-change state without calling /me', async () => {
    const { calls } = mockFetch({
      'POST /provider/auth/login': {
        status: 200,
        body: { token: 'tok-pwc', expires_in: 900, password_change_required: true },
      },
    })
    await expect(signIn('admin@provider.test', PASSWORD)).resolves.toBe('password_change')
    expect(calls).toHaveLength(1)
    expect(useSessionStore.getState()).toMatchObject({ status: 'password_change', pwcOnly: true })
  })

  it('wrong credentials throw a form error and store nothing', async () => {
    mockFetch({ 'POST /provider/auth/login': { status: 401, body: { error: 'invalid_credentials' } } })
    const err = await signIn('admin@provider.test', 'wrong').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ code: 'invalid_credentials', sessionEnded: false })
    expect(useSessionStore.getState().status).toBe('anonymous')
    expect(sessionStorage.length).toBe(0)
  })
})

describe('changePassword', () => {
  function authenticated(token: string) {
    useSessionStore.getState().startSession(token, 900, false)
    useSessionStore.getState().setMe(superAdminMe)
  }

  it('switches to the new token and reloads /provider/me with it', async () => {
    useSessionStore.getState().startSession('tok-pwc', 900, true)
    const { calls } = mockFetch({
      'POST /provider/auth/password': { status: 200, body: { token: 'tok-new', expires_in: 900 } },
      'GET /provider/me': { status: 200, body: superAdminMe },
    })
    await changePassword(PASSWORD, NEW_PASSWORD)
    expect(calls[0].headers.Authorization).toBe('Bearer tok-pwc')
    expect(calls[0].body).toEqual({ current_password: PASSWORD, new_password: NEW_PASSWORD })
    expect(calls[1].headers.Authorization).toBe('Bearer tok-new')
    expect(useSessionStore.getState()).toMatchObject({ status: 'authenticated', token: 'tok-new', pwcOnly: false })
    expect(storedToken()).toBe('tok-new')
  })

  it('a wrong current password is a form error; the user stays signed in', async () => {
    authenticated('tok-full')
    mockFetch({ 'POST /provider/auth/password': { status: 401, body: { error: 'invalid_credentials' } } })
    await expect(changePassword('wrong', NEW_PASSWORD)).rejects.toMatchObject({ code: 'invalid_credentials' })
    expect(useSessionStore.getState()).toMatchObject({ status: 'authenticated', token: 'tok-full' })
    expect(storedToken()).toBe('tok-full')
  })

  it('a policy rejection carries the server detail and keeps the session', async () => {
    authenticated('tok-full')
    mockFetch({
      'POST /provider/auth/password': {
        status: 400,
        body: { error: 'password_policy', detail: 'password policy: must be 12-128 characters' },
      },
    })
    await expect(changePassword(PASSWORD, 'short')).rejects.toMatchObject({
      code: 'password_policy',
      detail: 'password policy: must be 12-128 characters',
    })
    expect(useSessionStore.getState().status).toBe('authenticated')
  })

  it('a revoked session during the change ends the session', async () => {
    authenticated('tok-full')
    mockFetch({ 'POST /provider/auth/password': { status: 401, body: { error: 'provider session revoked' } } })
    await expect(changePassword(PASSWORD, NEW_PASSWORD)).rejects.toMatchObject({ sessionEnded: true })
    expect(useSessionStore.getState()).toMatchObject({ status: 'anonymous', endReason: 'session_ended' })
  })
})

describe('signOut', () => {
  it('posts logout with the bearer token and clears the session', async () => {
    useSessionStore.getState().startSession('tok-full', 900, false)
    const { calls } = mockFetch({ 'POST /provider/auth/logout': { status: 204 } })
    await signOut()
    expect(calls[0]).toMatchObject({ method: 'POST', path: '/provider/auth/logout' })
    expect(calls[0].headers.Authorization).toBe('Bearer tok-full')
    expect(useSessionStore.getState()).toMatchObject({ status: 'anonymous', endReason: 'logged_out' })
    expect(sessionStorage.length).toBe(0)
  })

  it.each([
    ['a server error', { status: 500, body: { error: 'server_error' } }],
    ['an already-revoked session', { status: 401, body: { error: 'provider session revoked' } }],
    ['a network failure', 'network-error' as const],
  ])('still clears locally after %s', async (_name, route) => {
    useSessionStore.getState().startSession('tok-full', 900, false)
    mockFetch({ 'POST /provider/auth/logout': route })
    await signOut()
    expect(useSessionStore.getState()).toMatchObject({ status: 'anonymous', endReason: 'logged_out' })
    expect(sessionStorage.length).toBe(0)
  })
})

describe('no persistence of secrets', () => {
  it('passwords never reach storage and tokens never reach localStorage', async () => {
    mockFetch({
      'POST /provider/auth/login': {
        status: 200,
        body: { token: 'tok-pwc', expires_in: 900, password_change_required: true },
      },
      'POST /provider/auth/password': (c: RecordedCall) =>
        c.headers.Authorization === 'Bearer tok-pwc'
          ? { status: 200, body: { token: 'tok-new', expires_in: 900 } }
          : { status: 401, body: { error: 'provider session revoked' } },
      'GET /provider/me': { status: 200, body: superAdminMe },
    })
    await signIn('admin@provider.test', PASSWORD)
    await changePassword(PASSWORD, NEW_PASSWORD)

    const sessionDump = Array.from({ length: sessionStorage.length }, (_, i) => {
      const key = sessionStorage.key(i)!
      return `${key}=${sessionStorage.getItem(key)}`
    }).join('\n')
    expect(sessionDump).not.toContain(PASSWORD)
    expect(sessionDump).not.toContain(NEW_PASSWORD)
    expect(sessionDump).not.toContain('tok-pwc')
    expect(localStorage.length).toBe(0)
    expect(window.location.href).not.toMatch(/tok-|horse|passphrase/)
    expect(JSON.stringify(useSessionStore.getState())).not.toMatch(/horse|passphrase/)
  })
})
