package policy

import (
	"context"
	"os"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/appmeta"
	"github.com/yourorg/ztna/controller/internal/posture"
)

// Connector-revocation Phase C (design §8, D19): the ACL lists a connector
// whose identity is revoked — or revoked and then deleted — for as long as
// any of its certificates can still be valid; never an active connector, and
// never one whose certificates have all expired.
func TestCompileACLSnapshot_RevokedConnectors(t *testing.T) {
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

	wsID := mustInsertWorkspace(t, ctx, pool, "revconn")
	rnID, activeID := mustInsertRNWithConnector(t, ctx, pool, wsID, "rn-a", "td-revconn", "10.30.0.1")
	_, revokedID := mustInsertRNWithConnector(t, ctx, pool, wsID, "rn-b", "td-revconn", "10.30.0.2")
	_, deletedID := mustInsertRNWithConnector(t, ctx, pool, wsID, "rn-c", "td-revconn", "10.30.0.3")
	_, expiredID := mustInsertRNWithConnector(t, ctx, pool, wsID, "rn-d", "td-revconn", "10.30.0.4")
	_ = rnID
	// Another workspace's revoked connector must never leak in.
	otherWS := mustInsertWorkspace(t, ctx, pool, "revconn-other")
	_, otherRevoked := mustInsertRNWithConnector(t, ctx, pool, otherWS, "rn-x", "td-other", "10.31.0.1")

	cert := func(tenant, conn, notAfter string) {
		t.Helper()
		if _, err := pool.Exec(ctx,
			`INSERT INTO connector_certificates (tenant_id, connector_id, serial, issued_at, not_after)
			 VALUES ($1, $2, md5(random()::text), NOW() - interval '1 hour', NOW() + $3::interval)`,
			tenant, conn, notAfter); err != nil {
			t.Fatalf("insert certificate: %v", err)
		}
	}
	cert(wsID, activeID, "1 day")
	cert(wsID, revokedID, "-2 days") // an old expired cert …
	cert(wsID, revokedID, "1 day")   // … and a still-valid one: listed
	cert(wsID, deletedID, "1 day")
	cert(wsID, expiredID, "-1 hour") // all certificates expired: not listed
	cert(otherWS, otherRevoked, "1 day")

	revoke := func(id string) {
		t.Helper()
		if _, err := pool.Exec(ctx, `UPDATE connectors SET status='revoked', revoked_at=NOW() WHERE id=$1`, id); err != nil {
			t.Fatalf("revoke: %v", err)
		}
	}
	revoke(revokedID)
	revoke(deletedID)
	revoke(expiredID)
	revoke(otherRevoked)
	if _, err := pool.Exec(ctx, `DELETE FROM connectors WHERE id=$1`, deletedID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	compiled, err := CompileACLSnapshot(ctx, NewStore(pool), posture.NewStore(pool), notifier, wsID)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	var wsTD string
	if err := pool.QueryRow(ctx, `SELECT trust_domain FROM workspaces WHERE id=$1`, wsID).Scan(&wsTD); err != nil {
		t.Fatalf("workspace trust domain: %v", err)
	}
	var got []string
	for _, rc := range compiled.Snapshot.RevokedConnectors {
		got = append(got, rc.ConnectorId)
		// A deleted connector's row is gone: the workspace trust domain is used.
		td := "td-revconn"
		if rc.ConnectorId == deletedID {
			td = wsTD
		}
		want := appmeta.ConnectorSPIFFEID(td, rc.ConnectorId)
		if rc.ConnectorSpiffe != want {
			t.Errorf("connector %s spiffe = %q, want %q", rc.ConnectorId, rc.ConnectorSpiffe, want)
		}
	}
	want := []string{revokedID, deletedID}
	sort.Strings(got)
	sort.Strings(want)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("revoked_connectors = %v, want %v (revoked + revoked-then-deleted; not active %s, not expired %s, not other workspace %s)",
			got, want, activeID, expiredID, otherRevoked)
	}
}

var _ = (*pgxpool.Pool)(nil)
