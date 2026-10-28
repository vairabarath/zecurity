package policy

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// allowCrossWorkspaceRows drops the 045 workspace-consistency constraints
// (Sprint 22 FU-2) on a throwaway database, so a test can plant the
// inconsistent rows that the application and the compiler must still ignore
// on their own.
func allowCrossWorkspaceRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, ctx, pool, `ALTER TABLE resources DROP CONSTRAINT resources_remote_network_same_workspace_fkey`)
	mustExec(t, ctx, pool, `ALTER TABLE access_rules
		DROP CONSTRAINT access_rules_resource_same_workspace_fkey,
		DROP CONSTRAINT access_rules_group_same_workspace_fkey`)
	mustExec(t, ctx, pool, `ALTER TABLE group_members DROP CONSTRAINT group_members_user_same_workspace_fkey`)
}

// FU-2: the schema itself refuses rows that tie together objects of different
// workspaces, and still accepts (and cascades) consistent ones.
func TestWorkspaceConsistency_SchemaRefusesCrossWorkspaceRows(t *testing.T) {
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

	type ws struct{ id, rn, grp, user, res string }
	seed := func(slug, ip string) ws {
		w := ws{id: mustInsertWorkspace(t, ctx, pool, slug)}
		w.rn, _ = mustInsertRNWithConnector(t, ctx, pool, w.id, "rn-"+slug, "td-"+slug, ip)
		w.grp = mustInsertGroup(t, ctx, pool, w.id, "grp-"+slug)
		w.user = mustInsertUser(t, ctx, pool, w.id)
		w.res = mustInsertResource(t, ctx, pool, w.id, w.rn, "res-"+slug, ip, 22)
		return w
	}
	a := seed("cons-a", "10.90.0.1")
	b := seed("cons-b", "10.91.0.1")

	// Consistent rows are accepted; the member's workspace comes from its group.
	mustAssignResourceToGroup(t, ctx, pool, a.id, a.res, a.grp)
	mustAddGroupMember(t, ctx, pool, a.grp, a.user)
	var memberWS string
	if err := pool.QueryRow(ctx, `SELECT workspace_id::text FROM group_members WHERE group_id = $1 AND user_id = $2`, a.grp, a.user).Scan(&memberWS); err != nil {
		t.Fatal(err)
	}
	if memberWS != a.id {
		t.Fatalf("member workspace_id = %s, want the group's %s", memberWS, a.id)
	}

	for _, tc := range []struct {
		name, constraint, sql string
		args                  []any
	}{
		{"resource on another workspace's network", "resources_remote_network_same_workspace_fkey",
			`INSERT INTO resources (tenant_id, remote_network_id, name, host, port_from, protocol, status)
			 VALUES ($1, $2, 'x', '10.90.9.9', 22, 'tcp', 'unprotected')`, []any{a.id, b.rn}},
		{"resource moved onto another workspace's network", "resources_remote_network_same_workspace_fkey",
			`UPDATE resources SET remote_network_id = $1 WHERE id = $2`, []any{b.rn, a.res}},
		{"rule for another workspace's resource", "access_rules_resource_same_workspace_fkey",
			`INSERT INTO access_rules (workspace_id, resource_id, group_id) VALUES ($1, $2, $3)`, []any{a.id, b.res, a.grp}},
		{"rule for another workspace's group", "access_rules_group_same_workspace_fkey",
			`INSERT INTO access_rules (workspace_id, resource_id, group_id) VALUES ($1, $2, $3)`, []any{a.id, a.res, b.grp}},
		{"member from another workspace", "group_members_user_same_workspace_fkey",
			`INSERT INTO group_members (group_id, user_id) VALUES ($1, $2)`, []any{a.grp, b.user}},
		{"member whose writer claims the user's workspace", "group_members_user_same_workspace_fkey",
			`INSERT INTO group_members (group_id, user_id, workspace_id) VALUES ($1, $2, $3)`, []any{a.grp, b.user, b.id}},
		{"member moved into another workspace's group", "group_members_user_same_workspace_fkey",
			`UPDATE group_members SET group_id = $1 WHERE group_id = $2`, []any{b.grp, a.grp}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, tc.args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23503" || pgErr.ConstraintName != tc.constraint {
				t.Fatalf("got %v, want foreign key violation of %s", err, tc.constraint)
			}
		})
	}

	// Deletes still cascade through the composite keys.
	mustExec(t, ctx, pool, `DELETE FROM users WHERE id = $1`, a.user)
	mustExec(t, ctx, pool, `DELETE FROM remote_networks WHERE id = $1`, a.rn)
	var members, resources, rules int
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM group_members WHERE workspace_id = $1`, a.id).Scan(&members)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM resources WHERE tenant_id = $1`, a.id).Scan(&resources)
	_ = pool.QueryRow(ctx, `SELECT count(*) FROM access_rules WHERE workspace_id = $1`, a.id).Scan(&rules)
	if members != 0 || resources != 0 || rules != 0 {
		t.Fatalf("after deletes: %d members, %d resources, %d rules; want all cascaded", members, resources, rules)
	}
}
