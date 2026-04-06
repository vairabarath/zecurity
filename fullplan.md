# ZTNA — Admin Bootstrap, Auth & WorkspaceCA Isolation
## Team Plan: 1 Frontend + 3 Backend

---

## What We Are Building Right Now

The Twingate admin console equivalent.
One React frontend. One Go Controller.
No Express. No proxy layer. React talks directly to Go via GraphQL.
Go owns everything: OIDC auth, workspace bootstrap, PKI, DB.

```
React Admin (shadcn/ui + Tailwind)
        ↓  GraphQL over HTTPS
Go Controller (gqlgen + pgx/v5)
        ↓
PostgreSQL (Docker)
```

gRPC is NOT in scope this sprint.
It gets added when we build the Connector and Linux client.
The folder structure is laid out for it now so the transition is zero-cost.

---

## gqlgen — Schema-First Clarification

gqlgen is schema-first, not code-first.

You write `schema.graphqls` first.
gqlgen reads it and generates Go interfaces and boilerplate.
You implement the resolver functions against those interfaces.

This is the correct approach for your system because:
- The schema file is the contract between Member 1 (frontend) and the 3 backend members
- Frontend runs graphql-codegen against the same schema to generate TypeScript types
- Both sides are always in sync — the schema is the single source of truth
- If a backend member changes the schema, the frontend TypeScript compiler
  immediately shows what broke — no manual coordination needed

Code-first (generating schema from Go structs) would remove that guarantee.
Schema-first keeps it. That is why gqlgen is the right choice here.

---

## Team Split — 1 Frontend + 3 Backend

### Member 1 — Frontend (React)
Owns the entire `/admin` directory.
React + TypeScript + Vite + shadcn/ui + Tailwind + Apollo Client.
Consumes the GraphQL schema the backend defines.
Does NOT invent API contracts — implements what the schema says.
Runs graphql-codegen to get TypeScript types automatically from schema.

### Member 2 — Backend: Auth + Session
Owns the OIDC flow inside Go.
PKCE generation, Google token exchange, id_token verification,
JWT issuance, refresh token (httpOnly cookie), Redis session storage.
Also owns the `/auth/callback` HTTP handler (not a GraphQL route).

### Member 3 — Backend: Bootstrap + PKI
Owns the most critical piece: the atomic workspace creation transaction
and the entire PKI package (Root CA, Intermediate CA, WorkspaceCA).
This is the hardest work on the team. Nothing else fully works until
Member 3's bootstrap transaction is solid.

### Member 4 — Backend: GraphQL Schema + Resolvers + DB + Middleware
Owns `schema.graphqls` — writes and owns the contract.
Sets up gqlgen, pgx connection pool, Docker PostgreSQL, migrations.
Implements TenantDB middleware, TenantContext, all query resolvers.
Is the integration point — unblocks Member 1 with a working schema
and unblocks Member 2+3 by providing the DB layer they call into.

---

## Folder Structure

