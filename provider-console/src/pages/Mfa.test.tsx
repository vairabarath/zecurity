import { screen, waitFor } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { describe, expect, it } from 'vitest'
import { useSessionStore } from '@/auth/session'
import { mockFetch, superAdminMe, type FakeResponse } from '@/test/fetch'
import { renderApp, sessionStorageDump, storedToken } from '@/test/render'

// Sprint 22 Phase T: mandatory super-admin TOTP in the console. The server
// decides (D-28, OQ-3); the console follows its "mfa" answers.

const PASSWORD = 'correct horse battery staple'
const SECRET = 'JBSWY3DPEHPK3PXPJBSWY3DPEHPK3PXP'
const URI = `otpauth://totp/Zecurity%20Provider:admin@provider.test?secret=${SECRET}&issuer=Zecurity+Provider`

const mfaLogin = (stage: 'verify' | 'enroll'): FakeResponse => ({
  status: 200,
  body: { token: `tok-mfa-${stage}`, expires_in: 300, password_change_required: false, mfa: stage },
})
const fullSession: FakeResponse = { status: 200, body: { token: 'tok-full', expires_in: 900, password_change_required: false } }

async function signIn() {
  const user = userEvent.setup()
  await user.type(screen.getByLabelText('Email'), 'admin@provider.test')
  await user.type(screen.getByLabelText('Password'), PASSWORD)
  await user.click(screen.getByRole('button', { name: 'Sign in' }))
  return user
}

describe('MFA: verify', () => {
  it('an enrolled super-admin enters a code and lands on Home', async () => {
    const { calls } = mockFetch({
      'POST /provider/auth/login': mfaLogin('verify'),
      'POST /provider/auth/totp/verify': fullSession,
      'GET /provider/me': { status: 200, body: superAdminMe },
    })
    renderApp('/login')
    const user = await signIn()
    expect(await screen.findByRole('heading', { name: 'Enter your authentication code' })).toBeInTheDocument()
    expect(calls.map((c) => c.path)).toEqual(['/provider/auth/login']) // no /provider/me with an MFA-stage token
    await user.type(screen.getByLabelText('Authentication code'), '123456')
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByRole('heading', { name: 'Your session' })).toBeInTheDocument()
    const verify = calls.find((c) => c.path === '/provider/auth/totp/verify')!
    expect(verify.body).toEqual({ code: '123456' })
    expect(verify.headers.Authorization).toBe('Bearer tok-mfa-verify')
    expect(storedToken()).toBe('tok-full')
  })

  it('a wrong code keeps the user on the page with a message; the code is cleared', async () => {
    mockFetch({
      'POST /provider/auth/login': mfaLogin('verify'),
      'POST /provider/auth/totp/verify': { status: 401, body: { error: 'invalid_code' } },
    })
    renderApp('/login')
    const user = await signIn()
    await screen.findByRole('heading', { name: 'Enter your authentication code' })
    await user.type(screen.getByLabelText('Authentication code'), '000000')
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByText('That code isn’t right. Enter the current code from your authenticator app.')).toBeInTheDocument()
    expect(screen.getByLabelText('Authentication code')).toHaveValue('')
    expect(useSessionStore.getState().status).toBe('mfa') // a wrong code does not end the session
  })

  it('too many wrong codes shows the wait time', async () => {
    mockFetch({
      'POST /provider/auth/login': mfaLogin('verify'),
      'POST /provider/auth/totp/verify': { status: 429, body: { error: 'too_many_attempts' }, headers: { 'Retry-After': '600' } },
    })
    renderApp('/login')
    const user = await signIn()
    await screen.findByRole('heading', { name: 'Enter your authentication code' })
    await user.type(screen.getByLabelText('Authentication code'), '000000')
    await user.click(screen.getByRole('button', { name: 'Verify' }))
    expect(await screen.findByText(/Too many attempts\. Try again in 10 minutes\./)).toBeInTheDocument()
  })

  it('only six digits can be submitted', async () => {
    mockFetch({ 'POST /provider/auth/login': mfaLogin('verify') })
    renderApp('/login')
    const user = await signIn()
    await screen.findByRole('heading', { name: 'Enter your authentication code' })
    await user.type(screen.getByLabelText('Authentication code'), '12a4')
    expect(screen.getByLabelText('Authentication code')).toHaveValue('124')
    expect(screen.getByRole('button', { name: 'Verify' })).toBeDisabled()
  })

  it('every other page sends an MFA-stage session to /mfa', async () => {
    mockFetch({ 'POST /provider/auth/login': mfaLogin('verify') })
    const view = renderApp('/login')
    await signIn()
    await screen.findByRole('heading', { name: 'Enter your authentication code' })
    view.unmount()
    renderApp('/relays')
    expect(screen.getByRole('heading', { name: 'Enter your authentication code' })).toBeInTheDocument()
  })
})

