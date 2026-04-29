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
        setUser(result.data!.me)

        // Step 5 — Accept invite if this login came from an invite link
        const inviteToken = sessionStorage.getItem('ztna_invite_token')
        if (inviteToken) {
          sessionStorage.removeItem('ztna_invite_token')
          const jwt = useAuthStore.getState().accessToken
          await fetch(`/api/invitations/${inviteToken}/accept`, {
            method: 'POST',
            headers: { Authorization: `Bearer ${jwt}` },
          })
          // Errors are intentionally ignored — expired or already-accepted
          // invitations should not block the user from logging in.
        }

        // Step 6 — Role-based redirect
        if (result.data!.me.role === 'ADMIN') {
          navigate('/dashboard', { replace: true })
        } else {
          navigate('/client-install', { replace: true })
        }

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