```
ztna/
├── admin/                           ← Member 1
│   ├── src/
│   │   ├── apollo/                  ← Apollo Client setup, auth link
│   │   ├── pages/
│   │   │   ├── Login.tsx            ← initiateAuth → redirect → read token
│   │   │   ├── Dashboard.tsx        ← me query, workspace query
│   │   │   └── Settings.tsx         ← workspace settings
│   │   ├── components/              ← shadcn/ui wrappers, layout
│   │   ├── graphql/
│   │   │   ├── mutations.graphql    ← initiateAuth
│   │   │   └── queries.graphql      ← me, workspace
│   │   ├── hooks/
│   │   │   ├── useAuth.ts           ← token storage, refresh logic
│   │   │   └── useWorkspace.ts
│   │   └── generated/               ← DO NOT EDIT — codegen output
│   └── codegen.yml                  ← points at controller schema
│
├── controller/                      ← Member 2 + 3 + 4
│   ├── cmd/
│   │   └── server/
│   │       └── main.go              ← wires everything, starts HTTP server
│   ├── graph/                       ← Member 4 owns this
│   │   ├── schema.graphqls          ← THE contract. written first.
│   │   ├── generated.go             ← DO NOT EDIT — gqlgen output
│   │   ├── resolver.go              ← gqlgen base resolver struct
│   │   └── resolvers/
│   │       ├── auth.resolvers.go    ← initiateAuth mutation
│   │       └── workspace.resolvers.go ← me, workspace queries
│   ├── internal/
│   │   ├── auth/                    ← Member 2
│   │   │   ├── oidc.go              ← PKCE, token exchange, id_token verify
│   │   │   ├── session.go           ← JWT sign/verify, refresh token
│   │   │   └── callback.go          ← HTTP handler for /auth/callback
│   │   ├── bootstrap/               ← Member 3
│   │   │   └── bootstrap.go         ← atomic workspace + user + CA transaction
│   │   ├── pki/                     ← Member 3
│   │   │   ├── root.go              ← Root CA init (runs once at startup)
│   │   │   ├── intermediate.go      ← Intermediate CA init
│   │   │   └── workspace.go         ← WorkspaceCA generation per workspace
│   │   ├── db/                      ← Member 4
│   │   │   ├── pool.go              ← pgx connection pool
│   │   │   └── tenant.go            ← TenantDB wrapper (enforcer)
│   │   ├── tenant/                  ← Member 4
│   │   │   └── context.go           ← TenantContext struct, context keys
│   │   ├── middleware/              ← Member 4
│   │   │   └── session.go           ← JWT verify → TenantContext inject
│   │   └── models/                  ← Member 4
│   │       ├── workspace.go
│   │       └── user.go
│   ├── migrations/
│   │   └── 001_schema.sql           ← Member 4
│   └── docker-compose.yml           ← Member 4
│
├── proto/                           ← FUTURE (not this sprint)
│   └── connector.proto              ← gRPC service definition (added later)
│
└── plan.md
```

---

## The GraphQL Schema — Written by Member 4 First

This is the first thing that gets committed.
Member 1 cannot start building pages without it.
Member 2 + 3 cannot write resolvers without it.
Member 4 writes this on day one.

```graphql
# controller/graph/schema.graphqls

type Query {
  # Returns the currently authenticated user.
  # Requires valid JWT in Authorization header.
  me: User!

  # Returns the workspace the current user belongs to.
  # Scoped to tenant_id from JWT — never crosses workspaces.
  workspace: Workspace!
}

type Mutation {
  # Step 1 of login: React calls this first.
  # Go generates PKCE pair, stores code_verifier in Redis,
  # builds and returns the Google OAuth redirect URL.
  initiateAuth(provider: String!): AuthInitPayload!

  # Step 2 of login: NOT a GraphQL call.
  # Google redirects to /auth/callback (plain HTTP handler in Go).
  # Go handles token exchange and redirects React back with JWT.
  # This mutation is kept here for documentation only.
  # It is implemented as a plain HTTP handler, not a resolver.
}

type AuthInitPayload {
  # The full Google OAuth URL React should redirect the browser to.
  redirectUrl: String!
  # The signed state value. React stores this for CSRF verification.
  state:       String!
}

type User {
  id:          ID!
  email:       String!
  role:        Role!
  provider:    String!
  createdAt:   String!
}

type Workspace {
  id:          ID!
  slug:        String!
  name:        String!
  status:      WorkspaceStatus!
  createdAt:   String!
}

enum Role {
  ADMIN
  MEMBER
  VIEWER
}

enum WorkspaceStatus {
  PROVISIONING
  ACTIVE
  SUSPENDED
  DELETED
}
```

This schema is intentionally small. It covers only bootstrap + auth.
Invite user, create resource, manage policies — all future sprints.

---

## Auth Flow — Go Handles OIDC Directly (No Express)

### Step 1 — React calls initiateAuth mutation

