# Member 4 — Deep Implementation Plan
## GraphQL Schema · gqlgen · pgx · TenantDB · Middleware · Resolvers

---

## Role on the Team

Member 4 is the integration pillar.
Everything ships from here first. Every other member depends on
something Member 4 commits before they can start.

```
Member 1 needs → schema.graphqls
Member 2 needs → auth.Service interface + db/pool.go
Member 3 needs → db/pool.go + 001_schema.sql
```

Commit these four things first. Push immediately.
The rest of the team starts the moment these land.

---

## Everything Member 4 Owns

```
controller/
  docker-compose.yml
  .env.example
  go.mod / go.sum
  migrations/
    001_schema.sql
  graph/
    schema.graphqls               ← THE contract, committed first
    gqlgen.yml
    generated.go                  ← DO NOT EDIT (gqlgen output)
    models_gen.go                 ← DO NOT EDIT (gqlgen output)
    resolver.go
    resolvers/
      auth.resolvers.go           ← thin stub, delegates to auth.Service
      query.resolvers.go          ← me + workspace resolvers
  internal/
    auth/
      service.go                  ← interface only, Member 2 implements
    db/
      pool.go
      tenant.go                   ← TenantDB enforcer
    tenant/
      context.go                  ← TenantContext + typed ctx keys
    middleware/
      auth.go                     ← JWT verify → TenantContext inject
      workspace.go                ← workspace status guard
    models/
      workspace.go
      user.go
  cmd/
    server/
      main.go
```

Member 4 never touches:
- `internal/auth/` beyond the interface file
- `internal/bootstrap/`
- `internal/pki/`

---

## Build Order — Strictly by Dependency

### Phase 1 — Ship First (Unblocks Everyone)

These four things go out in the first commit. Nothing else matters until they land.

**docker-compose.yml**

```yaml
version: "3.9"

services:
  postgres:
    image: postgres:16-alpine
    container_name: ztna_postgres
    restart: unless-stopped
    environment:
      POSTGRES_USER: ztna
      POSTGRES_PASSWORD: ztna_dev_secret
      POSTGRES_DB: ztna_platform
    ports:
      - "5432:5432"
    volumes:
      - ./migrations:/docker-entrypoint-initdb.d
      - ztna_pgdata:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ztna -d ztna_platform"]
      interval: 5s
      timeout: 5s
      retries: 5

  redis:
    image: redis:7-alpine
    container_name: ztna_redis
    restart: unless-stopped
    ports:
      - "6379:6379"
    healthcheck:
      test: ["CMD", "redis-cli", "ping"]
      interval: 5s
      timeout: 3s
      retries: 5

volumes:
  ztna_pgdata:
```

**migrations/001_schema.sql**

```sql
CREATE EXTENSION IF NOT EXISTS "pgcrypto";

-- Root CA — created once at controller startup
CREATE TABLE ca_root (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    encrypted_key   TEXT        NOT NULL,
    nonce           TEXT        NOT NULL,
    certificate_pem TEXT        NOT NULL,
    not_before      TIMESTAMPTZ NOT NULL,
    not_after       TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Intermediate CA — created once at controller startup
CREATE TABLE ca_intermediate (
    id              UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    encrypted_key   TEXT        NOT NULL,
    nonce           TEXT        NOT NULL,
    certificate_pem TEXT        NOT NULL,
    not_before      TIMESTAMPTZ NOT NULL,
    not_after       TIMESTAMPTZ NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Workspaces — root of the tenant hierarchy
-- status: provisioning → active → suspended → deleted
-- ca_cert_pem: public WorkspaceCA cert only, never the private key
CREATE TABLE workspaces (
    id          UUID        PRIMARY KEY DEFAULT gen_random_uuid(),
    slug        TEXT        NOT NULL UNIQUE,
    name        TEXT        NOT NULL,
    status      TEXT        NOT NULL DEFAULT 'provisioning'
                            CHECK (status IN (
                              'provisioning','active','suspended','deleted'
                            )),
    ca_cert_pem TEXT,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Users — one row per (provider_sub, workspace)
-- provider_sub is the IdP's immutable subject — NOT email
-- Same Google account in two workspaces = two rows, two tenant_ids
-- UNIQUE (tenant_id, provider_sub) = idempotency guard for bootstrap
CREATE TABLE users (
    id              UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id       UUID    NOT NULL
                            REFERENCES workspaces(id)
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

-- WorkspaceCA private keys — encrypted at rest
-- NEVER returned via GraphQL
-- NEVER in any API response
-- Accessed only by PKI service (Member 3)
CREATE TABLE workspace_ca_keys (
    id                    UUID    PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id             UUID    NOT NULL
                                  REFERENCES workspaces(id)
                                  ON DELETE CASCADE
                                  UNIQUE,
    encrypted_private_key TEXT    NOT NULL,
    nonce                 TEXT    NOT NULL,
    key_algorithm         TEXT    NOT NULL DEFAULT 'EC-P384',
    certificate_pem       TEXT    NOT NULL,
    not_before            TIMESTAMPTZ NOT NULL,
    not_after             TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- provider_sub lookup is the first query in every auth flow
-- happens before tenant_id is known — must be fast
CREATE INDEX idx_users_provider_sub ON users (provider_sub, provider);

-- all subsequent queries scoped to tenant_id
CREATE INDEX idx_users_tenant_email ON users (tenant_id, email);
CREATE INDEX idx_users_tenant_role  ON users (tenant_id, role);

-- workspace status check happens on every authenticated request
CREATE INDEX idx_workspaces_active  ON workspaces (id)
    WHERE status = 'active';
```

