import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Observable } from 'rxjs'
import { ApolloClient, ApolloLink, InMemoryCache } from '@apollo/client/core'
import { ServerError } from '@apollo/client/errors'
import { gql } from '@apollo/client'
import { useAuthStore } from '@/store/auth'
import { errorLink } from './error'

// Sprint 23 S1 (D-45): a request refused because the provider suspended the
// workspace ends the local session and lands on the login page with the
// suspended message; no refresh, no retry. A refresh refused for the same
// reason does the same.

const QUERY = gql`query Probe { me { id } }`
const client = new ApolloClient({ link: ApolloLink.empty(), cache: new InMemoryCache() })
const SUSPENDED_403 = JSON.stringify({
  errors: [{ message: 'this workspace is suspended by the provider', extensions: { code: 'TENANT_SUSPENDED' } }],
})

function failing(error: unknown, attempts: { n: number }) {
  return new ApolloLink(() => {
    attempts.n++
    return new Observable((o) => o.error(error))
  })
}

function run(link: ApolloLink) {
  return new Promise<{ error?: unknown; done?: boolean }>((resolve) => {
    ApolloLink.execute(link, { query: QUERY }, { client }).subscribe({
      next: () => {},
      error: (error) => resolve({ error }),
      complete: () => resolve({ done: true }),
    })
  })
}

describe('errorLink: provider suspension', () => {
  let href = ''
  beforeEach(() => {
    href = ''
    vi.stubGlobal('location', {
      get href() {
        return href
      },
      set href(v: string) {
        href = v
      },
    })
    useAuthStore.setState({ accessToken: 'tok', isRefreshing: false })
  })
  afterEach(() => {
    vi.unstubAllGlobals()
    vi.restoreAllMocks()
  })

  it('a 403 TENANT_SUSPENDED clears the session and goes to the suspended login page, without refreshing', async () => {
    const fetchSpy = vi.spyOn(globalThis, 'fetch')
    const attempts = { n: 0 }
    const err = new ServerError('refused', { response: new Response(SUSPENDED_403, { status: 403 }), bodyText: SUSPENDED_403 })
    const res = await run(ApolloLink.from([errorLink, failing(err, attempts)]))
    expect(res.error).toBe(err)
    expect(attempts.n).toBe(1)
    expect(fetchSpy).not.toHaveBeenCalled()
    expect(useAuthStore.getState().accessToken).toBeNull()
    expect(href).toBe('/login?error=workspace_suspended')
  })

  it('a refresh refused as "workspace suspended" goes to the suspended login page', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({ error: 'workspace suspended' }), { status: 401 }))
    const attempts = { n: 0 }
    const err = new ServerError('expired', { response: new Response('{}', { status: 401 }), bodyText: '{}' })
    await run(ApolloLink.from([errorLink, failing(err, attempts)]))
    expect(useAuthStore.getState().accessToken).toBeNull()
    expect(href).toBe('/login?error=workspace_suspended')
  })

  it('any other refused refresh still goes to the plain login page', async () => {
    vi.spyOn(globalThis, 'fetch').mockResolvedValue(new Response(JSON.stringify({ error: 'session revoked' }), { status: 401 }))
    const attempts = { n: 0 }
    const err = new ServerError('expired', { response: new Response('{}', { status: 401 }), bodyText: '{}' })
    await run(ApolloLink.from([errorLink, failing(err, attempts)]))
    expect(href).toBe('/login')
  })
})
