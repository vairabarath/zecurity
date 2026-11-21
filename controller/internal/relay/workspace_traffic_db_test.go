package relay

import (
	"context"
	"testing"
)

// Sprint 24 R1 (D-54): the trust domain → workspace lookup returns active
// workspaces only (a suspended tenant keeps status 'active', D-45); unknown
// and inactive trust domains are absent, so the heartbeat ignores them.
func TestActiveWorkspacesByTrustDomain(t *testing.T) {
	e := newAdminEnv(t, false)
	ctx := context.Background()
	store := NewStore(e.pool)

	active := e.workspace(t, "r1-active")
	suspended := e.workspace(t, "r1-suspended")
	if _, err := e.pool.Exec(ctx, `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 'op', suspension_reason = 'r' WHERE id = $1`, suspended); err != nil {
		t.Fatal(err)
	}
	inactive := e.workspace(t, "r1-deleted")
	if _, err := e.pool.Exec(ctx, `UPDATE workspaces SET status = 'deleted' WHERE id = $1`, inactive); err != nil {
		t.Fatal(err)
	}

	got, err := store.ActiveWorkspacesByTrustDomain(ctx, []string{
		"r1-active.zecurity.test", "r1-suspended.zecurity.test", "r1-deleted.zecurity.test", "nobody.zecurity.test",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"r1-active.zecurity.test": active, "r1-suspended.zecurity.test": suspended}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for td, id := range want {
		if got[td] != id {
			t.Fatalf("%s → %q, want %q (all: %v)", td, got[td], id, got)
		}
	}

	if empty, err := store.ActiveWorkspacesByTrustDomain(ctx, nil); err != nil || len(empty) != 0 {
		t.Fatalf("no trust domains → %v, %v", empty, err)
	}
}
