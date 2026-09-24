package relay

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestEvictRelaysIntegration_HeartbeatWinsRace proves the guarded eviction:
// a relay selected as an eviction candidate whose heartbeat is persisted before
// EvictRelays runs must NOT be evicted, while a relay that stays stale is.
// Requires PKI_TEST_DATABASE_URL; otherwise skips.
func TestEvictRelaysIntegration_HeartbeatWinsRace(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueRelayTestDBName(t)

	adminPool := mustConnectRelayPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := withRelayTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectRelayPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyRelayMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	store := NewStore(pool)

	// Two active relays whose persisted heartbeat is 10 minutes old.
	newStaleActiveRelay := func(name string) string {
		t.Helper()
		id, err := store.CreateRelay(ctx, name, []string{}, []string{})
		if err != nil {
			t.Fatalf("create relay %s: %v", name, err)
		}
		if _, err := pool.Exec(ctx,
			`UPDATE relays SET status = 'active', last_heartbeat_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`,
			id,
		); err != nil {
			t.Fatalf("make relay %s stale: %v", name, err)
		}
		return id
	}
	racing := newStaleActiveRelay("racing-relay")
	dead := newStaleActiveRelay("dead-relay")

	threshold := time.Now().UTC().Add(-90 * time.Second)
	candidates, err := store.ListEvictionCandidates(ctx, threshold)
	if err != nil {
		t.Fatalf("ListEvictionCandidates: %v", err)
	}
	if len(candidates) != 2 {
		t.Fatalf("candidates = %v, want both stale relays", candidates)
	}

	// The racing relay's heartbeat is persisted between candidate selection and
	// eviction (the throttled DB write came due).
	if err := store.RecordHeartbeat(ctx, racing, "2a", time.Now().Add(time.Hour),
		"1.0.0", "relay-a", "", 0, "", "", 0, 1024); err != nil {
		t.Fatalf("RecordHeartbeat: %v", err)
	}

	evicted, err := store.EvictRelays(ctx, candidates, threshold)
	if err != nil {
		t.Fatalf("EvictRelays: %v", err)
	}
	if len(evicted) != 1 || evicted[0] != dead {
		t.Fatalf("evicted = %v, want only the dead relay %s", evicted, dead)
	}

	for id, want := range map[string]string{racing: "active", dead: "inactive"} {
		row, err := store.LoadRelayByID(ctx, id)
		if err != nil {
			t.Fatalf("LoadRelayByID %s: %v", id, err)
		}
		if row.Status != want {
			t.Fatalf("relay %s status = %q, want %q", id, row.Status, want)
		}
	}
}
