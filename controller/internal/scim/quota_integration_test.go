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
}
