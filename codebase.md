# ZECURITY Codebase Walkthrough

## Purpose

This repository implements a small multi-tenant ZTNA-style admin platform with:

- A Go controller in `controller/`
- A React admin console in `admin/`
- PostgreSQL for workspace and user data
- Redis for PKCE state and refresh tokens
- A PKI hierarchy for per-workspace certificate authorities

The current product scope is:

- Google OAuth login with PKCE
- First-login workspace bootstrap
- JWT-based API auth
- Refresh token cookie flow
- Tenant-scoped GraphQL queries for `me` and `workspace`

---

## End-to-End Runtime Flow

### New user signup flow

1. React signup wizard collects email, account type, and workspace name.
2. React calls GraphQL `initiateAuth(provider: "google", workspaceName)`.
3. Go auth service generates PKCE verifier/challenge and signed `state`.
4. Go stores `state -> code_verifier + optional workspaceName` in Redis.
5. Browser is redirected to Google OAuth.
6. Google redirects back to `GET /auth/callback?code=...&state=...`.
7. Go verifies signed state, loads and deletes PKCE verifier from Redis, exchanges the code for tokens, verifies the Google `id_token`, and extracts user identity.
8. Go bootstrap service either finds an existing user membership or creates:
   - `workspaces` row
   - `users` row
   - workspace CA via PKI service
   - `workspace_ca_keys` row
9. Go issues an access JWT and a refresh token.
10. Go sets refresh token as an `httpOnly` cookie on `/auth/refresh`.
11. Go redirects browser to frontend `/auth/callback#token=<jwt>`.
12. React reads the hash token, stores it in Zustand memory, fetches `me`, and navigates to `/dashboard`.

### Returning user flow

1. React login page calls `initiateAuth(provider: "google")` without workspace name.
2. Callback flow is the same.
3. Bootstrap service finds existing user by `(provider_sub, provider)`, updates `last_login_at`, and returns the existing tenant membership.

### Protected API flow

1. React Apollo link attaches `Authorization: Bearer <jwt>`.
2. Go `AuthMiddleware` verifies JWT and injects `TenantContext`.
3. Go `WorkspaceGuard` confirms the workspace status is `active`.
4. GraphQL resolvers query tenant-scoped data using `TenantDB`.

### Refresh flow

1. If GraphQL gets `UNAUTHORIZED`, Apollo error link calls `POST /auth/refresh`.
2. Browser sends the `refresh_token` cookie.
3. Go refresh handler parses the access token to get `user_id`, loads the stored refresh token from Redis, compares them in constant time, issues a new access JWT, and returns it in JSON.
4. Apollo retries the failed operation with the new JWT.

---

## Repository Map

### Top level

- `Makefile`: contains `gqlgen` target to regenerate Go GraphQL code.
- `fullplan.md`: overall project architecture and team split.
- `member1-plan.md`: original frontend implementation plan.
- `member1-updated-plan.md`: updated frontend flow with signup wizard.
- `member2-plan.md`: auth/session backend plan.
- `member3-plan.md`: PKI/bootstrap backend plan.
- `member4-plan.md`: GraphQL/DB/middleware backend plan.
- `proto/.gitkeep`: reserved placeholder for future protobuf/gRPC work.
- `.gitignore`, `.codex`, `.qwen/settings.json`, `.qwen/settings.json.orig`: tooling/editor metadata, not runtime logic.

### Planning instruction docs

These are implementation guidance documents, not executable code:

