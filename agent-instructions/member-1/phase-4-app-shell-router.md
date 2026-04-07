# Phase 4 — App Shell + Router

No backend needed. Sets up React entry point, routing, and the auth guard hook.
Protected routes render nothing until auth state is confirmed.

---

## File 1: `admin/src/main.tsx`

**Path:** `admin/src/main.tsx`

```tsx
import React from 'react'
import ReactDOM from 'react-dom/client'
import { ApolloProvider } from '@apollo/client'
import { BrowserRouter } from 'react-router-dom'
import { apolloClient } from '@/apollo/client'
import App from './App'
import './index.css'

ReactDOM.createRoot(document.getElementById('root')!).render(
  <React.StrictMode>
    <ApolloProvider client={apolloClient}>
      <BrowserRouter>
        <App />
      </BrowserRouter>
    </ApolloProvider>
  </React.StrictMode>
)
```

Provider order: `StrictMode` → `ApolloProvider` → `BrowserRouter` → `App`.
Apollo must wrap the router because hooks inside route components use Apollo queries.

---

## File 2: `admin/src/App.tsx`

**Path:** `admin/src/App.tsx`

```tsx
import { Routes, Route, Navigate } from 'react-router-dom'
import Login from '@/pages/Login'
import AuthCallback from '@/pages/AuthCallback'
import Dashboard from '@/pages/Dashboard'
import Settings from '@/pages/Settings'
import { AppShell } from '@/components/layout/AppShell'
import { useRequireAuth } from '@/hooks/useRequireAuth'

// ProtectedLayout wraps routes that require authentication.
// useRequireAuth redirects to /login if no token in store.
function ProtectedLayout() {
  const { isReady } = useRequireAuth()
  if (!isReady) return null // or a loading spinner
  return <AppShell />
}

export default function App() {
  return (
    <Routes>
      {/* Public routes */}
      <Route path="/login"         element={<Login />} />
      <Route path="/auth/callback" element={<AuthCallback />} />

      {/* Protected routes */}
      <Route element={<ProtectedLayout />}>
        <Route path="/"          element={<Navigate to="/dashboard" replace />} />
        <Route path="/dashboard" element={<Dashboard />} />
        <Route path="/settings"  element={<Settings />} />
      </Route>
    </Routes>
  )
}
```

### Route Structure

| Path | Auth Required | Component |
|------|---------------|-----------|
| `/login` | No | Login.tsx |
| `/auth/callback` | No | AuthCallback.tsx |
| `/` | Yes | Redirects to `/dashboard` |
| `/dashboard` | Yes | Dashboard.tsx |
| `/settings` | Yes | Settings.tsx |

`/auth/callback` MUST be public — it is the page where the JWT is read from the URL hash after Google OAuth redirect. No token exists yet at that point.

---

## File 3: `admin/src/hooks/useRequireAuth.ts`

**Path:** `admin/src/hooks/useRequireAuth.ts`

```typescript
import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuthStore } from '@/store/auth'

// useRequireAuth checks for a valid access token.
// If none exists, attempts a silent refresh via the httpOnly cookie.
// If refresh fails, redirects to /login.
//
// Returns { isReady: true } when auth state is confirmed.
// Protected pages should render nothing until isReady is true.
export function useRequireAuth() {
  const navigate = useNavigate()
  const { accessToken, isRefreshing } = useAuthStore()
  const [isReady, setIsReady] = useState(false)

  useEffect(() => {
    if (accessToken) {
      // Already have a token — ready immediately
      setIsReady(true)
      return
    }

    // No token in memory. Try silent refresh.
    // This handles the page reload case:
    //   - User had a valid session
    //   - Page was refreshed (memory cleared)
    //   - Refresh cookie still valid
    //   - Silent refresh restores the session
    async function trySilentRefresh() {
      try {
        const resp = await fetch('/auth/refresh', {
          method: 'POST',
          credentials: 'include',
          headers: {
            // No Authorization header here — we have no token yet.
            // Member 2's refresh handler handles missing token gracefully.
            Authorization: 'Bearer ',
          },
        })

        if (resp.ok) {
          const data = await resp.json()
          useAuthStore.getState().setAccessToken(data.access_token)
          setIsReady(true)
        } else {
          // Cookie expired or invalid — go to login
          navigate('/login', { replace: true })
        }
      } catch {
        navigate('/login', { replace: true })
      }
    }

    trySilentRefresh()
  }, [accessToken, navigate])

  return { isReady: isReady && !isRefreshing }
}
```

### Silent Refresh Flow (Page Reload)

```
User reloads page
  → Zustand store is empty (memory cleared)
  → useRequireAuth fires
  → No accessToken in store
  → POST /auth/refresh with httpOnly cookie
  → Cookie valid → new JWT returned → store updated → isReady = true
  → Cookie expired → navigate to /login
```

This prevents flash of protected content before redirect.
`ProtectedLayout` renders `null` until `isReady` is true.

---

## Verification Checklist

```
[x] /login renders without redirect
[x] /dashboard redirects to /login when no token in store
[x] /auth/callback is accessible without auth (public route)
[x] / redirects to /dashboard
[x] ProtectedLayout renders null when isReady is false
[x] useRequireAuth attempts POST /auth/refresh on page load if no token
[x] On refresh success: new token stored, isReady becomes true
[x] On refresh failure: redirect to /login
[x] No flash of protected content before redirect
```