**graph/schema.graphqls**

```graphql
type Query {
  """
  Returns the authenticated user.
  Requires valid JWT. Scoped to tenant_id from JWT.
  """
  me: User!

  """
  Returns the workspace the current user belongs to.
  Never returns data from another workspace.
  """
  workspace: Workspace!
}

type Mutation {
  """
  Step 1 of login. Generates PKCE pair, returns Google OAuth URL.
  Public — no JWT required.
  """
  initiateAuth(provider: String!): AuthInitPayload!
}

type AuthInitPayload {
  """Full Google OAuth URL. React redirects the browser here."""
  redirectUrl: String!

  """HMAC-signed state. React stores in sessionStorage for CSRF check."""
  state: String!
}

type User {
  id:        ID!
  email:     String!
  role:      Role!
  provider:  String!
  createdAt: String!
}

type Workspace {
  id:        ID!
  slug:      String!
  name:      String!
  status:    WorkspaceStatus!
  createdAt: String!
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

**internal/auth/service.go** — interface only, Member 2 implements

```go
package auth

import (
    "context"
    "net/http"

    "github.com/yourorg/ztna/controller/graph/model"
)

// Service is the contract between Member 4 (consumer)
// and Member 2 (implementor).
// Member 4 depends on this interface.
// Member 2 writes the concrete implementation.
// Neither touches the other's files.
type Service interface {
    // InitiateAuth builds the IdP redirect URL with PKCE.
    // Called by the initiateAuth GraphQL mutation resolver.
    InitiateAuth(ctx context.Context, provider string) (*model.AuthInitPayload, error)

    // CallbackHandler handles GET /auth/callback.
    // Google redirects here after user authenticates.
    // Verifies state, exchanges code, calls Bootstrap,
    // issues JWT, sets refresh cookie, redirects React.
    CallbackHandler() http.Handler

    // RefreshHandler handles POST /auth/refresh.
    // Reads httpOnly refresh cookie, issues new JWT.
    RefreshHandler() http.Handler
}
```

Once these four files are committed and `docker-compose up` runs cleanly,
push and notify the team. Everyone starts immediately.

---

### Phase 2 — Core Types and DB Layer

Build these next. Members 2 and 3 need them to write their code.

**internal/models/workspace.go**

```go
package models

import "time"

type Workspace struct {
    ID         string    `db:"id"`
    Slug       string    `db:"slug"`
    Name       string    `db:"name"`
    Status     string    `db:"status"`
    CACertPEM  *string   `db:"ca_cert_pem"`
    CreatedAt  time.Time `db:"created_at"`
    UpdatedAt  time.Time `db:"updated_at"`
}
```

**internal/models/user.go**

```go
package models

import "time"

