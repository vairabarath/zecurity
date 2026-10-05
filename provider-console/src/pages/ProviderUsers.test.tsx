import { fireEvent, screen, waitFor, within } from '@testing-library/react'
import userEvent, { type UserEvent } from '@testing-library/user-event'
import { describe, expect, it, vi } from 'vitest'
import type { OperatorView } from '@/api/types'
import { useSessionStore } from '@/auth/session'
import { mockFetch, relayOpsMe, superAdminMe, type FakeResponse, type RecordedCall } from '@/test/fetch'
import { renderApp, sessionStorageDump, signedInAs } from '@/test/render'

const TEMP = 'TempPass-Xyz98765'

const op = (over: Partial<OperatorView>): OperatorView => ({
  id: 'id',
  email: 'x@provider.test',
  role: 'relay-ops',
  disabled_at: null,
  created_at: '2026-10-01T08:00:00Z',
  last_login_at: null,
  must_change_password: false,
  has_password: true,
  ...over,
})

const me = op({ id: superAdminMe.user_id, email: superAdminMe.email, role: 'super-admin', last_login_at: '2026-10-05T09:00:00Z' })
const ops1 = op({ id: 'ops1', email: 'ops1@provider.test', must_change_password: true })
const ops2 = op({ id: 'ops2', email: 'ops2@provider.test', disabled_at: '2026-10-02T10:00:00Z' })
const ops3 = op({ id: 'ops3', email: 'ops3@provider.test', has_password: false })
const admin2 = op({ id: 'admin2', email: 'admin2@provider.test', role: 'super-admin' })
const ROSTER = [me, ops1, ops2, ops3, admin2]

function setup(routes: Record<string, FakeResponse | ((c: RecordedCall) => FakeResponse)> = {}, roster = ROSTER) {
  signedInAs(superAdminMe)
  const fetchMock = mockFetch({ 'GET /provider/users': { status: 200, body: roster }, ...routes })
  const user = userEvent.setup()
  renderApp('/users')
  return { user, ...fetchMock }
}

// hidden: true also finds rows while a modal dialog hides the page from the a11y tree.
const row = (email: string) => screen.getByRole('row', { name: email, hidden: true })
const listCalls = (calls: RecordedCall[]) => calls.filter((c) => c.method === 'GET' && c.path === '/provider/users')
const writes = (calls: RecordedCall[]) => calls.filter((c) => c.method !== 'GET')

async function openRowAction(user: UserEvent, email: string, item: string) {
  await user.click(await screen.findByRole('button', { name: `Actions for ${email}` }))
  await user.click(await screen.findByRole('menuitem', { name: item }))
  return screen.findByRole('dialog')
}

describe('Provider users: list', () => {
  it('lists operators with role, status and badges', async () => {
    setup()
    await screen.findByRole('table', { name: 'Provider operators' })
    expect(within(row('ops1@provider.test')).getByText('Must change password')).toBeInTheDocument()
    expect(within(row('ops2@provider.test')).getByText('Disabled')).toBeInTheDocument()
    expect(within(row('ops3@provider.test')).getByText('No password')).toBeInTheDocument()
    expect(within(row('admin2@provider.test')).getByText('Super-admin')).toBeInTheDocument()
    expect(within(row(me.email)).getByText('(you)')).toBeInTheDocument()
  })

  it('your own row has no role-change, disable or reset controls', async () => {
    setup()
    await screen.findByRole('table')
    expect(within(row(me.email)).queryByRole('button')).not.toBeInTheDocument()
    expect(screen.queryByRole('button', { name: `Actions for ${me.email}` })).not.toBeInTheDocument()
    expect(within(row(me.email)).getByText('Use the account menu')).toBeInTheDocument()
  })

  it('offers enable (not reset or disable) on a disabled row, and reset/disable on an active one', async () => {
    const { user } = setup()
    await user.click(await screen.findByRole('button', { name: 'Actions for ops2@provider.test' }))
    const disabledItems = (await screen.findAllByRole('menuitem')).map((m) => m.textContent)
    expect(disabledItems).toEqual(['Change role', 'Enable'])
    await user.keyboard('{Escape}')
    await user.click(screen.getByRole('button', { name: 'Actions for ops1@provider.test' }))
    const activeItems = (await screen.findAllByRole('menuitem')).map((m) => m.textContent)
    expect(activeItems).toEqual(['Change role', 'Reset password', 'Disable'])
  })

  it('a load error shows a message with Retry', async () => {
    const { user, calls } = setup({ 'GET /provider/users': { status: 500, body: { error: 'list provider users failed' } } })
    expect(await screen.findByText(/Couldn’t load operators/)).toBeInTheDocument()
    await user.click(screen.getByRole('button', { name: 'Retry' }))
    await waitFor(() => expect(listCalls(calls)).toHaveLength(2))
  })

  it('a server 403 shows Forbidden and keeps the session', async () => {
    setup({ 'GET /provider/users': { status: 403, body: { error: 'provider action forbidden' } } })
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    expect(useSessionStore.getState().status).toBe('authenticated')
  })
})

