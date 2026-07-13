package transport

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/yourorg/ztna/controller/internal/appmeta"
)

// TestCompileTransportSnapshot verifies the transport compiler groups active
// connectors by remote_network_id and resolves tunnel/relay/SPIFFE coordinates
// identically to the ACL compiler. Requires PKI_TEST_DATABASE_URL; otherwise skips.
func TestCompileTransportSnapshot(t *testing.T) {
	adminDSN := os.Getenv("PKI_TEST_DATABASE_URL")
	if adminDSN == "" {
		t.Skip("PKI_TEST_DATABASE_URL not set")
	}
	ctx := context.Background()
	dbName := uniqueTransportTestDBName(t)

	adminPool := mustConnectTransportPool(t, ctx, adminDSN)
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, "CREATE DATABASE "+dbName); err != nil {
		t.Fatalf("create test database: %v", err)
	}
	defer func() {
		if _, err := adminPool.Exec(ctx, "DROP DATABASE IF EXISTS "+dbName); err != nil {
			t.Logf("drop test database: %v", err)
		}
	}()

	testDSN, err := withTransportTestDBName(adminDSN, dbName)
	if err != nil {
		t.Fatalf("build test dsn: %v", err)
	}
	pool := mustConnectTransportPool(t, ctx, testDSN)
	defer pool.Close()
	if err := applyTransportMigrations(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}

	store := NewStore(pool)
	notifier := NewNotifier(NewSnapshotCache())

	t.Run("connector with placement carries relay coords", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, pool, "ws-tp-1")
		rnID := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-1")
		connID := mustInsertConnector(t, ctx, pool, wsID, rnID, "td-1", "10.1.0.1")
		relayID := mustInsertActiveRelay(t, ctx, pool, "relay.x:9093")
		mustInsertPlacement(t, ctx, pool, connID, relayID)

		snap, err := CompileTransportSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if len(snap.RemoteNetworks) != 1 {
			t.Fatalf("want 1 remote_network, got %d", len(snap.RemoteNetworks))
		}
		rn := snap.RemoteNetworks[0]
		if rn.RemoteNetworkId != rnID {
			t.Fatalf("remote_network_id: want %q got %q", rnID, rn.RemoteNetworkId)
		}
		if len(rn.Connectors) != 1 {
			t.Fatalf("want 1 connector, got %d", len(rn.Connectors))
		}
		c := rn.Connectors[0]
		if c.ConnectorId != connID {
			t.Fatalf("connector_id: want %q got %q", connID, c.ConnectorId)
		}
		if c.ConnectorTunnelAddr != "10.1.0.1:9092" {
			t.Fatalf("tunnel_addr: want 10.1.0.1:9092 got %q", c.ConnectorTunnelAddr)
		}
		if c.RelayAddr != "relay.x:9093" {
			t.Fatalf("relay_addr: want relay.x:9093 got %q", c.RelayAddr)
		}
		if c.ConnectorSpiffe != appmeta.ConnectorSPIFFEID("td-1", connID) {
			t.Fatalf("connector_spiffe mismatch: got %q", c.ConnectorSpiffe)
		}
		if c.RelaySpiffeId != appmeta.RelaySPIFFEID(relayID) {
			t.Fatalf("relay_spiffe_id mismatch: got %q", c.RelaySpiffeId)
		}
	})

	t.Run("connector without placement is direct-only (empty relay)", func(t *testing.T) {
		wsID := mustInsertWorkspace(t, ctx, pool, "ws-tp-2")
		rnID := mustInsertRemoteNetwork(t, ctx, pool, wsID, "rn-2")
		_ = mustInsertConnector(t, ctx, pool, wsID, rnID, "td-2", "10.2.0.1")

		snap, err := CompileTransportSnapshot(ctx, store, notifier, wsID)
		if err != nil {
			t.Fatalf("compile: %v", err)
		}
		if len(snap.RemoteNetworks) != 1 || len(snap.RemoteNetworks[0].Connectors) != 1 {
			t.Fatalf("unexpected shape: %+v", snap.RemoteNetworks)
		}
		c := snap.RemoteNetworks[0].Connectors[0]
		if c.RelayAddr != "" || c.RelaySpiffeId != "" {
			t.Fatalf("no-placement connector must have empty relay, got addr=%q spiffe=%q", c.RelayAddr, c.RelaySpiffeId)
		}
		if c.ConnectorTunnelAddr != "10.2.0.1:9092" {
			t.Fatalf("tunnel_addr: want 10.2.0.1:9092 got %q", c.ConnectorTunnelAddr)
		}
	})
}