type User struct {
    ID          string     `db:"id"`
    TenantID    string     `db:"tenant_id"`
    Email       string     `db:"email"`
    Provider    string     `db:"provider"`
    ProviderSub string     `db:"provider_sub"`
    Role        string     `db:"role"`
    Status      string     `db:"status"`
    LastLoginAt *time.Time `db:"last_login_at"`
    CreatedAt   time.Time  `db:"created_at"`
    UpdatedAt   time.Time  `db:"updated_at"`
}
```

**internal/tenant/context.go**

```go
package tenant

import "context"

// contextKey is an unexported named type.
// Prevents key collisions with any other package storing values in context.
// Never use a raw string as a context key.
type contextKey string

const key contextKey = "tenantContext"

// TenantContext holds the verified identity for one request.
// Extracted from the JWT by AuthMiddleware.
// Every resolver and DB call reads from this — never from raw JWT claims.
// All three fields are populated together or not at all.
type TenantContext struct {
    TenantID string // workspace UUID
    UserID   string // user UUID
    Role     string // "admin" | "member" | "viewer"
}

// Set stores a TenantContext into ctx.
// Called only by AuthMiddleware after JWT verification succeeds.
func Set(ctx context.Context, tc TenantContext) context.Context {
    return context.WithValue(ctx, key, tc)
}

// Get retrieves the TenantContext from ctx.
// Returns (zero, false) if not present.
// Use this when absence is a valid case (e.g. public route handlers).
func Get(ctx context.Context) (TenantContext, bool) {
    tc, ok := ctx.Value(key).(TenantContext)
    return tc, ok
}

// MustGet retrieves the TenantContext from ctx.
// Panics if not present.
//
// Use this in all resolvers and repository functions.
// A missing TenantContext at this point means middleware was bypassed —
// that is always a programming error, never a user error.
// It must panic loudly so it gets caught and fixed immediately.
// A silent error return would let it go unnoticed until production.
func MustGet(ctx context.Context) TenantContext {
    tc, ok := Get(ctx)
    if !ok {
        panic(
            "tenant.MustGet: TenantContext not in context. " +
            "AuthMiddleware was bypassed. This is a code bug.",
        )
    }
    return tc
}
```

**internal/db/pool.go**

```go
package db

import (
    "context"
    "fmt"
    "os"
    "time"

    "github.com/jackc/pgx/v5/pgxpool"
)

var Pool *pgxpool.Pool

// Init creates the pgx connection pool from DATABASE_URL.
// Verifies connectivity before returning.
// Must be called before any DB operations.
// HTTP server must not start until this returns nil.
func Init(ctx context.Context) error {
    dsn := os.Getenv("DATABASE_URL")
    if dsn == "" {
        return fmt.Errorf("DATABASE_URL not set")
    }

    cfg, err := pgxpool.ParseConfig(dsn)
    if err != nil {
        return fmt.Errorf("parse DATABASE_URL: %w", err)
    }

    cfg.MaxConns = 25
    cfg.MinConns = 2
    cfg.MaxConnWaitDuration = 5 * time.Second

    pool, err := pgxpool.NewWithConfig(ctx, cfg)
    if err != nil {
        return fmt.Errorf("create pool: %w", err)
    }

    if err := pool.Ping(ctx); err != nil {
        return fmt.Errorf("ping database: %w", err)
    }

    Pool = pool
    return nil
}

func Close() {
    if Pool != nil {
        Pool.Close()
    }
}
```

**internal/db/tenant.go**

```go
package db

import (
    "context"
    "fmt"

    "github.com/jackc/pgx/v5"
    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/yourorg/ztna/controller/internal/tenant"
)

// TenantDB wraps a pgx pool and enforces that every query
// runs with a valid TenantContext in the context.
//
// Design decision: TenantDB does NOT auto-append WHERE tenant_id = $x.
// Every SQL string explicitly includes the tenant_id parameter.
// This makes isolation visible and auditable in every query.
// TenantDB just guarantees the context is valid before Postgres sees the query.
//
// If TenantContext is missing → panic.
// This is always a programming error. Fail loudly.
type TenantDB struct {
    pool *pgxpool.Pool
}

func NewTenantDB(pool *pgxpool.Pool) *TenantDB {
    return &TenantDB{pool: pool}
}