- `agent-instructions/member-1/codebase-explanation.md`
- `agent-instructions/member-1/changes/phase-deviations.md`
- `agent-instructions/member-1/phase-1-project-setup.md`
- `agent-instructions/member-1/phase-2-auth-store.md`
- `agent-instructions/member-1/phase-3-apollo-client.md`
- `agent-instructions/member-1/phase-4-app-shell-router.md`
- `agent-instructions/member-1/phase-5-auth-pages.md`
- `agent-instructions/member-1/phase-6-layout-nav.md`
- `agent-instructions/member-1/phase-7-dashboard-settings.md`
- `agent-instructions/member-2/phase-1-config-redis.md`
- `agent-instructions/member-2/phase-2-pkce-initiate-auth.md`
- `agent-instructions/member-2/phase-3-idtoken-verification.md`
- `agent-instructions/member-2/phase-4-token-exchange.md`
- `agent-instructions/member-2/phase-5-jwt-session.md`
- `agent-instructions/member-2/phase-6-callback-handler.md`
- `agent-instructions/member-2/phase-7-refresh-handler.md`
- `agent-instructions/member-3/phase-1-crypto-helpers.md`
- `agent-instructions/member-3/phase-2-service-interface.md`
- `agent-instructions/member-3/phase-3-root-ca.md`
- `agent-instructions/member-3/phase-4-intermediate-ca.md`
- `agent-instructions/member-3/phase-5-workspace-ca.md`
- `agent-instructions/member-3/phase-6-bootstrap.md`
- `agent-instructions/member-3/phase-7-wiring.md`
- `agent-instructions/member-4/phase-1-ship-first.md`
- `agent-instructions/member-4/phase-2-core-types-db.md`
- `agent-instructions/member-4/phase-3-middleware.md`
- `agent-instructions/member-4/phase-4-gqlgen-setup.md`
- `agent-instructions/member-4/phase-5-resolvers.md`
- `agent-instructions/member-4/phase-6-main-env.md`

They explain how the codebase was intended to be split and implemented across four contributors.

---

## Backend: Go Controller

## Entry point and wiring

### `controller/cmd/server/main.go`

This is the application bootstrap file.

Responsibilities:

- Loads `.env` from either repo root or `controller/.env`.
- Initializes PostgreSQL via `db.Init`.
- Initializes PKI via `pki.Init`.
- Builds `bootstrap.Service`.
- Creates tenant-aware DB wrapper `db.NewTenantDB`.
- Builds auth service via `auth.NewService`.
- Creates gqlgen server with shared resolver dependencies.
- Registers public HTTP routes:
  - `/auth/callback`
  - `/auth/refresh`
  - `/health`
  - `/playground` in development only
- Wraps `/graphql` with auth and workspace middleware, except for the `initiateAuth` operation.

Important logic:

- `routeGraphQL` checks `X-Public-Operation == "initiateAuth"` and bypasses auth middleware only for that mutation.
- `healthHandler` pings Postgres and returns `{"status":"ok"}` on success.
- `mustEnv` crashes startup if required environment variables are missing.

### `controller/internal/appmeta/identity.go`

Stores application-wide constant strings:

- `ProductName = "ZECURITY"`
- JWT issuer name
- PKI naming constants for Root CA, Intermediate CA, and workspace organization labels

These constants are reused across auth, middleware, and PKI code to avoid drift.

## Database layer

### `controller/internal/db/pool.go`

Creates and owns the global `pgxpool.Pool`.

Logic:

- Reads `DATABASE_URL`
- Parses config
- Sets pool sizing
- Pings DB on startup
- Stores the pool in package global `db.Pool`
- Exposes `Close()`

### `controller/internal/db/tenant.go`

Defines `TenantDB`, a thin wrapper around `pgxpool.Pool`.

Key design decision:

- It does not inject `tenant_id` automatically into SQL.
- It only enforces that a `TenantContext` exists.
- Every query still includes explicit `tenant_id` conditions in SQL.

This makes tenant boundaries visible and auditable in code.

Methods:

- `QueryRow`
- `Query`
- `Exec`
- `BeginTx`
- `RawPool`

`RawPool` is intentionally available for infrastructure code such as health checks, middleware workspace status checks, and PKI startup.

### `controller/internal/tenant/context.go`

Defines request-scoped tenant identity:

- `TenantID`
- `UserID`
- `Role`

Functions:

- `Set`: stores tenant context in `context.Context`
- `Get`: safe lookup
- `MustGet`: panics if missing, treating absence as a programming bug

## Middleware

