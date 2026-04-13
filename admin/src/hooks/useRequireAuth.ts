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
