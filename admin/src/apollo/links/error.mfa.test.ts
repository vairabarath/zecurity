import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { Observable } from 'rxjs'
import { ApolloClient, ApolloLink, InMemoryCache } from '@apollo/client/core'
import { CombinedGraphQLErrors, ServerError } from '@apollo/client/errors'
import { gql } from '@apollo/client'
import { useAuthStore } from '@/store/auth'
import { useMfaPrompt } from '@/store/mfaPrompt'
import { errorLink } from './error'

// PENDING-06: the error link answers MFA_REQUIRED with the MFA prompt and
// sends the operation once more with the new token.

const QUERY = gql`query Probe { me { id } }`
const BODY_403 = JSON.stringify({ errors: [{ message: 'mfa', extensions: { code: 'MFA_REQUIRED' } }] })
const client = new ApolloClient({ link: ApolloLink.empty(), cache: new InMemoryCache() })

type Reply = { error: unknown } | { data: unknown }

// A terminating link that answers each attempt from `replies` and records the
// Authorization header it was sent with.
function terminal(replies: Reply[], seen: string[]) {
  return new ApolloLink((operation) => {
    seen.push((operation.getContext().headers?.Authorization as string | undefined) ?? '')
    const reply = replies.shift()!
    return new Observable((o) => {
      if ('error' in reply) o.error(reply.error)
      else {
        o.next({ data: reply.data } as ApolloLink.Result)
        o.complete()
      }
    })
  })
}

// Mirrors authLink: reads the token from the store on every request.
const auth = new ApolloLink((operation, forward) => {
  operation.setContext({ headers: { Authorization: `Bearer ${useAuthStore.getState().accessToken}` } })
  return forward(operation)
})

function run(replies: Reply[], seen: string[]) {
  const link = ApolloLink.from([errorLink, auth, terminal(replies, seen)])
  return new Promise<{ data?: unknown; error?: unknown }>((resolve) => {
    ApolloLink.execute(link, { query: QUERY }, { client }).subscribe({
      next: (r) => resolve({ data: r.data }),
      error: (error) => resolve({ error }),
    })
  })
}

// Answers the prompt the moment it opens: verifying swaps the token.
function answerPrompt(ok: boolean) {
  return useMfaPrompt.subscribe((s) => {
    if (s.open) {
      if (ok) useAuthStore.setState({ accessToken: 'marked' })
      queueMicrotask(() => useMfaPrompt.getState().finish(ok))
    }
  })
}

describe('errorLink MFA_REQUIRED (PENDING-06)', () => {
  let unsub: () => void = () => {}
  beforeEach(() => useAuthStore.setState({ accessToken: 'plain' }))
  afterEach(() => {
    unsub()
    useMfaPrompt.setState({ open: false, pending: null, resolve: null })
  })

  it('console-wide 403: prompts, then retries with the new token', async () => {
    unsub = answerPrompt(true)
    const seen: string[] = []
    const err = new ServerError('forbidden', { response: new Response(BODY_403, { status: 403 }), bodyText: BODY_403 })
    const out = await run([{ error: err }, { data: { me: { id: 'u' } } }], seen)
    expect(out.data).toEqual({ me: { id: 'u' } })
    expect(seen).toEqual(['Bearer plain', 'Bearer marked'])
    expect(useMfaPrompt.getState().reason).toBe('console')
  })

  it('break-glass GraphQL error: prompts as a step-up', async () => {
    unsub = answerPrompt(true)
    const seen: string[] = []
    const err = new CombinedGraphQLErrors({ errors: [{ message: 'fresh factor', extensions: { code: 'MFA_REQUIRED' } }] })
    const out = await run([{ error: err }, { data: { ok: true } }], seen)
    expect(out.data).toEqual({ ok: true })
    expect(useMfaPrompt.getState().reason).toBe('step-up')
  })

  it('closing the prompt passes the original error on, with no retry', async () => {
    unsub = answerPrompt(false)
    const seen: string[] = []
    const err = new CombinedGraphQLErrors({ errors: [{ message: 'fresh factor', extensions: { code: 'MFA_REQUIRED' } }] })
    const out = await run([{ error: err }, { data: { ok: true } }], seen)
    expect(out.error).toBe(err)
    expect(seen).toHaveLength(1)
  })

  it('the retry is not prompted again', async () => {
    const opened = vi.fn()
    unsub = useMfaPrompt.subscribe((s) => {
      if (s.open) {
        opened()
        queueMicrotask(() => useMfaPrompt.getState().finish(true))
      }
    })
    const seen: string[] = []
    const err = () => new CombinedGraphQLErrors({ errors: [{ message: 'x', extensions: { code: 'MFA_REQUIRED' } }] })
    const out = await run([{ error: err() }, { error: err() }], seen)
    expect(out.error).toBeInstanceOf(CombinedGraphQLErrors)
    expect(opened).toHaveBeenCalledTimes(1)
    expect(seen).toHaveLength(2)
  })

  it('expired token → refresh → MFA refusal: prompts instead of signing out', async () => {
    vi.stubGlobal('fetch', vi.fn().mockResolvedValue(
      new Response(JSON.stringify({ access_token: 'fresh' }), { status: 200 }),
    ))
    unsub = answerPrompt(true)
    const seen: string[] = []
    const expired = new ServerError('unauthorized', { response: new Response('{}', { status: 401 }), bodyText: '{}' })
    const mfa = new ServerError('forbidden', { response: new Response(BODY_403, { status: 403 }), bodyText: BODY_403 })
    const out = await run([{ error: expired }, { error: mfa }, { data: { me: { id: 'u' } } }], seen)
    vi.unstubAllGlobals()
    expect(out.data).toEqual({ me: { id: 'u' } })
    expect(seen).toEqual(['Bearer plain', 'Bearer fresh', 'Bearer marked'])
  })

  it('concurrent refusals share one prompt', async () => {
    const a = useMfaPrompt.getState().requestMfa('console')
    const b = useMfaPrompt.getState().requestMfa('console')
    expect(a).toBe(b)
    useMfaPrompt.getState().finish(true)
    await expect(Promise.all([a, b])).resolves.toEqual([true, true])
  })
})
