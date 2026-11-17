package policy

import (
	"context"
	"os"
	"testing"

	"github.com/yourorg/ztna/controller/internal/posture"
)

// Sprint 23 S1 (D-45, D-08 preserved): a suspended tenant's snapshot is
// deny-all (no entries, no remote networks), another tenant is unaffected,
// and resume compiles the normal snapshot again.
func TestCompileACLSnapshot_SuspendedTenantDenyAll(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueTestDBName(t)
	adminPool := mustConnectTestPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()
	testDSN, err := withTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database dsn: %v", err)
	}
	pool := mustConnectTestPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	type ws struct{ id, res, dev string }
	seed := func(slug, lan, host string) ws {
		w := ws{id: mustInsertWorkspace(t, ctx, pool, slug)}
		rn, _ := mustInsertRNWithConnector(t, ctx, pool, w.id, "rn-"+slug, "td-"+slug, lan)
		w.res = mustInsertResource(t, ctx, pool, w.id, rn, "res-"+slug, host, 5432)
		grp := mustInsertGroup(t, ctx, pool, w.id, "grp-"+slug)
		user := mustInsertUser(t, ctx, pool, w.id)
		mustAddGroupMember(t, ctx, pool, grp, user)
		w.dev = "spiffe://td-" + slug + "/client/" + slug
		mustInsertClientDevice(t, ctx, pool, w.id, user, w.dev)
		mustAssignResourceToGroup(t, ctx, pool, w.id, w.res, grp)
		return w
	}
	a := seed("sus-a", "10.60.0.1", "10.60.1.1")
	b := seed("sus-b", "10.70.0.1", "10.70.1.1")

	notifier := NewNotifier(NewSnapshotCache())
	compile := func(wsID string) (entries, networks int) {
		t.Helper()
		c, err := CompileACLSnapshot(ctx, NewStore(pool), posture.NewStore(pool), notifier, wsID)
		if err != nil {
			t.Fatalf("compile %s: %v", wsID, err)
		}
		if c.Snapshot.WorkspaceId != wsID {
			t.Fatalf("snapshot workspace %q, want %q", c.Snapshot.WorkspaceId, wsID)
		}
		return len(c.Snapshot.Entries), len(c.Snapshot.RemoteNetworks)
	}
	if e, n := compile(a.id); e != 1 || n != 1 {
		t.Fatalf("active A: %d entries %d networks, want 1 and 1", e, n)
	}

	mustExec(t, ctx, pool, `UPDATE workspaces SET suspended_at = NOW(), suspended_by = 'op@x', suspension_reason = 'test' WHERE id = $1`, a.id)
	if e, n := compile(a.id); e != 0 || n != 0 {
		t.Fatalf("suspended A: %d entries %d networks, want deny-all (0 and 0)", e, n)
	}
	if e, n := compile(b.id); e != 1 || n != 1 {
		t.Fatalf("active B while A is suspended: %d entries %d networks, want 1 and 1", e, n)
	}

	mustExec(t, ctx, pool, `UPDATE workspaces SET suspended_at = NULL, suspended_by = NULL, suspension_reason = NULL WHERE id = $1`, a.id)
	if e, n := compile(a.id); e != 1 || n != 1 {
		t.Fatalf("resumed A: %d entries %d networks, want the normal snapshot back", e, n)
	}

	// Fail closed: a lookup error returns no snapshot (callers default-deny).
	closed := mustConnectTestPool(t, ctx, testDSN)
	closed.Close()
	if c, err := CompileACLSnapshot(ctx, NewStore(closed), posture.NewStore(closed), notifier, a.id); err == nil || c != nil {
		t.Fatalf("compile with a failing lookup: snapshot %v err %v, want an error and no snapshot", c, err)
	}
}
