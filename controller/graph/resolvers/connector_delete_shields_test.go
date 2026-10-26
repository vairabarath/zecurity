package resolvers

import (
	"strings"
	"testing"
)

// Connector-revocation Phase F (finding A): deleting a connector cascades to
// the shields rows attached to it without revoking them. The delete is
// refused while an enrolled, non-revoked shield is attached.

func (f *certHistoryFixture) shield(t *testing.T, name, connectorID, status string) string {
	t.Helper()
	return f.scalar(t, `INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status)
		VALUES ($1, $2, $3, $4, $5) RETURNING id::text`, f.ws, f.rn, connectorID, name, status)
}

func (f *certHistoryFixture) count(t *testing.T, sql string, args ...any) string {
	t.Helper()
	return f.scalar(t, "SELECT count(*)::text FROM "+sql, args...)
}

// AF-1: an active or disconnected shield blocks the delete; the error names
// the count and both rows stay.
func TestDeleteConnector_RefusedWhileShieldsAttached(t *testing.T) {
	f := newCertHistoryFixture(t)
	id := f.connector(t, "with-shields", []string{"d1d1d1d1"}, valid)
	// A peer connector in the same remote network, so the revoke itself is
	// allowed (Fix 16 F46 refuses revoking the last one with shields attached).
	f.connector(t, "peer", nil, valid)
	f.shield(t, "sh-active", id, "active")
	f.shield(t, "sh-disconnected", id, "disconnected")
	if _, err := f.mr.RevokeConnector(f.ctx, id, nil); err != nil {
		t.Fatalf("RevokeConnector: %v", err)
	}

	ok, err := f.mr.DeleteConnector(f.ctx, id)
	if ok || err == nil || !strings.Contains(err.Error(), "2 shield(s) still attached") {
		t.Fatalf("DeleteConnector = %v, %v; want refusal naming 2 shields", ok, err)
	}
	if n := f.count(t, `connectors WHERE id = $1`, id); n != "1" {
		t.Fatalf("connector row deleted despite attached shields")
	}
	if n := f.count(t, `shields WHERE connector_id = $1`, id); n != "2" {
		t.Fatalf("shields rows = %s, want 2", n)
	}
}

// AF-2: once the shields are revoked (tombstoned) or moved to another
// connector, the delete succeeds; pending shields (no certificate) never
// block it, and a revoked shield's tombstone survives the cascade.
func TestDeleteConnector_AllowedOnceShieldsRevokedOrMoved(t *testing.T) {
	f := newCertHistoryFixture(t)
	id := f.connector(t, "old", []string{"e1e1e1e1"}, valid)
	other := f.connector(t, "new", []string{"e2e2e2e2"}, valid)
	revoked := f.shield(t, "sh-revoked", id, "active")
	moved := f.shield(t, "sh-moved", id, "active")
	f.shield(t, "sh-pending", id, "pending")
	if _, err := f.mr.RevokeConnector(f.ctx, id, nil); err != nil {
		t.Fatalf("RevokeConnector: %v", err)
	}
	if _, err := f.mr.DeleteConnector(f.ctx, id); err == nil {
		t.Fatal("DeleteConnector succeeded with two enrolled shields attached")
	}

	f.exec(t, `UPDATE shields SET status = 'revoked' WHERE id = $1`, revoked)
	f.exec(t, `INSERT INTO shield_revocations (shield_id, tenant_id) VALUES ($1, $2)`, revoked, f.ws)
	f.exec(t, `UPDATE shields SET connector_id = $1 WHERE id = $2`, other, moved)

	if ok, err := f.mr.DeleteConnector(f.ctx, id); err != nil || !ok {
		t.Fatalf("DeleteConnector: %v", err)
	}
	if n := f.count(t, `connectors WHERE id = $1`, id); n != "0" {
		t.Fatal("connector row not deleted")
	}
	if n := f.count(t, `shields WHERE id = $1 AND connector_id = $2`, moved, other); n != "1" {
		t.Fatal("the moved shield must survive")
	}
	if n := f.count(t, `shield_revocations WHERE shield_id = $1`, revoked); n != "1" {
		t.Fatal("the revoked shield's tombstone must survive the cascade")
	}

	pending := f.connector(t, "never-enrolled", nil, valid)
	f.exec(t, `UPDATE connectors SET status = 'pending' WHERE id = $1`, pending)
	f.shield(t, "sh-pending-only", pending, "pending")
	if ok, err := f.mr.DeleteConnector(f.ctx, pending); err != nil || !ok {
		t.Fatalf("DeleteConnector(pending): %v", err)
	}
}
