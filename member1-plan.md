# Member 1 — Deep Implementation Plan
## React · TypeScript · Vite · shadcn/ui · Apollo Client · GraphQL Codegen

---

## Role on the Team

Member 1 owns the entire admin frontend.
One person, one codebase, one responsibility: the React admin console.

Member 1 consumes what the backend defines.
Member 1 does NOT invent API contracts.
If something is not in `schema.graphqls`, it does not exist in the frontend.

---

## Hard Dependencies — What Member 1 Waits For

### Must wait for Member 4 Phase 1:

**`controller/graph/schema.graphqls`** — without this, codegen cannot run
and there are no TypeScript types. This is the only hard blocker.
The moment Member 4 commits this file, Member 1 starts.

That is the only wait. Everything else can be built before the
backend is running. Apollo Client can run against a mock server
while the backend is being completed.

### Must coordinate with Member 2 before testing the login flow:

**The redirect URL** — Member 2's callback handler redirects to
`/#token=<JWT>` (URL fragment). Member 1 must read from
`window.location.hash`, not from query params, not from the body.
If Member 1 reads from the wrong place, login silently does nothing.

**The `X-Public-Operation` header** — Member 4's route handler
reads `X-Public-Operation: initiateAuth` to skip auth middleware
for the `initiateAuth` mutation. Member 1 must set this header
on that specific mutation. If Member 1 forgets it, the mutation
returns 401 because auth middleware runs and finds no JWT.

**The refresh cookie path** — Member 2 sets the refresh token cookie
with `Path=/auth/refresh`. The browser only sends this cookie to
`POST /auth/refresh`. Member 1 must call that exact path for refresh.

---

## Everything Member 1 Owns

```
admin/
  index.html
  vite.config.ts
  tsconfig.json
  package.json
  codegen.yml
  tailwind.config.ts
  components.json                  ← shadcn/ui config
  src/
    main.tsx                       ← React root, providers
    App.tsx                        ← router, route definitions
    store/
      auth.ts                      ← Zustand store for JWT + user state
    apollo/
      client.ts                    ← Apollo Client instance
      links/
        auth.ts                    ← attaches Bearer token to requests
        error.ts                   ← handles 401 → triggers refresh
    graphql/
      mutations.graphql            ← initiateAuth
      queries.graphql              ← me, workspace
    generated/                     ← DO NOT EDIT — codegen output
      graphql.ts
    pages/
      Login.tsx                    ← initiateAuth → redirect → read hash
      AuthCallback.tsx             ← reads JWT from hash, boots session
      Dashboard.tsx                ← me + workspace queries
      Settings.tsx                 ← workspace info
    components/
      layout/
        AppShell.tsx               ← sidebar + header wrapper
        Sidebar.tsx                ← nav links
        Header.tsx                 ← user menu, sign out
      ui/                          ← shadcn/ui component re-exports
    hooks/
      useAuth.ts                   ← read/write auth state
      useRequireAuth.ts            ← redirect to login if not authed
    lib/
      utils.ts                     ← shadcn/ui cn() utility
```

---

## Build Order — Strictly by Dependency

### Phase 1 — Project Setup (No Backend Needed)

Everything here runs before the backend exists.

**Step 1 — Scaffold project**

```bash
npm create vite@latest admin -- --template react-ts
cd admin
npm install
```

**Step 2 — Install dependencies**

```bash
# Apollo Client + GraphQL
npm install @apollo/client graphql

# State management
npm install zustand

# Router
npm install react-router-dom

# shadcn/ui dependencies
npm install tailwindcss @tailwindcss/vite
npm install class-variance-authority clsx tailwind-merge
npm install lucide-react

# shadcn/ui CLI (run once to init)
npx shadcn@latest init

# GraphQL codegen
npm install -D @graphql-codegen/cli
npm install -D @graphql-codegen/client-preset
```

**Step 3 — shadcn/ui init**

When `npx shadcn@latest init` runs, answer:
- Style: Default
- Base color: Slate
- CSS variables: Yes

Then add the components needed for admin UI:
```bash
npx shadcn@latest add button card badge avatar
npx shadcn@latest add dropdown-menu separator skeleton
npx shadcn@latest add toast alert
```

