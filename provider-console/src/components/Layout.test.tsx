import { screen, within } from '@testing-library/react'
import userEvent from '@testing-library/user-event'
import { MemoryRouter, Route, Routes } from 'react-router-dom'
import { render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import type { Section } from '@/auth/roles'
import { useSessionStore } from '@/auth/session'
import { mockFetch, relayOpsMe, superAdminMe } from '@/test/fetch'
import { renderApp, signedInAs } from '@/test/render'
import { Layout } from './Layout'

// A matrix shaped like the one Phase P will have, to prove the nav follows it.
const matrix: Section[] = [
  { key: 'home', path: '/', label: 'Home', roles: ['super-admin', 'relay-ops'] },
  { key: 'users', path: '/users', label: 'Provider users', roles: ['super-admin'] },
  { key: 'relays', path: '/relays', label: 'Relays', roles: ['super-admin', 'relay-ops'] },
]

function navLabels() {
  return within(screen.getByRole('navigation', { name: 'Sections' }))
    .queryAllByRole('link')
    .map((a) => a.textContent)
}

function renderLayout() {
  return render(
    <MemoryRouter>
      <Routes>
        <Route element={<Layout sections={matrix} />}>
          <Route index element={<p>content</p>} />
        </Route>
      </Routes>
    </MemoryRouter>,
  )
}

describe('Layout', () => {
  it('super-admin sees every section in the matrix', () => {
    signedInAs(superAdminMe)
    renderLayout()
    expect(navLabels()).toEqual(['Home', 'Provider users', 'Relays'])
  })

  it('relay-ops sees only its sections', () => {
    signedInAs(relayOpsMe)
    renderLayout()
    expect(navLabels()).toEqual(['Home', 'Relays'])
  })

  it('an unknown role sees no sections', () => {
    signedInAs({ ...superAdminMe, role: 'root' })
    renderLayout()
    expect(navLabels()).toEqual([])
  })

  it('shows the email, role and session countdown', () => {
    signedInAs(superAdminMe)
    renderApp('/')
    const menu = screen.getByRole('button', { name: 'Account menu' })
    expect(within(menu).getByText(superAdminMe.email)).toBeInTheDocument()
    expect(within(menu).getByText('Super-admin')).toBeInTheDocument()
    expect(screen.getAllByLabelText('Session time remaining').length).toBeGreaterThan(0)
  })

  it('account menu → Change password opens the voluntary change', async () => {
    signedInAs(superAdminMe)
    renderApp('/')
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Account menu' }))
    await user.click(await screen.findByRole('menuitem', { name: 'Change password' }))
    expect(screen.getByRole('heading', { name: 'Change password' })).toBeInTheDocument()
  })

  it('account menu → Sign out logs out on the server and shows Login', async () => {
    signedInAs(superAdminMe, 'tok-full')
    const { calls } = mockFetch({ 'POST /provider/auth/logout': { status: 204 } })
    renderApp('/')
    const user = userEvent.setup()
    await user.click(screen.getByRole('button', { name: 'Account menu' }))
    await user.click(await screen.findByRole('menuitem', { name: 'Sign out' }))
    expect(await screen.findByText('You have signed out.')).toBeInTheDocument()
    expect(calls[0]).toMatchObject({ method: 'POST', path: '/provider/auth/logout' })
    expect(calls[0].headers.Authorization).toBe('Bearer tok-full')
    expect(useSessionStore.getState().status).toBe('anonymous')
    expect(sessionStorage.length).toBe(0)
  })
})
