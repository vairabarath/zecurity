# Phase 1 — Ship First (Unblocks Everyone)

These four things go out in the first commit. Nothing else matters until they land.
Commit these, push immediately. The rest of the team starts the moment these land.

---

## File 1: `controller/docker-compose.yml`

**Path:** `controller/docker-compose.yml`

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

---

## File 2: `controller/migrations/001_schema.sql`

**Path:** `controller/migrations/001_schema.sql`

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

---

## File 3: `controller/graph/schema.graphqls`

**Path:** `controller/graph/schema.graphqls`

This file already exists and matches the plan. Verify it contains:

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

---

## File 4: `controller/internal/auth/service.go`

**Path:** `controller/internal/auth/service.go`

Interface only. Member 2 implements. Member 4 consumes.

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

---

## Verification Checklist

```
[ ] docker-compose up starts Postgres + Redis with no errors
[ ] 001_schema.sql runs cleanly on fresh Postgres container
[ ] schema.graphqls committed and pushed
[ ] auth/service.go interface committed and pushed
```
