package shield

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestUpdateShieldHealthResyncsResourceHostOnLanIPChange exercises the shield-sync
// fix against a real Postgres (migrations applied): when a shield's LAN IP changes
// on heartbeat, the resources bound to it that were tracking the old IP are
// re-pointed to the new IP, lanIPChanged is reported, and unrelated hosts
// (127.0.0.1) are preserved. Gated on SHIELD_TEST_DATABASE_URL; skipped otherwise.
func TestUpdateShieldHealthResyncsResourceHostOnLanIPChange(t *testing.T) {
	dsn := os.Getenv("SHIELD_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SHIELD_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	// Defensive: clear any leftover from a crashed prior run (slug is UNIQUE).
	_, _ = db.Exec(ctx, `DELETE FROM workspaces WHERE slug = 'shieldsync-test'`)

	must := func(q string, args ...any) string {
		t.Helper()
		var id string
		if err := db.QueryRow(ctx, q, args...).Scan(&id); err != nil {
			t.Fatalf("seed (%s): %v", q, err)
		}
		return id
	}

	// Seed the FK chain; everything cascades from the workspace on cleanup.
	wsID := must(`INSERT INTO workspaces (slug, name, trust_domain, status) VALUES ('shieldsync-test','shieldsync','shieldsync.example.com','active') RETURNING id`)
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, wsID) })

	rnID := must(`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1,'rn','office') RETURNING id`, wsID)
	connID := must(`INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1,$2,'conn','active') RETURNING id`, wsID, rnID)
	shieldID := must(`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, lan_ip)
		VALUES ($1,$2,$3,'sh','active','10.0.0.5') RETURNING id`, wsID, rnID, connID)

	// One resource tracking the shield's LAN IP, one on localhost (must be preserved).
	trackResID := must(`INSERT INTO resources (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
		VALUES ($1,$2,$3,'db','10.0.0.5','tcp',5432,5432,'protected','apply') RETURNING id`, wsID, rnID, shieldID)
	loopResID := must(`INSERT INTO resources (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
		VALUES ($1,$2,$3,'local','127.0.0.1','tcp',9000,9000,'protected','apply') RETURNING id`, wsID, rnID, shieldID)

	svc := &service{db: db}

	hostOf := func(id string) string {
		t.Helper()
		var h string
		if err := db.QueryRow(ctx, `SELECT host FROM resources WHERE id = $1`, id).Scan(&h); err != nil {
			t.Fatalf("read host: %v", err)
		}
		return h
	}

	// 1. LAN IP changes → lanIPChanged, tracking resource re-pointed, localhost preserved.
	_, lanIPChanged, err := svc.UpdateShieldHealth(ctx, shieldID, connID, "active", "v1", "10.0.0.9", 1000)
	if err != nil {
		t.Fatalf("UpdateShieldHealth (change): %v", err)
	}
	if !lanIPChanged {
		t.Fatal("expected lanIPChanged=true on IP change")
	}
	if got := hostOf(trackResID); got != "10.0.0.9" {
		t.Fatalf("tracking resource host not re-synced: got %q want 10.0.0.9", got)
	}
	if got := hostOf(loopResID); got != "127.0.0.1" {
		t.Fatalf("localhost resource must be preserved: got %q want 127.0.0.1", got)
	}

	// 2. Same LAN IP → no change reported, hosts untouched.
	_, lanIPChanged, err = svc.UpdateShieldHealth(ctx, shieldID, connID, "active", "v1", "10.0.0.9", 1001)
	if err != nil {
		t.Fatalf("UpdateShieldHealth (no-op): %v", err)
	}
	if lanIPChanged {
		t.Fatal("expected lanIPChanged=false when IP unchanged")
	}
	if got := hostOf(trackResID); got != "10.0.0.9" {
		t.Fatalf("host changed on a no-op heartbeat: got %q", got)
	}

	// 3. Connector not active → the shields UPDATE matches 0 rows (heartbeat
	//    rejected). Resources must NOT be re-synced even though a new IP was
	//    reported — regression guard for the data-modifying-CTE edge (synced must
	//    be gated on `updated` actually changing the shield row).
	if _, err := db.Exec(ctx, `UPDATE connectors SET status = 'pending' WHERE id = $1`, connID); err != nil {
		t.Fatalf("deactivate connector: %v", err)
	}
	_, lanIPChanged, err = svc.UpdateShieldHealth(ctx, shieldID, connID, "active", "v1", "10.0.0.99", 1002)
	if err != nil {
		t.Fatalf("UpdateShieldHealth (inactive connector): %v", err)
	}
	if lanIPChanged {
		t.Fatal("expected lanIPChanged=false when the shield row was not updated")
	}
	if got := hostOf(trackResID); got != "10.0.0.9" {
		t.Fatalf("resource host wrongly re-synced despite a rejected heartbeat: got %q want 10.0.0.9", got)
	}

	// 4. Empty lan_ip (shield IP detection failed) → resources.host must be
	//    preserved (never wiped to empty); the `NULLIF($6,'') IS NOT NULL` guard
	//    keeps the last-known-good host so routing continues.
	if _, err := db.Exec(ctx, `UPDATE connectors SET status = 'active' WHERE id = $1`, connID); err != nil {
		t.Fatalf("reactivate connector: %v", err)
	}
	if _, _, err := svc.UpdateShieldHealth(ctx, shieldID, connID, "active", "v1", "", 1003); err != nil {
		t.Fatalf("UpdateShieldHealth (empty lan_ip): %v", err)
	}
	if got := hostOf(trackResID); got != "10.0.0.9" {
		t.Fatalf("resource host wrongly wiped on empty lan_ip heartbeat: got %q want 10.0.0.9", got)
	}
}