func (t *TenantDB) require(ctx context.Context) tenant.TenantContext {
    return tenant.MustGet(ctx)
}

// QueryRow executes a query returning a single row.
// SQL must include tenant_id scoping explicitly.
func (t *TenantDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
    t.require(ctx)
    return t.pool.QueryRow(ctx, sql, args...)
}

// Query executes a query returning multiple rows.
// SQL must include tenant_id scoping explicitly.
func (t *TenantDB) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
    t.require(ctx)
    return t.pool.Query(ctx, sql, args...)
}

// Exec executes INSERT, UPDATE, or DELETE.
// SQL must include tenant_id scoping explicitly.
func (t *TenantDB) Exec(ctx context.Context, sql string, args ...any) error {
    t.require(ctx)
    _, err := t.pool.Exec(ctx, sql, args...)
    return err
}

// BeginTx starts a transaction.
// All queries within the transaction must also scope by tenant_id.
func (t *TenantDB) BeginTx(ctx context.Context) (pgx.Tx, error) {
    t.require(ctx)
    return t.pool.Begin(ctx)
}

// RawPool returns the underlying pool for operations that are
// explicitly NOT tenant-scoped: PKI table reads, health checks,
// workspace status guard, migrations.
// Every call site must have a comment explaining why raw pool is used.
func (t *TenantDB) RawPool() *pgxpool.Pool {
    return t.pool
}
```

---

### Phase 3 — Middleware

**internal/middleware/auth.go**

```go
package middleware

import (
    "fmt"
    "net/http"
    "strings"

    "github.com/golang-jwt/jwt/v5"
    "github.com/yourorg/ztna/controller/internal/tenant"
)

type Claims struct {
    TenantID string `json:"tenant_id"`
    Role     string `json:"role"`
    jwt.RegisteredClaims
}

// AuthMiddleware verifies the JWT and injects TenantContext.
//
// On valid JWT   → calls next with TenantContext in ctx
// On invalid JWT → returns 401 JSON, stops the chain
//
// Public routes must be registered outside this middleware.
// No bypass logic lives here — route registration handles that.
func AuthMiddleware(secret string) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

            raw := r.Header.Get("Authorization")
            if raw == "" {
                writeJSON401(w, "missing Authorization header")
                return
            }

            parts := strings.SplitN(raw, " ", 2)
            if len(parts) != 2 || parts[0] != "Bearer" {
                writeJSON401(w, "malformed Authorization header")
                return
            }

            claims := &Claims{}
            token, err := jwt.ParseWithClaims(
                parts[1], claims,
                func(t *jwt.Token) (interface{}, error) {
                    // Enforce expected signing method.
                    // Skipping this check allows alg=none attacks.
                    if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
                        return nil, fmt.Errorf("unexpected alg: %v", t.Header["alg"])
                    }
                    return []byte(secret), nil
                },
                jwt.WithIssuer("ztna-controller"),
                jwt.WithExpirationRequired(),
            )
            if err != nil || !token.Valid {
                writeJSON401(w, "invalid or expired token")
                return
            }

            if claims.Subject == "" || claims.TenantID == "" || claims.Role == "" {
                writeJSON401(w, "token missing required claims")
                return
            }

            ctx := tenant.Set(r.Context(), tenant.TenantContext{
                TenantID: claims.TenantID,
                UserID:   claims.Subject,
                Role:     claims.Role,
            })

            next.ServeHTTP(w, r.WithContext(ctx))
        })
    }
}

func writeJSON401(w http.ResponseWriter, msg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusUnauthorized)
    fmt.Fprintf(w,
        `{"errors":[{"message":%q,"extensions":{"code":"UNAUTHORIZED"}}]}`, msg)
}
```

**internal/middleware/workspace.go**

```go
package middleware

import (
    "fmt"
    "net/http"

    "github.com/jackc/pgx/v5/pgxpool"
    "github.com/yourorg/ztna/controller/internal/tenant"
)

