---
type: code-study
flow: signup-and-login
created: 2026-05-05
---

# Code Study 01 — Auth Flow End-to-End

> Trace the complete path of a user signing up: from typing an email at `/signup` to landing on `/dashboard` as an ADMIN. Every file. Every function. Every link clickable.

---

## High-Level Flow

```
Browser                    Frontend (React)              Backend (Go Controller)
  │                               │                               │
  │── visits /signup ────────────▶│                               │
  │                         Step1Email                            │
  │── types email + type ────────▶│ (stored in Zustand)           │
  │── clicks Continue ───────────▶│                               │
  │                         Step2Workspace                        │
  │── types network name ────────▶│ (stored in Zustand)           │
  │── clicks Continue ───────────▶│                               │
  │                         Step3Auth                             │
  │── clicks "Sign in w/Google" ─▶│──initiateAuth mutation───────▶│
  │                               │◀─{ redirectUrl, state }───────│
  │◀──────── redirect to Google ──│                               │
  │                               │                               │
  │── Google login ──────────────▶│ (at Google's servers)         │
  │◀──── redirect to /auth/callback?code=...&state=... ──────────│
  │                               │ (Go callback handler runs)    │
  │                               │  ↳ verifies state             │
  │                               │  ↳ exchanges code for id_token │
  │                               │  ↳ Bootstrap creates workspace │
  │                               │  ↳ issues JWT                 │
  │◀──── redirect to /auth/callback#token=<JWT> ─────────────────│
  │                         AuthCallback                          │
  │── reads token from hash ─────▶│                               │
  │                               │──me query ───────────────────▶│
  │                               │◀─{ id, email, role } ─────────│
  │◀──── navigate to /dashboard ──│                               │
```

---

# Part A — Foundational Concepts

## What is React?
A JavaScript library for building UIs. Instead of "change this DOM element", you describe what the UI should look like for given data; React figures out the minimal DOM changes. This is **declarative** programming.

## What is ReactDOM?
The bridge between React's virtual tree and the real browser DOM. `ReactDOM.createRoot(...)` is the React 18 concurrent-mode API.

## What is `React.StrictMode`?
A development-only safety wrapper. It double-invokes render functions and effects to surface side-effect bugs. Has zero cost in production builds — Vite strips it.

## What is GraphQL?
One endpoint (`/graphql`); the client tells the server exactly what fields it wants. Replaces traditional REST APIs where every endpoint returns a fixed shape. Our controller exposes `POST /graphql`.

## What is Apollo Client?
The library that manages GraphQL on the frontend. It:
1. Sends queries/mutations
2. Caches results in `InMemoryCache`
3. Provides `useQuery` and `useMutation` hooks for React

## What is `ApolloProvider`?
Uses React's Context API to make the Apollo client available to every component in the tree. Without it, every component would import the client directly.

## What is React Router and `BrowserRouter`?
React apps are **Single Page Applications** — the browser loads `index.html` once, and JavaScript handles all navigation. React Router watches the URL and renders different components per path. `BrowserRouter` uses HTML5 History API (`pushState`) so URLs look real (`/connectors/123`, not `/#/connectors/123`).

