package scim

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yourorg/ztna/controller/internal/identity"
	"github.com/yourorg/ztna/controller/internal/idp"
	"github.com/yourorg/ztna/controller/internal/policy"
)

// Sprint 23 S3 (D-47): SCIM user provisioning goes through the user quota.
// At the limit it is refused with an RFC 7644 error (403, the quota text as
// detail) and nothing is created; an identity the tenant already counts
// (a pending invitation) is provisioned even at the limit.
func TestSCIMProvision_Quota(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("scim_quota_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	adminPool := mustConnectPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()
	testDSN, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatal(err)
	}
	pool := mustConnectPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	ws := seedWorkspace(ctx, t, pool, "quota")
	conn := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")
	ds := NewDirectoryService(pool, idp.NewStore(pool, nil), identity.NewAuditSink(pool), policy.NewNotifier(policy.NewSnapshotCache()), nil, nil)
	sc := scopeFor(t, ctx, ds, ws, conn)
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by)
		VALUES ($1, 'users', 1, 'test', 'op')`, ws); err != nil {
		t.Fatal(err)
	}
	provision := func(user, ext string) *SCIMError {
		_, serr := ds.Provision(ctx, sc, map[string]any{"userName": user, "externalId": ext, "active": true}, "")
		return serr
	}
	if serr := provision("ann@quota.test", "ext-ann"); serr != nil {
		t.Fatalf("first user: %v", serr)
	}
	serr := provision("bob@quota.test", "ext-bob")
	if serr == nil || serr.Status != 403 || !strings.Contains(serr.Detail, "quota exceeded: users limit is 1") {
		t.Fatalf("over the limit: %+v, want 403 quota exceeded", serr)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE tenant_id = $1`, ws).Scan(&n); err != nil || n != 1 {
		t.Fatalf("users = %d (err %v), want 1", n, err)
	}
	// A pending invitation already counts its identity.
	if _, err := pool.Exec(ctx, `UPDATE workspace_quota_overrides SET limit_value = 2 WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO invitations (email, workspace_id, invited_by, token)
		SELECT 'cara@quota.test', $1, id, 'tok-cara' FROM users WHERE tenant_id = $1 LIMIT 1`, ws); err != nil {
		t.Fatal(err)
	}
	if serr := provision("cara@quota.test", "ext-cara"); serr != nil {
		t.Fatalf("invited identity at the limit: %v", serr)
	}

	// Reactivating a removed (deleted) user takes a slot again: refused at
	// the limit, allowed once there is room. Lifting a suspension takes none.
	var annID string
	if err := pool.QueryRow(ctx, `SELECT id::text FROM users WHERE tenant_id = $1 AND email = 'ann@quota.test'`, ws).Scan(&annID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'deleted' WHERE id = $1`, annID); err != nil {
		t.Fatal(err)
	}
	if serr := provision("dan@quota.test", "ext-dan"); serr != nil { // takes ann's freed slot
		t.Fatalf("provision into the freed slot: %v", serr)
	}
	serr = ds.Reactivate(ctx, sc, annID, "")
	if serr == nil || serr.Status != 403 || !strings.Contains(serr.Detail, "quota exceeded: users") {
		t.Fatalf("reactivating a removed user at the limit: %+v, want 403 quota exceeded", serr)
	}
	var st string
	if err := pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, annID).Scan(&st); err != nil || st != "deleted" {
		t.Fatalf("refused reactivation changed the status to %q (err %v)", st, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_quota_overrides SET limit_value = 3 WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	if serr := ds.Reactivate(ctx, sc, annID, ""); serr != nil {
		t.Fatalf("reactivation with room: %v", serr)
	}
	if _, err := pool.Exec(ctx, `UPDATE users SET status = 'suspended' WHERE id = $1`, annID); err != nil {
		t.Fatal(err)
	}
	if serr := ds.Reactivate(ctx, sc, annID, ""); serr != nil {
		t.Fatalf("lifting a suspension at the limit: %v", serr)
	}
}

// Sprint 24 X1 (D-47 refinement, 2026-11-20): the SCIM mapping probe takes no
// users quota slot, so SCIM can be enabled at the limit; its user is created
// suspended; one probe per workspace at a time; real creates are still refused.
func TestSCIMMappingProbe_Quota(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := fmt.Sprintf("scim_probe_quota_test_%d_%d", os.Getpid(), time.Now().UnixNano())
	adminPool := mustConnectPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() { _, _ = adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName) }()
	testDSN, err := withDBName(adminDSN, dbName)
	if err != nil {
		t.Fatal(err)
	}
	pool := mustConnectPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyAllMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	ws := seedWorkspace(ctx, t, pool, "probequota")
	connID := seedSCIMConnection(ctx, t, pool, ws, "okta", "sub", "externalId")
	idpStore := idp.NewStore(pool, nil)
	ds := NewDirectoryService(pool, idpStore, identity.NewAuditSink(pool), policy.NewNotifier(policy.NewSnapshotCache()), nil, nil)
	sc := scopeFor(t, ctx, ds, ws, connID)
	if sc.probe {
		t.Fatal("a scope resolved for a SCIM request must never be a probe scope")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workspace_quota_overrides (workspace_id, key, limit_value, reason, set_by)
		VALUES ($1, 'users', 1, 'test', 'op')`, ws); err != nil {
		t.Fatal(err)
	}
	if _, serr := ds.Provision(ctx, sc, map[string]any{"userName": "ann@probe.test", "externalId": "ext-ann", "active": true}, ""); serr != nil {
		t.Fatalf("first user: %v", serr)
	}
	liveUsers := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE tenant_id = $1 AND status <> 'deleted'`, ws).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	conn, err := idpStore.GetByID(ctx, connID)
	if err != nil {
		t.Fatalf("load connection: %v", err)
	}

	t.Run("probe at the users limit verifies and leaves the count unchanged", func(t *testing.T) {
		res := ds.ProbeMapping(ctx, conn)
		if !res.Verified {
			t.Fatalf("probe at the limit: %s", res.Reason)
		}
		if n := liveUsers(); n != 1 {
			t.Fatalf("live users after the probe = %d, want 1", n)
		}
	})

	t.Run("a real create at the limit is still refused with the quota error", func(t *testing.T) {
		_, serr := ds.Provision(ctx, sc, map[string]any{"userName": "bob@probe.test", "externalId": "ext-bob", "active": true}, "")
		if serr == nil || serr.Status != 403 || !strings.Contains(serr.Detail, "quota exceeded: users limit is 1") {
			t.Fatalf("real create at the limit: %+v, want 403 quota exceeded", serr)
		}
	})

	t.Run("the probe user is created suspended (no login or enrollment)", func(t *testing.T) {
		prov := newSCIMProvisioner(pool, ws, connID, "", nil)
		prov.probe = true
		core, err := prov.Provision(ctx, identity.ProvisionInput{
			Email: "mapping-probe-x@probe.internal", Provider: "okta", Subject: "mapping-probe-x",
			ConnectionID: connID, Issuer: "https://probe.example.com",
		})
		if err != nil {
			t.Fatalf("probe provision at the limit: %v", err)
		}
		var st string
		if err := pool.QueryRow(ctx, `SELECT status FROM users WHERE id = $1`, core.UserID).Scan(&st); err != nil || st != "suspended" {
			t.Fatalf("probe user status = %q (err %v), want suspended", st, err)
		}
		if core.Status != "suspended" {
			t.Fatalf("returned status = %q, want suspended", core.Status)
		}
		if _, err := pool.Exec(ctx, `UPDATE users SET status = 'deleted' WHERE id = $1`, core.UserID); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("only one probe per workspace at a time", func(t *testing.T) {
		ds.probing.Store(ws, struct{}{})
		res := ds.ProbeMapping(ctx, conn)
		ds.probing.Delete(ws)
		if res.Verified || !strings.Contains(res.Reason, "already running") {
			t.Fatalf("second concurrent probe: %+v, want refused", res)
		}
		if n := liveUsers(); n != 1 {
			t.Fatalf("a refused probe created users: %d, want 1", n)
		}
		if res := ds.ProbeMapping(ctx, conn); !res.Verified {
			t.Fatalf("probe after the first finished: %s", res.Reason)
		}
	})
}