### `controller/internal/middleware/auth.go`

Authenticates Bearer JWTs.

Flow:

1. Reads `Authorization` header.
2. Requires `Bearer <token>` format.
3. Parses JWT with HMAC secret.
4. Enforces HMAC signing method, issuer, and expiry.
5. Requires `sub`, `tenant_id`, and `role`.
6. Injects `TenantContext` into request context.

Failure mode:

- Returns GraphQL-shaped JSON 401 with extension code `UNAUTHORIZED`.

### `controller/internal/middleware/workspace.go`

Guards all protected requests by workspace status.

Flow:

1. Reads `TenantContext`.
2. Queries `workspaces.status` by tenant ID using raw pool.
3. Requires status to be `active`.

Failure mode:

- Returns GraphQL-shaped JSON 403 with extension code `FORBIDDEN`.

### `controller/internal/middleware/session.go`

Currently empty placeholder file. No logic exists there yet.

## GraphQL schema and resolvers

### `controller/graph/schema.graphqls`

Defines the GraphQL contract:

- Query `me`
- Query `workspace`
- Mutation `initiateAuth(provider, workspaceName?)`
- Types `User`, `Workspace`, `AuthInitPayload`
- Enums `Role`, `WorkspaceStatus`

### `controller/graph/gqlgen.yml`

gqlgen config:

- schema source is `graph/schema.graphqls`
- generated executor goes to `graph/generated.go` (gitignored)
- generated models go to `graph/models_gen.go`
- resolvers use follow-schema layout in `graph/resolvers`
- GraphQL types are mapped onto internal Go models

### `controller/graph/model/model.go`

Defines `AuthInitPayload` manually to avoid an import cycle between graph and auth packages.

### `controller/graph/models_gen.go`

Generated enum/model support code for:

- `Role`
- `WorkspaceStatus`

This file provides enum validation and GraphQL/JSON marshal-unmarshal helpers.

### `controller/graph/resolver.go`

Empty gqlgen package anchor file.

### `controller/graph/resolvers/resolver.go`

Defines shared resolver dependencies:

- `TenantDB`
- `AuthService`

### `controller/graph/resolvers/schema.resolvers.go`

Contains all current resolver logic.

Resolvers:

- `InitiateAuth`: delegates directly to `AuthService.InitiateAuth`.
- `Me`: reads current user by `id` and `tenant_id`, requiring `status = 'active'`.
- `Workspace`: reads current workspace by tenant ID.
- `User.Role`: converts DB lowercase role strings to GraphQL enum values.
- `User.CreatedAt`: formats time as RFC3339-like string.
- `Workspace.Status`: converts DB lowercase status strings to GraphQL enum values.
- `Workspace.CreatedAt`: formats time string.

Important detail:

- Resolver SQL remains explicit about tenant scope instead of hiding it.

### `controller/graph/resolvers/auth.resolvers.go`

Empty placeholder file. No extra auth resolver logic beyond `schema.resolvers.go`.

### `controller/graph/resolvers/workspace.resolvers.go`

Empty placeholder file. Workspace resolver logic currently lives in `schema.resolvers.go`.

## Internal data models

### `controller/internal/models/user.go`

Represents `users` table fields for DB scanning.

### `controller/internal/models/workspace.go`

Represents `workspaces` table fields for DB scanning.

## Auth package

### `controller/internal/auth/service.go`

Defines the auth service interface used by the rest of the controller:

- `InitiateAuth`
- `CallbackHandler`
- `RefreshHandler`

### `controller/internal/auth/config.go`

Defines auth configuration and `NewService`.

Logic:

- Validates required config fields
- Applies default JWT issuer and token TTLs
- Connects to Redis via `newRedisClient`
- Returns concrete `serviceImpl`

### `controller/internal/auth/redis.go`

Redis wrapper for two responsibilities:

- PKCE state storage
- Refresh token storage

PKCE logic:

- `SetPKCEState` stores `code_verifier` keyed by signed state.
- It stores either a raw string or JSON payload containing `code_verifier` and `workspaceName`.
- TTL is 5 minutes.

