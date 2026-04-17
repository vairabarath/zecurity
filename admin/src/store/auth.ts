import { create } from 'zustand'
import type { MeQuery } from '@/generated/graphql'

// Access token is stored in sessionStorage so it survives page refreshes
// within the same tab but is cleared when the tab is closed.
// localStorage is avoided due to XSS risk (any JS on the page can read it).
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