```
React → POST /graphql
  mutation { initiateAuth(provider: "google") {
    redirectUrl
    state
  }}

Member 2 (Go):
  generate code_verifier (random 64 bytes, base64url)
  generate code_challenge = SHA256(code_verifier), base64url
  generate state = HMAC(random_nonce, JWT_SECRET)
  store { code_verifier } in Redis, key=state, TTL=5min
  build Google OAuth URL:
    https://accounts.google.com/o/oauth2/v2/auth
    ?client_id=<CLIENT_ID>
    &redirect_uri=https://<domain>/auth/callback
    &response_type=code
    &scope=openid email profile
    &code_challenge=<challenge>
    &code_challenge_method=S256
    &state=<state>
  return { redirectUrl, state }

React:
  store state in sessionStorage
  redirect browser to redirectUrl
```

### Step 2 — Google redirects to Go /auth/callback

```
Google → GET /auth/callback?code=<code>&state=<state>

Member 2 (Go HTTP handler — not GraphQL):
  verify state HMAC against JWT_SECRET
  compare state with sessionStorage value (via cookie or re-check)
  retrieve code_verifier from Redis using state as key
  delete code_verifier from Redis immediately (single use)

  POST to https://oauth2.googleapis.com/token:
    code=<code>
    code_verifier=<verifier>
    client_id=<CLIENT_ID>
    client_secret=<CLIENT_SECRET>
    redirect_uri=<same as above>
    grant_type=authorization_code

  receive { id_token, access_token, expires_in }

  verify id_token:
    fetch Google JWKS from https://www.googleapis.com/oauth2/v3/certs
    verify signature against matching kid
    check aud == CLIENT_ID
    check iss == "https://accounts.google.com"
    check exp > now()

  extract claims: email, sub (provider_sub), name

  call bootstrap.Bootstrap(ctx, email, "google", sub, name)
  ← this is a direct Go function call, not a network call
  receive { tenant_id, user_id, role }

  sign JWT:
    { sub: user_id, tenant_id, role, iss: "ztna-controller", exp: now+15min }
    signed with HS256 using JWT_SECRET
    (swap to RS256 later — one config change)

  create refresh token:
    random 256-bit value
    store in Redis: key=refresh:<user_id>, value=token, TTL=7days
    set as httpOnly + SameSite=Strict + Secure cookie

  redirect to React:
    302 → https://<domain>/auth/callback#token=<JWT>
    (hash fragment — never sent to server, only browser reads it)
```

### Step 3 — React reads token, calls me query

```
React:
  reads JWT from URL hash (#token=...)
  clears hash from URL (replaceState)
  stores JWT in memory (React context / Zustand — NOT localStorage)

React → POST /graphql:
  query { me { id email role } workspace { id slug name status } }
  Authorization: Bearer <JWT>

Member 4 (Go middleware):
  reads Authorization header
  verifies JWT signature + exp
  extracts: user_id (sub), tenant_id, role
  builds TenantContext{ TenantID, UserID, Role }
  stores in request context (typed key, not string key)

  checks workspace status:
    SELECT status FROM workspaces WHERE id = tenant_id
    if status != 'active' → GraphQL error "workspace unavailable"

Member 4 (Go resolver — me query):
  reads TenantContext from ctx
  calls TenantDB.QueryRow(ctx,
    "SELECT id, email, role, provider, created_at
     FROM users
     WHERE id = $1 AND tenant_id = $2",
    userID, tenantID)
  returns User

Member 4 (Go resolver — workspace query):
  reads TenantContext from ctx
  calls TenantDB.QueryRow(ctx,
    "SELECT id, slug, name, status, created_at
     FROM workspaces
     WHERE id = $1",
    tenantID)
  returns Workspace

React:
  receives User + Workspace
  renders admin dashboard
```

---

## Bootstrap Transaction — Member 3

Pure in-process Go function. No HTTP. No gRPC. Direct function call from Member 2.

```go
// internal/bootstrap/bootstrap.go

func Bootstrap(ctx context.Context,
    email, provider, providerSub, name string,
) (*Result, error)
```