Single-use retrieval:

- `GetAndDeletePKCEState` uses a Redis pipeline for atomic GET+DEL behavior.
- Returns `(codeVerifier, workspaceName, found, error)`.

Refresh token logic:

- `SetRefreshToken`
- `GetRefreshToken`
- `DeleteRefreshToken`

Key namespaces:

- `pkce:<state>`
- `refresh:<userID>`

### `controller/internal/auth/oidc.go`

Implements `InitiateAuth`.

Logic:

1. Only accepts provider `"google"`.
2. Generates a random PKCE `code_verifier`.
3. Computes SHA-256 `code_challenge`.
4. Generates an HMAC-signed state using `JWT_SECRET`.
5. Stores verifier and optional workspace name in Redis.
6. Builds Google OAuth authorization URL.
7. Returns `redirectUrl` and `state`.

Supporting helpers:

- `generateSignedState(secret)`
- `verifySignedState(state, secret)`

State format:

- `base64url(nonce) + "." + base64url(hmac_sha256(nonce))`

### `controller/internal/auth/exchange.go`

Handles server-to-server code exchange with Google token endpoint.

Logic:

- POSTs `code`, `code_verifier`, client ID, client secret, redirect URI, and grant type
- Parses JSON response
- Requires `id_token` to be present

### `controller/internal/auth/idtoken.go`

Verifies Google `id_token`.

Security checks performed:

- Extracts `kid` from token header
- Loads Google JWKS and caches keys for one hour
- Requires RSA signing method
- Verifies signature
- Verifies audience equals configured Google client ID
- Verifies issuer is a Google issuer
- Requires unexpired token
- Requires `email_verified = true`
- Requires non-empty `sub`

Key helpers:

- `getGooglePublicKey`
- `fetchGoogleJWKS`
- `containsString`

### `controller/internal/auth/session.go`

Owns access JWTs and refresh tokens.

Access token logic:

- `issueAccessToken(userID, tenantID, role)`
- Signs HMAC JWT with claims:
  - `tenant_id`
  - `role`
  - `sub = user_id`
  - `iss`
  - `iat`
  - `exp`

Refresh token logic:

- `issueRefreshToken(ctx, userID)`
- Generates 32 random bytes
- Base64url-encodes them
- Stores token in Redis with configured TTL

Also includes `verifyAccessToken`, used by tests rather than production request flow.

### `controller/internal/auth/callback.go`

Implements `GET /auth/callback`.

Complete flow:

1. Read `code` and `state`.
2. Verify signed state.
3. Load and delete PKCE verifier from Redis.
4. Exchange code for Google tokens.
5. Verify Google `id_token`.
6. Extract `email`, `sub`, and `name`.
7. Call bootstrap service.
8. Issue access JWT.
9. Issue refresh token and set `refresh_token` cookie.
10. Redirect frontend to `/auth/callback#token=<jwt>`.

Cookie settings:

- `HttpOnly: true`
- `SameSite: Strict`
- `Secure: true`
- `Path: /auth/refresh`

Error handling:

- Any failure redirects to frontend `/login?error=<reason>`
- Internal details are intentionally not leaked

Testing hooks:

- `exchangeCodeForTokensHook`
- `verifyGoogleIDTokenHook`

These make integration tests easier by allowing overrides.

### `controller/internal/auth/refresh.go`

Implements `POST /auth/refresh`.

Flow:

1. Read `refresh_token` cookie.
2. Read `Authorization` header.
3. Parse access token without claims validation to extract identity.
4. Load stored refresh token from Redis by `user_id`.
5. Compare cookie and stored token using constant-time comparison.
6. Issue new access token.
7. Return JSON `{ "access_token": "..." }`.

Helpers:

- `writeJSONError`
- `extractBearer`

Important design note:

- Refresh token is not rotated on every refresh.
- A future sign-out endpoint is implied but not implemented.

## Bootstrap package

### `controller/internal/bootstrap/bootstrap.go`

