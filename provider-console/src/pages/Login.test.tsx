import { screen } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { useSessionStore } from '@/auth/session'
import { mockFetch, superAdminMe, type FakeResponse } from '@/test/fetch'
import { renderApp, sessionStorageDump, storedToken } from '@/test/render'

const PASSWORD = 'correct horse battery staple'

const loginOk = (pwc = false): FakeResponse => ({
  status: 200,
  body: { token: pwc ? 'tok-pwc' : 'tok-full', expires_in: 900, password_change_required: pwc },
})

async function submitLogin(email = 'admin@provider.test', password = PASSWORD) {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Email'), email)
  await user.type(screen.getByLabelText('Password'), password)
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  return user
}

describe('Login', () => {
  it('an anonymous visit to / shows Login', () => {
    renderApp('/')
    expect(screen.getByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
  })

  it('signs in and lands on Home', async () => {
    const { calls } = mockFetch({
      'POST /provider/auth/login': loginOk(),
      'GET /provider/me': { status: 200, body: superAdminMe },
    })
    renderApp('/login')
    await submitLogin()
    expect(await screen.findByRole('heading', { name: 'Your session' })).toBeInTheDocument()
    expect(calls[0].body).toEqual({ email: 'admin@provider.test', password: PASSWORD })
    expect(storedToken()).toBe('tok-full')
    expect(sessionStorageDump()).not.toContain(PASSWORD)
    expect(localStorage.length).toBe(0)
  })

  it('returns to the page the user was heading for', async () => {
    mockFetch({
      'POST /provider/auth/login': loginOk(),
      'GET /provider/me': { status: 200, body: superAdminMe },
    })
    renderApp('/forbidden')
    expect(screen.getByRole('heading', { name: 'Sign in' })).toBeInTheDocument()
    await submitLogin()
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
  })

  it('a must-change login goes to Change password and nothing else is reachable', async () => {
    const { calls } = mockFetch({ 'POST /provider/auth/login': loginOk(true) })
    const view = renderApp('/login')
    await submitLogin()
    expect(await screen.findByRole('heading', { name: 'Set a new password' })).toBeInTheDocument()
    expect(calls).toHaveLength(1) // no /provider/me with a password-change-only token
    view.unmount()
    renderApp('/')
    expect(screen.getByRole('heading', { name: 'Set a new password' })).toBeInTheDocument()
  })

  it('wrong credentials show one generic message and keep the user on Login', async () => {
    mockFetch({ 'POST /provider/auth/login': { status: 401, body: { error: 'invalid_credentials' } } })
    renderApp('/login')
    await submitLogin('nobody@provider.test', 'wrong password')
    expect(await screen.findByText('Email or password is incorrect.')).toBeInTheDocument()
    expect(screen.getByLabelText('Password')).toHaveValue('')
    expect(screen.getByLabelText('Email')).toHaveValue('nobody@provider.test')
    expect(useSessionStore.getState().status).toBe('anonymous')
    expect(sessionStorage.length).toBe(0)
  })

  it.each([
    [540, 'Too many attempts. Try again in 9 minutes.'],
    [30, 'Too many attempts. Try again in 1 minute.'],
  ])('429 with Retry-After %i says "%s"', async (seconds, message) => {
    mockFetch({
      'POST /provider/auth/login': {
        status: 429,
        body: { error: 'too_many_attempts' },
        headers: { 'Retry-After': String(seconds) },
      },
    })
    renderApp('/login')
    await submitLogin()
    expect(await screen.findByText(message)).toBeInTheDocument()
  })

  it('503 says sign-in is unavailable', async () => {
    mockFetch({ 'POST /provider/auth/login': { status: 503, body: { error: 'login_unavailable' } } })
    renderApp('/login')
    await submitLogin()
    expect(await screen.findByText(/Sign-in is unavailable right now/)).toBeInTheDocument()
  })

  it('fails closed when /provider/me errors after a successful login', async () => {
    mockFetch({
      'POST /provider/auth/login': loginOk(),
      'GET /provider/me': { status: 500, body: { error: 'provider lookup failed' } },
    })
    renderApp('/login')
    await submitLogin()
    expect(await screen.findByText('Your session could not be verified. Sign in again.')).toBeInTheDocument()
    expect(useSessionStore.getState().status).toBe('anonymous')
    expect(sessionStorage.length).toBe(0)
  })

  it.each([
    ['logged_out', 'You have signed out.'],
    ['expired', 'Your session expired. Sign in again.'],
    ['session_ended', 'Your session has ended. Sign in again.'],
    ['disabled', 'This account is disabled. Contact a super-admin.'],
  ] as const)('shows the %s notice', (reason, text) => {
    useSessionStore.getState().endSession(reason)
    renderApp('/login')
    expect(screen.getByText(text)).toBeInTheDocument()
  })

  it('the form posts (credentials never go in the URL) and the password field is masked', () => {
    renderApp('/login')
    const form = screen.getByRole('button', { name: 'Sign in' }).closest('form')!
    expect(form).toHaveAttribute('method', 'post')
    expect(screen.getByLabelText('Password')).toHaveAttribute('type', 'password')
  })
})