```
BEGIN TRANSACTION (pgx Tx)

  STEP 1 — Check returning user
    SELECT id, tenant_id, role FROM users
    WHERE provider_sub = $1 AND provider = $2
    FOUND:
      UPDATE users SET last_login_at = NOW() WHERE id = $id
      COMMIT
      return { tenant_id, user_id, role }   ← done, skip below

  NOT FOUND — first signup:

  STEP 2 — Create workspace
    INSERT INTO workspaces (slug, name, status='provisioning')
    → tenant_id generated by Postgres (gen_random_uuid())

  STEP 3 — Create first admin user
    INSERT INTO users
      (tenant_id, email, provider, provider_sub, role='admin')
    → user_id generated by Postgres

  STEP 4 — Generate WorkspaceCA keypair IN MEMORY
    algorithm: EC P-384
    private key object lives only in RAM at this point
    never written to disk or DB unencrypted

  STEP 5 — Build and sign CSR with Intermediate CA
    CSR subject: CN=workspace-<tenant_id>
    CSR SAN:     URI:tenant:<tenant_id>   ← isolation anchor
    Sign with Intermediate CA private key
    → signed WorkspaceCA certificate (PEM)

  STEP 6 — Encrypt WorkspaceCA private key
    derive encryption key: HKDF(master_secret, tenant_id, "workspace-ca")
    encrypt with AES-256-GCM
    → ciphertext (base64) + nonce (base64)
    private key object zeroed from memory after encryption

  STEP 7 — Store encrypted key
    INSERT INTO workspace_ca_keys
      (tenant_id, encrypted_private_key, nonce,
       key_algorithm='EC-P384', certificate_pem,
       not_before, not_after)

  STEP 8 — Activate workspace
    UPDATE workspaces
    SET status='active', ca_cert_pem=<signed cert PEM>
    WHERE id = tenant_id

COMMIT

return { tenant_id, user_id, role='admin' }
```

Any failure in steps 2–8 triggers ROLLBACK.
No workspace row survives with status='provisioning'.
No orphaned CA keys exist without a workspace.
The guarantee: workspace exists and is 'active', or it does not exist at all.

---

## PKI Hierarchy — Member 3

Set up once at controller startup. Not per-request.

```
STARTUP SEQUENCE (runs in main.go before HTTP server starts):

  pki.InitRootCA()
    Check: does root CA exist in DB (table: ca_root)?
    NO → generate EC P-384 keypair
         self-sign → Root CA certificate
         encrypt private key with master secret
         store in DB
         log: "Root CA initialized"
    YES → load, decrypt, hold in memory for Intermediate CA signing only

  pki.InitIntermediateCA()
    Check: does intermediate CA exist in DB (table: ca_intermediate)?
    NO → generate EC P-384 keypair
         build CSR
         sign CSR with Root CA → Intermediate CA certificate
         encrypt private key with master secret
         store in DB
         zero Root CA private key from memory ← Root CA done, not needed again
         log: "Intermediate CA initialized"
    YES → load, decrypt, hold in memory (needed for WorkspaceCA signing)

HTTP server starts ONLY after both CAs are initialized.
```

```
PER WORKSPACE (called from bootstrap transaction):

  pki.GenerateWorkspaceCA(tenantID string) (*WorkspaceCAResult, error)
    generate EC P-384 keypair
    build CSR with SAN = URI:tenant:<tenantID>
    sign with Intermediate CA (loaded in memory at startup)
    encrypt private key with HKDF(master_secret, tenantID, "workspace-ca")
    return { encryptedKey, nonce, certPEM, notBefore, notAfter }
    ← caller (bootstrap) stores this in workspace_ca_keys table
```

```
CERTIFICATE CHAIN:
  Root CA (self-signed, EC P-384)
    └── Intermediate CA (signed by Root, EC P-384)
          └── WorkspaceCA-<tenantID> (signed by Intermediate, EC P-384)
                └── [future] Device cert (signed by WorkspaceCA)
                └── [future] Connector cert (signed by WorkspaceCA)

VERIFICATION OF ANY FUTURE LEAF CERT:
  1. Is signature valid? (cert chain up to Root CA)
  2. Is cert not expired? (not_before, not_after)
  3. Does SAN tenant:<tenantID> match JWT tenant_id claim?
  Step 3 is the isolation guarantee. Cryptographic chain alone is not enough.
```

