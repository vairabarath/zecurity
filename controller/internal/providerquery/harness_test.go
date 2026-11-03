package providerquery

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// newTestDB creates a throwaway database on the PKI_TEST_DATABASE_URL server,
// applies every migration in order, and drops it afterwards (same pattern as
// internal/provider/store_test.go). It never touches the dev database.
func newTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("providerquery_test_%d_%d", os.Getpid(), time.Now().UnixNano())

	admin, err := pgxpool.New(ctx, adminDSN)
	if err != nil {
		t.Fatalf("connect admin: %v", err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		admin.Close()
		t.Fatalf("create test database: %v", err)
	}
	parsed, err := url.Parse(adminDSN)
	if err != nil {
		t.Fatal(err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		t.Fatalf("connect test db: %v", err)
	}
	t.Cleanup(func() {
		pool.Close()
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
		admin.Close()
	})

	files, err := filepath.Glob(filepath.Join("..", "..", "migrations", "*.sql"))
	if err != nil || len(files) == 0 {
		t.Fatalf("list migrations: %v (%d files)", err, len(files))
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			t.Fatalf("apply %s: %v", filepath.Base(f), err)
		}
	}
	return pool
}

func mustExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}

func mustScalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", sql, err)
	}
	return v
}

// seedRelay inserts a relay and returns its id. createdAt fixes the keyset
// position so pagination tests are deterministic.
func seedRelay(t *testing.T, pool *pgxpool.Pool, name, status string, createdAt time.Time) string {
	t.Helper()
	return mustScalar[string](t, pool,
		`INSERT INTO relays (name, status, created_at) VALUES ($1, $2, $3) RETURNING id::text`,
		name, status, createdAt)
}

// seedTenant inserts a workspace and one remote network; returns both ids.
func seedTenant(t *testing.T, pool *pgxpool.Pool, slug string) (tenantID, networkID string) {
	t.Helper()
	tenantID = mustScalar[string](t, pool,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $1 || '.zecurity.test') RETURNING id::text`, slug)
	networkID = mustScalar[string](t, pool,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net-' || $2, 'office') RETURNING id::text`, tenantID, slug)
	return tenantID, networkID
}

// fakeHeartbeats is a HeartbeatSource for unit tests.
type fakeHeartbeats struct {
	at  map[string]time.Time
	err error
}

func (f fakeHeartbeats) LastHeartbeat(_ context.Context, id string) (time.Time, bool, error) {
	if f.err != nil {
		return time.Time{}, false, f.err
	}
	t, ok := f.at[id]
	return t, ok, nil
}
