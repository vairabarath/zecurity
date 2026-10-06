import type { ReactNode } from 'react'
import { Navigate, Outlet, useLocation } from 'react-router-dom'
import { Skeleton } from '@/components/ui/skeleton'
import { hasRole, type ProviderRole } from './roles'
import { useSessionStore } from './session'

// Route guards (Sprint 21 C3). They only steer navigation so the user isn't
// shown screens the server would refuse; the controller still authorizes
// every request.

/** Where a guard sent the user from, so Login can return them there. */
export interface FromState {
  from?: string
}

/** Only same-app paths: "/x" yes; "//host", absolute URLs and /login no. */
function safeReturnPath(from: string | undefined): string {
  if (!from || !from.startsWith('/') || from.startsWith('//') || from.includes('\\') || from.startsWith('/login')) return '/'
  return from
}

function SessionLoading() {
  return (
    <div className="flex min-h-screen items-center justify-center p-6" aria-busy="true" aria-label="Loading session">
      <Skeleton className="h-24 w-full max-w-sm" />
    </div>
  )
}

function Guarded({ children }: { children?: ReactNode }) {
  return children ?? <Outlet />
}

/** Full session with /provider/me loaded. */
export function RequireSession({ children }: { children?: ReactNode }) {
  const status = useSessionStore((s) => s.status)
  const location = useLocation()
  if (status === 'loading') return <SessionLoading />
  if (status === 'anonymous') {
    const state: FromState = { from: location.pathname + location.search }
    return <Navigate to="/login" replace state={state} />
  }
  if (status === 'password_change') return <Navigate to="/change-password" replace />
  return <Guarded>{children}</Guarded>
}

/** Any token, including a password-change-only one (the Change password page). */
export function RequireToken({ children }: { children?: ReactNode }) {
  const status = useSessionStore((s) => s.status)
  if (status === 'loading') return <SessionLoading />
  if (status === 'anonymous') return <Navigate to="/login" replace />
  return <Guarded>{children}</Guarded>
}

/** The login page: a signed-in user goes on to where they were heading. */
export function RedirectIfAuthenticated({ children }: { children?: ReactNode }) {
  const status = useSessionStore((s) => s.status)
  const location = useLocation()
  if (status === 'loading') return <SessionLoading />
  if (status === 'password_change') return <Navigate to="/change-password" replace />
  if (status === 'authenticated') {
    return <Navigate to={safeReturnPath((location.state as FromState | null)?.from)} replace />
  }
  return <Guarded>{children}</Guarded>
}

/** Use inside RequireSession. A role outside `roles` (or an unknown role) → Forbidden. */
export function RequireRole({ roles, children }: { roles: readonly ProviderRole[]; children?: ReactNode }) {
  const role = useSessionStore((s) => s.me?.role)
  if (!hasRole(role, roles)) return <Navigate to="/forbidden" replace />
  return <Guarded>{children}</Guarded>
}
