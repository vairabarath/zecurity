package permission

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ── test harness ───────────────────────────────────────────────────────────────

func mustConnectPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func withDBName(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func applyMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)

	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			return err
		}
	}
	return nil
}

func mustInsertWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test') RETURNING id::text`, slug,
	).Scan(&id); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	return id
}

func mustInsertUser(t *testing.T, ctx context.Context, pool *pgxpool.Pool, workspaceID, email, role string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO users (tenant_id, email, provider, provider_sub, role)
		 VALUES ($1, $2, 'test', $2, $3) RETURNING id::text`,
		workspaceID, email, role,
	).Scan(&id); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return id
}

// ── integration entrypoint ─────────────────────────────────────────────────────

func TestPermissionStore_Integration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}

	// Migrate a throwaway database rather than the one PKI_TEST_DATABASE_URL
	// names. That database is shared with other packages' integration tests,
	// which `go test ./...` runs in parallel, so resetting its schema here
	// dropped their tables out from under them mid-test.
	ctx := context.Background()
	dbName := "permission_test_" + uuid.NewString()[0:8]

	adminPool := mustConnectPool(t, ctx, adminDSN)
	defer adminPool.Close()

	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectPool(t, ctx, testDSN)
	defer pool.Close()

	if err := applyMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	ws := mustInsertWorkspace(t, ctx, pool, "perm-"+time.Now().Format("150405"))
	alice := mustInsertUser(t, ctx, pool, ws, "alice@test", "admin") // ADMIN, no grant row
	bob := mustInsertUser(t, ctx, pool, ws, "bob@test", "member")    // member, gets grant

	store := NewStore(pool)

	t.Run("ADMIN without grant → denied (locked rule)", func(t *testing.T) {
		ok, err := store.HasPermission(ctx, ws, alice, BreakGlassMapping)
		if err != nil {
			t.Fatalf("HasPermission: %v", err)
		}
		if ok {
			t.Fatalf("ADMIN without an explicit row must NOT satisfy the permission")
		}
	})

	t.Run("explicit possession → true", func(t *testing.T) {
		if err := store.Grant(ctx, ws, bob, BreakGlassMapping, alice); err != nil {
			t.Fatalf("grant: %v", err)
		}
		ok, err := store.HasPermission(ctx, ws, bob, BreakGlassMapping)
		if err != nil {
			t.Fatalf("HasPermission: %v", err)
		}
		if !ok {
			t.Fatalf("Bob was granted %s but HasPermission returned false", BreakGlassMapping)
		}
	})

	t.Run("grant is idempotent", func(t *testing.T) {
		if err := store.Grant(ctx, ws, bob, BreakGlassMapping, alice); err != nil {
			t.Fatalf("duplicate grant: %v", err)
		}
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM workspace_permissions WHERE workspace_id=$1 AND user_id=$2 AND permission=$3`,
			ws, bob, BreakGlassMapping,
		).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		if n != 1 {
			t.Fatalf("expected exactly 1 grant row after duplicate grant, got %d", n)
		}
	})

	t.Run("scope isolation across users", func(t *testing.T) {
		// A different user in the same workspace must NOT have it.
		carol := mustInsertUser(t, ctx, pool, ws, "carol@test", "member")
		ok, err := store.HasPermission(ctx, ws, carol, BreakGlassMapping)
		if err != nil {
			t.Fatalf("HasPermission carol: %v", err)
		}
		if ok {
			t.Fatalf("Carol must not inherit Bob's grant")
		}
	})

	t.Run("revoke removes possession", func(t *testing.T) {
		if err := store.Revoke(ctx, ws, bob, BreakGlassMapping); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		ok, err := store.HasPermission(ctx, ws, bob, BreakGlassMapping)
		if err != nil {
			t.Fatalf("HasPermission after revoke: %v", err)
		}
		if ok {
			t.Fatalf("Bob's permission should be gone after revoke")
		}
		// Revoking a non-existent grant is a no-op, not an error.
		if err := store.Revoke(ctx, ws, bob, BreakGlassMapping); err != nil {
			t.Fatalf("idempotent revoke: %v", err)
		}
	})

	t.Run("invalid scope rejected", func(t *testing.T) {
		if _, err := store.HasPermission(ctx, "", bob, BreakGlassMapping); err != ErrInvalidScope {
			t.Fatalf("empty workspace should be ErrInvalidScope, got %v", err)
		}
		if err := store.Grant(ctx, ws, "", BreakGlassMapping, alice); err != ErrInvalidScope {
			t.Fatalf("empty user grant should be ErrInvalidScope, got %v", err)
		}
	})
}
