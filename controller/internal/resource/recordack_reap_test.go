package resource

import (
	"context"
	"testing"
)

// TestRecordAck_ReapReturnsReaped locks the RecordAck signature change: when an
// 'unprotected' ack reaps a 'deleting' tombstone (physical DELETE), it must
// report reaped=true so the caller can fire NotifyPolicyChange. A second ack for
// the now-absent row must report reaped=false. Runs in its own throwaway database
// with a seeded shield (Fix 02); set RESOURCE_TEST_DATABASE_URL.
func TestRecordAck_ReapReturnsReaped(t *testing.T) {
	db := newResourceTestDB(t)
	ctx := context.Background()
	tenantID, networkID, shieldID := seedShield(t, db)

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

	// Fix 18: only the resource's own shield may reap it.
	if reaped, err := RecordAck(ctx, db, tenantID, Ack{ResourceID: resID, Status: "unprotected", ShieldID: "00000000-0000-0000-0000-000000000001"}); err != nil || reaped {
		t.Fatalf("ack from another shield: reaped=%v err=%v, want ignored", reaped, err)
	}
	reaped, err := RecordAck(ctx, db, tenantID, Ack{ResourceID: resID, Status: "unprotected", ShieldID: shieldID})
	if err != nil {
		t.Fatalf("RecordAck: %v", err)
	}
	if !reaped {
		t.Fatal("expected reaped=true when an 'unprotected' ack reaps a tombstone")
	}

	reaped2, err := RecordAck(ctx, db, tenantID, Ack{ResourceID: resID, Status: "unprotected", ShieldID: shieldID})
	if err != nil {
		t.Fatalf("RecordAck (second): %v", err)
	}
	if reaped2 {
		t.Fatal("expected reaped=false; the row was already reaped")
	}
}