describe('MFA: enrol', () => {
  it('a super-admin without TOTP sees the setup key and QR once, confirms, and lands on Home', async () => {
    const { calls } = mockFetch({
      'POST /provider/auth/login': mfaLogin('enroll'),
      'POST /provider/auth/totp/enroll': { status: 200, body: { secret: SECRET, otpauth_uri: URI } },
      'POST /provider/auth/totp/enroll/confirm': fullSession,
      'GET /provider/me': { status: 200, body: superAdminMe },
    })
    renderApp('/login', undefined, { strict: true })
    const user = await signIn()
    expect(await screen.findByRole('heading', { name: 'Set up two-factor authentication' })).toBeInTheDocument()
    expect(await screen.findByLabelText('Setup key')).toHaveTextContent(SECRET.replace(/(.{4})/g, '$1 ').trim())
    expect(screen.getByRole('img', { name: 'QR code for your authenticator app' })).toBeInTheDocument()
    // Exactly one enrolment request, even under StrictMode (a second would replace the secret).
    expect(calls.filter((c) => c.path === '/provider/auth/totp/enroll')).toHaveLength(1)
    expect(sessionStorageDump()).not.toContain(SECRET)

    await user.type(screen.getByLabelText('Authentication code'), '654321')
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    expect(await screen.findByRole('heading', { name: 'Your session' })).toBeInTheDocument()
    expect(calls.find((c) => c.path === '/provider/auth/totp/enroll/confirm')!.body).toEqual({ code: '654321' })
    expect(sessionStorageDump()).not.toContain(SECRET)
    expect(document.body.textContent).not.toContain(SECRET)
  })

  it('a wrong confirmation code keeps the setup key on screen', async () => {
    mockFetch({
      'POST /provider/auth/login': mfaLogin('enroll'),
      'POST /provider/auth/totp/enroll': { status: 200, body: { secret: SECRET, otpauth_uri: URI } },
      'POST /provider/auth/totp/enroll/confirm': { status: 401, body: { error: 'invalid_code' } },
    })
    renderApp('/login')
    const user = await signIn()
    await screen.findByLabelText('Setup key')
    await user.type(screen.getByLabelText('Authentication code'), '000000')
    await user.click(screen.getByRole('button', { name: 'Confirm and continue' }))
    expect(await screen.findByText(/That code isn’t right/)).toBeInTheDocument()
    expect(screen.getByLabelText('Setup key')).toBeInTheDocument()
  })
})

describe('MFA: other paths into the second factor', () => {
  it('a forced password change continues to TOTP enrolment, not Home', async () => {
    mockFetch({
      'POST /provider/auth/login': { status: 200, body: { token: 'tok-pwc', expires_in: 600, password_change_required: true } },
      'POST /provider/auth/password': { status: 200, body: { token: 'tok-mfa-enroll', expires_in: 300, password_change_required: false, mfa: 'enroll' } },
      'POST /provider/auth/totp/enroll': { status: 200, body: { secret: SECRET, otpauth_uri: URI } },
    })
    renderApp('/login')
    const user = await signIn()
    await screen.findByRole('heading', { name: 'Set a new password' })
    await user.type(screen.getByLabelText(/current password/i), PASSWORD)
    await user.type(screen.getByLabelText('New password'), 'a-brand-new-password-1')
    await user.type(screen.getByLabelText('Confirm new password'), 'a-brand-new-password-1')
    await user.click(screen.getByRole('button', { name: 'Save new password' }))
    expect(await screen.findByRole('heading', { name: 'Set up two-factor authentication' })).toBeInTheDocument()
  })

  it('a stored pre-TOTP super-admin session is refused with a sign-in-again notice (OQ-3)', async () => {
    sessionStorage.setItem('zecurity.provider.session', JSON.stringify({ token: 'tok-legacy', expiresAt: Date.now() + 600_000, pwcOnly: false }))
    mockFetch({ 'GET /provider/me': { status: 403, body: { error: 'mfa_required' } } })
    const { rehydrate } = await import('@/auth/flows')
    await rehydrate()
    renderApp('/')
    await waitFor(() => expect(useSessionStore.getState().status).toBe('anonymous'))
    expect(await screen.findByText('Sign in again and confirm with your authenticator app.')).toBeInTheDocument()
    expect(storedToken()).toBeUndefined()
  })
})
