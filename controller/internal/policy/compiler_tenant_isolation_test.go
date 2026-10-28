package policy

import (
	"context"
	"os"
	"testing"

	"github.com/yourorg/ztna/controller/internal/posture"
)

// B-1 (cross-tenant access rules): the compiler must never combine one
// workspace's rules with another workspace's resources, groups or connectors,
// even if inconsistent rows already exist (planted here directly in SQL, the
// way the unscoped mutations could write them). The schema can't prevent such
// rows (single-column foreign keys), so the compiler query is the last line.
func TestCompileACLSnapshot_TenantIsolation(t *testing.T) {
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

	type ws struct{ id, rn, conn, res, grp, dev string }
	seed := func(slug, lan, host string) ws {
		w := ws{id: mustInsertWorkspace(t, ctx, pool, slug)}
		w.rn, w.conn = mustInsertRNWithConnector(t, ctx, pool, w.id, "rn-"+slug, "td-"+slug, lan)
		w.res = mustInsertResource(t, ctx, pool, w.id, w.rn, "res-"+slug, host, 5432)
		w.grp = mustInsertGroup(t, ctx, pool, w.id, "grp-"+slug)
		user := mustInsertUser(t, ctx, pool, w.id)
		mustAddGroupMember(t, ctx, pool, w.grp, user)
		w.dev = "spiffe://td-" + slug + "/client/" + slug
		mustInsertClientDevice(t, ctx, pool, w.id, user, w.dev)
		return w
	}
	a := seed("iso-a", "10.40.0.1", "10.40.1.1")
	b := seed("iso-b", "10.50.0.1", "10.50.1.1")

	// Legitimate rules in each workspace.
	mustAssignResourceToGroup(t, ctx, pool, a.id, a.res, a.grp)
	mustAssignResourceToGroup(t, ctx, pool, b.id, b.res, b.grp)
	// Planted inconsistent rows owned by A: B's resource with A's group, and
	// A's resource with B's group. The schema refuses them (FU-2), so drop
	// that check here: the compiler must ignore them on its own.
	allowCrossWorkspaceRows(t, ctx, pool)
	mustExec(t, ctx, pool, `INSERT INTO access_rules (workspace_id, resource_id, group_id) VALUES ($1, $2, $3)`, a.id, b.res, a.grp)
	mustExec(t, ctx, pool, `INSERT INTO access_rules (workspace_id, resource_id, group_id) VALUES ($1, $2, $3)`, a.id, a.res, b.grp)

	compile := func(wsID string) (entries map[string][]string, connectors map[string]bool) {
		t.Helper()
		c, err := CompileACLSnapshot(ctx, NewStore(pool), posture.NewStore(pool), NewNotifier(NewSnapshotCache()), wsID)
		if err != nil {
			t.Fatalf("compile %s: %v", wsID, err)
		}
		entries, connectors = map[string][]string{}, map[string]bool{}
		for _, e := range c.Snapshot.Entries {
			entries[e.ResourceId] = append(entries[e.ResourceId], e.AllowedSpiffeIds...)
		}
		for _, rn := range c.Snapshot.RemoteNetworks {
			for _, cn := range rn.Connectors {
				connectors[cn.ConnectorId] = true
			}
		}
		return entries, connectors
	}

	aEntries, aConns := compile(a.id)
	if _, ok := aEntries[b.res]; ok {
		t.Errorf("A's snapshot contains B's resource %s (planted rule crossed tenants)", b.res)
	}
	if aConns[b.conn] {
		t.Errorf("A's snapshot contains B's connector %s", b.conn)
	}
	if got := aEntries[a.res]; len(got) != 1 || got[0] != a.dev {
		t.Errorf("A's own entry = %v, want exactly [%s] (legitimate rule must still compile; planted B-group rule must add nothing)", got, a.dev)
	}
	if !aConns[a.conn] {
		t.Errorf("A's snapshot lost its own connector")
	}

	bEntries, bConns := compile(b.id)
	if got := bEntries[b.res]; len(got) != 1 || got[0] != b.dev {
		t.Errorf("B's own entry = %v, want exactly [%s]", got, b.dev)
	}
	if _, ok := bEntries[a.res]; ok || bConns[a.conn] {
		t.Errorf("B's snapshot contains A's resource or connector")
	}

	// Connector lookup is scoped by workspace even when handed a foreign
	// remote network id directly.
	rows, err := NewStore(pool).GetConnectorsForRemoteNetworks(ctx, a.id, []string{a.rn, b.rn})
	if err != nil {
		t.Fatalf("GetConnectorsForRemoteNetworks: %v", err)
	}
	for _, r := range rows {
		if r.ConnectorID == b.conn {
			t.Errorf("connector lookup for A returned B's connector")
		}
	}
	if len(rows) != 1 || rows[0].ConnectorID != a.conn {
		t.Errorf("connector lookup for A = %d rows, want only A's connector", len(rows))
	}

	// A protected resource pointing at a foreign shield must not route to the
	// foreign shield's connector: it fails closed (treated as unavailable).
	shieldB := mustInsertShield(t, ctx, pool, b.id, b.rn, b.conn)
	mustProtectResource(t, ctx, pool, a.res, shieldB)
	aEntries, _ = compile(a.id)
	if _, ok := aEntries[a.res]; ok {
		t.Errorf("A's resource bound to B's shield still compiled (would prefer B's connector)")
	}
}