// WorkspaceGuard checks workspace status = 'active' before
// allowing the request through.
//
// Runs after AuthMiddleware (requires TenantContext).
// Runs before any GraphQL resolver.
//
// 'provisioning' → bootstrap transaction did not complete
// 'suspended'    → admin disabled the workspace
// 'deleted'      → workspace is gone
// All non-active states → 403, request stops
func WorkspaceGuard(pool *pgxpool.Pool) func(http.Handler) http.Handler {
    return func(next http.Handler) http.Handler {
        return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {

            tc, ok := tenant.Get(r.Context())
            if !ok {
                writeJSON403(w, "no tenant context")
                return
            }

            // Raw pool — this is infrastructure middleware, not a
            // tenant-scoped business query. tenant_id is explicit.
            var status string
            err := pool.QueryRow(r.Context(),
                "SELECT status FROM workspaces WHERE id = $1",
                tc.TenantID,
            ).Scan(&status)

            if err != nil {
                writeJSON403(w, "workspace not found")
                return
            }

            if status != "active" {
                writeJSON403(w, fmt.Sprintf("workspace not active: %s", status))
                return
            }

            next.ServeHTTP(w, r)
        })
    }
}

func writeJSON403(w http.ResponseWriter, msg string) {
    w.Header().Set("Content-Type", "application/json")
    w.WriteHeader(http.StatusForbidden)
    fmt.Fprintf(w,
        `{"errors":[{"message":%q,"extensions":{"code":"FORBIDDEN"}}]}`, msg)
}
```

---

### Phase 4 — gqlgen Setup

**graph/gqlgen.yml**

```yaml
schema:
  - graph/schema.graphqls

exec:
  filename: graph/generated.go
  package: graph

model:
  filename: graph/models_gen.go
  package: graph

resolver:
  layout: follow-schema
  dir: graph/resolvers
  package: resolvers
  filename_template: "{name}.resolvers.go"

models:
  User:
    model: github.com/yourorg/ztna/controller/internal/models.User
  Workspace:
    model: github.com/yourorg/ztna/controller/internal/models.Workspace
```

Run this after any schema change:
```
go run github.com/99designs/gqlgen generate
```

**graph/resolver.go**

```go
package graph

import (
    "github.com/yourorg/ztna/controller/internal/auth"
    "github.com/yourorg/ztna/controller/internal/db"
)

// Resolver holds shared dependencies for all resolvers.
// Member 4 owns this struct.
// Add fields here when new services are needed by resolvers.
type Resolver struct {
    TenantDB    *db.TenantDB
    AuthService auth.Service
}
```

---

### Phase 5 — Resolvers

**graph/resolvers/query.resolvers.go**

```go
package resolvers

import (
    "context"
    "fmt"

    "github.com/yourorg/ztna/controller/internal/models"
    "github.com/yourorg/ztna/controller/internal/tenant"
)

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
    if err != nil {
        return nil, fmt.Errorf("me: %w", err)
    }
    return &u, nil
}

func (r *queryResolver) Workspace(ctx context.Context) (*models.Workspace, error) {
    tc := tenant.MustGet(ctx)

    var ws models.Workspace
    err := r.TenantDB.QueryRow(ctx,
        `SELECT id, slug, name, status, ca_cert_pem, created_at, updated_at
         FROM workspaces
         WHERE id = $1`,
        tc.TenantID,
    ).Scan(
        &ws.ID, &ws.Slug, &ws.Name,
        &ws.Status, &ws.CACertPEM,
        &ws.CreatedAt, &ws.UpdatedAt,
    )
    if err != nil {
        return nil, fmt.Errorf("workspace: %w", err)
    }
    return &ws, nil
}
```

Note: `Workspace` query does not add `AND tenant_id = $x` because
`id` in the workspaces table IS the tenant_id — it is the root table.
`Me` query adds `AND tenant_id = $2` as defence-in-depth even though
`id` alone uniquely identifies the user.

**graph/resolvers/auth.resolvers.go**

```go
package resolvers

import (
    "context"

    "github.com/yourorg/ztna/controller/graph/model"
)

// InitiateAuth is intentionally thin.
// All logic lives in internal/auth/ (Member 2's territory).
// This file is just the GraphQL entry point.
func (r *mutationResolver) InitiateAuth(
    ctx context.Context, provider string,
) (*model.AuthInitPayload, error) {
    return r.AuthService.InitiateAuth(ctx, provider)
}
```

---

### Phase 6 — main.go

```go
package main

