package resource

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestMarkUnprotectingResolvesDeadShield exercises Finding 7 against a real
// Postgres: unprotecting a resource whose shield is REVOKED must resolve straight
// to the terminal 'unprotected' state, instead of parking in 'protecting' forever
// waiting for an ack from a shield that no longer exists. A live ('active') or
// merely-disconnected shield must still take the normal 'protecting'/remove path,
// because it can still apply the removal and ack it (disconnected acks on reconnect).
//
// Self-provisioning: creates its own workspace/network/connector/shields/resources
// and tears them all down via the workspace CASCADE. Set RESOURCE_TEST_DATABASE_URL
// to run; skipped otherwise.
//
//	RESOURCE_TEST_DATABASE_URL=postgres://ztna:ztna_dev_secret@localhost:5432/ztna_platform \
//	  go test ./internal/resource/ -run TestMarkUnprotectingResolvesDeadShield -v
func TestMarkUnprotectingResolvesDeadShield(t *testing.T) {
	dsn := os.Getenv("RESOURCE_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("RESOURCE_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	db, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(db.Close)

	slug := fmt.Sprintf("f7-test-%d", time.Now().UnixNano())

	var tenantID string
	if err := db.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug, "ws-"+slug+".zecurity.in",
	).Scan(&tenantID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	// Deleting the workspace CASCADEs to networks/connectors/shields/resources.
	t.Cleanup(func() {
		_, _ = db.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, tenantID)
	})

	var networkID string
	if err := db.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net', 'other') RETURNING id`,
		tenantID,
	).Scan(&networkID); err != nil {
		t.Fatalf("insert remote_network: %v", err)
	}

	var connectorID string
	if err := db.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status)
		 VALUES ($1, $2, 'conn', 'active') RETURNING id`,
		tenantID, networkID,
	).Scan(&connectorID); err != nil {
		t.Fatalf("insert connector: %v", err)
	}

	// Make a shield with the given status + a 'protected' resource bound to it.
	mkCase := func(shieldStatus string, port int) string {
		t.Helper()
		var shieldID string
		if err := db.QueryRow(ctx,
			`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status)
			 VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			tenantID, networkID, connectorID, "shield-"+shieldStatus, shieldStatus,
		).Scan(&shieldID); err != nil {
			t.Fatalf("insert shield(%s): %v", shieldStatus, err)
		}
		var resID string
		if err := db.QueryRow(ctx,
			`INSERT INTO resources
			   (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
			 VALUES ($1, $2, $3, $4, '10.0.0.1', 'tcp', $5, $5, 'protected', 'apply') RETURNING id`,
			tenantID, networkID, shieldID, "res-"+shieldStatus, port,
		).Scan(&resID); err != nil {
			t.Fatalf("insert resource(%s): %v", shieldStatus, err)
		}
		return resID
	}

	cases := []struct {
		shieldStatus string
		wantStatus   string
	}{
		{"active", "protecting"},       // normal path — shield applies + acks the removal
		{"disconnected", "protecting"}, // temporarily offline — picks up the remove + acks on reconnect
		{"revoked", "unprotected"},     // Finding 7 — no shield to ack, resolve straight through
	}

	for i, c := range cases {
		resID := mkCase(c.shieldStatus, 8080+i)
		row, err := MarkUnprotecting(ctx, db, tenantID, resID)
		if err != nil {
			t.Fatalf("MarkUnprotecting(shield=%s): %v", c.shieldStatus, err)
		}
		if row.Status != c.wantStatus {
			t.Errorf("shield=%s: status = %q, want %q", c.shieldStatus, row.Status, c.wantStatus)
		}
		// pending_action is 'remove' in both branches (it is only meaningful while
		// status='protecting'; the normal terminal path leaves it 'remove' too).
		if row.PendingAction != "remove" {
			t.Errorf("shield=%s: pending_action = %q, want \"remove\"", c.shieldStatus, row.PendingAction)
		}
	}
}
