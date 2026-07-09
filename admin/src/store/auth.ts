import { create } from 'zustand'
import type { MeQuery } from '@/generated/graphql'

// Access token is stored in sessionStorage (JS-readable, per-tab, cleared on tab close).
// Security posture: sessionStorage does NOT reduce XSS read risk versus localStorage —
// any active XSS can read both via JS. The actual XSS mitigations are:
//   1. Short TTL (15 min) — limits the window a stolen token is valid.
//   2. httpOnly refresh cookie — the long-lived credential is XSS-proof.
//   3. sessionStorage is per-tab (not shared across tabs) and cleared on close,
//      which limits persistence but does not prevent in-page exfiltration.
// See CodeStudy/04-Connector-Enrollment-Flow.md F6 for full threat model.
const SESSION_KEY = 'ztna_access_token'

interface AuthState {
  accessToken: string | null
  user: MeQuery['me'] | null
  isRefreshing: boolean

  setAccessToken: (token: string) => void
  setUser: (user: MeQuery['me']) => void
  setRefreshing: (v: boolean) => void
  clearAuth: () => void
}

export const useAuthStore = create<AuthState>((set) => ({
  accessToken: sessionStorage.getItem(SESSION_KEY),
  user: null,
  isRefreshing: false,

  setAccessToken: (token) => {
    sessionStorage.setItem(SESSION_KEY, token)
    set({ accessToken: token })
  },
  setUser: (user) => set({ user }),
  setRefreshing: (v) => set({ isRefreshing: v }),

  clearAuth: () => {
    sessionStorage.removeItem(SESSION_KEY)
    set({ accessToken: null, user: null, isRefreshing: false })
  },
}))
