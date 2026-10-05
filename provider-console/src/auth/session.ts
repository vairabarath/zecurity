import { create } from 'zustand'
import type { Me } from '@/api/types'

// Provider session state (Sprint 21 C-a).
//
// Token rules: the token lives in this store's memory and in sessionStorage
// (so a refresh in the same tab survives), never in localStorage, the URL or
// a log. A rehydrated token is not trusted until GET /provider/me accepts it
// (see flows.ts). There is no refresh token: at expiry the session ends.

export const SESSION_STORAGE_KEY = 'zecurity.provider.session'

export type SessionStatus =
  | 'anonymous' // no token
  | 'loading' // token held, /provider/me not answered yet
  | 'password_change' // password-change-only token: only Change password is reachable
  | 'authenticated' // full token and /provider/me loaded

/** Why the last session ended; the login page turns this into a notice. */
export type EndReason =
  | 'logged_out'
  | 'expired'
  | 'session_ended' // middleware 401: revoked, password reset, role change, bad token
  | 'disabled' // middleware 403 "not a provider user"
  | 'unverified' // a stored session could not be checked against the server

interface StoredSession {
  token: string
  expiresAt: number
  pwcOnly: boolean
}

interface SessionState {
  status: SessionStatus
  token?: string
  /** Epoch milliseconds. */
  expiresAt?: number
  /** True while the token is password-change-only (claim pwc). */
  pwcOnly: boolean
  me?: Me
  endReason?: EndReason

  /** Stores a freshly issued token (login or password change). */
  startSession: (token: string, expiresInSeconds: number, pwcOnly: boolean) => void
  /** Puts a token read back from sessionStorage into memory, pending validation. */
  restoreSession: (stored: StoredSession) => void
  setMe: (me: Me) => void
  /** The server answered 403 password_change_required. */
  requirePasswordChange: () => void
  /** Clears every trace of the session and records why. */
  endSession: (reason: EndReason) => void
  clearEndReason: () => void
}

let expiryTimer: ReturnType<typeof setTimeout> | undefined

function scheduleExpiry(expiresAt: number) {
  clearTimeout(expiryTimer)
  expiryTimer = setTimeout(
    () => useSessionStore.getState().endSession('expired'),
    Math.max(0, expiresAt - Date.now()),
  )
}

// sessionStorage can be unavailable (privacy modes, blocked storage). The
// session then simply doesn't survive a refresh.
function writeStored(s: StoredSession) {
  try {
    sessionStorage.setItem(SESSION_STORAGE_KEY, JSON.stringify(s))
  } catch {
    /* storage unavailable */
  }
}

function removeStored() {
  try {
    sessionStorage.removeItem(SESSION_STORAGE_KEY)
  } catch {
    /* storage unavailable */
  }
}

/** Reads the stored session; malformed or expired entries are removed. */
export function readStoredSession(now = Date.now()): StoredSession | undefined {
  let raw: string | null
  try {
    raw = sessionStorage.getItem(SESSION_STORAGE_KEY)
  } catch {
    return undefined
  }
  if (!raw) return undefined
  try {
    const v = JSON.parse(raw) as Partial<StoredSession>
    if (
      typeof v.token === 'string' &&
      v.token !== '' &&
      typeof v.expiresAt === 'number' &&
      typeof v.pwcOnly === 'boolean' &&
      v.expiresAt > now
    ) {
      return { token: v.token, expiresAt: v.expiresAt, pwcOnly: v.pwcOnly }
    }
  } catch {
    /* fall through */
  }
  removeStored()
  return undefined
}

export const useSessionStore = create<SessionState>()((set) => ({
  status: 'anonymous',
  pwcOnly: false,

  startSession: (token, expiresInSeconds, pwcOnly) => {
    const expiresAt = Date.now() + expiresInSeconds * 1000
    writeStored({ token, expiresAt, pwcOnly })
    scheduleExpiry(expiresAt)
    set({
      token,
      expiresAt,
      pwcOnly,
      me: undefined,
      endReason: undefined,
      status: pwcOnly ? 'password_change' : 'loading',
    })
  },

  restoreSession: ({ token, expiresAt, pwcOnly }) => {
    scheduleExpiry(expiresAt)
    set({ token, expiresAt, pwcOnly, me: undefined, endReason: undefined, status: 'loading' })
  },

  setMe: (me) => set({ me, status: 'authenticated' }),

  requirePasswordChange: () =>
    set((s) => {
      if (!s.token) return {}
      if (s.expiresAt) writeStored({ token: s.token, expiresAt: s.expiresAt, pwcOnly: true })
      return { pwcOnly: true, me: undefined, status: 'password_change' }
    }),

  endSession: (reason) => {
    clearTimeout(expiryTimer)
    expiryTimer = undefined
    removeStored()
    set({
      status: 'anonymous',
      token: undefined,
      expiresAt: undefined,
      pwcOnly: false,
      me: undefined,
      endReason: reason,
    })
  },

  clearEndReason: () => set({ endReason: undefined }),
}))
