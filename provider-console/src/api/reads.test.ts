import { describe, expect, it } from 'vitest'
import { useSessionStore } from '@/auth/session'
import { mockFetch, superAdminMe } from '@/test/fetch'
import { ApiError } from './client'
import {
  buildQuery,
  getCertificates,
  getRelay,
  getTenant,
  listRelays,
  listTenants,
  queryAudit,
  type CertificateParams,
} from './reads'
import type { CertificateExpiryResult, Page, RelayDetail, RelaySummary, TenantDetail } from './types'

const RELAY_ID = '11111111-2222-3333-4444-555555555555'
const TENANT_ID = '66666666-7777-8888-9999-000000000000'

const relay: RelaySummary = {
  id: RELAY_ID,
  name: 'relay-1',
  status: 'active',
  version: null,
  hostname: null,
  public_addr: '203.0.113.5:9093',
  address_scope: 'public',
  capacity_label: 'high',
  connection_count: 3,
  max_connections: 500,
  cert_serial: 'abc',
  cert_not_after: '2026-11-01T00:00:00Z',
  last_heartbeat_at: null,
  attached_connectors: 0,
  created_at: '2026-10-01T00:00:00Z',
}

function signedIn() {
  useSessionStore.getState().startSession('tok-full', 900, false)
  useSessionStore.getState().setMe(superAdminMe)
}

function params(search: string): Record<string, string> {
  return Object.fromEntries(new URLSearchParams(search))
}

describe('buildQuery', () => {
  it('serializes only allowlisted, non-empty values', () => {
    expect(buildQuery({ status: 'active', limit: 25, cursor: '' }, ['status', 'limit', 'cursor'])).toBe('?status=active&limit=25')
    expect(buildQuery({}, ['status'])).toBe('')
    expect(buildQuery(undefined, ['status'])).toBe('')
  })

  it('drops keys outside the allowlist even if present at runtime', () => {
    const sneaky = { status: 'active', token: 'tok-full', password: 'hunter2', details: '{"x":1}' }
    expect(buildQuery(sneaky, ['status', 'limit', 'cursor'])).toBe('?status=active')
  })

  it('skips null and non-scalar values', () => {
    expect(buildQuery({ action: null, target_id: { a: 1 }, limit: 5 }, ['action', 'target_id', 'limit'])).toBe('?limit=5')
  })

  it('encodes special characters', () => {
    const q = buildQuery({ provider_email: 'a+b@x.test', action: 'a&b=c' }, ['provider_email', 'action'])
    expect(params(q.slice(1))).toEqual({ provider_email: 'a+b@x.test', action: 'a&b=c' })
  })
})