Creates new workspaces or reuses existing memberships.

Flow:

1. Looks up user by `(provider_sub, provider)`.
2. If found:
   - updates `last_login_at`
   - returns existing `tenant_id`, `user_id`, `role`
3. If not found:
   - begins DB transaction
   - slugifies workspace name
   - inserts workspace with `status = 'provisioning'`
   - inserts admin user
   - calls PKI service to generate workspace CA
   - inserts encrypted CA key material into `workspace_ca_keys`
   - updates workspace to `status = 'active'` and stores CA cert PEM
   - commits transaction

Supporting helpers:

- `slugify`
- `isNoRows`

Design implication:

- Bootstrap is the bridge between auth identity and tenant provisioning.

## PKI package

### `controller/internal/pki/service.go`

Defines public PKI interface:

- `GenerateWorkspaceCA(ctx, tenantID)`

`Init`:

- requires `PKI_MASTER_SECRET`
- ensures root CA exists
- ensures intermediate CA exists
- loads intermediate signing material into memory

### `controller/internal/pki/crypto.go`

Shared cryptographic helpers.

Core logic:

- `generateECKeyPair`: creates EC P-384 key
- `encryptPrivateKey`: marshals EC private key to DER, derives per-context AES key using HKDF-SHA256, encrypts with AES-256-GCM
- `decryptPrivateKey`: reverse operation
- `encodeCertToPEM`
- `parseCertFromPEM`
- `newSerialNumber`
- `zeroBytes`
- `certValidity`

Important PKI design details:

- Different HKDF contexts are used for root, intermediate, and workspace keys.
- Certificates are backdated by one hour to reduce clock-skew issues.

### `controller/internal/pki/root.go`

Ensures one root CA row exists.

Logic:

- Checks `ca_root` row count.
- If absent:
  - generates P-384 key
  - self-signs root CA cert
  - encrypts private key using context `"root-ca"`
  - inserts into `ca_root`
- `loadRootCA` decrypts and returns the root CA for intermediate signing

### `controller/internal/pki/intermediate.go`

Ensures one intermediate CA exists and is loaded into memory.

Logic:

- Checks `ca_intermediate` row count.
- If absent:
  - loads root CA
  - generates intermediate key
  - signs intermediate certificate with root CA
  - encrypts key using context `"intermediate-ca"`
  - stores in `ca_intermediate`
- Always loads decrypted intermediate cert+key into service memory

### `controller/internal/pki/workspace.go`

Generates per-workspace CA material.

Logic:

- Requires in-memory intermediate key to exist
- Generates P-384 key
- Creates CA cert signed by intermediate
- Encodes tenant identity in URI `tenant:<tenantID>`
- Encrypts workspace private key using HKDF context equal to the tenant ID
- Returns `WorkspaceCAResult` for DB persistence by bootstrap transaction

## SQL schema

### `controller/migrations/001_schema.sql`

Creates all current DB tables:

- `ca_root`
- `ca_intermediate`
- `workspaces`
- `users`
- `workspace_ca_keys`

Important schema decisions:

- `workspaces.status` supports `provisioning`, `active`, `suspended`, `deleted`
- `users` are unique by `(tenant_id, provider_sub)`
- same Google account can exist in multiple workspaces
- CA private keys are stored encrypted at rest
- indexes support provider lookup, tenant queries, and active workspace checks

## Backend config and environment files

### `controller/go.mod` and `controller/go.sum`

Go module metadata and dependency lock state.

Main dependencies:

- gqlgen
- golang-jwt
- pgx/v5
- godotenv
- redis/go-redis

### `controller/docker-compose.yml`

Local development infrastructure:

- Postgres 16 Alpine
- Redis 7 Alpine
- health checks
- migration volume mount

### `controller/.env.example`

Documents required env vars:

- `DATABASE_URL`
- `REDIS_URL`
- `PORT`
- `ENV`
- `JWT_SECRET`
- Google OAuth config
- `PKI_MASTER_SECRET`
- `ALLOWED_ORIGIN`

