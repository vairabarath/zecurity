import { vi } from 'vitest'
import type { Me } from '@/api/types'

// A fetch stub for tests: routes are matched on "METHOD /path" and every call
// is recorded with its headers and parsed body.

export interface FakeResponse {
  status: number
  body?: unknown
  headers?: Record<string, string>
}

export interface RecordedCall {
  method: string
  path: string
  /** Raw query string without "?" ("" when none). */
  search: string
  headers: Record<string, string>
  body?: unknown
}

type Route = FakeResponse | ((call: RecordedCall) => FakeResponse) | 'network-error'

export function mockFetch(routes: Record<string, Route>) {
  const calls: RecordedCall[] = []
  const fn = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), 'http://console.test')
    const call: RecordedCall = {
      method: init?.method ?? 'GET',
      path: url.pathname,
      search: url.search.replace(/^\?/, ''),
      headers: { ...(init?.headers as Record<string, string> | undefined) },
      body: typeof init?.body === 'string' ? JSON.parse(init.body) : undefined,
    }
    calls.push(call)
    const route = routes[`${call.method} ${call.path}`]
    if (route === undefined) throw new Error(`unexpected request ${call.method} ${call.path}`)
    if (route === 'network-error') throw new TypeError('Failed to fetch')
    const r = typeof route === 'function' ? route(call) : route
    const body = r.status === 204 || r.body === undefined ? null : JSON.stringify(r.body)
    return new Response(body, {
      status: r.status,
      headers: { 'Content-Type': 'application/json', ...r.headers },
    })
  })
  vi.stubGlobal('fetch', fn)
  return { fn, calls }
}

export const superAdminMe: Me = {
  user_id: '11111111-1111-1111-1111-111111111111',
  email: 'admin@provider.test',
  role: 'super-admin',
  last_login_at: '2026-10-02T09:00:00Z',
}

export const relayOpsMe: Me = {
  user_id: '22222222-2222-2222-2222-222222222222',
  email: 'ops@provider.test',
  role: 'relay-ops',
  last_login_at: '2026-10-02T09:05:00Z',
}

/** The exact 401/403 bodies RequireProvider writes (controller/internal/middleware/provider.go). */
export const middlewareSessionEnding401 = [
  'missing Authorization header',
  'malformed Authorization header',
  'invalid or expired provider token',
  'provider session revoked',
] as const