describe('Provider users: role guard', () => {
  it('relay-ops has no nav entry and /users shows Forbidden without calling the API', () => {
    signedInAs(relayOpsMe)
    const { calls } = mockFetch({})
    renderApp('/users')
    expect(screen.getByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    expect(screen.queryByRole('link', { name: 'Provider users' })).not.toBeInTheDocument()
    expect(calls).toHaveLength(0)
  })

  it('super-admin sees the nav entry', async () => {
    setup()
    expect(await screen.findByRole('link', { name: 'Provider users' })).toHaveAttribute('href', '/users')
  })
})

describe('Provider users: add operator', () => {
  const newOp = op({ id: 'new', email: 'new@provider.test', must_change_password: true })

  it('creates, shows the temporary password once, reloads, and discards it on close', async () => {
    const { user, calls } = setup({
      'POST /provider/users': { status: 201, body: { user: newOp, temporary_password: TEMP } },
    })
    await user.click(await screen.findByRole('button', { name: 'Add operator' }))
    const dialog = await screen.findByRole('dialog', { name: 'Add operator' })
    expect(within(dialog).getByRole('combobox', { name: 'Role' })).toHaveTextContent('Relay ops')
    await user.type(within(dialog).getByLabelText('Email'), 'new@provider.test')
    await user.click(within(dialog).getByRole('button', { name: 'Add operator' }))

    const pw = await screen.findByRole('dialog', { name: 'Temporary password' })
    expect(within(pw).getByLabelText('Temporary password')).toHaveTextContent(TEMP)
    expect(within(pw).getByText('new@provider.test')).toBeInTheDocument()
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'POST', path: '/provider/users', body: { email: 'new@provider.test', role: 'relay-ops' } }),
    ])
    expect(listCalls(calls)).toHaveLength(2) // initial + reload after the mutation

    await user.click(within(pw).getByRole('button', { name: 'I’ve shared it, close' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain(TEMP)
    expect(sessionStorageDump()).not.toContain(TEMP)
    expect(localStorage.length).toBe(0)
    expect(window.location.href).not.toContain(TEMP)
  })

  it('can add a super-admin', async () => {
    const { user, calls } = setup({
      'POST /provider/users': { status: 201, body: { user: { ...newOp, role: 'super-admin' }, temporary_password: TEMP } },
    })
    await user.click(await screen.findByRole('button', { name: 'Add operator' }))
    const dialog = await screen.findByRole('dialog', { name: 'Add operator' })
    await user.type(within(dialog).getByLabelText('Email'), 'boss@provider.test')
    await user.click(within(dialog).getByRole('combobox', { name: 'Role' }))
    await user.click(await screen.findByRole('option', { name: 'Super-admin' }))
    await user.click(within(dialog).getByRole('button', { name: 'Add operator' }))
    await screen.findByRole('dialog', { name: 'Temporary password' })
    expect(writes(calls)[0].body).toEqual({ email: 'boss@provider.test', role: 'super-admin' })
  })

  it.each([
    ['already_exists', 'An operator with this email already exists.'],
    ['exists_disabled', 'This email belongs to a disabled operator. Enable that operator instead.'],
    ['invalid_email', 'Enter a valid email address.'],
  ])('%s shows a readable message and keeps the dialog open', async (code, message) => {
    const { user, calls } = setup({ 'POST /provider/users': { status: code === 'invalid_email' ? 400 : 409, body: { error: code } } })
    await user.click(await screen.findByRole('button', { name: 'Add operator' }))
    const dialog = await screen.findByRole('dialog', { name: 'Add operator' })
    await user.type(within(dialog).getByLabelText('Email'), 'ops2@provider.test')
    await user.click(within(dialog).getByRole('button', { name: 'Add operator' }))
    expect(await within(dialog).findByText(message)).toBeInTheDocument()
    expect(listCalls(calls)).toHaveLength(1) // no reload after a failure
  })
})