**Step 4 — Vite config**

```typescript
// vite.config.ts
import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import path from 'path'

export default defineConfig({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, './src'),
    },
  },
  server: {
    port: 5173,
    // Proxy API calls to Go controller in development
    // so CORS is not needed during local dev
    proxy: {
      '/graphql': 'http://localhost:8080',
      '/auth':    'http://localhost:8080',
    },
  },
})
```

The proxy is critical for local development. Without it, the browser
blocks requests to `localhost:8080` from `localhost:5173` due to CORS.
The proxy makes all API calls appear same-origin to the browser.

**Step 5 — Codegen config**

```yaml
# codegen.yml
schema: '../controller/graph/schema.graphqls'
documents: 'src/graphql/**/*.graphql'
generates:
  src/generated/graphql.ts:
    preset: client
    config:
      scalars:
        ID: string
```

The schema path `'../controller/graph/schema.graphqls'` points directly
at Member 4's file. When Member 4 changes the schema, Member 1 runs:

```bash
npx graphql-codegen
```

TypeScript compiler immediately shows what broke. No manual syncing.

Add to `package.json` scripts:
```json
{
  "scripts": {
    "dev": "vite",
    "build": "tsc && vite build",
    "codegen": "graphql-codegen",
    "codegen:watch": "graphql-codegen --watch"
  }
}
```

**Step 6 — Write GraphQL operation files**

These are the only GraphQL files Member 1 writes.
Everything else is generated.

```graphql
# src/graphql/mutations.graphql

mutation InitiateAuth($provider: String!) {
  initiateAuth(provider: $provider) {
    redirectUrl
    state
  }
}
```

```graphql
# src/graphql/queries.graphql

query Me {
  me {
    id
    email
    role
    provider
    createdAt
  }
}

query GetWorkspace {
  workspace {
    id
    slug
    name
    status
    createdAt
  }
}
```

Run codegen now. This generates `src/generated/graphql.ts` with:
- `InitiateAuthMutation` type
- `InitiateAuthDocument` (the typed query document)
- `MeQuery` type
- `GetWorkspaceQuery` type
- All enum types: `Role`, `WorkspaceStatus`
- React hooks: `useInitiateAuthMutation`, `useMeQuery`, `useGetWorkspaceQuery`

Member 1 imports these hooks directly. Never writes raw gql`` strings.

---

### Phase 2 — Auth Store (Zustand)

**src/store/auth.ts**

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

### Phase 3 — Apollo Client + Links

**src/apollo/links/auth.ts**

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

**src/apollo/links/error.ts**

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

**src/apollo/client.ts**

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
  // credentials: 'include' is needed so the browser sends
  // the httpOnly refresh cookie on the /auth/refresh call.
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

### Phase 4 — App Shell + Router

**src/main.tsx**

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

**src/App.tsx**

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

**src/hooks/useRequireAuth.ts**

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

---

### Phase 5 — Auth Pages

**src/pages/Login.tsx**

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

**src/pages/AuthCallback.tsx**

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

---

### Phase 6 — Protected Layout + Nav

**src/components/layout/AppShell.tsx**

```tsx
import { Outlet } from 'react-router-dom'
import { Sidebar } from './Sidebar'
import { Header } from './Header'

// AppShell wraps all protected pages.
// Outlet renders the active route's page component.
export function AppShell() {
  return (
    <div className="flex h-screen overflow-hidden bg-background">
      <Sidebar />
      <div className="flex flex-col flex-1 overflow-hidden">
        <Header />
        <main className="flex-1 overflow-y-auto p-6">
          <Outlet />
        </main>
      </div>
    </div>
  )
}
```

**src/components/layout/Sidebar.tsx**

