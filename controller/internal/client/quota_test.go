package client

import (
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/auth"
	"github.com/yourorg/ztna/controller/internal/quota"
)

// Sprint 23 S3 (D-47): client sign-in JIT-creates a user and device
// enrollment creates a client device; both go through the quota, in the
// creating transaction.

func TestClientQuota_DevicesAndJITUsers(t *testing.T) {
	pool, ctx, wsID, userID, _ := deviceGateTestHarness(t)
	setLimit := func(key string, n int) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by)
			VALUES ($1, $2, $3, 'test', 'op') ON CONFLICT (workspace_id, key) DO UPDATE SET limit_value = EXCLUDED.limit_value`, wsID, key, n); err != nil {
			t.Fatal(err)
		}
	}
	devices := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM client_devices WHERE workspace_id = $1`, wsID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	sess := deviceSession{id: "sid", deadlines: auth.SessionDeadlines{IdleUntil: time.Now().Add(time.Hour)}}

	setLimit(quota.KeyClientDevices, 1)
	if _, err := insertClientDevice(ctx, pool, userID, wsID, "d1", "linux", sess); err != nil {
		t.Fatalf("first device: %v", err)
	}
	_, err := insertClientDevice(ctx, pool, userID, wsID, "d2", "linux", sess)
	if qe, ok := quota.AsExceeded(err); !ok || qe.Key != quota.KeyClientDevices || qe.Limit != 1 || qe.Used != 1 {
		t.Fatalf("second device: %v, want quota_exceeded {client_devices, 1, 1}", err)
	}
	if n := devices(); n != 1 {
		t.Fatalf("devices = %d, want 1 (nothing created when refused)", n)
	}
	// A revoked device frees its slot.
	if _, err := pool.Exec(ctx, `UPDATE client_devices SET revoked_at = NOW() WHERE workspace_id = $1`, wsID); err != nil {
		t.Fatal(err)
	}
	if _, err := insertClientDevice(ctx, pool, userID, wsID, "d3", "linux", sess); err != nil {
		t.Fatalf("after revoking: %v", err)
	}

	// JIT user creation at the user limit (the harness user is the one user).
	var connID string
	if err := pool.QueryRow(ctx, `INSERT INTO identity_connections (tenant_id, provider, display_name, issuer)
		VALUES ($1, 'oidc', 'idp', 'https://idp.test') RETURNING id::text`, wsID).Scan(&connID); err != nil {
		t.Fatal(err)
	}
	setLimit(quota.KeyUsers, 1)
	_, _, _, err = upsertUser(ctx, pool, wsID, "new@example.test", "oidc", "sub-new", connID, "https://idp.test", true)
	if qe, ok := quota.AsExceeded(err); !ok || qe.Key != quota.KeyUsers {
		t.Fatalf("JIT user over the limit: %v, want quota_exceeded users", err)
	}
	var users int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE tenant_id = $1`, wsID).Scan(&users); err != nil || users != 1 {
		t.Fatalf("users = %d (err %v), want 1", users, err)
	}
	// An invited identity (pending invitation) takes no new slot: it can join
	// even at the limit, because the invitation already counted it.
	if _, err := pool.Exec(ctx, `INSERT INTO invitations (email, workspace_id, invited_by, token) VALUES ('invited@example.test', $1, $2, 'tok-q')`, wsID, userID); err != nil {
		t.Fatal(err)
	}
	setLimit(quota.KeyUsers, 2)
	if _, _, created, err := upsertUser(ctx, pool, wsID, "Invited@example.test", "oidc", "sub-inv", connID, "https://idp.test", true); err != nil || !created {
		t.Fatalf("invited join: created=%v err=%v", created, err)
	}
}
