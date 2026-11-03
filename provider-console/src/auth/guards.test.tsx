import { render, screen } from '@testing-library/react'
import { MemoryRouter, Route, Routes, useLocation } from 'react-router-dom'
import { describe, expect, it } from 'vitest'
import { relayOpsMe, superAdminMe } from '@/test/fetch'
import type { Me } from '@/api/types'
import { RedirectIfAuthenticated, RequireRole, RequireSession, RequireToken } from './guards'
import { useSessionStore } from './session'

function LoginProbe() {
  const location = useLocation()
  const from = (location.state as { from?: string } | null)?.from
  return <p>login page{from ? ` from ${from}` : ''}</p>
}

// A test-only route table: the real one arrives with the pages (commit 4).
function renderAt(path: string, state?: unknown) {
  return render(
    <MemoryRouter initialEntries={[{ pathname: path.split('?')[0], search: path.includes('?') ? `?${path.split('?')[1]}` : '', state }]}>
      <Routes>
        <Route
          path="/login"
          element={
            <RedirectIfAuthenticated>
              <LoginProbe />
            </RedirectIfAuthenticated>
          }
        />
        <Route
          path="/change-password"
          element={
            <RequireToken>
              <p>change password page</p>
            </RequireToken>
          }
        />
        <Route path="/forbidden" element={<p>forbidden page</p>} />
        <Route element={<RequireSession />}>
          <Route path="/" element={<p>home page</p>} />
          <Route path="/reports" element={<p>reports page</p>} />
          <Route element={<RequireRole roles={['super-admin']} />}>
            <Route path="/admin-only" element={<p>admin-only page</p>} />
          </Route>
        </Route>
      </Routes>
    </MemoryRouter>,
  )
}

function authenticatedAs(me: Me) {
  useSessionStore.getState().startSession('tok', 900, false)
  useSessionStore.getState().setMe(me)
}

describe('RequireSession', () => {
  it('sends an anonymous user to Login and remembers the target', () => {
    renderAt('/reports?week=40')
    expect(screen.getByText('login page from /reports?week=40')).toBeInTheDocument()
  })

  it('shows a loading state while the session is being verified', () => {
    useSessionStore.getState().startSession('tok', 900, false)
    renderAt('/')
    expect(screen.getByLabelText('Loading session')).toBeInTheDocument()
    expect(screen.queryByText('home page')).not.toBeInTheDocument()
  })

  it('a password-change-only session can only reach Change password', () => {
    useSessionStore.getState().startSession('tok', 900, true)
    renderAt('/reports')
    expect(screen.getByText('change password page')).toBeInTheDocument()
  })

  it('an authenticated user gets the page', () => {
    authenticatedAs(relayOpsMe)
    renderAt('/reports')
    expect(screen.getByText('reports page')).toBeInTheDocument()
  })
})

describe('RequireToken', () => {
  it('sends an anonymous user to Login', () => {
    renderAt('/change-password')
    expect(screen.getByText('login page')).toBeInTheDocument()
  })

  it('allows a voluntary change from a full session', () => {
    authenticatedAs(superAdminMe)
    renderAt('/change-password')
    expect(screen.getByText('change password page')).toBeInTheDocument()
  })
})

describe('RedirectIfAuthenticated', () => {
  it('shows Login when anonymous', () => {
    renderAt('/login')
    expect(screen.getByText('login page')).toBeInTheDocument()
  })

  it('sends a signed-in user back to where they were heading', () => {
    authenticatedAs(superAdminMe)
    renderAt('/login', { from: '/reports' })
    expect(screen.getByText('reports page')).toBeInTheDocument()
  })

  it.each(['//evil.example/x', 'https://evil.example/', '/\\evil.example', '/login'])(
    'ignores an unsafe return path %s',
    (from) => {
      authenticatedAs(superAdminMe)
      renderAt('/login', { from })
      expect(screen.getByText('home page')).toBeInTheDocument()
    },
  )

  it('sends a password-change-only session to Change password', () => {
    useSessionStore.getState().startSession('tok', 900, true)
    renderAt('/login')
    expect(screen.getByText('change password page')).toBeInTheDocument()
  })
})

describe('RequireRole', () => {
  it('lets an allowed role through', () => {
    authenticatedAs(superAdminMe)
    renderAt('/admin-only')
    expect(screen.getByText('admin-only page')).toBeInTheDocument()
  })

  it('sends relay-ops to Forbidden', () => {
    authenticatedAs(relayOpsMe)
    renderAt('/admin-only')
    expect(screen.getByText('forbidden page')).toBeInTheDocument()
  })

  it('treats an unknown role as allowed nothing', () => {
    authenticatedAs({ ...superAdminMe, role: 'root' })
    renderAt('/admin-only')
    expect(screen.getByText('forbidden page')).toBeInTheDocument()
  })
})
