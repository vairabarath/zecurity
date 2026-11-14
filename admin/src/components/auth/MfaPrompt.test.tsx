import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { act, render, screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { useAuthStore } from '@/store/auth'
import { useMfaPrompt } from '@/store/mfaPrompt'

vi.mock('@/apollo/client', () => ({ apolloClient: { clearStore: vi.fn().mockResolvedValue(undefined) } }))

const { MfaPrompt } = await import('./MfaPrompt')

function json(status: number, body: unknown) {
  return new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } })
}

// Routes the prompt's REST calls to per-test handlers.
function stubApi(routes: Record<string, () => Response>) {
  const calls: string[] = []
  vi.stubGlobal('fetch', vi.fn(async (url: string) => {
    calls.push(url)
    const route = routes[url]
    if (!route) throw new Error(`unexpected ${url}`)
    return route()
  }))
  return calls
}

function open(reason: 'console' | 'step-up' | 'self' = 'step-up') {
  let answer!: Promise<boolean>
  act(() => {
    answer = useMfaPrompt.getState().requestMfa(reason)
  })
  return answer
}

describe('MfaPrompt (PENDING-06)', () => {
  beforeEach(() => useAuthStore.setState({ accessToken: 'plain' }))
  afterEach(() => {
    vi.unstubAllGlobals()
    act(() => useMfaPrompt.setState({ open: false, pending: null, resolve: null }))
  })

  it('enrolled: verifies a code, stores the marked token and answers true', async () => {
    stubApi({
      '/auth/mfa/status': () => json(200, { enrolled: true, consoleRequireMfa: false, sessionMfaAt: null }),
      '/auth/mfa/verify': () => json(200, { accessToken: 'marked', expiresIn: 900, enrolled: false }),
    })
    render(<MfaPrompt />)
    const answer = open()
    await userEvent.type(await screen.findByLabelText('Authentication code'), '123456')
    await userEvent.click(screen.getByRole('button', { name: 'Verify' }))
    await expect(answer).resolves.toBe(true)
    expect(useAuthStore.getState().accessToken).toBe('marked')
  })

  it('wrong code: says so and stays open', async () => {
    stubApi({
      '/auth/mfa/status': () => json(200, { enrolled: true, consoleRequireMfa: false, sessionMfaAt: null }),
      '/auth/mfa/verify': () => json(401, { code: 'INVALID_CODE', error: 'that code is not valid' }),
    })
    render(<MfaPrompt />)
    open()
    await userEvent.type(await screen.findByLabelText('Authentication code'), '000000')
    await userEvent.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByText(/That code is not right/)).toBeInTheDocument()
    expect(useMfaPrompt.getState().open).toBe(true)
  })

  it('not enrolled: shows the QR code and setup key, enrolment requested once', async () => {
    const calls = stubApi({
      '/auth/mfa/status': () => json(200, { enrolled: false, consoleRequireMfa: true, sessionMfaAt: null }),
      '/auth/mfa/enroll': () => json(200, { secret: 'JBSWY3DPEHPK3PXP', uri: 'otpauth://totp/Zecurity:a@x?secret=JBSWY3DPEHPK3PXP' }),
    })
    render(<MfaPrompt />)
    open('console')
    expect(await screen.findByLabelText('Setup key')).toHaveTextContent('JBSW Y3DP EHPK 3PXP')
    expect(screen.getByRole('img', { name: /QR code/ })).toBeInTheDocument()
    expect(screen.getByRole('button', { name: 'Confirm and continue' })).toBeInTheDocument()
    expect(calls.filter((c) => c === '/auth/mfa/enroll')).toHaveLength(1)
  })

  it('console requirement offers Sign out; a step-up offers Cancel, which answers false', async () => {
    stubApi({
      '/auth/mfa/status': () => json(200, { enrolled: true, consoleRequireMfa: true, sessionMfaAt: null }),
    })
    const { unmount } = render(<MfaPrompt />)
    open('console')
    expect(await screen.findByRole('button', { name: 'Sign out' })).toBeInTheDocument()
    act(() => useMfaPrompt.getState().finish(false))
    unmount()

    render(<MfaPrompt />)
    const answer = open('step-up')
    await userEvent.click(await screen.findByRole('button', { name: 'Cancel' }))
    await expect(answer).resolves.toBe(false)
    await waitFor(() => expect(screen.queryByLabelText('Authentication code')).toBeNull())
  })
})
