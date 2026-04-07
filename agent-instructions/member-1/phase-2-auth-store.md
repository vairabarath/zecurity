# Phase 2 — Auth Store (Zustand)

No backend needed. Build this immediately after Phase 1.
This store holds the JWT and user data in memory only.

---

## File 1: `admin/src/store/auth.ts`

**Path:** `admin/src/store/auth.ts`

```typescript
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
```

---

## Design Decisions

1. **Memory only, no persist middleware** — JWT never touches localStorage or sessionStorage. XSS cannot steal it.
2. **`isRefreshing` flag** — Prevents two concurrent refresh calls. Two calls would both try to use the same cookie, and one would fail.
3. **`MeQuery['me']` type** — Derived from codegen output. If schema changes, TypeScript immediately catches type mismatches.
4. **`clearAuth()` resets everything** — Called on sign out and on refresh failure. No partial state possible.

---

## Verification Checklist

```
[x] setAccessToken stores token in memory (not localStorage)
[x] clearAuth resets all state to null/false
[x] Zustand store is NOT persisted (no persist middleware used)
[x] MeQuery type imports correctly from generated/graphql.ts
[x] isRefreshing flag defaults to false
```
