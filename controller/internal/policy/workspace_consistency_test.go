package policy

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// allowCrossWorkspaceRows drops the workspace-consistency constraints that
// the planted-row tests need (045 / FU-2, 046 / FU-5) on a throwaway database, so a test can plant the
// inconsistent rows that the application and the compiler must still ignore
// on their own.
func allowCrossWorkspaceRows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	mustExec(t, ctx, pool, `ALTER TABLE resources
		DROP CONSTRAINT resources_remote_network_same_workspace_fkey,
		DROP CONSTRAINT resources_shield_same_workspace_fkey`)
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

// FU-5: every foreign key between two workspace-scoped tables carries the
// workspace, so a new migration can't quietly add one that doesn't. Keys to
// identity_connections are the one exception: a platform connection
// (tenant_id NULL, ADR-024) is shared by every workspace.
func TestWorkspaceConsistency_EveryCrossTableKeyCarriesTheWorkspace(t *testing.T) {
	ctx, pool := newConsistencyDB(t)
	rows, err := pool.Query(ctx, `
		WITH scoped AS (
		  SELECT a.attrelid AS rel FROM pg_attribute a JOIN pg_class c ON c.oid = a.attrelid
		  WHERE c.relnamespace = 'public'::regnamespace AND c.relkind = 'r'
		    AND a.attname IN ('tenant_id', 'workspace_id') AND NOT a.attisdropped)
		SELECT con.conname FROM pg_constraint con
		WHERE con.contype = 'f'
		  AND con.conrelid IN (SELECT rel FROM scoped) AND con.confrelid IN (SELECT rel FROM scoped)
		  AND con.confrelid NOT IN ('workspaces'::regclass, 'identity_connections'::regclass)
		  AND NOT EXISTS (SELECT 1 FROM pg_attribute a WHERE a.attrelid = con.conrelid
		                  AND a.attnum = ANY(con.conkey) AND a.attname IN ('tenant_id', 'workspace_id'))`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		t.Errorf("foreign key %s doesn't include the workspace", name)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}

// FU-5 behaviour, one case per family of keys: a write that points into
// another workspace is refused; deleting the parent of a SET NULL key clears
// only the reference, never the row's workspace; cascades still cascade.
func TestWorkspaceConsistency_DevicesShieldsAndProfiles(t *testing.T) {
	ctx, pool := newConsistencyDB(t)
	type ws struct{ id, rn, conn, user, admin, dev, prof, res, shield, report string }
	seed := func(slug, ip string) ws {
		w := ws{id: mustInsertWorkspace(t, ctx, pool, slug)}
		w.rn, w.conn = mustInsertRNWithConnector(t, ctx, pool, w.id, "rn-"+slug, "td-"+slug, ip)
		w.user = mustInsertUser(t, ctx, pool, w.id)
		w.admin = mustInsertUser(t, ctx, pool, w.id)
		w.dev = mustInsertClientDevice(t, ctx, pool, w.id, w.user, "spiffe://td-"+slug+"/client/"+slug)
		w.res = mustInsertResource(t, ctx, pool, w.id, w.rn, "res-"+slug, ip, 22)
		w.shield = mustInsertShield(t, ctx, pool, w.id, w.rn, w.conn)
		mustScan := func(dst *string, sql string, args ...any) {
			t.Helper()
			if err := pool.QueryRow(ctx, sql, args...).Scan(dst); err != nil {
				t.Fatalf("seed %s: %v", slug, err)
			}
		}
		mustScan(&w.prof, `INSERT INTO device_profiles (workspace_id, name) VALUES ($1, 'prof') RETURNING id::text`, w.id)
		mustScan(&w.report, `INSERT INTO device_posture_reports (report_id, device_id, workspace_id, client_version, os_info, reported_at)
			VALUES (gen_random_uuid()::text, $1, $2, '1', '{}', now()) RETURNING id::text`, w.dev, w.id)
		return w
	}
	a := seed("fam-a", "10.92.0.1")
	b := seed("fam-b", "10.93.0.1")

	// Consistent rows, including every SET NULL reference.
	mustExec(t, ctx, pool, `UPDATE client_devices SET approved_at = now(), approved_by = $1 WHERE id = $2`, a.admin, a.dev)
	mustExec(t, ctx, pool, `UPDATE resources SET shield_id = $1 WHERE id = $2`, a.shield, a.res)
	mustExec(t, ctx, pool, `INSERT INTO resource_profile_bindings (resource_id, profile_id, workspace_id) VALUES ($1, $2, $3)`, a.res, a.prof, a.id)
	mustExec(t, ctx, pool, `INSERT INTO device_profile_evaluations (device_id, profile_id, workspace_id, satisfied, profile_revision, report_id)
		VALUES ($1, $2, $3, true, 1, $4)`, a.dev, a.prof, a.id, a.report)

	for _, tc := range []struct {
		name, constraint, sql string
		args                  []any
	}{
		{"device of another workspace's user", "client_devices_user_same_workspace_fkey",
			`INSERT INTO client_devices (workspace_id, user_id, name, os) VALUES ($1, $2, 'x', 'linux')`, []any{a.id, b.user}},
		{"device approved by another workspace's admin", "client_devices_approved_by_same_workspace_fkey",
			`UPDATE client_devices SET approved_by = $1 WHERE id = $2`, []any{b.admin, a.dev}},
		{"posture report for another workspace's device", "device_posture_reports_device_same_workspace_fkey",
			`INSERT INTO device_posture_reports (report_id, device_id, workspace_id, client_version, os_info, reported_at)
			 VALUES ('x', $1, $2, '1', '{}', now())`, []any{b.dev, a.id}},
		{"evaluation against another workspace's profile", "device_profile_evaluations_profile_same_workspace_fkey",
			`UPDATE device_profile_evaluations SET profile_id = $1 WHERE device_id = $2`, []any{b.prof, a.dev}},
		{"evaluation citing another workspace's report", "device_profile_evaluations_report_same_workspace_fkey",
			`UPDATE device_profile_evaluations SET report_id = $1 WHERE device_id = $2`, []any{b.report, a.dev}},
		{"binding of another workspace's resource", "resource_profile_bindings_resource_same_workspace_fkey",
			`INSERT INTO resource_profile_bindings (resource_id, profile_id, workspace_id) VALUES ($1, $2, $3)`, []any{b.res, a.prof, a.id}},
		{"binding to another workspace's profile", "resource_profile_bindings_profile_same_workspace_fkey",
			`UPDATE resource_profile_bindings SET profile_id = $1 WHERE resource_id = $2`, []any{b.prof, a.res}},
		{"resource protected by another workspace's shield", "resources_shield_same_workspace_fkey",
			`UPDATE resources SET shield_id = $1 WHERE id = $2`, []any{b.shield, a.res}},
		{"shield on another workspace's connector", "shields_connector_same_workspace_fkey",
			`UPDATE shields SET connector_id = $1 WHERE id = $2`, []any{b.conn, a.shield}},
		{"shield on another workspace's network", "shields_remote_network_same_workspace_fkey",
			`UPDATE shields SET remote_network_id = $1 WHERE id = $2`, []any{b.rn, a.shield}},
		{"connector on another workspace's network", "connectors_remote_network_same_workspace_fkey",
			`UPDATE connectors SET remote_network_id = $1 WHERE id = $2`, []any{b.rn, a.conn}},
		{"permission for another workspace's user", "workspace_permissions_user_same_workspace_fkey",
			`INSERT INTO workspace_permissions (workspace_id, user_id, permission) VALUES ($1, $2, 'resources.manage')`, []any{a.id, b.user}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pool.Exec(ctx, tc.sql, tc.args...)
			var pgErr *pgconn.PgError
			if !errors.As(err, &pgErr) || pgErr.Code != "23503" || pgErr.ConstraintName != tc.constraint {
				t.Fatalf("got %v, want foreign key violation of %s", err, tc.constraint)
			}
		})
	}

	// SET NULL keys clear only the reference.
	mustExec(t, ctx, pool, `DELETE FROM users WHERE id = $1`, a.admin)
	mustExec(t, ctx, pool, `DELETE FROM device_posture_reports WHERE id = $1`, a.report)
	mustExec(t, ctx, pool, `DELETE FROM shields WHERE id = $1`, a.shield)
	var approvedBy, reportID, shieldID *string
	var devWS, evalWS, resWS string
	if err := pool.QueryRow(ctx, `SELECT approved_by::text, workspace_id::text FROM client_devices WHERE id = $1`, a.dev).Scan(&approvedBy, &devWS); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT report_id::text, workspace_id::text FROM device_profile_evaluations WHERE device_id = $1`, a.dev).Scan(&reportID, &evalWS); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT shield_id::text, tenant_id::text FROM resources WHERE id = $1`, a.res).Scan(&shieldID, &resWS); err != nil {
		t.Fatal(err)
	}
	if approvedBy != nil || reportID != nil || shieldID != nil {
		t.Errorf("SET NULL left references: approved_by=%v report_id=%v shield_id=%v", approvedBy, reportID, shieldID)
	}
	if devWS != a.id || evalWS != a.id || resWS != a.id {
		t.Errorf("SET NULL changed a row's workspace: device %s, evaluation %s, resource %s; want %s", devWS, evalWS, resWS, a.id)
	}

	// Cascades: the profile takes its bindings and evaluations with it.
	mustExec(t, ctx, pool, `DELETE FROM device_profiles WHERE id = $1`, a.prof)
	var left int
	_ = pool.QueryRow(ctx, `SELECT (SELECT count(*) FROM resource_profile_bindings WHERE workspace_id = $1)
		+ (SELECT count(*) FROM device_profile_evaluations WHERE workspace_id = $1)`, a.id).Scan(&left)
	if left != 0 {
		t.Errorf("%d bindings/evaluations left after deleting their profile", left)
	}
}

// newConsistencyDB makes a throwaway database with every migration applied.
func newConsistencyDB(t *testing.T) (context.Context, *pgxpool.Pool) {
	t.Helper()
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueTestDBName(t)
	adminPool := mustConnectTestPool(t, ctx, adminDSN)
	t.Cleanup(adminPool.Close)
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	t.Cleanup(func() {
		if _, err := adminPool.Exec(context.Background(), "DROP DATABASE IF EXISTS "+dbName+" WITH (FORCE)"); err != nil {
			t.Logf("drop test database: %v", err)
		}
	})
	testDSN, err := withTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test database dsn: %v", err)
	}
	pool := mustConnectTestPool(t, ctx, testDSN)
	t.Cleanup(pool.Close)
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	return ctx, pool
}
