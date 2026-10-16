package policy

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// Certificate audit bug 3: a resource protected through a revoked shield must
// become unreachable — no ACL entry, so connectors default-deny and tear down
// live sessions — until an administrator unprotects it or assigns another
// shield. It must never fall back to direct connector routing. A shield row
// that is gone (deleted after revocation, or cascaded away) counts as revoked.
func TestCompileACLSnapshot_RevokedShieldMakesItsResourcesUnreachable(t *testing.T) {
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
	notifier := NewNotifier(NewSnapshotCache())

	type fixture struct {
		wsID, shieldID, protectedID, directID string
	}
	setup := func(t *testing.T, slug string) fixture {
		wsID := mustInsertWorkspace(t, ctx, pool, slug)
		grpID := mustInsertGroup(t, ctx, pool, wsID, "grp-"+slug)
		userID := mustInsertUser(t, ctx, pool, wsID)
		mustAddGroupMember(t, ctx, pool, grpID, userID)
		_ = mustInsertClientDevice(t, ctx, pool, wsID, userID, "spiffe://td/client/dev-"+slug)
		rnID, connID := mustInsertRNWithConnector(t, ctx, pool, wsID, "rn-"+slug, "td-"+slug, "10.20.0.1")
		shieldID := mustInsertShield(t, ctx, pool, wsID, rnID, connID)
		protectedID := mustInsertResource(t, ctx, pool, wsID, rnID, "git-"+slug, "10.20.0.10", 22)
		mustProtectResource(t, ctx, pool, protectedID, shieldID)
		directID := mustInsertResource(t, ctx, pool, wsID, rnID, "wiki-"+slug, "10.20.0.11", 80)
		mustAssignResourceToGroup(t, ctx, pool, wsID, protectedID, grpID)
		mustAssignResourceToGroup(t, ctx, pool, wsID, directID, grpID)
		return fixture{wsID, shieldID, protectedID, directID}
	}
	compile := func(t *testing.T, wsID string) map[string]string {
		t.Helper()
		compiled, err := CompileACLSnapshot(ctx, NewStore(pool), posture.NewStore(pool), notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		routes := map[string]string{}
		for _, e := range compiled.Snapshot.Entries {
			routes[e.ResourceId] = e.RouteType + "|" + e.ShieldId
		}
		return routes
	}

	t.Run("live shield routes through the shield", func(t *testing.T) {
		f := setup(t, "live")
		routes := compile(t, f.wsID)
		if routes[f.protectedID] != "shield|"+f.shieldID {
			t.Fatalf("protected resource: want shield route via %s, got %q", f.shieldID, routes[f.protectedID])
		}
		if routes[f.directID] != "connector|" {
			t.Fatalf("unprotected resource: want connector route, got %q", routes[f.directID])
		}
	})

	for i, tc := range []struct {
		name   string
		revoke func(t *testing.T, f fixture)
	}{
		{"revoked shield", func(t *testing.T, f fixture) {
			mustExec(t, ctx, pool, `UPDATE shields SET status = 'revoked' WHERE id = $1`, f.shieldID)
			mustExec(t, ctx, pool, `INSERT INTO shield_revocations (shield_id, tenant_id) VALUES ($1, $2)`, f.shieldID, f.wsID)
		}},
		{"tombstone alone is authoritative", func(t *testing.T, f fixture) {
			mustExec(t, ctx, pool, `INSERT INTO shield_revocations (shield_id, tenant_id) VALUES ($1, $2)`, f.shieldID, f.wsID)
		}},
		{"revoked shield row deleted", func(t *testing.T, f fixture) {
			mustExec(t, ctx, pool, `UPDATE shields SET status = 'revoked' WHERE id = $1`, f.shieldID)
			mustExec(t, ctx, pool, `INSERT INTO shield_revocations (shield_id, tenant_id) VALUES ($1, $2)`, f.shieldID, f.wsID)
			mustExec(t, ctx, pool, `DELETE FROM shields WHERE id = $1`, f.shieldID)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t, fmt.Sprintf("revoked-%d", i))
			tc.revoke(t, f)
			routes := compile(t, f.wsID)
			if route, ok := routes[f.protectedID]; ok {
				t.Fatalf("resource behind a revoked shield must be unreachable, got route %q", route)
			}
			if routes[f.directID] != "connector|" {
				t.Fatalf("an unrelated resource must stay reachable, got %q", routes[f.directID])
			}
		})
	}
}

func mustInsertShield(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, rnID, connID string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO shields (tenant_id, remote_network_id, connector_id, name, status, trust_domain)
		 VALUES ($1, $2, $3, 'shield', 'active', 'td') RETURNING id::text`,
		tenantID, rnID, connID,
	).Scan(&id); err != nil {
		t.Fatalf("insert shield: %v", err)
	}
	return id
}

func mustProtectResource(t *testing.T, ctx context.Context, pool *pgxpool.Pool, resourceID, shieldID string) {
	t.Helper()
	mustExec(t, ctx, pool,
		`UPDATE resources SET status = 'protected', shield_id = $2 WHERE id = $1`, resourceID, shieldID)
}

func mustExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("exec %q: %v", sql, err)
	}
}