// ── minimal DB harness (mirrors policy integration helpers) ──────────────────

func mustConnectTransportPool(t *testing.T, ctx context.Context, dsn string) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("pgxpool.New: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}
	return pool
}

func uniqueTransportTestDBName(t *testing.T) string {
	t.Helper()
	return fmt.Sprintf("transport_test_%d", os.Getpid())
}

func withTransportTestDBName(dsn, dbName string) (string, error) {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	parsed.Path = "/" + dbName
	return parsed.String(), nil
}

func applyTransportMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	dir, err := filepath.Abs(filepath.Join("..", "..", "migrations"))
	if err != nil {
		return err
	}
	files, err := filepath.Glob(filepath.Join(dir, "*.sql"))
	if err != nil {
		return err
	}
	sort.Strings(files)
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return err
		}
		if _, err := pool.Exec(ctx, string(b)); err != nil {
			return fmt.Errorf("execute %s: %w", filepath.Base(f), err)
		}
	}
	return nil
}

func mustInsertWorkspace(t *testing.T, ctx context.Context, pool *pgxpool.Pool, slug string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO workspaces (slug, name, status, trust_domain)
		 VALUES ($1, $1, 'active', $1 || '.test') RETURNING id::text`, slug,
	).Scan(&id); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	return id
}

func mustInsertRemoteNetwork(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, name string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO remote_networks (tenant_id, name, location)
		 VALUES ($1, $2, 'home') RETURNING id::text`, tenantID, name,
	).Scan(&id); err != nil {
		t.Fatalf("insert remote_network: %v", err)
	}
	return id
}

func mustInsertConnector(t *testing.T, ctx context.Context, pool *pgxpool.Pool, tenantID, rnID, trustDomain, lanAddr string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO connectors (tenant_id, remote_network_id, name, status, trust_domain, lan_addr, last_heartbeat_at)
		 VALUES ($1, $2, 'test-connector', 'active', $3, $4, NOW()) RETURNING id::text`,
		tenantID, rnID, trustDomain, lanAddr,
	).Scan(&id); err != nil {
		t.Fatalf("insert connector: %v", err)
	}
	return id
}

func mustInsertActiveRelay(t *testing.T, ctx context.Context, pool *pgxpool.Pool, publicAddr string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(ctx,
		`INSERT INTO relays (name, status, public_addr, last_heartbeat_at)
		 VALUES ('test-relay', 'active', $1, NOW()) RETURNING id::text`, publicAddr,
	).Scan(&id); err != nil {
		t.Fatalf("insert relay: %v", err)
	}
	return id
}

func mustInsertPlacement(t *testing.T, ctx context.Context, pool *pgxpool.Pool, connectorID, relayID string) {
	t.Helper()
	if _, err := pool.Exec(ctx,
		`INSERT INTO connector_relay_placement (connector_id, relay_id, attached_at, last_confirmed, source)
		 VALUES ($1, $2, NOW(), NOW(), 'heartbeat')
		 ON CONFLICT (connector_id) DO UPDATE SET relay_id = EXCLUDED.relay_id`,
		connectorID, relayID,
	); err != nil {
		t.Fatalf("insert placement: %v", err)
	}
}