### `controller/.env`

Local developer environment file. Runtime secret/config file, not application logic.

## Backend tests

The repository includes backend integration tests:

- `controller/internal/auth/integration_test.go`
- `controller/internal/bootstrap/bootstrap_integration_test.go`
- `controller/internal/pki/root_integration_test.go`
- `controller/internal/pki/intermediate_integration_test.go`
- `controller/internal/pki/workspace_integration_test.go`

Based on the production code shape, these are intended to validate auth flow wiring, bootstrap transaction correctness, and PKI creation behavior.

---

## Frontend: React Admin

## Frontend entry and app shell

### `admin/src/main.tsx`

Bootstraps the React app:

- wraps the app in `ApolloProvider`
- wraps routes in `BrowserRouter`
- imports global CSS

### `admin/src/App.tsx`

Defines routes.

Public routes:

- `/login`
- `/auth/callback`
- `/signup`
- `/signup/workspace`
- `/signup/auth`

Protected routes:

- `/`
- `/dashboard`
- `/settings`

`ProtectedLayout` uses `useRequireAuth` and only renders the app shell when auth state is ready.

## State management

### `admin/src/store/auth.ts`

Zustand store for session state.

Fields:

- `accessToken`
- `user`
- `isRefreshing`

Actions:

- `setAccessToken`
- `setUser`
- `setRefreshing`
- `clearAuth`

Important security choice:

- Access token is memory-only, not stored in localStorage or sessionStorage.

### `admin/src/store/signup.ts`

Zustand store for signup wizard state.

Fields:

- `email`
- `accountType`
- `workspaceName`
- `slug`

Actions:

- `setEmail`
- `setAccountType`
- `setWorkspaceName`
- `reset`

Helpers:

- `slugify`: mirrors backend slug generation
- `suggestWorkspaceName`: infers a human-friendly workspace name from email domain unless the domain is a generic provider

## Apollo client and GraphQL transport

### `admin/src/apollo/client.ts`

Creates Apollo Client with:

- `errorLink`
- `authLink`
- `HttpLink`

Transport config:

- GraphQL endpoint is `/graphql`
- credentials are `same-origin`
- watch queries use `cache-and-network`

### `admin/src/apollo/links/auth.ts`

Adds request headers.

Logic:

- If a JWT exists, adds `Authorization: Bearer <token>`.
- If operation name is `InitiateAuth`, sets `X-Public-Operation: initiateAuth`.

That header is what lets Go bypass auth middleware for the public auth-initiation mutation.

### `admin/src/apollo/links/error.ts`

Handles auth failures.

Logic:

- Detects GraphQL errors with extension code `UNAUTHORIZED`
- Calls `refreshAccessToken`
- Avoids duplicate concurrent refreshes with `isRefreshing`
- On successful refresh:
  - stores new JWT
  - retries the failed operation
- On failure:
  - clears auth
  - redirects to `/login`

Implementation detail:

- Uses RxJS `Observable` because Apollo Client v4 error handling expects retry logic in observable form.

## Auth protection hook

### `admin/src/hooks/useRequireAuth.ts`

Used by protected routes.

Flow:

1. If access token already exists, mark page ready.
2. If not, attempt silent refresh with `POST /auth/refresh`.
3. If refresh succeeds, store returned JWT and mark ready.
4. If refresh fails, navigate to `/login`.

## Pages

### `admin/src/pages/Login.tsx`

Simple Google sign-in page.

Flow:

- Executes `InitiateAuth` mutation with `provider: "google"`
- Stores returned `state` in `sessionStorage`
- Redirects browser to returned `redirectUrl`

Also links to signup flow for users without a workspace.

### `admin/src/pages/AuthCallback.tsx`

Handles browser return after backend OAuth callback redirect.

Flow:

1. Reads `#token=<jwt>` from URL hash.
2. If missing, forwards any `?error=` to `/login`.
3. Clears hash from browser URL.
4. Saves token in auth store.
5. Runs `Me` query using Apollo client.
6. Stores `me` response in auth store.
7. Navigates to `/dashboard`.

