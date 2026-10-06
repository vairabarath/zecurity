package resource

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

// newResourceTestDB creates a throwaway database on the server named by
// RESOURCE_TEST_DATABASE_URL (used only as an admin connection), applies every
// schema file in controller/migrations in lexical order, and drops the database
// when the test ends. Tests must not rely on the schema already existing in the
// shared database: CI never provisions it, and a cached test result can hide
// that for a long time.
func newResourceTestDB(t *testing.T) *pgxpool.Pool {
	t.Helper()
	adminDSN := os.Getenv("RESOURCE_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("RESOURCE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("resource_test_%d_%d", os.Getpid(), time.Now().UnixNano())

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
		admin.Close()
		t.Fatalf("parse RESOURCE_TEST_DATABASE_URL: %v", err)
	}
	parsed.Path = "/" + dbName
	pool, err := pgxpool.New(ctx, parsed.String())
	if err != nil {
		admin.Close()
		t.Fatalf("connect test database: %v", err)
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