```tsx
import { NavLink } from 'react-router-dom'
import { LayoutDashboard, Settings } from 'lucide-react'
import { cn } from '@/lib/utils'

const nav = [
  { to: '/dashboard', label: 'Dashboard', icon: LayoutDashboard },
  { to: '/settings',  label: 'Settings',  icon: Settings },
]

export function Sidebar() {
  return (
    <aside className="w-56 border-r flex flex-col bg-card">
      <div className="p-4 border-b">
        <span className="font-semibold text-sm">ZTNA Console</span>
      </div>
      <nav className="flex-1 p-2 space-y-1">
        {nav.map(({ to, label, icon: Icon }) => (
          <NavLink
            key={to}
            to={to}
            className={({ isActive }) =>
              cn(
                'flex items-center gap-2 px-3 py-2 rounded-md text-sm transition-colors',
                isActive
                  ? 'bg-primary text-primary-foreground'
                  : 'text-muted-foreground hover:bg-accent hover:text-accent-foreground'
              )
            }
          >
            <Icon className="w-4 h-4" />
            {label}
          </NavLink>
        ))}
      </nav>
    </aside>
  )
}
```

**src/components/layout/Header.tsx**

```tsx
import { useNavigate } from 'react-router-dom'
import { useAuthStore } from '@/store/auth'
import { apolloClient } from '@/apollo/client'
import { Avatar, AvatarFallback } from '@/components/ui/avatar'
import {
  DropdownMenu,
  DropdownMenuContent,
  DropdownMenuItem,
  DropdownMenuTrigger,
} from '@/components/ui/dropdown-menu'
import { Badge } from '@/components/ui/badge'

export function Header() {
  const navigate = useNavigate()
  const { user, clearAuth } = useAuthStore()

  async function handleSignOut() {
    // Clear Apollo cache so no stale data persists
    await apolloClient.clearStore()
    clearAuth()
    navigate('/login', { replace: true })
    // Note: this does NOT call a sign-out endpoint.
    // The refresh cookie will expire on its own (7 days).
    // A sign-out endpoint that deletes the Redis refresh key
    // can be added later for immediate invalidation.
  }

  const initials = user?.email?.slice(0, 2).toUpperCase() ?? '??'

  return (
    <header className="h-14 border-b flex items-center justify-between px-6">
      <div />
      <div className="flex items-center gap-3">
        {user && (
          <Badge variant="outline" className="text-xs">
            {user.role}
          </Badge>
        )}
        <DropdownMenu>
          <DropdownMenuTrigger asChild>
            <Avatar className="cursor-pointer h-8 w-8">
              <AvatarFallback className="text-xs">{initials}</AvatarFallback>
            </Avatar>
          </DropdownMenuTrigger>
          <DropdownMenuContent align="end">
            <div className="px-2 py-1.5 text-xs text-muted-foreground">
              {user?.email}
            </div>
            <DropdownMenuItem onClick={handleSignOut}>
              Sign out
            </DropdownMenuItem>
          </DropdownMenuContent>
        </DropdownMenu>
      </div>
    </header>
  )
}
```

---

### Phase 7 — Dashboard + Settings Pages

**src/pages/Dashboard.tsx**

```tsx
import { useMeQuery, useGetWorkspaceQuery, WorkspaceStatus } from '@/generated/graphql'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Skeleton } from '@/components/ui/skeleton'

// Status badge color mapping
const statusVariant: Record<WorkspaceStatus, 'default' | 'secondary' | 'destructive' | 'outline'> = {
  [WorkspaceStatus.Active]:       'default',
  [WorkspaceStatus.Provisioning]: 'secondary',
  [WorkspaceStatus.Suspended]:    'destructive',
  [WorkspaceStatus.Deleted]:      'outline',
}

export default function Dashboard() {
  const { data: meData,        loading: meLoading }  = useMeQuery()
  const { data: wsData,        loading: wsLoading }  = useGetWorkspaceQuery()

  return (
    <div className="space-y-6">
      <h1 className="text-xl font-semibold">Dashboard</h1>

      <div className="grid grid-cols-1 md:grid-cols-2 gap-4">

        {/* User card */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Your Account</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            {meLoading ? (
              <>
                <Skeleton className="h-4 w-48" />
                <Skeleton className="h-4 w-24" />
              </>
            ) : (
              <>
                <p className="text-sm">{meData?.me.email}</p>
                <Badge variant="outline">{meData?.me.role}</Badge>
              </>
            )}
          </CardContent>
        </Card>

        {/* Workspace card */}
        <Card>
          <CardHeader>
            <CardTitle className="text-sm font-medium">Workspace</CardTitle>
          </CardHeader>
          <CardContent className="space-y-2">
            {wsLoading ? (
              <>
                <Skeleton className="h-4 w-48" />
                <Skeleton className="h-4 w-24" />
              </>
            ) : (
              <>
                <p className="text-sm font-medium">{wsData?.workspace.name}</p>
                <p className="text-xs text-muted-foreground">
                  {wsData?.workspace.slug}
                </p>
                <Badge variant={statusVariant[wsData?.workspace.status!]}>
                  {wsData?.workspace.status}
                </Badge>
              </>
            )}
          </CardContent>
        </Card>

      </div>
    </div>
  )
}
```

