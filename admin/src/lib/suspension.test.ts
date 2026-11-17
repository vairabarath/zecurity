import { describe, expect, it } from 'vitest'
import { ServerError } from '@apollo/client/errors'
import { isSuspendedRefresh, isTenantSuspendedError, SUSPENDED_LOGIN_PATH } from './suspension'

// Sprint 23 S1 (D-45): the console recognises the provider-suspension refusal
// in the request and refresh shapes.

function serverError(status: number, body: string) {
  return new ServerError('refused', { response: new Response(body, { status }), bodyText: body })
}

const SUSPENDED_BODY = JSON.stringify({
  errors: [{ message: 'this workspace is suspended by the provider', extensions: { code: 'TENANT_SUSPENDED' } }],
})

describe('isTenantSuspendedError', () => {
  it('reads the 403 TENANT_SUSPENDED body', () => {
    expect(isTenantSuspendedError(serverError(403, SUSPENDED_BODY))).toBe(true)
  })
  it('ignores everything else', () => {
    expect(isTenantSuspendedError(serverError(401, SUSPENDED_BODY))).toBe(false)
    expect(isTenantSuspendedError(serverError(403, '{"errors":[{"message":"x","extensions":{"code":"FORBIDDEN"}}]}'))).toBe(false)
    expect(isTenantSuspendedError(serverError(403, 'not json'))).toBe(false)
    expect(isTenantSuspendedError(new Error('TENANT_SUSPENDED'))).toBe(false)
  })
})

describe('isSuspendedRefresh', () => {
  const json = (status: number, body: unknown) => new Response(JSON.stringify(body), { status })
  it('is true only for the 401 "workspace suspended" refusal, and leaves the body readable', async () => {
    const resp = json(401, { error: 'workspace suspended' })
    expect(await isSuspendedRefresh(resp)).toBe(true)
    expect(await resp.json()).toEqual({ error: 'workspace suspended' })
    expect(await isSuspendedRefresh(json(401, { error: 'session revoked' }))).toBe(false)
    expect(await isSuspendedRefresh(json(500, { error: 'workspace suspended' }))).toBe(false)
    expect(await isSuspendedRefresh(new Response('nope', { status: 401 }))).toBe(false)
  })
  it('sends the user to the login page with the suspended reason', () => {
    expect(SUSPENDED_LOGIN_PATH).toBe('/login?error=workspace_suspended')
  })
})
