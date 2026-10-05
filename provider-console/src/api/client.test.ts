import { describe, expect, it } from 'vitest'
import { SESSION_STORAGE_KEY, useSessionStore } from '@/auth/session'
import { middlewareSessionEnding401, mockFetch, superAdminMe } from '@/test/fetch'
import { ApiError, request } from './client'

function signedIn() {
  const s = useSessionStore.getState()
  s.startSession('tok-full', 900, false)
  s.setMe(superAdminMe)
}

async function caught(p: Promise<unknown>): Promise<ApiError> {
  try {
    await p
  } catch (e) {
    expect(e).toBeInstanceOf(ApiError)
    return e as ApiError
  }
  throw new Error('expected the request to fail')
}

describe('request', () => {
  it('sends the bearer token, JSON body and no cookies', async () => {
    signedIn()
    const { fn, calls } = mockFetch({ 'POST /provider/x': { status: 200, body: { ok: true } } })
    await expect(request('POST', '/provider/x', { body: { a: 1 } })).resolves.toEqual({ ok: true })
    expect(calls[0].headers.Authorization).toBe('Bearer tok-full')
    expect(calls[0].headers['Content-Type']).toBe('application/json')
    expect(calls[0].body).toEqual({ a: 1 })
    expect(fn.mock.calls[0][1]).toMatchObject({ credentials: 'omit', cache: 'no-store' })
  })

  it('omits the bearer token when auth is false', async () => {
    signedIn()
    const { calls } = mockFetch({ 'POST /provider/x': { status: 200, body: {} } })
    await request('POST', '/provider/x', { auth: false })
    expect(calls[0].headers.Authorization).toBeUndefined()
  })

  it('returns undefined for 204', async () => {
    signedIn()
    mockFetch({ 'POST /provider/x': { status: 204 } })
    await expect(request('POST', '/provider/x')).resolves.toBeUndefined()
  })

  it('parses the error code, detail and Retry-After', async () => {
    mockFetch({
      'POST /provider/a': { status: 400, body: { error: 'password_policy', detail: 'too short' } },
      'POST /provider/b': { status: 429, body: { error: 'too_many_attempts' }, headers: { 'Retry-After': '540' } },
      'POST /provider/c': { status: 502 },
    })
    expect(await caught(request('POST', '/provider/a'))).toMatchObject({
      status: 400,
      code: 'password_policy',
      detail: 'too short',
    })
    expect(await caught(request('POST', '/provider/b'))).toMatchObject({
      status: 429,
      code: 'too_many_attempts',
      retryAfterSeconds: 540,
    })
    expect(await caught(request('POST', '/provider/c'))).toMatchObject({ status: 502, code: 'http_502' })
  })

  it.each(middlewareSessionEnding401)('middleware 401 "%s" ends the session', async (msg) => {
    signedIn()
    mockFetch({ 'GET /provider/me': { status: 401, body: { error: msg } } })
    const err = await caught(request('GET', '/provider/me'))
    expect(err.sessionEnded).toBe(true)
    const s = useSessionStore.getState()
    expect(s).toMatchObject({ status: 'anonymous', token: undefined, me: undefined, endReason: 'session_ended' })
    expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).toBeNull()
  })

  it('a 401 after local expiry is reported as expired', async () => {
    useSessionStore.getState().restoreSession({ token: 't', expiresAt: Date.now() - 1, pwcOnly: false })
    mockFetch({ 'GET /provider/me': { status: 401, body: { error: 'invalid or expired provider token' } } })
    await caught(request('GET', '/provider/me'))
    expect(useSessionStore.getState().endReason).toBe('expired')
  })

  it('401 invalid_credentials on a credential check is a form error and keeps the session', async () => {
    signedIn()
    mockFetch({ 'POST /provider/auth/password': { status: 401, body: { error: 'invalid_credentials' } } })
    const err = await caught(request('POST', '/provider/auth/password', { credentialCheck: true }))
    expect(err).toMatchObject({ code: 'invalid_credentials', sessionEnded: false })
    expect(useSessionStore.getState()).toMatchObject({ status: 'authenticated', token: 'tok-full' })
    expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).not.toBeNull()
  })

  it('401 invalid_credentials on any other call still ends the session', async () => {
    signedIn()
    mockFetch({ 'GET /provider/me': { status: 401, body: { error: 'invalid_credentials' } } })
    expect((await caught(request('GET', '/provider/me'))).sessionEnded).toBe(true)
    expect(useSessionStore.getState().status).toBe('anonymous')
  })

  it('403 "not a provider user" (disabled) ends the session', async () => {
    signedIn()
    mockFetch({ 'GET /provider/me': { status: 403, body: { error: 'not a provider user' } } })
    expect((await caught(request('GET', '/provider/me'))).sessionEnded).toBe(true)
    expect(useSessionStore.getState()).toMatchObject({ status: 'anonymous', endReason: 'disabled' })
    expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).toBeNull()
  })

  it('403 password_change_required moves to the password-change state and keeps the token', async () => {
    signedIn()
    mockFetch({ 'GET /provider/me': { status: 403, body: { error: 'password_change_required' } } })
    const err = await caught(request('GET', '/provider/me'))
    expect(err).toMatchObject({ code: 'password_change_required', sessionEnded: false })
    expect(useSessionStore.getState()).toMatchObject({ status: 'password_change', pwcOnly: true, token: 'tok-full' })
    expect(JSON.parse(sessionStorage.getItem(SESSION_STORAGE_KEY)!).pwcOnly).toBe(true)
  })

  it('403 forbidden is an error for the page; the session stays', async () => {
    signedIn()
    mockFetch({ 'GET /provider/users': { status: 403, body: { error: 'forbidden' } } })
    expect(await caught(request('GET', '/provider/users'))).toMatchObject({ status: 403, code: 'forbidden' })
    expect(useSessionStore.getState()).toMatchObject({ status: 'authenticated', token: 'tok-full' })
  })

  it('5xx and network errors leave the session alone', async () => {
    signedIn()
    mockFetch({
      'GET /provider/me': { status: 500, body: { error: 'provider lookup failed' } },
      'GET /provider/users': 'network-error',
    })
    expect((await caught(request('GET', '/provider/me'))).code).toBe('provider lookup failed')
    expect(await caught(request('GET', '/provider/users'))).toMatchObject({ status: 0, code: 'network_error' })
    expect(useSessionStore.getState().status).toBe('authenticated')
  })
})