**src/pages/Settings.tsx**

```tsx
import { useGetWorkspaceQuery } from '@/generated/graphql'
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Skeleton } from '@/components/ui/skeleton'

export default function Settings() {
  const { data, loading } = useGetWorkspaceQuery()

  return (
    <div className="space-y-6">
      <h1 className="text-xl font-semibold">Settings</h1>

      <Card>
        <CardHeader>
          <CardTitle className="text-sm font-medium">Workspace Info</CardTitle>
        </CardHeader>
        <CardContent>
          {loading ? (
            <div className="space-y-2">
              <Skeleton className="h-4 w-48" />
              <Skeleton className="h-4 w-64" />
            </div>
          ) : (
            <dl className="space-y-3 text-sm">
              <div className="flex gap-4">
                <dt className="text-muted-foreground w-24">Name</dt>
                <dd>{data?.workspace.name}</dd>
              </div>
              <div className="flex gap-4">
                <dt className="text-muted-foreground w-24">Slug</dt>
                <dd className="font-mono text-xs">{data?.workspace.slug}</dd>
              </div>
              <div className="flex gap-4">
                <dt className="text-muted-foreground w-24">ID</dt>
                <dd className="font-mono text-xs">{data?.workspace.id}</dd>
              </div>
              <div className="flex gap-4">
                <dt className="text-muted-foreground w-24">Created</dt>
                <dd>{new Date(data?.workspace.createdAt!).toLocaleDateString()}</dd>
              </div>
            </dl>
          )}
        </CardContent>
      </Card>
    </div>
  )
}
```

---

## Dependency Map — What Blocks What

```
Can start immediately (no backend needed):
  Phase 1 — project setup, Vite, shadcn/ui, packages
  Phase 2 — auth store (Zustand)
  Phase 3 — Apollo client + links (can mock the token)
  Phase 4 — AppShell + router structure

Needs Member 4 schema before:
  Writing graphql/*.graphql files
  Running codegen
  Using generated hooks in pages
  → This is the only hard blocker. Everything else builds first.

Needs Member 2 running before end-to-end login test:
  The callback redirect (/#token=...) only works when Member 2's
  callback handler is running and doing the real OAuth flow.
  Until then, AuthCallback.tsx can be tested by manually setting:
    window.location.hash = '#token=<test-jwt>'
  and verifying the me query fires correctly.

Needs Member 4 running before GraphQL queries work:
  me query and workspace query need a real server.
  Until then, Apollo's mock link can return hardcoded data:
    const mockUser = { id: '1', email: 'test@test.com', role: Role.Admin, ... }
```

---

## How to Test Before Backend Is Ready

Member 1 does not wait for the backend to test UI logic.

**Testing the auth store:**
```typescript
// In browser console or a test file
import { useAuthStore } from '@/store/auth'
useAuthStore.getState().setAccessToken('fake-jwt-for-testing')
useAuthStore.getState().setUser({
  id: '1', email: 'test@example.com',
  role: 'ADMIN', provider: 'google', createdAt: new Date().toISOString()
})
// Now navigate to /dashboard — it should render with this data
```

**Testing AuthCallback without a real OAuth flow:**
```typescript
// Navigate to /auth/callback and set the hash manually in console
window.location.hash = '#token=fake-test-token'
// AuthCallback reads this, stores it, calls me query
// me query fails (no real server) → redirects to /login?error=session_failed
// That's the correct fallback behavior
```