---

## DB Schema — Member 4

```sql
-- 001_schema.sql

CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Root and Intermediate CAs stored once at controller startup
CREATE TABLE ca_root (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    encrypted_key   TEXT        NOT NULL,
    nonce           TEXT        NOT NULL,
    certificate_pem TEXT        NOT NULL,
    not_before      TIMESTAMPTZ NOT NULL,
    not_after       TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE ca_intermediate (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    encrypted_key   TEXT        NOT NULL,
    nonce           TEXT        NOT NULL,
    certificate_pem TEXT        NOT NULL,
    not_before      TIMESTAMPTZ NOT NULL,
    not_after       TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Workspaces: root of tenant hierarchy
CREATE TABLE workspaces (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT        NOT NULL UNIQUE,
    name        TEXT        NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'provisioning'
                            CHECK (status IN
                              ('provisioning','active','suspended','deleted')),
    ca_cert_pem TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Users: one row per (provider_sub, workspace)
-- Same Google account in two workspaces = two rows with different tenant_id
CREATE TABLE users (
    id              UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID    NOT NULL REFERENCES workspaces(id)
                            ON DELETE CASCADE,
    email           TEXT    NOT NULL,
    provider        TEXT    NOT NULL,
    provider_sub    TEXT    NOT NULL,
    role            TEXT    NOT NULL DEFAULT 'member'
                            CHECK (role IN ('admin','member','viewer')),
    status          TEXT    NOT NULL DEFAULT 'active'
                            CHECK (status IN ('active','suspended','deleted')),
    last_login_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tenant_id, provider_sub)
);

-- WorkspaceCA private keys: encrypted at rest, never returned via API
CREATE TABLE workspace_ca_keys (
    id                    UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID    NOT NULL REFERENCES workspaces(id)
                                  ON DELETE CASCADE UNIQUE,
    encrypted_private_key TEXT    NOT NULL,
    nonce                 TEXT    NOT NULL,
    key_algorithm         TEXT    NOT NULL DEFAULT 'EC-P384',
    certificate_pem       TEXT    NOT NULL,
    not_before            TIMESTAMPTZ NOT NULL,
    not_after             TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Indexes
-- provider_sub lookup happens before tenant_id is known (auth flow step 1)
CREATE INDEX idx_users_provider_sub  ON users (provider_sub, provider);
-- All other queries are scoped to tenant_id
CREATE INDEX idx_users_tenant_email  ON users (tenant_id, email);
CREATE INDEX idx_users_tenant_role   ON users (tenant_id, role);
```

---

## Middleware Stack — Member 4

```
Every GraphQL request:

  1. CORS + rate limit (all requests)

  2. Session middleware (protected routes only)
       read Authorization: Bearer <token>
       verify JWT: signature, exp, iss
       FAIL → GraphQL error { code: UNAUTHORIZED }
       PASS → extract sub (user_id), tenant_id, role
              build TenantContext{ TenantID, UserID, Role }
              store in ctx with typed key (not string key)

  3. Workspace status guard
       SELECT status FROM workspaces WHERE id = $tenant_id
       status != 'active' → GraphQL error { code: WORKSPACE_UNAVAILABLE }

  4. TenantDB enforcer (applied at query execution time)
       every call to TenantDB.Query(ctx, sql, args...)
       reads TenantContext from ctx
       MISSING → panic("query attempted without TenantContext")
       ← this is a hard programming error, not a runtime condition
       PRESENT → proceed, query executes with tenant_id scoped

  5. GraphQL resolver runs
       has TenantContext available
       all DB calls go through TenantDB wrapper
       returns only data belonging to that tenant_id

Public routes that bypass step 2–4:
  POST /graphql with initiateAuth mutation
  GET  /auth/callback
  GET  /health
```

---

## What Member 1 Builds (Frontend)

Day 1: project setup, codegen wired to `controller/graph/schema.graphqls`.
TypeScript types generate automatically from schema. No manual type writing.

