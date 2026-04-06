# Phase 2 — Core Types and DB Layer

Build these next. Members 2 and 3 need them to write their code.

---

## File 1: `controller/internal/models/workspace.go`

**Path:** `controller/internal/models/workspace.go`

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

---

## File 2: `controller/internal/models/user.go`

**Path:** `controller/internal/models/user.go`

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

---

## File 3: `controller/internal/tenant/context.go`

**Path:** `controller/internal/tenant/context.go`

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

---

## File 4: `controller/internal/db/pool.go`

**Path:** `controller/internal/db/pool.go`

```go
package db

import (
	"context"
	"fmt"
	"os"

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
	cfg.MinIdleConns = 2

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

---

## File 5: `controller/internal/db/tenant.go`

**Path:** `controller/internal/db/tenant.go`

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

## Verification Checklist

```
[ ] models.User and models.Workspace match schema exactly
[ ] tenant.MustGet panics with descriptive message when ctx is empty
[ ] db.Init returns nil on valid DATABASE_URL
[ ] TenantDB.QueryRow panics when ctx has no TenantContext
[ ] TenantDB.QueryRow does NOT panic when ctx has TenantContext
```