describe('read wrappers', () => {
  it('listRelays: GET /provider/relays with status, limit and cursor', async () => {
    signedIn()
    const page: Page<RelaySummary> = { items: [relay], next_cursor: 'c2' }
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: page } })
    await expect(listRelays({ status: 'inactive', limit: 50, cursor: 'c1' })).resolves.toEqual(page)
    expect(calls[0]).toMatchObject({ method: 'GET', path: '/provider/relays', body: undefined })
    expect(params(calls[0].search)).toEqual({ status: 'inactive', limit: '50', cursor: 'c1' })
    expect(calls[0].headers.Authorization).toBe('Bearer tok-full')
  })

  it('listRelays with no params sends no query string', async () => {
    signedIn()
    const { calls } = mockFetch({ 'GET /provider/relays': { status: 200, body: { items: [], next_cursor: null } } })
    await listRelays()
    expect(calls[0].search).toBe('')
  })

  it('getRelay: escapes the id into the path and preserves nulls and []', async () => {
    signedIn()
    const detail: RelayDetail = {
      ...relay,
      dns_allowlist: [],
      ip_allowlist: [],
      certificates: [],
      certificates_truncated: false,
      attachments: [],
      attachments_truncated: false,
    }
    const { calls } = mockFetch({
      [`GET /provider/relays/${RELAY_ID}`]: { status: 200, body: detail },
      'GET /provider/relays/a%2F..%2Fx': { status: 404, body: { error: 'not_found' } },
    })
    const got = await getRelay(RELAY_ID)
    expect(got).toEqual(detail)
    expect(got.version).toBeNull()
    expect(got.dns_allowlist).toEqual([])
    await expect(getRelay('a/../x')).rejects.toMatchObject({ status: 404, code: 'not_found' })
    expect(calls[1].path).toBe('/provider/relays/a%2F..%2Fx')
  })

  it('listTenants: GET /provider/tenants with filters', async () => {
    signedIn()
    const { calls } = mockFetch({ 'GET /provider/tenants': { status: 200, body: { items: [], next_cursor: null } } })
    await listTenants({ status: 'deleted', limit: 10 })
    expect(params(calls[0].search)).toEqual({ status: 'deleted', limit: '10' })
  })

  it('getTenant: one GET to /provider/tenants/{id}', async () => {
    signedIn()
    const detail = { id: TENANT_ID, remote_networks: [], connectors: [], shields: [] } as unknown as TenantDetail
    const { calls } = mockFetch({ [`GET /provider/tenants/${TENANT_ID}`]: { status: 200, body: detail } })
    await expect(getTenant(TENANT_ID)).resolves.toEqual(detail)
    expect(calls).toHaveLength(1)
    expect(calls[0]).toMatchObject({ method: 'GET', search: '' })
  })

  it('queryAudit: every filter is serialized exactly', async () => {
    signedIn()
    const { calls } = mockFetch({ 'GET /provider/audit': { status: 200, body: { items: [], next_cursor: null } } })
    await queryAudit({
      action: 'tenant.read',
      target_type: 'tenant',
      target_id: TENANT_ID,
      provider_email: 'Admin+ops@provider.test',
      since: '2026-10-01T00:00:00Z',
      until: '2026-10-02T00:00:00Z',
      limit: 100,
      cursor: 'abc',
    })
    expect(params(calls[0].search)).toEqual({
      action: 'tenant.read',
      target_type: 'tenant',
      target_id: TENANT_ID,
      provider_email: 'Admin+ops@provider.test',
      since: '2026-10-01T00:00:00Z',
      until: '2026-10-02T00:00:00Z',
      limit: '100',
      cursor: 'abc',
    })
  })

  it('getCertificates: within, kind, tenant_id, limit, cursor', async () => {
    signedIn()
    const result: CertificateExpiryResult = {
      summary: { expired: 0, lt_24h: 1, lt_7d: 0, lt_30d: 0, ok: 2 },
      items: [],
      next_cursor: null,
    }
    const { calls } = mockFetch({ 'GET /provider/certificates': { status: 200, body: result } })
    const p: CertificateParams = { within: '48h', kind: 'relay', tenant_id: TENANT_ID, limit: 20, cursor: 'x' }
    await expect(getCertificates(p)).resolves.toEqual(result)
    expect(params(calls[0].search)).toEqual({ within: '48h', kind: 'relay', tenant_id: TENANT_ID, limit: '20', cursor: 'x' })
  })

  it('never puts the session token in a URL', async () => {
    signedIn()
    const { calls } = mockFetch({
      'GET /provider/relays': { status: 200, body: { items: [], next_cursor: null } },
      'GET /provider/audit': { status: 200, body: { items: [], next_cursor: null } },
    })
    await listRelays({ status: 'active', ...({ token: 'tok-full' } as object) })
    await queryAudit({ action: 'x', ...({ access_token: 'tok-full' } as object) })
    for (const c of calls) {
      expect(c.search).not.toContain('tok-full')
      expect(c.path).not.toContain('tok-full')
    }
  })
})

describe('read wrappers: error handling (C-a client, unchanged)', () => {
  it('400 codes surface as ApiError and keep the session', async () => {
    signedIn()
    mockFetch({ 'GET /provider/relays': { status: 400, body: { error: 'invalid_cursor' } } })
    const err = await listRelays({ cursor: 'bad' }).catch((e: unknown) => e)
    expect(err).toBeInstanceOf(ApiError)
    expect(err).toMatchObject({ status: 400, code: 'invalid_cursor', sessionEnded: false })
    expect(useSessionStore.getState().status).toBe('authenticated')
  })

  it('a role 403 keeps the session (the page shows Forbidden)', async () => {
    signedIn()
    mockFetch({ 'GET /provider/tenants': { status: 403, body: { error: 'forbidden' } } })
    await expect(listTenants()).rejects.toMatchObject({ status: 403, code: 'forbidden', sessionEnded: false })
    expect(useSessionStore.getState().status).toBe('authenticated')
  })

  it('a 500 from tenant detail (audit failure) is an ApiError; the session stays', async () => {
    signedIn()
    mockFetch({ [`GET /provider/tenants/${TENANT_ID}`]: { status: 500, body: { error: 'server_error' } } })
    await expect(getTenant(TENANT_ID)).rejects.toMatchObject({ status: 500, code: 'server_error', sessionEnded: false })
  })

  it('a revoked session ends the session', async () => {
    signedIn()
    mockFetch({ 'GET /provider/certificates': { status: 401, body: { error: 'provider session revoked' } } })
    await expect(getCertificates()).rejects.toMatchObject({ sessionEnded: true })
    expect(useSessionStore.getState()).toMatchObject({ status: 'anonymous', endReason: 'session_ended' })
  })

  it('a disabled account ends the session', async () => {
    signedIn()
    mockFetch({ 'GET /provider/audit': { status: 403, body: { error: 'not a provider user' } } })
    await expect(queryAudit()).rejects.toMatchObject({ sessionEnded: true })
    expect(useSessionStore.getState().endReason).toBe('disabled')
  })
})
