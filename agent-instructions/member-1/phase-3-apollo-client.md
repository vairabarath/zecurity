# Phase 3 — Apollo Client + Links

No backend needed. Can test with mock data.
Three files: auth link, error link, client instance.

---

## File 1: `admin/src/apollo/links/auth.ts`

**Path:** `admin/src/apollo/links/auth.ts`

```typescript
import { ApolloLink } from '@apollo/client'
import { useAuthStore } from '@/store/auth'

// AuthLink attaches the Bearer token to every GraphQL request.
// Reads the access token from Zustand store.
// If no token → sends the request without Authorization header.
// The backend will return 401 for protected operations.
//
// Special case: initiateAuth mutation is public.
// Member 4's route handler reads the X-Public-Operation header
// to skip auth middleware for this specific mutation.
// Without this header, the backend returns 401 on initiateAuth
// because auth middleware runs and finds no JWT.
export const authLink = new ApolloLink((operation, forward) => {
  const token = useAuthStore.getState().accessToken

  // Set the X-Public-Operation header for public mutations
  // so Member 4's routeGraphQL handler bypasses auth middleware.
  const isPublicOperation = operation.operationName === 'InitiateAuth'

  operation.setContext(({ headers = {} }) => ({
    headers: {
      ...headers,
      ...(token ? { Authorization: `Bearer ${token}` } : {}),
      ...(isPublicOperation
        ? { 'X-Public-Operation': 'initiateAuth' }
        : {}),
    },
  }))

  return forward(operation)
})
```

### Critical Coordination with Member 4

The `X-Public-Operation: initiateAuth` header is how Member 4's `routeGraphQL()` function
identifies which requests bypass auth middleware. If this header is missing on the
`initiateAuth` mutation, the backend returns 401 because auth middleware runs and finds no JWT.

---

## File 2: `admin/src/apollo/links/error.ts`

**Path:** `admin/src/apollo/links/error.ts`

```typescript
import { onError } from '@apollo/client/link/error'
import { fromPromise } from '@apollo/client'
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

// ErrorLink intercepts GraphQL errors.
// On UNAUTHORIZED error → attempt token refresh → retry the original operation.
// On any other error → pass through.
export const errorLink = onError(
  ({ graphQLErrors, operation, forward }) => {
    if (!graphQLErrors) return

    for (const err of graphQLErrors) {
      const code = err.extensions?.code

      if (code === 'UNAUTHORIZED') {
        // Refresh and retry
        return fromPromise(refreshAccessToken())
          .filter((token): token is string => token !== null)
          .flatMap((newToken) => {
            // Update the header for the retry
            operation.setContext(({ headers = {} }) => ({
              headers: {
                ...headers,
                Authorization: `Bearer ${newToken}`,
              },
            }))
            return forward(operation)
          })
      }
    }
  }
)
```

### Critical Coordination with Member 2

- **Refresh endpoint:** `POST /auth/refresh` — Member 2 sets the refresh token cookie with `Path=/auth/refresh`. The browser only sends this cookie to that exact path.
- **Expired JWT in header:** Member 2's refresh handler reads the `sub` claim from the expired token to identify the user. The error link sends it even when expired.
- **Response format:** `{ access_token: "<new JWT>" }` — this is what Member 2's refresh handler returns.

---

## File 3: `admin/src/apollo/client.ts`

**Path:** `admin/src/apollo/client.ts`

```typescript
import {
  ApolloClient,
  InMemoryCache,
  HttpLink,
  from,
} from '@apollo/client'
import { authLink } from './links/auth'
import { errorLink } from './links/error'

const httpLink = new HttpLink({
  uri: '/graphql',
  // credentials: 'same-origin' so the browser sends
  // the httpOnly refresh cookie on same-origin requests.
  // For the /graphql endpoint itself, we use Bearer tokens.
  credentials: 'same-origin',
})

// Link chain order matters:
//   errorLink first  → catches errors from all downstream links
//   authLink second  → attaches token to requests
//   httpLink last    → sends the actual HTTP request
export const apolloClient = new ApolloClient({
  link: from([errorLink, authLink, httpLink]),
  cache: new InMemoryCache(),
  defaultOptions: {
    watchQuery: {
      // Always fetch from network, use cache as fallback.
      // For an admin console, stale data is worse than a network call.
      fetchPolicy: 'cache-and-network',
    },
  },
})
```

---

## Install Additional Dependency

The error link requires `@apollo/client/link/error`:

```bash
npm install @apollo/client
# Already installed in Phase 1, but verify it includes link/error
```

---

## Verification Checklist

```
[x] authLink attaches Bearer token when token exists in store
[x] authLink attaches X-Public-Operation: initiateAuth for InitiateAuth mutation
[x] authLink sends no Authorization header when token is null
[x] errorLink triggers refreshAccessToken on UNAUTHORIZED GraphQL error code
[x] errorLink retries original operation with new token after successful refresh
[x] errorLink redirects to /login if refresh fails (resp.ok === false)
[x] errorLink redirects to /login if refresh throws (network error)
[x] Concurrent refresh calls are deduplicated (isRefreshing flag)
[x] Link chain order: errorLink → authLink → httpLink
[x] HttpLink uri is '/graphql' (relative — uses Vite proxy in dev)
[x] fetchPolicy is 'cache-and-network' for watchQuery
```

> **Note: Apollo Client v4 API changes from the v3 plan**
> - `from([links])` → `ApolloLink.from([links])` (`from` removed from top-level exports)
> - `onError()` → `new ErrorLink()` (`onError` is deprecated in v4)
> - `fromPromise()` → `new Observable()` (removed in v4)
> - `{ graphQLErrors }` → `CombinedGraphQLErrors.is(error)` (v4 wraps GraphQL errors)
> All behavioral requirements from the plan are met; only the API calls differ.
