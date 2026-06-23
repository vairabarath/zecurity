package resource

import (
	"context"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// TestRecordAck_ReapReturnsReaped locks the RecordAck signature change: when an
// 'unprotected' ack reaps a 'deleting' tombstone (physical DELETE), it must
// report reaped=true so the caller can fire NotifyPolicyChange. A second ack for
// the now-absent row must report reaped=false. Env-gated like the other resource
// integration tests.
func TestRecordAck_ReapReturnsReaped(t *testing.T) {
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
	t.Cleanup(db.Close)

	var tenantID, networkID string
	if err := db.QueryRow(ctx,
		`SELECT tenant_id, remote_network_id FROM shields WHERE id = $1`, shieldID,
	).Scan(&tenantID, &networkID); err != nil {
		t.Fatalf("load shield: %v", err)
	}

	var resID string
	if err := db.QueryRow(ctx,
		`INSERT INTO resources (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
		 VALUES ($1,$2,$3,'reap-test','10.0.0.97','tcp',8080,8080,'deleting','remove')
		 RETURNING id`,
		tenantID, networkID, shieldID,
	).Scan(&resID); err != nil {
		t.Fatalf("seed tombstone: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM resources WHERE id = $1`, resID) })

	reaped, err := RecordAck(ctx, db, tenantID, resID, "unprotected", "", 0)
	if err != nil {
		t.Fatalf("RecordAck: %v", err)
	}
	if !reaped {
		t.Fatal("expected reaped=true when an 'unprotected' ack reaps a tombstone")
	}

	reaped2, err := RecordAck(ctx, db, tenantID, resID, "unprotected", "", 0)
	if err != nil {
		t.Fatalf("RecordAck (second): %v", err)
	}
	if reaped2 {
		t.Fatal("expected reaped=false; the row was already reaped")
	}
}