```
Login page
  → call initiateAuth(provider: "google")
  → receive redirectUrl
  → redirect browser to redirectUrl
  → on return: read JWT from URL hash
  → store JWT in Zustand store (memory only, never localStorage)
  → redirect to /dashboard

Dashboard page
  → call me query + workspace query
  → render user name, role, workspace name, status

Token refresh
  → on 401 from any GraphQL call:
      POST /auth/refresh (plain HTTP, sends httpOnly cookie automatically)
      receive new JWT
      retry original request
  → on refresh failure: redirect to login

Apollo Client setup
  → auth link: reads JWT from Zustand, attaches as Bearer header
  → error link: handles 401 → trigger refresh flow
```

shadcn/ui provides: sidebar nav, cards, badges, buttons, form inputs.
No custom component design work needed. Focus is on the data flow.

---

## Day-by-Day Sequence for the Sprint

### Day 1 — Unblock everyone
Member 4: commit `schema.graphqls` and `001_schema.sql` and `docker-compose.yml`.
Member 1: set up Vite project, run codegen, confirm TypeScript types generate.
Member 2: set up Go module, confirm pgx connects to Docker Postgres.
Member 3: write `pki` package interfaces (no implementation yet, just signatures).

### Day 2–3 — Core backend
Member 2: implement `oidc.go` — PKCE generation, token exchange, id_token verify.
Member 3: implement `pki/root.go` and `pki/intermediate.go` — CA initialization.
Member 4: implement `db/pool.go`, `tenant/context.go`, `middleware/session.go`.

### Day 4–5 — Bootstrap + integration
Member 3: implement `pki/workspace.go` + `bootstrap/bootstrap.go`.
Member 2: implement `auth/callback.go` — wires oidc → bootstrap → JWT issue.
Member 4: implement `me` and `workspace` resolvers.

### Day 6 — Frontend wires up
Member 1: login page calls initiateAuth, reads token from hash, calls me query.
All members: integration test the full flow end to end.

### Day 7 — Harden
Full flow test: sign in → bootstrap → dashboard → refresh → sign out.
Verify: workspace status gate, TenantDB enforcer panic on missing context,
WorkspaceCA cert chain validates correctly.

---

## gRPC — Added Later, Zero Rework

When Connector sprint starts:

```
ztna/
  proto/
    connector.proto    ← new file

controller/
  internal/
    grpc/
      connector_server.go  ← new file
      implements ConnectorService from proto

main.go adds:
  grpcServer := grpc.NewServer(grpc.Creds(tlsCreds))
  pb.RegisterConnectorServiceServer(grpcServer, &ConnectorServer{})
  go grpcServer.Serve(grpcListener)   ← port 8443
  httpServer.Serve(httpListener)      ← port 443 (GraphQL, unchanged)
```

Same Go process. Same DB pool. Same TenantContext.
The Connector presents its WorkspaceCA-signed mTLS cert.
Go gRPC server extracts tenant_id from cert SAN — same isolation model.
GraphQL and gRPC run side by side in the same binary.
Zero rework on auth, bootstrap, PKI, or DB layer.

---

## Summary

```
Member 1  React + shadcn/ui + Apollo Client + codegen
Member 2  OIDC in Go: PKCE, token exchange, id_token verify, JWT, callback handler
Member 3  Bootstrap transaction + entire PKI package (Root→Intermediate→WorkspaceCA)
Member 4  Schema (contract), gqlgen setup, pgx, DB middleware, resolvers

Protocol  GraphQL over HTTPS (admin console, this sprint)
          gRPC over mTLS (Connector + Linux client, future sprint)

Auth      OIDC handled entirely in Go — no Express, no proxy
PKI       Three-level CA hierarchy, WorkspaceCA per workspace,
          created atomically with workspace in a single DB transaction
Isolation tenant_id on every table, TenantContext in every request,
          TenantDB enforcer panics on missing context (hard fail)
DB        PostgreSQL in Docker, 5 tables, schema-first migrations
gRPC      Wired into same Go binary later, no rework needed
```
