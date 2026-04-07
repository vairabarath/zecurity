# Phase 5 — Auth Pages (Login + AuthCallback)

Needs `schema.graphqls` from Member 4 Phase 1 for codegen hooks.
Full end-to-end test needs Member 2 running.
Can be tested manually without backend (see testing section below).

---

## File 1: `admin/src/pages/Login.tsx`

**Path:** `admin/src/pages/Login.tsx`

```tsx
import { useNavigate } from 'react-router-dom'
import { useInitiateAuthMutation } from '@/generated/graphql'
import { Button } from '@/components/ui/button'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'

// Login page.
// Single action: "Sign in with Google".
// Calls initiateAuth mutation → gets redirectUrl → redirects browser there.
//
// No form, no password, no email input.
// Google handles all identity verification.
export default function Login() {
  const navigate = useNavigate()
  const [initiateAuth, { loading, error }] = useInitiateAuthMutation()

  async function handleSignIn() {
    try {
      const result = await initiateAuth({
        variables: { provider: 'google' },
        // X-Public-Operation header is set by authLink automatically
        // because operation name is "InitiateAuth"
      })

      const { redirectUrl, state } = result.data!.initiateAuth

      // Store state for CSRF verification.
      // We store in sessionStorage (not localStorage) because:
      //   - sessionStorage is cleared when the tab closes
      //   - It only needs to survive the OAuth redirect round-trip
      //   - localStorage would persist state across sessions (unnecessary)
      // Note: Member 2's callback handler verifies state via HMAC,
      // but we still store it here in case we need to compare it.
      sessionStorage.setItem('oauth_state', state)

      // Redirect the browser to Google's auth page.
      // This is a full browser redirect — not fetch, not axios.
      // The current page unloads. Google handles auth.
      // Google will redirect back to /auth/callback on our server.
      window.location.href = redirectUrl

    } catch (e) {
      // Error is shown via Apollo's error state below
      console.error('initiateAuth failed:', e)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-background">
      <Card className="w-full max-w-md">
        <CardHeader>
          <CardTitle className="text-center">ZTNA Admin Console</CardTitle>
        </CardHeader>
        <CardContent className="flex flex-col gap-4">
          {error && (
            <p className="text-destructive text-sm text-center">
              Sign in failed. Please try again.
            </p>
          )}
          <Button
            onClick={handleSignIn}
            disabled={loading}
            className="w-full"
          >
            {loading ? 'Redirecting...' : 'Sign in with Google'}
          </Button>
        </CardContent>
      </Card>
    </div>
  )
}
```

### Login Flow

```
User clicks "Sign in with Google"
  → initiateAuth mutation fires
  → authLink adds X-Public-Operation: initiateAuth header (bypasses auth middleware)
  → Go returns { redirectUrl, state }
  → state stored in sessionStorage
  → browser redirects to Google OAuth URL
  → Google authenticates user
  → Google redirects to /auth/callback on our server (Member 2 handles this)
  → Member 2 exchanges code, issues JWT, redirects to /#token=<JWT>
```

---

## File 2: `admin/src/pages/AuthCallback.tsx`

**Path:** `admin/src/pages/AuthCallback.tsx`

