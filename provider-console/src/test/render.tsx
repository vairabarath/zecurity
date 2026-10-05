import { render } from '@testing-library/react'
import { MemoryRouter } from 'react-router-dom'
import { AppRoutes } from '@/App'
import { useSessionStore } from '@/auth/session'
import type { Me } from '@/api/types'

/** Renders the real route table at `path`. */
export function renderApp(path = '/', state?: unknown) {
  return render(
    <MemoryRouter initialEntries={[{ pathname: path, state }]}>
      <AppRoutes />
    </MemoryRouter>,
  )
}

/** A full, verified session for `me`. */
export function signedInAs(me: Me, token = 'tok-full') {
  useSessionStore.getState().startSession(token, 900, false)
  useSessionStore.getState().setMe(me)
}

/** The token currently in sessionStorage, if any. */
export function storedToken(): string | undefined {
  const raw = sessionStorage.getItem('zecurity.provider.session')
  return raw ? (JSON.parse(raw) as { token: string }).token : undefined
}

/** Everything in sessionStorage as one string, for "never stored" checks. */
export function sessionStorageDump(): string {
  return Array.from({ length: sessionStorage.length }, (_, i) => {
    const key = sessionStorage.key(i)!
    return `${key}=${sessionStorage.getItem(key)}`
  }).join('\n')
}
