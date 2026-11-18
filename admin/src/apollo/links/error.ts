import { Observable } from 'rxjs'
import { ErrorLink } from '@apollo/client/link/error'
import { ServerError } from '@apollo/client/errors'
import type { ApolloLink } from '@apollo/client/core'
import { useAuthStore } from '@/store/auth'
import { handleCapabilityRefusal, isCapabilityRefusal } from './refusal'
import { mfaRequiredKind } from '@/lib/mfa'
import { requestMfa } from '@/store/mfaPrompt'
import { isSuspendedRefresh, isTenantSuspendedError, SUSPENDED_LOGIN_PATH } from '@/lib/suspension'

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
      // Refresh failed — session is dead, redirect to login (with the
      // suspended message when the provider suspended the workspace).
      const suspended = await isSuspendedRefresh(resp)
      store.clearAuth()
      window.location.href = suspended ? SUSPENDED_LOGIN_PATH : '/login'
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

// Is this a token-expiry (refresh-worthy) error?
//
// The controller's AuthMiddleware returns HTTP 401 (application/json) for any
// missing / malformed / invalid / expired token. Apollo v4 surfaces every
// non-2xx response as a ServerError (it throws before parsing the body, so a 401
// is NEVER a CombinedGraphQLErrors), and ServerError exposes `.statusCode`. So a
// 401 ServerError is the single, reliable refresh trigger.
//
// Note: authorization failures from the @requires directive come back as HTTP 200
// GraphQL errors ("forbidden: requires capability …") — those must NOT trigger a
// refresh, since refreshing the token can't grant a missing capability. Keying
// on 401 only is correct; refusals are handled below.
function isUnauthorizedError(error: unknown): boolean {
  return ServerError.is(error) && error.statusCode === 401
}

type Observer = {
  next: (result: ApolloLink.Result) => void
  error: (err: unknown) => void
  complete: () => void
}

// PENDING-06: answers MFA_REQUIRED (console-wide 403 or a break-glass step-up)
// by opening the console's MFA prompt and, once a code is verified, sending
// the operation once more. authLink reads the token from the store on every
// request, so the retry carries the new MFA-marked token. The retry's own
// errors go straight to the caller: there is no second prompt for the same
// operation. If the user closes the prompt, the original error is passed on.
//
// Replaying a refused mutation is safe: the controller checks MFA before any
// write (RequireConsoleMFA before the resolver; RequireBreakGlassMFA before
// the resolver changes anything).
function promptAndRetry(
  kind: 'console' | 'step-up',
  original: unknown,
  operation: ApolloLink.Operation,
  forward: ApolloLink.ForwardFunction,
  observer: Observer,
) {
  requestMfa(kind)
    .then((ok) => {
      if (!ok) {
        observer.error(original)
        return
      }
      forward(operation).subscribe({
        next: (result: ApolloLink.Result) => observer.next(result),
        error: (err: unknown) => observer.error(err),
        complete: () => observer.complete(),
      })
    })
    .catch(() => observer.error(original))
}

// ErrorLink intercepts GraphQL errors.
// On UNAUTHORIZED error → attempt token refresh → retry the original operation.
// On MFA_REQUIRED → open the MFA prompt → retry the original operation.
// On a capability refusal → tell the user and re-read `me` (the error still
// passes through to the caller).
// On any other error → pass through.
export const errorLink = new ErrorLink(({ error, operation, forward }: {
  error: unknown
  operation: ApolloLink.Operation
  forward: ApolloLink.ForwardFunction
}) => {
  // Sprint 23 S1 (D-45): the provider suspended the workspace. The session
  // is over: no retry, no refresh; the login page says why.
  if (isTenantSuspendedError(error)) {
    useAuthStore.getState().clearAuth()
    window.location.href = SUSPENDED_LOGIN_PATH
    return
  }
  if (isCapabilityRefusal(error)) {
    void handleCapabilityRefusal()
    return
  }
  const mfaKind = mfaRequiredKind(error)
  if (mfaKind) {
    return new Observable<ApolloLink.Result>((observer) => {
      promptAndRetry(mfaKind, error, operation, forward, observer)
    })
  }
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
          error: (err: unknown) => {
            // A refreshed session can still lack MFA (console-wide MFA):
            // this retry bypasses the handler above, so check here too.
            const kind = mfaRequiredKind(err)
            if (kind) promptAndRetry(kind, err, operation, forward, observer)
            else observer.error(err)
          },
        })
      })
      .catch(() => {
        observer.complete()
      })
  })
})
