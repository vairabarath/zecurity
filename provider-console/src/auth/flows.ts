import * as authApi from '@/api/auth'
import { ApiError } from '@/api/client'
import { ErrorCode, type SessionResponse, type TOTPEnrollResponse } from '@/api/types'
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
  if (stored.mfa) return // the TOTP step needs no /provider/me (it would refuse the token)
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
export async function signIn(email: string, password: string): Promise<'password_change' | 'mfa' | 'authenticated'> {
  const res = await authApi.login(email, password)
  if (res.mfa) {
    useSessionStore.getState().startMFA(res.token, res.expires_in, res.mfa)
    return 'mfa'
  }
  useSessionStore.getState().startSession(res.token, res.expires_in, res.password_change_required)
  if (res.password_change_required) return 'password_change'
  await loadMe()
  return 'authenticated'
}

/**
 * Changes the password and switches to the new token (the old one is now
 * dead). An operator who requires TOTP gets an MFA-stage token and goes on to
 * the TOTP step (Sprint 22 Phase T).
 */
export async function changePassword(currentPassword: string, newPassword: string): Promise<'mfa' | 'authenticated'> {
  const res = await authApi.changePassword(currentPassword, newPassword)
  return acceptSession(res)
}

/** Verifies a TOTP code with the MFA-stage token and switches to the full session. */
export async function verifyTotp(code: string): Promise<void> {
  await acceptSession(await authApi.verifyTotp(code))
}

/** Starts TOTP enrolment. The secret is returned to the caller only; never stored. */
export function startTotpEnrollment(): Promise<TOTPEnrollResponse> {
  return authApi.enrollTotp()
}

/** Confirms enrolment with the first code and switches to the full session. */
export async function confirmTotpEnrollment(code: string): Promise<void> {
  await acceptSession(await authApi.confirmTotp(code))
}

async function acceptSession(res: SessionResponse): Promise<'mfa' | 'authenticated'> {
  if (res.mfa) {
    useSessionStore.getState().startMFA(res.token, res.expires_in, res.mfa)
    return 'mfa'
  }
  useSessionStore.getState().startSession(res.token, res.expires_in, false)
  await loadMe()
  return 'authenticated'
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
