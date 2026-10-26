package resource

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Sprint 20 Fix 16 (F50): a resource whose shield is revoked or gone is
// deleted at once, and a 'deleting' tombstone that got no removal ack within
// the grace is reaped by the sweeper. Runs against a real Postgres in its own
// throwaway database; set RESOURCE_TEST_DATABASE_URL (admin DSN).
//
//	RESOURCE_TEST_DATABASE_URL=postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform \
//	  go test ./internal/resource/ -run 'TestDeleteIfShieldGone|TestReapStaleTombstones' -v
func f50Fixture(t *testing.T) (db *pgxpool.Pool, ctx context.Context, tenantID, connectorID string, mk func(shieldStatus, resStatus string, port int) (string, string)) {
	t.Helper()
	db = newResourceTestDB(t)
	var networkID string
	ctx = context.Background()
	slug := fmt.Sprintf("f50-test-%d", time.Now().UnixNano())
	if err := db.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug, "ws-"+slug+".zecurity.in",
	).Scan(&tenantID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, tenantID)
	})
	if err := db.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net', 'other') RETURNING id`,
		tenantID,
	).Scan(&networkID); err != nil {
		t.Fatalf("insert remote_network: %v", err)
	}
	if err := db.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status)
		 VALUES ($1, $2, 'conn', 'active') RETURNING id`,
		tenantID, networkID,
	).Scan(&connectorID); err != nil {
		t.Fatalf("insert connector: %v", err)
	}
	mk = func(shieldStatus, resStatus string, port int) (string, string) {
		t.Helper()
		var shieldID, resID string
		if err := db.QueryRow(ctx,
			`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status)
			 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			tenantID, networkID, connectorID, fmt.Sprintf("shield-%d", port), shieldStatus,
		).Scan(&shieldID); err != nil {
			t.Fatalf("insert shield(%s): %v", shieldStatus, err)
		}
		if err := db.QueryRow(ctx,
			`INSERT INTO resources
			   (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
			 VALUES ($1, $2, $3, $4, '10.0.0.1', 'tcp', $5, $5, $6, 'remove') RETURNING id`,
			tenantID, networkID, shieldID, fmt.Sprintf("res-%d", port), port, resStatus,
		).Scan(&resID); err != nil {
			t.Fatalf("insert resource(%s/%s): %v", shieldStatus, resStatus, err)
		}
		return shieldID, resID
	}
	return db, ctx, tenantID, connectorID, mk
}

func TestDeleteIfShieldGone(t *testing.T) {
	db, ctx, tenantID, _, mk := f50Fixture(t)
	cases := []struct {
		shield, res string
		want        bool
	}{
		{"active", "protected", false},       // live shield: normal remove + ack path
		{"disconnected", "protected", false}, // offline for now: the sweeper decides later
		{"revoked", "protected", true},       // no shield will ever ack
		{"revoked", "failed", true},
		{"revoked", "deleting", true}, // a tombstone already stuck
	}
	for i, c := range cases {
		_, resID := mk(c.shield, c.res, 9100+i)
		got, err := DeleteIfShieldGone(ctx, db, tenantID, resID)
		if err != nil {
			t.Fatalf("shield=%s res=%s: %v", c.shield, c.res, err)
		}
		if got != c.want {
			t.Errorf("shield=%s res=%s: deleted=%v, want %v", c.shield, c.res, got, c.want)
		}
	}
	// Tenant-scoped: another tenant's id never matches.
	_, resID := mk("revoked", "protected", 9199)
	if got, _ := DeleteIfShieldGone(ctx, db, "00000000-0000-0000-0000-000000000000", resID); got {
		t.Error("deleted across tenants")
	}
}

func TestReapStaleTombstones(t *testing.T) {
	db, ctx, _, connectorID, mk := f50Fixture(t)
	shieldOld, oldID := mk("active", "deleting", 9201)
	_, freshID := mk("active", "deleting", 9202)
	_, protID := mk("active", "protected", 9203)
	if _, err := db.Exec(ctx,
		`UPDATE resources SET updated_at = NOW() - INTERVAL '5 seconds' WHERE id = ANY($1)`,
		[]string{oldID, protID},
	); err != nil {
		t.Fatalf("age rows: %v", err)
	}

	reaped, err := ReapStaleTombstones(ctx, db, 2*time.Second)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(reaped) != 1 || reaped[0].ID != oldID {
		t.Fatalf("reaped %+v, want only the stale tombstone %s", reaped, oldID)
	}
	if reaped[0].ShieldID != shieldOld || reaped[0].ConnectorID != connectorID {
		t.Errorf("reaped %+v: want shield %s connector %s for the snapshot re-push", reaped[0], shieldOld, connectorID)
	}
	var left []string
	rows, err := db.Query(ctx, `SELECT id FROM resources WHERE id = ANY($1) ORDER BY id`, []string{oldID, freshID, protID})
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		left = append(left, id)
	}
	rows.Close()
	if len(left) != 2 {
		t.Errorf("left %v: the fresh tombstone and the protected row must stay", left)
	}
}
