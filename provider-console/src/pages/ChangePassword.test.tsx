import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { useSessionStore } from '@/auth/session'
import { mockFetch, superAdminMe } from '@/test/fetch'
import { renderApp, sessionStorageDump, signedInAs, storedToken } from '@/test/render'

const CURRENT = 'Temp-ABCdef234567'
const NEW = 'a brand new long passphrase'

async function fill(current: string, next: string, confirm = next) {
  const user = userEvent.setup()
  if (current) await user.type(screen.getByLabelText(/current password/i), current)
  if (next) await user.type(screen.getByLabelText('New password'), next)
  if (confirm) await user.type(screen.getByLabelText('Confirm new password'), confirm)
  return user
}

function save() {
  return screen.getByRole('button', { name: 'Save new password' })
}

describe('Change password', () => {
  it('anonymous users are sent to Login', () => {
    renderApp('/change-password')
    expect(screen.getByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
  })

  it('forced change: stores the new token and lands on Home', async () => {
    useSessionStore.getState().startSession('tok-pwc', 900, true)
    const { calls } = mockFetch({
      'POST /provider/auth/password': { status: 200, body: { token: 'tok-new', expires_in: 900 } },
      'GET /provider/me': { status: 200, body: superAdminMe },
    })
    renderApp('/change-password')
    expect(screen.getByRole('heading', { name: 'Set a new password' })).toBeInTheDocument()
    const user = await fill(CURRENT, NEW)
    await user.click(save())
    expect(await screen.findByRole('heading', { name: 'Your session' })).toBeInTheDocument()
    expect(calls[0]).toMatchObject({
      method: 'POST',
      path: '/provider/auth/password',
      body: { current_password: CURRENT, new_password: NEW },
    })
    expect(calls[0].headers.Authorization).toBe('Bearer tok-pwc')
    expect(storedToken()).toBe('tok-new')
    expect(sessionStorageDump()).not.toMatch(/Temp-|passphrase/)
  })

  it('client-side hints block a too-short password without calling the server', async () => {
    signedInAs(superAdminMe)
    const { calls } = mockFetch({})
    renderApp('/change-password')
    await fill(CURRENT, 'short')
    expect(screen.getByText('Use 12–128 characters.')).toBeInTheDocument()
    expect(save()).toBeDisabled()
    expect(calls).toHaveLength(0)
  })

  it('hints when the new password is the email address', async () => {
    signedInAs({ ...superAdminMe, email: 'long.admin.name@provider.test' })
    renderApp('/change-password')
    await fill(CURRENT, 'LONG.ADMIN.NAME@provider.test')
    expect(screen.getByText('Don’t use your email address.')).toBeInTheDocument()
    expect(save()).toBeDisabled()
  })

  it('a mismatched confirmation blocks submit', async () => {
    signedInAs(superAdminMe)
    renderApp('/change-password')
    await fill(CURRENT, NEW, NEW + 'x')
    expect(screen.getByText('Passwords don’t match.')).toBeInTheDocument()
    expect(save()).toBeDisabled()
  })

  it('a wrong current password is a form error; the user stays signed in', async () => {
    signedInAs(superAdminMe, 'tok-full')
    mockFetch({ 'POST /provider/auth/password': { status: 401, body: { error: 'invalid_credentials' } } })
    renderApp('/change-password')
    const user = await fill('not-the-password', NEW)
    await user.click(save())
    expect(await screen.findByText('Current password is incorrect.')).toBeInTheDocument()
    expect(screen.getByRole('heading', { name: 'Change password' })).toBeInTheDocument()
    expect(useSessionStore.getState()).toMatchObject({ status: 'authenticated', token: 'tok-full' })
    expect(storedToken()).toBe('tok-full')
    expect(screen.getByLabelText('Current password')).toHaveValue('')
    expect(screen.getByLabelText('New password')).toHaveValue('')
  })

  it('shows the server’s policy detail', async () => {
    signedInAs(superAdminMe)
    mockFetch({
      'POST /provider/auth/password': {
        status: 400,
        body: { error: 'password_policy', detail: 'password policy: must differ from the current password' },
      },
    })
    renderApp('/change-password')
    const user = await fill(CURRENT, NEW)
    await user.click(save())
    expect(await screen.findByText('New password rejected: must differ from the current password.')).toBeInTheDocument()
    expect(useSessionStore.getState().status).toBe('authenticated')
  })

  it('a revoked session during the change goes to Login', async () => {
    signedInAs(superAdminMe)
    mockFetch({ 'POST /provider/auth/password': { status: 401, body: { error: 'provider session revoked' } } })
    renderApp('/change-password')
    const user = await fill(CURRENT, NEW)
    await user.click(save())
    expect(await screen.findByText('Your session has ended. Sign in again.')).toBeInTheDocument()
  })

  it('voluntary change offers Cancel back to Home', async () => {
    signedInAs(superAdminMe)
    renderApp('/change-password')
    expect(screen.getByRole('heading', { name: 'Change password' })).toBeInTheDocument()
    await userEvent.setup().click(screen.getByRole('link', { name: 'Cancel' }))
    expect(screen.getByRole('heading', { name: 'Your session' })).toBeInTheDocument()
  })

  it('forced change offers Sign out', async () => {
    useSessionStore.getState().startSession('tok-pwc', 900, true)
    const { calls } = mockFetch({ 'POST /provider/auth/logout': { status: 204 } })
    renderApp('/change-password')
    await userEvent.setup().click(screen.getByRole('button', { name: 'Sign out' }))
    expect(await screen.findByText('You have signed out.')).toBeInTheDocument()
    expect(calls[0].headers.Authorization).toBe('Bearer tok-pwc')
    expect(sessionStorage.length).toBe(0)
  })
})
