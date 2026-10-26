package resource

import (
	"context"
	"fmt"
	"testing"
	"time"
)

// Sprint 20 Fix 02 (F35): RecordAck reports whether an ack changed the ACL
// compiler's output, i.e. reaped a tombstone or moved the resource between the
// connector-routed (pending, unprotected) and shield-routed statuses. Real
// Postgres (throwaway database); set RESOURCE_TEST_DATABASE_URL.
func TestRecordAckReportsACLChanges(t *testing.T) {
	db := newResourceTestDB(t)
	ctx := context.Background()
	slug := fmt.Sprintf("f35-test-%d", time.Now().UnixNano())
	var tenantID, networkID, connectorID, shieldID string
	if err := db.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`,
		slug, "ws-"+slug+".zecurity.in",
	).Scan(&tenantID); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	t.Cleanup(func() { _, _ = db.Exec(context.Background(), `DELETE FROM workspaces WHERE id = $1`, tenantID) })
	if err := db.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, 'net', 'other') RETURNING id`, tenantID,
	).Scan(&networkID); err != nil {
		t.Fatalf("insert remote_network: %v", err)
	}
	if err := db.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status) VALUES ($1, $2, 'conn', 'active') RETURNING id`,
		tenantID, networkID,
	).Scan(&connectorID); err != nil {
		t.Fatalf("insert connector: %v", err)
	}
	if err := db.QueryRow(ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status) VALUES ($1, $2, $3, 'sh', 'active') RETURNING id`,
		tenantID, networkID, connectorID,
	).Scan(&shieldID); err != nil {
		t.Fatalf("insert shield: %v", err)
	}
	port := 9300
	mk := func(status, action string) string {
		t.Helper()
		port++
		var id string
		if err := db.QueryRow(ctx,
			`INSERT INTO resources (tenant_id, remote_network_id, shield_id, name, host, protocol, port_from, port_to, status, pending_action)
			 VALUES ($1, $2, $3, $4, '10.0.0.1', 'tcp', $5, $5, $6, $7) RETURNING id`,
			tenantID, networkID, shieldID, fmt.Sprintf("r-%d", port), port, status, action,
		).Scan(&id); err != nil {
			t.Fatalf("insert resource(%s/%s): %v", status, action, err)
		}
		return id
	}
	statusOf := func(id string) string {
		var s string
		if err := db.QueryRow(ctx, `SELECT status FROM resources WHERE id = $1`, id).Scan(&s); err != nil {
			return "(gone)"
		}
		return s
	}

	cases := []struct {
		name               string
		status, action     string
		ack                string
		wantChanged        bool
		wantStatusAfterAck string
	}{
		{"unprotect completes: shield → connector route", "protecting", "remove", "unprotected", true, "unprotected"},
		{"protect completes: same (shield) route", "protecting", "apply", "protected", false, "protected"},
		{"protect fails: same (shield) route", "protecting", "apply", "failed", false, "failed"},
		{"protected re-verified as failed: same route", "protected", "apply", "failed", false, "failed"},
		{"stale unprotected ack during an apply: guarded out", "protecting", "apply", "unprotected", false, "protecting"},
		{"tombstone reaped", "deleting", "remove", "unprotected", true, "(gone)"},
	}
	for _, c := range cases {
		id := mk(c.status, c.action)
		changed, err := RecordAck(ctx, db, tenantID, id, c.ack, "", time.Now().Unix())
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if changed != c.wantChanged {
			t.Errorf("%s: aclChanged = %v, want %v", c.name, changed, c.wantChanged)
		}
		if got := statusOf(id); got != c.wantStatusAfterAck {
			t.Errorf("%s: status = %s, want %s", c.name, got, c.wantStatusAfterAck)
		}
	}
}