## What is Tailwind CSS?
A utility-first CSS framework. Instead of writing `.card { padding: 16px; }`, you write `<div className="p-4">`. Each class does one thing. We use Tailwind v4 — imported in [admin/src/index.css](admin/src/index.css#L2) via `@import "tailwindcss"`.

## What is Zustand?
A state management library. Lets multiple components share data globally without passing props. Defined with `create((set) => ({ ... }))`. Components call `useSignupStore()` or `useAuthStore()` to read/write.

## What is gqlgen?
A Go library that turns a GraphQL schema file into Go code. Unlike most GraphQL libraries (which use reflection at runtime), gqlgen runs as a **code generator at build time** — it reads `.graphqls` schema files and writes a Go file with all the parsing, dispatching, and serialization code already inlined.

You write two things:
1. **A schema file** — what types and operations exist
2. **Resolver functions** — what to do when each operation is called

gqlgen writes everything in between: HTTP body parsing, GraphQL query parsing, argument unmarshaling, dispatch tables, response serialization, error handling.

Generate command: `cd controller && go generate ./graph/...`.

---

# Part B — Provider Tree and Routes

## Provider Tree (Mount Order)

[admin/src/main.tsx](admin/src/main.tsx)

```tsx
<React.StrictMode>        ← dev-only safety wrapper
  <ApolloProvider>        ← GraphQL context
    <BrowserRouter>       ← routing context
      <App />
    </BrowserRouter>
  </ApolloProvider>
</React.StrictMode>
```

Apollo wraps Router so the auth link's token-refresh logic doesn't depend on routing state. Router wraps App so `<Routes>` works.

## Routes Map

[admin/src/App.tsx](admin/src/App.tsx)

| Path | Component | Guard |
|------|-----------|-------|
| `/login` | [Login.tsx](admin/src/pages/Login.tsx) | public |
| `/auth/callback` | [AuthCallback.tsx](admin/src/pages/AuthCallback.tsx) | public |
| `/invite/:token` | [InviteAccept.tsx](admin/src/pages/InviteAccept.tsx) | public |
| `/signup` | [Step1Email.tsx](admin/src/pages/signup/Step1Email.tsx) | public |
| `/signup/workspace` | [Step2Workspace.tsx](admin/src/pages/signup/Step2Workspace.tsx) | public |
| `/signup/auth` | [Step3Auth.tsx](admin/src/pages/signup/Step3Auth.tsx) | public |
| `/client-install` | [ClientInstall.tsx](admin/src/pages/ClientInstall.tsx) | `ProtectedLayout` (any logged-in user) |
| `/dashboard` and admin pages | various | `AdminLayout` (must be `role === 'ADMIN'`) |

`AdminLayout` and `ProtectedLayout` both call [`useRequireAuth()`](admin/src/hooks/useRequireAuth.ts) which probes the backend `me` query before rendering.

---

# Part C — Signup Wizard (Frontend Only)

## Step 1 — `Step1Email.tsx` (`/signup`)

[admin/src/pages/signup/Step1Email.tsx](admin/src/pages/signup/Step1Email.tsx)

```tsx
const { email, accountType, setEmail, setAccountType } = useSignupStore()
const [localEmail, setLocalEmail] = useState(email)
```

**Two variables for email** — `localEmail` (local React state) updates on every keystroke for the input field. `setEmail` (from the Zustand store) is only called when the form submits. Standard pattern: local state drives the input, store state holds the committed value.

**Validation** — checks for `@` in email and that an account type is selected. The Continue button is disabled until both pass.

**On submit:**
```tsx
event.preventDefault()                  // stops the browser from reloading the page
setEmail(localEmail)                    // commits to Zustand
setAccountType(localAccountType)        // commits to Zustand
navigate('/signup/workspace')           // React Router changes URL — no server hit
```

`event.preventDefault()` is critical. Without it, submitting an HTML form does a full page reload, destroying React state. `navigate(...)` swaps the URL without contacting any server.

## Step 2 — `Step2Workspace.tsx` (`/signup/workspace`)

[admin/src/pages/signup/Step2Workspace.tsx](admin/src/pages/signup/Step2Workspace.tsx)

### Guard

```tsx
useEffect(() => {
  if (!email || !email.includes('@')) {
    navigate('/signup', { replace: true })
  }
}, [email, navigate])
```

If a user lands here directly without going through Step 1, `email` is empty in the store → redirect back. `replace: true` keeps it out of browser history (no Back-button loop).

### Auto-suggestion + Slug derivation

[admin/src/store/signup.ts](admin/src/store/signup.ts) implements `suggestWorkspaceName`:

- `alice@acme.com` → `"Acme"`
- `bob@my-corp.io` → `"My Corp"`
- `carol@gmail.com` → `""` (generic provider, skip)

Every `setWorkspaceName` call also derives a slug:

```tsx
setWorkspaceName: (workspaceName) =>
  set({ workspaceName, slug: slugify(workspaceName) }),
```

| Input | Slug |
|-------|------|
| `Acme Corp` | `acme-corp` |
| `My Network!` | `my-network` |
| (empty) | `workspace` |

The slug is shown in the live preview as `acme-corp.zecurity.in`.

## Step 3 — `Step3Auth.tsx` (`/signup/auth`)

[admin/src/pages/signup/Step3Auth.tsx](admin/src/pages/signup/Step3Auth.tsx) — first frontend↔backend interaction.

```tsx
const [initiateAuth, { loading, error }] = useMutation<
  InitiateAuthMutation,
  InitiateAuthMutationVariables
>(InitiateAuthDocument)
```

`useMutation` is an Apollo hook. `InitiateAuthDocument` is **auto-generated** from [admin/src/graphql/mutations.graphql](admin/src/graphql/mutations.graphql) into [admin/src/generated/graphql.ts](admin/src/generated/graphql.ts) by `npm run codegen`.

When the user clicks "Sign in with Google":

```tsx
const result = await initiateAuth({
  variables: { provider: 'google', workspaceName },
})
const { redirectUrl, state } = result.data!.initiateAuth
sessionStorage.setItem('oauth_state', state)
reset()
window.location.href = redirectUrl   // hard browser redirect to Google
```

`window.location.href = redirectUrl` is a **hard browser navigation** — React is gone after this; the browser is now on Google's domain.

---

# Part D — Backend: initiateAuth Mutation

## Stage 1 — Apollo Sends the Request

Apollo packages the operation:

```
POST /graphql
Content-Type: application/json
X-Public-Operation: InitiateAuth
{
  "query": "mutation InitiateAuth($provider: String!, $workspaceName: String) { ... }",
  "variables": { "provider": "google", "workspaceName": "Acme" }
}
```

The `X-Public-Operation: InitiateAuth` header is set by [admin/src/apollo/links/auth.ts](admin/src/apollo/links/auth.ts) — the frontend tells the server "this operation needs no JWT."

## Stage 2 — Server Routing → Public Bypass

[controller/cmd/server/main.go line 156](controller/cmd/server/main.go#L156) mounts the GraphQL endpoint:

```go
mux.Handle("/graphql", routeGraphQL(protected, gqlSrv))
```

Two handlers exist:
- `protected` (line 153) — wraps `gqlSrv` in `AuthMiddleware → WorkspaceGuard`
- `gqlSrv` (line 125) — the raw gqlgen handler, no auth wrapper

The `routeGraphQL` function ([main.go line 290](controller/cmd/server/main.go#L290)):

```go
func routeGraphQL(protected, public http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        op := r.Header.Get("X-Public-Operation")
        if _, ok := publicOperations[op]; ok {
            public.ServeHTTP(w, r)   // ← bypass auth
            return
        }
        protected.ServeHTTP(w, r)
    })
}
```

`publicOperations` ([main.go line 284](controller/cmd/server/main.go#L284)) is a hard-coded allowlist:

```go
var publicOperations = map[string]struct{}{
    "InitiateAuth":            {},
    "LookupWorkspace":         {},
    "LookupWorkspacesByEmail": {},
}
```

Since `InitiateAuth` is on the list, the request **skips the auth middleware entirely**.

## Stage 3 — gqlgen Dispatches to the Resolver

### Step A — The Schema File Defines the Contract

[controller/graph/schema.graphqls line 32](controller/graph/schema.graphqls#L32):

```graphql
type Mutation {
  initiateAuth(provider: String!, workspaceName: String): AuthInitPayload!
}

type AuthInitPayload {
  redirectUrl: String!
  state:       String!
}
```

This is the only place where the operation is declared. `provider: String!` means required. `workspaceName: String` (no `!`) means optional. `AuthInitPayload!` is the required return type.

When you run `go generate ./graph/...`, gqlgen produces [controller/graph/generated.go](controller/graph/generated.go) (~10,000 lines, all auto-generated).

### Step B — gqlgen Generates the `MutationResolver` Interface

[controller/graph/generated.go line 259](controller/graph/generated.go#L259):

```go
type MutationResolver interface {
    InitiateAuth(ctx context.Context, provider string, workspaceName *string) (*model.AuthInitPayload, error)
    ...
}
```

Signature exactly mirrors the GraphQL declaration:
- `provider: String!` → `provider string`
- `workspaceName: String` (nullable) → `workspaceName *string`
- `AuthInitPayload!` → `(*model.AuthInitPayload, error)`

### Step C — You Implement the Resolver

[controller/graph/resolvers/schema.resolvers.go line 22](controller/graph/resolvers/schema.resolvers.go#L22):

```go
func (r *mutationResolver) InitiateAuth(ctx context.Context, provider string, workspaceName *string) (*model.AuthInitPayload, error) {
    return r.AuthService.InitiateAuth(ctx, provider, workspaceName)
}
```

`mutationResolver` is `type mutationResolver struct{ *Resolver }` ([line 205](controller/graph/resolvers/schema.resolvers.go#L205)).

### Step D — The `Resolver` Struct Holds All Dependencies

[controller/graph/resolvers/resolver.go line 16](controller/graph/resolvers/resolver.go#L16):

```go
type Resolver struct {
    TenantDB          *db.TenantDB
    AuthService       auth.Service          // ← this one
    ConnectorCfg      connector.Config
    ConnectorRegistry *connector.ConnectorRegistry
    ShieldSvc         shield.Service
    ResourceCfg       resource.Config
    Redis             valkeycompat.Cmdable
    Pool              *pgxpool.Pool
    InvitationStore   *invitation.Store
    InvitationEmailer *invitation.Emailer
    PolicyStore       *policy.Store
    PolicyNotifier    *policy.Notifier
}
```

A **manual dependency injection container**. Resolvers reach into this struct; they never construct dependencies themselves.

`auth.Service` is an **interface** ([controller/internal/auth/service.go](controller/internal/auth/service.go)), not a concrete type. Concrete impl is `serviceImpl`. Using an interface lets us mock in tests.

### Step E — Server Startup Wires Everything Together

[controller/cmd/server/main.go line 70](controller/cmd/server/main.go#L70):

```go
authSvc, err := auth.NewService(auth.Config{
    Pool:               db.Pool,
    BootstrapService:   bootstrapSvc,
    JWTSecret:          mustEnv("JWT_SECRET"),
    ...
})
```

Then [main.go line 125](controller/cmd/server/main.go#L125):

```go
gqlSrv := handler.NewDefaultServer(
    graph.NewExecutableSchema(graph.Config{
        Resolvers: &resolvers.Resolver{
            TenantDB:          tenantDB,
            AuthService:       authSvc,        // ← injected here
            ...
        },
    }),
)
```

A single resolver instance shared across every request. No per-request allocation.

### Step F — gqlgen Receives the Request

When the POST arrives, gqlgen:
1. Reads + parses the JSON body
2. Parses the GraphQL query → AST
3. Validates against the schema
4. Walks the AST

In [generated.go line 9064](controller/graph/generated.go#L9064):

```go
switch field.Name {
case "initiateAuth":
    out.Values[i] = ec.OperationContext.RootResolverMiddleware(innerCtx, func(ctx context.Context) (res graphql.Marshaler) {
        return ec._Mutation_initiateAuth(ctx, field)
    })
```

### Step G — The Generated Field Resolver

[generated.go line 3910](controller/graph/generated.go#L3910):

```go
func (ec *executionContext) _Mutation_initiateAuth(ctx context.Context, field graphql.CollectedField) (ret graphql.Marshaler) {
    return graphql.ResolveField(
        ctx,
        ec.OperationContext,
        field,
        func(ctx context.Context, field graphql.CollectedField) (*graphql.FieldContext, error) {
            return ec.fieldContext_Mutation_initiateAuth(ctx, field)
        },
        func(ctx context.Context) (any, error) {
            fc := graphql.GetFieldContext(ctx)
            return ec.Resolvers.Mutation().InitiateAuth(ctx, fc.Args["provider"].(string), fc.Args["workspaceName"].(*string))
        },
        nil,
        func(ctx context.Context, selections ast.SelectionSet, v *model.AuthInitPayload) graphql.Marshaler {
            return ec.marshalNAuthInitPayload2...(ctx, selections, v)
        },
        true,
        true,
    )
}
```

The line that actually invokes your code:
```go
return ec.Resolvers.Mutation().InitiateAuth(ctx, fc.Args["provider"].(string), fc.Args["workspaceName"].(*string))
```

`ec.Resolvers.Mutation()` is implemented at [schema.resolvers.go line 194](controller/graph/resolvers/schema.resolvers.go#L194):
```go
func (r *Resolver) Mutation() graph.MutationResolver { return &mutationResolver{r} }
```

Argument unmarshaling lives at [generated.go line 2257](controller/graph/generated.go#L2257) — `unmarshalNString2string` for required `provider`, `unmarshalOString2ᚖstring` (the `ᚖ` is gqlgen's escape for `*`) for optional `workspaceName`.

### Step H — Your Resolver Runs

Finally:
```go
func (r *mutationResolver) InitiateAuth(ctx context.Context, provider string, workspaceName *string) (*model.AuthInitPayload, error) {
    return r.AuthService.InitiateAuth(ctx, provider, workspaceName)
}
```

`r.AuthService` is the interface; dispatch goes to the concrete `*serviceImpl` in [oidc.go line 31](controller/internal/auth/oidc.go#L31).

## Stage 4 — `auth.InitiateAuth` Does the Real Work

[controller/internal/auth/oidc.go line 31](controller/internal/auth/oidc.go#L31).

### 4.1 Provider check
```go
if provider != "google" {
    return nil, fmt.Errorf("unsupported provider: %s", provider)
}
```

### 4.2 Generate PKCE code_verifier ([oidc.go:41](controller/internal/auth/oidc.go#L41))
```go
verifierBytes := make([]byte, 64)
rand.Read(verifierBytes)
codeVerifier := base64.RawURLEncoding.EncodeToString(verifierBytes)
```
64 random bytes → base64url string. Never leaves the server.

### 4.3 Derive code_challenge ([oidc.go:49](controller/internal/auth/oidc.go#L49))
```go
hash := sha256.Sum256([]byte(codeVerifier))
codeChallenge := base64.RawURLEncoding.EncodeToString(hash[:])
```
Sent to Google. Google saves it. Later checks `SHA256(verifier) == challenge`.

### 4.4 Generate signed state ([oidc.go:57](controller/internal/auth/oidc.go#L57))
Calls `generateSignedState(s.cfg.JWTSecret)` at [oidc.go:94](controller/internal/auth/oidc.go#L94):
```go
nonce := make([]byte, 32)
rand.Read(nonce)
mac := hmac.New(sha256.New, []byte(secret))
mac.Write(nonce)
sig := mac.Sum(nil)
return base64url(nonce) + "." + base64url(sig), nil
```

Format: `<nonce>.<hmac_sig>`. HMAC ensures only this server could have produced it — CSRF protection.

### 4.5 Store PKCE state in Valkey ([oidc.go:66](controller/internal/auth/oidc.go#L66))

Calls [controller/internal/auth/valkey.go line 52](controller/internal/auth/valkey.go#L52):
```go
func (r *valkeyClient) SetPKCEState(ctx context.Context, state, codeVerifier string, workspaceName *string) error {
    key := pkceKey(state)   // "pkce:<state>"
    if workspaceName != nil && *workspaceName != "" {
        payload := map[string]string{
            "code_verifier": codeVerifier,
            "workspaceName": *workspaceName,
        }
        jsonBytes, _ := json.Marshal(payload)
        return r.rdb.Set(ctx, key, string(jsonBytes), 5*time.Minute).Err()
    }
    return r.rdb.Set(ctx, key, codeVerifier, 5*time.Minute).Err()
}
```

**Key insight:** `workspaceName` is parked here, NOT in PostgreSQL. TTL = 5 minutes. If the user abandons, the key auto-expires and no DB rows are ever created.

### 4.6 Build Google URL ([oidc.go:71](controller/internal/auth/oidc.go#L71))
```go
params := url.Values{}
params.Set("client_id", s.cfg.GoogleClientID)
params.Set("redirect_uri", s.cfg.RedirectURI)
params.Set("response_type", "code")
params.Set("scope", "openid email profile")
params.Set("code_challenge", codeChallenge)
params.Set("code_challenge_method", "S256")
params.Set("state", state)
redirectURL := "https://accounts.google.com/o/oauth2/v2/auth?" + params.Encode()
```

### 4.7 Return
```go
return &model.AuthInitPayload{
    RedirectURL: redirectURL,
    State:       state,
}, nil
```

## Stage 5 — gqlgen Marshals the Response

The marshal step at [generated.go line 3923](controller/graph/generated.go#L3923) serializes back to JSON:

```json
{
  "data": {
    "initiateAuth": {
      "redirectUrl": "https://accounts.google.com/o/oauth2/v2/auth?...",
      "state": "abc.def"
    }
  }
}
```

## Stage 6 — Apollo Resolves, Browser Navigates to Google

```tsx
window.location.href = redirectUrl
```

The browser fetches Google's OAuth page. The React app unmounts.

---

# Part E — Backend: /auth/callback Handler

## Stage 7 — Google Authenticates, Redirects Back

```
HTTP/1.1 302 Found
Location: https://yourdomain.com/auth/callback?code=4/0AY0e-g7...&state=abc.def
```

The browser GETs `/auth/callback?...` on the **Go server**.

## Stage 8 — main.go Routes /auth/callback

[main.go line 145](controller/cmd/server/main.go#L145):
```go
mux.Handle("/auth/callback", authSvc.CallbackHandler())
```

`CallbackHandler()` is a method that returns a `http.Handler` closure with `s` bound (so it can access config, Redis, bootstrap).

## Stage 9 — CallbackHandler Walks Through 10+ Steps

[callback.go line 34](controller/internal/auth/callback.go#L34).

### 9.1 Read code + state ([callback.go:45](controller/internal/auth/callback.go#L45))
```go
code := r.URL.Query().Get("code")
state := r.URL.Query().Get("state")
if code == "" || state == "" {
    fail("missing_params")
    return
}
```

`fail` ([callback.go:40](controller/internal/auth/callback.go#L40)) redirects to `/login?error=<reason>` — never exposes internal errors.

### 9.2 Verify state HMAC ([callback.go:57](controller/internal/auth/callback.go#L57))

Calls back into [oidc.go line 113](controller/internal/auth/oidc.go#L113):

```go
parts := strings.SplitN(state, ".", 2)
nonce := base64url.Decode(parts[0])
gotSig := base64url.Decode(parts[1])
expectedSig := hmac.New(sha256.New, []byte(secret)).Write(nonce).Sum(nil)
if !hmac.Equal(gotSig, expectedSig) { return error }
```

`hmac.Equal` is **constant-time** — prevents timing attacks.

### 9.3 Atomic GETDEL from Redis ([callback.go:66](controller/internal/auth/callback.go#L66))

[valkey.go line 70](controller/internal/auth/valkey.go#L70):
```go
val, err := r.rdb.GetDel(ctx, pkceKey(state)).Result()
```

`GETDEL` is atomic — single-use. Replay attempts fail because the key is deleted.

If found, recovers `(codeVerifier, workspaceName)`.

### 9.4 Server-to-Server Token Exchange ([callback.go:79](controller/internal/auth/callback.go#L79))

Calls [controller/internal/auth/exchange.go line 91](controller/internal/auth/exchange.go#L91):

```go
body := url.Values{}
body.Set("code", code)
body.Set("code_verifier", codeVerifier)
body.Set("client_id", s.cfg.GoogleClientID)
body.Set("client_secret", s.cfg.GoogleClientSecret)
body.Set("redirect_uri", s.cfg.RedirectURI)
body.Set("grant_type", "authorization_code")
// POST to https://oauth2.googleapis.com/token
```

Google verifies:
- `client_id + client_secret` match
- `SHA256(code_verifier) == code_challenge` originally sent
- The `code` was issued for our app and not yet redeemed

Returns `{ id_token, access_token, expires_in, token_type }`. We use only `id_token`.

### 9.5 Verify the id_token ([callback.go:88](controller/internal/auth/callback.go#L88))

[controller/internal/auth/idtoken.go line 55](controller/internal/auth/idtoken.go#L55) runs **six checks**:

1. **Signature** — verified against Google's RSA public keys, fetched from `https://www.googleapis.com/oauth2/v3/certs` and cached for 1h ([idtoken.go:130](controller/internal/auth/idtoken.go#L130) `getGooglePublicKey`)
2. **Algorithm** ([idtoken.go:82](controller/internal/auth/idtoken.go#L82)) — must be RS256. Blocks `alg=none` and `alg=HS256` confusion attacks
3. **Audience (`aud`)** ([idtoken.go:100](controller/internal/auth/idtoken.go#L100)) — must equal our `GOOGLE_CLIENT_ID`
4. **Issuer (`iss`)** ([idtoken.go:107](controller/internal/auth/idtoken.go#L107)) — `accounts.google.com` or `https://accounts.google.com`
5. **Expiry (`exp`)** ([idtoken.go:92](controller/internal/auth/idtoken.go#L92)) — must be in the future
6. **`email_verified == true`** ([idtoken.go:114](controller/internal/auth/idtoken.go#L114)) — Google verified the email
7. **`sub` non-empty** ([idtoken.go:120](controller/internal/auth/idtoken.go#L120)) — immutable identity anchor

### 9.6 Extract identity ([callback.go:97](controller/internal/auth/callback.go#L97))
```go
email := googleClaims.Email
providerSub := googleClaims.Sub      // immutable identity key
name := googleClaims.Name

bootstrapName := name
if workspaceName != "" {
    bootstrapName = workspaceName    // signup flow: use what user typed
}
```

### 9.7 Bootstrap — Where the Workspace Is Actually Created

[controller/internal/bootstrap/bootstrap.go line 31](controller/internal/bootstrap/bootstrap.go#L31) — three branches:

**Branch A — returning user** ([bootstrap.go:39](controller/internal/bootstrap/bootstrap.go#L39)):
```sql
SELECT id, tenant_id, role FROM users
WHERE provider_sub = $1 AND provider = $2 LIMIT 1
```
Found → update `last_login_at`, return existing. No new workspace.

**Branch B — invited user** ([bootstrap.go:76](controller/internal/bootstrap/bootstrap.go#L76)):
```sql
SELECT workspace_id, role FROM workspace_members
WHERE email = $1 AND status = 'invited' AND user_id IS NULL
```
Found → `runInvitedUserTransaction` ([bootstrap.go:205](controller/internal/bootstrap/bootstrap.go#L205)).

**Branch C — first-time signup** → `runBootstrapTransaction` ([bootstrap.go:96](controller/internal/bootstrap/bootstrap.go#L96)):

```go
tx, err := s.Pool.Begin(ctx)
defer tx.Rollback(ctx)   // ← key Go pattern
```

`defer tx.Rollback(ctx)` runs on every return path. After successful `tx.Commit()`, `Rollback` is a no-op.

Six DB operations inside the transaction:

```go
// 1. Workspace as 'provisioning'
INSERT INTO workspaces (slug, name, status, trust_domain)
VALUES ($1, $2, 'provisioning', $3) RETURNING id

// 2. Admin user
INSERT INTO users (tenant_id, email, provider, provider_sub, role, status)
VALUES ($1, $2, $3, $4, 'admin', 'active') RETURNING id

// 3. Generate workspace CA (PKI bootstrap)
caResult := s.PKIService.GenerateWorkspaceCA(ctx, tenantID)

// 4. Store CA key material
INSERT INTO workspace_ca_keys (...)

// 5. Flip workspace to active
UPDATE workspaces SET status = 'active', ca_cert_pem = $1 WHERE id = $2

// 6. Insert into workspace_members
INSERT INTO workspace_members (workspace_id, user_id, email, role, status, joined_at)
VALUES (..., 'admin', 'active', NOW())

tx.Commit(ctx)
```

The PKI step ([bootstrap.go:142](controller/internal/bootstrap/bootstrap.go#L142)) calls into [controller/internal/pki/workspace.go](controller/internal/pki/workspace.go) — generates ECDSA P-384 keypair, signs a CA cert with the controller's intermediate. This CA is the trust anchor for every Connector/Shield/Client cert in this workspace.

If anything fails → automatic `tx.Rollback`. No partial state.

Returns `&Result{TenantID, UserID, Role: "admin"}`.

### 9.8 Issue access JWT ([callback.go:121](controller/internal/auth/callback.go#L121))

[controller/internal/auth/session.go line 67](controller/internal/auth/session.go#L67):
```go
claims := jwtClaims{
    TenantID: tenantID,
    Role:     role,
    Email:    email,
    RegisteredClaims: jwt.RegisteredClaims{
        Subject:   userID,
        Issuer:    s.cfg.JWTIssuer,
        IssuedAt:  jwt.NewNumericDate(now),
        ExpiresAt: jwt.NewNumericDate(now.Add(15min)),
    },
}
token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
signed, _ := token.SignedString([]byte(s.cfg.JWTSecret))
```

HS256 = HMAC-SHA256. Symmetric — same secret signs and verifies.

### 9.9 Issue refresh token ([callback.go:129](controller/internal/auth/callback.go#L129))

[session.go line 99](controller/internal/auth/session.go#L99):
```go
raw := make([]byte, 32)         // 256 bits of entropy
rand.Read(raw)
token := base64.RawURLEncoding.EncodeToString(raw)
s.redisClient.SetRefreshToken(ctx, userID, token, 7*24*time.Hour)
```

Opaque random bytes — NOT a JWT. JWTs can't be revoked; opaque tokens checked against Redis can. Enables "log out everywhere."

### 9.10 Set httpOnly refresh cookie ([callback.go:141](controller/internal/auth/callback.go#L141))

```go
http.SetCookie(w, &http.Cookie{
    Name:     "refresh_token",
    Value:    refreshToken,
    Path:     "/auth/refresh",          // browser only sends to this endpoint
    HttpOnly: true,                     // JS can't read (XSS protection)
    SameSite: http.SameSiteStrictMode,  // CSRF protection
    Secure:   true,                     // HTTPS only
    MaxAge:   int(ttl.Seconds()),
})
```

### 9.11 Final Redirect with JWT in URL fragment ([callback.go:156](controller/internal/auth/callback.go#L156))

```go
http.Redirect(w, r, s.cfg.AllowedOrigin+"/auth/callback#token="+accessToken, http.StatusFound)
```

Critical: **fragment (`#token=...`) is never sent to any server**. Not logged anywhere.

---

# Part F — Frontend: AuthCallback + me Query

## Stage 10 — React /auth/callback Mounts

React Router matches `/auth/callback` to [AuthCallback.tsx](admin/src/pages/AuthCallback.tsx). Its `useEffect` runs.

## Stage 11 — Read Hash, Scrub URL, Store Token

[AuthCallback.tsx line 38](admin/src/pages/AuthCallback.tsx#L38):

```tsx
const hash = window.location.hash   // "#token=eyJhbGci..."
const token = hash.slice('#token='.length)
window.history.replaceState(null, '', window.location.pathname)
setAccessToken(token)
```

`replaceState` rewrites the URL bar without triggering navigation. Token is gone from address bar and browser history.

[auth.ts line 25](admin/src/store/auth.ts#L25):
```tsx
setAccessToken: (token) => {
  sessionStorage.setItem(SESSION_KEY, token)  // survives F5
  set({ accessToken: token })                 // in-memory
}
```

`sessionStorage` not `localStorage` — narrower XSS exposure window.

The `useRef` guard ([AuthCallback.tsx:30](admin/src/pages/AuthCallback.tsx#L30)) prevents double-execution under StrictMode:
```tsx
const handled = useRef(false)
useEffect(() => {
  if (handled.current) return
  handled.current = true
  // ...
})
```

## Stage 12 — Fire the `me` Query

```tsx
const result = await apolloClient.query<MeQuery>({
  query: MeDocument,
  fetchPolicy: 'network-only',
})
```

Why `apolloClient.query` not `useQuery`? Imperative — we need to `await` before deciding where to navigate.

`fetchPolicy: 'network-only'` skips the cache.

### The me query operation

[admin/src/graphql/queries.graphql](admin/src/graphql/queries.graphql):
```graphql
query Me {
  me {
    id
    email
    role
    provider
    createdAt
  }
}
```

Auto-generated into `MeDocument` constant + `MeQuery` TypeScript type by `npm run codegen`.

## Stage 13 — Apollo Link Chain

[admin/src/apollo/client.ts](admin/src/apollo/client.ts):
```ts
link: ApolloLink.from([errorLink, authLink, httpLink])
```

### authLink attaches the token

[admin/src/apollo/links/auth.ts line 18](admin/src/apollo/links/auth.ts#L18):
```ts
const token = useAuthStore.getState().accessToken
const isPublicOperation = PUBLIC_OPERATIONS.includes(opName)

operation.setContext(({ headers = {} }) => ({
  headers: {
    ...headers,
    ...(token ? { Authorization: `Bearer ${token}` } : {}),
    ...(isPublicOperation ? { 'X-Public-Operation': opName } : {}),
  },
}))
```

For `Me`:
- ✅ `Authorization: Bearer <JWT>` is set
- ❌ No `X-Public-Operation`

### httpLink terminates

```
POST /graphql
Authorization: Bearer eyJhbGci...
{ "operationName": "Me", "query": "query Me { me { id ... } }", "variables": {} }
```

## Stage 14 — Server Routes to Protected Chain

[main.go line 290](controller/cmd/server/main.go#L290) routeGraphQL → no `X-Public-Operation` → `protected` chain:

```
AuthMiddleware → WorkspaceGuard → gqlSrv
```

## Stage 15 — AuthMiddleware Verifies JWT and Injects Tenant Context

[middleware/auth.go line 27](controller/internal/middleware/auth.go#L27):

### 15.1 Extract Bearer
```go
raw := r.Header.Get("Authorization")
parts := strings.SplitN(raw, " ", 2)
// parts[0] = "Bearer", parts[1] = "<JWT>"
```

### 15.2 Verify JWT
```go
claims := &Claims{}
token, err := jwt.ParseWithClaims(parts[1], claims,
    func(t *jwt.Token) (interface{}, error) {
        if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
            return nil, fmt.Errorf("unexpected alg")
        }
        return []byte(secret), nil
    },
    jwt.WithIssuer(appmeta.ControllerIssuer),
    jwt.WithExpirationRequired(),
)
```

Four checks bundled:
| Check | Defends Against |
|-------|-----------------|
| HMAC algorithm enforced | `alg=none` attack |
| Signature matches HS256 + secret | Forged tokens |
| Issuer matches `"ztna-controller"` | Tokens from other internal services |
| Expiration required + automatic exp check | Replay of old tokens |

### 15.3 Sanity check
```go
if claims.Subject == "" || claims.TenantID == "" || claims.Role == "" {
    writeJSON401(w, "token missing required claims")
    return
}
```

### 15.4 Inject TenantContext
```go
ctx := tenant.Set(r.Context(), tenant.TenantContext{
    TenantID: claims.TenantID,
    UserID:   claims.Subject,
    Role:     claims.Role,
    Email:    claims.Email,
})
next.ServeHTTP(w, r.WithContext(ctx))
```

[tenant/context.go line 25](controller/internal/tenant/context.go#L25) — uses unexported `contextKey` type to prevent key collisions.

## Stage 16 — WorkspaceGuard

[middleware/workspace.go line 21](controller/internal/middleware/workspace.go#L21):

```go
tc, ok := tenant.Get(r.Context())
var status string
pool.QueryRow(r.Context(),
    "SELECT status FROM workspaces WHERE id = $1",
    tc.TenantID,
).Scan(&status)
if status != "active" {
    writeJSON403(w, ...)
    return
}
```

Defense in depth — even if JWT is valid, the workspace might have been suspended/deleted since issuance.

## Stage 17 — gqlgen Dispatches to Me Resolver

[generated.go line 9281](controller/graph/generated.go#L9281):
```go
case "me":
    field := field
    innerFunc := func(ctx context.Context, fs *graphql.FieldSet) (res graphql.Marshaler) {
        defer func() {
            if r := recover(); r != nil {
                ec.Error(ctx, ec.Recover(ctx, r))
            }
        }()
        res = ec._Query_me(ctx, field)
        ...
    }
```

The `defer func() { recover() }` catches panics from `tenant.MustGet` and turns them into GraphQL errors instead of crashing.

[generated.go line 5010](controller/graph/generated.go#L5010):
```go
func (ec *executionContext) _Query_me(ctx context.Context, field graphql.CollectedField) (ret graphql.Marshaler) {
    return graphql.ResolveField(
        ctx, ec.OperationContext, field,
        ...
        func(ctx context.Context) (any, error) {
            return ec.Resolvers.Query().Me(ctx)
        },
        nil,
        func(ctx context.Context, selections ast.SelectionSet, v *models.User) graphql.Marshaler {
            return ec.marshalNUser2...(ctx, selections, v)
        },
        true, true,
    )
}
```

`ec.Resolvers.Query()` from [schema.resolvers.go line 197](controller/graph/resolvers/schema.resolvers.go#L197) → `&queryResolver{r}`.

## Stage 18 — Me Resolver Reads from DB

[schema.resolvers.go line 27](controller/graph/resolvers/schema.resolvers.go#L27):

```go
func (r *queryResolver) Me(ctx context.Context) (*models.User, error) {
    tc := tenant.MustGet(ctx)

    var u models.User
    err := r.TenantDB.QueryRow(ctx,
        `SELECT id, tenant_id, email, provider, provider_sub,
                role, status, last_login_at, created_at, updated_at
         FROM users
         WHERE id        = $1
           AND tenant_id = $2
           AND status    = 'active'`,
        tc.UserID, tc.TenantID,
    ).Scan(
        &u.ID, &u.TenantID, &u.Email,
        &u.Provider, &u.ProviderSub,
        &u.Role, &u.Status, &u.LastLoginAt,
        &u.CreatedAt, &u.UpdatedAt,
    )
    if err != nil { return nil, fmt.Errorf("me: %w", err) }
    return &u, nil
}
```

Three key patterns:

### Pattern 1 — `tenant.MustGet(ctx)`

[tenant/context.go line 45](controller/internal/tenant/context.go#L45):
```go
func MustGet(ctx context.Context) TenantContext {
    tc, ok := Get(ctx)
    if !ok {
        panic("tenant.MustGet: TenantContext not in context. " +
              "AuthMiddleware was bypassed. This is a code bug.")
    }
    return tc
}
```

Resolvers panic on missing tenant — that's a programming bug (middleware ordering). gqlgen's `recover()` catches it.

### Pattern 2 — Double-keyed query

```sql
WHERE id = $1 AND tenant_id = $2
```

Even though `users.id` is globally unique, also filter by `tenant_id`. If a JWT had a mismatched (user, tenant) pair, query returns zero rows — no cross-tenant leak. **Defense in depth**.

### Pattern 3 — pgx Scan, no ORM

`QueryRow().Scan(&u.ID, ...)` — positional, must match SELECT column order. Fast, explicit, zero runtime surprises.

## Stage 19 — Field-Level Resolvers Transform Output

[schema.resolvers.go line 164](controller/graph/resolvers/schema.resolvers.go#L164):
```go
func (r *userResolver) Role(ctx context.Context, obj *models.User) (graph.Role, error) {
    role := graph.Role(strings.ToUpper(obj.Role))
    if !role.IsValid() {
        return "", fmt.Errorf("invalid role: %q", obj.Role)
    }
    return role, nil
}
```

DB has lowercase `"admin"`. GraphQL exposes uppercase `ADMIN`. Field resolver does the conversion.

Similarly [`CreatedAt`](controller/graph/resolvers/schema.resolvers.go#L174) formats `time.Time` as ISO8601.

Final response:
```json
{
  "data": {
    "me": {
      "id": "01234567-89ab-cdef-...",
      "email": "alice@acme.com",
      "role": "ADMIN",
      "provider": "google",
      "createdAt": "2026-05-05T10:23:00Z"
    }
  }
}
```

## Stage 20 — Apollo Returns to React

```tsx
const result = await apolloClient.query<MeQuery>({...})
setUser(result.data!.me)
```

Apollo also caches the result. The cache is keyed by `__typename + id`. Future queries for the same user use the cache.

## Stage 21 — Role-Based Redirect

[AuthCallback.tsx line 83](admin/src/pages/AuthCallback.tsx#L83):

```tsx
if (result.data!.me.role === 'ADMIN') {
  navigate('/dashboard', { replace: true })
} else {
  navigate('/client-install', { replace: true })
}
```

`replace: true` swaps history entry — Back button won't loop.

## Stage 22 — App.tsx AdminLayout Wraps the Dashboard

[App.tsx line 43](admin/src/App.tsx#L43):
```tsx
function AdminLayout() {
  const { isReady } = useRequireAuth()
  const { user } = useAuthStore()
  if (!isReady) return null
  if (user && user.role !== 'ADMIN') return <Navigate to="/client-install" replace />
  return <AppShell />
}
```

[`useRequireAuth`](admin/src/hooks/useRequireAuth.ts) probes `me` AGAIN. Defense in depth — never trust client state alone:

```tsx
if (accessToken) {
  probeMe()
} else {
  trySilentRefresh()
}
```

`trySilentRefresh` does `POST /auth/refresh` with the httpOnly cookie. If valid → new access token → user never sees a login screen.

## Stage 23 — AppShell Renders the Layout

[AppShell.tsx](admin/src/components/layout/AppShell.tsx):
```tsx
export function AppShell() {
  return (
    <div className="admin-shell">
      <Sidebar />
      <Header />
      <main className="app-panel ...">
        <Outlet />
      </main>
    </div>
  )
}
```

`<Outlet />` renders whichever child route matched — for `/dashboard` it's `<Dashboard />`.

---

# Part G — Files Touched

## Frontend
- [admin/index.html](admin/index.html)
- [admin/src/main.tsx](admin/src/main.tsx)
- [admin/src/index.css](admin/src/index.css)
- [admin/src/App.tsx](admin/src/App.tsx)
- [admin/src/pages/signup/Step1Email.tsx](admin/src/pages/signup/Step1Email.tsx)
- [admin/src/pages/signup/Step2Workspace.tsx](admin/src/pages/signup/Step2Workspace.tsx)
- [admin/src/pages/signup/Step3Auth.tsx](admin/src/pages/signup/Step3Auth.tsx)
- [admin/src/pages/AuthCallback.tsx](admin/src/pages/AuthCallback.tsx)
- [admin/src/store/signup.ts](admin/src/store/signup.ts)
- [admin/src/store/auth.ts](admin/src/store/auth.ts)
- [admin/src/hooks/useRequireAuth.ts](admin/src/hooks/useRequireAuth.ts)
- [admin/src/apollo/client.ts](admin/src/apollo/client.ts)
- [admin/src/apollo/links/auth.ts](admin/src/apollo/links/auth.ts)
- [admin/src/apollo/links/error.ts](admin/src/apollo/links/error.ts)
- [admin/src/graphql/queries.graphql](admin/src/graphql/queries.graphql)
- [admin/src/graphql/mutations.graphql](admin/src/graphql/mutations.graphql)
- [admin/src/generated/graphql.ts](admin/src/generated/graphql.ts)
- [admin/src/components/layout/AppShell.tsx](admin/src/components/layout/AppShell.tsx)

## Backend
- [controller/cmd/server/main.go](controller/cmd/server/main.go)
- [controller/graph/schema.graphqls](controller/graph/schema.graphqls)
- [controller/graph/generated.go](controller/graph/generated.go)
- [controller/graph/resolvers/resolver.go](controller/graph/resolvers/resolver.go)
- [controller/graph/resolvers/schema.resolvers.go](controller/graph/resolvers/schema.resolvers.go)
- [controller/internal/auth/service.go](controller/internal/auth/service.go)
- [controller/internal/auth/oidc.go](controller/internal/auth/oidc.go)
- [controller/internal/auth/exchange.go](controller/internal/auth/exchange.go)
- [controller/internal/auth/idtoken.go](controller/internal/auth/idtoken.go)
- [controller/internal/auth/callback.go](controller/internal/auth/callback.go)
- [controller/internal/auth/refresh.go](controller/internal/auth/refresh.go)
- [controller/internal/auth/session.go](controller/internal/auth/session.go)
- [controller/internal/auth/valkey.go](controller/internal/auth/valkey.go)
- [controller/internal/auth/config.go](controller/internal/auth/config.go)
- [controller/internal/bootstrap/bootstrap.go](controller/internal/bootstrap/bootstrap.go)
- [controller/internal/middleware/auth.go](controller/internal/middleware/auth.go)
- [controller/internal/middleware/workspace.go](controller/internal/middleware/workspace.go)
- [controller/internal/middleware/session.go](controller/internal/middleware/session.go)
- [controller/internal/tenant/context.go](controller/internal/tenant/context.go)
- [controller/internal/pki/workspace.go](controller/internal/pki/workspace.go)
- [controller/internal/models/user.go](controller/internal/models/user.go)
- [controller/internal/models/workspace.go](controller/internal/models/workspace.go)

## Database
- `workspaces` table — created during Bootstrap
- `workspace_ca_keys` table — workspace-scoped PKI material
- `users` table
- `workspace_members` table

---

# Part H — Quick-Reference Call Chains

## InitiateAuth call stack
```
Browser POST /graphql (X-Public-Operation: InitiateAuth)
  → main.go: routeGraphQL — bypass auth
  → gqlgen handler
  → schema.resolvers.go: mutationResolver.InitiateAuth (forwarder)
  → oidc.go: serviceImpl.InitiateAuth
      → rand.Read — code_verifier
      → sha256.Sum256 — code_challenge
      → oidc.go: generateSignedState — HMAC nonce
      → valkey.go: SetPKCEState — Redis SET TTL=5min
      → build Google URL
  → return JSON to browser
```

## /auth/callback call stack
```
Google → GET /auth/callback?code=...&state=...
  → main.go: mux.Handle("/auth/callback", authSvc.CallbackHandler())
  → callback.go: CallbackHandler
      → oidc.go: verifySignedState — HMAC check
      → valkey.go: GetAndDeletePKCEState — Redis GETDEL
      → exchange.go: exchangeCodeForTokens — POST to Google
      → idtoken.go: VerifyGoogleIDToken — 6 checks
          → idtoken.go: getGooglePublicKey → fetchGoogleJWKS
      → bootstrap.go: Bootstrap → runBootstrapTransaction
          → INSERT workspaces (provisioning)
          → INSERT users (admin)
          → pki/workspace.go: GenerateWorkspaceCA
          → INSERT workspace_ca_keys
          → UPDATE workspaces SET status='active'
          → INSERT workspace_members
      → session.go: issueAccessToken — sign HS256 JWT
      → session.go: issueRefreshToken
          → valkey.go: SetRefreshToken — Redis SET TTL=7d
      → http.SetCookie — httpOnly Secure SameSite=Strict
      → http.Redirect → /auth/callback#token=<JWT>
```

## me query call stack
```
Browser POST /graphql + Authorization: Bearer <JWT>
  → main.go: routeGraphQL — protected path
  → middleware/auth.go: AuthMiddleware
      → jwt.ParseWithClaims — verify HS256 + issuer + exp
      → tenant.Set — inject TenantContext
  → middleware/workspace.go: WorkspaceGuard
      → SELECT workspaces.status — must be 'active'
  → gqlgen handler
  → generated.go: _Query_me
  → schema.resolvers.go: queryResolver.Me
      → tenant.MustGet — read TenantContext
      → SELECT users WHERE id=? AND tenant_id=?
  → field-level resolvers: Role uppercases, CreatedAt formats
  → return JSON to browser
```

## Full signup → dashboard flow
```
[Step3Auth click]
 1. Apollo POST /graphql (X-Public-Operation: InitiateAuth)
 2-5. InitiateAuth + JSON response
 6. window.location.href = redirectUrl → leave React
 7. Google authenticates → 302 /auth/callback?code=...&state=...
 8-9. Go CallbackHandler 11 sub-steps → Bootstrap → JWT
 10-11. /auth/callback#token=... → AuthCallback.tsx → setAccessToken
 12-19. apolloClient.query(me) → middleware → resolver → DB → response
 20-21. setUser, navigate('/dashboard')
 22. App.tsx AdminLayout mounts → useRequireAuth probes me again
 23. AppShell renders → Dashboard component
```

---

# Part I — Key Invariants

| Invariant | Where enforced |
|-----------|---------------|
| Workspace not created until Google verifies user identity | `Bootstrap` runs from callback, not `InitiateAuth` |
| PKCE prevents auth-code interception | `code_verifier` never leaves controller; Google checks `SHA256(verifier) == challenge` |
| State CSRF protection | HMAC-signed in `generateSignedState`, verified in `verifySignedState` |
| JWT never appears in URL path or query string | Backend uses URL hash (`#token=...`); frontend `replaceState` clears it |
| Refresh token unreadable by JS | httpOnly cookie; only the browser sends it |
| Every cross-tenant query scoped by `tenant_id` | `Me`, `Users`, `Workspace` resolvers all filter by `tc.TenantID` |
| Frontend cannot claim to be a different user/tenant | `tenant.MustGet(ctx)` reads from JWT claims, not request body |
| Access token short-lived (~15min), refresh long-lived (7d) | `session.go` + `refresh.go` |
| State is single-use | Redis `GETDEL` atomic |
| Public operations limited to allowlist | `publicOperations` map in `main.go` |
| Tenant context key cannot collide | Unexported `type contextKey string` in `tenant/context.go` |
| Resolvers panic-loud on missing tenant | `tenant.MustGet` panics; gqlgen `recover()` catches |

---

# Part J — Persistent-Session Path (After Browser Refresh)

When the user reloads the dashboard tab:
- `accessToken` survives in `sessionStorage`
- In-memory `user` is gone

[`useRequireAuth`](admin/src/hooks/useRequireAuth.ts) re-hydrates:

```tsx
if (accessToken) {
  probeMe()
} else {
  trySilentRefresh()
}
```

If the access token expired but the refresh cookie is still valid:
- `POST /auth/refresh` with the httpOnly cookie ([refresh.go](controller/internal/auth/refresh.go))
- Returns new access token
- User never sees a login screen

If both fail → `clearAuth()` → redirect to `/login`.

---

## Next Flows to Study

- **Connector creation + enrollment** — admin clicks "New Connector" → install command → connector starts → controller signs cert
- **Resource creation + ACL push** — admin defines a resource → policy compiles → ACL snapshot pushed to connector via heartbeat piggyback
- **Device enrollment + RDE tunnel (Sprint 9)** — `zecurity setup` → device cert → `zecurity up` → TUN device → traffic flows through Connector → Shield/direct
