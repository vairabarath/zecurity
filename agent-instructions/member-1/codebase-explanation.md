# Member 1 — Full Codebase Explanation

## Architecture Overview

```
Browser
  │
  ├─ Vite dev server (port 5173)
  │    ├─ /graphql      → proxy → localhost:8080 (Go backend)
  │    └─ /auth/refresh → proxy → localhost:8080 (Go backend)
  │
  └─ React App
       │
       ├─ Apollo Client  ──→ GraphQL queries/mutations to backend
       ├─ Zustand store  ──→ JWT + user in memory only (no localStorage)
       └─ React Router   ──→ 5 routes (2 public, 3 protected)
```

---

## File Tree (29 source files)

```
admin/
  index.html                          ← Vite HTML entry
  vite.config.ts                      ← Dev server, proxy, Tailwind, @ alias
  tsconfig.app.json                   ← TypeScript config
  codegen.yml                         ← GraphQL codegen config
  components.json                     ← shadcn/ui config
  package.json                        ← Dependencies
  src/
    main.tsx                          ← React root mount
    App.tsx                           ← Route definitions + auth guard
    index.css                         ← shadcn/ui CSS variables + Tailwind
    store/
      auth.ts                         ← Zustand auth store (JWT, user, refresh state)
    apollo/
      client.ts                       ← Apollo Client instance + link chain
      links/
        auth.ts                       ← Attaches Bearer token + X-Public-Operation header
        error.ts                      ← Catches 401 → refreshes token → retries request
    graphql/
      mutations.graphql               ← initiateAuth mutation
      queries.graphql                 ← me + workspace queries
    generated/                        ← DO NOT EDIT — codegen output
      graphql.ts                      ← TypeScript types + document nodes
      gql.ts                          ← graphql() helper
      fragment-masking.ts             ← Fragment masking utilities
      index.ts                        ← Barrel re-export
    hooks/
      useRequireAuth.ts               ← Auth guard hook (silent refresh + redirect)
    pages/
      Login.tsx                       ← Google OAuth entry point
      AuthCallback.tsx                ← Reads JWT from URL hash, boots session
      Dashboard.tsx                   ← User card + Workspace card
      Settings.tsx                    ← Workspace info display
    components/
      layout/
        AppShell.tsx                  ← Sidebar + Header + Outlet wrapper
        Sidebar.tsx                   ← Dashboard/Settings nav links
        Header.tsx                    ← Avatar + role badge + sign out dropdown
      ui/                             ← shadcn/ui components (button, card, badge, etc.)
    lib/
      utils.ts                        ← cn() class merging utility
```

---

## Layer 1: Entry Point (`main.tsx`)

```tsx
<ApolloProvider client={apolloClient}>   // Apollo wraps everything
  <BrowserRouter>                        // Router inside Apollo (queries work in routes)
    <App />                              // Route definitions
  </BrowserRouter>
</ApolloProvider>
```

---

## Layer 2: Routing (`App.tsx`)

Two route groups:

**Public** (no auth needed):
| Route | Component | Purpose |
|-------|-----------|---------|
| `/login` | `Login.tsx` | Google OAuth entry point |
| `/auth/callback` | `AuthCallback.tsx` | Receives JWT hash after OAuth redirect |

**Protected** (require auth via `useRequireAuth`):
| Route | Component | Purpose |
|-------|-----------|---------|
| `/` | → redirect to `/dashboard` | Root convenience redirect |
| `/dashboard` | `Dashboard.tsx` | User info + workspace status |
| `/settings` | `Settings.tsx` | Workspace details |

`ProtectedLayout` renders `null` until auth state is confirmed — prevents flash of protected content.

---

## Layer 3: Auth Store (`store/auth.ts`)

Zustand store, **memory only** — JWT never touches `localStorage` (XSS protection):

```typescript
interface AuthState {
  accessToken: string | null    // The JWT
  user: MeQuery['me'] | null    // { id, email, role, provider, createdAt }
  isRefreshing: boolean         // Prevents concurrent refresh calls
  setAccessToken, setUser, setRefreshing, clearAuth  // Actions
}
```

Key design: page reload = memory cleared = silent refresh via `POST /auth/refresh` using httpOnly cookie.

---

## Layer 4: Apollo Client + Link Chain (`apollo/`)

Three links chained in order: `errorLink → authLink → httpLink`

### `auth.ts` — Attaches auth headers to every request

```
Every GraphQL request:
  ├─ If token exists → Authorization: Bearer <JWT>
  ├─ If mutation is InitiateAuth → X-Public-Operation: initiateAuth
  │   (tells backend to skip auth middleware for this public mutation)
  └─ Otherwise → no Authorization header (will get 401, triggers refresh)
```

### `error.ts` — Handles 401 errors with token refresh

```
On UNAUTHORIZED GraphQL error:
  ├─ Call POST /auth/refresh (browser auto-sends httpOnly cookie)
  ├─ Success → store new JWT → retry original request
  └─ Failure → clearAuth → redirect to /login

Concurrent refresh deduplication:
  If isRefreshing === true → subscribe and wait → use result from first call
```