```tsx
import { useEffect, useRef } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuthStore } from '@/store/auth'
import { apolloClient } from '@/apollo/client'
import { MeDocument } from '@/generated/graphql'
import type { MeQuery } from '@/generated/graphql'

// AuthCallback handles the return from Google OAuth.
//
// Member 2's callback handler redirects the browser to:
//   /#token=<JWT>
//
// React Router renders this component for /auth/callback.
// But the JWT is in the hash fragment (#token=...) NOT in the path.
// The hash is window.location.hash, not the route path.
// It is NEVER sent to the server — only the browser can see it.
//
// This component:
//   1. Reads the JWT from window.location.hash
//   2. Clears the hash from the URL (so JWT doesn't stay visible)
//   3. Stores JWT in Zustand (memory only)
//   4. Calls me query to load user data
//   5. Redirects to /dashboard
//
// If anything fails → redirect to /login?error=...
export default function AuthCallback() {
  const navigate = useNavigate()
  const { setAccessToken, setUser } = useAuthStore()
  // useRef prevents double-execution in React Strict Mode
  const handled = useRef(false)

  useEffect(() => {
    if (handled.current) return
    handled.current = true

    async function handleCallback() {
      // Step 1 — Read JWT from URL hash
      // window.location.hash = "#token=eyJ..."
      const hash = window.location.hash

      if (!hash || !hash.startsWith('#token=')) {
        // No token in hash — could be an error redirect or direct navigation
        const params = new URLSearchParams(window.location.search)
        const error = params.get('error')
        navigate(`/login${error ? `?error=${error}` : ''}`, { replace: true })
        return
      }

      const token = hash.slice('#token='.length)

      // Step 2 — Clear hash from URL
      // replaceState removes #token=... without triggering a navigation.
      // The JWT is no longer visible in the address bar or browser history.
      window.history.replaceState(null, '', window.location.pathname)

      // Step 3 — Store JWT in Zustand (memory only)
      setAccessToken(token)

      // Step 4 — Load user data with the new token
      // apolloClient will use the token via authLink on this query.
      // We call directly (not a hook) because we need to await it
      // before deciding where to navigate.
      try {
        const result = await apolloClient.query<MeQuery>({
          query: MeDocument,
          fetchPolicy: 'network-only', // always fresh after login
        })
        setUser(result.data.me)

        // Step 5 — Navigate to dashboard
        navigate('/dashboard', { replace: true })

      } catch {
        // Token was issued but me query failed.
        // Something is wrong — clear auth and restart.
        useAuthStore.getState().clearAuth()
        navigate('/login?error=session_failed', { replace: true })
      }
    }

    handleCallback()
  }, [navigate, setAccessToken, setUser])

  // Render nothing visible — this page exists only for the redirect logic.
  // A brief flash before navigating to dashboard.
  return (
    <div className="min-h-screen flex items-center justify-center">
      <p className="text-muted-foreground text-sm">Completing sign in...</p>
    </div>
  )
}
```

### Critical Coordination with Member 2

**The JWT is in the URL hash fragment, NOT in query params.**

Member 2's `/auth/callback` handler redirects to:
```
302 → https://<domain>/auth/callback#token=<JWT>
```

The hash fragment (`#token=...`) is NEVER sent to the server.
Only the browser (JavaScript) can read it via `window.location.hash`.
This is a security design — the token never appears in server logs.

If Member 1 reads from `window.location.search` instead of `window.location.hash`,
login will silently do nothing. This is the most common integration bug.

---

## How to Test Before Backend Is Ready

**Testing AuthCallback without a real OAuth flow:**

```typescript
// Navigate to /auth/callback in the browser
// Open console, set the hash manually:
window.location.hash = '#token=fake-test-token'
// AuthCallback reads this, stores it, calls me query
// me query fails (no real server) → redirects to /login?error=session_failed
// That's the correct fallback behavior
```

**Testing the auth store integration:**

```typescript
// In browser console
import { useAuthStore } from '@/store/auth'
useAuthStore.getState().setAccessToken('fake-jwt-for-testing')
useAuthStore.getState().setUser({
  id: '1', email: 'test@example.com',
  role: 'ADMIN', provider: 'google', createdAt: new Date().toISOString()
})
// Now navigate to /dashboard — it should render with this data
```

---

## Verification Checklist

```
[x] Login button calls initiateAuth mutation
[x] X-Public-Operation header present on the initiateAuth request (check DevTools Network tab)
[x] On mutation success: window.location.href set to redirectUrl
[x] state stored in sessionStorage before redirect
[x] Error state shown if mutation fails
[x] AuthCallback reads JWT from window.location.hash (NOT query params)
[x] Hash cleared from URL after reading (replaceState)
[x] Token stored in Zustand (NOT localStorage)
[x] me query called after token is stored
[x] On me query success: navigate to /dashboard
[x] On me query failure: clearAuth + navigate to /login?error=session_failed
[x] Double execution prevented (useRef guard for React Strict Mode)
```

> **Note: Apollo Client v4 API change from the v3 plan**
> - `useInitiateAuthMutation()` hook → `useMutation(InitiateAuthDocument)` (codegen no longer generates React hooks in v4 + client preset)
> - All behavioral requirements from the plan are preserved; only the import/hook pattern differs.