**Testing token refresh without a real server:**
The error link calls `/auth/refresh` on 401.
In development, you can mock the refresh endpoint in vite.config.ts:
```typescript
proxy: {
  '/auth/refresh': {
    target: 'http://localhost:8080',
    // or mock it directly:
    bypass: (req) => {
      if (req.method === 'POST') {
        // Return a fake new token for UI testing
      }
    }
  }
}
```

---

## Integration Checklist

```
Phase 1 — Project setup
  ✓ npm run dev starts without errors on localhost:5173
  ✓ codegen.yml points at ../controller/graph/schema.graphqls
  ✓ npm run codegen generates src/generated/graphql.ts
  ✓ generated file contains: useInitiateAuthMutation, useMeQuery,
    useGetWorkspaceQuery, Role enum, WorkspaceStatus enum
  ✓ Vite proxy forwards /graphql and /auth to localhost:8080

Phase 2 — Auth store
  ✓ setAccessToken stores token in memory (not localStorage)
  ✓ clearAuth resets all state
  ✓ Zustand store is NOT persisted (no persist middleware)

Phase 3 — Apollo links
  ✓ authLink attaches Bearer token when token exists
  ✓ authLink attaches X-Public-Operation: initiateAuth for InitiateAuth mutation
  ✓ authLink sends no Authorization header when token is null
  ✓ errorLink triggers refresh on UNAUTHORIZED GraphQL error
  ✓ errorLink retries original operation with new token after refresh
  ✓ errorLink redirects to /login if refresh fails
  ✓ concurrent refresh calls are deduplicated (isRefreshing flag)

Phase 4 — Router + shell
  ✓ /login renders without redirect
  ✓ /dashboard redirects to /login when no token in store
  ✓ /auth/callback is accessible without auth
  ✓ AppShell renders Sidebar + Header + Outlet
  ✓ NavLink active state highlights current route

Phase 5 — Login page
  ✓ Button calls initiateAuth mutation
  ✓ X-Public-Operation header present on the request (check DevTools)
  ✓ On success: window.location.href set to redirectUrl
  ✓ state stored in sessionStorage before redirect
  ✓ Error state shown if mutation fails

Phase 6 — AuthCallback
  ✓ Reads JWT from window.location.hash (not query params)
  ✓ Hash cleared from URL after reading (replaceState)
  ✓ Token stored in Zustand (not localStorage)
  ✓ me query called after token is stored
  ✓ On me query success: navigate to /dashboard
  ✓ On me query failure: clearAuth + navigate to /login?error=session_failed
  ✓ Double execution prevented (useRef guard for React Strict Mode)

Phase 7 — Dashboard + Settings
  ✓ Loading skeletons shown while queries are in flight
  ✓ me query renders user email and role
  ✓ workspace query renders workspace name, slug, status
  ✓ WorkspaceStatus badge color matches status value
  ✓ Sign out clears Apollo cache AND Zustand store

Silent refresh (page reload):
  ✓ useRequireAuth attempts POST /auth/refresh on page load if no token
  ✓ On success: new token stored, protected page renders
  ✓ On failure: redirect to /login
  ✓ No flash of protected content before redirect
```

---

## Summary

```
Phase 1  Project setup, Vite, shadcn/ui, codegen config
         → starts immediately, no backend needed

Phase 2  Zustand auth store
         → no backend needed

Phase 3  Apollo Client + auth link + error link
         → no backend needed, can test with mock data

Phase 4  AppShell + router + useRequireAuth
         → no backend needed

Phase 5  Login.tsx + AuthCallback.tsx
         → needs schema.graphqls (Member 4 Phase 1)
         → full end-to-end test needs Member 2 running

Phase 6  Dashboard.tsx + Settings.tsx
         → needs schema.graphqls + codegen (Member 4 Phase 1)
         → full data needs Member 4 resolvers running

Waits for:
  Member 4 Phase 1 → schema.graphqls (only hard blocker)
  Member 2 (coordinate) → callback redirect format (/#token=...)
  Member 2 (coordinate) → X-Public-Operation header contract
  Member 2 (coordinate) → refresh cookie path (/auth/refresh)
```
