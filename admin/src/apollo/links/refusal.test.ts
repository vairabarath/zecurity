import { beforeEach, describe, expect, it, vi } from 'vitest'
import { CombinedGraphQLErrors } from '@apollo/client/errors'
import { Capability, Role } from '@/generated/graphql'
import { useAuthStore } from '@/store/auth'
import { setTestRole } from '@/test/roles'

const toastError = vi.fn()
vi.mock('sonner', () => ({ toast: { error: (...a: unknown[]) => toastError(...a) } }))

const { handleCapabilityRefusal, isCapabilityRefusal } = await import('./refusal')

function gqlErrors(...messages: string[]) {
  return new CombinedGraphQLErrors({ errors: messages.map((message) => ({ message })) })
}

describe('capability refusal handling (PENDING-19)', () => {
  beforeEach(() => {
    toastError.mockReset()
    useAuthStore.setState({ accessToken: 'tok' })
  })

  it('recognises only @requires refusals', () => {
    expect(isCapabilityRefusal(gqlErrors('forbidden: requires capability NETWORK_WRITE'))).toBe(true)
    expect(isCapabilityRefusal(gqlErrors('only an Admin can change their own group membership'))).toBe(false)
    expect(isCapabilityRefusal(gqlErrors('internal system error'))).toBe(false)
    expect(isCapabilityRefusal(new Error('forbidden: requires capability X'))).toBe(false)
  })

  it('shows a toast and re-reads me, so the controls follow the live role', async () => {
    setTestRole(Role.Devops)
    const fetchMock = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        data: { me: { __typename: 'User', id: 'u', email: 'd@x', role: Role.Viewer, provider: 'google', createdAt: '', capabilities: [Capability.ConsoleRead] } },
      }),
    })
    vi.stubGlobal('fetch', fetchMock)
    await handleCapabilityRefusal()
    expect(toastError).toHaveBeenCalledWith("You don't have permission for this action", expect.objectContaining({ id: 'capability-refused' }))
    const [url, init] = fetchMock.mock.calls[0]
    expect(url).toBe('/graphql')
    expect(init.headers.Authorization).toBe('Bearer tok')
    expect(JSON.parse(init.body).query).toContain('capabilities')
    expect(useAuthStore.getState().user?.role).toBe(Role.Viewer)
    expect(useAuthStore.getState().user?.capabilities).toEqual([Capability.ConsoleRead])
  })

  it('makes one me request for a burst of refusals', async () => {
    let resolve!: (v: unknown) => void
    const fetchMock = vi.fn().mockReturnValue(new Promise((r) => { resolve = r }))
    vi.stubGlobal('fetch', fetchMock)
    const a = handleCapabilityRefusal()
    const b = handleCapabilityRefusal()
    resolve({ ok: false, json: async () => ({}) })
    await Promise.all([a, b])
    expect(fetchMock).toHaveBeenCalledTimes(1)
    expect(toastError).toHaveBeenCalledTimes(2) // same toast id: sonner shows one
  })

  it('keeps the user when the lookup fails', async () => {
    setTestRole(Role.Devops)
    vi.stubGlobal('fetch', vi.fn().mockRejectedValue(new Error('down')))
    await handleCapabilityRefusal()
    expect(useAuthStore.getState().user?.role).toBe(Role.Devops)
  })
})
