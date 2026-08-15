package resource

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestBuildShieldSnapshotGeneration exercises the F11 fix against a real Postgres
// (migration 018 applied). Set RESOURCE_TEST_DATABASE_URL and RESOURCE_TEST_SHIELD_ID
// to run; skipped otherwise. It creates a throwaway resource bound to the given
// shield and asserts the generation only advances on real desired-state changes.
func TestBuildShieldSnapshotGeneration(t *testing.T) {
	dsn := os.Getenv("RESOURCE_TEST_DATABASE_URL")
	shieldID := os.Getenv("RESOURCE_TEST_SHIELD_ID")
	if dsn == "" || shieldID == "" {
		t.Skip("RESOURCE_TEST_DATABASE_URL / RESOURCE_TEST_SHIELD_ID not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// Registered first so it runs LAST: t.Cleanup is LIFO and the row-delete
	// cleanup below must still have a live pool. (A plain defer db.Close() would
	// run before any t.Cleanup, closing the pool out from under the delete.)
	t.Cleanup(db.Close)

	var tenantID, networkID string
	if err := db.QueryRow(ctx,
		`SELECT tenant_id, remote_network_id FROM shields WHERE id = $1`, shieldID,
	).Scan(&tenantID, &networkID); err != nil {
		t.Fatalf("load shield: %v", err)
	}

	// Throwaway resource, cleaned up at the end.
	var resID string
	if err := db.QueryRow(ctx,
		`INSERT INTO resources (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
		 VALUES ($1,$2,$3,'f11-test','10.0.0.99','tcp',8080,8080,'protected','apply')
		 RETURNING id`,
		tenantID, networkID, shieldID,
	).Scan(&resID); err != nil {
		t.Fatalf("insert test resource: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM resources WHERE id = $1`, resID)
	})

	gen := func() uint64 {
		t.Helper()
		snap, err := BuildShieldSnapshot(ctx, db, shieldID)
		if err != nil {
			t.Fatalf("BuildShieldSnapshot: %v", err)
		}
		return snap.Generation
	}

	g1 := gen() // first build with this content → bumps
	g2 := gen() // identical content → must dedup (no bump)
	if g2 != g1 {
		t.Fatalf("dedup failed: identical content bumped generation %d → %d", g1, g2)
	}

	// Metadata-only change must NOT bump.
	if _, err := db.Exec(ctx, `UPDATE resources SET description = 'noise' WHERE id = $1`, resID); err != nil {
		t.Fatalf("metadata update: %v", err)
	}
	g3 := gen()
	if g3 != g2 {
		t.Fatalf("metadata write churned generation %d → %d", g2, g3)
	}

	// Desired payload change (port) MUST bump.
	if _, err := db.Exec(ctx, `UPDATE resources SET port_to = 9090 WHERE id = $1`, resID); err != nil {
		t.Fatalf("port update: %v", err)
	}
	g4 := gen()
	if g4 <= g2 {
		t.Fatalf("payload change did not bump generation: %d → %d", g2, g4)
	}

	// ── Sprint 16 Phase 8: local_target ──────────────────────────────────────
	//
	// Two separate failures hide here and neither breaks the build:
	//   - desiredForShield without COALESCE(local_target, '') → the value is
	//     always "" and every shield keeps dialing its LAN IP
	//   - fingerprintDesired without r.LocalTarget → the generation never bumps,
	//     so the shield is never told to re-apply and the new value lands only on
	//     the next full reconnect
	//
	// Assert BOTH: the generation moved, and the value actually arrived. A test
	// that checks only the generation passes with an empty local_target.
	if _, err := db.Exec(ctx,
		`UPDATE resources SET local_target = '127.0.0.1' WHERE id = $1`, resID); err != nil {
		t.Fatalf("local_target update: %v", err)
	}
	snap, err := BuildShieldSnapshot(ctx, db, shieldID)
	if err != nil {
		t.Fatalf("BuildShieldSnapshot after local_target: %v", err)
	}
	gLocalTarget := snap.Generation
	if gLocalTarget <= g4 {
		t.Fatalf("local_target change did not bump generation: %d → %d — the shield "+
			"would never re-apply (fingerprintDesired must hash LocalTarget)", g4, gLocalTarget)
	}

	var found *PendingRow
	for _, r := range snap.Resources {
		if r.ID == resID {
			found = r
		}
	}
	if found == nil {
		t.Fatalf("test resource %s is missing from its own shield's snapshot", resID)
	}
	if found.LocalTarget != "127.0.0.1" {
		t.Fatalf("local_target = %q, want 127.0.0.1 — the value never reached the "+
			"snapshot (desiredForShield must select COALESCE(local_target, ''))",
			found.LocalTarget)
	}

	// Clearing it back to NULL must bump again and read as "" — never a stale
	// value and never a nil-deref. Empty is what tells the shield to fall back to
	// dialing `host`, so the round trip to empty is as load-bearing as setting it.
	if _, err := db.Exec(ctx,
		`UPDATE resources SET local_target = NULL WHERE id = $1`, resID); err != nil {
		t.Fatalf("local_target clear: %v", err)
	}
	snap, err = BuildShieldSnapshot(ctx, db, shieldID)
	if err != nil {
		t.Fatalf("BuildShieldSnapshot after clearing local_target: %v", err)
	}
	gCleared := snap.Generation
	if gCleared <= gLocalTarget {
		t.Fatalf("clearing local_target did not bump generation: %d → %d",
			gLocalTarget, gCleared)
	}
	for _, r := range snap.Resources {
		if r.ID == resID && r.LocalTarget != "" {
			t.Fatalf("cleared local_target = %q, want empty", r.LocalTarget)
		}
	}

	// Leaving the desired set (unprotect) MUST bump.
	if _, err := db.Exec(ctx, `UPDATE resources SET status = 'unprotected' WHERE id = $1`, resID); err != nil {
		t.Fatalf("unprotect update: %v", err)
	}
	g5 := gen()
	if g5 <= gCleared {
		t.Fatalf("leaving desired set did not bump generation: %d → %d", gCleared, g5)
	}

	t.Logf("generation lifecycle OK: first=%d dedup=%d metadata=%d payload=%d "+
		"local_target=%d cleared=%d left=%d",
		g1, g2, g3, g4, gLocalTarget, gCleared, g5)
}
