import * as authApi from '@/api/auth'
import { ApiError } from '@/api/client'
import { ErrorCode } from '@/api/types'
import { readStoredSession, useSessionStore } from './session'

// Session transitions that need the API. Errors from the API calls are
// rethrown so the pages can show them; session-ending responses have already
// cleared the store (see client.ts) by the time they get here.

/**
 * App start: restore a token from sessionStorage, then validate it with
 * GET /provider/me before trusting it. Fails closed: if the server can't
 * confirm the session, it is dropped and the user signs in again.
 */
export async function rehydrate(): Promise<void> {
  const stored = readStoredSession()
  if (!stored) return
  const store = useSessionStore.getState()
  store.restoreSession(stored)
  try {
    store.setMe(await authApi.getMe())
  } catch (err) {
    if (err instanceof ApiError && (err.sessionEnded || err.code === ErrorCode.passwordChangeRequired)) {
      return // client.ts already moved the store to the right state
    }
    useSessionStore.getState().endSession('unverified')
  }
}

/** Signs in. Resolves to where the user must go next. */
export async function signIn(email: string, password: string): Promise<'password_change' | 'authenticated'> {
  const res = await authApi.login(email, password)
  useSessionStore.getState().startSession(res.token, res.expires_in, res.password_change_required)
  if (res.password_change_required) return 'password_change'
  await loadMe()
  return 'authenticated'
}

/** Changes the password and switches to the new token (the old one is now dead). */
export async function changePassword(currentPassword: string, newPassword: string): Promise<void> {
  const res = await authApi.changePassword(currentPassword, newPassword)
  useSessionStore.getState().startSession(res.token, res.expires_in, false)
  await loadMe()
}

/** Ends the session on the server (all tabs) and locally. The local clear always happens. */
export async function signOut(): Promise<void> {
  try {
    await authApi.logout()
  } catch {
    /* the local session is cleared regardless */
  }
  useSessionStore.getState().endSession('logged_out')
}

async function loadMe(): Promise<void> {
  useSessionStore.getState().setMe(await authApi.getMe())
}
