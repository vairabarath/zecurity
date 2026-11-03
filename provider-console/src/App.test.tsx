import { act, screen, within } from '@testing-library/react'
import { describe, expect, it, vi } from 'vitest'
import { getMe } from '@/api/auth'
import { request } from '@/api/client'
import { rehydrate } from '@/auth/flows'
import { SESSION_STORAGE_KEY, useSessionStore } from '@/auth/session'
import { formatDateTime } from '@/lib/format'
import { mockFetch, relayOpsMe, superAdminMe } from '@/test/fetch'
import { renderApp, signedInAs } from '@/test/render'

function storeSession(token: string, pwcOnly = false) {
  sessionStorage.setItem(SESSION_STORAGE_KEY, JSON.stringify({ token, expiresAt: Date.now() + 600_000, pwcOnly }))
}

describe('application wiring', () => {
  it('unknown routes show Not found', () => {
    renderApp('/does-not-exist')
    expect(screen.getByRole('heading', { name: 'Page not found' })).toBeInTheDocument()
    expect(screen.getByRole('link', { name: /back to the provider console/i })).toHaveAttribute('href', '/')
  })

  it('Home shows the identity, role, sign-in time and expiry', () => {
    signedInAs(relayOpsMe)
    renderApp('/')
    const card = screen.getByRole('region', { name: 'Your session' })
    expect(within(card).getByText(relayOpsMe.email)).toBeInTheDocument()
    expect(within(card).getByText('Relay ops')).toBeInTheDocument()
    expect(within(card).getByText('Signed in at')).toBeInTheDocument()
    expect(within(card).getByText(formatDateTime(relayOpsMe.last_login_at))).toBeInTheDocument()
    expect(within(card).getByText(formatDateTime(useSessionStore.getState().expiresAt))).toBeInTheDocument()
  })

  it('Forbidden keeps the session and the shell', () => {
    signedInAs(relayOpsMe)
    renderApp('/forbidden')
    expect(screen.getByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    expect(screen.getByRole('navigation', { name: 'Sections' })).toBeInTheDocument()
    expect(useSessionStore.getState().status).toBe('authenticated')
  })
})

describe('refresh (rehydrate)', () => {
  it('shows a loading state, then Home once /provider/me accepts the stored token', async () => {
    storeSession('tok-stored')
    let release!: () => void
    const gate = new Promise<void>((r) => (release = r))
    const { calls } = mockFetch({ 'GET /provider/me': { status: 200, body: superAdminMe } })
    const fetchMock = vi.mocked(fetch)
    const real = fetchMock.getMockImplementation()!
    fetchMock.mockImplementation(async (...args) => {
      await gate
      return real(...args)
    })

    const pending = rehydrate()
    renderApp('/')
    expect(screen.getByLabelText('Loading session')).toBeInTheDocument()
    expect(screen.queryByRole('heading', { name: 'Sign in' })).not.toBeInTheDocument()
    await act(async () => {
      release()
      await pending
    })
    expect(screen.getByRole('heading', { name: 'Your session' })).toBeInTheDocument()
    expect(calls[0].headers.Authorization).toBe('Bearer tok-stored')
  })

  it.each([
    ['revoked', { status: 401, body: { error: 'provider session revoked' } }, 'Your session has ended. Sign in again.'],
    ['disabled', { status: 403, body: { error: 'not a provider user' } }, 'This account is disabled. Contact a super-admin.'],
    ['unconfirmed', { status: 500, body: { error: 'provider lookup failed' } }, 'Your session could not be verified. Sign in again.'],
  ])('a %s stored session lands on Login with a notice', async (_name, response, notice) => {
    storeSession('tok-stored')
    mockFetch({ 'GET /provider/me': response })
    await rehydrate()
    renderApp('/')
    expect(screen.getByText(notice)).toBeInTheDocument()
    expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).toBeNull()
  })

  it('a stored password-change-only session lands on Change password', async () => {
    storeSession('tok-pwc', true)
    mockFetch({ 'GET /provider/me': { status: 403, body: { error: 'password_change_required' } } })
    await rehydrate()
    renderApp('/')
    expect(screen.getByRole('heading', { name: 'Set a new password' })).toBeInTheDocument()
  })
})

describe('responses during a session', () => {
  it('a session-ending 401 clears the session and shows Login', async () => {
    signedInAs(superAdminMe)
    renderApp('/')
    mockFetch({ 'GET /provider/me': { status: 401, body: { error: 'provider session revoked' } } })
    await act(async () => {
      await getMe().catch(() => undefined)
    })
    expect(screen.getByText('Your session has ended. Sign in again.')).toBeInTheDocument()
    expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).toBeNull()
  })

  it('a disabled-account 403 ends the session', async () => {
    signedInAs(superAdminMe)
    renderApp('/')
    mockFetch({ 'GET /provider/me': { status: 403, body: { error: 'not a provider user' } } })
    await act(async () => {
      await getMe().catch(() => undefined)
    })
    expect(screen.getByText('This account is disabled. Contact a super-admin.')).toBeInTheDocument()
  })

  it('a password_change_required 403 routes to Change password', async () => {
    signedInAs(superAdminMe)
    renderApp('/')
    mockFetch({ 'GET /provider/me': { status: 403, body: { error: 'password_change_required' } } })
    await act(async () => {
      await getMe().catch(() => undefined)
    })
    expect(screen.getByRole('heading', { name: 'Set a new password' })).toBeInTheDocument()
  })

  it('any other 403 leaves the session and the page alone', async () => {
    signedInAs(superAdminMe, 'tok-full')
    renderApp('/')
    mockFetch({ 'GET /provider/users': { status: 403, body: { error: 'forbidden' } } })
    await act(async () => {
      await request('GET', '/provider/users').catch(() => undefined)
    })
    expect(screen.getByRole('heading', { name: 'Your session' })).toBeInTheDocument()
    expect(useSessionStore.getState()).toMatchObject({ status: 'authenticated', token: 'tok-full' })
  })

  it('at expiry the user is returned to Login', () => {
    vi.useFakeTimers()
    signedInAs(superAdminMe)
    renderApp('/')
    expect(screen.getByRole('heading', { name: 'Your session' })).toBeInTheDocument()
    act(() => {
      vi.advanceTimersByTime(900_000)
    })
    expect(screen.getByText('Your session expired. Sign in again.')).toBeInTheDocument()
    expect(sessionStorage.getItem(SESSION_STORAGE_KEY)).toBeNull()
  })
})