Uses `useRef` guard to avoid double execution in React Strict Mode.

### `admin/src/pages/Dashboard.tsx`

Loads:

- `Me`
- `GetWorkspace`

UI:

- user card
- workspace card
- skeleton loading placeholders
- workspace status badge with variant mapping

### `admin/src/pages/Settings.tsx`

Loads workspace data and displays:

- name
- slug as network ID
- workspace ID
- created date

### `admin/src/pages/signup/Step1Email.tsx`

Signup wizard step 1.

Behavior:

- collects email
- collects account type (`home` or `office`)
- validates minimally by checking `@`
- stores values in signup store
- moves to `/signup/workspace`

### `admin/src/pages/signup/Step2Workspace.tsx`

Signup wizard step 2.

Behavior:

- guards against missing signup email
- auto-suggests workspace name from email domain
- keeps live `slug` preview in sync through signup store
- submits to `/signup/auth`

### `admin/src/pages/signup/Step3Auth.tsx`

Signup wizard step 3.

Behavior:

- guards against missing email or workspace name
- displays summary card
- calls `InitiateAuth` with `workspaceName`
- stores `state` in sessionStorage
- resets signup store
- redirects to Google

## Layout components

### `admin/src/components/layout/AppShell.tsx`

Protected application frame:

- sidebar
- header
- outlet area for child routes

### `admin/src/components/layout/Sidebar.tsx`

Static navigation with:

- Dashboard
- Settings

Uses `NavLink` and `cn` helper for active-state styling.

### `admin/src/components/layout/Header.tsx`

Top bar showing:

- user role badge
- avatar dropdown
- sign-out action

Sign-out logic:

- clears Apollo cache
- clears auth store
- navigates to `/login`

Current limitation:

- It does not call a backend sign-out endpoint, so the refresh cookie remains valid until expiry.

## GraphQL documents and generated client code

### `admin/src/graphql/mutations.graphql`

Defines `InitiateAuth` mutation.

### `admin/src/graphql/queries.graphql`

Defines:

- `Me`
- `GetWorkspace`

### `admin/src/generated/graphql.ts`

Generated TypeScript GraphQL schema and operation types.

Includes:

- scalar typings
- schema type definitions
- operation result and variable types
- typed document constants for `InitiateAuth`, `Me`, and `GetWorkspace`

### `admin/src/generated/gql.ts`

Generated helper mapping GraphQL source strings to typed document nodes.

Note:

- The generated file warns about bundle-size inefficiency of the generic document map and recommends babel or swc plugin optimization for production builds.

### `admin/src/generated/fragment-masking.ts`

Generated fragment-masking utilities from GraphQL codegen preset.

No app-specific fragments are currently defined, but the helper is generated by default.

### `admin/src/generated/index.ts`

Re-exports generated helpers.

## UI primitives and styling

These are mostly standard reusable UI wrappers around Radix or simple Tailwind components. They do not contain business logic, but they define the frontend visual building blocks.

### `admin/src/components/ui/button.tsx`

Reusable button with variant and size support using `class-variance-authority`.

### `admin/src/components/ui/input.tsx`

Reusable input field wrapper.

### `admin/src/components/ui/card.tsx`

Reusable card layout primitives.

### `admin/src/components/ui/badge.tsx`

Reusable badge with style variants.

### `admin/src/components/ui/avatar.tsx`

Radix avatar wrapper.

### `admin/src/components/ui/dropdown-menu.tsx`

Radix dropdown wrapper with content, item, sub-menu, label, separator, checkbox, and radio item helpers.

### `admin/src/components/ui/label.tsx`

Radix label wrapper.

### `admin/src/components/ui/separator.tsx`

Radix separator wrapper.

### `admin/src/components/ui/skeleton.tsx`

Simple animated loading placeholder.

### `admin/src/components/ui/alert.tsx`

Alert component with default and destructive variants.

### `admin/src/components/ui/toast.tsx`

Radix toast component primitives and variants.

