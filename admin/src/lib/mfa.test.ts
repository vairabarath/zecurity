import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { CombinedGraphQLErrors, ServerError } from '@apollo/client/errors'
import { useAuthStore } from '@/store/auth'
import { MfaApiError, getMfaStatus, mfaRequiredKind, verifyMfaCode } from './mfa'

function serverError(status: number, body: string) {
  return new ServerError('refused', { response: new Response(body, { status }), bodyText: body })
}

const CONSOLE_BODY = JSON.stringify({
  errors: [{ message: 'multi-factor authentication is required', extensions: { code: 'MFA_REQUIRED' } }],
})

function jsonResponse(status: number, body: unknown, headers: Record<string, string> = {}) {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json', ...headers },
  })
}

describe('mfaRequiredKind (PENDING-06)', () => {
  it('reads the console-wide refusal from a 403 body', () => {
    expect(mfaRequiredKind(serverError(403, CONSOLE_BODY))).toBe('console')
  })

  it('reads a step-up refusal from GraphQL errors', () => {
    const err = new CombinedGraphQLErrors({
      errors: [{ message: 'needs a fresh second factor', extensions: { code: 'MFA_REQUIRED' } }],
    })
    expect(mfaRequiredKind(err)).toBe('step-up')
  })

  it('ignores everything else', () => {
    expect(mfaRequiredKind(serverError(403, '{"errors":[{"message":"x","extensions":{"code":"FORBIDDEN"}}]}'))).toBeNull()
    expect(mfaRequiredKind(serverError(403, 'not json'))).toBeNull()
    expect(mfaRequiredKind(serverError(401, CONSOLE_BODY))).toBeNull()
    expect(mfaRequiredKind(new CombinedGraphQLErrors({ errors: [{ message: 'MFA_REQUIRED' }] }))).toBeNull()
    expect(mfaRequiredKind(new Error('MFA_REQUIRED'))).toBeNull()
  })
})

describe('MFA REST calls', () => {
  beforeEach(() => useAuthStore.setState({ accessToken: 'old' }))
  afterEach(() => vi.unstubAllGlobals())

  it('verify stores the new MFA-marked access token', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(200, { accessToken: 'marked', expiresIn: 900, enrolled: false }))
    vi.stubGlobal('fetch', fetchMock)
    await expect(verifyMfaCode('123456')).resolves.toEqual({ enrolled: false })
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/auth/mfa/verify')
    expect(init.headers.Authorization).toBe('Bearer old')
    expect(JSON.parse(init.body)).toEqual({ code: '123456' })
    expect(useAuthStore.getState().accessToken).toBe('marked')
  })

  it('maps error codes and Retry-After', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      jsonResponse(429, { code: 'RATE_LIMITED', error: 'too many' }, { 'Retry-After': '120' }),
    ))
    const err = await verifyMfaCode('000000').catch((e: unknown) => e)
    expect(err).toBeInstanceOf(MfaApiError)
    expect((err as MfaApiError).code).toBe('RATE_LIMITED')
    expect((err as MfaApiError).retryAfterSeconds).toBe(120)
    expect(useAuthStore.getState().accessToken).toBe('old')
  })

  it('a wrong code (401 INVALID_CODE) is not a session expiry: no refresh', async () => {
    const fetchMock = vi.fn().mockResolvedValue(jsonResponse(401, { code: 'INVALID_CODE', error: 'that code is not valid' }))
    vi.stubGlobal('fetch', fetchMock)
    const err = await verifyMfaCode('000000').catch((e: unknown) => e)
    expect((err as MfaApiError).code).toBe('INVALID_CODE')
    expect(fetchMock).toHaveBeenCalledTimes(1)
  })

  it('refreshes an expired access token once and retries', async () => {
    const fetchMock = vi.fn()
      .mockResolvedValueOnce(jsonResponse(401, { code: 'UNAUTHORIZED' }))
      .mockResolvedValueOnce(jsonResponse(200, { access_token: 'fresh' }))
      .mockResolvedValueOnce(jsonResponse(200, { enrolled: true, consoleRequireMfa: false, sessionMfaAt: null }))
    vi.stubGlobal('fetch', fetchMock)
    await expect(getMfaStatus()).resolves.toMatchObject({ enrolled: true })
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual(['/auth/mfa/status', '/auth/refresh', '/auth/mfa/status'])
    expect(fetchMock.mock.calls[2][1].headers.Authorization).toBe('Bearer fresh')
  })

  it('a dead session is UNAUTHORIZED, without a redirect', async () => {
    vi.stubGlobal('fetch', vi.fn()
      .mockResolvedValueOnce(jsonResponse(401, {}))
      .mockResolvedValueOnce(jsonResponse(401, {})))
    const err = await getMfaStatus().catch((e: unknown) => e)
    expect((err as MfaApiError).code).toBe('UNAUTHORIZED')
  })
})
