import { useEffect, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useAuthStore } from '@/store/auth'
import { apolloClient } from '@/apollo/client'
import { MeDocument, type MeQuery } from '@/generated/graphql'

// useRequireAuth guards protected routes.
//
// On mount it always probes the session by running MeQuery:
//   - no token in store       → silent refresh via httpOnly cookie, then probe
//   - probe succeeds          → hydrate user, mark ready
//   - probe 401s              → errorLink refreshes and retries; if that fails
//                               it clears auth and redirects itself
//   - probe fails for any
//     other reason             → clearAuth + redirect to /login
//
// This fixes two bugs that shared a root cause:
//   1. After a browser reload the Zustand `user` was null (in-memory only),
//      so Header rendered "??" instead of the email initials.
//   2. A stale access token in sessionStorage kept the user on the dashboard
//      even when the session was actually dead.
export function useRequireAuth() {
  const navigate = useNavigate()
  const { accessToken, isRefreshing } = useAuthStore()
  const [isReady, setIsReady] = useState(false)

  useEffect(() => {
    let cancelled = false

    async function probeMe() {
      try {
        const result = await apolloClient.query<MeQuery>({
          query: MeDocument,
          fetchPolicy: 'network-only',
        })
        if (cancelled) return
        if (!result.data?.me) {
          useAuthStore.getState().clearAuth()
          navigate('/login', { replace: true })
          return
        }
        useAuthStore.getState().setUser(result.data.me)
        setIsReady(true)
      } catch {
        if (cancelled) return
        // errorLink handles the 401 → refresh path on its own. If we still
        // reach this catch, refresh failed or some other error occurred.
        useAuthStore.getState().clearAuth()
        navigate('/login', { replace: true })
      }
    }

    async function trySilentRefresh() {
      try {
        const resp = await fetch('/auth/refresh', {
          method: 'POST',
          credentials: 'include',
          headers: {
            Authorization: 'Bearer ',
          },
        })
        if (!resp.ok) {
          if (cancelled) return
          navigate('/login', { replace: true })
          return
        }
        const data = await resp.json()
        if (cancelled) return
        useAuthStore.getState().setAccessToken(data.access_token)
        await probeMe()
      } catch {
        if (cancelled) return
        navigate('/login', { replace: true })
      }
    }

    if (accessToken) {
      probeMe()
    } else {
      trySilentRefresh()
    }

    return () => {
      cancelled = true
    }
  }, [accessToken, navigate])

  return { isReady: isReady && !isRefreshing }
}
