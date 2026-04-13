package db

import (
	"context"

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