### `admin/src/components/ui/toaster.tsx`

Renders all queued toast notifications.

### `admin/src/components/ui/use-toast.ts`

Local toast state manager.

Logic:

- in-memory toast reducer
- add, update, dismiss, remove actions
- limit of 5 concurrent toasts
- delayed removal queue

Important note:

- The `REMOVE_TOAST` reducer branch appears logically inverted:
  - when `action.toastId` is truthy it returns `[]`
  - when falsy it filters by ID
- That looks like a bug in the current implementation.

### `admin/src/lib/utils.ts`

Defines `cn(...)` helper using `clsx` and `tailwind-merge`.

### `admin/src/index.css`

Global Tailwind v4 import and CSS variable theme tokens for light and dark palettes.

## Frontend config and metadata

### `admin/package.json`

Frontend package manifest.

Key libraries:

- React 19
- Apollo Client 4
- React Router 7
- Zustand
- Tailwind CSS 4
- Radix UI primitives
- GraphQL Codegen

Scripts:

- `dev`
- `build`
- `lint`
- `preview`
- `codegen`
- `codegen:watch`

### `admin/package-lock.json`

NPM lockfile. Dependency resolution snapshot, not handwritten logic.

### `admin/vite.config.ts`

Vite config:

- React plugin
- Tailwind plugin
- alias `@ -> ./src`
- dev proxy for:
  - `/graphql`
  - `/auth/refresh`

Not proxied:

- `/auth/callback`

### `admin/codegen.yml`

GraphQL codegen config.

Logic:

- schema source is backend GraphQL schema
- documents come from `src/graphql/**/*.graphql`
- generated client output goes to `src/generated/`

### `admin/eslint.config.js`

ESLint flat config using:

- JS recommended rules
- TypeScript recommended rules
- React hooks rules
- Vite react-refresh rules

### `admin/tsconfig.json`

Top-level TypeScript project references file.

### `admin/tsconfig.app.json`

TypeScript config for frontend app build.

### `admin/tsconfig.node.json`

TypeScript config for Vite/node-side files.

### `admin/components.json`

shadcn/ui configuration metadata.

### `admin/index.html`

Vite HTML shell.

### `admin/README.md`

Default Vite/React template documentation; not project-specific runtime logic.

### `admin/public/favicon.svg`

Frontend favicon asset.

### `admin/public/icons.svg`

Static SVG sprite/asset file.

### `admin/.gitignore`

Frontend ignore rules only.

---

## Architecture and Security Decisions

- Tenant scoping is explicit in SQL, not hidden behind ORM magic.
- JWT access token stays in memory only.
- Refresh token is isolated in an `httpOnly` cookie with path restriction.
- OAuth uses PKCE and signed state.
- Google identity anchors on `sub`, not email.
- CA private keys are encrypted at rest with AES-GCM and per-context HKDF-derived keys.
- Workspace creation is transactional and only becomes `active` after CA material is stored.

---

## Current Gaps and Notable Observations

- `controller/internal/middleware/session.go` is empty placeholder code.
- `controller/graph/resolvers/auth.resolvers.go` and `workspace.resolvers.go` are placeholders because current resolvers live in `schema.resolvers.go`.
- Frontend sign-out is client-side only and does not revoke refresh tokens server-side.
- `admin/src/components/ui/use-toast.ts` likely contains a reducer bug in `REMOVE_TOAST`.
- `implementation-report.md` was referenced in the IDE context but is not present in the repository root at the time of analysis.

---

## Short Summary

This codebase is a two-part system:

- The Go controller owns auth, tenant enforcement, GraphQL, bootstrap provisioning, and PKI.
- The React admin app owns login/signup UI, session handling in memory, Apollo transport, and dashboard/settings pages.

The most important cross-cutting path is:

- `InitiateAuth` in GraphQL
- `/auth/callback` in Go
- `bootstrap.Service`
- `pki.Service`
- JWT middleware
- React `AuthCallback`
- Apollo refresh retry flow

That path is the backbone of the entire product.
