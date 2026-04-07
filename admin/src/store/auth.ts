import { create } from 'zustand'
import type { MeQuery } from '@/generated/graphql'

// AuthState holds the JWT and user data in memory.
// NEVER persisted to localStorage or sessionStorage.
// If the page reloads, the user goes through the refresh flow.
// If refresh fails, they go back to login.
//
// This is intentional:
//   localStorage is accessible to any JavaScript on the page (XSS risk)
//   Memory is cleared on page close — forces re-auth for fresh sessions
//   The httpOnly refresh cookie handles transparent re-auth on reload
interface AuthState {
  // The access JWT. null = not authenticated.
  accessToken: string | null

  // The current user. null = not authenticated or not yet loaded.
  user: MeQuery['me'] | null

  // True while the refresh flow is running.
  // Prevents multiple concurrent refresh attempts.
  isRefreshing: boolean

  // Actions
  setAccessToken: (token: string) => void
  setUser: (user: MeQuery['me']) => void
  setRefreshing: (v: boolean) => void
  clearAuth: () => void
}

export const useAuthStore = create<AuthState>((set) => ({
  accessToken: null,
  user: null,
  isRefreshing: false,

  setAccessToken: (token) => set({ accessToken: token }),
  setUser: (user) => set({ user }),
  setRefreshing: (v) => set({ isRefreshing: v }),

  clearAuth: () => set({
    accessToken: null,
    user: null,
    isRefreshing: false,
  }),
}))
