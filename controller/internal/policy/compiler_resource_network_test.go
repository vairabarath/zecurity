package policy

import (
	"context"
	"os"
	"testing"

	"github.com/yourorg/ztna/controller/internal/posture"
)

// A legacy resource whose remote network belongs to another workspace (the
// old createResource/updateResource accepted any network ID) must not pull
// that network or its connectors into the owning workspace's snapshot.
func TestCompileACLSnapshot_ResourceOnForeignNetwork(t *testing.T) {
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

	wsA := mustInsertWorkspace(t, ctx, pool, "rnown-a")
	wsB := mustInsertWorkspace(t, ctx, pool, "rnown-b")
	rnA, connA := mustInsertRNWithConnector(t, ctx, pool, wsA, "rn-a", "td-a", "10.80.0.1")
	rnB, connB := mustInsertRNWithConnector(t, ctx, pool, wsB, "rn-b", "td-b", "10.81.0.1")

	grp := mustInsertGroup(t, ctx, pool, wsA, "grp-a")
	user := mustInsertUser(t, ctx, pool, wsA)
	mustAddGroupMember(t, ctx, pool, grp, user)
	mustInsertClientDevice(t, ctx, pool, wsA, user, "spiffe://td-a/client/a")
	// Positive control: A's resource on A's own network, granted to the same
	// group (so the same device). It must compile with A's connector.
	valid := mustInsertResource(t, ctx, pool, wsA, rnA, "valid", "10.80.1.1", 22)
	mustAssignResourceToGroup(t, ctx, pool, wsA, valid, grp)
	// Negative control: A's resource, but on B's network (planted the way the
	// old code wrote it), granted to the same group. Device selection is the
	// same for both, so only the network check can tell them apart.
	legacy := mustInsertResource(t, ctx, pool, wsA, rnB, "legacy", "10.81.1.1", 22)
	mustAssignResourceToGroup(t, ctx, pool, wsA, legacy, grp)

	c, err := CompileACLSnapshot(ctx, NewStore(pool), posture.NewStore(pool), NewNotifier(NewSnapshotCache()), wsA)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	validFound := false
	for _, e := range c.Snapshot.Entries {
		switch e.ResourceId {
		case legacy:
			t.Errorf("A's snapshot contains the resource routed through B's network %s", e.RemoteNetworkId)
		case valid:
			validFound = true
			if e.RemoteNetworkId != rnA || len(e.AllowedSpiffeIds) != 1 || e.AllowedSpiffeIds[0] != "spiffe://td-a/client/a" {
				t.Errorf("valid entry = network %s allowed %v, want A's network and A's device", e.RemoteNetworkId, e.AllowedSpiffeIds)
			}
		}
	}
	if !validFound {
		t.Errorf("positive control missing: A's valid resource is not in the snapshot")
	}
	connAFound := false
	for _, rn := range c.Snapshot.RemoteNetworks {
		if rn.RemoteNetworkId == rnB {
			t.Errorf("A's snapshot contains B's remote network")
		}
		for _, cn := range rn.Connectors {
			switch cn.ConnectorId {
			case connB:
				t.Errorf("A's snapshot contains B's connector (%s)", cn.ConnectorTunnelAddr)
			case connA:
				connAFound = true
			}
		}
	}
	if !connAFound {
		t.Errorf("positive control missing: A's connector is not in the snapshot")
	}
}
