package relay

import (
	"context"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// AD-1 (connector-revocation Phase D): of the identities a relay holds, only
// revoked connectors — and revoked-then-deleted ones while a certificate is
// still valid — are returned, across workspaces; active, all-expired and
// malformed identities are not.
func TestRevokedConnectorSPIFFEIDsIntegration(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueRelayTestDBName(t)
	adminPool := mustConnectRelayPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()
	testDSN, err := withRelayTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectRelayPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyRelayMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	ws := func(slug string) (id, td string) {
		td = "ws-" + slug + ".zecurity.test"
		if err := pool.QueryRow(ctx,
			`INSERT INTO workspaces (slug, name, status, trust_domain) VALUES ($1, $1, 'active', $2) RETURNING id`,
			slug, td).Scan(&id); err != nil {
			t.Fatalf("insert workspace: %v", err)
		}
		return id, td
	}
	conn := func(wsID, td, status, certValidity string) string {
		t.Helper()
		var rnID, id string
		if err := pool.QueryRow(ctx,
			`INSERT INTO remote_networks (tenant_id, name, location) VALUES ($1, md5(random()::text), 'other') RETURNING id`,
			wsID).Scan(&rnID); err != nil {
			t.Fatalf("insert remote network: %v", err)
		}
		if err := pool.QueryRow(ctx,
			`INSERT INTO connectors (tenant_id, remote_network_id, name, status, trust_domain, revoked_at)
			 VALUES ($1, $2, md5(random()::text), $3, $4, CASE WHEN $3 = 'revoked' THEN NOW() END) RETURNING id`,
			wsID, rnID, status, td).Scan(&id); err != nil {
			t.Fatalf("insert connector: %v", err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO connector_certificates (tenant_id, connector_id, serial, issued_at, not_after)
			 VALUES ($1, $2, md5(random()::text), NOW() - interval '1 hour', NOW() + $3::interval)`,
			wsID, id, certValidity); err != nil {
			t.Fatalf("insert certificate: %v", err)
		}
		return id
	}
	wsA, tdA := ws("rcA")
	wsB, tdB := ws("rcB")
	active := conn(wsA, tdA, "active", "1 day")
	revokedA := conn(wsA, tdA, "revoked", "1 day")
	revokedB := conn(wsB, tdB, "revoked", "1 day") // other workspace, same relay
	expired := conn(wsA, tdA, "revoked", "-1 hour")
	deleted := conn(wsB, tdB, "revoked", "1 day")
	if _, err := pool.Exec(ctx, `DELETE FROM connectors WHERE id = $1`, deleted); err != nil {
		t.Fatalf("delete connector: %v", err)
	}

	id := appmeta.ConnectorSPIFFEID
	asked := []string{
		id(tdA, active), id(tdA, revokedA), id(tdB, revokedB), id(tdA, expired), id(tdB, deleted),
		strings.ToUpper(id(tdA, revokedA))[:len("spiffe://")] + id(tdA, revokedA)[len("spiffe://"):], // scheme case: malformed
		id(tdB, revokedA), // right id, wrong workspace: not this connector
		"spiffe://" + tdA + "/connector/not-a-uuid",
	}
	got, err := NewStore(pool).RevokedConnectorSPIFFEIDs(ctx, asked)
	if err != nil {
		t.Fatalf("RevokedConnectorSPIFFEIDs: %v", err)
	}
	want := []string{id(tdA, revokedA), id(tdB, revokedB), id(tdB, deleted)}
	sort.Strings(want)
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("revoked = %v\nwant      %v", got, want)
	}
}

var _ *pgxpool.Pool