import (
    "context"
    "log"
    "net/http"
    "os"

    "github.com/99designs/gqlgen/graphql/handler"
    "github.com/99designs/gqlgen/graphql/playground"
    "github.com/yourorg/ztna/controller/graph"
    "github.com/yourorg/ztna/controller/internal/auth"
    "github.com/yourorg/ztna/controller/internal/db"
    "github.com/yourorg/ztna/controller/internal/middleware"
    "github.com/yourorg/ztna/controller/internal/pki"
)

func main() {
    ctx := context.Background()

    // 1. Connect to Postgres
    if err := db.Init(ctx); err != nil {
        log.Fatalf("db init: %v", err)
    }
    defer db.Close()
    log.Println("✓ database connected")

    // 2. Initialize PKI — HTTP server does not start until CAs exist
    pkiService, err := pki.Init(ctx, db.Pool)
    if err != nil {
        log.Fatalf("pki init: %v", err)
    }
    log.Println("✓ PKI ready (root CA + intermediate CA)")

    // 3. Build shared infrastructure
    tenantDB := db.NewTenantDB(db.Pool)

    // 4. Build Auth service (Member 2 implements auth.NewService)
    authSvc := auth.NewService(auth.Config{
        Pool:               db.Pool,
        PKIService:         pkiService,
        JWTSecret:          mustEnv("JWT_SECRET"),
        GoogleClientID:     mustEnv("GOOGLE_CLIENT_ID"),
        GoogleClientSecret: mustEnv("GOOGLE_CLIENT_SECRET"),
        RedirectURI:        mustEnv("GOOGLE_REDIRECT_URI"),
        RedisURL:           mustEnv("REDIS_URL"),
    })

    // 5. Build GraphQL server
    gqlSrv := handler.NewDefaultServer(
        graph.NewExecutableSchema(graph.Config{
            Resolvers: &graph.Resolver{
                TenantDB:    tenantDB,
                AuthService: authSvc,
            },
        }),
    )

    // 6. Register routes
    mux := http.NewServeMux()

    // Public — no auth middleware
    mux.Handle("/auth/callback", authSvc.CallbackHandler())
    mux.Handle("/auth/refresh",  authSvc.RefreshHandler())
    mux.Handle("/health",        healthHandler())

    // Playground — development only
    if os.Getenv("ENV") == "development" {
        mux.Handle("/playground", playground.Handler("ZTNA", "/graphql"))
        log.Println("✓ playground at /playground")
    }

    // Protected GraphQL endpoint
    // Chain: AuthMiddleware → WorkspaceGuard → GraphQL handler
    // Public mutations (initiateAuth) bypass via operationName check
    jwtSecret := mustEnv("JWT_SECRET")
    protected := middleware.AuthMiddleware(jwtSecret)(
        middleware.WorkspaceGuard(db.Pool)(gqlSrv),
    )
    mux.Handle("/graphql", routeGraphQL(protected, gqlSrv))

    // 7. Start server
    addr := ":" + envOr("PORT", "8080")
    log.Printf("✓ listening on %s", addr)
    log.Fatal(http.ListenAndServe(addr, mux))
}

// routeGraphQL sends public mutations directly to gqlSrv (no auth)
// and everything else through the protected chain.
// Public mutations are identified by the X-Operation header,
// set by Apollo Client for the initiateAuth mutation only.
func routeGraphQL(protected, public http.Handler) http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if r.Header.Get("X-Public-Operation") == "initiateAuth" {
            public.ServeHTTP(w, r)
            return
        }
        protected.ServeHTTP(w, r)
    })
}

func healthHandler() http.Handler {
    return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        if err := db.Pool.Ping(r.Context()); err != nil {
            w.WriteHeader(http.StatusServiceUnavailable)
            return
        }
        w.WriteHeader(http.StatusOK)
        w.Write([]byte(`{"status":"ok"}`))
    })
}

func mustEnv(key string) string {
    v := os.Getenv(key)
    if v == "" {
        log.Fatalf("required env var %s not set", key)
    }
    return v
}