### `client.ts` — Wires it together

```typescript
link: from([errorLink, authLink, httpLink])
     ↑ order matters — error wrapper first
cache: InMemoryCache
fetchPolicy: 'cache-and-network'  // Always fetch fresh data
```

---

## Layer 5: GraphQL Operations (`graphql/*.graphql`)

Two files, three operations — codegen turns these into TypeScript types:

```graphql
# mutations.graphql
mutation InitiateAuth($provider: String!) {
  initiateAuth(provider: $provider) {
    redirectUrl   # Google OAuth URL
    state         # CSRF state
  }
}

# queries.graphql
query Me { me { id email role provider createdAt } }
query GetWorkspace { workspace { id slug name status createdAt } }
```

---

## Layer 6: Pages

### `Login.tsx` — Single button, no form

```
User clicks "Sign in with Google"
  → initiateAuth mutation
    → authLink adds X-Public-Operation header (bypasses backend auth)
    → Backend returns { redirectUrl, state }
  → Store state in sessionStorage
  → window.location.href = redirectUrl  (full browser redirect to Google)
```

### `AuthCallback.tsx` — JWT from hash fragment, NOT query params

```
Google redirects to: /#token=<JWT>
  → Read JWT from window.location.hash
  → Clear hash with replaceState (never visible in history)
  → Store JWT in Zustand (memory only)
  → Call me query to load user data
  → Navigate to /dashboard
  → On failure: clearAuth → /login?error=session_failed

useRef guard prevents double-execution in React StrictMode
```

### `Dashboard.tsx` — Two data cards

```
useQuery(MeDocument)        → User email + role badge
useQuery(GetWorkspaceDocument) → Workspace name + slug + status badge

Status badge colors:
  ACTIVE → green (default)
  PROVISIONING → gray (secondary)
  SUSPENDED → red (destructive)
  DELETED → outline
```

### `Settings.tsx` — Read-only workspace info

```
useQuery(GetWorkspaceDocument)
  → Name, Slug (monospace), ID (monospace), Created date
```

---

## Layer 7: Layout (`components/layout/`)

### `AppShell.tsx` — Protected page wrapper

```
<Sidebar />
  <Header />
  <main>
    <Outlet />   ← Current route's page component renders here
  </main>
```

### `Sidebar.tsx` — Navigation

```
NavLink active state:
  Active → bg-primary text-primary-foreground
  Inactive → text-muted-foreground hover:bg-accent
```

### `Header.tsx` — Top bar

```
Right side:
  [Role badge] [Avatar with initials dropdown → user email + Sign out]

Sign out flow:
  1. apolloClient.clearStore()   — no stale GraphQL cache
  2. clearAuth()                  — JWT + user removed from memory
  3. navigate('/login')           — redirect
```

---

## Layer 8: Auth Guard (`hooks/useRequireAuth.ts`)

Called by `ProtectedLayout` before rendering any protected page:

```
If accessToken exists → isReady = true (render immediately)
If no accessToken:
  → POST /auth/refresh (silent refresh using httpOnly cookie)
  → Success → store JWT → isReady = true
  → Failure → navigate('/login')
```

Handles the **page reload case**: user reloads page → Zustand memory cleared → cookie still valid → silent refresh restores session.

---

## Critical Design Decisions

| Decision | Why |
|----------|-----|
| JWT in memory only | localStorage is accessible to any JS on page (XSS theft) |
| URL hash for JWT delivery | Hash never sent to server (no server log exposure) |
| `X-Public-Operation` header | Tells backend to skip auth middleware for `initiateAuth` |
| `isRefreshing` flag | Two concurrent refresh calls = cookie conflict = one fails |
| `fetchPolicy: cache-and-network` | Admin console needs fresh data, not stale cache |
| Client-side sign out only | Refresh cookie is httpOnly (can't delete from JS), expires naturally in 7 days |
| `ProtectedLayout` renders `null` | Prevents flash of protected content before auth check |

---

## Deviations from Plan

All deviations are documented in `agent-instructions/member-1/changes/phase-deviations.md`. The main changes were:

1. **Apollo Client v4 sub-package imports** — `useQuery`/`useMutation`/`ApolloProvider` moved to `@apollo/client/react`, core types to `@apollo/client/core`, `ErrorLink` to `@apollo/client/link/error`
2. **Codegen hooks removed** — `useInitiateAuthMutation`, `useMeQuery`, `useGetWorkspaceQuery` no longer generated; replaced with `useMutation(InitiateAuthDocument)` and `useQuery(MeDocument)`
3. **Vite proxy narrowed** — `/auth` → `/auth/refresh` (prevents intercepting `/auth/callback` client-side route)
4. **TypeScript flags adjusted** — `erasableSyntaxOnly` + `verbatimModuleSyntax` removed (incompatible with codegen output)