describe('Provider users: temporary password copy', () => {
  async function issue(user: UserEvent) {
    await openRowAction(user, 'ops1@provider.test', 'Reset password')
    await user.click(screen.getByRole('button', { name: 'Reset password' }))
    return screen.findByRole('dialog', { name: 'Temporary password' })
  }

  it('copies only on click, then shows Copied', async () => {
    const { user } = setup({ 'POST /provider/users/ops1/reset-password': { status: 200, body: { temporary_password: TEMP } } })
    const writeText = vi.spyOn(navigator.clipboard, 'writeText')
    const pw = await issue(user)
    expect(writeText).not.toHaveBeenCalled()
    await user.click(within(pw).getByRole('button', { name: 'Copy' }))
    expect(writeText).toHaveBeenCalledWith(TEMP)
    expect(await within(pw).findByRole('button', { name: 'Copied' })).toBeInTheDocument()
  })

  it('if the clipboard fails, the password stays visible for manual copy', async () => {
    const { user } = setup({ 'POST /provider/users/ops1/reset-password': { status: 200, body: { temporary_password: TEMP } } })
    vi.spyOn(navigator.clipboard, 'writeText').mockRejectedValue(new Error('denied'))
    const pw = await issue(user)
    await user.click(within(pw).getByRole('button', { name: 'Copy' }))
    expect(await within(pw).findByText(/Select the password and copy it manually/)).toBeInTheDocument()
    expect(within(pw).getByLabelText('Temporary password')).toHaveTextContent(TEMP)
  })

  it('clicking outside does not discard it; Escape does', async () => {
    const { user } = setup({ 'POST /provider/users/ops1/reset-password': { status: 200, body: { temporary_password: TEMP } } })
    await issue(user)
    // The page under a modal ignores pointer events, so dispatch directly.
    fireEvent.pointerDown(document.body)
    expect(screen.getByRole('dialog', { name: 'Temporary password' })).toBeInTheDocument()
    await user.keyboard('{Escape}')
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(document.body.textContent).not.toContain(TEMP)
  })
})

describe('Provider users: change role', () => {
  it('shows email and both roles; confirming the same role is disabled', async () => {
    const { user, calls } = setup({
      'PATCH /provider/users/ops1': { status: 200, body: { user: { ...ops1, role: 'super-admin' } } },
    })
    const dialog = await openRowAction(user, 'ops1@provider.test', 'Change role')
    expect(within(dialog).getByText('ops1@provider.test')).toBeInTheDocument()
    expect(within(dialog).getByText('Current role').nextElementSibling).toHaveTextContent('Relay ops')
    expect(within(dialog).getByRole('button', { name: 'Change role' })).toBeDisabled()

    await user.click(within(dialog).getByRole('combobox', { name: 'New role' }))
    await user.click(await screen.findByRole('option', { name: 'Super-admin' }))
    await user.click(within(dialog).getByRole('button', { name: 'Change to Super-admin' }))

    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(writes(calls)).toEqual([
      expect.objectContaining({ method: 'PATCH', path: '/provider/users/ops1', body: { role: 'super-admin' } }),
    ])
    expect(listCalls(calls)).toHaveLength(2)
  })

  it('last_super_admin is explained', async () => {
    const { user } = setup({ 'PATCH /provider/users/admin2': { status: 409, body: { error: 'last_super_admin' } } })
    const dialog = await openRowAction(user, 'admin2@provider.test', 'Change role')
    await user.click(within(dialog).getByRole('combobox', { name: 'New role' }))
    await user.click(await screen.findByRole('option', { name: 'Relay ops' }))
    await user.click(within(dialog).getByRole('button', { name: 'Change to Relay ops' }))
    expect(await within(dialog).findByText(/would leave no active super-admin/)).toBeInTheDocument()
  })
})

