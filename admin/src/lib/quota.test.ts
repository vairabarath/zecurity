import { describe, expect, it } from 'vitest'
import { CombinedGraphQLErrors, ServerError } from '@apollo/client/errors'
import { quotaExceeded, quotaExceededMessage } from './quota'

// Sprint 23 S3 (D-47): the console reads quota_exceeded {key, limit, used}
// from a GraphQL error extension or a REST 409 body.

describe('quotaExceeded', () => {
  it('reads the GraphQL extension', () => {
    const err = new CombinedGraphQLErrors({
      errors: [{ message: 'quota exceeded', extensions: { code: 'QUOTA_EXCEEDED', key: 'connectors', limit: 10, used: 10 } }],
    })
    expect(quotaExceeded(err)).toEqual({ key: 'connectors', limit: 10, used: 10 })
  })
  it('reads the REST 409 body', () => {
    const body = JSON.stringify({ error: 'quota_exceeded', key: 'users', limit: 5, used: 5 })
    const err = new ServerError('conflict', { response: new Response(body, { status: 409 }), bodyText: body })
    expect(quotaExceeded(err)).toEqual({ key: 'users', limit: 5, used: 5 })
  })
  it('ignores everything else', () => {
    expect(quotaExceeded(new CombinedGraphQLErrors({ errors: [{ message: 'x', extensions: { code: 'FORBIDDEN' } }] }))).toBeNull()
    expect(quotaExceeded(new ServerError('x', { response: new Response('nope', { status: 409 }), bodyText: 'nope' }))).toBeNull()
    expect(quotaExceeded(new Error('QUOTA_EXCEEDED'))).toBeNull()
  })
  it('says which limit was reached and what to do', () => {
    expect(quotaExceededMessage({ key: 'remote_networks', limit: 3, used: 3 })).toBe(
      'Remote networks limit reached (3 of 3 in use). Remove some, or ask your provider to raise the limit.',
    )
  })
})
