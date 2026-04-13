import { Observable } from 'rxjs'
import { ErrorLink } from '@apollo/client/link/error'
import { CombinedGraphQLErrors } from '@apollo/client/errors'
import type { ApolloLink } from '@apollo/client/core'
import { useAuthStore } from '@/store/auth'

// refreshAccessToken calls POST /auth/refresh.
// The browser automatically sends the httpOnly cookie (same-origin).
// Returns the new access token on success, null on failure.
async function refreshAccessToken(): Promise<string | null> {
  const store = useAuthStore.getState()

  // If already refreshing, wait instead of making two concurrent calls.
  // Two concurrent refresh calls would both try to use the same cookie,
  // and one would fail. Zustand's isRefreshing flag prevents this.
  if (store.isRefreshing) {
    // Wait for current refresh to finish
    await new Promise<void>((resolve) => {
      const unsub = useAuthStore.subscribe((s) => {
        if (!s.isRefreshing) {
          unsub()
          resolve()
        }
      })
    })
    return useAuthStore.getState().accessToken
  }

  store.setRefreshing(true)

  try {
    const resp = await fetch('/auth/refresh', {
      method: 'POST',
      credentials: 'include',          // sends the httpOnly cookie
      headers: {
        // Send the expired JWT so the server can extract user_id from it
        // (Member 2's refresh handler reads sub claim even from expired tokens)
        Authorization: `Bearer ${store.accessToken ?? ''}`,
      },
    })

    if (!resp.ok) {
      // Refresh failed — session is dead, redirect to login
      store.clearAuth()
      window.location.href = '/login'
      return null
    }

    const data = await resp.json()
    const newToken: string = data.access_token

    store.setAccessToken(newToken)
    return newToken

  } catch {
    store.clearAuth()
    window.location.href = '/login'
    return null
  } finally {
    store.setRefreshing(false)
  }
}

// Check if the error is an UNAUTHORIZED GraphQL error.
// In Apollo Client v4, GraphQL errors are wrapped in CombinedGraphQLErrors.
function isUnauthorizedError(error: unknown): boolean {
  if (CombinedGraphQLErrors.is(error)) {
    return error.errors.some((e) => (e.extensions?.code as string) === 'UNAUTHORIZED')
  }
  return false
}

// ErrorLink intercepts GraphQL errors.
// On UNAUTHORIZED error → attempt token refresh → retry the original operation.
// On any other error → pass through.
export const errorLink = new ErrorLink(({ error, operation, forward }: {
  error: unknown
  operation: ApolloLink.Operation
  forward: ApolloLink.ForwardFunction
}) => {
  if (!isUnauthorizedError(error)) return

  // Return an Observable that performs the refresh and retries the operation.
  // This is the Apollo Client v4 way — no fromPromise.
  return new Observable((observer) => {
    refreshAccessToken()
      .then((newToken) => {
        if (newToken === null) {
          // Refresh failed, complete without emitting
          observer.complete()
          return
        }

        // Update the header for the retry
        operation.setContext(({ headers = {} }) => ({
          headers: {
            ...headers,
            Authorization: `Bearer ${newToken}`,
          },
        }))

        // Retry the original operation
        forward(operation).subscribe({
          next: (result: ApolloLink.Result) => {
            observer.next(result)
            observer.complete()
          },
          error: (err: unknown) => observer.error(err),
        })
      })
      .catch(() => {
        observer.complete()
      })
  })
})
