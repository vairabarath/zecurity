// The console route guard (PENDING-19 R2): CONSOLE_READ gets the shell,
// Member (and no user) is sent to /install.
import { describe, expect, it, vi } from 'vitest'
import { render, screen } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { Role } from '@/generated/graphql'
import { useAuthStore } from '@/store/auth'
import { setTestRole } from '@/test/roles'

vi.mock('@/hooks/useRequireAuth', () => ({ useRequireAuth: () => ({ isReady: true }) }))
vi.mock('@/components/layout/AppShell', () => ({ AppShell: () => <div>CONSOLE SHELL</div> }))
vi.mock('@/pages/ClientInstall', () => ({ default: () => <div>CLIENT INSTALL</div> }))

const { default: App } = await import('@/App')

function renderAt(path: string) {
  render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  )
}

describe('console guard', () => {
  it.each([Role.Admin, Role.Viewer, Role.Devops, Role.Security, Role.Helpdesk])(
    '%s gets the console',
    (role) => {
      setTestRole(role)
      renderAt('/dashboard')
      expect(screen.getByText('CONSOLE SHELL')).toBeInTheDocument()
    },
  )

  it('Member is sent to /install', () => {
    setTestRole(Role.Member)
    renderAt('/users')
    expect(screen.getByText('CLIENT INSTALL')).toBeInTheDocument()
    expect(screen.queryByText('CONSOLE SHELL')).toBeNull()
  })

  it('no user is sent to /install (deny by default)', () => {
    useAuthStore.setState({ user: null })
    renderAt('/dashboard')
    expect(screen.getByText('CLIENT INSTALL')).toBeInTheDocument()
  })
})