describe('Provider users: disable / enable / reset', () => {
  it('disable needs confirmation; cancel sends nothing', async () => {
    const { user, calls } = setup({ 'POST /provider/users/ops1/disable': { status: 204 } })
    let dialog = await openRowAction(user, 'ops1@provider.test', 'Disable')
    expect(within(dialog).getByText('ops1@provider.test')).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: 'Cancel' }))
    expect(writes(calls)).toHaveLength(0)

    dialog = await openRowAction(user, 'ops1@provider.test', 'Disable')
    await user.click(within(dialog).getByRole('button', { name: 'Disable' }))
    await waitFor(() => expect(screen.queryByRole('dialog')).not.toBeInTheDocument())
    expect(writes(calls)).toEqual([expect.objectContaining({ method: 'POST', path: '/provider/users/ops1/disable' })])
    expect(listCalls(calls)).toHaveLength(2)
  })

  it.each([
    ['cannot_modify_self', /can’t change your own account/],
    ['last_super_admin', /would leave no active super-admin/],
  ])('disable %s shows a readable message', async (code, message) => {
    const { user } = setup({ 'POST /provider/users/admin2/disable': { status: 409, body: { error: code } } })
    const dialog = await openRowAction(user, 'admin2@provider.test', 'Disable')
    await user.click(within(dialog).getByRole('button', { name: 'Disable' }))
    expect(await within(dialog).findByText(message)).toBeInTheDocument()
  })

  it('enable reloads and offers "Reset password now" without resetting by itself', async () => {
    let enabled = false
    const { user, calls } = setup({
      'GET /provider/users': () => ({
        status: 200,
        body: enabled ? ROSTER.map((o) => (o.id === 'ops2' ? { ...o, disabled_at: null, has_password: false } : o)) : ROSTER,
      }),
      'POST /provider/users/ops2/enable': () => {
        enabled = true
        return { status: 204 }
      },
      'POST /provider/users/ops2/reset-password': { status: 200, body: { temporary_password: TEMP } },
    })
    const dialog = await openRowAction(user, 'ops2@provider.test', 'Enable')
    expect(within(dialog).getByText(/without a password/)).toBeInTheDocument()
    await user.click(within(dialog).getByRole('button', { name: 'Enable' }))

    const offer = await screen.findByRole('dialog', { name: 'Operator enabled' })
    expect(writes(calls).map((c) => c.path)).toEqual(['/provider/users/ops2/enable'])
    expect(listCalls(calls)).toHaveLength(2)
    expect(within(row('ops2@provider.test')).getByText('No password')).toBeInTheDocument()

    await user.click(within(offer).getByRole('button', { name: 'Reset password now' }))
    const pw = await screen.findByRole('dialog', { name: 'Temporary password' })
    expect(within(pw).getByLabelText('Temporary password')).toHaveTextContent(TEMP)
    expect(writes(calls).map((c) => c.path)).toEqual([
      '/provider/users/ops2/enable',
      '/provider/users/ops2/reset-password',
    ])
  })

  it('"Later" after enable closes without resetting', async () => {
    const { user, calls } = setup({ 'POST /provider/users/ops2/enable': { status: 204 } })
    const dialog = await openRowAction(user, 'ops2@provider.test', 'Enable')
    await user.click(within(dialog).getByRole('button', { name: 'Enable' }))
    const offer = await screen.findByRole('dialog', { name: 'Operator enabled' })
    await user.click(within(offer).getByRole('button', { name: 'Later' }))
    expect(screen.queryByRole('dialog')).not.toBeInTheDocument()
    expect(writes(calls).map((c) => c.path)).toEqual(['/provider/users/ops2/enable'])
  })

  it('reset needs confirmation and shows the password once', async () => {
    const { user, calls } = setup({
      'POST /provider/users/ops3/reset-password': { status: 200, body: { temporary_password: TEMP } },
    })
    const dialog = await openRowAction(user, 'ops3@provider.test', 'Reset password')
    expect(writes(calls)).toHaveLength(0)
    await user.click(within(dialog).getByRole('button', { name: 'Reset password' }))
    const pw = await screen.findByRole('dialog', { name: 'Temporary password' })
    expect(within(pw).getByLabelText('Temporary password')).toHaveTextContent(TEMP)
    expect(listCalls(calls)).toHaveLength(2)
  })

  it('account_disabled on reset says to enable first', async () => {
    const { user } = setup({ 'POST /provider/users/ops1/reset-password': { status: 409, body: { error: 'account_disabled' } } })
    const dialog = await openRowAction(user, 'ops1@provider.test', 'Reset password')
    await user.click(within(dialog).getByRole('button', { name: 'Reset password' }))
    expect(await within(dialog).findByText(/Enable them first/)).toBeInTheDocument()
  })

  it('not_found refreshes the list and explains', async () => {
    const { user, calls } = setup({ 'POST /provider/users/ops1/disable': { status: 404, body: { error: 'not_found' } } })
    const dialog = await openRowAction(user, 'ops1@provider.test', 'Disable')
    await user.click(within(dialog).getByRole('button', { name: 'Disable' }))
    expect(await within(dialog).findByText(/no longer exists/)).toBeInTheDocument()
    await waitFor(() => expect(listCalls(calls)).toHaveLength(2))
  })
})

describe('Provider users: session handling (C-a, unchanged)', () => {
  it('a revoked session during an action goes to Login', async () => {
    const { user } = setup({ 'POST /provider/users/ops1/disable': { status: 401, body: { error: 'provider session revoked' } } })
    const dialog = await openRowAction(user, 'ops1@provider.test', 'Disable')
    await user.click(within(dialog).getByRole('button', { name: 'Disable' }))
    expect(await screen.findByText('Your session has ended. Sign in again.')).toBeInTheDocument()
  })

  it('a 403 on an action shows Forbidden and keeps the session', async () => {
    const { user } = setup({ 'POST /provider/users/ops1/disable': { status: 403, body: { error: 'forbidden' } } })
    const dialog = await openRowAction(user, 'ops1@provider.test', 'Disable')
    await user.click(within(dialog).getByRole('button', { name: 'Disable' }))
    expect(await screen.findByRole('heading', { name: /don’t have access/ })).toBeInTheDocument()
    expect(useSessionStore.getState().status).toBe('authenticated')
  })
})