func envOr(key, fallback string) string {
    if v := os.Getenv(key); v != "" {
        return v
    }
    return fallback
}
```

---

## .env.example

```env
DATABASE_URL=postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform
REDIS_URL=redis://localhost:6379
PORT=8080
ENV=development

JWT_SECRET=replace_with_32_plus_random_bytes
JWT_ISSUER=ztna-controller

GOOGLE_CLIENT_ID=your_google_client_id
GOOGLE_CLIENT_SECRET=your_google_client_secret
GOOGLE_REDIRECT_URI=http://localhost:8080/auth/callback

PKI_MASTER_SECRET=replace_with_64_plus_random_bytes

ALLOWED_ORIGIN=http://localhost:5173
```

---

## Integration Points With Other Members

### With Member 1 (Frontend)

Member 1 runs graphql-codegen against `graph/schema.graphqls`.
If Member 4 adds or changes a type, Member 1 reruns codegen.
TypeScript compiler shows what broke. No manual coordination needed.

Member 1 sets `X-Public-Operation: initiateAuth` header in Apollo Client
for the initiateAuth mutation only. This is what `routeGraphQL` reads
to skip auth middleware.

### With Member 2 (Auth)

Member 2 implements `auth.Service` interface from `internal/auth/service.go`.
Member 2 also implements `auth.NewService(auth.Config{...})` constructor.
Member 4 calls `auth.NewService` in `main.go` — the config struct must match.

Member 2 agrees on the `Claims` struct in `internal/middleware/auth.go`.
Member 2's `session.go` must produce JWTs with the same fields:
`sub`, `tenant_id`, `role`, `iss: "ztna-controller"`.
If these diverge, JWT verification fails. Agree before either implements.

### With Member 3 (Bootstrap + PKI)

Member 3 implements `pki.Init(ctx, pool)` returning a `pki.Service`.
Member 4 calls it in `main.go`. The return type must be agreed on.

Member 3's bootstrap function reads and writes to the tables Member 4
defined in `001_schema.sql`. Member 3 must not alter the schema directly —
any changes go through Member 4 who updates the migration file.

---

## Checklist — What Done Looks Like

```
Phase 1 — Unblocks team
  ✓ docker-compose up starts Postgres + Redis with no errors
  ✓ schema.graphqls committed and pushed
  ✓ auth.Service interface committed and pushed
  ✓ 001_schema.sql runs cleanly on fresh Postgres container

Phase 2 — Core types + DB
  ✓ models.User and models.Workspace match schema exactly
  ✓ tenant.MustGet panics with descriptive message when ctx is empty
  ✓ db.Init returns nil on valid DATABASE_URL
  ✓ TenantDB.QueryRow panics when ctx has no TenantContext
  ✓ TenantDB.QueryRow does NOT panic when ctx has TenantContext

Phase 3 — Middleware
  ✓ AuthMiddleware returns 401 on missing header
  ✓ AuthMiddleware returns 401 on expired token
  ✓ AuthMiddleware returns 401 on wrong signing method
  ✓ AuthMiddleware injects TenantContext on valid token
  ✓ WorkspaceGuard returns 403 when workspace status != 'active'
  ✓ WorkspaceGuard calls next when workspace status = 'active'

Phase 4 — gqlgen
  ✓ go run github.com/99designs/gqlgen generate completes with no errors
  ✓ generated.go and models_gen.go exist and compile

Phase 5 — Resolvers
  ✓ me resolver returns user scoped to tenant_id from JWT
  ✓ workspace resolver returns workspace scoped to tenant_id from JWT
  ✓ initiateAuth resolver delegates to AuthService without error

Phase 6 — main.go
  ✓ server starts and /health returns 200
  ✓ /graphql returns 401 without JWT
  ✓ /graphql returns user data with valid JWT
  ✓ /auth/callback route registered (Member 2's handler)
  ✓ /playground available in development mode only
```

---

## Summary

```
Phase 1  docker-compose + schema + interface + migration → push → team starts
Phase 2  models + tenant context + db pool + TenantDB enforcer
Phase 3  auth middleware + workspace guard
Phase 4  gqlgen config + generate + base resolver struct
Phase 5  me resolver + workspace resolver + initiateAuth stub
Phase 6  main.go wiring + health check + route registration
```
