package policy

import (
	"context"
	"os"
	"testing"
)

// FU-7 (Sprint 22 Phase D): the ACL snapshot's top-level relay address never
// names a draining relay, even the most recently heartbeating one.
func TestGetActiveRelay_SkipsDrainingRelay(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueTestDBName(t)
	adminPool := mustConnectTestPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()
	testDBDSN, err := withTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatal(err)
	}
	pool := mustConnectTestPool(t, ctx, testDBDSN)
	defer pool.Close()
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	var fresh, older string
	if err := pool.QueryRow(ctx, `INSERT INTO relays (name, status, public_addr, last_heartbeat_at)
		VALUES ('fresh', 'active', 'fresh.relay.test:9093', NOW()) RETURNING id::text`).Scan(&fresh); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO relays (name, status, public_addr, last_heartbeat_at)
		VALUES ('older', 'active', 'older.relay.test:9093', NOW() - interval '1 minute') RETURNING id::text`).Scan(&older); err != nil {
		t.Fatal(err)
	}
	store := NewStore(pool)
	pick := func() string {
		t.Helper()
		r, err := store.GetActiveRelay(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if r == nil {
			return ""
		}
		return r.ID
	}
	if got := pick(); got != fresh {
		t.Fatalf("before drain: picked %s, want %s", got, fresh)
	}
	if _, err := pool.Exec(ctx, `UPDATE relays SET drain_started_at = NOW() WHERE id = $1`, fresh); err != nil {
		t.Fatal(err)
	}
	if got := pick(); got != older {
		t.Fatalf("after drain: picked %s, want %s", got, older)
	}
	if _, err := pool.Exec(ctx, `UPDATE relays SET drain_started_at = NOW() WHERE id = $1`, older); err != nil {
		t.Fatal(err)
	}
	if got := pick(); got != "" {
		t.Fatalf("all draining: picked %s, want none", got)
	}
}
